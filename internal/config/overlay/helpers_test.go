// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

const v1 = "ruralz/v1alpha1"

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

// pipeline runs stage D as the loader does (01 req 53): CheckBase on every
// base resource, then Apply once.
func pipeline(t testing.TB, base, patches []*tree.Resource, opts Options) ([]*tree.Resource, diag.List) {
	t.Helper()
	var ds diag.List
	for _, r := range base {
		ds = append(ds, CheckBase(r, opts)...)
	}
	out, more, err := Apply(t.Context(), base, patches, opts)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return out, append(ds, more...)
}

// jsonParser builds a tree from JSON text with the 1-based line and
// column (in bytes; fixtures are ASCII) of every key and value.
type jsonParser struct {
	src    string
	origin int
	dec    *json.Decoder
	file   tree.FileID
	style  tree.Style
	lines  []int
}

func (p *jsonParser) pos() tree.Pos {
	off := p.origin + int(p.dec.InputOffset())
	for off < len(p.src) && strings.IndexByte(" \t\r\n,:", p.src[off]) >= 0 {
		off++
	}
	line := sort.Search(len(p.lines), func(i int) bool { return p.lines[i] > off })
	return tree.Pos{File: p.file, Line: int32(line), Column: int32(off - p.lines[line-1] + 1)} //nolint:gosec // G115: test fixtures are small.
}

func (p *jsonParser) value() (*tree.Node, error) {
	at := p.pos()
	tok, err := p.dec.Token()
	if err != nil {
		return nil, err
	}
	n := &tree.Node{Pos: at, Style: p.style}
	switch v := tok.(type) {
	case json.Delim:
		switch v {
		case '{':
			n.Kind, n.Style = tree.KindMap, tree.StylePlain
			for p.dec.More() {
				kp := p.pos()
				kt, err := p.dec.Token()
				if err != nil {
					return nil, err
				}
				val, err := p.value()
				if err != nil {
					return nil, err
				}
				n.Members = append(n.Members, tree.Member{Key: kt.(string), KeyPos: kp, Value: val})
			}
		case '[':
			n.Kind, n.Style = tree.KindList, tree.StylePlain
			for p.dec.More() {
				val, err := p.value()
				if err != nil {
					return nil, err
				}
				n.Items = append(n.Items, val)
			}
		default:
			return nil, errors.New("unexpected delimiter")
		}
		if _, err := p.dec.Token(); err != nil {
			return nil, err
		}
	case string:
		n.Kind, n.Text = tree.KindString, v
	case json.Number:
		n.Kind, n.Text = tree.KindFloat, string(v)
		if i, err := strconv.ParseInt(string(v), 10, 64); err == nil {
			n.Kind, n.Text = tree.KindInt, strconv.FormatInt(i, 10)
		}
	case bool:
		n.Kind, n.Bool = tree.KindBool, v
	case nil:
		n.Kind = tree.KindNull
	}
	return n, nil
}

func parseJSON(src string, file tree.FileID) (*tree.Node, error) {
	return parseRange(src, 0, len(src), file)
}

// parseRange parses src[from:to] with positions counted in all of src.
func parseRange(src string, from, to int, file tree.FileID) (*tree.Node, error) {
	p := &jsonParser{src: src, origin: from, file: file, style: tree.StyleDoubleQuoted, lines: []int{0}}
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			p.lines = append(p.lines, i+1)
		}
	}
	p.dec = json.NewDecoder(strings.NewReader(src[from:to]))
	p.dec.UseNumber()
	n, err := p.value()
	if err != nil {
		return nil, err
	}
	if _, err := p.dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing data")
	}
	return n, nil
}

// docs parses the resource documents of file path, separated by lines
// holding "---", into files.
func docs(t testing.TB, files *tree.FileTable, role tree.Role, path, src string) []*tree.Resource {
	t.Helper()
	rs, err := docsErr(files, role, path, src)
	if err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	return rs
}

func docsErr(files *tree.FileTable, role tree.Role, path, src string) ([]*tree.Resource, error) {
	id := files.Add(tree.File{Path: path, Role: role})
	var out []*tree.Resource
	from := 0
	for from <= len(src) {
		to := strings.Index(src[from:], "\n---\n")
		next := len(src) + 1
		if to < 0 {
			to = len(src)
		} else {
			to += from
			next = to + len("\n---\n")
		}
		if strings.TrimSpace(src[from:to]) != "" {
			root, err := parseRange(src, from, to, id)
			if err != nil {
				return nil, err
			}
			if root.Kind != tree.KindMap {
				return nil, errors.New("document root is not an object")
			}
			out = append(out, resourceOf(root))
		}
		from = next
	}
	return out, nil
}

// resourceOf reads the envelope of root as the loader does.
func resourceOf(root *tree.Node) *tree.Resource {
	r := &tree.Resource{Root: root, Start: root.Pos}
	if v, ok := root.Get("apiVersion"); ok {
		r.APIVersion = v.Text
	}
	if v, ok := root.Get("kind"); ok {
		r.ID.Kind = v1alpha1.Kind(v.Text)
	}
	md, _ := root.Get("metadata")
	if v, ok := md.Get("name"); ok {
		r.ID.Name = v.Text
	}
	return r
}

// dump writes n as JSON keeping member order, so tests compare structure
// and order but not positions.
func dump(n *tree.Node) string {
	var b bytes.Buffer
	dumpTo(&b, n)
	return b.String()
}

func dumpTo(b *bytes.Buffer, n *tree.Node) {
	if n == nil {
		b.WriteString("<nil>")
		return
	}
	switch n.Kind {
	case tree.KindNull:
		b.WriteString("null")
	case tree.KindBool:
		b.WriteString(strconv.FormatBool(n.Bool))
	case tree.KindInt, tree.KindFloat:
		b.WriteString(n.Text)
	case tree.KindString:
		q, _ := json.Marshal(n.Text)
		b.Write(q)
	case tree.KindMap:
		b.WriteByte('{')
		for i, m := range n.Members {
			if i > 0 {
				b.WriteByte(',')
			}
			q, _ := json.Marshal(m.Key)
			b.Write(q)
			b.WriteByte(':')
			dumpTo(b, m.Value)
		}
		b.WriteByte('}')
	case tree.KindList:
		b.WriteByte('[')
		for i, it := range n.Items {
			if i > 0 {
				b.WriteByte(',')
			}
			dumpTo(b, it)
		}
		b.WriteByte(']')
	}
}

// compact re-encodes JSON text without whitespace, keeping member order.
func compact(t testing.TB, src string) string {
	t.Helper()
	n, err := parseJSON(src, 0)
	if err != nil {
		t.Fatalf("compact(%s): %v", src, err)
	}
	return dump(n)
}

// texts returns the text form of each diagnostic, sorted.
func texts(l diag.List) []string {
	l = append(diag.List(nil), l...)
	l.Sort()
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = string(d.AppendText(nil))
	}
	return out
}

// codes returns the diagnostic codes in sorted diagnostic order.
func codes(l diag.List) []string {
	l = append(diag.List(nil), l...)
	l.Sort()
	out := make([]string, len(l))
	for i, d := range l {
		out[i] = d.Code
	}
	return out
}

// at returns the node at a path given in the human form's field names,
// such as "spec.listeners.0.port" (numbers index lists).
func at(t testing.TB, n *tree.Node, path string) *tree.Node {
	t.Helper()
	cur := n
	for _, seg := range strings.Split(path, ".") {
		if i, err := strconv.Atoi(seg); err == nil && cur.Kind == tree.KindList {
			if i >= len(cur.Items) {
				t.Fatalf("at(%s): index %d out of range", path, i)
			}
			cur = cur.Items[i]
			continue
		}
		next, ok := cur.Get(seg)
		if !ok {
			t.Fatalf("at(%s): no member %q in %s", path, seg, dump(cur))
		}
		cur = next
	}
	return cur
}

// schemaSet returns the embedded set, optionally with a second served
// version (the committed view relabeled) for RZ-CFG-030 tests (01 risk
// 17).
func schemaSet(t testing.TB, extra ...string) *schemaidx.Set {
	t.Helper()
	if len(extra) == 0 {
		return schemas(t)
	}
	x1, ok := schemas(t).Index(v1)
	if !ok {
		t.Fatal("no ruralz/v1alpha1 index")
	}
	idx := []*schemaidx.Index{x1}
	for _, v := range extra {
		src := bytes.ReplaceAll(schema.RenderedV1alpha1(), []byte(`"`+v1+`"`), []byte(strconv.Quote(v)))
		x, err := schemaidx.Load(v, src)
		if err != nil {
			t.Fatalf("Load(%s): %v", v, err)
		}
		idx = append(idx, x)
	}
	s, err := schemaidx.NewSet(idx...)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
