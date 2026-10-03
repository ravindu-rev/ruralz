// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// tempPrefix starts every temporary file name; readers never open such
// names, so a temporary file left by a crash is ignored.
const tempPrefix = ".tmp-"

// WriteFileAtomic replaces dir/name with data: it writes a temporary file
// in dir with mode perm, fsyncs it, renames it over name and fsyncs dir
// (spec 04 requirement 5 "written atomically"). A crash leaves either the
// old or the new content, plus at most one ignored temporary file. name
// must be a plain file name.
func WriteFileAtomic(dir, name string, data []byte, perm fs.FileMode) error {
	tmp, err := writeTemp(dir, name, data, perm)
	if err != nil {
		return err
	}
	// G703: tmp comes from os.CreateTemp in dir and name is a checked
	// plain file name.
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil { //nolint:gosec // G703: see above.
		_ = os.Remove(tmp) //nolint:gosec // G703: tmp comes from os.CreateTemp.
		return fmt.Errorf("nodedir: %w", err)
	}
	return syncDir(dir)
}

// createExclusive creates dir/name with data and FilePerm unless it
// exists: the complete temporary file is hard-linked to name, so a reader
// never sees a partial file and an existing file is never replaced. An
// existing name returns an error matching fs.ErrExist.
func createExclusive(dir, name string, data []byte) error {
	tmp, err := writeTemp(dir, name, data, FilePerm)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if err := os.Link(tmp, filepath.Join(dir, name)); err != nil {
		return fmt.Errorf("nodedir: %w", err)
	}
	return syncDir(dir)
}

// writeTemp writes data to a new temporary file in dir with mode perm and
// fsyncs it; it returns the temporary path.
func writeTemp(dir, name string, data []byte, perm fs.FileMode) (string, error) {
	if name == "" || filepath.Base(name) != name || strings.HasPrefix(name, tempPrefix) || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", ErrBadName, name)
	}
	f, err := os.CreateTemp(dir, tempPrefix+name+"-*")
	if err != nil {
		return "", fmt.Errorf("nodedir: %w", err)
	}
	tmp := f.Name()
	err = f.Chmod(perm)
	if err == nil || errors.Is(err, errors.ErrUnsupported) {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp) //nolint:gosec // G703: tmp comes from os.CreateTemp.
		return "", fmt.Errorf("nodedir: %w", err)
	}
	return tmp, nil
}
