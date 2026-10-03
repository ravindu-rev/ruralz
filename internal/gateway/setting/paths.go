// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package setting

import (
	"io/fs"
	"os"
	"path/filepath"
)

// within reports whether path is root or lies inside it. Both are
// compared cleaned and absolute and, where they or a parent exist,
// symlink-resolved, so a root or a protected path reached through a
// symbolic link cannot slip past (spec 04 requirement 3, spec 01
// requirement 45).
func within(path, root string) bool {
	roots := pathForms(root)
	for _, p := range pathForms(path) {
		for _, r := range roots {
			if rel, err := filepath.Rel(r, p); err == nil && (rel == "." || filepath.IsLocal(rel)) {
				return true
			}
		}
	}
	return false
}

// pathForms returns p cleaned and absolute, plus its symlink-resolved form
// when that differs.
func pathForms(p string) []string {
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if resolved := resolveExisting(p); resolved != p {
		return []string{p, resolved}
	}
	return []string{p}
}

// maxLinkDepth bounds the dangling-link chain resolveExisting follows.
const maxLinkDepth = 40

// resolveExisting resolves symbolic links in the longest existing prefix
// of the absolute path p and appends the rest unchanged; p itself when no
// prefix resolves. A dangling link (a data directory not created yet,
// reached through a link) is followed to its target, so it cannot hide a
// path inside the secret root.
func resolveExisting(p string) string {
	return resolveDepth(p, 0)
}

func resolveDepth(p string, depth int) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		if target, ok := danglingTarget(cur); ok && depth < maxLinkDepth {
			return filepath.Join(resolveDepth(target, depth+1), rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// danglingTarget returns the absolute target of p when p is a symbolic
// link.
func danglingTarget(p string) (string, bool) {
	fi, err := os.Lstat(p)
	if err != nil || fi.Mode()&fs.ModeSymlink == 0 {
		return "", false
	}
	target, err := os.Readlink(p)
	if err != nil {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(p), target)
	}
	return filepath.Clean(target), true
}
