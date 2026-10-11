// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package convert

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// embedded indexes the committed rendered view once per test binary.
var embedded = sync.OnceValues(schemaidx.Embedded)

func hubIndex(t testing.TB) *schemaidx.Index {
	t.Helper()
	set, err := embedded()
	if err != nil {
		t.Fatalf("schemaidx.Embedded: %v", err)
	}
	x, ok := set.Index(HubAPIVersion)
	if !ok {
		t.Fatalf("no index for %s", HubAPIVersion)
	}
	return x
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

// equivalent compares two trees by value with object members in any
// order, as the hub and canonical forms do.
func equivalent(a, b *tree.Node) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Kind != b.Kind || a.Text != b.Text || a.Bool != b.Bool ||
		len(a.Members) != len(b.Members) || len(a.Items) != len(b.Items) {
		return false
	}
	for _, m := range a.Members {
		v, ok := b.Get(m.Key)
		if !ok || !equivalent(m.Value, v) {
			return false
		}
	}
	for i := range a.Items {
		if !equivalent(a.Items[i], b.Items[i]) {
			return false
		}
	}
	return true
}

// samePositions reports whether every node of a has the position of the
// node at the same place in b (b must have a's shape).
func samePositions(a, b *tree.Node) bool {
	if a.Pos != b.Pos {
		return false
	}
	for i := range a.Members {
		if a.Members[i].KeyPos != b.Members[i].KeyPos || !samePositions(a.Members[i].Value, b.Members[i].Value) {
			return false
		}
	}
	for i := range a.Items {
		if !samePositions(a.Items[i], b.Items[i]) {
			return false
		}
	}
	return true
}

// dump renders a tree as compact JSON-like text for failure messages.
func dump(n *tree.Node) string {
	var b strings.Builder
	var walk func(n *tree.Node)
	walk = func(n *tree.Node) {
		switch n.Kind {
		case tree.KindMap:
			b.WriteByte('{')
			for i, m := range n.Members {
				if i > 0 {
					b.WriteByte(',')
				}
				fmt.Fprintf(&b, "%q:", m.Key)
				walk(m.Value)
			}
			b.WriteByte('}')
		case tree.KindList:
			b.WriteByte('[')
			for i, it := range n.Items {
				if i > 0 {
					b.WriteByte(',')
				}
				walk(it)
			}
			b.WriteByte(']')
		case tree.KindString:
			fmt.Fprintf(&b, "%q", n.Text)
		case tree.KindInt, tree.KindFloat:
			b.WriteString(n.Text)
		case tree.KindBool:
			fmt.Fprint(&b, n.Bool)
		default:
			b.WriteString("null")
		}
	}
	if n == nil {
		return "<nil>"
	}
	walk(n)
	return b.String()
}
