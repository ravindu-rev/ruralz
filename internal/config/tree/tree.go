// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package tree is the configuration pipeline's only in-memory document
// model: an ordered value tree whose every key and scalar keeps its source
// position. The YAML and JSON front ends produce it; overlay merge,
// substitution, schema validation, defaults, canonicalization and
// rendering all read and rewrite it. Numbers keep their exact text, so no
// value passes through float64 before the canonical encoder.
package tree

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// FileID indexes a FileTable.
type FileID uint32

// Role says why a file was read.
type Role uint8

// File roles.
const (
	// RoleBase is a base Bundle file.
	RoleBase Role = iota + 1
	// RoleOverlay is a file of the selected overlay.
	RoleOverlay
	// RoleEnvironments is an --environments file.
	RoleEnvironments
	// RoleCanonical is ruralz.canonical.v1 content (Last-Known-Good, dumps).
	RoleCanonical
)

// File is one source file.
type File struct {
	// Path is slash-separated and relative to the Bundle root, or as given.
	Path string
	// Role says why the file was read.
	Role Role
}

// FileTable is the append-only table of files read by one load. It is
// safe for concurrent use.
type FileTable struct {
	mu    sync.Mutex
	files []File
}

// Add appends f and returns its ID.
func (t *FileTable) Add(f File) FileID {
	t.mu.Lock()
	defer t.mu.Unlock()
	id := FileID(len(t.files)) //nolint:gosec // G115: a load reads at most 20,000 files.
	t.files = append(t.files, f)
	return id
}

// File returns the file with id; the zero File when id is unknown.
func (t *FileTable) File(id FileID) File {
	t.mu.Lock()
	defer t.mu.Unlock()
	if int(id) >= len(t.files) {
		return File{}
	}
	return t.files[id]
}

// Location converts a position to a diagnostic location.
func (t *FileTable) Location(p Pos) diag.Location {
	if !p.Known() {
		return diag.Location{}
	}
	return diag.Location{File: t.File(p.File).Path, Line: int(p.Line), Column: int(p.Column)}
}

// Pos is a source position; Line and Column are 1-based, Column counts
// code points. The zero Pos is unknown.
type Pos struct {
	// File is the source file.
	File FileID
	// Line is 1-based; 0 means unknown.
	Line int32
	// Column is 1-based in code points; 0 means unknown.
	Column int32
}

// Known reports whether p carries a line.
func (p Pos) Known() bool { return p.Line > 0 }

// Kind is a node's JSON data model type.
type Kind uint8

// Node kinds.
const (
	// KindNull is null (removed by overlay merge before the hub).
	KindNull Kind = iota
	// KindBool is true or false.
	KindBool
	// KindInt is an integer; Text holds its normalized decimal form.
	KindInt
	// KindFloat is a non-integer number; Text holds its source text.
	KindFloat
	// KindString is a string; Text holds the decoded value.
	KindString
	// KindMap is an object with ordered members.
	KindMap
	// KindList is an array.
	KindList
)

// Style records how a scalar was written; renderers and diagnostics use it.
type Style uint8

// Scalar styles.
const (
	// StylePlain is an unquoted YAML scalar.
	StylePlain Style = iota
	// StyleSingleQuoted is a single-quoted YAML scalar.
	StyleSingleQuoted
	// StyleDoubleQuoted is a double-quoted YAML scalar.
	StyleDoubleQuoted
	// StyleLiteral is a YAML literal block (|).
	StyleLiteral
	// StyleFolded is a YAML folded block (>).
	StyleFolded
	// StyleJSON is a value from a .json file.
	StyleJSON
	// StyleDefaulted is a value materialized from a schema or registry
	// default; diagnostics on it point at the parent.
	StyleDefaulted
)

// Node is one value.
type Node struct {
	// Kind is the data model type.
	Kind Kind
	// Style is how the value was written.
	Style Style
	// Pos is where the value starts.
	Pos Pos
	// Text is the string value, the normalized decimal of an integer or
	// the source text of a float.
	Text string
	// Bool is the value of a KindBool node.
	Bool bool
	// Members are the object members in authored order (KindMap).
	Members []Member
	// Items are the list elements (KindList).
	Items []*Node
	// Vars lists the variables substituted into this scalar.
	Vars []string
}

// Member is one object member.
type Member struct {
	// Key is the decoded key; keys are always strings.
	Key string
	// KeyPos is where the key starts.
	KeyPos Pos
	// Value is the member value.
	Value *Node
}

// Get returns the value of member key of a map node.
func (n *Node) Get(key string) (*Node, bool) {
	if n == nil || n.Kind != KindMap {
		return nil, false
	}
	for i := range n.Members {
		if n.Members[i].Key == key {
			return n.Members[i].Value, true
		}
	}
	return nil, false
}

// Set replaces or appends member key of a map node.
func (n *Node) Set(key string, keyPos Pos, v *Node) {
	for i := range n.Members {
		if n.Members[i].Key == key {
			n.Members[i].Value = v
			return
		}
	}
	n.Members = append(n.Members, Member{Key: key, KeyPos: keyPos, Value: v})
}

// Delete removes member key of a map node and reports whether it existed.
func (n *Node) Delete(key string) bool {
	i := slices.IndexFunc(n.Members, func(m Member) bool { return m.Key == key })
	if i < 0 {
		return false
	}
	n.Members = slices.Delete(n.Members, i, i+1)
	return true
}

// Clone returns a deep copy.
func (n *Node) Clone() *Node {
	if n == nil {
		return nil
	}
	c := *n
	c.Vars = slices.Clone(n.Vars)
	if n.Members != nil {
		c.Members = make([]Member, len(n.Members))
		for i, m := range n.Members {
			c.Members[i] = Member{Key: m.Key, KeyPos: m.KeyPos, Value: m.Value.Clone()}
		}
	}
	if n.Items != nil {
		c.Items = make([]*Node, len(n.Items))
		for i, it := range n.Items {
			c.Items[i] = it.Clone()
		}
	}
	return &c
}

// At follows a key-aware path. Keyed elements match the list entry whose
// KeyField member equals Key; Item elements match the first set element
// equal to Item (itemEqual); Index elements select by position.
func (n *Node) At(p diag.Path) (*Node, bool) {
	cur := n
	for _, e := range p {
		switch e.Kind {
		case diag.ElemField:
			var ok bool
			if cur, ok = cur.Get(e.Name); !ok {
				return nil, false
			}
		case diag.ElemIndex:
			if cur == nil || cur.Kind != KindList || e.Index < 0 || e.Index >= len(cur.Items) {
				return nil, false
			}
			cur = cur.Items[e.Index]
		case diag.ElemKeyed:
			if cur == nil || cur.Kind != KindList {
				return nil, false
			}
			i := slices.IndexFunc(cur.Items, func(it *Node) bool {
				k, ok := it.Get(e.KeyField)
				return ok && k.Text == e.Key
			})
			if i < 0 {
				return nil, false
			}
			cur = cur.Items[i]
		case diag.ElemItem:
			if cur == nil || cur.Kind != KindList {
				return nil, false
			}
			i := slices.IndexFunc(cur.Items, func(it *Node) bool { return itemEqual(it, e.Item) })
			if i < 0 {
				return nil, false
			}
			cur = cur.Items[i]
		}
	}
	return cur, cur != nil
}

// itemEqual reports whether n equals a diag.Item value: a string matches a
// string node, a json.Number an integer or float node of equal value, a
// bool a bool node, and a json.RawMessage (object and array elements) the
// node of equal JSON value; numbers compare by float64 value, as the
// canonical form does.
func itemEqual(n *Node, v any) bool {
	switch t := v.(type) {
	case string:
		return n.Kind == KindString && n.Text == t
	case json.Number:
		return numberEqual(n, string(t))
	case bool:
		return n.Kind == KindBool && n.Bool == t
	case json.RawMessage:
		d := json.NewDecoder(bytes.NewReader(t))
		d.UseNumber()
		var x any
		if d.Decode(&x) != nil {
			return false
		}
		return valueEqual(n, x)
	default:
		return false
	}
}

func numberEqual(n *Node, text string) bool {
	if n.Kind != KindInt && n.Kind != KindFloat {
		return false
	}
	if n.Text == text {
		return true
	}
	a, errA := strconv.ParseFloat(n.Text, 64)
	b, errB := strconv.ParseFloat(text, 64)
	return errA == nil && errB == nil && a == b
}

// valueEqual compares n with a value decoded by encoding/json with
// UseNumber.
func valueEqual(n *Node, v any) bool {
	switch t := v.(type) {
	case nil:
		return n.Kind == KindNull
	case string, bool, json.Number:
		return itemEqual(n, t)
	case []any:
		if n.Kind != KindList || len(n.Items) != len(t) {
			return false
		}
		for i := range t {
			if !valueEqual(n.Items[i], t[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		if n.Kind != KindMap || len(n.Members) != len(t) {
			return false
		}
		for _, m := range n.Members {
			x, ok := t[m.Key]
			if !ok || !valueEqual(m.Value, x) {
				return false
			}
		}
		return true
	default:
		return false
	}
}

// JSONValue converts to the encoding/json data model: map[string]any,
// []any, string, json.Number, bool or nil.
func (n *Node) JSONValue() any {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case KindBool:
		return n.Bool
	case KindInt, KindFloat:
		return json.Number(n.Text)
	case KindString:
		return n.Text
	case KindMap:
		m := make(map[string]any, len(n.Members))
		for _, mem := range n.Members {
			m[mem.Key] = mem.Value.JSONValue()
		}
		return m
	case KindList:
		l := make([]any, len(n.Items))
		for i, it := range n.Items {
			l[i] = it.JSONValue()
		}
		return l
	default:
		return nil
	}
}

// ID identifies a resource by kind and metadata.name.
type ID struct {
	// Kind is the resource kind.
	Kind v1alpha1.Kind
	// Name is metadata.name.
	Name string
}

// String returns "<Kind>/<name>".
func (id ID) String() string { return string(id.Kind) + "/" + id.Name }

// ResourceID converts to the diagnostic form.
func (id ID) ResourceID() *diag.ResourceID {
	return &diag.ResourceID{Kind: string(id.Kind), Name: id.Name}
}

// Resource is one document after parsing: its identity, apiVersion and
// envelope tree (apiVersion, kind, metadata, spec).
type Resource struct {
	// ID is the identity (kind, metadata.name).
	ID ID
	// APIVersion is the document's apiVersion.
	APIVersion string
	// Root is the envelope mapping.
	Root *Node
	// Start is the document start in its file.
	Start Pos
}
