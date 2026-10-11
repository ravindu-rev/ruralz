// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const v1 = v1alpha1.APIVersion

// embedded indexes the committed rendered view once per test binary.
var embedded = sync.OnceValues(schemaidx.Embedded)

func schemas(t testing.TB) *schemaidx.Set {
	t.Helper()
	s, err := embedded()
	if err != nil {
		t.Fatalf("schemaidx.Embedded: %v", err)
	}
	return s
}

func hubIndex(t testing.TB) *schemaidx.Index {
	t.Helper()
	x, ok := schemas(t).Index(v1)
	if !ok {
		t.Fatalf("no index for %s", v1)
	}
	return x
}

func newStage(t testing.TB) *Stage {
	t.Helper()
	s, err := New(Options{Schemas: schemas(t), Registry: registry.New()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// fixture holds the file table of one test's resources.
type fixture struct {
	files *tree.FileTable
}

func newFixture() *fixture { return &fixture{files: &tree.FileTable{}} }

// resource parses a JSON resource envelope as file path with role base.
func (f *fixture) resource(t testing.TB, path, src string) *tree.Resource {
	t.Helper()
	return f.resourceRole(t, path, src, tree.RoleBase)
}

// resourceRole parses a JSON resource envelope as file path with role.
func (f *fixture) resourceRole(t testing.TB, path, src string, role tree.Role) *tree.Resource {
	t.Helper()
	fid := f.files.Add(tree.File{Path: path, Role: role})
	root, err := parse(src, fid)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return envelope(root)
}

// envelope builds the tree.Resource of a parsed envelope.
func envelope(root *tree.Node) *tree.Resource {
	r := &tree.Resource{Root: root, Start: root.Pos}
	if v, ok := root.Get("apiVersion"); ok {
		r.APIVersion = v.Text
	}
	if k, ok := root.Get("kind"); ok {
		r.ID.Kind = v1alpha1.Kind(k.Text)
	}
	if m, ok := root.Get("metadata"); ok {
		if n, ok := m.Get("name"); ok {
			r.ID.Name = n.Text
		}
	}
	return r
}

// parse builds a positioned tree from JSON text as the profile's JSON
// front end does: an integer literal (no '.', 'e' or 'E') is KindInt,
// every other number KindFloat with its text.
func parse(src string, file tree.FileID) (*tree.Node, error) {
	p := &parser{s: jsonval.NewScanner([]byte(src), jsonval.ScanOptions{}), loc: jsonval.NewLocator([]byte(src)), file: file}
	t, err := p.s.Next()
	if err != nil {
		return nil, err
	}
	n, err := p.value(t)
	if err != nil {
		return nil, err
	}
	if _, err := p.s.Next(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("trailing data: %w", err)
	}
	return n, nil
}

func mustParse(t testing.TB, src string) *tree.Node {
	t.Helper()
	n, err := parse(src, 0)
	if err != nil {
		t.Fatalf("parse %q: %v", src, err)
	}
	return n
}

type parser struct {
	s    *jsonval.Scanner
	loc  *jsonval.Locator
	file tree.FileID
}

func (p *parser) pos(off int) tree.Pos {
	q := p.loc.Position(off)
	return tree.Pos{File: p.file, Line: int32(q.Line), Column: int32(q.Column)} //nolint:gosec // G115: test inputs are small.
}

func (p *parser) value(t jsonval.Token) (*tree.Node, error) {
	n := &tree.Node{Pos: p.pos(t.Offset), Style: tree.StyleJSON}
	switch t.Kind {
	case jsonval.KindNull:
		n.Kind = tree.KindNull
	case jsonval.KindTrue, jsonval.KindFalse:
		n.Kind, n.Bool = tree.KindBool, t.Kind == jsonval.KindTrue
	case jsonval.KindNumber:
		n.Text = p.s.Text(t)
		n.Kind = tree.KindFloat
		if !strings.ContainsAny(n.Text, ".eE") {
			n.Kind = tree.KindInt
		}
	case jsonval.KindString:
		n.Kind, n.Text = tree.KindString, p.s.Text(t)
	case jsonval.KindObjectStart:
		n.Kind = tree.KindMap
		for {
			k, err := p.s.Next()
			if err != nil {
				return nil, err
			}
			if k.Kind == jsonval.KindObjectEnd {
				return n, nil
			}
			key, keyPos := p.s.Text(k), p.pos(k.Offset)
			v, err := p.s.Next()
			if err != nil {
				return nil, err
			}
			val, err := p.value(v)
			if err != nil {
				return nil, err
			}
			n.Members = append(n.Members, tree.Member{Key: key, KeyPos: keyPos, Value: val})
		}
	case jsonval.KindArrayStart:
		n.Kind = tree.KindList
		n.Items = []*tree.Node{}
		for {
			v, err := p.s.Next()
			if err != nil {
				return nil, err
			}
			if v.Kind == jsonval.KindArrayEnd {
				return n, nil
			}
			val, err := p.value(v)
			if err != nil {
				return nil, err
			}
			n.Items = append(n.Items, val)
		}
	default:
		return nil, fmt.Errorf("unexpected token %v", t.Kind)
	}
	return n, nil
}

// equal compares two trees by value and member order, ignoring positions
// and styles.
func equal(a, b *tree.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Text != b.Text || a.Bool != b.Bool ||
		len(a.Members) != len(b.Members) || len(a.Items) != len(b.Items) {
		return false
	}
	for i := range a.Members {
		if a.Members[i].Key != b.Members[i].Key || !equal(a.Members[i].Value, b.Members[i].Value) {
			return false
		}
	}
	for i := range a.Items {
		if !equal(a.Items[i], b.Items[i]) {
			return false
		}
	}
	return true
}

// dump renders a tree as compact JSON text (members in tree order), for
// goldens and failure messages.
func dump(n *tree.Node) string {
	if n == nil {
		return "<nil>"
	}
	b, err := appendJSON(nil, n, nil, nil)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

// at returns the node at a dotted path of member names and [i] indexes.
func at(t testing.TB, root *tree.Node, path string) *tree.Node {
	t.Helper()
	n, ok := lookup(root, path)
	if !ok {
		t.Fatalf("no node at %s in %s", path, dump(root))
	}
	return n
}

func lookup(root *tree.Node, path string) (*tree.Node, bool) {
	cur := root
	for _, part := range strings.Split(path, ".") {
		name, idx, hasIdx := strings.Cut(part, "[")
		var ok bool
		if name != "" {
			if cur, ok = cur.Get(name); !ok {
				return nil, false
			}
		}
		if hasIdx {
			var i int
			if _, err := fmt.Sscanf(idx, "%d]", &i); err != nil || cur.Kind != tree.KindList || i >= len(cur.Items) {
				return nil, false
			}
			cur = cur.Items[i]
		}
	}
	return cur, true
}

// texts renders diagnostics in their one-line text form.
func texts(ds diag.List) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = string(d.AppendText(nil))
	}
	return out
}
