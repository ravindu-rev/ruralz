// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
)

// Lock is the held data-directory lock: an exclusive BSD flock on the
// lock file, taken with TryLock and kept until Release (spec 04
// requirements 5 and 7). Only a Lock writes holder.json and files under
// the data directory, and never after Release, which the Drain calls at
// its start. The owner keeps the Lock reachable for the process lifetime:
// a collected Lock closes its file, which drops the lock. Methods are
// safe for concurrent use.
type Lock struct {
	dir *Dir

	mu          sync.Mutex
	f           *os.File // nil after Release
	wroteHolder bool
}

// TryLock takes the lock without blocking. It returns ErrLocked when
// another open file description holds it, in this process or another
// (a starting ruralzd then enters handover on Linux or exits 1, spec 04
// requirement 7), and an error matching errors.ErrUnsupported on
// platforms without flock.
func (d *Dir) TryLock() (*Lock, error) {
	f, err := lockFile(d.LockPath())
	if err != nil {
		return nil, err
	}
	return &Lock{dir: d, f: f}, nil
}

// Dir returns the locked data directory.
func (l *Lock) Dir() *Dir { return l.dir }

// Held reports whether Release has not been called yet.
func (l *Lock) Held() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.f != nil
}

// Release removes holder.json when this Lock wrote it, then drops the
// lock and closes the lock file. It waits for an in-flight write and makes
// every later write fail with ErrReleased. It is idempotent.
func (l *Lock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	var errs []error
	if l.wroteHolder {
		// Removed while still locked, so it can never delete the record
		// a successor writes after taking the lock.
		if err := os.Remove(l.dir.HolderPath()); err != nil && !errors.Is(err, fs.ErrNotExist) { //nolint:gosec // G703: a fixed name under the data directory.
			errs = append(errs, fmt.Errorf("nodedir: %w", err))
		}
	}
	if err := unlockFile(l.f); err != nil {
		errs = append(errs, err)
	}
	l.f = nil
	return errors.Join(errs...)
}

// WriteHolder atomically writes holder.json (spec 04 requirement 5, spec
// 10 requirement 95). An empty Format is set to HolderFormat; the record
// must then pass Holder.Validate.
func (l *Lock) WriteHolder(h Holder) error {
	if h.Format == "" {
		h.Format = HolderFormat
	}
	data, err := h.Encode()
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrReleased
	}
	if err := WriteFileAtomic(l.dir.root, HolderFile, data, FilePerm); err != nil {
		return err
	}
	l.wroteHolder = true
	return nil
}

// WriteFile atomically replaces rel, a slash-separated path under the data
// directory whose parent exists (for example "lkg/lkg.json"), with data
// and mode FilePerm. The lock and holder files are refused; they have
// their own methods.
func (l *Lock) WriteFile(rel string, data []byte) error {
	path, err := l.dir.writablePath(rel)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrReleased
	}
	return WriteFileAtomic(filepath.Dir(path), filepath.Base(path), data, FilePerm)
}

// Remove deletes rel, a slash-separated path under the data directory,
// with the same restrictions as WriteFile. A missing file is not an
// error.
func (l *Lock) Remove(rel string) error {
	path, err := l.dir.writablePath(rel)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return ErrReleased
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("nodedir: %w", err)
	}
	return nil
}

// writablePath resolves rel for WriteFile and Remove.
func (d *Dir) writablePath(rel string) (string, error) {
	path, err := d.localPath(rel)
	if err != nil {
		return "", err
	}
	if path == d.LockPath() || path == d.HolderPath() || path == d.NodeIDPath() {
		return "", fmt.Errorf("%w: %q is managed by nodedir", ErrBadName, rel)
	}
	return path, nil
}
