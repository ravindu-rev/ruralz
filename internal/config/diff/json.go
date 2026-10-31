// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// WriteJSON writes the JSON form to w.
func (d *Diff) WriteJSON(w io.Writer) error {
	_, err := w.Write(d.AppendJSON(nil))
	return err
}

// AppendJSON appends the ruralz.diff.v1 document, indented by two spaces
// with one member or element per line, members in the order the format
// lists them, strings minimally escaped (no HTML escaping) and values in
// canonical form, then a newline. The bytes depend only on d.
func (d *Diff) AppendJSON(dst []byte) []byte {
	w := &jsonWriter{dst: dst}
	w.open('{')
	w.key("format")
	w.str(Format)
	w.key("from")
	w.endpoint(&d.From)
	w.key("to")
	w.endpoint(&d.To)
	if d.Environment != "" {
		w.key("environment")
		w.str(d.Environment)
	}
	w.key("resources")
	w.open('[')
	for i := range d.Resources {
		w.next()
		w.resource(&d.Resources[i])
	}
	w.close(']')
	w.key("effective")
	w.open('[')
	for i := range d.Effective {
		w.next()
		w.chainChange(&d.Effective[i])
	}
	w.close(']')
	w.key("summary")
	w.summary(&d.Summary)
	w.close('}')
	return append(w.dst, '\n')
}

// jsonWriter writes indented JSON. full records, per open container,
// whether it holds an element yet.
type jsonWriter struct {
	dst  []byte
	full []bool
}

// open starts an object or array.
func (w *jsonWriter) open(c byte) {
	w.dst = append(w.dst, c)
	w.full = append(w.full, false)
}

// close ends the innermost container; an empty one stays on one line.
func (w *jsonWriter) close(c byte) {
	full := w.full[len(w.full)-1]
	w.full = w.full[:len(w.full)-1]
	if full {
		w.newline()
	}
	w.dst = append(w.dst, c)
}

// next starts an element of the innermost container.
func (w *jsonWriter) next() {
	if w.full[len(w.full)-1] {
		w.dst = append(w.dst, ',')
	}
	w.full[len(w.full)-1] = true
	w.newline()
}

// newline starts a line at the current depth.
func (w *jsonWriter) newline() {
	w.dst = append(w.dst, '\n')
	w.dst = append(w.dst, strings.Repeat("  ", len(w.full))...)
}

// key starts a member of the innermost object.
func (w *jsonWriter) key(k string) {
	w.next()
	w.str(k)
	w.dst = append(w.dst, ": "...)
}

// str writes a string; invalid UTF-8, which no validated tree holds,
// becomes U+FFFD.
func (w *jsonWriter) str(s string) {
	out, err := jsonval.AppendString(w.dst, s)
	if err != nil {
		out, _ = jsonval.AppendString(w.dst, strings.ToValidUTF8(s, "�"))
	}
	w.dst = out
}

// int writes an integer.
func (w *jsonWriter) int(n int) { w.dst = strconv.AppendInt(w.dst, int64(n), 10) }

// strs writes an array of strings.
func (w *jsonWriter) strs(ss []string) {
	w.open('[')
	for _, s := range ss {
		w.next()
		w.str(s)
	}
	w.close(']')
}

// raw writes compact JSON, such as RFC 8785 output, re-indented.
func (w *jsonWriter) raw(src []byte) {
	inString, escaped := false, false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inString {
			w.dst = append(w.dst, c)
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
			w.dst = append(w.dst, c)
		case '{', '[':
			if i+1 < len(src) && (src[i+1] == '}' || src[i+1] == ']') {
				w.dst = append(w.dst, c, src[i+1])
				i++
				continue
			}
			w.open(c)
			w.full[len(w.full)-1] = true
			w.newline()
		case '}', ']':
			w.close(c)
		case ',':
			w.dst = append(w.dst, ',')
			w.newline()
		case ':':
			w.dst = append(w.dst, ": "...)
		default:
			w.dst = append(w.dst, c)
		}
	}
}

// node writes a value tree in canonical form.
func (w *jsonWriter) node(n *tree.Node) {
	b, err := canonical.AppendNode(nil, n)
	if err != nil {
		// Compare checked every value it shows.
		b = []byte("null")
	}
	w.raw(b)
}

// endpoint writes one side.
func (w *jsonWriter) endpoint(e *Endpoint) {
	w.open('{')
	w.key("revision")
	w.str(short(e.Digest))
	if m := e.Origin.member(); m != "" {
		w.key(m)
		w.str(e.Location)
	}
	w.key("digest")
	if e.Digest.IsZero() {
		w.str("")
	} else {
		w.str(e.Digest.String())
	}
	if e.Environment != "" {
		w.key("environment")
		w.str(e.Environment)
	}
	w.close('}')
}

// resource writes one changed resource.
func (w *jsonWriter) resource(r *Resource) {
	w.open('{')
	w.key("kind")
	w.str(string(r.ID.Kind))
	w.key("name")
	w.str(r.ID.Name)
	w.key("change")
	w.str(r.Change.String())
	if r.Source != "" {
		w.key("source")
		w.str(r.Source)
	}
	w.key("impact")
	w.strs(r.Impact.Strings())
	if f := r.Flags.Strings(); f != nil {
		w.key("flags")
		w.strs(f)
	}
	if r.Change == Modified {
		w.key("ops")
		w.open('[')
		for i := range r.Ops {
			w.next()
			w.op(&r.Ops[i])
		}
		w.close(']')
	}
	w.close('}')
}

// op writes one field operation.
func (w *jsonWriter) op(op *Op) {
	w.open('{')
	w.key("op")
	w.str(op.Kind.String())
	w.key("path")
	w.path(op.Path)
	switch op.Kind {
	case OpMove:
		w.key("from")
		w.int(op.FromIndex)
		w.key("to")
		w.int(op.ToIndex)
	case OpReplace:
		w.key("from")
		w.node(op.From)
		w.key("to")
		w.node(op.To)
	case OpAdd:
		w.key("to")
		w.node(op.To)
	case OpRemove:
		w.key("from")
		w.node(op.From)
	}
	w.close('}')
}

// path writes a key-aware path: strings for fields, integers for atomic
// positions and single-member objects for keyed entries ({"name": "x"})
// and set elements ({"item": value}).
func (w *jsonWriter) path(p diag.Path) {
	w.open('[')
	for _, e := range p {
		w.next()
		switch e.Kind {
		case diag.ElemField:
			w.str(e.Name)
		case diag.ElemIndex:
			w.int(e.Index)
		case diag.ElemKeyed:
			w.open('{')
			w.key(e.KeyField)
			w.str(e.Key)
			w.close('}')
		case diag.ElemItem:
			w.open('{')
			w.key("item")
			w.item(e.Item)
			w.close('}')
		}
	}
	w.close(']')
}

// item writes a set element as schemaidx.ItemElem builds it.
func (w *jsonWriter) item(v any) {
	switch t := v.(type) {
	case string:
		w.str(t)
	case json.Number:
		w.dst = append(w.dst, canonicalNumber(string(t))...)
	case bool:
		w.dst = strconv.AppendBool(w.dst, t)
	case json.RawMessage:
		w.raw(t)
	default:
		w.dst = append(w.dst, "null"...)
	}
}

// chainChange writes one effective change.
func (w *jsonWriter) chainChange(c *ChainChange) {
	w.open('{')
	w.key("route")
	w.str(c.Route)
	w.key("phase")
	w.str(c.Phase.String())
	if c.Leg != "" {
		w.key("leg")
		w.str(c.Leg)
	}
	w.key("op")
	w.str(c.Op.String())
	w.key("policy")
	w.str(c.Policy)
	w.key("filterClass")
	w.str(c.Class.String())
	if c.After != "" {
		w.key("after")
		w.str(c.After)
	}
	w.close('}')
}

// summary writes the summary.
func (w *jsonWriter) summary(s *Summary) {
	w.open('{')
	w.key("changed")
	w.int(s.Changed)
	w.key("added")
	w.int(s.Added)
	w.key("removed")
	w.int(s.Removed)
	w.key("modified")
	w.int(s.Modified)
	w.key("routeChainsChanged")
	w.int(s.RouteChainsChanged)
	w.key("impact")
	w.strs(s.Impact.Strings())
	w.close('}')
}
