// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// conversionData is the annotation that carries newer-only fields through
// a down-conversion; it never reaches the hub or the canonical form (02
// req 7).
const conversionData = "ruralz.io/conversion-data"

// header and trailer enclose the resources of every canonical document;
// RFC 8785 orders "format" before "resources".
const (
	header  = `{"format":"` + Format + `","resources":[`
	trailer = `]}`
)

// maxEncodeDepth bounds the encoder's recursion, so a cyclic tree fails
// instead of exhausting the stack.
const maxEncodeDepth = jsonval.MaxDepthLimit

// Options configures Encoder.Encode.
type Options struct {
	// Level is the schema level to render at. It must equal the index's
	// level (schemaidx.Index.Level, 0 for every M1 schema), the only level
	// this release renders; other levels are the extension point for
	// rendering at a pinned level (02 req 28).
	Level int
	// Workers is the number of goroutines that encode resources; less
	// than 1 means 1 and more than 256 means 256, and an encode never runs
	// more goroutines than it has resources. The Node passes max(1,
	// GOMAXPROCS/2) and the CLI GOMAXPROCS (02 req 78).
	Workers int
	// Now is the clock of the yield timer (01 req 53, 02 section 7
	// OQ-performance-budgets-and-benchmarking-6 (a)): the pipeline passes
	// the Now its caller supplies (clock.Real().Now in every binary), tests
	// a fake. Every goroutine of an encode reads it between two units of
	// work (two resources written, two chunks joined, two pieces of 64
	// KiB hashed) and every 256 units inside one (resources collected and
	// checked, comparisons of the sort, values written), and calls
	// runtime.Gosched once YieldAfter has passed since its last yield, so
	// stage L shares the processor with live traffic during activation
	// (see the package documentation, Concurrency). Goroutines call it
	// concurrently, so it must be safe for concurrent use; it must not
	// panic (a panic in it surfaces as one in the encoding does, see the
	// package documentation). Nil never yields and reads no clock.
	Now func() time.Time
	// YieldAfter is the work a goroutine does before it yields; zero or
	// negative means DefaultYieldAfter (100 µs).
	YieldAfter time.Duration
}

// Encoder writes ruralz.canonical.v1 documents. It is immutable and safe
// for concurrent use.
type Encoder struct {
	idx *schemaidx.Index
	// observe, set by tests only, receives the yield timer of each
	// goroutine of a call once the goroutine is done with it.
	observe func(*yielder)
}

// NewEncoder returns an encoder over the hub schema idx, which supplies
// the schema level and the x-ruralz-since keywords. A nil idx is a schema
// of level 0 without since keywords.
func NewEncoder(idx *schemaidx.Index) *Encoder { return &Encoder{idx: idx} }

// item is one resource to encode.
type item struct {
	id   tree.ID
	root *tree.Node
}

// Encode writes the resources of b, sorted by kind and name (02 req 22),
// as a ruralz.canonical.v1 document and returns it with its digest (02
// reqs 21 to 29). It reads each resource's materialized Tree, not its
// typed views, and checks ctx between resources so a superseded reload
// can stop early. An error is ctx's or wraps ErrEncode: the trees hold a
// value with no canonical form, which stage F and G never hand over.
func (e *Encoder) Encode(ctx context.Context, b *hub.Bundle, opt Options) (revision.Revision, error) {
	if b == nil {
		return revision.Revision{}, fmt.Errorf("%w: no Bundle", ErrEncode)
	}
	y := e.newYielder(opt)
	defer e.done(y)
	rs := b.Resources()
	items := make([]item, len(rs))
	for i, r := range rs {
		y.charge(siteItem, 1)
		items[i] = item{id: r.ID, root: r.Tree}
	}
	return e.encode(ctx, items, opt, y)
}

// EncodeResources is Encode over envelope trees, such as Decode's output
// after stage G; apiVersion members are dropped and the resources sorted.
func (e *Encoder) EncodeResources(ctx context.Context, rs []tree.Resource, opt Options) (revision.Revision, error) {
	y := e.newYielder(opt)
	defer e.done(y)
	items := make([]item, len(rs))
	for i := range rs {
		y.charge(siteItem, 1)
		items[i] = item{id: rs[i].ID, root: rs[i].Root}
	}
	return e.encode(ctx, items, opt, y)
}

// newYielder returns the yield timer of one goroutine of an encode.
func (e *Encoder) newYielder(opt Options) *yielder {
	return newYielder(opt.Now, yieldAfter(opt.YieldAfter))
}

// done hands a goroutine's yield timer to the tests' observer.
func (e *Encoder) done(y *yielder) {
	if e.observe != nil {
		e.observe(y)
	}
}

// AppendNode appends the RFC 8785 form of one value tree, without schema
// (no since omission): members in UTF-16 code unit order, minimal string
// escaping and canonical numbers (02 req 24). The diff engine and tests
// compare values by these bytes. An error wraps ErrEncode.
func AppendNode(dst []byte, n *tree.Node) ([]byte, error) {
	var w writer
	out, err := w.value(dst, n, nil, 0)
	if err != nil {
		return dst, encodeError("", err)
	}
	return out, nil
}

// level returns the schema level the encoder renders at.
func (e *Encoder) level() int {
	if e.idx == nil {
		return 0
	}
	return e.idx.Level()
}

// resourceSchema returns the envelope schema of kind when since omission
// is active, else nil.
func (e *Encoder) resourceSchema(kind string) *schemaidx.Node {
	if e.idx == nil || e.idx.Level() == 0 {
		return nil
	}
	s, _ := e.idx.Resource(kind)
	return s
}

// encode writes items on the goroutines of the call: the calling one,
// whose yield timer is y, and, when the work is split, one per writer,
// each with its own timer.
func (e *Encoder) encode(ctx context.Context, items []item, opt Options, y *yielder) (revision.Revision, error) {
	if err := ctx.Err(); err != nil {
		return revision.Revision{}, err
	}
	if opt.Level != e.level() {
		return revision.Revision{}, fmt.Errorf("%w: schema level %d is not served; this release renders level %d", ErrEncode, opt.Level, e.level())
	}
	if err := e.order(items, y); err != nil {
		return revision.Revision{}, err
	}
	workers := workerCount(opt.Workers)
	chunks := chunkCount(len(items), workers)
	outs := make([][]byte, chunks)
	ws := make([]writer, min(workers, chunks))
	// parallel runs one chunk at a time on the calling goroutine when
	// there is one worker or one chunk, else every chunk on goroutines of
	// its own.
	pooled := workers > 1 && chunks > 1
	for wk := range ws {
		ws[wk].y = y
		if pooled {
			ws[wk].y = e.newYielder(opt)
		}
	}
	err := parallel(ctx, len(items), workers, chunks, func(wk, k, lo, hi int) error {
		w := &ws[wk]
		var buf []byte
		if chunks == 1 {
			buf = append(buf, header...)
		}
		for i := lo; i < hi; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			w.y.check(siteResource)
			if i > lo {
				buf = append(buf, ',')
			}
			var err error
			it := &items[i]
			if buf, err = w.resource(buf, string(it.id.Kind), it.id.Name, it.root, e.resourceSchema(string(it.id.Kind))); err != nil {
				return encodeError(idText(it.id), err)
			}
		}
		outs[k] = buf
		return nil
	})
	if pooled {
		for wk := range ws {
			e.done(ws[wk].y)
		}
	}
	if err != nil {
		return revision.Revision{}, err
	}
	var content []byte
	if chunks == 1 {
		content = outs[0]
		content = append(content, trailer...)
	} else {
		size := len(header) + len(trailer) + chunks
		for _, o := range outs {
			size += len(o)
		}
		content = make([]byte, 0, size)
		content = append(content, header...)
		for k, o := range outs {
			if k > 0 {
				content = append(content, ',')
			}
			content = append(content, o...)
			outs[k] = nil
			y.check(siteJoin)
		}
		content = append(content, trailer...)
	}
	return revision.Revision{Digest: sum(content, y), Content: content}, nil
}

// compareItems orders resources by kind (hub.KindOrder), then by name
// bytewise (02 req 22).
func compareItems(a, b *item) int {
	return cmp.Or(cmp.Compare(hub.KindOrder(a.id.Kind), hub.KindOrder(b.id.Kind)), cmp.Compare(a.id.Name, b.id.Name))
}

// order sorts items in canonical order and refuses kinds the format never
// holds and duplicate identities, charging y for each resource and
// comparison.
func (e *Encoder) order(items []item, y *yielder) error {
	for i := range items {
		y.charge(siteKind, 1)
		k := items[i].id.Kind
		if !canonicalKind(k) {
			return fmt.Errorf("%w: %s: kind %s never appears in %s", ErrEncode, idText(items[i].id), strconv.Quote(clip(string(k))), Format)
		}
		if e.idx != nil {
			if _, ok := e.idx.Resource(string(k)); !ok {
				return fmt.Errorf("%w: %s: kind %s is not in the %s schema", ErrEncode, idText(items[i].id), strconv.Quote(clip(string(k))), e.idx.APIVersion())
			}
		}
	}
	if !slices.IsSortedFunc(items, func(a, b item) int {
		y.charge(siteSorted, 1)
		return compareItems(&a, &b)
	}) {
		slices.SortStableFunc(items, func(a, b item) int {
			y.charge(siteSort, 1)
			return compareItems(&a, &b)
		})
	}
	for i := 1; i < len(items); i++ {
		y.charge(siteDuplicate, 1)
		if items[i].id == items[i-1].id {
			return fmt.Errorf("%w: duplicate resource %s", ErrEncode, idText(items[i].id))
		}
	}
	return nil
}

// canonicalKind reports a kind the format holds: every kind but
// Environment and Cluster, which never leave the loader (02 req 22).
func canonicalKind(k v1alpha1.Kind) bool { return hub.KindOrder(k) < 99 }

// nodeError is a value with no canonical form, with its path from the
// failing node outward (innermost element first).
type nodeError struct {
	rev diag.Path
	msg string
	err error
}

func (e *nodeError) Error() string { return e.msg }

func (e *nodeError) Unwrap() error { return e.err }

// path returns the key-aware path from the encoded root to the node.
func (e *nodeError) path() diag.Path {
	p := make(diag.Path, len(e.rev))
	for i, el := range e.rev {
		p[len(p)-1-i] = el
	}
	return p
}

// within records that err happened under the path element el.
func within(err error, el diag.PathElem) error {
	var ne *nodeError
	if errors.As(err, &ne) {
		ne.rev = append(ne.rev, el)
	}
	return err
}

// encodeError wraps a writer failure in ErrEncode, naming the resource id
// (when given, already bounded by shown) and the path, its member names
// bounded (boundPath), so the message stays short whatever the tree holds.
func encodeError(id string, err error) error {
	var ne *nodeError
	if !errors.As(err, &ne) {
		return fmt.Errorf("%w: %s: %w", ErrEncode, id, err)
	}
	where := id
	if p := ne.path(); len(p) > 0 {
		if where != "" {
			where += " "
		}
		where += boundPath(p).String()
	}
	if where == "" {
		where = "value"
	}
	if ne.err != nil {
		return fmt.Errorf("%w: %s: %s: %w", ErrEncode, where, ne.msg, ne.err)
	}
	return fmt.Errorf("%w: %s: %s", ErrEncode, where, ne.msg)
}

// writer is the RFC 8785 writer over value trees. One writer is owned by
// one goroutine; order is the stack of member sort scratch, y the
// goroutine's yield timer (nil for none), and defaults the canonical
// bytes of the default of each since position met, encoded once per
// writer (schema nodes are immutable).
type writer struct {
	order    []int
	y        *yielder
	defaults map[*schemaidx.Node][]byte
}

// resource appends one resource object: kind, metadata without the
// conversion-data annotation, and spec (02 req 21). rs is the envelope
// schema when since omission is active, else nil.
func (w *writer) resource(dst []byte, kind, name string, root *tree.Node, rs *schemaidx.Node) ([]byte, error) {
	if root == nil || root.Kind != tree.KindMap {
		return dst, &nodeError{msg: "resource tree is not a mapping"}
	}
	var kindN, meta, spec *tree.Node
	for i := range root.Members {
		m := &root.Members[i]
		switch m.Key {
		case "apiVersion":
		case "kind":
			kindN = m.Value
		case "metadata":
			meta = m.Value
		case "spec":
			spec = m.Value
		default:
			return dst, &nodeError{rev: diag.Path{diag.Field(m.Key)}, msg: "member has no place in a " + Format + " resource"}
		}
	}
	if kindN != nil && (kindN.Kind != tree.KindString || kindN.Text != kind) {
		return dst, &nodeError{rev: diag.Path{diag.Field("kind")}, msg: "kind does not match the resource identity " + strconv.Quote(clip(kind))}
	}
	if meta == nil || meta.Kind != tree.KindMap {
		return dst, &nodeError{rev: diag.Path{diag.Field("metadata")}, msg: "metadata is not a mapping"}
	}
	if n, ok := meta.Get("name"); !ok || n.Kind != tree.KindString || n.Text != name {
		return dst, &nodeError{rev: diag.Path{diag.Field("name"), diag.Field("metadata")}, msg: "metadata.name does not match the resource identity " + strconv.Quote(clip(name))}
	}
	var metaS, specS *schemaidx.Node
	if rs != nil {
		sel := rs.Select(root)
		metaS, _ = sel.Property("metadata")
		specS, _ = sel.Property("spec")
	}
	dst = append(dst, `{"kind":`...)
	dst, err := jsonval.AppendString(dst, kind)
	if err != nil {
		return dst, &nodeError{rev: diag.Path{diag.Field("kind")}, msg: "string is not valid UTF-8", err: err}
	}
	dst = append(dst, `,"metadata":`...)
	if dst, err = w.value(dst, stripConversionData(meta), metaS, 1); err != nil {
		return dst, within(err, diag.Field("metadata"))
	}
	if spec != nil {
		dst = append(dst, `,"spec":`...)
		if dst, err = w.value(dst, spec, specS, 1); err != nil {
			return dst, within(err, diag.Field("spec"))
		}
	}
	return append(dst, '}'), nil
}

// stripConversionData returns meta without the conversion-data
// annotation, and without annotations when that was the only one, so a
// down-converted resource keeps the digest of its source (02 req 7). meta
// is returned as it is when it holds no such annotation.
func stripConversionData(meta *tree.Node) *tree.Node {
	ai := slices.IndexFunc(meta.Members, func(m tree.Member) bool { return m.Key == "annotations" })
	if ai < 0 {
		return meta
	}
	ann := meta.Members[ai].Value
	if ann == nil || ann.Kind != tree.KindMap {
		return meta
	}
	ci := slices.IndexFunc(ann.Members, func(m tree.Member) bool { return m.Key == conversionData })
	if ci < 0 {
		return meta
	}
	out := *meta
	if len(ann.Members) == 1 {
		out.Members = slices.Delete(slices.Clone(meta.Members), ai, ai+1)
		return &out
	}
	stripped := *ann
	stripped.Members = slices.Delete(slices.Clone(ann.Members), ci, ci+1)
	out.Members = slices.Clone(meta.Members)
	out.Members[ai].Value = &stripped
	return &out
}

// value appends the RFC 8785 form of n. s is n's schema when since
// omission is active, else nil.
func (w *writer) value(dst []byte, n *tree.Node, s *schemaidx.Node, depth int) ([]byte, error) {
	if n == nil {
		return dst, &nodeError{msg: "missing value"}
	}
	if depth > maxEncodeDepth {
		return dst, &nodeError{msg: "nesting deeper than " + strconv.Itoa(maxEncodeDepth), err: jsonval.ErrDepth}
	}
	w.y.charge(siteValue, 1)
	switch n.Kind {
	case tree.KindBool:
		if n.Bool {
			return append(dst, "true"...), nil
		}
		return append(dst, "false"...), nil
	case tree.KindInt, tree.KindFloat:
		out, err := jsonval.AppendCanonicalNumber(dst, json.Number(n.Text))
		if err != nil {
			return dst, &nodeError{msg: "number " + strconv.Quote(clip(n.Text)) + " has no RFC 8785 form", err: err}
		}
		return out, nil
	case tree.KindString:
		out, err := jsonval.AppendString(dst, n.Text)
		if err != nil {
			return dst, &nodeError{msg: "string is not valid UTF-8", err: err}
		}
		return out, nil
	case tree.KindList:
		return w.list(dst, n, s, depth)
	case tree.KindMap:
		return w.object(dst, n, s, depth)
	default:
		return dst, &nodeError{msg: "null has no " + Format + " form"}
	}
}

func (w *writer) list(dst []byte, n *tree.Node, s *schemaidx.Node, depth int) ([]byte, error) {
	var is *schemaidx.Node
	if s != nil {
		is, _ = s.Items()
	}
	dst = append(dst, '[')
	for i, it := range n.Items {
		if i > 0 {
			dst = append(dst, ',')
		}
		var err error
		if dst, err = w.value(dst, it, is, depth+1); err != nil {
			return dst, within(err, diag.Index(i))
		}
	}
	return append(dst, ']'), nil
}

// object appends a mapping with members in UTF-16 code unit order (RFC
// 8785 section 3.2.3). A member whose schema carries x-ruralz-since above
// 0 is left out when it equals its default (02 req 23): its value is
// written once and taken back when its bytes are the default's, so since
// positions nested in one another cost one encoding of each value, not
// one per enclosing since position.
func (w *writer) object(dst []byte, n *tree.Node, s *schemaidx.Node, depth int) ([]byte, error) {
	if s != nil {
		s = s.Select(n)
	}
	ms := n.Members
	start := len(w.order)
	sorted := true
	for i := range ms {
		w.order = append(w.order, i)
		if i > 0 && sorted && jsonval.CompareUTF16(ms[i-1].Key, ms[i].Key) >= 0 {
			sorted = false
		}
	}
	ord := w.order[start:]
	if !sorted {
		slices.SortStableFunc(ord, func(a, b int) int { return jsonval.CompareUTF16(ms[a].Key, ms[b].Key) })
	}
	dst = append(dst, '{')
	wrote := false
	for k, i := range ord {
		m := &ms[i]
		if k > 0 && ms[ord[k-1]].Key == m.Key {
			w.order = w.order[:start]
			return dst, &nodeError{rev: diag.Path{diag.Field(m.Key)}, msg: "duplicate member name"}
		}
		var cs *schemaidx.Node
		if s != nil {
			cs = memberSchema(s, m.Key)
		}
		mark, wroteBefore := len(dst), wrote
		if wrote {
			dst = append(dst, ',')
		}
		wrote = true
		var err error
		if dst, err = jsonval.AppendString(dst, m.Key); err != nil {
			w.order = w.order[:start]
			return dst, &nodeError{rev: diag.Path{diag.Field(m.Key)}, msg: "member name is not valid UTF-8", err: err}
		}
		dst = append(dst, ':')
		at := len(dst)
		if dst, err = w.value(dst, m.Value, cs, depth+1); err != nil {
			w.order = w.order[:start]
			return dst, within(err, diag.Field(m.Key))
		}
		if cs.Since() > 0 {
			def, err := w.defaultOf(cs, depth+1)
			if err != nil {
				w.order = w.order[:start]
				return dst, within(err, diag.Field(m.Key))
			}
			if def != nil && bytes.Equal(dst[at:], def) {
				dst, wrote = dst[:mark], wroteBefore
			}
		}
	}
	w.order = w.order[:start]
	return append(dst, '}'), nil
}

// memberSchema returns the schema of member name of an object whose
// schema (dispatch applied) is s: a declared property, else the
// typed-map value schema, else nil (open content).
func memberSchema(s *schemaidx.Node, name string) *schemaidx.Node {
	if p, ok := s.Property(name); ok {
		return p
	}
	if v, ok := s.Values(); ok {
		return v
	}
	return nil
}

// defaultOf returns the canonical bytes of the default of a position
// whose schema s carries x-ruralz-since above 0, with since omission
// applied inside, or nil when it has none: such a position is written
// whenever present (02 req 23). The bytes are encoded once per writer, at
// the depth of the first position that asks, and compared with each
// value written there.
func (w *writer) defaultOf(s *schemaidx.Node, depth int) ([]byte, error) {
	if b, ok := w.defaults[s]; ok {
		return b, nil
	}
	var b []byte
	if def := s.DefaultNode(); def != nil {
		var err error
		if b, err = w.value(nil, def, s, depth); err != nil {
			return nil, err
		}
	}
	if w.defaults == nil {
		w.defaults = make(map[*schemaidx.Node][]byte)
	}
	w.defaults[s] = b
	return b, nil
}
