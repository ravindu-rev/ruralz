// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// CodeUnserved is RZ-CFG-007: a target or source apiVersion this release
// does not serve.
const CodeUnserved = convert.CodeUnserved

// ErrPath marks a file path Convert or WriteTree refuses: one that is not
// a clean, relative, slash-separated path that stays inside its root on
// every OS, that names no .yaml, .yml or .json file, or that two files
// share.
var ErrPath = errors.New("render: invalid file path")

// SourceFile is one base or overlay file of a source Bundle as stage B
// parses it (profile.Parse): neither merged nor substituted.
type SourceFile struct {
	// Path is slash-separated and relative to the Bundle root, such as
	// routes/orders.yaml or overlays/prod/scale.yaml. Its extension
	// (.yaml, .yml or .json, profile.FormatOf) is the format the file is
	// written back in.
	Path string
	// Documents are the roots of the file's documents in stream order;
	// a .json file has exactly one.
	Documents []*tree.Node
}

// File is one rewritten source file.
type File struct {
	// Path is the SourceFile path, unchanged.
	Path string
	// Content is the file in its format.
	Content []byte
}

// Convert rewrites the base and overlay files of a source Bundle under
// apiVersion target, the first half of ruralz bundle render --api-version.
// Every document is converted from its own apiVersion to target through
// the hub with convert.Registry.Convert, which runs both halves even when
// the two apiVersions are equal, so the whole path runs in M1, where
// ruralz/v1alpha1 is the only served apiVersion. Each is converted as a
// partial document: source documents are unsubstituted and overlay
// documents patch part of a resource, so ${VAR} text, $${ escapes, $patch
// entries and ruralz.io/patch annotations pass through as authored, and
// nothing is merged or substituted. The converter keeps every field it
// does not change in its authored order; comments are lost.
//
// Each file is written back in its own format: a YAML file as a
// restricted-profile stream of its documents in order, a .json file as
// one JSON object indented by two spaces, both verbatim (no $${ escaping,
// since substitution has not run) and ending with a newline. The result
// is sorted by path. An unserved target is one RZ-CFG-007 diagnostic, and
// a document whose apiVersion is not served one RZ-CFG-007 at that
// apiVersion; any error diagnostic returns no files. The inputs are never
// changed: each document is converted as a copy. Invalid paths, duplicate
// paths, a nil or non-mapping document and a .json file without exactly
// one document are ErrPath or ErrInput. ctx is checked between files.
func Convert(ctx context.Context, files []SourceFile, target string, conv *convert.Registry) ([]File, diag.List, error) {
	if conv == nil {
		return nil, nil, fmt.Errorf("%w: no converter registry", ErrOptions)
	}
	if d, bad := conv.CheckServed(target); bad {
		d.Message = "target apiVersion " + strconv.Quote(clip(target)) + " is not served by this release"
		d.Path = nil
		return nil, diag.List{d}, nil
	}
	sorted, err := sourceOrder(files)
	if err != nil {
		return nil, nil, err
	}
	found := diag.NewCollector(DefaultMaxDiagnostics)
	converted := make([][]*tree.Node, len(sorted))
	for i, sf := range sorted {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		converted[i] = make([]*tree.Node, len(sf.Documents))
		for j, doc := range sf.Documents {
			out, ds := convertDocument(conv, sf.Path, doc, target)
			found.AddAll(ds)
			converted[i][j] = out
		}
		if found.Full() {
			break
		}
	}
	if ds := found.List(); ds.HasErrors() {
		return nil, ds, nil
	}
	out := make([]File, len(sorted))
	for i, sf := range sorted {
		content, err := encodeSource(sf.Path, converted[i])
		if err != nil {
			return nil, nil, err
		}
		out[i] = File{Path: sf.Path, Content: content}
	}
	return out, found.List(), nil
}

// sourceOrder returns a copy of files sorted by path, after checking every
// path and document.
func sourceOrder(files []SourceFile) ([]SourceFile, error) {
	out := slices.Clone(files)
	slices.SortFunc(out, func(a, b SourceFile) int { return cmp.Compare(a.Path, b.Path) })
	for i, sf := range out {
		if err := checkPath(sf.Path); err != nil {
			return nil, err
		}
		if i > 0 && out[i-1].Path == sf.Path {
			return nil, fmt.Errorf("%w: two source files %s", ErrPath, strconv.Quote(clip(sf.Path)))
		}
		for j, doc := range sf.Documents {
			if doc == nil || doc.Kind != tree.KindMap {
				return nil, fmt.Errorf("%w: document %d of %s is not a mapping", ErrInput, j+1, sf.Path)
			}
		}
		if f, _ := profile.FormatOf(sf.Path); f == profile.FormatJSON && len(sf.Documents) != 1 {
			return nil, fmt.Errorf("%w: %s holds %d documents, and a .json file holds one", ErrInput, sf.Path, len(sf.Documents))
		}
	}
	return out, nil
}

// checkPath returns ErrPath unless p is a configuration file path that
// stays inside its root on every OS (validPath) with a .yaml, .yml or
// .json extension.
func checkPath(p string) error {
	if !validPath(p) {
		return fmt.Errorf("%w: %s is not a clean relative slash-separated path", ErrPath, strconv.Quote(clip(p)))
	}
	if _, ok := profile.FormatOf(p); !ok {
		return fmt.Errorf("%w: %s is not a .yaml, .yml or .json file", ErrPath, strconv.Quote(clip(p)))
	}
	return nil
}

// validPath reports whether p is a slash-separated path relative to a root
// that stays inside it: not empty, without a backslash or NUL byte, not
// absolute, already clean (no "." or ".." element, no empty element, no
// trailing slash) and local on this OS (filepath.IsLocal, which also
// refuses volume names and reserved Windows names).
func validPath(p string) bool {
	return p != "" && p != "." && !strings.ContainsAny(p, "\\\x00") && !path.IsAbs(p) && path.Clean(p) == p &&
		p != ".." && !strings.HasPrefix(p, "../") && filepath.IsLocal(filepath.FromSlash(p))
}

// convertDocument converts a copy of doc, a document of the file at
// path, to target, and locates the converter's diagnostics in that file.
func convertDocument(conv *convert.Registry, path string, doc *tree.Node, target string) (*tree.Node, diag.List) {
	from, kind := stringMember(doc, "apiVersion"), stringMember(doc, "kind")
	out, ds := conv.Convert(from, target, kind, doc.Clone(), true)
	var res *diag.ResourceID
	if meta, ok := doc.Get("metadata"); ok {
		if name := stringMember(meta, "name"); kind != "" && name != "" {
			res = &diag.ResourceID{Kind: kind, Name: name}
		}
	}
	for i := range ds {
		d := &ds[i]
		if d.Resource == nil {
			d.Resource = res
		}
		if d.File == "" && d.Line == 0 {
			at := doc.Pos
			if n, ok := doc.At(d.Path); ok && len(d.Path) > 0 && n.Pos.Known() {
				at = n.Pos
			}
			d.Location = diag.Location{File: path, Line: int(at.Line), Column: int(at.Column)}
		}
	}
	return out, ds
}

// stringMember returns the string value of member key of n, or "".
func stringMember(n *tree.Node, key string) string {
	if v, ok := n.Get(key); ok && v != nil && v.Kind == tree.KindString {
		return v.Text
	}
	return ""
}

// encodeSource writes the converted documents of the file at p in its
// format, verbatim.
func encodeSource(p string, docs []*tree.Node) ([]byte, error) {
	var buf bytes.Buffer
	var err error
	verbatim := profile.EncodeOptions{Verbatim: true}
	if f, _ := profile.FormatOf(p); f == profile.FormatJSON {
		err = profile.EncodeJSON(&buf, docs[0], verbatim)
	} else {
		err = profile.EncodeWith(&buf, docs, verbatim)
	}
	if err != nil {
		return nil, fmt.Errorf("render: %s: %w", p, err)
	}
	return buf.Bytes(), nil
}
