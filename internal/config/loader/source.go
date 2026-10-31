// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
)

// Source names what a load reads.
type Source struct {
	// FS is the file system that holds Path; nil reads the operating
	// system's files, Path then being an OS path.
	FS fs.FS
	// Path is a Bundle directory rooted at ruralz.yaml, or a file that
	// holds a one-file Bundle, such as a rendered Bundle; for an
	// environments file, that file. Within FS it is a slash path, "." for
	// the root of FS.
	Path string
}

// IsZero reports whether s names nothing.
func (s Source) IsZero() bool { return s.FS == nil && s.Path == "" }

// opened is a source ready to read: a directory as an fs.FS rooted at the
// Bundle root, or one file.
type opened struct {
	// root is the Bundle root of a directory source.
	root fs.FS
	// file is the one-file Bundle's path within fsys, for a file source.
	file string
	fsys fs.FS
	// name is the source as given, slash-separated, for diagnostics about a
	// file source.
	name string
	// closer closes the os.Root of an OS directory.
	closer io.Closer
}

// open resolves src. The OS path of a directory is opened as an os.Root,
// so no later read leaves it; the path itself may be a symbolic link,
// which is how a directory is replaced atomically. A missing source, or
// one that is neither a directory nor a regular file, is ErrSource.
func open(src Source) (*opened, error) {
	if src.FS == nil {
		if src.Path == "" {
			return nil, fmt.Errorf("%w: no path given", ErrSource)
		}
		name := filepath.ToSlash(src.Path)
		info, err := os.Stat(src.Path)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSource, err)
		}
		switch {
		case info.IsDir():
			r, err := os.OpenRoot(src.Path)
			if err != nil {
				return nil, fmt.Errorf("%w: %w", ErrSource, err)
			}
			return &opened{root: r.FS(), name: name, closer: r}, nil
		case info.Mode().IsRegular():
			dir, base := filepath.Split(src.Path)
			if dir == "" {
				dir = "."
			}
			return &opened{fsys: os.DirFS(dir), file: base, name: name}, nil
		default:
			return nil, fmt.Errorf("%w: %s is neither a directory nor a regular file", ErrSource, name)
		}
	}
	p := src.Path
	if p == "" {
		p = "."
	}
	info, err := fs.Stat(src.FS, p)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSource, err)
	}
	switch {
	case info.IsDir():
		sub, err := fs.Sub(src.FS, p)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSource, err)
		}
		return &opened{root: sub, name: p}, nil
	case info.Mode().IsRegular():
		return &opened{fsys: src.FS, file: p, name: p}, nil
	default:
		return nil, fmt.Errorf("%w: %s is neither a directory nor a regular file", ErrSource, p)
	}
}

// close releases an os.Root.
func (o *opened) close() {
	if o.closer != nil {
		_ = o.closer.Close()
	}
}

// errNotRegular reports a file that stopped being a regular file between
// the directory listing and its read.
var errNotRegular = errors.New("not a regular file")

// readFile reads name from fsys, at most limit bytes: over is true, with
// the first limit+1 bytes, when the file holds more.
func readFile(fsys fs.FS, name string, limit int64) (data []byte, over bool, err error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, errNotRegular
	}
	var buf bytes.Buffer
	if size := info.Size(); size > 0 && size <= limit && size < math.MaxInt32 {
		buf.Grow(int(size) + 1)
	}
	// One byte past the limit tells that the file passes it; a limit of
	// math.MaxInt64 reads everything.
	n, err := buf.ReadFrom(io.LimitReader(f, min(limit, math.MaxInt64-1)+1))
	if err != nil {
		return nil, false, err
	}
	return buf.Bytes(), n > limit, nil
}

// lfsPrefixes start a Git LFS pointer file, the current and the earliest
// form of the pointer specification.
func lfsPrefixes() [][]byte {
	return [][]byte{
		[]byte("version https://git-lfs.github.com/spec/"),
		[]byte("version https://hawser.github.com/spec/"),
	}
}

// lfsPointer reports whether data is a Git LFS pointer, the text Git keeps
// in place of a file stored in LFS.
func lfsPointer(data []byte) bool {
	for _, p := range lfsPrefixes() {
		if bytes.HasPrefix(data, p) {
			return true
		}
	}
	return false
}
