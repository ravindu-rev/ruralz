// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"errors"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// reservedPrefix is the label and annotation key prefix only Ruralz may
// write (01 req 35).
const reservedPrefix = "ruralz.io/"

// conversionAnnotation is the one reserved annotation a Bundle may carry
// (01 req 35).
const conversionAnnotation = "ruralz.io/conversion-data"

// Validate runs stage F on one resource after substitution (01 reqs 32 to
// 35): the JSON Schema of its kind in this view, keyed-list and set
// uniqueness, and the reserved ruralz.io/ label and annotation prefix. It
// returns the findings sorted as diag.List.Sort sorts, each with the
// resource identity, a key-aware path and the source location from files
// (nil files gives line and column only), at most MaxDiagnostics of them.
// A resource whose kind the view does not dispatch is validated against
// the root schema, which reports its kind.
func (v *View) Validate(res *tree.Resource, files *tree.FileTable) diag.List {
	if res == nil {
		return nil
	}
	return v.check(res, files, MaxDiagnostics).result()
}

// check validates res and returns the mapper holding at most limit
// diagnostics.
func (v *View) check(res *tree.Resource, files *tree.FileTable, limit int) *mapper {
	m := newMapper(v, res, files, limit)
	if res.Root == nil {
		m.emit(CodeSchema, position{}, res.Start, "must be an object, not an empty document", "")
		return m
	}
	m.mapErrors(v.schemaErrors(res))
	return m
}

// schemaErrors runs the JSON Schema of res's kind, or the root schema for
// a kind the view does not dispatch, on res.Root (non-nil).
func (v *View) schemaErrors(res *tree.Resource) error {
	sch, ok := v.kinds[string(res.ID.Kind)]
	if !ok {
		sch = v.root
	}
	return sch.Validate(res.Root.JSONValue())
}

// mapErrors maps the schema validation result err of m.res, then runs the
// checks the schema cannot express.
func (m *mapper) mapErrors(err error) {
	if err != nil {
		var verr *jsonschema.ValidationError
		if errors.As(err, &verr) {
			m.visit(verr)
		} else {
			m.emit(CodeSchema, position{pos: m.res.Root.Pos}, m.res.Root.Pos, "cannot be validated: "+err.Error(), "")
		}
	}
	m.flushSecrets()
	m.checkLists()
	m.checkReserved()
}

// frame is one element of the path of the node Walk visits, with its path
// node built on first use: every duplicate below the same element shares
// it, so the element is rendered once.
type frame struct {
	elem diag.PathElem
	path *pathNode
}

// framePath returns the path node of frames, building the missing ones.
func (m *mapper) framePath(frames []frame) *pathNode {
	var parent *pathNode
	for i := range frames {
		if frames[i].path == nil {
			frames[i].path = m.memo.child(parent, frames[i].elem)
		}
		parent = frames[i].path
	}
	return parent
}

// checkLists reports duplicates the schema cannot express (01 req 34):
// two entries of a map or orderedMap list with equal key values, and two
// equal elements of a set, each RZ-CFG-005 at the second with the first as
// the related location.
func (m *mapper) checkLists() {
	// frames follows Walk's pre-order: at depth d the first d-1 frames are
	// the ancestors' elements.
	var frames []frame
	_ = m.v.idx.Walk(string(m.res.ID.Kind), m.res.Root, func(c *schemaidx.Cursor) error {
		if d := c.Depth(); d > 0 {
			last, _ := c.Last()
			frames = append(frames[:d-1], frame{elem: last})
		} else {
			frames = frames[:0]
		}
		if c.Node.Kind != tree.KindList || c.Info.Node == nil || c.Info.InSecret {
			return nil
		}
		lt, key := c.Info.Node.List()
		switch {
		case lt.Keyed():
			m.duplicateKeys(c, key, frames)
		case lt == schemaidx.ListSet:
			m.duplicateElements(c, frames)
		default:
		}
		return nil
	})
}

// duplicateKeys reports keyed-list entries whose key equals an earlier
// entry's. Keys compare by canonical JSON, as set elements do: the string
// "1" and the integer 1 are different keys, 1 and 1.0 the same. Entries
// without a scalar key are left to the schema (required, type). frames
// is the list's path.
func (m *mapper) duplicateKeys(c *schemaidx.Cursor, key string, frames []frame) {
	first := map[string]*tree.Node{}
	for _, item := range c.Node.Items {
		k, ok := item.Get(key)
		if !ok || (k.Kind != tree.KindString && k.Kind != tree.KindInt && k.Kind != tree.KindFloat) {
			continue
		}
		canon := string(schemaidx.AppendItemJSON(nil, k))
		prev, dup := first[canon]
		if !dup {
			first[canon] = k
			continue
		}
		shown := shorten(canon)
		if k.Kind == tree.KindString {
			shown = quote(k.Text)
		}
		p := position{path: m.memo.child(m.framePath(frames), diag.Keyed(key, k.Text)), node: k, pos: knownPos(k, item)}
		m.emit(CodeSchema, p, p.pos, "duplicate entry: "+key+" "+shown+" is already used by an earlier entry", "",
			diag.Related{Location: m.location(knownPos(prev, nil)), Message: "first entry at"})
	}
}

// duplicateElements reports set elements equal to an earlier element,
// compared by canonical JSON (numbers by value, as the canonical form
// encodes them). frames is the set's path.
func (m *mapper) duplicateElements(c *schemaidx.Cursor, frames []frame) {
	first := map[string]*tree.Node{}
	for i, item := range c.Node.Items {
		canon := string(schemaidx.AppendItemJSON(nil, item))
		prev, dup := first[canon]
		if !dup {
			first[canon] = item
			continue
		}
		p := position{path: m.memo.child(m.framePath(frames), schemaidx.ItemElem(c.Info.Node, item, i)), node: item, pos: knownPos(item, c.Node)}
		m.emit(CodeSchema, p, p.pos, "duplicate element "+shorten(canon)+" in a set", "",
			diag.Related{Location: m.location(knownPos(prev, c.Node)), Message: "first element at"})
	}
}

// knownPos returns n's position, or fallback's when n's is unknown.
func knownPos(n, fallback *tree.Node) tree.Pos {
	if n != nil && n.Pos.Known() {
		return n.Pos
	}
	if fallback != nil {
		return fallback.Pos
	}
	return tree.Pos{}
}

// shorten bounds canonical JSON quoted in a message.
func shorten(s string) string {
	r := []rune(s)
	if len(r) <= maxQuoted {
		return s
	}
	return string(r[:maxQuoted]) + "..."
}

// checkReserved reports label keys, and annotation keys other than
// ruralz.io/conversion-data, that start with ruralz.io/ (01 req 35), at
// the key. The prefix is compared case-insensitively, as DNS prefixes
// are, so a differently cased key cannot bypass the reservation.
func (m *mapper) checkReserved() {
	meta, ok := m.res.Root.Get("metadata")
	if !ok {
		return
	}
	metaPath := m.memo.child(nil, diag.Field("metadata"))
	for _, field := range []string{"labels", "annotations"} {
		pairs, ok := meta.Get(field)
		if !ok || pairs.Kind != tree.KindMap {
			continue
		}
		what := "label"
		if field == "annotations" {
			what = "annotation"
		}
		fieldPath := m.memo.child(metaPath, diag.Field(field))
		for _, mem := range pairs.Members {
			if !hasReservedPrefix(mem.Key) || (field == "annotations" && mem.Key == conversionAnnotation) {
				continue
			}
			at := mem.KeyPos
			if !at.Known() {
				at = pairs.Pos
			}
			p := position{path: m.memo.child(fieldPath, diag.Field(mem.Key)), node: mem.Value, pos: at}
			msg := what + " key " + quote(mem.Key) + " uses the reserved prefix " + reservedPrefix
			hint := ""
			if field == "annotations" {
				hint = "only " + conversionAnnotation + " may use the prefix"
			}
			m.emit(CodeSchema, p, at, msg, hint)
		}
	}
}

func hasReservedPrefix(key string) bool {
	return len(key) >= len(reservedPrefix) && strings.EqualFold(key[:len(reservedPrefix)], reservedPrefix)
}
