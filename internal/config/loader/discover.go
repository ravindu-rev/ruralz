// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
)

// readDirBatch is the directory entries read at once, so a directory with
// many entries that are not configuration files costs little memory.
const readDirBatch = 256

// candidate is a configuration file stage A found.
type candidate struct {
	// path is slash-separated and relative to the Bundle root.
	path string
	// overlay names the overlay that holds the file; "" for a base file.
	overlay string
}

// finding is a stage A diagnostic with the overlay it belongs to, so a
// render reports an overlay's findings only when it selects the overlay.
// lost reports an entry that may hold documents the load cannot read.
type finding struct {
	d       diag.Diagnostic
	overlay string
	lost    bool
}

// ignoreFrame is the rules of one .ruralzignore and the directory they
// apply below: base is "" for the root, else the directory and a slash.
type ignoreFrame struct {
	base  string
	rules ignoreList
}

// walker is one stage A walk over a Bundle directory.
type walker struct {
	ctx      context.Context
	fsys     fs.FS
	maxFiles int
	files    []candidate
	findings []finding
	// overlays holds every overlay directory with a valid name.
	overlays map[string]bool
	// stack holds the .ruralzignore rules of the directories on the path
	// the walk is in, the root's first.
	stack []ignoreFrame
	// used counts the source bytes of the .ruralzignore files read, of
	// maxBytes; patterns counts their patterns, of maxPatterns.
	used, maxBytes        int64
	patterns, maxPatterns int
	// stop is the RZ-CFG-001 of a limit the walk passed: the files, the
	// source bytes or the ignore patterns. The walk ends there.
	stop *diag.Diagnostic
	// rootIgnored reports that a .ruralzignore excludes ruralz.yaml.
	rootIgnored bool
}

// isConfig reports whether a file name has a configuration extension.
func isConfig(name string) bool {
	_, ok := profile.FormatOf(name)
	return ok
}

// join returns the root-relative path of name in dir; "" is the root.
func join(dir, name string) string {
	if dir == "" {
		return name
	}
	return dir + "/" + name
}

// fsPath returns the fs.FS name of a root-relative path.
func fsPath(rel string) string {
	if rel == "" {
		return "."
	}
	return rel
}

// problem records a finding of the overlay owner, "" for the base, of an
// entry whose documents the load loses.
func (w *walker) problem(owner string, d diag.Diagnostic) {
	w.findings = append(w.findings, finding{overlay: owner, d: d, lost: true})
}

// entryDiagnostic is the RZ-CFG-001 of an entry a Bundle cannot hold: a
// symbolic link, or an entry that is neither a regular file nor a
// directory.
func entryDiagnostic(rel string, t fs.FileMode) diag.Diagnostic {
	d := diag.Diagnostic{Code: CodeLimit, Severity: diag.SeverityError, Location: diag.Location{File: rel}}
	if t&fs.ModeSymlink != 0 {
		d.Message = "symbolic link: a Bundle holds only regular files and directories"
		d.Hint = "Ruralz Control reads the Bundle from Git, where a link has no file content; commit the file itself"
		return d
	}
	d.Message = "not a regular file or directory: a Bundle holds only regular files and directories"
	return d
}

// submoduleDiagnostic is the RZ-CFG-001 of a Git submodule: a directory
// below the root that holds .git.
func submoduleDiagnostic(rel string) diag.Diagnostic {
	return diag.Diagnostic{
		Code: CodeLimit, Severity: diag.SeverityError, Location: diag.Location{File: rel},
		Message: "Git submodule (the directory holds .git): a Bundle holds only regular files and directories",
		Hint:    "Ruralz Control reads the Bundle from one Git tree, where a submodule has no files",
	}
}

// listing is what discovery looks at in one directory.
type listing struct {
	// entries are the directories, configuration files and entries that
	// are neither regular files nor directories, sorted by name; hidden
	// entries are left out.
	entries []fs.DirEntry
	// gitlink reports a .git entry, which marks a Git submodule below the
	// root.
	gitlink bool
	// ignore is the directory's .ruralzignore entry, or nil.
	ignore fs.DirEntry
}

// list reads the entries of dir in batches, keeping only what discovery
// looks at.
func (w *walker) list(dir string) (listing, error) {
	var out listing
	f, err := w.fsys.Open(fsPath(dir))
	if err != nil {
		return out, err
	}
	defer func() { _ = f.Close() }()
	rd, ok := f.(fs.ReadDirFile)
	if !ok {
		return out, fmt.Errorf("%s: %w", fsPath(dir), errors.ErrUnsupported)
	}
	for {
		if err := w.ctx.Err(); err != nil {
			return out, err
		}
		batch, err := rd.ReadDir(readDirBatch)
		for _, e := range batch {
			name := e.Name()
			switch {
			case name == ".git":
				out.gitlink = true
			case name == IgnoreFile:
				out.ignore = e
			case strings.HasPrefix(name, "."):
			case e.Type().IsRegular() && !isConfig(name):
			default:
				out.entries = append(out.entries, e)
			}
		}
		if errors.Is(err, io.EOF) || (err == nil && len(batch) == 0) {
			break
		}
		if err != nil {
			return out, err
		}
	}
	slices.SortFunc(out.entries, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return out, nil
}

// enter reads the listing of dir and pushes the rules of its
// .ruralzignore. ok is false when the directory is a Git submodule, whose
// entries the walk skips, or when a limit stopped the walk; leave pops
// what enter pushed.
func (w *walker) enter(dir, owner string) (l listing, pushed, ok bool, err error) {
	if l, err = w.list(dir); err != nil {
		return l, false, false, err
	}
	if l.gitlink && dir != "" {
		w.problem(owner, submoduleDiagnostic(dir))
		return l, false, false, nil
	}
	if l.ignore != nil {
		if pushed, err = w.readIgnore(dir, owner, l.ignore); err != nil || w.stop != nil {
			return l, pushed, false, err
		}
	}
	return l, pushed, true, nil
}

// leave pops the rules enter pushed.
func (w *walker) leave(pushed bool) {
	if pushed {
		w.stack = w.stack[:len(w.stack)-1]
	}
}

// readIgnore reads the .ruralzignore of dir, against the source and
// pattern budgets of the load, and pushes its rules. A .ruralzignore that
// is not a regular file is RZ-CFG-001 and adds no rule.
func (w *walker) readIgnore(dir, owner string, e fs.DirEntry) (pushed bool, err error) {
	rel := join(dir, IgnoreFile)
	if t := e.Type(); !t.IsRegular() {
		d := entryDiagnostic(rel, t)
		if t.IsDir() {
			d.Message = "directory: " + IgnoreFile + " must be a regular file of patterns"
		}
		w.findings = append(w.findings, finding{overlay: owner, d: d})
		return false, nil
	}
	left := w.maxBytes - w.used
	data, over, err := readFile(w.fsys, rel, left)
	switch {
	case errors.Is(err, errNotRegular):
		w.findings = append(w.findings, finding{overlay: owner, d: entryDiagnostic(rel, fs.ModeIrregular)})
		return false, nil
	case err != nil:
		return false, err
	case over:
		d := sizeLimit(rel, data, left, w.maxBytes)
		w.stop = &d
		return false, nil
	}
	w.used += int64(len(data))
	rules, n, d := parseIgnore(data, rel, w.patterns, w.maxPatterns)
	if d != nil {
		w.stop = d
		return false, nil
	}
	w.patterns += n
	base := ""
	if dir != "" {
		base = dir + "/"
	}
	w.stack = append(w.stack, ignoreFrame{base: base, rules: rules})
	return true, nil
}

// ignored reports whether the .ruralzignore files of the directories above
// rel exclude it, as Git reads them: a deeper file's rules override a
// shallower file's, and within a file the last matching rule wins; each
// file matches the path relative to its own directory.
func (w *walker) ignored(rel string, dir bool) bool {
	for i := len(w.stack) - 1; i >= 0; i-- {
		f := w.stack[i]
		if ignored, matched := f.rules.match(rel[len(f.base):], dir); matched {
			return ignored
		}
	}
	return false
}

// walk visits dir, a directory of the base (overlay "") or of an overlay,
// depth first in name order, and records its configuration files.
func (w *walker) walk(dir, owner string) error {
	l, pushed, ok, err := w.enter(dir, owner)
	defer w.leave(pushed)
	if err != nil || !ok {
		return err
	}
	for _, e := range l.entries {
		if w.stop != nil {
			return nil
		}
		name, t := e.Name(), e.Type()
		rel := join(dir, name)
		if w.ignored(rel, t.IsDir()) {
			if dir == "" && name == RootFile {
				w.rootIgnored = true
			}
			continue
		}
		if dir == "" && name == OverlayDir && t.IsDir() {
			if err := w.walkOverlays(); err != nil {
				return err
			}
			continue
		}
		switch {
		case t.IsDir():
			if err := w.walk(rel, owner); err != nil {
				return err
			}
		case t.IsRegular():
			w.add(rel, owner)
		default:
			w.problem(owner, entryDiagnostic(rel, t))
		}
	}
	return nil
}

// walkOverlays visits overlays/: each subdirectory with a valid overlay
// name is an overlay. A file directly in overlays/ belongs to no overlay
// and is not read; overlays/.ruralzignore applies to every overlay.
func (w *walker) walkOverlays() error {
	l, pushed, ok, err := w.enter(OverlayDir, "")
	defer w.leave(pushed)
	if err != nil || !ok {
		return err
	}
	for _, e := range l.entries {
		if w.stop != nil {
			return nil
		}
		name, t := e.Name(), e.Type()
		rel := join(OverlayDir, name)
		if !overlay.ValidName(name) || w.ignored(rel, t.IsDir()) {
			continue
		}
		switch {
		case t.IsDir():
			w.overlays[name] = true
			if err := w.walk(rel, name); err != nil {
				return err
			}
		case t.IsRegular():
		default:
			w.overlays[name] = true
			w.problem(name, entryDiagnostic(rel, t))
		}
	}
	return nil
}

// add records a configuration file; the first past maxFiles stops the
// walk with RZ-CFG-001.
func (w *walker) add(rel, owner string) {
	if len(w.files) == w.maxFiles {
		w.stop = &diag.Diagnostic{
			Code: CodeLimit, Severity: diag.SeverityError, Location: diag.Location{File: rel},
			Message: "the Bundle holds more than " + strconv.Itoa(w.maxFiles) + " configuration files",
			Hint:    "exclude files that are not configuration with " + IgnoreFile,
		}
		return
	}
	w.files = append(w.files, candidate{path: rel, overlay: owner})
}
