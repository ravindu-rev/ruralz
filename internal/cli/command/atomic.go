// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
)

// WriteFileAtomic writes data to path atomically (spec 10 req 17): a
// temporary file ".<name>.tmp-<16 hex random>" in path's directory is
// created with perm (before umask), written, synced and renamed over path,
// replacing an existing file. A missing parent directory is an error, and
// the temporary file never outlives a failure.
func WriteFileAtomic(path string, data []byte, perm fs.FileMode) error {
	return writeFileAtomic(path, data, perm, rand.Reader, os.Rename)
}

func writeFileAtomic(path string, data []byte, perm fs.FileMode, rnd io.Reader,
	rename func(oldpath, newpath string) error,
) (err error) {
	_, name := filepath.Split(path)
	if name == "" {
		return fmt.Errorf("write %s: not a file path", path)
	}
	dir := filepath.Dir(path)
	if st, serr := os.Stat(dir); serr != nil {
		return fmt.Errorf("write %s: parent directory: %w", path, serr)
	} else if !st.IsDir() {
		return fmt.Errorf("write %s: parent %s is not a directory", path, dir)
	}
	var suffix [8]byte
	if _, err := io.ReadFull(rnd, suffix[:]); err != nil {
		return fmt.Errorf("write %s: random name: %w", path, err)
	}
	tmp := filepath.Join(dir, "."+name+".tmp-"+hex.EncodeToString(suffix[:]))
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm) //nolint:gosec // G304: the user names --output-file; the CLI writes where it is told.
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if _, err = f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return fmt.Errorf("write %s: sync: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return fmt.Errorf("write %s: close: %w", path, err)
	}
	if err = rename(tmp, path); err != nil {
		return fmt.Errorf("write %s: rename: %w", path, err)
	}
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable where directories can be synced; it is
// best effort, because the data itself is already synced.
func syncDir(dir string) {
	if runtime.GOOS == "windows" {
		return
	}
	d, err := os.Open(dir) //nolint:gosec // G304: the directory of the user-named --output-file.
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// WriteData writes a command's data to stdout, or atomically to path with
// mode 0644 before umask when path is set (--output-file, spec 10
// req 17). Call it once, after the command has succeeded, so nothing is
// written on a non-zero exit. A failure is exit 2.
func WriteData(sio IO, path string, data []byte) error {
	if path == "" {
		if _, err := sio.Stdout.Write(data); err != nil {
			return NoResult(fmt.Errorf("write stdout: %w", err))
		}
		return nil
	}
	if err := WriteFileAtomic(path, data, 0o644); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return NoResultf("--output-file %s: parent directory does not exist", path)
		}
		return NoResult(err)
	}
	return nil
}
