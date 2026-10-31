// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// ErrOutputDir marks an output directory WriteTree refuses: an empty
// name, a path that exists and is not an empty directory, or a directory
// inside the source Bundle.
var ErrOutputDir = errors.New("render: unusable output directory")

// File modes of a written tree: directories and files readable by their
// owner's group at most, as for every file Ruralz writes.
const (
	dirMode  fs.FileMode = 0o750
	fileMode fs.FileMode = 0o600
)

// tempPrefix starts the name of the hidden directory WriteTree fills
// before it renames it to the output directory; a Bundle loader skips
// hidden paths, so a leftover never joins a Bundle.
const tempPrefix = ".ruralz-render-"

// TreeOptions configure WriteTree.
type TreeOptions struct {
	// SourceRoot is the root directory of the source Bundle. An output
	// directory equal to it or inside it, compared both as written and
	// with symbolic links resolved, is ErrOutputDir, since rendering the
	// source again would then read the converted files as base files. ""
	// skips the check.
	SourceRoot string
}

// WriteTree writes files, as Convert returns them, under dir at their
// paths, the second half of ruralz bundle render --api-version. dir must
// not exist or must be an empty directory, so a converted tree never
// mixes with other files; its missing parents are created. The
// files are written into a new hidden directory beside dir (tempPrefix),
// which is then renamed to dir, so dir afterwards holds every file or, on
// any failure, is left as it was and the hidden directory is removed.
// Directories are created with mode 0o750 and files with 0o600, and no
// file is ever opened twice. Invalid or duplicate paths are ErrPath; an
// unusable dir is ErrOutputDir.
func WriteTree(dir string, files []File, o TreeOptions) (err error) {
	if dir == "" {
		return fmt.Errorf("%w: no directory named", ErrOutputDir)
	}
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b File) int { return cmp.Compare(a.Path, b.Path) })
	for i, f := range sorted {
		if err := checkPath(f.Path); err != nil {
			return err
		}
		if i > 0 && sorted[i-1].Path == f.Path {
			return fmt.Errorf("%w: two files %s", ErrPath, strconv.Quote(clip(f.Path)))
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOutputDir, err)
	}
	if o.SourceRoot != "" {
		if err := outsideSource(o.SourceRoot, abs); err != nil {
			return err
		}
	}
	replace, err := usableDir(abs)
	if err != nil {
		return err
	}
	parent := filepath.Dir(abs)
	if err := os.MkdirAll(parent, dirMode); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	tmp, err := os.MkdirTemp(parent, tempPrefix)
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	for _, f := range sorted {
		if err := writeNew(filepath.Join(tmp, filepath.FromSlash(f.Path)), f.Content); err != nil {
			return err
		}
	}
	if replace {
		// The empty directory goes only now, so a failure above leaves it;
		// Remove refuses a directory that gained an entry meanwhile.
		if err := os.Remove(abs); err != nil {
			return fmt.Errorf("%w: %w", ErrOutputDir, err)
		}
	}
	if err := os.Rename(tmp, abs); err != nil {
		if replace {
			// Put back the empty directory the caller named.
			_ = os.Mkdir(abs, dirMode)
		}
		return fmt.Errorf("render: %w", err)
	}
	return nil
}

// usableDir checks that abs does not exist or is an empty directory, and
// reports whether it exists.
func usableDir(abs string) (exists bool, err error) {
	fi, err := os.Lstat(abs)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, fmt.Errorf("%w: %w", ErrOutputDir, err)
	case !fi.IsDir():
		return false, fmt.Errorf("%w: %s exists and is not a directory", ErrOutputDir, abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrOutputDir, err)
	}
	if len(entries) > 0 {
		return false, fmt.Errorf("%w: %s is not empty", ErrOutputDir, abs)
	}
	return true, nil
}

// writeNew creates the file p, which must not exist, with its missing
// parent directories, and writes content to it.
func writeNew(p string, content []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), dirMode); err != nil {
		return fmt.Errorf("render: %w", err)
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode) //nolint:gosec // G304: p is a checked relative path joined to the directory WriteTree created.
	if err != nil {
		return fmt.Errorf("render: %w", err)
	}
	_, werr := f.Write(content)
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("render: write %s: %w", p, werr)
	}
	return nil
}

// outsideSource returns ErrOutputDir when the absolute directory dir is
// root or lies inside it, comparing the paths as written and with the
// symbolic links of their existing parts resolved.
func outsideSource(root, dir string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrOutputDir, err)
	}
	pairs := [][2]string{{rootAbs, dir}, {resolved(rootAbs), resolved(dir)}}
	for _, p := range pairs {
		if within(p[0], p[1]) {
			return fmt.Errorf("%w: %s is inside the source Bundle %s", ErrOutputDir, dir, rootAbs)
		}
	}
	return nil
}

// within reports whether the absolute path p is base or lies below it.
func within(base, p string) bool {
	rel, err := filepath.Rel(base, p)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}

// resolved returns the absolute path p with the symbolic links of its
// longest existing prefix resolved and the rest appended, or p when even
// the root cannot be resolved.
func resolved(p string) string {
	var rest []string
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			slices.Reverse(rest)
			return filepath.Join(append([]string{r}, rest...)...)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = append(rest, filepath.Base(cur))
		cur = parent
	}
}
