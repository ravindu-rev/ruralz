// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resolver

import (
	"fmt"
	"path/filepath"
	"strings"
)

// checkProtected refuses a secret root that is, or contains, a protected
// path (spec 01 requirement 45, spec 04 requirement 3, spec 06
// requirement 88). Both sides are compared as cleaned absolute paths and,
// where they (or a parent) exist, symlink-resolved, so a root reached
// through a symbolic link, or a protected path reached through one,
// cannot slip past. The error names every offending setting.
func checkProtected(root string, protected []Protected) error {
	roots := pathForms(root)
	var bad []string
	for _, p := range protected {
		if p.Path == "" {
			continue
		}
		for _, form := range pathForms(p.Path) {
			if withinAny(form, roots) {
				bad = append(bad, fmt.Sprintf("%s=%s", p.Setting, p.Path))
				break
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s is %s=%s or inside it; move it out of the secret root",
			ErrProtectedInRoot, strings.Join(bad, ", "), SettingSecretRoot, root)
	}
	return nil
}

// pathForms returns p cleaned and made absolute, plus its symlink-resolved
// form when it differs.
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

// resolveExisting resolves symbolic links in the longest existing prefix
// of the absolute path p and appends the rest unchanged; p itself when no
// prefix resolves.
func resolveExisting(p string) string {
	rest := ""
	for cur := p; ; {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// withinAny reports whether p is one of roots or lies inside one.
func withinAny(p string, roots []string) bool {
	for _, root := range roots {
		if rel, err := filepath.Rel(root, p); err == nil && (rel == "." || filepath.IsLocal(rel)) {
			return true
		}
	}
	return false
}
