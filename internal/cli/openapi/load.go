// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package openapi

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Limits bound one load: the root document and every file its references
// name, together. A zero field takes its default, the limit of the same
// name of one Bundle load (internal/config/loader): 64 MiB of source text,
// 20,000 files, 64 nesting levels, 1,000,000 nodes in one document and a
// node budget of 4,000,000 across the files.
type Limits struct {
	// MaxSourceBytes bounds the bytes of every file the load reads.
	MaxSourceBytes int64
	// MaxFiles bounds the files the load reads.
	MaxFiles int
	// MaxDepth bounds the nesting of a document; its root mapping is depth
	// 1.
	MaxDepth int
	// MaxDocumentNodes bounds the nodes of one document.
	MaxDocumentNodes int
	// MaxNodes is the node budget the files of one load share.
	MaxNodes int
	// MaxProblems bounds the problems one load reports; the load stops at
	// the first past it.
	MaxProblems int
}

// DefaultMaxProblems is the default of Limits.MaxProblems, the diagnostics
// cap of a Bundle run.
const DefaultMaxProblems = loader.DefaultMaxDiagnostics

// withDefaults returns l with every zero field set to its default.
func (l Limits) withDefaults() Limits {
	l.MaxSourceBytes = cmp.Or(l.MaxSourceBytes, loader.DefaultMaxSourceBytes)
	l.MaxFiles = cmp.Or(l.MaxFiles, loader.DefaultMaxFiles)
	l.MaxDepth = cmp.Or(l.MaxDepth, loader.DefaultMaxDepth)
	l.MaxDocumentNodes = cmp.Or(l.MaxDocumentNodes, loader.DefaultMaxDocumentNodes)
	l.MaxNodes = cmp.Or(l.MaxNodes, loader.DefaultMaxResources*loader.NodesPerResource)
	l.MaxProblems = cmp.Or(l.MaxProblems, DefaultMaxProblems)
	return l
}

// FS reads the files of a load. ReadFile returns at most limit+1 bytes of
// the regular file name, an operating system path as the load joined it,
// so a file past the limit is told from one at it.
type FS interface {
	ReadFile(name string, limit int64) ([]byte, error)
}

// OSFS reads regular files of the operating system.
type OSFS struct{}

// ErrNotRegular reports a path that names a directory, a device, a pipe or
// another file that is not a regular file.
var ErrNotRegular = errors.New("not a regular file")

// ReadFile reads the regular file name, at most limit+1 bytes of it. It
// checks the file before opening it, so a named pipe is never opened.
func (OSFS) ReadFile(name string, limit int64) ([]byte, error) {
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, ErrNotRegular
	}
	f, err := os.Open(name) //nolint:gosec // G304: the document the user names and the files its references name.
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(io.LimitReader(f, limit+1))
}

// Options configure Load.
type Options struct {
	// Limits bound the load.
	Limits Limits
	// FS reads the files; nil is OSFS.
	FS FS
}

// Problem is one finding of a load: where it is and what is wrong. A
// problem without a file is about the load as a whole.
type Problem struct {
	diag.Location
	// Message is one line.
	Message string
}

// String returns "<file>:<line>:<column>: <message>", the location as far
// as it is known.
func (p Problem) String() string {
	if p.File == "" {
		return p.Message
	}
	s := p.File
	if p.Line > 0 {
		s += ":" + strconv.Itoa(p.Line)
		if p.Column > 0 {
			s += ":" + strconv.Itoa(p.Column)
		}
	}
	return s + ": " + p.Message
}

// Error is a load that found problems: the document is not a readable
// OpenAPI 3.0 or 3.1 document. Problems are in the order the load met
// them.
type Error struct {
	// Problems are the findings, at most Limits.MaxProblems plus one.
	Problems []Problem
}

// Error returns the first problem and the count of the others.
func (e *Error) Error() string {
	switch len(e.Problems) {
	case 0:
		return "openapi: invalid document"
	case 1:
		return e.Problems[0].String()
	default:
		return fmt.Sprintf("%s (and %d more problems)", e.Problems[0].String(), len(e.Problems)-1)
	}
}

// ReadError is a root document that cannot be read: missing, unreadable
// or not a regular file. Its references' failures are problems instead.
type ReadError struct {
	// Path is the root document's path as given.
	Path string
	// Err is the cause.
	Err error
}

// Error names the path and the cause.
func (e *ReadError) Error() string { return "cannot read " + e.Path + ": " + causeText(e.Err) }

// Unwrap returns the cause.
func (e *ReadError) Unwrap() error { return e.Err }

// causeText is the message of an I/O error without the path the
// operating system repeats in it.
func causeText(err error) string {
	if pe := (*fs.PathError)(nil); errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// errStop ends a load at a limit or the problems cap; the problems say
// why.
var errStop = errors.New("openapi: load stopped")

// file is one file of a load.
type file struct {
	// id is the file's index in the load's file table.
	id tree.FileID
	// path is the operating system path the load read.
	path string
	// root is the file's one document, or nil when it failed.
	root *tree.Node
}

// load is the state of one Load call.
type load struct {
	ctx      context.Context
	fs       FS
	limits   Limits
	files    *tree.FileTable
	byPath   map[string]*file
	byID     []*file
	read     int64
	budget   *profile.Budget
	problems []Problem
}

// newLoad returns the state of a load under o.
func newLoad(ctx context.Context, o Options) *load {
	l := o.Limits.withDefaults()
	f := o.FS
	if f == nil {
		f = OSFS{}
	}
	return &load{
		ctx: ctx, fs: f, limits: l, files: &tree.FileTable{}, byPath: map[string]*file{},
		budget: profile.NewBudget(l.MaxNodes, "the files of the load hold more than "+strconv.Itoa(l.MaxNodes)+" YAML and JSON nodes"),
	}
}

// add records a problem; past the cap it stops the load.
func (l *load) add(at diag.Location, format string, args ...any) {
	if len(l.problems) > l.limits.MaxProblems {
		return
	}
	l.problems = append(l.problems, Problem{Location: at, Message: fmt.Sprintf(format, args...)})
}

// full reports whether the problems passed the cap.
func (l *load) full() bool { return len(l.problems) > l.limits.MaxProblems }

// displayName is the name of an operating system path in problems: slash
// separated on every OS, as configuration diagnostics name files.
func displayName(path string) string { return filepath.ToSlash(path) }

// at returns the location of n.
func (l *load) at(n *tree.Node) diag.Location {
	if n == nil {
		return diag.Location{}
	}
	return l.files.Location(n.Pos)
}

// fileAt returns the file a node of the load came from.
func (l *load) fileAt(n *tree.Node) *file {
	if n == nil || int(n.Pos.File) >= len(l.byID) {
		return nil
	}
	return l.byID[n.Pos.File]
}

// open returns the file at the cleaned operating system path p, reading
// and parsing it on first use. from is where a reference named it, the
// location of a failure to read it; the root has none. A file that fails
// is cached as failed, its problems reported once. A limit or the
// problems cap is errStop; ctx's end is ctx.Err().
func (l *load) open(p string, from diag.Location, root bool) (*file, error) {
	if err := l.ctx.Err(); err != nil {
		return nil, err
	}
	if f, ok := l.byPath[p]; ok {
		return f, nil
	}
	if len(l.byID) >= l.limits.MaxFiles {
		l.add(from, "the load reads more than %d files", l.limits.MaxFiles)
		return nil, errStop
	}
	name := displayName(p)
	f := &file{path: p}
	f.id = l.files.Add(tree.File{Path: name})
	l.byPath[p] = f
	l.byID = append(l.byID, f)
	left := l.limits.MaxSourceBytes - l.read
	data, err := l.fs.ReadFile(p, left)
	if err != nil {
		if root {
			return nil, &ReadError{Path: name, Err: err}
		}
		l.add(from, "cannot read %s: %s", name, causeText(err))
		return f, nil
	}
	if int64(len(data)) > left {
		l.add(diag.Location{File: name}, "the load reads more than %d bytes of source text", l.limits.MaxSourceBytes)
		return nil, errStop
	}
	l.read += int64(len(data))
	format, ok := profile.FormatOf(name)
	if !ok {
		// A name without .yaml, .yml or .json reads as YAML, as a one-file
		// Bundle does; YAML 1.2 reads JSON text too.
		format = profile.FormatYAML
	}
	docs, ds, err := profile.Parse(l.ctx, data, f.id, name, format, profile.Options{
		MaxBytes:          int(min(l.limits.MaxSourceBytes, int64(^uint(0)>>1))),
		MaxDepth:          l.limits.MaxDepth,
		MaxDocumentTokens: l.limits.MaxDocumentNodes,
		Budget:            l.budget,
		MaxDiagnostics:    l.limits.MaxProblems,
	})
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		l.add(d.Location, "%s", d.Message)
	}
	switch {
	case l.budget.Exceeded():
		return nil, errStop
	case len(ds) > 0:
	case len(docs) == 0:
		l.add(diag.Location{File: name}, "the file holds no document")
	case len(docs) > 1:
		l.add(l.files.Location(docs[1].Start), "the file holds %d YAML documents; an OpenAPI file holds one", len(docs))
	default:
		f.root = docs[0].Root
	}
	if l.full() {
		return nil, errStop
	}
	return f, nil
}

// sortedProblems returns the problems in file, line, column and message
// order: the order a reader fixes them in.
func (l *load) sortedProblems() []Problem {
	out := slices.Clone(l.problems)
	slices.SortStableFunc(out, func(a, b Problem) int {
		return cmp.Or(
			cmp.Compare(a.File, b.File), cmp.Compare(a.Line, b.Line), cmp.Compare(a.Column, b.Column),
			strings.Compare(a.Message, b.Message),
		)
	})
	return slices.CompactFunc(out, func(a, b Problem) bool { return a == b })
}
