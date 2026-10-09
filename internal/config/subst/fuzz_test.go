// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// shape describes a tree's structure: object keys, list lengths and
// scalars as one class, so substitution may re-type a scalar but never
// change the shape.
func shape(n *tree.Node) string {
	var b strings.Builder
	var walk func(n *tree.Node)
	walk = func(n *tree.Node) {
		switch n.Kind {
		case tree.KindMap:
			b.WriteByte('{')
			for _, m := range n.Members {
				b.WriteString(strconv.Quote(m.Key))
				b.WriteByte(':')
				walk(m.Value)
				b.WriteByte(',')
			}
			b.WriteByte('}')
		case tree.KindList:
			b.WriteByte('[')
			for _, it := range n.Items {
				walk(it)
				b.WriteByte(',')
			}
			b.WriteByte(']')
		default:
			b.WriteByte('s')
		}
	}
	walk(n)
	return b.String()
}

// reference substitutes text by a regular-expression reading of the 01
// req 26 grammar, independent of scan: ok is false for malformed text,
// undefined counts the variables without value or default and exprs the
// expressions.
func reference(text string, vars Map) (out string, ok bool, undefined, exprs int) {
	re := regexp.MustCompile(`\$\$\{|\$\{([A-Za-z_][A-Za-z0-9_]*)(:-([^}]*))?\}|\$\{|[^$]+|\$`)
	var b strings.Builder
	for _, m := range re.FindAllStringSubmatch(text, -1) {
		switch {
		case m[0] == "$${":
			b.WriteString("${")
		case m[0] == "${" || (m[1] != "" && strings.Contains(m[3], "${")):
			return "", false, 0, 0
		case m[1] != "":
			exprs++
			v, set := vars[m[1]]
			switch {
			case m[2] != "" && (!set || v == ""):
				v = m[3]
			case !set:
				undefined++
			}
			b.WriteString(v)
		default:
			b.WriteString(m[0])
		}
	}
	return b.String(), true, undefined, exprs
}

// forbiddenSpots places text, a JSON-safe string, in three positions
// where substitution is forbidden (01 req 28): a CEL field, a reference
// field and a name inside an x-ruralz-secret subtree. Each entry is a
// document and the at path of the text.
func forbiddenSpots(text string) [][2]string {
	q := quote(text)
	return [][2]string{
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":` + q + `}}}`, "spec.match.when"},
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":` + q + `}]}}`, "spec.upstreams.0.name"},
		{`{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"stateStore":{"url":{"secretRef":{"provider":"env","name":` + q + `}}}}}`, "spec.stateStore.url.secretRef.name"},
	}
}

// FuzzSubst is property P3 of spec 01 (11 req 25 "substitution never
// injects structure"): for any resource and variable value (newlines, ": ",
// "- ", "{", "#", "${", "$${" included), substitution never changes the
// tree's shape, emits only registered codes and is deterministic; in a
// free string position the text equals an independent reading of the
// grammar, so values are inserted literally and never re-scanned; in a
// forbidden position (CEL, reference, x-ruralz-secret subtree) text
// holding an unescaped ${, an expression or a malformed one, is
// RZ-CFG-011 alone and stays as authored, and any other text becomes
// exactly its literal reading with no diagnostic but RZ-CFG-013, so no
// unescaped ${ is ever substituted there (01 req 28); and substituting
// Escape(text) yields text.
func FuzzSubst(f *testing.F) {
	for _, s := range [][3]string{
		{prefixRoute("x"), "a: b\n- c", "pre ${A} mid ${B:-d} post $${A}"},
		{`{"kind":"Policy","spec":{"type":"${T}","config":{"rule":"${A}","x":["${A}",{"${A}":1}]}}}`, "authz.cel", "${A}${A}"},
		{`{"kind":"Gateway","spec":{"listeners":[{"name":"${A}","port":"${A}","tls":{"certificates":[{"privateKey":{"secretRef":{"name":"${A}"}}}]}}]}}`, "8443", "${A:-${B}}"},
		{`{"kind":"Policy","spec":{"type":"validation.json-schema","config":{"schema":{"enum":["${A}","$${A}"]}}}}`, "${B} #{", "$$${A}"},
		{`{"metadata":{"labels":{"a":"${TOKEN}"}},"spec":{"match":{"when":"$${A}"}}}`, "", "${1X} ${X-d} ${X:=d} ${"},
		{`{"spec":{"upstreams":[{"name":"${A}"}],"timeout":"${A}${B}"}}`, "$${", "${A:-}${B}"},
		{prefixRoute("x"), "v", "$${A} AKIAABCDEFGHIJKLMNOP $"},
		{prefixRoute("x"), "v", "a $${ b"},
	} {
		f.Add(s[0], s[1], s[2])
	}
	f.Fuzz(func(t *testing.T, doc, value, text string) {
		if len(doc)+len(value)+len(text) > 8192 {
			return
		}
		set := schemas(t)
		vars := Map{"A": value, "T": value, "TOKEN": value}
		s := New(Options{Schemas: set, Variables: vars})
		if root, err := parseJSON(doc, 0); err == nil && root.Kind == tree.KindMap {
			kind := v1alpha1.KindRoute
			x, _ := set.Index(v1)
			if k, ok := root.Get("kind"); ok && k.Kind == tree.KindString && slices.Contains(x.Kinds(), k.Text) {
				kind = v1alpha1.Kind(k.Text)
			}
			before := shape(root)
			r1 := &tree.Resource{ID: tree.ID{Kind: kind, Name: "x"}, APIVersion: v1, Root: root.Clone()}
			r2 := &tree.Resource{ID: r1.ID, APIVersion: v1, Root: root.Clone()}
			ds1, ds2 := s.Apply(r1), s.Apply(r2)
			if after := shape(r1.Root); after != before {
				t.Fatalf("shape changed:\n%s\n%s", before, after)
			}
			if dump(r1.Root) != dump(r2.Root) || !slices.Equal(texts(ds1), texts(ds2)) {
				t.Fatal("not deterministic")
			}
			for _, d := range ds1 {
				if _, ok := errcode.Lookup(d.Code); !ok {
					t.Fatalf("unregistered code %q", d.Code)
				}
			}
		}
		if strings.ContainsAny(text, "\"\\\x00") || !utf8.ValidString(text) {
			return
		}
		files := &tree.FileTable{}
		rs, err := docsErr(files, tree.RoleBase, "r.yaml", prefixRoute(text))
		if err != nil || len(rs) != 1 {
			return
		}
		ds := s.Apply(rs[0])
		got := at(t, rs[0].Root, "spec.match.path.prefix")
		want, ok, undefined, exprs := reference(text, vars)
		errs := 0
		for _, d := range ds {
			if d.Code != CodeWarning {
				errs++
			}
		}
		switch {
		case !ok:
			if g := codes(ds); !slices.Contains(g, CodeMalformed) || errs != 1 || got.Text != text {
				t.Fatalf("malformed %q: %q, text %q", text, texts(ds), got.Text)
			}
		case undefined > 0:
			if errs != undefined || got.Text != text {
				t.Fatalf("%q: %d undefined, got %q", text, undefined, texts(ds))
			}
		default:
			if errs != 0 || got.Kind != tree.KindString || got.Text != want {
				t.Fatalf("%q with A=%q: got %q (%q), want %q", text, value, got.Text, texts(ds), want)
			}
		}
		for _, spot := range forbiddenSpots(text) {
			rs, err := docsErr(files, tree.RoleBase, "f.yaml", spot[0])
			if err != nil || len(rs) != 1 {
				t.Fatalf("%s: %v", spot[0], err)
			}
			ds := s.Apply(rs[0])
			got := at(t, rs[0].Root, spot[1])
			if !ok || exprs > 0 {
				if g := codes(ds); !slices.Equal(g, []string{CodeForbidden}) || got.Text != text {
					t.Fatalf("%s holding %q: %q, text %q; want RZ-CFG-011 alone and the authored text", spot[1], text, texts(ds), got.Text)
				}
				continue
			}
			for _, d := range ds {
				if d.Code != CodeWarning {
					t.Fatalf("%s holding %q: %s", spot[1], text, d.AppendText(nil))
				}
			}
			if got.Kind != tree.KindString || got.Text != want {
				t.Fatalf("%s holding %q = %q, want the literal %q", spot[1], text, got.Text, want)
			}
		}
		rs, err = docsErr(files, tree.RoleBase, "e.yaml", prefixRoute(Escape(text)))
		if err != nil || len(rs) != 1 {
			return
		}
		for _, d := range s.Apply(rs[0]) {
			if d.Code != CodeWarning {
				t.Fatalf("Escape(%q) = %q: %s", text, Escape(text), d.AppendText(nil))
			}
		}
		if g := at(t, rs[0].Root, "spec.match.path.prefix").Text; g != text {
			t.Fatalf("substituting Escape(%q) = %q", text, g)
		}
	})
}

// BenchmarkApply substitutes a Gateway-sized resource with expressions,
// escapes and plain strings (stage E share of the 01 req 54 budget).
func BenchmarkApply(b *testing.B) {
	var ls []string
	for i := range 50 {
		n := strconv.Itoa(i)
		ls = append(ls, `{"name":"l`+n+`","protocol":"http","port":"${PORT:-80`+n+`}","hostnames":["a`+n+`.example","$${x}"]}`)
	}
	src := `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g","labels":{"env":"${ENV}"}},"spec":{"listeners":[` +
		strings.Join(ls, ",") + `],"telemetry":{"traceSampling":"${RATE}"}}}`
	files := &tree.FileTable{}
	rs := docs(b, files, tree.RoleBase, "ruralz.yaml", src)
	s := New(Options{Schemas: schemas(b), Files: files, Variables: Map{"ENV": "prod", "RATE": "0.25"}})
	b.ReportAllocs()
	for range b.N {
		b.StopTimer()
		r := &tree.Resource{ID: rs[0].ID, APIVersion: v1, Root: rs[0].Root.Clone()}
		b.StartTimer()
		if ds := s.Apply(r); len(ds) != 0 {
			b.Fatal(texts(ds))
		}
	}
}
