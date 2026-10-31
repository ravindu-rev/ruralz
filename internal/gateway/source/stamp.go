// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"errors"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

// ignoreFile is the one dot-named file the loader reads, in any
// directory.
const ignoreFile = ".ruralzignore"

// readDirBatch is the directory entries read at once, so a directory with
// many entries costs little memory.
const readDirBatch = 256

// stamp is what one poll sees of the source: the root's identity and an
// order-independent digest of every walked entry's name, type and mode
// and, for files, size and modification time.
type stamp struct {
	// missing reports a source that cannot be stat'ed; err says why.
	missing bool
	err     error
	// info is the root followed through symbolic links, its identity for
	// os.SameFile.
	info os.FileInfo
	// link is the root's own symbolic-link target, "" when it is none.
	link string
	// sum adds one FNV-1a hash per entry, the root's own included, so the
	// walk order does not matter and memory stays constant; n counts them.
	sum uint64
	n   int
}

// sameRoot reports whether s and o see the same root: both missing, or
// the same symbolic-link target and the same file or directory.
func (s stamp) sameRoot(o stamp) bool {
	if s.missing || o.missing {
		return s.missing == o.missing
	}
	return s.link == o.link && os.SameFile(s.info, o.info)
}

// equal reports whether s and o see the same root and the same entries.
func (s stamp) equal(o stamp) bool {
	return s.sameRoot(o) && s.sum == o.sum && s.n == o.n
}

// stamp stamps the source.
func (w *Watcher) stamp() stamp {
	p := w.cfg.Path
	var s stamp
	if li, err := os.Lstat(p); err == nil && li.Mode()&fs.ModeSymlink != 0 {
		s.link, _ = os.Readlink(p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return stamp{missing: true, err: err}
	}
	s.info = info
	s.add(".", info)
	if info.IsDir() {
		s.walk(p, "")
	}
	return s
}

// add hashes one entry into the stamp: its name, type and mode, and for
// anything but a directory its size and modification time. A directory's
// own size and time change with every entry it gains or loses, ignored
// dot entries such as an editor's swap file included, and its walked
// entries already count.
func (s *stamp) add(rel string, fi fs.FileInfo) {
	h := fnv.New64a()
	_, _ = io.WriteString(h, rel)
	_, _ = h.Write([]byte{0})
	_, _ = io.WriteString(h, strconv.FormatUint(uint64(fi.Mode()), 16))
	if !fi.IsDir() {
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, strconv.FormatInt(fi.Size(), 10))
		_, _ = h.Write([]byte{0})
		_, _ = io.WriteString(h, strconv.FormatInt(fi.ModTime().UnixNano(), 10))
	}
	s.sum += h.Sum64()
	s.n++
}

// addError hashes an entry that could not be read, so a directory that
// becomes readable again is a change.
func (s *stamp) addError(rel string, err error) {
	h := fnv.New64a()
	_, _ = io.WriteString(h, rel)
	_, _ = io.WriteString(h, "\x00error\x00")
	_, _ = io.WriteString(h, err.Error())
	s.sum += h.Sum64()
	s.n++
}

// walk adds every entry under dir, rel being its slash path from the
// root: not following symbolic links, skipping dot-named entries but
// .ruralzignore files.
func (s *stamp) walk(dir, rel string) {
	f, err := os.Open(dir) //nolint:gosec // G304: the operator's RURALZ_CONFIG tree.
	if err != nil {
		s.addError(rel, err)
		return
	}
	defer func() { _ = f.Close() }()
	for {
		entries, err := f.ReadDir(readDirBatch)
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") && (name != ignoreFile || e.IsDir()) {
				continue
			}
			erel := path.Join(rel, name)
			fi, ierr := e.Info()
			if ierr != nil {
				// Removed between the listing and the stat: the next poll
				// sees the tree without it.
				if !errors.Is(ierr, fs.ErrNotExist) {
					s.addError(erel, ierr)
				}
				continue
			}
			s.add(erel, fi)
			if e.IsDir() {
				s.walk(filepath.Join(dir, name), erel)
			}
		}
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.addError(rel, err)
			}
			return
		}
	}
}
