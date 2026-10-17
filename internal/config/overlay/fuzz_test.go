// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// orderDocs is an overlay touching every directive: a merge, a delete, a
// replace, two new identities and an ordered list reorder.
var orderDocs = []string{
	`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"edge"},"spec":{"listeners":[{"name":"https","port":9443},{"name":"admin","$patch":"delete"}],"policies":[{"$patch":"replace"},{"name":"rl"},{"name":"cors"}]}}`,
	`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"ruralz.io/patch":"delete"}}}`,
	`{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u","annotations":{"ruralz.io/patch":"replace"}},"spec":{"protocol":"http","endpoints":[{"address":"b:2"}]}}`,
	`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"new"},"spec":{"match":{"path":{"prefix":"/n"}}}}`,
	`{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"added"},"spec":{"type":"cors","config":{"allowOrigins":["*"]}}}`,
}

const upstream = `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","endpoints":[{"address":"a:1"}],"timeout":"3s"}}`

// permutations returns every ordering of n indexes.
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for _, p := range permutations(n - 1) {
		for i := 0; i <= len(p); i++ {
			q := slices.Insert(slices.Clone(p), i, n-1)
			out = append(out, q)
		}
	}
	return out
}

// resultOf applies orderDocs arranged as files: perm orders the documents
// and split cuts the ordered documents into files at the given indexes;
// file names are numbered so that their lexical order is the reverse of
// the arrangement when reverse is set.
func resultOf(t *testing.T, perm []int, split []int, reverse bool) (string, int) {
	t.Helper()
	files := &tree.FileTable{}
	base := docs(t, files, tree.RoleBase, "ruralz.yaml", gateway+"\n---\n"+route+"\n---\n"+upstream)
	var groups [][]string
	start := 0
	for _, cut := range append(slices.Clone(split), len(perm)) {
		var g []string
		for _, i := range perm[start:cut] {
			g = append(g, orderDocs[i])
		}
		if len(g) > 0 {
			groups = append(groups, g)
		}
		start = cut
	}
	var over []*tree.Resource
	for i, g := range groups {
		n := i
		if reverse {
			n = len(groups) - i
		}
		over = append(over, docs(t, files, tree.RoleOverlay, "overlays/prod/f"+strconv.Itoa(n)+".yaml", strings.Join(g, "\n---\n"))...)
	}
	out, ds := pipeline(t, base, over, Options{Schemas: schemas(t), Files: files})
	var b strings.Builder
	for _, r := range out {
		b.WriteString(r.ID.String())
		b.WriteString(" ")
		b.WriteString(dump(r.Root))
		b.WriteString("\n")
	}
	return b.String(), len(ds)
}

// TestResultIndependentOfFileOrder is property P5 of spec 01 (WP-36 Done
// when): splitting an overlay across files in any order yields the same
// result.
func TestResultIndependentOfFileOrder(t *testing.T) {
	want, n := resultOf(t, []int{0, 1, 2, 3, 4}, nil, false)
	if n != 0 {
		t.Fatalf("reference arrangement has %d diagnostics", n)
	}
	for _, perm := range permutations(len(orderDocs)) {
		for _, split := range [][]int{nil, {1}, {2, 4}, {1, 2, 3, 4}} {
			for _, reverse := range []bool{false, true} {
				got, n := resultOf(t, perm, split, reverse)
				if got != want || n != 0 {
					t.Fatalf("perm %v split %v reverse %v: %d diagnostics, result\n%s\nwant\n%s", perm, split, reverse, n, got, want)
				}
			}
		}
	}
	if !strings.Contains(want, `Route/new`) || strings.Contains(want, "Route/r ") || !strings.Contains(want, `"address":"b:2"`) {
		t.Errorf("reference result misses an applied directive:\n%s", want)
	}
}

// FuzzOverlay: any base and overlay document merge without panic; every
// code is registered; the merge is deterministic; removing from the
// overlay a member whose value holds nothing but removals and replaces a
// base scalar changes neither the result nor the diagnostics (01 req 25
// "absent key: no-op", also for a member the dispatch tests); a clean
// merge is idempotent (applying the overlay again, base check included,
// changes and reports nothing) and keeps no directive: no ruralz.io/patch
// annotation and no $patch member anywhere outside a JSONSchemaDocument
// value, free content included, also when the overlay changes how base
// content is read.
func FuzzOverlay(f *testing.F) {
	seeds := [][2]string{
		{gateway, orderDocs[0]},
		{route, `{"spec":{"timeout":"10s","match":{"when":null,"methods":["GET"]},"policies":[{"$patch":"replace"},{"name":"a"}]}}`},
		{`{"kind":"Policy","spec":{"type":"authz.ip","config":{"issuers":[{"issuer":"a","x":1}]}}}`, `{"spec":{"type":"auth.jwt","config":{"issuers":[{"issuer":"a","y":2},{"issuer":"b","$patch":"delete"}]}}}`},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"type":"object","enum":[1]}}}}`, `{"spec":{"config":{"schema":{"enum":[null],"$patch":"x"}}}}`},
		{`{"kind":"Policy","spec":{"type":"plugin","config":{"a":{"b":[1]}}}}`, `{"metadata":{"annotations":{"ruralz.io/patch":"replace"}},"spec":{"config":{"a":null}}}`},
		{route, `{"metadata":{"annotations":{"ruralz.io/patch":"delete"}}}`},
		{gateway, `{"spec":{"listeners":[{"name":"x","$patch":"merge"},{"port":1},"s",{"$patch":"replace"}],"$patch":1}}`},
		{`{"spec":{"listeners":[{"name":"a"},{"name":"a"}]}}`, `{"spec":{"listeners":[{"name":"a","port":2}]}}`},
		{`{"kind":"Gateway","spec":[1,2]}`, `{"spec":{"listeners":{"a":1}}}`},
		{`{"kind":"Policy","spec":{"type":"plugin","config":{"l":[{"k":"a"}]}}}`, `{"spec":{"config":{"l":[{"$patch":"replace"},{"k":"b","$patch":"delete"}],"m":{"$patch":"x"}}}}`},
		{`{"kind":"Policy","spec":{"type":"cors","config":{"x":{"$patch":"delete"}}}}`, `{"spec":{"config":{"x":{"y":1}}}}`},
		{`{"kind":"Policy","spec":{"type":"headers","config":{"request":{"add":[{"name":"a","value":"1"}]}}}}`, `{"spec":{"config":{"request":{"add":[{"$patch":"replace"},{"name":"b","value":"2"}]}}}}`},
		// A base $patch exempt inside a JSON Schema document that the
		// overlay's spec.type moves into free content (review finding).
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}}`, `{"spec":{"type":"cors"}}`},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"allOf":[{"$patch":1}],"properties":{"$patch":{}}}}}}`, `{"spec":{"type":null,"config":{"schema":{"x":1}}}}`},
		{`{"kind":"Policy","spec":{"type":"cors","config":{"schema":{"$patch":1}}}}`, `{"spec":{"type":"validation.json-schema"}}`},
		// Removals of what the base lacks (review finding, 01 req 25
		// "absent key: no-op").
		{`{"kind":"Upstream","spec":{"protocol":"http","discovery":{"type":"dns"}}}`, `{"spec":{"endpoints":[{"address":"a:1","$patch":"delete"}],"tls":{"sni":null}}}`},
		{route, `{"spec":{"composition":{"steps":[{"name":"s","$patch":"delete"}]},"timeout":{"a":null},"policies":[{"$patch":"replace"}]}}`},
		{route, `{"metadata":{"annotations":{"ruralz.io/patch":"replace"}},"spec":{"x":null}}`},
		// An overlay value holding nothing but removals that replaces a
		// base scalar the dispatch tests (review finding): the base type
		// stays and config merges under it.
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"$patch":1}}}}`, `{"spec":{"type":{"a":null}}}`},
		{`{"kind":"Policy","spec":{"type":"auth.jwt","config":{"issuers":[{"issuer":"a"},{"issuer":"b"}]}}}`, `{"spec":{"type":{"x":null},"config":{"issuers":[{"issuer":"a","jwksUri":"k"}]}}}`},
		{`{"kind":"Policy","spec":{"type":"auth.jwt","config":{"issuers":[{"issuer":"a"},{"issuer":"b"}]}}}`, `{"spec":{"type":{"x":{"y":null}},"config":{"issuers":[{"issuer":"b","$patch":"delete"}]}}}`},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"type":"object","required":["a"]}}}}`, `{"spec":{"type":{"x":null},"config":{"schema":{"type":"string"}}}}`},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":"x"}}`, `{"spec":{"type":{"a":1},"config":{"schema":{"$patch":1}}}}`},
	}
	for _, s := range seeds {
		f.Add(s[0], s[1], true)
		f.Add(s[0], s[1], false)
	}
	f.Fuzz(func(t *testing.T, base, over string, same bool) {
		if len(base)+len(over) > 8192 {
			return
		}
		b, errB := parseJSON(base, 0)
		o, errO := parseJSON(over, 1)
		if errB != nil || errO != nil || b.Kind != tree.KindMap || o.Kind != tree.KindMap || duplicateKeys(b) || duplicateKeys(o) {
			return // stage B rejects duplicate keys (RZ-CFG-002) before any merge
		}
		kind := v1alpha1.KindRoute
		if k, ok := b.Get("kind"); ok && k.Kind == tree.KindString && slices.Contains(schemaKinds(t), k.Text) {
			kind = v1alpha1.Kind(k.Text)
		}
		name := "x"
		if !same {
			name = "y"
		}
		mk := func() ([]*tree.Resource, []*tree.Resource) {
			return []*tree.Resource{{ID: tree.ID{Kind: kind, Name: "x"}, APIVersion: v1, Root: b.Clone()}},
				[]*tree.Resource{{ID: tree.ID{Kind: kind, Name: name}, APIVersion: v1, Root: o.Clone()}}
		}
		opts := Options{Schemas: schemas(t)}
		b1, o1 := mk()
		out1, ds1 := pipeline(t, b1, o1, opts)
		b2, o2 := mk()
		out2, ds2 := pipeline(t, b2, o2, opts)
		if g, w := dumpAll(out1), dumpAll(out2); g != w || !slices.Equal(texts(ds1), texts(ds2)) {
			t.Fatalf("not deterministic:\n%s\n%s", g, w)
		}
		if same && len(out1) == 1 {
			// Removing an overlay member whose value holds nothing but
			// removals and replaces a base scalar changes neither the
			// result nor the diagnostics (01 req 25 "absent key: no-op").
			x, _ := opts.Schemas.Index(v1)
			pruned := o.Clone()
			if dropRemovals(x, string(kind), b, out1[0].Root, pruned) {
				b4 := []*tree.Resource{{ID: tree.ID{Kind: kind, Name: "x"}, APIVersion: v1, Root: b.Clone()}}
				out4, ds4 := pipeline(t, b4, []*tree.Resource{{ID: tree.ID{Kind: kind, Name: "x"}, APIVersion: v1, Root: pruned}}, opts)
				if g, w := dumpAll(out4), dumpAll(out1); g != w || !slices.Equal(texts(ds4), texts(ds1)) {
					t.Fatalf("a member holding nothing but removals over a base scalar changed the merge:\n%s %q\nwithout it\n%s %q", w, texts(ds1), g, texts(ds4))
				}
			}
		}
		for _, d := range ds1 {
			if _, ok := errcode.Lookup(d.Code); !ok {
				t.Fatalf("unregistered code %q", d.Code)
			}
		}
		if ds1.HasErrors() {
			return
		}
		_, again := mk()
		out3, ds3 := pipeline(t, cloneAll(out1), again, opts)
		if g, w := dumpAll(out3), dumpAll(out1); g != w || len(ds3) != 0 {
			t.Fatalf("not idempotent (%q):\n%s\nwant\n%s", texts(ds3), g, w)
		}
		for _, r := range out1 {
			if p, ok := strayPatch(t, r); ok {
				t.Fatalf("%s kept a %s member at %s: %s", r.ID, PatchKey, p, dump(r.Root))
			}
			md, _ := r.Root.Get("metadata")
			ann, _ := md.Get("annotations")
			if _, ok := ann.Get(PatchAnnotation); ok {
				t.Fatalf("%s kept the %s annotation", r.ID, PatchAnnotation)
			}
		}
	})
}

// dropRemovals deletes from ov, an overlay document root whose base is
// base, every member whose value is an object of nothing but nulls and
// such objects and whose base value is a scalar, at any depth of the
// objects both hold. It skips $patch members, the ruralz.io/patch
// annotation (directives, not values) and the JSONSchemaDocument
// positions of the base view and of merged, the merged root, whose
// overlay value is data. It reports whether it deleted a member.
func dropRemovals(x *schemaidx.Index, kind string, base, merged, ov *tree.Node) bool {
	dropped := false
	var walk func(b, o *tree.Node, path diag.Path)
	walk = func(b, o *tree.Node, path diag.Path) {
		kept := o.Members[:0]
		for _, om := range o.Members {
			p := path.Append(diag.Field(om.Key))
			bv, ok := b.Get(om.Key)
			switch {
			case !ok || om.Key == PatchKey || p.String() == `metadata.annotations["ruralz.io/patch"]` ||
				documentAt(x, kind, base, p) || documentAt(x, kind, merged, p):
			case nullTree(om.Value) && !composite(bv):
				dropped = true
				continue
			case om.Value.Kind == tree.KindMap && bv.Kind == tree.KindMap:
				walk(bv, om.Value, p)
			default:
			}
			kept = append(kept, om)
		}
		o.Members = kept
	}
	walk(base, ov, nil)
	return dropped
}

// nullTree reports an object with members, each null or a null tree.
func nullTree(n *tree.Node) bool {
	if n.Kind != tree.KindMap || len(n.Members) == 0 {
		return false
	}
	for _, m := range n.Members {
		if m.Value.Kind != tree.KindNull && !nullTree(m.Value) {
			return false
		}
	}
	return true
}

// documentAt reports a JSONSchemaDocument position at path p of root.
func documentAt(x *schemaidx.Index, kind string, root *tree.Node, p diag.Path) bool {
	info, ok := x.Lookup(kind, root, p)
	return ok && info.Node.Def() == schemaDocumentDef
}

// strayPatch returns the path of a $patch member of r outside the value of
// the JSONSchemaDocument definition, found with schemaidx.Walk rather
// than with the checkBase it verifies.
func strayPatch(t testing.TB, r *tree.Resource) (diag.Path, bool) {
	t.Helper()
	x, ok := schemas(t).Index(r.APIVersion)
	if !ok {
		t.Fatalf("no index for %s", r.APIVersion)
	}
	stop := errors.New("stop")
	var found diag.Path
	err := x.Walk(string(r.ID.Kind), r.Root, func(c *schemaidx.Cursor) error {
		if c.Info.Node.Def() == schemaDocumentDef {
			return schemaidx.ErrSkipChildren
		}
		if c.Node.Kind != tree.KindMap {
			return nil
		}
		if _, ok := c.Node.Get(PatchKey); ok {
			found = c.Path().Append(diag.Field(PatchKey))
			return stop
		}
		return nil
	})
	if err != nil && !errors.Is(err, stop) {
		t.Fatalf("Walk %s: %v", r.ID, err)
	}
	return found, found != nil
}

// duplicateKeys reports an object with two members of one key anywhere
// in n.
func duplicateKeys(n *tree.Node) bool {
	seen := map[string]bool{}
	for _, m := range n.Members {
		if seen[m.Key] || duplicateKeys(m.Value) {
			return true
		}
		seen[m.Key] = true
	}
	for _, it := range n.Items {
		if duplicateKeys(it) {
			return true
		}
	}
	return false
}

func schemaKinds(t testing.TB) []string {
	x, _ := schemas(t).Index(v1)
	return x.Kinds()
}

func dumpAll(rs []*tree.Resource) string {
	var b strings.Builder
	for _, r := range rs {
		b.WriteString(r.ID.String() + " " + dump(r.Root) + "\n")
	}
	return b.String()
}

func cloneAll(rs []*tree.Resource) []*tree.Resource {
	out := make([]*tree.Resource, len(rs))
	for i, r := range rs {
		c := *r
		c.Root = r.Root.Clone()
		out[i] = &c
	}
	return out
}

// BenchmarkApply merges an overlay patching 100 of 1,000 Routes and adding
// 100 more (stage D share of the 01 req 54 load budget).
func BenchmarkApply(b *testing.B) {
	files := &tree.FileTable{}
	var baseSrc, overSrc []string
	for i := range 1000 {
		n := strconv.Itoa(i)
		baseSrc = append(baseSrc, `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r`+n+`"},"spec":{"timeout":"5s","match":{"path":{"prefix":"/`+n+`"},"methods":["GET"]},"upstreams":[{"name":"u","weight":1}],"policies":[{"name":"a"},{"name":"b"}]}}`)
		if i%10 == 0 {
			overSrc = append(overSrc,
				`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r`+n+`"},"spec":{"timeout":"9s","policies":[{"name":"c"},{"name":"a","$patch":"delete"}]}}`,
				`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"n`+n+`"},"spec":{"match":{"path":{"prefix":"/n"}}}}`)
		}
	}
	base := docs(b, files, tree.RoleBase, "ruralz.yaml", strings.Join(baseSrc, "\n---\n"))
	over := docs(b, files, tree.RoleOverlay, "overlays/prod/o.yaml", strings.Join(overSrc, "\n---\n"))
	opts := Options{Schemas: schemas(b), Files: files}
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		bs, ov := cloneAll(base), cloneAll(over)
		b.StartTimer()
		if _, ds, err := Apply(b.Context(), bs, ov, opts); err != nil || len(ds) != 0 {
			b.Fatal(err, texts(ds))
		}
	}
}
