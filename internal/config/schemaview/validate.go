// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"errors"
	"slices"
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
// (nil files gives line and column only), at most MaxDiagnostics+1 of
// them (Options.MaxDiagnostics). A finding about a value that stage E
// substituted variables into names the variables in a clause that never
// contains a value (01 req 29); a duplicate keyed-list key or set element
// is quoted as its path shows it (01 req 48), substituted or not. A
// resource whose kind the view does not dispatch is validated
// against the root schema, which reports its kind. Validate does not
// yield; ValidateWith does.
func (v *View) Validate(res *tree.Resource, files *tree.FileTable) diag.List {
	return v.ValidateWith(res, files, Options{})
}

// ValidateWith is Validate with options: opts.MaxDiagnostics is the cap,
// and opts.Yield is called every 256 units of work, so a loader worker can
// yield inside one resource (01 req 53), between the spans of work that
// Options.Yield lists as unable to yield.
func (v *View) ValidateWith(res *tree.Resource, files *tree.FileTable, opts Options) diag.List {
	if res == nil {
		return nil
	}
	limit := opts.MaxDiagnostics
	if limit <= 0 {
		limit = MaxDiagnostics
	}
	return v.check(res, files, limit, opts).result()
}

// check validates res and returns the mapper holding at most limit+1
// diagnostics, limit the cap.
func (v *View) check(res *tree.Resource, files *tree.FileTable, limit int, opts Options) *mapper {
	m := newMapper(v, res, files, limit, opts)
	m.run()
	return m
}

// run validates the mapper's resource: the JSON Schema of its kind
// (View.runSchema), whose errors it maps, then the checks the schema
// cannot express, skipping the member values the instance left out.
func (m *mapper) run() {
	if m.res.Root == nil {
		m.emit(CodeSchema, position{}, m.res.Start, "must be an object, not an empty document", "")
		return
	}
	shadowed, err := m.v.runSchema(m.res, m.work)
	m.shadowed = shadowed
	m.mapErrors(err)
}

// errValidator is the error of a jsonschema/v6 Validate call that
// panicked. It never carries the panic value, which may quote the
// instance.
var errValidator = errors.New("the schema validator failed")

// invalidNumberError reports an instance with a number node, at position
// at, whose text is outside the RFC 8259 grammar, which the tree contract
// excludes (tree.Node.Text): the validator would parse it with no bound,
// so it is not run.
type invalidNumberError struct {
	at tree.Pos
}

func (*invalidNumberError) Error() string { return "a number is not an RFC 8259 number literal" }

// runSchema runs the JSON Schema of res's kind, or the root schema for a
// kind the view does not dispatch, on res.Root (non-nil), and returns its
// error with the member values the instance left out because an earlier
// member has their key (built.shadowed), which the checks after it skip.
// Building the instance charges its nodes on w, the bytes of its member
// names and of long number literals; the validator cannot yield, so its
// run is one span charged afterwards with the nodes and the bytes of the
// member names and strings it hashes and reads (Options.Yield).
func (v *View) runSchema(res *tree.Resource, w *work) (map[*tree.Node]bool, error) {
	sch, ok := v.kinds[string(res.ID.Kind)]
	if !ok {
		sch = v.root
	}
	inst := w.instance(res.Root, v.nums)
	if inst.invalid != nil {
		return inst.shadowed, &invalidNumberError{at: inst.invalid.Pos}
	}
	err := validate(sch, inst.value)
	w.charge(inst.nodes + inst.keyBytes + inst.stringBytes)
	return inst.shadowed, err
}

// validate runs sch on inst. A panic inside jsonschema/v6 returns
// errValidator: the backstop for a library path the number model does
// not cover, so a resource never stops the process (a ruralzd reload
// keeps its active Revision).
func validate(sch *jsonschema.Schema, inst any) (err error) {
	defer func() {
		if recover() != nil {
			err = errValidator
		}
	}()
	return sch.Validate(inst)
}

// mapErrors maps the schema validation result err of m.res, then runs the
// checks the schema cannot express.
func (m *mapper) mapErrors(err error) {
	if err != nil {
		var verr *jsonschema.ValidationError
		var num *invalidNumberError
		switch {
		case errors.As(err, &verr):
			m.visit(verr)
		case errors.As(err, &num):
			// At the number's source position, with the root's path: the
			// instance was not validated.
			m.emit(CodeSchema, position{pos: num.at}, num.at, "cannot be validated: "+err.Error(), "")
		default:
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
// the related location. Walk's own work is charged per node it reports
// (walkUnits). The value of a member that an earlier member with its key
// hides (mapper.shadowed) is skipped with everything below it: the
// validator never saw it, and its path names the earlier member.
func (m *mapper) checkLists() {
	// frames follows Walk's pre-order: at depth d the first d-1 frames are
	// the ancestors' elements.
	var frames []frame
	_ = m.v.idx.Walk(string(m.res.ID.Kind), m.res.Root, func(c *schemaidx.Cursor) error {
		last, _ := c.Last()
		m.work.charge(walkUnits(c.Node, last))
		if m.shadowed[c.Node] {
			return schemaidx.ErrSkipChildren
		}
		if d := c.Depth(); d > 0 {
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

// walkUnits returns the work units schemaidx.Walk spends on node n, which
// it reports with path element last: one for the node; two per member of
// an object, whose dispatch Walk applies twice (Select before and after
// the callback) reading its members; the bytes of a member name, which it
// hashes to find the member's schema; and, for a list element, what
// ItemElem reads to name it: the members it scans for a keyed entry's key
// and the bytes of a set element (elemBytes). Walk cannot yield inside a
// node, so they are charged when it reports the node.
func walkUnits(n *tree.Node, last diag.PathElem) int {
	units := 1
	if n != nil {
		units += 2 * len(n.Members)
	}
	switch last.Kind {
	case diag.ElemField:
		units += len(last.Name)
	case diag.ElemKeyed:
		if n != nil {
			units += len(n.Members)
		}
	case diag.ElemItem:
		units += elemBytes(last, n)
	case diag.ElemIndex:
	}
	return units
}

// duplicateKeys reports keyed-list entries whose key equals an earlier
// entry's. Keys compare by canonical JSON, as set elements do: the string
// "1" and the integer 1 are different keys, 1 and 1.0 the same. Entries
// without a scalar key are left to the schema (required, type). The
// message names the variables stage E substituted into either key (01
// req 29). frames is the list's path. The canonical JSON of a key is a
// span that cannot yield (Options.Yield), charged with its bytes after it.
func (m *mapper) duplicateKeys(c *schemaidx.Cursor, key string, frames []frame) {
	first := map[string]*tree.Node{}
	var buf []byte
	for _, item := range c.Node.Items {
		m.work.tick()
		k, ok := m.member(item, key)
		if !ok || (k.Kind != tree.KindString && k.Kind != tree.KindInt && k.Kind != tree.KindFloat) {
			continue
		}
		buf = appendCanonical(buf[:0], k)
		canon := string(buf)
		m.work.charge(itemJSONBytes(k, len(canon)))
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
		msg := "duplicate entry: " + key + " " + shown + " is already used by an earlier entry" +
			substituted(k.Vars, "the key") + substituted(prev.Vars, "the earlier key")
		m.emit(CodeSchema, p, p.pos, msg, "",
			diag.Related{Location: m.location(knownPos(prev, nil)), Message: "first entry at"})
	}
}

// duplicateElements reports set elements equal to an earlier element,
// compared by canonical JSON (numbers by value, as the canonical form
// encodes them). The message names the variables stage E substituted into
// the scalars of either element (01 req 29). frames is the set's path.
func (m *mapper) duplicateElements(c *schemaidx.Cursor, frames []frame) {
	first := map[string]*tree.Node{}
	var buf []byte
	for i, item := range c.Node.Items {
		// The canonical JSON of one element is a span that cannot yield
		// (Options.Yield); its bytes are charged after it.
		buf = appendCanonical(buf[:0], item)
		canon := string(buf)
		m.work.charge(itemJSONBytes(item, len(canon)))
		prev, dup := first[canon]
		if !dup {
			first[canon] = item
			continue
		}
		e := schemaidx.ItemElem(c.Info.Node, item, i)
		m.work.charge(elemBytes(e, item))
		p := position{path: m.memo.child(m.framePath(frames), e), node: item, pos: knownPos(item, c.Node)}
		msg := "duplicate element " + shorten(canon) + " in a set" +
			substituted(m.varsBelow(item), "the element") + substituted(m.varsBelow(prev), "the earlier element")
		m.emit(CodeSchema, p, p.pos, msg, "",
			diag.Related{Location: m.location(knownPos(prev, c.Node)), Message: "first element at"})
	}
}

// appendCanonical appends the canonical JSON of n (schemaidx.AppendItemJSON)
// to dst, which the uniqueness checks reuse across a list's elements. The
// buffer is first grown to a string's or number's length, so a long
// scalar is written without the repeated growth of appending to a short
// buffer, which allocates several times its size.
func appendCanonical(dst []byte, n *tree.Node) []byte {
	if n != nil && (n.Kind == tree.KindString || n.Kind == tree.KindInt || n.Kind == tree.KindFloat) {
		dst = slices.Grow(dst, len(n.Text)+2)
	}
	return schemaidx.AppendItemJSON(dst, n)
}

// varsBelow returns the variables stage E substituted into the scalars at
// and below n (tree.Node.Vars), in document order without repeats,
// skipping the members an earlier member with their key hides
// (mapper.shadowed). Each node and each byte of a variable name it hashes
// is a work unit.
func (m *mapper) varsBelow(n *tree.Node) []string {
	var out []string
	var seen map[string]bool
	var walk func(n *tree.Node)
	walk = func(n *tree.Node) {
		m.work.tick()
		if n == nil {
			return
		}
		for _, name := range n.Vars {
			m.work.charge(len(name))
			if seen == nil {
				seen = map[string]bool{}
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		for _, mem := range n.Members {
			if !m.shadowed[mem.Value] {
				walk(mem.Value)
			}
		}
		for _, it := range n.Items {
			walk(it)
		}
	}
	walk(n)
	return out
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

// shorten bounds canonical JSON quoted in a message to maxQuoted code
// points, invalid UTF-8 bytes of a shortened text becoming U+FFFD as
// []rune makes them. It reads at most that prefix (clip).
func shorten(s string) string {
	head, long := clip(s)
	if !long {
		return s
	}
	return string([]rune(head)) + "..."
}

// checkReserved reports label keys, and annotation keys other than
// ruralz.io/conversion-data, that start with ruralz.io/ (01 req 35), at
// the key. Both the prefix and the exemption compare bytes exactly, as
// 01 req 35 and the configuration model ("Identity and references")
// write them: label and annotation keys have no syntax that folds case,
// so Ruralz.IO/team is an ordinary key, not the reserved prefix.
func (m *mapper) checkReserved() {
	meta, ok := m.member(m.res.Root, "metadata")
	if !ok {
		return
	}
	metaPath := m.memo.child(nil, diag.Field("metadata"))
	for _, field := range []string{"labels", "annotations"} {
		pairs, ok := m.member(meta, field)
		if !ok || pairs.Kind != tree.KindMap {
			continue
		}
		what := "label"
		if field == "annotations" {
			what = "annotation"
		}
		fieldPath := m.memo.child(metaPath, diag.Field(field))
		for j, mem := range pairs.Members {
			m.work.tick()
			if !strings.HasPrefix(mem.Key, reservedPrefix) || (field == "annotations" && mem.Key == conversionAnnotation) {
				continue
			}
			// A key an earlier member repeats is reported once, at the
			// earlier member, which its path names.
			if first, _ := m.memo.index(pairs, mem.Key); first != j {
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
