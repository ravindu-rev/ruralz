// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// embedded compiles the embedded views once for all tests.
var embedded = sync.OnceValues(func() (*Set, error) {
	idx, err := schemaidx.Embedded()
	if err != nil {
		return nil, err
	}
	return Embedded(idx)
})

// testView returns the compiled ruralz/v1alpha1 view.
func testView(t testing.TB) *View {
	t.Helper()
	s, err := embedded()
	if err != nil {
		t.Fatalf("Embedded() error = %v", err)
	}
	v, ok := s.View("ruralz/v1alpha1")
	if !ok {
		t.Fatal(`View("ruralz/v1alpha1") not found`)
	}
	return v
}

// parseJSON builds a positioned tree from one JSON text, as the strict
// JSON front end does (StyleJSON scalars, positions in code points).
func parseJSON(data []byte, file tree.FileID) (*tree.Node, error) {
	s := jsonval.NewScanner(data, jsonval.ScanOptions{})
	loc := jsonval.NewLocator(data)
	pos := func(off int) tree.Pos {
		p := loc.Position(off)
		return tree.Pos{File: file, Line: int32(p.Line), Column: int32(p.Column)} //nolint:gosec // G115: test inputs are small.
	}
	var value func(tok jsonval.Token) (*tree.Node, error)
	value = func(tok jsonval.Token) (*tree.Node, error) {
		n := &tree.Node{Style: tree.StyleJSON, Pos: pos(tok.Offset)}
		switch tok.Kind {
		case jsonval.KindNull:
			n.Kind = tree.KindNull
		case jsonval.KindTrue, jsonval.KindFalse:
			n.Kind, n.Bool = tree.KindBool, tok.Kind == jsonval.KindTrue
		case jsonval.KindNumber:
			n.Text = s.Text(tok)
			n.Kind = tree.KindInt
			if strings.ContainsAny(n.Text, ".eE") {
				n.Kind = tree.KindFloat
			}
		case jsonval.KindString:
			n.Kind, n.Text = tree.KindString, s.Text(tok)
		case jsonval.KindObjectStart:
			n.Kind = tree.KindMap
			for {
				k, err := s.Next()
				if err != nil {
					return nil, err
				}
				if k.Kind == jsonval.KindObjectEnd {
					return n, nil
				}
				vt, err := s.Next()
				if err != nil {
					return nil, err
				}
				key, keyPos := s.Text(k), pos(k.Offset)
				child, err := value(vt)
				if err != nil {
					return nil, err
				}
				n.Members = append(n.Members, tree.Member{Key: key, KeyPos: keyPos, Value: child})
			}
		case jsonval.KindArrayStart:
			n.Kind = tree.KindList
			n.Items = []*tree.Node{}
			for {
				it, err := s.Next()
				if err != nil {
					return nil, err
				}
				if it.Kind == jsonval.KindArrayEnd {
					return n, nil
				}
				child, err := value(it)
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, child)
			}
		default:
			return nil, errors.New("unexpected token " + tok.Kind.String())
		}
		return n, nil
	}
	tok, err := s.Next()
	if err != nil {
		return nil, err
	}
	n, err := value(tok)
	if err != nil {
		return nil, err
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return n, nil
}

// resourceOf wraps a parsed document as a resource, as stage C does.
func resourceOf(root *tree.Node) *tree.Resource {
	r := &tree.Resource{Root: root}
	if root != nil {
		r.Start = root.Pos
	}
	if k, ok := root.Get("kind"); ok && k.Kind == tree.KindString {
		r.ID.Kind = v1alpha1.Kind(k.Text)
	}
	if a, ok := root.Get("apiVersion"); ok && a.Kind == tree.KindString {
		r.APIVersion = a.Text
	}
	if meta, ok := root.Get("metadata"); ok {
		if n, ok := meta.Get("name"); ok && n.Kind == tree.KindString {
			r.ID.Name = n.Text
		}
	}
	return r
}

// mustResource parses src (a file named name) into a resource.
func mustResource(t testing.TB, files *tree.FileTable, name, src string) *tree.Resource {
	t.Helper()
	id := files.Add(tree.File{Path: name, Role: tree.RoleBase})
	root, err := parseJSON([]byte(src), id)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	return resourceOf(root)
}

// validateJSON validates one JSON resource and returns its diagnostics.
func validateJSON(t testing.TB, src string) diag.List {
	t.Helper()
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", src)
	return testView(t).Validate(res, files)
}

// texts renders diagnostics in their text form, one per line.
func texts(l diag.List) string {
	var b strings.Builder
	if err := diag.WriteText(&b, l); err != nil {
		return err.Error()
	}
	return b.String()
}

// syntheticView compiles testdata/synthetic.schema.json, a small schema
// that uses keywords the rendered view does not (multipleOf, exclusive
// bounds, uniqueItems, generic oneOf, anyOf and not, propertyNames,
// contains, dependentRequired, a false schema) for the generic mappings.
func syntheticView(t testing.TB) *View {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "synthetic.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load("test/v1", data)
	if err != nil {
		t.Fatalf("schemaidx.Load() error = %v", err)
	}
	v, err := Compile(data, idx)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return v
}

// validateWith validates one JSON resource with view v.
func validateWith(t testing.TB, v *View, src string) diag.List {
	t.Helper()
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", src)
	return v.Validate(res, files)
}
