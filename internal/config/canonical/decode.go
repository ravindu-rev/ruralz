// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// DefaultMaxDepth is the nesting limit of canonical content when
// DecoderOptions leaves it zero: twice the authoring limit of 64, so
// every Revision this release builds decodes, with room for the document
// and resources levels and materialized object defaults.
const DefaultMaxDepth = 128

// DecoderOptions configures a Decoder.
type DecoderOptions struct {
	// MaxDepth limits the nesting of the content (DefaultMaxDepth when
	// zero or negative, jsonval.MaxDepthLimit at most).
	MaxDepth int
	// Workers is the number of goroutines that check and re-encode
	// resources; less than 1 means 1.
	Workers int
}

// Decoder reads ruralz.canonical.v1 content back into resource trees. It
// is immutable and safe for concurrent use.
type Decoder struct {
	idx *schemaidx.Index
	enc *Encoder
	// level is the served schema level: a field above it is from a newer
	// producer.
	level    int
	maxDepth int
	workers  int
}

// NewDecoder returns a decoder for content of the hub apiVersion idx
// serves. Decoding needs idx; a nil idx makes every call fail with
// ErrNoIndex.
func NewDecoder(idx *schemaidx.Index, o DecoderOptions) *Decoder {
	depth := o.MaxDepth
	switch {
	case depth <= 0:
		depth = DefaultMaxDepth
	case depth > jsonval.MaxDepthLimit:
		depth = jsonval.MaxDepthLimit
	}
	d := &Decoder{idx: idx, enc: NewEncoder(idx), maxDepth: depth, workers: max(1, o.Workers)}
	if idx != nil {
		d.level = idx.Level()
	}
	return d
}

// Source names the content for positions and diagnostics.
type Source struct {
	// Name is the file name diagnostics carry, such as the Last-Known-Good
	// content file or a dump saved by ruralz node dump; "" prints as "-".
	Name string
	// Files receives the content as a tree.RoleCanonical file, and the
	// positions of the returned trees refer to it. Nil uses a table of the
	// call's own.
	Files *tree.FileTable
}

// Decode reads canonical content into one envelope tree per resource, in
// content order, for pipeline.FromResources. It fails with CodeNewer for
// content from a newer producer and with CodeMismatch for anything that is
// not exactly ruralz.canonical.v1 bytes; see the package documentation.
// The error's Findings locate every problem. It applies no integer range
// rule and materializes no default. A non-nil error that is not an errcode
// error is ctx's or ErrNoIndex.
func (d *Decoder) Decode(ctx context.Context, content []byte, src Source) ([]tree.Resource, error) {
	return d.decode(ctx, newInput(content, 0, len(content), src))
}

// Verify checks that content hashes to want (CodeMismatch "content hashes
// to sha256:X, want sha256:Y" otherwise) and then decodes it as Decode
// does. Last-Known-Good boot, handover candidates and dumps read by the
// CLI verify.
func (d *Decoder) Verify(ctx context.Context, content []byte, want revision.Digest, src Source) ([]tree.Resource, error) {
	if err := hashMatches(content, want, src.Name); err != nil {
		return nil, err
	}
	return d.Decode(ctx, content, src)
}

// hashMatches reports CodeMismatch when content does not hash to want.
func hashMatches(content []byte, want revision.Digest, name string) error {
	if got := revision.Sum(content); got != want {
		return fail(CodeMismatch, nil, finding(CodeMismatch, diag.Location{File: name}, nil, nil,
			"content hashes to "+got.String()+", want "+want.String()))
	}
	return nil
}

// input is the content of one call and how its offsets map to positions:
// content is body[lo:hi], and positions count from the start of body (a
// dump embeds the content).
type input struct {
	body    []byte
	lo, hi  int
	name    string
	files   *tree.FileTable
	file    tree.FileID
	locator *jsonval.Locator
}

func newInput(body []byte, lo, hi int, src Source) *input {
	files := src.Files
	if files == nil {
		files = &tree.FileTable{}
	}
	in := &input{body: body, lo: lo, hi: hi, name: src.Name, files: files, locator: jsonval.NewLocator(body)}
	in.file = files.Add(tree.File{Path: src.Name, Role: tree.RoleCanonical})
	return in
}

// content returns the canonical bytes.
func (in *input) content() []byte { return in.body[in.lo:in.hi] }

// pos returns the tree position of a content offset.
func (in *input) pos(off int) tree.Pos {
	p := in.locator.Position(in.lo + off)
	if p.Line > math.MaxInt32 || p.Column > math.MaxInt32 {
		return tree.Pos{}
	}
	return tree.Pos{File: in.file, Line: int32(p.Line), Column: int32(p.Column)} //nolint:gosec // G115: bounded by the check above.
}

// location returns the diagnostic location of a tree position, or of the
// file alone when the position is unknown.
func (in *input) location(p tree.Pos) diag.Location {
	if !p.Known() {
		return diag.Location{File: in.name}
	}
	return diag.Location{File: in.name, Line: int(p.Line), Column: int(p.Column)}
}

// at returns the diagnostic location of a content offset.
func (in *input) at(off int) diag.Location { return in.location(in.pos(off)) }

// entry is one decoded resource with the byte range of its object in the
// content.
type entry struct {
	res        tree.Resource
	start, end int
	// newer and form are the findings of the per-resource checks; diff
	// is the content offset where the re-encoded bytes first differ, or
	// -1. Workers record offsets only: the locator that turns them into
	// positions is not safe for concurrent use.
	newer diag.List
	form  *diag.Diagnostic
	diff  int
}

func (d *Decoder) decode(ctx context.Context, in *input) ([]tree.Resource, error) {
	if d.idx == nil {
		return nil, ErrNoIndex
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	doc, spans, err := parse(in, d.maxDepth)
	if err != nil {
		return nil, err
	}
	entries, err := d.envelope(doc, spans, in)
	if err != nil {
		return nil, err
	}
	if err := d.check(ctx, entries, in); err != nil {
		return nil, err
	}
	out := make([]tree.Resource, len(entries))
	for i := range entries {
		out[i] = entries[i].res
	}
	return out, nil
}

// envelope applies the document and resource shape rules: CodeNewer for a
// newer format, member or kind, CodeMismatch for any other shape.
func (d *Decoder) envelope(doc *tree.Node, spans [][2]int, in *input) ([]entry, error) {
	if doc.Kind != tree.KindMap {
		return nil, fail(CodeMismatch, nil, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": not a JSON object"))
	}
	var format, resources *tree.Node
	var newer, form diag.List
	for i := range doc.Members {
		m := &doc.Members[i]
		switch m.Key {
		case "format":
			format = m.Value
		case "resources":
			resources = m.Value
		default:
			newer = append(newer, finding(CodeNewer, in.location(m.KeyPos), nil, diag.Path{diag.Field(m.Key)},
				"member is not part of "+Format+"; the content comes from a newer release"))
		}
	}
	switch {
	case format == nil:
		return nil, fail(CodeMismatch, nil, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": no format member"))
	case format.Kind != tree.KindString:
		return nil, fail(CodeMismatch, nil, finding(CodeMismatch, in.location(format.Pos), nil, diag.Path{diag.Field("format")}, notCanonical+": format is not a string"))
	case format.Text != Format:
		return nil, fail(CodeNewer, nil, finding(CodeNewer, in.location(format.Pos), nil, diag.Path{diag.Field("format")},
			"format "+strconv.Quote(clip(format.Text))+" is not served by this release, which reads "+Format))
	}
	var entries []entry
	switch {
	case resources == nil:
		form = append(form, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": no resources member"))
	case resources.Kind != tree.KindList:
		form = append(form, finding(CodeMismatch, in.location(resources.Pos), nil, diag.Path{diag.Field("resources")}, notCanonical+": resources is not an array"))
	default:
		entries = make([]entry, 0, len(resources.Items))
		for i, rn := range resources.Items {
			e, nf, ff := d.resource(rn, i, in)
			newer, form = append(newer, nf...), append(form, ff...)
			if nf == nil && ff == nil {
				e.start, e.end = spans[i][0], spans[i][1]
				entries = append(entries, e)
			}
		}
	}
	if len(newer) > 0 {
		return nil, fail(CodeNewer, nil, newer...)
	}
	if len(form) > 0 {
		return nil, fail(CodeMismatch, nil, form...)
	}
	return entries, nil
}

// resource checks the shape of resource i and builds its envelope tree,
// with the served apiVersion added in front.
func (d *Decoder) resource(rn *tree.Node, i int, in *input) (e entry, newer, form diag.List) {
	at := diag.Path{diag.Field("resources"), diag.Index(i)}
	if rn.Kind != tree.KindMap {
		return e, nil, diag.List{finding(CodeMismatch, in.location(rn.Pos), nil, at, notCanonical+": resource is not an object")}
	}
	var kindN, meta *tree.Node
	for j := range rn.Members {
		m := &rn.Members[j]
		switch m.Key {
		case "kind":
			kindN = m.Value
		case "metadata":
			meta = m.Value
		case "spec":
		case "apiVersion":
			form = append(form, finding(CodeMismatch, in.location(m.KeyPos), nil, at.Append(diag.Field(m.Key)), notCanonical+": a resource carries no apiVersion"))
		default:
			newer = append(newer, finding(CodeNewer, in.location(m.KeyPos), nil, at.Append(diag.Field(m.Key)),
				"member is not part of a "+Format+" resource; the content comes from a newer release"))
		}
	}
	if kindN == nil || kindN.Kind != tree.KindString {
		return e, newer, append(form, finding(CodeMismatch, in.location(rn.Pos), nil, at, notCanonical+": resource has no kind"))
	}
	kind := v1alpha1.Kind(kindN.Text)
	if _, known := d.idx.Resource(kindN.Text); !known {
		return e, append(newer, finding(CodeNewer, in.location(kindN.Pos), nil, at.Append(diag.Field("kind")),
			"kind "+strconv.Quote(clip(kindN.Text))+" is unknown to "+d.idx.APIVersion()+"; the content comes from a newer release")), form
	}
	if !canonicalKind(kind) {
		return e, newer, append(form, finding(CodeMismatch, in.location(kindN.Pos), nil, at.Append(diag.Field("kind")),
			notCanonical+": kind "+kindN.Text+" never appears in "+Format))
	}
	var name *tree.Node
	if meta != nil {
		name, _ = meta.Get("name")
	}
	if name == nil || name.Kind != tree.KindString {
		return e, newer, append(form, finding(CodeMismatch, in.location(rn.Pos), nil, at, notCanonical+": resource has no metadata.name"))
	}
	root := &tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: rn.Pos, Members: make([]tree.Member, 0, len(rn.Members)+1)}
	av := &tree.Node{Kind: tree.KindString, Style: tree.StyleJSON, Pos: rn.Pos, Text: d.idx.APIVersion()}
	root.Members = append(root.Members, tree.Member{Key: "apiVersion", KeyPos: rn.Pos, Value: av})
	root.Members = append(root.Members, rn.Members...)
	e.res = tree.Resource{ID: tree.ID{Kind: kind, Name: name.Text}, APIVersion: d.idx.APIVersion(), Root: root, Start: rn.Pos}
	return e, newer, form
}

// check runs the per-resource checks, on the decoder's workers: fields
// unknown to the schema or above its level (CodeNewer), then the
// canonical form of the whole content by re-encoding (CodeMismatch).
func (d *Decoder) check(ctx context.Context, entries []entry, in *input) error {
	workers := d.workers
	chunks := chunkCount(len(entries), workers)
	ws := make([]writer, min(workers, chunks))
	content := in.content()
	err := parallel(ctx, len(entries), workers, chunks, func(wk, _, lo, hi int) error {
		w := &ws[wk]
		var buf []byte
		for i := lo; i < hi; i++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			e := &entries[i]
			e.newer = d.newer(e, in)
			buf = d.form(w, buf[:0], e, content, in)
		}
		return nil
	})
	if err != nil {
		return err
	}
	var newer diag.List
	for i := range entries {
		newer = append(newer, entries[i].newer...)
	}
	if len(newer) > 0 {
		return fail(CodeNewer, nil, newer...)
	}
	if f := d.sequence(entries, content, in); f != nil {
		return fail(CodeMismatch, nil, *f)
	}
	return nil
}

// newer walks a resource with the hub schema and reports every member the
// schema does not know (an undeclared member of a closed object) or whose
// x-ruralz-since exceeds the served level. A value whose shape does not
// fit its schema otherwise, such as an object where the schema wants a
// string, is stage F's (RZ-CFG-005) and is not judged here; open content
// is not either. Dispatch follows the instance (a Policy's config by its
// type), and each object's members are looked up in the schema's property
// map, so the walk is linear in the resource.
func (d *Decoder) newer(e *entry, in *input) diag.List {
	rs, ok := d.idx.Resource(string(e.res.ID.Kind))
	if !ok {
		return nil
	}
	nw := newerWalk{d: d, in: in, id: e.res.ID.ResourceID()}
	nw.node(e.res.Root, rs)
	return nw.out
}

// newerWalk is the state of one newer walk: the path from the resource
// root as frames, turned into a key-aware path only for a finding.
type newerWalk struct {
	d      *Decoder
	in     *input
	id     *diag.ResourceID
	frames []frame
	out    diag.List
}

// frame is one step of the walk: a member name, or element i of a list
// whose schema is list. elem caches the element's key-aware path element
// once a finding needed it, so many findings below one large keyed entry
// read its key once.
type frame struct {
	name string
	list *schemaidx.Node
	item *tree.Node
	i    int
	elem *diag.PathElem
}

func (w *newerWalk) node(n *tree.Node, s *schemaidx.Node) {
	switch n.Kind {
	case tree.KindMap:
		s = s.Select(n)
		if !s.Types().Has(schemaidx.TypeObject) {
			return
		}
		for i := range n.Members {
			m := &n.Members[i]
			c := memberSchema(s, m.Key)
			switch {
			case c == nil && !s.Closed():
				// Open content: kept and normalized generically.
			case c == nil:
				w.report(m, "field is unknown to "+w.d.idx.APIVersion()+" at schema level "+strconv.Itoa(w.d.level)+"; the content comes from a newer release")
			case c.Since() > w.d.level:
				w.report(m, "field is at schema level "+strconv.Itoa(c.Since())+", above this release's level "+strconv.Itoa(w.d.level))
			default:
				w.frames = append(w.frames, frame{name: m.Key})
				w.node(m.Value, c)
				w.frames = w.frames[:len(w.frames)-1]
			}
		}
	case tree.KindList:
		it, ok := s.Items()
		if !ok {
			return
		}
		for i, x := range n.Items {
			w.frames = append(w.frames, frame{list: s, item: x, i: i})
			w.node(x, it)
			w.frames = w.frames[:len(w.frames)-1]
		}
	default:
	}
}

// report records a finding at member m of the current object.
func (w *newerWalk) report(m *tree.Member, msg string) {
	p := make(diag.Path, 0, len(w.frames)+1)
	for i := range w.frames {
		f := &w.frames[i]
		switch {
		case f.list == nil:
			p = append(p, diag.Field(f.name))
		case f.elem == nil:
			e := schemaidx.ItemElem(f.list, f.item, f.i)
			f.elem = &e
			fallthrough
		default:
			p = append(p, *f.elem)
		}
	}
	p = append(p, diag.Field(m.Key))
	w.out = append(w.out, finding(CodeNewer, w.in.location(m.KeyPos), w.id, p, msg))
}

// form re-encodes a resource and records where its bytes differ from the
// content; buf is scratch and is returned for reuse.
func (d *Decoder) form(w *writer, buf []byte, e *entry, content []byte, in *input) []byte {
	e.diff = -1
	kind := string(e.res.ID.Kind)
	out, err := w.resource(buf, kind, e.res.ID.Name, e.res.Root, d.enc.resourceSchema(kind))
	if err != nil {
		var ne *nodeError
		f := finding(CodeMismatch, in.location(e.res.Start), e.res.ID.ResourceID(), nil, notCanonical+": "+err.Error())
		if errors.As(err, &ne) {
			f.Path = ne.path()
			if n, ok := e.res.Root.At(f.Path); ok {
				f.Location = in.location(n.Pos)
			}
		}
		e.form = &f
		return out
	}
	if got := content[e.start:e.end]; !bytes.Equal(out, got) {
		e.diff = e.start + firstDiff(out, got)
	}
	return out
}

// sequence checks what lies between the resources: the document header,
// one comma between resources in canonical order without duplicates, and
// the trailer. It returns the first failure by offset, including the
// resources' own.
func (d *Decoder) sequence(entries []entry, content []byte, in *input) *diag.Diagnostic {
	mismatch := func(off int, msg string) *diag.Diagnostic {
		f := finding(CodeMismatch, in.at(off), nil, nil, notCanonical+": "+msg)
		return &f
	}
	pos := 0
	expect := func(lit string, end int) *diag.Diagnostic {
		got := content[pos:end]
		if string(got) != lit {
			return mismatch(pos+firstDiff([]byte(lit), got), "the canonical encoding differs here")
		}
		pos = end
		return nil
	}
	for i := range entries {
		e := &entries[i]
		sep := ","
		if i == 0 {
			sep = header
		}
		if f := expect(sep, e.start); f != nil {
			return f
		}
		if i > 0 {
			prev := item{id: entries[i-1].res.ID}
			cur := item{id: e.res.ID}
			switch c := compareItems(&prev, &cur); {
			case c == 0:
				f := finding(CodeMismatch, in.location(e.res.Start), e.res.ID.ResourceID(), nil, notCanonical+": the resource appears twice")
				return &f
			case c > 0:
				f := finding(CodeMismatch, in.location(e.res.Start), e.res.ID.ResourceID(), nil, notCanonical+": resources are not in kind and name order")
				return &f
			}
		}
		if e.form != nil {
			return e.form
		}
		if e.diff >= 0 {
			f := finding(CodeMismatch, in.at(e.diff), e.res.ID.ResourceID(), nil, notCanonical+": the canonical encoding differs here")
			return &f
		}
		pos = e.end
	}
	if len(entries) == 0 {
		return expect(header+trailer, len(content))
	}
	return expect(trailer, len(content))
}

// firstDiff returns the index of the first byte where a and b differ, or
// the shorter length.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	return n
}

// parser builds a positioned tree from the content with jsonval's strict
// scanner (duplicate names rejected, depth limited). Values of every kind
// are built, null included, so the shape checks can locate them. Nodes
// come from slabs, each container's members and items collect on scratch
// stacks and are copied once into a slice of the exact size, and short
// names and strings are interned, so a large Revision decodes with few
// allocations (the 25 ms verify budget, target).
type parser struct {
	s       jsonval.Scanner
	in      *input
	slab    []tree.Node
	members []tree.Member
	items   []*tree.Node
	strs    map[string]string
}

// Parser bounds: nodes per slab, the longest interned string and the
// number of interned strings, which keeps hostile content from growing
// the table without bound.
const (
	slabSize    = 512
	internBytes = 32
	internMax   = 4096
)

// parse builds the document tree and the content byte ranges of the
// elements of its top-level resources array. A scan failure is
// CodeMismatch at its offset, wrapping the jsonval error.
func parse(in *input, maxDepth int) (*tree.Node, [][2]int, error) {
	p := &parser{in: in, strs: make(map[string]string)}
	p.s.Reset(in.content(), jsonval.ScanOptions{MaxDepth: maxDepth})
	doc, spans, err := p.document()
	if err != nil {
		var se *jsonval.Error
		if !errors.As(err, &se) {
			return nil, nil, err
		}
		msg := "invalid JSON: " + se.Msg
		if errors.Is(se, jsonval.ErrDepth) {
			msg = "nesting deeper than " + strconv.Itoa(maxDepth)
		}
		return nil, nil, fail(CodeMismatch, err, finding(CodeMismatch, in.at(se.Offset), nil, nil, notCanonical+": "+msg))
	}
	return doc, spans, nil
}

// node returns a fresh node from the slab.
func (p *parser) node() *tree.Node {
	if len(p.slab) == 0 {
		p.slab = make([]tree.Node, slabSize)
	}
	n := &p.slab[0]
	p.slab = p.slab[1:]
	return n
}

// text returns the decoded value of a string or name token, interning
// short unescaped ones; the map lookup by converted bytes does not
// allocate.
func (p *parser) text(t jsonval.Token) string {
	if t.Escaped || t.End-t.Offset-2 > internBytes {
		return p.s.Text(t)
	}
	raw := p.s.Bytes(t)
	raw = raw[1 : len(raw)-1]
	if v, ok := p.strs[string(raw)]; ok {
		return v
	}
	v := string(raw)
	if len(p.strs) < internMax {
		p.strs[v] = v
	}
	return v
}

// number returns a number token's text, interned when short.
func (p *parser) number(t jsonval.Token) string {
	raw := p.s.Bytes(t)
	if len(raw) > internBytes {
		return string(raw)
	}
	if v, ok := p.strs[string(raw)]; ok {
		return v
	}
	v := string(raw)
	if len(p.strs) < internMax {
		p.strs[v] = v
	}
	return v
}

// document reads the one top-level value; for a top-level object it
// records the byte range of every element of its resources array.
func (p *parser) document() (*tree.Node, [][2]int, error) {
	t, err := p.s.Next()
	if err != nil {
		return nil, nil, err
	}
	var doc *tree.Node
	var spans [][2]int
	if t.Kind == jsonval.KindObjectStart {
		doc, spans, err = p.top(t)
	} else {
		doc, _, err = p.value(t)
	}
	if err != nil {
		return nil, nil, err
	}
	if _, err := p.s.Next(); !errors.Is(err, io.EOF) {
		return nil, nil, err
	}
	return doc, spans, nil
}

// top reads the top-level object.
func (p *parser) top(start jsonval.Token) (*tree.Node, [][2]int, error) {
	n := p.node()
	*n = tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	var spans [][2]int
	for {
		nt, err := p.s.Next()
		if err != nil {
			return nil, nil, err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			return n, spans, nil
		}
		// Positions are taken in offset order, which keeps the locator
		// linear.
		name, keyPos := p.text(nt), p.in.pos(nt.Offset)
		vt, err := p.s.Next()
		if err != nil {
			return nil, nil, err
		}
		var v *tree.Node
		if name == "resources" && vt.Kind == jsonval.KindArrayStart {
			v, spans, _, err = p.array(vt, true)
		} else {
			v, _, err = p.value(vt)
		}
		if err != nil {
			return nil, nil, err
		}
		n.Members = append(n.Members, tree.Member{Key: name, KeyPos: keyPos, Value: v})
	}
}

// value reads the value starting with t and returns it with the offset
// just past it.
func (p *parser) value(t jsonval.Token) (*tree.Node, int, error) {
	switch t.Kind {
	case jsonval.KindArrayStart:
		n, _, end, err := p.array(t, false)
		return n, end, err
	case jsonval.KindObjectStart:
		return p.object(t)
	default:
	}
	n := p.node()
	*n = tree.Node{Style: tree.StyleJSON, Pos: p.in.pos(t.Offset)}
	switch t.Kind {
	case jsonval.KindNull:
		n.Kind = tree.KindNull
	case jsonval.KindTrue, jsonval.KindFalse:
		n.Kind, n.Bool = tree.KindBool, t.Kind == jsonval.KindTrue
	case jsonval.KindNumber:
		n.Kind, n.Text = tree.KindInt, p.number(t)
		if strings.ContainsAny(n.Text, ".eE") {
			n.Kind = tree.KindFloat
		}
	case jsonval.KindString:
		n.Kind, n.Text = tree.KindString, p.text(t)
	default:
		// Unreachable: the scanner yields only value starts here.
		return nil, 0, &jsonval.Error{Kind: jsonval.ErrSyntax, Msg: "invalid character at start of value", Offset: t.Offset, Other: -1}
	}
	return n, t.End, nil
}

// array reads an array after its start token and returns it with the
// offset just past it; with spans it records each element's byte range.
func (p *parser) array(start jsonval.Token, spans bool) (*tree.Node, [][2]int, int, error) {
	n := p.node()
	*n = tree.Node{Kind: tree.KindList, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	base := len(p.items)
	var out [][2]int
	for {
		t, err := p.s.Next()
		if err != nil {
			return nil, nil, 0, err
		}
		if t.Kind == jsonval.KindArrayEnd {
			n.Items = make([]*tree.Node, len(p.items)-base)
			copy(n.Items, p.items[base:])
			clear(p.items[base:])
			p.items = p.items[:base]
			return n, out, t.End, nil
		}
		v, end, err := p.value(t)
		if err != nil {
			return nil, nil, 0, err
		}
		p.items = append(p.items, v)
		if spans {
			out = append(out, [2]int{t.Offset, end})
		}
	}
}

// object reads an object after its start token and returns it with the
// offset just past it.
func (p *parser) object(start jsonval.Token) (*tree.Node, int, error) {
	n := p.node()
	*n = tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	base := len(p.members)
	for {
		nt, err := p.s.Next()
		if err != nil {
			return nil, 0, err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			if k := len(p.members) - base; k > 0 {
				n.Members = make([]tree.Member, k)
				copy(n.Members, p.members[base:])
				clear(p.members[base:])
				p.members = p.members[:base]
			}
			return n, nt.End, nil
		}
		name, keyPos := p.text(nt), p.in.pos(nt.Offset)
		vt, err := p.s.Next()
		if err != nil {
			return nil, 0, err
		}
		v, _, err := p.value(vt)
		if err != nil {
			return nil, 0, err
		}
		p.members = append(p.members, tree.Member{Key: name, KeyPos: keyPos, Value: v})
	}
}
