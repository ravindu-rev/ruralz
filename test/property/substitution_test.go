// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"math/rand/v2"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Names FuzzSubstitution writes.
const (
	substVariable   = "PROPERTY_VAR"
	substAnnotation = "property.ruralz.test/subst"
	codeForbidden   = "RZ-CFG-011"
)

// FuzzSubstitution is the Substitution row of the Required properties:
// ${VAR} never injects structure or appears in forbidden positions
// (docs/architecture/02-configuration-model.md "Environment
// substitution"). Every experiment edits one document of a golden corpus
// Bundle, renders it without --env with VAR set, through the pipeline, and
// checks the outcome against an oracle that reads the schema's x-ruralz-*
// keywords itself:
//
//   - An annotation pre${VAR}post, VAR holding any value (newlines, ": ",
//     "- ", "{", "#", "${" and "$${" included): the rendered resources
//     equal the Bundle's but for that annotation, which holds pre, the
//     value and post literally, and the Revision equals the one of the
//     annotation authored as that literal text escaped (subst.Escape).
//   - A string scalar elsewhere split as pre${VAR}post, VAR holding the
//     text cut out: a free position builds the Bundle's own Revision; a
//     forbidden one (apiVersion, kind, metadata.name, an x-ruralz-ref or
//     x-ruralz-cel field, the elements and typed-map values of one, any
//     scalar of an x-ruralz-secret subtree) is RZ-CFG-011 at that resource
//     and path, and builds nothing.
//   - A mapping key renamed to hold ${VAR} is RZ-CFG-011 at that resource
//     and builds nothing.
func FuzzSubstitution(f *testing.F) {
	values := []string{"a: b\n- c", "{x: [1, 2]}", "${HOME} $${X} $${", "", "# not a comment", "\"q\" 'q' \\", "line\nbreak\ttab", "8080", "true"}
	for i, s := range seeds(f) {
		a := corpus.Archive{Comment: []byte(s.name + "\n"), Files: s.files}
		f.Add(a.Format(), values[i%len(values)], uint64(i)+1)
	}
	f.Fuzz(func(t *testing.T, archive []byte, value string, seed uint64) {
		if !utf8.ValidString(value) || len(value) > 4096 {
			t.Skip("the restricted profile reads UTF-8 only")
		}
		tc := tools(t)
		src, ok := tc.parseSource(t, newMemBundle(corpus.ParseArchive(archive).Files, nil))
		if !ok {
			t.Skip("stages A to C refuse the Bundle")
		}
		e := &substExperiment{t: t, tc: tc, r: newRand(seed), src: src}
		e.base = e.build(nil, subst.Map{})
		if e.base.Validated == nil || e.base.HasErrors() {
			t.Skip("the Bundle does not build without variables: the negative corpus covers invalid input")
		}
		e.annotation(value)
		for range 4 {
			e.scalar()
		}
		for range 2 {
			e.key()
		}
	})
}

// substExperiment edits one document of src at a time.
type substExperiment struct {
	t    *testing.T
	tc   *toolchain
	r    *rand.Rand
	src  []sourceFile
	base *pipeline.Result
}

// docRef names one base document of src.
type docRef struct{ file, doc int }

// edit replaces one document of src.
type edit struct {
	at  docRef
	doc *tree.Node
}

// build runs the Bundle of src, with ed applied, without --env over vars,
// keeping the rendered resources. Every file is written by the profile
// encoder, the edited one included, so only the edit differs from the
// base run.
func (e *substExperiment) build(ed *edit, vars subst.Map) *pipeline.Result {
	e.t.Helper()
	files := map[string][]byte{}
	for i, sf := range e.src {
		docs := sf.docs
		if ed != nil && ed.at.file == i {
			docs = append([]*tree.Node(nil), docs...)
			docs[ed.at.doc] = ed.doc
		}
		if strings.HasSuffix(sf.path, ".json") && len(docs) == 1 {
			files[sf.path] = jsonOf(e.t, docs[0])
		} else {
			files[sf.path] = yamlOf(e.t, docs)
		}
	}
	return e.tc.run(e.t, memBundle{}.withFiles(files), noEnv, vars, true)
}

// baseDocs returns every base document of src.
func (e *substExperiment) baseDocs() []docRef {
	var out []docRef
	for i, sf := range e.src {
		if sf.overlay != "" {
			continue
		}
		for j := range sf.docs {
			out = append(out, docRef{file: i, doc: j})
		}
	}
	return out
}

// doc returns a copy of the document at.
func (e *substExperiment) doc(at docRef) *tree.Node { return e.src[at.file].docs[at.doc].Clone() }

// affix returns text without "$" for either side of an expression.
func (e *substExperiment) affix() string {
	return pick(e.r, []string{"", "", "x-", "-y ", "[", ": ", "a b", "#", "é"})
}

// annotation checks an annotation pre${VAR}post holding value.
func (e *substExperiment) annotation(value string) {
	t := e.t
	refs := e.baseDocs()
	at := pick(e.r, refs)
	pre, post := e.affix(), e.affix()
	expr, taken := "${"+substVariable+"}", value
	if e.r.IntN(2) == 0 {
		// ":-" takes the default for an empty value, as POSIX shells do.
		expr = "${" + substVariable + ":-dflt}"
		if value == "" {
			taken = "dflt"
		}
	}
	withExpr := e.doc(at)
	ensureMap(ensureMap(withExpr, "metadata"), "annotations").Set(substAnnotation, tree.Pos{}, str(pre+expr+post))
	got := e.build(&edit{at: at, doc: withExpr}, subst.Map{substVariable: value})
	if got.Validated == nil || got.HasErrors() {
		t.Fatalf("an annotation %q holding %q does not build:\n%s", pre+expr+post, value, texts(got.Diagnostics))
	}
	literal := e.doc(at)
	ensureMap(ensureMap(literal, "metadata"), "annotations").Set(substAnnotation, tree.Pos{}, str(subst.Escape(pre+taken+post)))
	want := e.build(&edit{at: at, doc: literal}, subst.Map{})
	if want.Validated == nil || got.Validated.Revision.Digest != want.Validated.Revision.Digest {
		t.Fatalf("an annotation %q holding %q builds %s, the literal %q builds %v", pre+expr+post, value,
			got.Validated.Revision.Digest.Short(), pre+taken+post, want.Diagnostics)
	}
	id := idOf(withExpr)
	for _, r := range got.Rendered {
		wantTree := renderedOf(e.base, r.ID)
		if wantTree == nil {
			t.Fatalf("substituting %q rendered %s, which the Bundle does not hold", value, r.ID)
		}
		if r.ID == id {
			if a := annotationOf(r.Root); a == nil || a.Kind != tree.KindString || a.Text != pre+taken+post {
				t.Fatalf("%s: the annotation renders %v, want the literal %q", r.ID, a, pre+taken+post)
			}
			wantTree = wantTree.Clone()
			ensureMap(ensureMap(wantTree, "metadata"), "annotations").Set(substAnnotation, tree.Pos{}, str(pre+taken+post))
		}
		if got, want := canonicalText(t, r.Root), canonicalText(t, wantTree); got != want {
			t.Fatalf("%s: substituting %q changed the resource's structure:\n got %s\nwant %s", r.ID, value, got, want)
		}
	}
	if len(got.Rendered) != len(e.base.Rendered) {
		t.Fatalf("substituting %q rendered %d resources, the Bundle %d", value, len(got.Rendered), len(e.base.Rendered))
	}
}

// position is one string scalar of a document.
type position struct {
	path      diag.Path
	text      string
	forbidden bool
}

// scalar checks a string scalar split as pre${VAR}post, VAR holding the
// text cut out.
func (e *substExperiment) scalar() {
	t := e.t
	refs := e.baseDocs()
	at := pick(e.r, refs)
	doc := e.doc(at)
	var cands []position
	for _, p := range positionsOf(e.tc, doc) {
		if p.text != "" && !strings.Contains(p.text, "$") {
			cands = append(cands, p)
		}
	}
	if len(cands) == 0 {
		return
	}
	p := pick(e.r, cands)
	i := 1 + e.r.IntN(len(p.text))
	for i < len(p.text) && !utf8.RuneStart(p.text[i]) {
		i++
	}
	j := i + e.r.IntN(len(p.text)-i+1)
	for j < len(p.text) && !utf8.RuneStart(p.text[j]) {
		j++
	}
	n, ok := doc.At(p.path)
	if !ok {
		t.Fatalf("%s: no node at %s", idOf(doc), p.path)
	}
	text := p.text[:i] + "${" + substVariable + "}" + p.text[j:]
	n.Text = text
	got := e.build(&edit{at: at, doc: doc}, subst.Map{substVariable: p.text[i:j]})
	id := idOf(doc)
	if !p.forbidden {
		if got.Validated == nil || got.HasErrors() || got.Validated.Revision.Digest != e.base.Validated.Revision.Digest {
			t.Fatalf("%s %s: %q with %s=%q, at a free position, does not build the Bundle's Revision:\n%s",
				id, p.path, text, substVariable, p.text[i:j], texts(got.Diagnostics))
		}
		return
	}
	if got.Validated != nil {
		t.Fatalf("%s %s: %q, at a forbidden position, builds a Revision", id, p.path, text)
	}
	if !hasFindingAt(got.Diagnostics, codeForbidden, id, doc, n) && !envelopeRefused(got.Diagnostics, id, p.path) {
		t.Fatalf("%s %s: %q, at a forbidden position, is not %s there:\n%s", id, p.path, text, codeForbidden, texts(got.Diagnostics))
	}
}

// key checks a mapping key renamed to hold ${VAR}: any mapping but the
// envelope and metadata, whose keys stage C reads to find the resource.
func (e *substExperiment) key() {
	t := e.t
	at := pick(e.r, e.baseDocs())
	doc := e.doc(at)
	meta, _ := doc.Get("metadata")
	var holders []*tree.Node
	walkMaps(doc, 0, func(depth int, n *tree.Node) {
		if len(n.Members) > 0 && depth > 0 && n != meta {
			holders = append(holders, n)
		}
	})
	if len(holders) == 0 {
		return
	}
	holder := pick(e.r, holders)
	m := &holder.Members[e.r.IntN(len(holder.Members))]
	renamed := m.Key + "${" + substVariable + "}"
	if _, dup := holder.Get(renamed); dup {
		return
	}
	m.Key = renamed
	got := e.build(&edit{at: at, doc: doc}, subst.Map{substVariable: "x"})
	id := idOf(doc)
	if got.Validated != nil {
		t.Fatalf("%s: the key %q builds a Revision", id, renamed)
	}
	if !hasCode(got.Diagnostics, codeForbidden, id) {
		t.Fatalf("%s: the key %q is not %s:\n%s", id, renamed, codeForbidden, texts(got.Diagnostics))
	}
}

// positionsOf returns every string scalar of doc with whether ${VAR} is
// forbidden there, read from the x-ruralz-* keywords of the schema of the
// document's apiVersion: the envelope fields apiVersion, kind and
// metadata.name; an x-ruralz-ref or x-ruralz-cel field and, through list
// items and typed-map values, its elements; and every scalar under an
// x-ruralz-secret field. A document whose apiVersion or kind has no
// schema yields none.
func positionsOf(tc *toolchain, doc *tree.Node) []position {
	idx, ok := tc.pipe.Schemas().Index(stringAt(doc, "apiVersion"))
	if !ok {
		return nil
	}
	var stack []kwFrame
	var out []position
	err := idx.Walk(stringAt(doc, "kind"), doc, func(c *schemaidx.Cursor) error {
		d := c.Depth()
		for len(stack) > 0 && stack[len(stack)-1].depth >= d {
			stack = stack[:len(stack)-1]
		}
		kw := c.Info.Node.Keywords()
		f := kwFrame{depth: d, node: c.Node, schema: c.Info.Node, secret: kw.Secret, refOrCEL: kw.Ref != "" || kw.CEL != nil}
		if len(stack) > 0 {
			parent := stack[len(stack)-1]
			f.secret = f.secret || parent.secret
			if parent.refOrCEL && inherits(parent, c) {
				f.refOrCEL = true
			}
		}
		stack = append(stack, f)
		if c.Node.Kind != tree.KindString {
			return nil
		}
		path := c.Path()
		out = append(out, position{path: path, text: c.Node.Text, forbidden: f.secret || f.refOrCEL || isEnvelope(path)})
		return nil
	})
	if err != nil {
		return nil
	}
	return out
}

// kwFrame is the keyword state of one node positionsOf visited.
type kwFrame struct {
	depth  int
	node   *tree.Node
	schema *schemaidx.Node
	// secret: the node or an ancestor carries x-ruralz-secret.
	secret bool
	// refOrCEL: the node carries x-ruralz-ref or x-ruralz-cel, or inherits
	// it as an element or typed-map value.
	refOrCEL bool
}

// inherits reports whether the node at c inherits its parent's
// x-ruralz-ref or x-ruralz-cel: an item of a list, or a value of a typed
// map (additionalProperties), never a declared object member.
func inherits(parent kwFrame, c *schemaidx.Cursor) bool {
	switch parent.node.Kind {
	case tree.KindList:
		return true
	case tree.KindMap:
		last, ok := c.Last()
		if !ok || last.Kind != diag.ElemField {
			return false
		}
		if _, declared := parent.schema.Property(last.Name); declared {
			return false
		}
		_, typed := parent.schema.Values()
		return typed
	default:
		return false
	}
}

// isEnvelope reports the paths apiVersion, kind and metadata.name.
func isEnvelope(p diag.Path) bool {
	switch {
	case len(p) == 1:
		return p[0].Kind == diag.ElemField && (p[0].Name == "apiVersion" || p[0].Name == "kind")
	case len(p) == 2:
		return p[0].Kind == diag.ElemField && p[0].Name == "metadata" && p[1].Kind == diag.ElemField && p[1].Name == "name"
	default:
		return false
	}
}

// envelopeRefused reports a finding about the resource id, or a document
// without one, for an edit of an envelope field: stage C reads apiVersion,
// kind and metadata.name to find the resource before substitution runs,
// so an edited envelope field may be refused there (an unserved apiVersion
// or kind, an invalid name) instead of by RZ-CFG-011.
func envelopeRefused(ds diag.List, id tree.ID, p diag.Path) bool {
	if !isEnvelope(p) {
		return false
	}
	for _, d := range ds {
		if d.Severity == diag.SeverityError && (d.Resource == nil || d.Resource.Kind == string(id.Kind)) {
			return true
		}
	}
	return false
}

// hasFindingAt reports an error code about the resource id whose path
// names the node n of the edited document doc: a path that went through a
// keyed list entry names the entry by its edited key.
func hasFindingAt(ds diag.List, code string, id tree.ID, doc, n *tree.Node) bool {
	for _, d := range ds {
		if d.Code != code || d.Resource == nil || d.Resource.Kind != string(id.Kind) || d.Resource.Name != id.Name {
			continue
		}
		if at, ok := doc.At(d.Path); ok && at == n {
			return true
		}
	}
	return false
}

// hasCode reports an error code about the resource id.
func hasCode(ds diag.List, code string, id tree.ID) bool {
	for _, d := range ds {
		if d.Code == code && d.Resource != nil && d.Resource.Kind == string(id.Kind) && d.Resource.Name == id.Name {
			return true
		}
	}
	return false
}

// walkMaps calls fn with every mapping under n and its depth.
func walkMaps(n *tree.Node, depth int, fn func(int, *tree.Node)) {
	switch n.Kind {
	case tree.KindMap:
		fn(depth, n)
		for _, m := range n.Members {
			walkMaps(m.Value, depth+1, fn)
		}
	case tree.KindList:
		for _, it := range n.Items {
			walkMaps(it, depth+1, fn)
		}
	default:
	}
}

// idOf returns the identity of a document.
func idOf(doc *tree.Node) tree.ID {
	meta, _ := doc.Get("metadata")
	return tree.ID{Kind: v1alpha1.Kind(stringAt(doc, "kind")), Name: stringAt(meta, "name")}
}

// renderedOf returns the rendered resource id of res, or nil.
func renderedOf(res *pipeline.Result, id tree.ID) *tree.Node {
	for _, r := range res.Rendered {
		if r.ID == id {
			return r.Root
		}
	}
	return nil
}

// annotationOf returns the substAnnotation of a resource, or nil.
func annotationOf(root *tree.Node) *tree.Node {
	meta, ok := root.Get("metadata")
	if !ok {
		return nil
	}
	ann, ok := meta.Get("annotations")
	if !ok {
		return nil
	}
	v, _ := ann.Get(substAnnotation)
	return v
}
