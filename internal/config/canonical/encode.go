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

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// conversionData is the annotation that carries newer-only fields through
// a down-conversion; it never reaches the hub or the canonical form.
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
	// rendering at a pinned level.
	Level int
	// Workers is the number of goroutines that encode resources; less
	// than 1 means 1. The Node passes max(1, GOMAXPROCS/2) and the CLI
	// GOMAXPROCS.
	Workers int
}

// Encoder writes ruralz.canonical.v1 documents. It is immutable and safe
// for concurrent use.
type Encoder struct {
	idx *schemaidx.Index
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

// Encode writes the resources of b, sorted by kind and name, as a
// ruralz.canonical.v1 document and returns it with its digest. It reads
// each resource's materialized Tree, not its typed views, and checks ctx
// between resources so a superseded reload can stop early. An error is
// ctx's or wraps ErrEncode: the trees hold a value with no canonical
// form, which stage F and G never hand over.
func (e *Encoder) Encode(ctx context.Context, b *hub.Bundle, opt Options) (revision.Revision, error) {
	if b == nil {
		return revision.Revision{}, fmt.Errorf("%w: no Bundle", ErrEncode)
	}
	rs := b.Resources()
	items := make([]item, len(rs))
	for i, r := range rs {
		items[i] = item{id: r.ID, root: r.Tree}
	}
	return e.encode(ctx, items, opt)
}

// EncodeResources is Encode over envelope trees, such as Decode's output
// after stage G; apiVersion members are dropped and the resources sorted.
func (e *Encoder) EncodeResources(ctx context.Context, rs []tree.Resource, opt Options) (revision.Revision, error) {
	items := make([]item, len(rs))
	for i := range rs {
		items[i] = item{id: rs[i].ID, root: rs[i].Root}
	}
	return e.encode(ctx, items, opt)
}

// AppendNode appends the RFC 8785 form of one value tree, without schema
// (no since omission): members in UTF-16 code unit order, minimal string
// escaping and canonical numbers. The diff engine and tests compare
// values by these bytes. An error wraps ErrEncode.
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

func (e *Encoder) encode(ctx context.Context, items []item, opt Options) (revision.Revision, error) {
	if err := ctx.Err(); err != nil {
		return revision.Revision{}, err
	}
	if opt.Level != e.level() {
		return revision.Revision{}, fmt.Errorf("%w: schema level %d is not served; this release renders level %d", ErrEncode, opt.Level, e.level())
	}
	if err := e.order(items); err != nil {
		return revision.Revision{}, err
	}
	workers := max(1, opt.Workers)
	chunks := chunkCount(len(items), workers)
	outs := make([][]byte, chunks)
	ws := make([]writer, min(workers, chunks))
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
			if i > lo {
				buf = append(buf, ',')
			}
			var err error
			it := &items[i]
			if buf, err = w.resource(buf, string(it.id.Kind), it.id.Name, it.root, e.resourceSchema(string(it.id.Kind))); err != nil {
				return encodeError(it.id.String(), err)
			}
		}
		outs[k] = buf
		return nil
	})
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
		}
		content = append(content, trailer...)
	}
	return revision.Revision{Digest: revision.Sum(content), Content: content}, nil
}

// compareItems orders resources by kind (hub.KindOrder), then by name
// bytewise.
func compareItems(a, b *item) int {
	return cmp.Or(cmp.Compare(hub.KindOrder(a.id.Kind), hub.KindOrder(b.id.Kind)), cmp.Compare(a.id.Name, b.id.Name))
}

// order sorts items in canonical order and refuses kinds the format never
// holds and duplicate identities.
func (e *Encoder) order(items []item) error {
	for i := range items {
		k := items[i].id.Kind
		if !canonicalKind(k) {
			return fmt.Errorf("%w: %s: kind %q never appears in %s", ErrEncode, items[i].id, string(k), Format)
		}
		if e.idx != nil {
			if _, ok := e.idx.Resource(string(k)); !ok {
				return fmt.Errorf("%w: %s: kind %q is not in the %s schema", ErrEncode, items[i].id, string(k), e.idx.APIVersion())
			}
		}
	}
	if !slices.IsSortedFunc(items, func(a, b item) int { return compareItems(&a, &b) }) {
		slices.SortStableFunc(items, func(a, b item) int { return compareItems(&a, &b) })
	}
	for i := 1; i < len(items); i++ {
		if items[i].id == items[i-1].id {
			return fmt.Errorf("%w: duplicate resource %s", ErrEncode, items[i].id)
		}
	}
	return nil
}

// canonicalKind reports a kind the format holds: every kind but
// Environment and Cluster, which never leave the loader.
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
// (when given) and the path.
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
		where += p.String()
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
// one goroutine; order is the stack of member sort scratch.
type writer struct {
	order []int
}

// resource appends one resource object: kind, metadata without the
// conversion-data annotation, and spec. rs is the envelope schema when
// since omission is active, else nil.
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
		return dst, &nodeError{rev: diag.Path{diag.Field("kind")}, msg: "kind does not match the resource identity " + strconv.Quote(kind)}
	}
	if meta == nil || meta.Kind != tree.KindMap {
		return dst, &nodeError{rev: diag.Path{diag.Field("metadata")}, msg: "metadata is not a mapping"}
	}
	if n, ok := meta.Get("name"); !ok || n.Kind != tree.KindString || n.Text != name {
		return dst, &nodeError{rev: diag.Path{diag.Field("name"), diag.Field("metadata")}, msg: "metadata.name does not match the resource identity " + strconv.Quote(name)}
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
// down-converted resource keeps the digest of its source. meta is
// returned as it is when it holds no such annotation.
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
// 0 is left out when it equals its default.
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
			if cs.Since() > 0 {
				omit, err := w.atDefault(m.Value, cs, depth+1)
				if err != nil {
					w.order = w.order[:start]
					return dst, within(err, diag.Field(m.Key))
				}
				if omit {
					continue
				}
			}
		}
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
		if dst, err = w.value(dst, m.Value, cs, depth+1); err != nil {
			w.order = w.order[:start]
			return dst, within(err, diag.Field(m.Key))
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

// atDefault reports whether v, at a position whose schema s carries
// x-ruralz-since above 0, equals s's default by canonical bytes. A
// position without a default is written whenever present.
func (w *writer) atDefault(v *tree.Node, s *schemaidx.Node, depth int) (bool, error) {
	def := s.DefaultNode()
	if def == nil {
		return false, nil
	}
	got, err := w.value(nil, v, s, depth)
	if err != nil {
		return false, err
	}
	want, err := w.value(nil, def, s, depth)
	if err != nil {
		return false, err
	}
	return bytes.Equal(got, want), nil
}

// clip shortens s for a message.
func clip(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
