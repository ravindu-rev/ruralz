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
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// DefaultMaxDepth is the nesting limit of canonical content when
// DecoderOptions leaves it zero: twice the authoring limit of 64, so
// every Revision this release builds decodes, with room for the document
// and resources levels and materialized object defaults.
const DefaultMaxDepth = 128

// DefaultMaxResources is the most elements of the resources array a decode
// reads when DecoderOptions leaves MaxResources zero: the loader's
// resource limit (20,000, target), which no Revision of this release
// exceeds, since a Node enforces no limit below the CLI default (proposed
// for canonical content).
const DefaultMaxResources = 20_000

// DefaultMaxValues is the most JSON values (objects, arrays and scalars,
// the document included) a decode builds when DecoderOptions leaves
// MaxValues zero: 2^26, one value per byte of the loader's 64 MiB source
// limit. Every value of an authored source takes at least one byte, and
// the defaults stage G adds to an object (at most four in the M1 schema)
// are fewer than the bytes the object and its member name or list
// separator take, so no Revision of this release exceeds it (proposed). A
// value costs about 120 to 160 bytes of tree (a tree.Node and its member
// or item slot), so the decoded trees of content at this budget take some
// 8 to 10 GiB; see the package documentation.
const DefaultMaxValues = 1 << 26

// maxCount is the most any count option of a Decoder means: a larger
// MaxDiagnostics, MaxResources or MaxValues means 2^30, so a caller can
// pass math.MaxInt for no practical limit and every count the decoder
// derives from one (a cap plus one, twice a cap) stays far from overflow.
const maxCount = 1 << 30

// countOption returns the count option v, def when v is zero or negative
// and maxCount at most.
func countOption(v, def int) int {
	switch {
	case v <= 0:
		return def
	case v > maxCount:
		return maxCount
	default:
		return v
	}
}

// DecoderOptions configures a Decoder.
type DecoderOptions struct {
	// MaxDepth limits the nesting of the content (DefaultMaxDepth when
	// zero or negative, jsonval.MaxDepthLimit at most). Deeper content is
	// CodeLimit.
	MaxDepth int
	// Workers is the number of goroutines a decode runs on: the calling
	// goroutine parses, and up to Workers-1 more check and re-encode
	// resources meanwhile, each started only when the parser hands a
	// chunk of resources over, so a decode never runs more goroutines
	// than it has chunks. Less than 1 means 1, which checks on the
	// calling goroutine; more than 256 means 256.
	Workers int
	// MaxDiagnostics caps the findings of one Decode, Verify or ReadDump
	// (DefaultMaxDiagnostics when zero or negative, 2^30 at most). A
	// failing decode reports the first MaxDiagnostics findings of the code
	// it fails with, in content order, and, when it met more, one
	// CodeTooMany diagnostic after them. The parse stops once more than
	// MaxDiagnostics newer findings (CodeNewer) are known; form findings
	// (CodeMismatch) past the cap are dropped without stopping the parse,
	// which goes on looking for newer findings. See the package
	// documentation.
	MaxDiagnostics int
	// MaxResources limits the elements of the content's resources array
	// (DefaultMaxResources when zero or negative, 2^30 at most). The parse
	// stops at the first element past it, with one CodeLimit finding
	// located there.
	MaxResources int
	// MaxValues limits the JSON values the parser builds, the document,
	// its members and every nested value included (DefaultMaxValues when
	// zero or negative, 2^30 at most), which bounds the memory of the
	// decoded trees, about 120 to 200 bytes per value. It is also the only
	// bound of a failing decode's element in flight: an element is built
	// in full before it is checked, so one wide element of a decode known
	// to fail holds its tree until it is checked, some 60 times its bytes
	// for a list of zeros (see the package documentation, Memory). The
	// parse stops at the first value past it, with one CodeLimit finding
	// located there.
	MaxValues int
	// Now is the clock of the yield timer, as Options.Now:
	// every goroutine of a Decode, Verify or ReadDump reads it between two
	// units of work (two elements parsed or checked, two entries sequenced
	// or built, two pieces of 64 KiB hashed or copied) and every 256 units
	// inside one (values built, walked or re-encoded, members judged,
	// tokens scanned, findings offered, sorted or listed), and calls
	// runtime.Gosched once YieldAfter has passed since its last yield, so
	// a Last-Known-Good or handover candidate verified by a serving Node
	// shares the processor with live traffic (see the package
	// documentation, Concurrency). Goroutines call it concurrently, so it
	// must be safe for concurrent use; it must not panic (a panic in it
	// surfaces as one in the decode does, see the package documentation).
	// Nil never yields and reads no clock.
	Now func() time.Time
	// YieldAfter is the work a goroutine does before it yields; zero or
	// negative means DefaultYieldAfter (100 µs).
	YieldAfter time.Duration
}

// Decoder reads ruralz.canonical.v1 content back into resource trees. It
// is immutable and safe for concurrent use.
type Decoder struct {
	idx *schemaidx.Index
	enc *Encoder
	// level is the served schema level: a field above it is from a newer
	// producer.
	level        int
	maxDepth     int
	workers      int
	maxDiags     int
	maxResources int
	maxValues    int
	now          func() time.Time
	yieldAfter   time.Duration
	// observe, set by tests only, receives the yield timer of each
	// goroutine of a call once the goroutine is done with it.
	observe func(*yielder)
}

// NewDecoder returns a decoder for content of the hub apiVersion idx
// serves. Decoding needs idx; a nil idx makes every call (Decode, Verify
// and ReadDump) fail with ErrNoIndex before it reads, hashes or decodes
// anything.
func NewDecoder(idx *schemaidx.Index, o DecoderOptions) *Decoder {
	depth := o.MaxDepth
	switch {
	case depth <= 0:
		depth = DefaultMaxDepth
	case depth > jsonval.MaxDepthLimit:
		depth = jsonval.MaxDepthLimit
	}
	d := &Decoder{
		idx: idx, enc: NewEncoder(idx), maxDepth: depth, workers: workerCount(o.Workers),
		maxDiags:     countOption(o.MaxDiagnostics, DefaultMaxDiagnostics),
		maxResources: countOption(o.MaxResources, DefaultMaxResources),
		maxValues:    countOption(o.MaxValues, DefaultMaxValues),
		now:          o.Now, yieldAfter: yieldAfter(o.YieldAfter),
	}
	if idx != nil {
		d.level = idx.Level()
	}
	return d
}

// newYielder returns the yield timer of one goroutine of a call.
func (d *Decoder) newYielder() *yielder { return newYielder(d.now, d.yieldAfter) }

// done hands a goroutine's yield timer to the tests' observer.
func (d *Decoder) done(y *yielder) {
	if d.observe != nil {
		d.observe(y)
	}
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
// content from a newer producer, with CodeMismatch for anything that is
// not exactly ruralz.canonical.v1 bytes and with CodeLimit for content
// past a limit of the DecoderOptions (nesting, resources, values); see the
// package documentation. The error's Findings locate every problem, the
// first DecoderOptions.MaxDiagnostics of them when there are more. It
// applies no integer range rule and materializes no default. A non-nil
// error that is not an errcode error is ctx's or ErrNoIndex.
func (d *Decoder) Decode(ctx context.Context, content []byte, src Source) ([]tree.Resource, error) {
	if d.idx == nil {
		return nil, ErrNoIndex
	}
	y := d.newYielder()
	defer d.done(y)
	return d.decode(ctx, newInput(content, 0, len(content), src), y)
}

// Verify checks that content hashes to want (CodeMismatch "content hashes
// to sha256:X, want sha256:Y" otherwise) and then decodes it as Decode
// does; a Decoder without a schema index fails with ErrNoIndex before
// hashing. Last-Known-Good boot, handover candidates and dumps read by the
// CLI verify.
func (d *Decoder) Verify(ctx context.Context, content []byte, want revision.Digest, src Source) ([]tree.Resource, error) {
	if d.idx == nil {
		return nil, ErrNoIndex
	}
	y := d.newYielder()
	defer d.done(y)
	if err := hashMatches(content, want, src.Name, y); err != nil {
		return nil, err
	}
	return d.decode(ctx, newInput(content, 0, len(content), src), y)
}

// hashMatches reports CodeMismatch when content does not hash to want.
func hashMatches(content []byte, want revision.Digest, name string, y *yielder) error {
	if got := sum(content, y); got != want {
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
	locator *locator
}

func newInput(body []byte, lo, hi int, src Source) *input {
	files := src.Files
	if files == nil {
		files = &tree.FileTable{}
	}
	in := &input{body: body, lo: lo, hi: hi, name: src.Name, files: files, locator: newLocator(body)}
	in.file = files.Add(tree.File{Path: src.Name, Role: tree.RoleCanonical})
	return in
}

// content returns the canonical bytes.
func (in *input) content() []byte { return in.body[in.lo:in.hi] }

// pos returns the tree position of a content offset.
func (in *input) pos(off int) tree.Pos { return posOf(in.file, in.locator.Position(in.lo+off)) }

// posOf returns the tree position in file of a locator position. A line
// or column past what tree.Pos holds (content over 2 GiB) is unknown, but
// the position keeps its file, so a later stage still reads the file's
// role (tree.RoleCanonical, which exempts it from the integer range rule)
// and no other file's.
func posOf(file tree.FileID, p jsonval.Position) tree.Pos {
	if p.Line > math.MaxInt32 || p.Column > math.MaxInt32 {
		return tree.Pos{File: file}
	}
	return tree.Pos{File: file, Line: int32(p.Line), Column: int32(p.Column)} //nolint:gosec // G115: bounded by the check above.
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

// entry is one resource of a canonical kind with a string metadata.name,
// as parsed (node, without the apiVersion its envelope gets), with the
// byte range of its object in the content.
type entry struct {
	id         tree.ID
	node       *tree.Node
	start, end int
	// form is the failure to re-encode the resource, and diff the content
	// offset where the re-encoded bytes first differ, or -1. Workers
	// record offsets only: the locator that turns them into positions is
	// not safe for concurrent use.
	form *diag.Diagnostic
	diff int
}

// decode parses and checks the content and decides the outcome, in this
// order:
//
//  1. more than maxDiags newer findings before the point where the parser
//     stopped: the first maxDiags of them and one CodeTooMany;
//  2. invalid JSON (CodeMismatch), a limit of the decoder passed
//     (CodeLimit) or a format other than ruralz.canonical.v1 (CodeNewer):
//     its one finding;
//  3. newer findings of the document, its resources or their fields;
//  4. form findings, the first maxDiags and one CodeTooMany when there
//     are more;
//  5. the canonical bytes, judged by the re-encoding of each resource.
//
// The parser stops at the first of the outcomes of item 2 and the point
// where it knows the newer findings before it passed the cap, and every
// element it completed before stopping is checked; so the outcome is a
// function of the content, never of how far the parser got before a
// worker reported.
//
// y is the calling goroutine's yield timer.
func (d *Decoder) decode(ctx context.Context, in *input, y *yielder) ([]tree.Resource, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	r := newRun(d.maxDiags)
	doc, chunks, end, err := d.stream(ctx, in, r, y)
	if err != nil {
		return nil, err
	}
	newer := r.newer.list(y)
	if len(newer) > d.maxDiags {
		return nil, d.tooMany(CodeNewer, newer, in)
	}
	switch {
	case errors.Is(end, errOther):
		return nil, fail(CodeNewer, nil, r.other)
	case end != nil:
		// Invalid JSON or a limit. The parser stops with errStop only once
		// the newer findings passed the cap, which the check above
		// reported.
		return nil, end
	}
	d.envelope(doc, in, r, y)
	if len(newer) > 0 {
		return nil, fail(CodeNewer, nil, newer...)
	}
	if form := r.form.list(y); len(form) > d.maxDiags {
		return nil, d.tooMany(CodeMismatch, form, in)
	} else if len(form) > 0 {
		return nil, fail(CodeMismatch, nil, form...)
	}
	n := 0
	for _, c := range chunks {
		y.charge(siteChunk, 1)
		n += len(c.entries)
	}
	entries := make([]entry, 0, n)
	for _, c := range chunks {
		y.charge(siteChunk, 1)
		entries = append(entries, c.entries...)
	}
	if f := d.sequence(entries, in.content(), in, y); f != nil {
		return nil, fail(CodeMismatch, nil, *f)
	}
	return d.envelopes(entries, y), nil
}

// envelopes builds the envelope tree of each entry, with the served
// apiVersion added in front of the resource's members, in two
// allocations: an apiVersion node and a root per resource, and the roots'
// members, each root's slice with its own length as capacity, so a later
// stage that appends to one reallocates instead of writing into its
// neighbor. Only a decode that succeeds builds them.
func (d *Decoder) envelopes(entries []entry, y *yielder) []tree.Resource {
	n := 0
	for i := range entries {
		n += len(entries[i].node.Members) + 1
	}
	nodes, members := make([]tree.Node, 2*len(entries)), make([]tree.Member, n)
	out := make([]tree.Resource, len(entries))
	av := d.idx.APIVersion()
	for i := range entries {
		y.check(siteEnvelope)
		e := &entries[i]
		rn := e.node
		root, version := &nodes[2*i], &nodes[2*i+1]
		k := len(rn.Members) + 1
		ms := members[:0:k]
		members = members[k:]
		*version = tree.Node{Kind: tree.KindString, Style: tree.StyleJSON, Pos: rn.Pos, Text: av}
		ms = append(ms, tree.Member{Key: "apiVersion", KeyPos: rn.Pos, Value: version})
		ms = append(ms, rn.Members...)
		*root = tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: rn.Pos, Members: ms}
		out[i] = tree.Resource{ID: e.id, APIVersion: av, Root: root, Start: rn.Pos}
	}
	return out
}

// tooMany returns the error of a decode that stopped at its cap: ds holds
// more than d.maxDiags findings of code in content order, of which the
// first d.maxDiags are kept, followed by one CodeTooMany
// "too many diagnostics; stopped after N", as diag.Collector writes it.
func (d *Decoder) tooMany(code string, ds diag.List, in *input) error {
	ds = ds[:d.maxDiags+1]
	ds[d.maxDiags] = diag.Diagnostic{
		Code: CodeTooMany, Severity: diag.SeverityError, Location: diag.Location{File: in.name},
		Message: "too many diagnostics; stopped after " + strconv.Itoa(d.maxDiags),
	}
	return errcode.Wrap(code, &Findings{Diagnostics: ds, Truncated: true})
}

// envelope applies the document rules of a parse that read the whole
// content: a document that is not an object, and a missing format or a
// missing or non-array resources member, are form findings. The newer
// members of the document and a format other than ruralz.canonical.v1
// were judged by the parser as it read them, so a format here is that
// string.
func (d *Decoder) envelope(doc *tree.Node, in *input, r *run, y *yielder) {
	if doc.Kind != tree.KindMap {
		r.form.add(order{member: -1}, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": not a JSON object"), y)
		return
	}
	var format, resources *tree.Node
	rk := -1
	for i := range doc.Members {
		y.charge(siteDocument, 1)
		switch m := &doc.Members[i]; m.Key {
		case "format":
			format = m.Value
		case "resources":
			resources, rk = m.Value, i
		}
	}
	if format == nil {
		r.form.add(order{member: -1}, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": no format member"), y)
	}
	switch {
	case resources == nil:
		r.form.add(order{member: -1, seq: 1}, finding(CodeMismatch, in.location(doc.Pos), nil, nil, notCanonical+": no resources member"), y)
	case resources.Kind != tree.KindList:
		r.form.add(order{member: rk}, finding(CodeMismatch, in.location(resources.Pos), nil, diag.Path{diag.Field("resources")}, notCanonical+": resources is not an array"), y)
	}
}

// ident is the identity the findings of one resource carry, built only
// when the resource's first finding needs it, so a resource without a
// finding costs no allocation for it: a resource whose kind and
// metadata.name are strings is named by them (shown) and its paths start
// at its root; any other has no identity and its paths start at the
// document root (resources[3]). One ident is owned by one checker and
// reset for each resource.
type ident struct {
	kind, name string
	named      bool
	i          int
	built      bool
	id         *diag.ResourceID
	base       diag.Path
}

// reset prepares x for resource i, named by kind and name when named.
func (x *ident) reset(i int, named bool, kind, name string) {
	*x = ident{kind: kind, name: name, named: named, i: i}
}

// get returns the identity and the base path of the resource's findings.
func (x *ident) get() (*diag.ResourceID, diag.Path) {
	if !x.built {
		x.built = true
		if x.named {
			x.id = resourceID(x.kind, x.name)
		} else {
			x.base = resourcePath(x.i, "")
		}
	}
	return x.id, x.base
}

// at returns the path of the resource's member, its name bounded.
func (x *ident) at(member string) diag.Path {
	_, base := x.get()
	return base.Append(diag.Field(clip(member)))
}

// resource checks resource i, collecting its findings in c.found in
// content order, and returns its entry (node nil when it has none).
//
// A resource whose kind and metadata.name are strings is identified by
// them in its findings (a kind this release does not know included), and
// their paths start at the resource root (status, kind, spec.x); the
// findings of a resource without a string kind or name carry no identity
// and a path from the document root (resources[3], resources[3].spec.x).
// Whenever the kind is known to the hub schema, metadata and spec are
// walked for newer fields, even when the envelope has a form problem (no
// string metadata.name, or Environment or Cluster, which the format never
// holds), so a newer producer wins there too. A resource without a string
// kind, or of a kind this release does not know, is judged by its envelope
// alone. Only a resource of a canonical kind with a string name has an
// entry, and only it is re-encoded.
func (c *checker) resource(rn *tree.Node, i int) entry {
	d, in, f, who := c.d, c.in, &c.found, &c.who
	if rn.Kind != tree.KindMap {
		if f.takeForm() {
			f.form = append(f.form, finding(CodeMismatch, in.location(rn.Pos), nil, resourcePath(i, ""), notCanonical+": resource is not an object"))
		}
		return entry{}
	}
	var kindN, meta, name *tree.Node
	for j := range rn.Members {
		switch m := &rn.Members[j]; m.Key {
		case "kind":
			kindN = m.Value
		case "metadata":
			meta = m.Value
		}
	}
	if meta != nil {
		name, _ = meta.Get("name")
	}
	hasKind := kindN != nil && kindN.Kind == tree.KindString
	hasName := name != nil && name.Kind == tree.KindString
	var kindText, nameText string
	if hasKind {
		kindText = kindN.Text
	}
	if hasName {
		nameText = name.Text
	}
	who.reset(i, hasKind && hasName, kindText, nameText)
	// Problems of the resource as a whole are located at its start, before
	// its members.
	switch {
	case !hasKind && f.takeForm():
		_, base := who.get()
		f.form = append(f.form, finding(CodeMismatch, in.location(rn.Pos), nil, base, notCanonical+": resource has no kind"))
	case hasKind && !hasName && f.takeForm():
		_, base := who.get()
		f.form = append(f.form, finding(CodeMismatch, in.location(rn.Pos), nil, base, notCanonical+": resource has no metadata.name"))
	}
	var rootS *schemaidx.Node
	known := false
	if hasKind {
		var schema *schemaidx.Node
		if schema, known = d.idx.Resource(kindText); known {
			if rootS = schema.Select(rn); !rootS.Types().Has(schemaidx.TypeObject) {
				rootS = nil
			}
		}
	}
	kind := v1alpha1.Kind(kindText)
	c.nw.start(who, f)
	defer c.nw.done()
	for j := range rn.Members {
		if !f.takeNewer() {
			// The resource already holds as many newer findings as the decode
			// keeps; its form findings no longer matter.
			break
		}
		c.y.charge(siteMember, 1)
		switch m := &rn.Members[j]; m.Key {
		case "kind":
			switch {
			case !hasKind:
			case !known:
				id, _ := who.get()
				f.newer = append(f.newer, finding(CodeNewer, in.location(kindN.Pos), id, who.at("kind"),
					"kind "+strconv.Quote(clip(kindText))+" is unknown to "+d.idx.APIVersion()+"; the content comes from a newer release"))
			case !canonicalKind(kind) && f.takeForm():
				id, _ := who.get()
				f.form = append(f.form, finding(CodeMismatch, in.location(kindN.Pos), id, who.at("kind"),
					notCanonical+": kind "+kindText+" never appears in "+Format))
			}
		case "metadata", "spec":
			if rootS != nil {
				c.nw.member(m, rootS)
			}
		case "apiVersion":
			if f.takeForm() {
				id, _ := who.get()
				f.form = append(f.form, finding(CodeMismatch, in.location(m.KeyPos), id, who.at(m.Key), notCanonical+": a resource carries no apiVersion"))
			}
		default:
			id, _ := who.get()
			f.newer = append(f.newer, finding(CodeNewer, in.location(m.KeyPos), id, who.at(m.Key),
				"member is not part of a "+Format+" resource; the content comes from a newer release"))
		}
	}
	if !hasName || !known || !canonicalKind(kind) {
		return entry{}
	}
	return entry{id: tree.ID{Kind: kind, Name: nameText}, node: rn}
}

// resourcePath is the path of resource i of the content from the document
// root, with the member name when it is not empty.
func resourcePath(i int, name string) diag.Path {
	if name == "" {
		return diag.Path{diag.Field("resources"), diag.Index(i)}
	}
	return diag.Path{diag.Field("resources"), diag.Index(i), diag.Field(name)}
}

// newerWalk walks the metadata and spec of a resource with the hub schema
// and reports every member the schema does not know (an undeclared member
// of a closed object) or whose x-ruralz-since exceeds the served level. A
// value whose shape does not fit its schema otherwise, such as an object
// where the schema wants a string, is stage F's (RZ-CFG-005) and is not
// judged here; open content is not either, and neither is a value outside
// an enum or a dispatch value no branch matches (stage F, RZ-CFG-005).
// Dispatch follows the instance (a Policy's config by its type), and each
// object's members are looked up in the schema's property map, so the walk
// is linear in the resource. One walk is owned by one checker and reused
// for its resources; the path from the resource root is kept as frames,
// turned into a key-aware path only for a finding, and the walk stops once
// its resource holds as many findings as the decode keeps.
type newerWalk struct {
	d  *Decoder
	in *input
	y  *yielder
	// who is the identity of the resource walked.
	who    *ident
	frames []frame
	out    *found
}

// frame is one step of the walk: a member name, or element i of a list
// whose schema is list. elem caches the element's bounded key-aware path
// element once a finding needed it, so many findings below one large
// keyed entry read its key once.
type frame struct {
	name string
	list *schemaidx.Node
	item *tree.Node
	i    int
	elem *diag.PathElem
}

// start prepares the walk of one resource whose findings go to out.
func (w *newerWalk) start(who *ident, out *found) {
	w.who, w.out, w.frames = who, out, w.frames[:0]
}

// done ends the walk of a resource: the frames' scratch keeps no list item
// of it, so a resource the checks drop is not kept alive by the walk
// while the next one is parsed.
func (w *newerWalk) done() {
	clear(w.frames[:cap(w.frames)])
	w.frames = w.frames[:0]
}

func (w *newerWalk) node(n *tree.Node, s *schemaidx.Node) {
	switch n.Kind {
	case tree.KindMap:
		w.object(n, s)
	case tree.KindList:
		it, ok := s.Items()
		if !ok {
			return
		}
		for i, x := range n.Items {
			w.y.charge(siteWalkItem, 1)
			if x.Kind != tree.KindMap && x.Kind != tree.KindList {
				continue
			}
			w.frames = append(w.frames, frame{list: s, item: x, i: i})
			w.node(x, it)
			w.frames = w.frames[:len(w.frames)-1]
		}
	default:
	}
}

// object walks the members of a map node whose schema is s.
func (w *newerWalk) object(n *tree.Node, s *schemaidx.Node) {
	s = s.Select(n)
	if !s.Types().Has(schemaidx.TypeObject) {
		return
	}
	for i := range n.Members {
		if !w.out.takeNewer() {
			return
		}
		w.member(&n.Members[i], s)
	}
}

// member judges member m of an object whose schema, dispatch applied, is
// s, and walks its value.
func (w *newerWalk) member(m *tree.Member, s *schemaidx.Node) {
	w.y.charge(siteWalkMember, 1)
	c := memberSchema(s, m.Key)
	switch {
	case c == nil && !s.Closed():
		// Open content: kept and normalized generically.
	case c == nil:
		w.report(m, "field is unknown to "+w.d.idx.APIVersion()+" at schema level "+strconv.Itoa(w.d.level)+"; the content comes from a newer release")
	case c.Since() > w.d.level:
		w.report(m, "field is at schema level "+strconv.Itoa(c.Since())+", above this release's level "+strconv.Itoa(w.d.level))
	case m.Value.Kind == tree.KindMap || m.Value.Kind == tree.KindList:
		w.frames = append(w.frames, frame{name: m.Key})
		w.node(m.Value, c)
		w.frames = w.frames[:len(w.frames)-1]
	default:
		// A scalar holds no member to judge.
	}
}

// report records a finding at member m of the current object, its path
// bounded (boundElem).
func (w *newerWalk) report(m *tree.Member, msg string) {
	id, base := w.who.get()
	p := make(diag.Path, 0, len(base)+len(w.frames)+1)
	p = append(p, base...)
	for i := range w.frames {
		f := &w.frames[i]
		if f.list == nil {
			p = append(p, diag.Field(clip(f.name)))
			continue
		}
		if f.elem == nil {
			e := boundElem(schemaidx.ItemElem(f.list, f.item, f.i), f.i)
			f.elem = &e
		}
		p = append(p, *f.elem)
	}
	p = append(p, diag.Field(clip(m.Key)))
	w.out.newer = append(w.out.newer, finding(CodeNewer, w.in.location(m.KeyPos), id, p, msg))
}

// form re-encodes a resource and records where its bytes differ from the
// content; buf is scratch and is returned for reuse.
func (d *Decoder) form(w *writer, buf []byte, e *entry, content []byte, in *input) []byte {
	e.diff = -1
	kind := string(e.id.Kind)
	out, err := w.resource(buf, kind, e.id.Name, e.node, d.enc.resourceSchema(kind))
	if err != nil {
		var ne *nodeError
		f := finding(CodeMismatch, in.location(e.node.Pos), resourceID(kind, e.id.Name), nil, notCanonical+": "+err.Error())
		if errors.As(err, &ne) {
			p := ne.path()
			if n, ok := e.node.At(p); ok {
				f.Location = in.location(n.Pos)
			}
			f.Path = boundPath(p)
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
// resources' own. entries holds every resource with an entry up to the
// first whose re-encoding failed, and may stop there (run.rejected).
func (d *Decoder) sequence(entries []entry, content []byte, in *input, y *yielder) *diag.Diagnostic {
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
		y.check(siteSequence)
		e := &entries[i]
		sep := ","
		if i == 0 {
			sep = header
		}
		if f := expect(sep, e.start); f != nil {
			return f
		}
		if i > 0 {
			prev := item{id: entries[i-1].id}
			cur := item{id: e.id}
			switch c := compareItems(&prev, &cur); {
			case c == 0:
				f := finding(CodeMismatch, in.location(e.node.Pos), resourceID(string(e.id.Kind), e.id.Name), nil, notCanonical+": the resource appears twice")
				return &f
			case c > 0:
				f := finding(CodeMismatch, in.location(e.node.Pos), resourceID(string(e.id.Kind), e.id.Name), nil, notCanonical+": resources are not in kind and name order")
				return &f
			}
		}
		if e.form != nil {
			return e.form
		}
		if e.diff >= 0 {
			f := finding(CodeMismatch, in.at(e.diff), resourceID(string(e.id.Kind), e.id.Name), nil, notCanonical+": the canonical encoding differs here")
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
// are built, null included, so the shape checks can locate them, at most
// maxValues of them. The content is copied into one string once, and every
// unescaped string, name and number text is a substring of it; nodes,
// members and items come from slabs, each container's members and items
// collecting on scratch stacks and then copied once into a slab slice of
// the exact length and capacity, so a later append reallocates instead of
// writing into a neighbor. A large Revision thus decodes with a few
// allocations per thousand nodes (the 25 ms verify budget, target). The
// trees keep the content string and the slabs alive as long as any of
// their nodes is. The elements of the top-level resources array go to the
// sink only: the document tree does not hold them, so an element the
// checks dropped is garbage at once.
type parser struct {
	s       jsonval.Scanner
	in      *input
	r       *run
	y       *yielder
	str     string
	slab    []tree.Node
	mslab   []tree.Member
	islab   []*tree.Node
	members []tree.Member
	items   []*tree.Node
	// values counts the values built, at most maxValues; maxResources
	// bounds the elements of the resources array.
	values, maxValues, maxResources int
	// member is the name of the document member being read and elem the
	// index of the resources element being read, -1 outside one; they
	// locate a limit finding.
	member string
	elem   int
}

// Slab sizes, in nodes, members and items. A container larger than a
// quarter of its slab gets a slice of its own.
const (
	slabSize   = 512
	memberSlab = 1024
	itemSlab   = 1024
)

// maxScratch is the most member or item slots the parser's scratch
// stacks keep from one element of the resources array to the next: the
// stacks of a wider element are dropped once it is complete, so its
// width is not kept for the rest of the parse.
const maxScratch = 4096

// sink receives element i of the top-level resources array as soon as it
// is complete, with the content byte range [start, end) of its value. The
// parser never writes to a completed element again, so the sink may hand
// it to another goroutine. An error stops the parse.
type sink func(i int, rn *tree.Node, start, end int) error

// parse builds the document tree, passing each element of its top-level
// resources array to each, and judges the members of the document as it
// reads them: each newer member is offered to r, and a format other than
// ruralz.canonical.v1 stops the parse with errOther, its finding in
// r.other, as does the newer cap with errStop. A scan failure is
// CodeMismatch at its offset, wrapping the jsonval error, except nesting
// deeper than the decoder's limit, which is CodeLimit; more resources or
// values than the decoder's limits are CodeLimit at the element or value
// past the limit; a sink error is returned as it is.
//
// y is the calling goroutine's yield timer.
func (d *Decoder) parse(in *input, r *run, y *yielder, each sink) (*tree.Node, error) {
	p := &parser{in: in, r: r, y: y, str: copyString(in.content(), y), maxValues: d.maxValues, maxResources: d.maxResources, elem: -1}
	p.s.Reset(in.content(), jsonval.ScanOptions{MaxDepth: d.maxDepth})
	doc, err := p.document(each)
	if err != nil {
		var se *jsonval.Error
		if !errors.As(err, &se) {
			return nil, err
		}
		if errors.Is(se, jsonval.ErrDepth) {
			return nil, fail(CodeLimit, err, finding(CodeLimit, in.at(se.Offset), nil, nil, "nesting deeper than "+strconv.Itoa(d.maxDepth)))
		}
		return nil, fail(CodeMismatch, err, finding(CodeMismatch, in.at(se.Offset), nil, nil, notCanonical+": invalid JSON: "+se.Msg))
	}
	return doc, nil
}

// limit returns the CodeLimit error of a limit the content passes at
// offset off, located at the resources element or the document member
// being read.
func (p *parser) limit(off int, msg string) error {
	var path diag.Path
	switch {
	case p.elem >= 0:
		path = resourcePath(p.elem, "")
	case p.member != "":
		path = diag.Path{diag.Field(clip(p.member))}
	}
	return fail(CodeLimit, nil, finding(CodeLimit, p.in.at(off), nil, path, msg))
}

// node returns a fresh node from the slab for the value that starts at
// offset off, or the CodeLimit error of the value past maxValues.
func (p *parser) node(off int) (*tree.Node, error) {
	if p.values >= p.maxValues {
		return nil, p.limit(off, "content holds more than "+strconv.Itoa(p.maxValues)+" values, the most this decoder reads")
	}
	p.values++
	p.y.charge(siteNode, 1)
	if len(p.slab) == 0 {
		p.slab = make([]tree.Node, slabSize)
	}
	n := &p.slab[0]
	p.slab = p.slab[1:]
	return n, nil
}

// memberSlice returns a slice of k members, k > 0, with capacity k.
func (p *parser) memberSlice(k int) []tree.Member {
	if k > memberSlab/4 {
		return make([]tree.Member, k)
	}
	if len(p.mslab) < k {
		p.mslab = make([]tree.Member, memberSlab)
	}
	s := p.mslab[:k:k]
	p.mslab = p.mslab[k:]
	return s
}

// itemSlice returns a slice of k items, k > 0, with capacity k.
func (p *parser) itemSlice(k int) []*tree.Node {
	if k > itemSlab/4 {
		return make([]*tree.Node, k)
	}
	if len(p.islab) < k {
		p.islab = make([]*tree.Node, itemSlab)
	}
	s := p.islab[:k:k]
	p.islab = p.islab[k:]
	return s
}

// text returns the decoded value of a string or name token: a substring
// of the content when it holds no escape.
func (p *parser) text(t jsonval.Token) string {
	if t.Escaped {
		return p.s.Text(t)
	}
	return p.str[t.Offset+1 : t.End-1]
}

// number returns a number token's text, a substring of the content.
func (p *parser) number(t jsonval.Token) string { return p.str[t.Offset:t.End] }

// document reads the one top-level value; for a top-level object it
// passes every element of its resources array to each.
func (p *parser) document(each sink) (*tree.Node, error) {
	t, err := p.s.Next()
	if err != nil {
		return nil, err
	}
	var doc *tree.Node
	if t.Kind == jsonval.KindObjectStart {
		doc, err = p.top(t, each)
	} else {
		// Not an object: only its kind and position matter.
		doc, err = p.shallow(t)
	}
	if err != nil {
		return nil, err
	}
	if _, err := p.s.Next(); !errors.Is(err, io.EOF) {
		return nil, err
	}
	return doc, nil
}

// top reads the top-level object. Member k of the document is judged once
// its value is complete: a member other than format and resources is a
// newer finding, and a format other than the string ruralz.canonical.v1,
// of any JSON type, stops the parse, because the rest of the content
// follows rules this release does not know. Before each member the parse
// stops once the newer findings passed the cap: everything after would
// come after the findings kept.
func (p *parser) top(start jsonval.Token, each sink) (*tree.Node, error) {
	n, err := p.node(start.Offset)
	if err != nil {
		return nil, err
	}
	*n = tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	for k := 0; ; k++ {
		if p.r.newer.full.Load() {
			return nil, errStop
		}
		nt, err := p.s.Next()
		if err != nil {
			return nil, err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			return n, nil
		}
		// Positions are taken in offset order, which keeps the locator
		// linear. The scanner rejects a second resources member before
		// its value, so each sees one array.
		name, keyPos := p.text(nt), p.in.pos(nt.Offset)
		vt, err := p.s.Next()
		if err != nil {
			return nil, err
		}
		p.member = name
		var v *tree.Node
		if name == "resources" && vt.Kind == jsonval.KindArrayStart {
			p.r.newer.setResources(k)
			p.r.form.setResources(k)
			v, _, err = p.array(vt, each)
		} else {
			// Of any other member only the kind and position matter: a
			// format string, the kind of another format value or of a
			// resources member that is not an array, and of a newer
			// member nothing.
			v, err = p.shallow(vt)
		}
		if err != nil {
			return nil, err
		}
		switch name {
		case "format":
			if v.Kind != tree.KindString || v.Text != Format {
				p.r.other = finding(CodeNewer, p.in.location(v.Pos), nil, diag.Path{diag.Field("format")},
					"format "+formatText(v)+" is not served by this release, which reads "+Format)
				return nil, errOther
			}
		case "resources":
		default:
			p.r.newer.add(order{member: k}, finding(CodeNewer, p.in.location(keyPos), nil, diag.Path{diag.Field(clip(name))},
				"member is not part of "+Format+"; the content comes from a newer release"), p.y)
		}
		n.Members = append(n.Members, tree.Member{Key: name, KeyPos: keyPos, Value: v})
	}
}

// formatText shows a format value in a finding: a string Go-quoted and a
// number clipped, as the content holds them, and an object or array by
// its brackets.
func formatText(v *tree.Node) string {
	switch v.Kind {
	case tree.KindString:
		return strconv.Quote(clip(v.Text))
	case tree.KindInt, tree.KindFloat:
		return clip(v.Text)
	case tree.KindBool:
		return strconv.FormatBool(v.Bool)
	case tree.KindMap:
		return "{...}"
	case tree.KindList:
		return "[...]"
	default:
		return "null"
	}
}

// shallow reads the value starting with t when only its kind and
// position matter: a scalar as value reads it, a container as a node
// without members or items whose content the scanner skips, still
// checking it, so a value that cannot matter costs no tree.
func (p *parser) shallow(t jsonval.Token) (*tree.Node, error) {
	kind := tree.KindMap
	switch t.Kind {
	case jsonval.KindObjectStart:
	case jsonval.KindArrayStart:
		kind = tree.KindList
	default:
		n, _, err := p.value(t)
		return n, err
	}
	n, err := p.node(t.Offset)
	if err != nil {
		return nil, err
	}
	*n = tree.Node{Kind: kind, Style: tree.StyleJSON, Pos: p.in.pos(t.Offset)}
	if _, err := skip(&p.s, t, p.y); err != nil {
		return nil, err
	}
	return n, nil
}

// value reads the value starting with t and returns it with the offset
// just past it.
func (p *parser) value(t jsonval.Token) (*tree.Node, int, error) {
	switch t.Kind {
	case jsonval.KindArrayStart:
		return p.array(t, nil)
	case jsonval.KindObjectStart:
		return p.object(t)
	default:
	}
	n, err := p.node(t.Offset)
	if err != nil {
		return nil, 0, err
	}
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
// offset just past it. A non-nil each receives every element once it is
// complete, and only it: the array node then holds no items, and an
// element past maxResources stops the parse with CodeLimit. A container
// copied out of the scratch stacks leaves no link in them, and the stacks
// of a wide element are dropped once it is complete (release), so the
// scratch keeps no element the checks have dropped alive.
func (p *parser) array(start jsonval.Token, each sink) (*tree.Node, int, error) {
	n, err := p.node(start.Offset)
	if err != nil {
		return nil, 0, err
	}
	*n = tree.Node{Kind: tree.KindList, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	base := len(p.items)
	for k := 0; ; k++ {
		t, err := p.s.Next()
		if err != nil {
			return nil, 0, err
		}
		if t.Kind == jsonval.KindArrayEnd {
			switch m := len(p.items) - base; {
			case each != nil:
				p.elem = -1
			case m > 0:
				n.Items = p.itemSlice(m)
				copy(n.Items, p.items[base:])
				clear(p.items[base:])
				p.items = p.items[:base]
			default:
				n.Items = []*tree.Node{}
			}
			return n, t.End, nil
		}
		if each != nil {
			if k >= p.maxResources {
				p.elem = k
				return nil, 0, p.limit(t.Offset, "content holds more than "+strconv.Itoa(p.maxResources)+" resources, the most this decoder reads")
			}
			p.elem = k
		}
		v, end, err := p.value(t)
		if err != nil {
			return nil, 0, err
		}
		if each != nil {
			p.release()
			if err := each(k, v, t.Offset, end); err != nil {
				return nil, 0, err
			}
			continue
		}
		p.items = append(p.items, v)
	}
}

// object reads an object after its start token and returns it with the
// offset just past it.
func (p *parser) object(start jsonval.Token) (*tree.Node, int, error) {
	n, err := p.node(start.Offset)
	if err != nil {
		return nil, 0, err
	}
	*n = tree.Node{Kind: tree.KindMap, Style: tree.StyleJSON, Pos: p.in.pos(start.Offset)}
	base := len(p.members)
	for {
		nt, err := p.s.Next()
		if err != nil {
			return nil, 0, err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			if k := len(p.members) - base; k > 0 {
				n.Members = p.memberSlice(k)
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

// release drops the scratch stacks between two elements of the resources
// array, where they are empty, when an element made them wider than
// maxScratch slots.
func (p *parser) release() {
	if len(p.items) == 0 && cap(p.items) > maxScratch {
		p.items = nil
	}
	if len(p.members) == 0 && cap(p.members) > maxScratch {
		p.members = nil
	}
}
