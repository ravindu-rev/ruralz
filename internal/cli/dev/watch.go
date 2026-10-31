// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/launch"
)

// ignoreFile is the per-directory ignore file the loader reads although
// its name starts with a dot.
const ignoreFile = ".ruralzignore"

// fileState is what a poll compares of one file or directory: an entry
// whose name, size, mode or modification time changes is a change, as for
// the Node's own watcher.
type fileState struct {
	size int64
	mode fs.FileMode
	mod  time.Time
	// err marks an entry that could not be read, such as a file that
	// disappeared.
	err bool
}

// snapshot is the state of every source ruralz dev run watches, by path.
type snapshot map[string]fileState

// equal reports whether s and o describe the same sources.
func (s snapshot) equal(o snapshot) bool { return maps.Equal(s, o) }

// snapshot reads the state of the sources: every entry under DIR, or DIR
// alone when it is a file, its overlays and .ruralzignore files included
// but the other entries whose name starts with a dot, which the loader
// never reads; the --environments file; the --secret-overrides file and
// every file it names.
func (r *runner) snapshot(ctx context.Context) snapshot {
	s := snapshot{}
	walkSource(s, r.src)
	if r.environments != "" {
		statInto(s, r.environments)
	}
	if r.overrides != "" {
		for _, f := range launch.OverrideFiles(ctx, r.overrides) {
			statInto(s, f)
		}
	}
	return s
}

// walkSource records src: a directory's entries, or the file itself.
func walkSource(s snapshot, src string) {
	fi, err := os.Stat(src)
	if err != nil || !fi.IsDir() {
		statInto(s, src)
		return
	}
	_ = filepath.WalkDir(src, func(p string, d fs.DirEntry, walkErr error) error {
		if d != nil && p != src && strings.HasPrefix(d.Name(), ".") && d.Name() != ignoreFile {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		s[p] = entryState(d, walkErr)
		return nil
	})
}

// entryState returns the compared state of a walked entry; an entry the
// walk or its stat could not read is marked so, which a later poll that
// reads it sees as a change.
func entryState(d fs.DirEntry, walkErr error) fileState {
	if walkErr != nil || d == nil {
		return fileState{err: true}
	}
	info, err := d.Info()
	if err != nil {
		return fileState{err: true}
	}
	return stateOf(info)
}

// statInto records the state of one file.
func statInto(s snapshot, p string) {
	fi, err := os.Stat(p)
	if err != nil {
		s[p] = fileState{err: true}
		return
	}
	s[p] = stateOf(fi)
}

// stateOf returns the compared state of fi; a directory's size is left
// out, as it says nothing of its entries.
func stateOf(fi fs.FileInfo) fileState {
	st := fileState{mode: fi.Mode(), mod: fi.ModTime()}
	if !fi.IsDir() {
		st.size = fi.Size()
	}
	return st
}
