// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// objectHeader is the first line of a stored object: its kind, format
// marker and size. The digest is the file name, and the bytes after the
// header hash to it.
type objectHeader struct {
	Kind   controlstore.Kind `json:"kind"`
	Format string            `json:"format"`
	Size   int64             `json:"size"`
}

// contentStore is this replica's content objects on disk, by digest:
// content/sha256/<first two hex>/<64 hex>. Every object verifies itself
// against its file name when read, and a mismatched one is removed and
// counts as missing, so the replica fetches it again. Objects the state
// machine releases are deleted, unless a backup pins the store: then they
// wait until the last pin goes, and stay if a later entry referenced them
// again.
type contentStore struct {
	dir        string
	referenced func(controlstore.Digest) bool

	mu       sync.Mutex
	local    map[controlstore.Digest]struct{}
	pins     int
	deferred map[controlstore.Digest]struct{}
}

// openContent opens the content directory, removing leftover temporary
// files, and indexes the objects it holds. The walk runs inside an
// os.Root of the directory, so a link planted under it never sends a
// removal outside.
func openContent(dir string, referenced func(controlstore.Digest) bool) (*contentStore, error) {
	root := filepath.Join(dir, "sha256")
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("controlstore: content directory: %w", err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("controlstore: content directory: %w", err)
	}
	defer func() { _ = r.Close() }()
	c := &contentStore{dir: root, referenced: referenced, local: map[controlstore.Digest]struct{}{}, deferred: map[controlstore.Digest]struct{}{}}
	err = fs.WalkDir(r.FS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".tmp") {
			return r.Remove(p)
		}
		dg, ok := parseObjectName(name)
		if !ok || path.Base(path.Dir(p)) != name[:2] {
			return nil
		}
		c.local[dg] = struct{}{}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("controlstore: index content: %w", err)
	}
	return c, nil
}

// parseObjectName reads a stored object's file name.
func parseObjectName(name string) (controlstore.Digest, bool) {
	var d controlstore.Digest
	if len(name) != 2*len(d) || strings.ToLower(name) != name {
		return d, false
	}
	if _, err := hex.Decode(d[:], []byte(name)); err != nil {
		return d, false
	}
	return d, true
}

// path returns an object's file.
func (c *contentStore) path(d controlstore.Digest) string {
	h := hex.EncodeToString(d[:])
	return filepath.Join(c.dir, h[:2], h)
}

// has reports whether the object is stored.
func (c *contentStore) has(d controlstore.Digest) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.local[d]
	return ok
}

// missing returns the digests of ds that are not stored.
func (c *contentStore) missing(ds []controlstore.Digest) []controlstore.Digest {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []controlstore.Digest
	for _, d := range ds {
		if _, ok := c.local[d]; !ok {
			out = append(out, d)
		}
	}
	return out
}

// put stores an object durably: a temporary file, synced, renamed into
// place, and the directory synced. Storing a stored object checks that
// its kind and format agree.
func (c *contentStore) put(o controlstore.Object) error {
	if err := o.Validate(); err != nil {
		return err
	}
	d := o.Digest()
	if c.has(d) {
		h, err := c.header(d)
		if err == nil {
			if h.Kind != o.Kind || h.Format != o.Format {
				return fmt.Errorf("%w: object %s is stored as %s %s, not %s %s", controlstore.ErrInvalid, d, h.Kind, h.Format, o.Kind, o.Format)
			}
			return nil
		}
	}
	return c.write(d, objectHeader{Kind: o.Kind, Format: o.Format, Size: int64(len(o.Data))}, bytes.NewReader(o.Data))
}

// write stores an object from r, verifying that its bytes hash to d.
func (c *contentStore) write(d controlstore.Digest, h objectHeader, r io.Reader) error {
	final := c.path(d)
	dir := filepath.Dir(final)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("controlstore: content directory: %w", err)
	}
	f, err := os.CreateTemp(dir, filepath.Base(final)+".*.tmp")
	if err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	line, err := json.Marshal(h)
	if err != nil {
		return err
	}
	bw := bufio.NewWriterSize(f, 64<<10)
	if _, err := bw.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	sum := newHashingReader(io.LimitReader(r, h.Size+1))
	n, err := io.Copy(bw, sum)
	if err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	if n != h.Size || sum.digest() != d {
		return fmt.Errorf("%w: object bytes do not hash to %s", controlstore.ErrInvalid, d)
	}
	if err := bw.Flush(); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	if err := os.Rename(tmp, final); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	ok = true
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("controlstore: store %s: %w", d, err)
	}
	c.mu.Lock()
	c.local[d] = struct{}{}
	delete(c.deferred, d)
	c.mu.Unlock()
	return nil
}

// header reads a stored object's header.
func (c *contentStore) header(d controlstore.Digest) (objectHeader, error) {
	f, err := os.Open(c.path(d))
	if err != nil {
		return objectHeader{}, err
	}
	defer func() { _ = f.Close() }()
	h, _, err := readObjectHeader(bufio.NewReader(f))
	return h, err
}

// readObjectHeader reads the header line.
func readObjectHeader(r *bufio.Reader) (objectHeader, int, error) {
	line, err := r.ReadSlice('\n')
	if err != nil {
		return objectHeader{}, 0, fmt.Errorf("%w: object header: %w", controlstore.ErrInvalid, err)
	}
	var h objectHeader
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&h); err != nil || h.Size < 0 {
		return objectHeader{}, 0, fmt.Errorf("%w: object header", controlstore.ErrInvalid)
	}
	return h, len(line), nil
}

// get returns a stored object after verifying it. A stored object whose
// bytes do not hash to its name is removed and reported missing.
func (c *contentStore) get(d controlstore.Digest) (controlstore.Object, error) {
	data, err := os.ReadFile(c.path(d))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			c.forget(d)
			return controlstore.Object{}, fmt.Errorf("%w: content %s", controlstore.ErrNotFound, d)
		}
		return controlstore.Object{}, fmt.Errorf("controlstore: read %s: %w", d, err)
	}
	h, n, err := readObjectHeader(bufio.NewReader(bytes.NewReader(data)))
	body := data[min(n, len(data)):]
	if err != nil || int64(len(body)) != h.Size || controlstore.Sum(body) != d {
		c.remove(d)
		return controlstore.Object{}, fmt.Errorf("%w: content %s failed its digest check", controlstore.ErrNotFound, d)
	}
	return controlstore.Object{Kind: h.Kind, Format: h.Format, Data: body}, nil
}

// forget drops an object from the index.
func (c *contentStore) forget(d controlstore.Digest) {
	c.mu.Lock()
	delete(c.local, d)
	c.mu.Unlock()
}

// remove deletes an object.
func (c *contentStore) remove(d controlstore.Digest) {
	c.forget(d)
	_ = os.Remove(c.path(d))
}

// release deletes objects no record references any longer, or defers them
// while a backup pins the store.
func (c *contentStore) release(ds []controlstore.Digest) {
	if len(ds) == 0 {
		return
	}
	c.mu.Lock()
	if c.pins > 0 {
		for _, d := range ds {
			c.deferred[d] = struct{}{}
		}
		c.mu.Unlock()
		return
	}
	c.mu.Unlock()
	for _, d := range ds {
		c.remove(d)
	}
}

// pin holds every object until the returned function runs: a backup at
// one index never misses an object that a later entry released.
func (c *contentStore) pin() func() {
	c.mu.Lock()
	c.pins++
	c.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			c.mu.Lock()
			c.pins--
			var drop []controlstore.Digest
			if c.pins == 0 {
				for d := range c.deferred {
					drop = append(drop, d)
				}
				c.deferred = map[controlstore.Digest]struct{}{}
			}
			c.mu.Unlock()
			slices.SortFunc(drop, func(a, b controlstore.Digest) int { return slices.Compare(a[:], b[:]) })
			for _, d := range drop {
				if !c.referenced(d) {
					c.remove(d)
				}
			}
		})
	}
}
