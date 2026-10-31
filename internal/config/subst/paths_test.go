// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// warningPaths returns the paths of the RZ-CFG-013 warnings of ds, sorted.
func warningPaths(ds diag.List) []string {
	var out []string
	for _, d := range ds {
		if d.Code == CodeWarning {
			out = append(out, d.Path.String())
		}
	}
	slices.Sort(out)
	return out
}

// revealed returns the first of texts that an RZ-CFG-013 warning of ds
// shows in its text form or in its JSON form, or "".
func revealed(t *testing.T, ds diag.List, texts []string) string {
	t.Helper()
	var warnings diag.List
	for _, d := range ds {
		if d.Code == CodeWarning {
			warnings = append(warnings, d)
		}
	}
	var js bytes.Buffer
	if err := diag.WriteJSON(&js, warnings); err != nil {
		t.Fatal(err)
	}
	for _, s := range texts {
		if strings.Contains(js.String(), s) {
			return s
		}
		for _, d := range warnings {
			if strings.Contains(string(d.AppendText(nil)), s) {
				return s
			}
		}
	}
	return ""
}

// TestCredentialPaths covers credentials in the paths of RZ-CFG-013
// warnings, which name the detector, never the value, in the text and JSON
// forms alike: no warning shows the credential it detected, or one an
// enclosing element names, in its path. Each row is one member of the
// class, where the credential is a mapping key (in labels, in annotations,
// in free content), a string set element, a key or string of a composite
// set element, the key field of a keyed entry, or an enclosing key or
// keyed entry of the warned value; the last rows show the path keeps every
// element that shows no credential. want lists the warnings' paths,
// sorted. An error below a credential (the RZ-CFG-011 of a key holding ${)
// still names it: the path rule covers RZ-CFG-013 warnings only.
func TestCredentialPaths(t *testing.T) {
	const (
		aws      = "AKIAABCDEFGHIJKLMNOP"
		userinfo = "https://user:pass@example.com" //nolint:gosec // G101: a sample of the URL detector, not a credential.
		scheme   = "Bearer abcdefghijklmnopqrstuvwxyz"
	)
	route := func(md, spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":` + md + `,"spec":` + spec + `}`
	}
	policy := func(typ, config string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"` + typ + `","plugin":"g","config":` + config + `}}`
	}
	cases := []struct {
		name, src string
		vars      Map
		// creds are the texts no diagnostic may show.
		creds, want []string
	}{
		{
			name: "a mapping key in labels", src: route(`{"name":"r","labels":{"`+aws+`":"x"}}`, `{}`),
			creds: []string{aws}, want: []string{"metadata.labels"},
		},
		{
			name: "a mapping key in annotations", src: route(`{"name":"r","annotations":{"`+aws+`":"x"}}`, `{}`),
			creds: []string{aws}, want: []string{"metadata.annotations"},
		},
		{
			name: "a mapping key in free content", src: policy("plugin", `{"tenants":{"`+scheme+`":"a"}}`),
			creds: []string{scheme}, want: []string{"spec.config.tenants"},
		},
		{
			name: "a string set element", src: policy("cors", `{"allowOrigins":["https://a.example","`+userinfo+`"]}`),
			creds: []string{userinfo}, want: []string{"spec.config.allowOrigins[1]"},
		},
		{
			name: "a string set element holding an expression", src: policy("cors", `{"allowOrigins":["`+userinfo+`/${P}"]}`), vars: Map{"P": "x"},
			creds: []string{userinfo}, want: []string{"spec.config.allowOrigins[0]"},
		},
		{
			name:  "a composite set element",
			src:   policy("auth.mtls", `{"caCertificate":{"secretRef":{"provider":"file","name":"/ca"}},"subjects":[{"subject":"`+aws+`"}]}`),
			creds: []string{aws}, want: []string{"spec.config.subjects[0].subject"},
		},
		{
			name:  "a key of a composite set element",
			src:   policy("auth.mtls", `{"subjects":[{"`+aws+`":"x"}]}`),
			creds: []string{aws}, want: []string{"spec.config.subjects[0]"},
		},
		{
			name:  "a list in a composite set element",
			src:   policy("auth.mtls", `{"subjects":[{"x":["`+aws+`"]}]}`),
			creds: []string{aws}, want: []string{"spec.config.subjects[0].x[0]"},
		},
		{
			name: "a composite set element without a credential keeps its JSON",
			src:  policy("auth.mtls", `{"subjects":[{"subject":"${DB_PASSWORD}","n":1}]}`), vars: Map{"DB_PASSWORD": "x"},
			creds: []string{aws}, want: []string{`spec.config.subjects[item={"n":1,"subject":"${DB_PASSWORD}"}].subject`},
		},
		{
			name:  "a string element among other scalars is named by its index",
			src:   policy("plugin", `{"l":[1,true,null,"`+aws+`"]}`),
			creds: []string{aws}, want: []string{"spec.config.l[3]"},
		},
		{
			name: "a credential in a later authored run of a set element",
			src:  policy("cors", `{"allowOrigins":["x${P}`+aws+`"]}`), vars: Map{"P": "p"},
			creds: []string{aws}, want: []string{"spec.config.allowOrigins[0]"},
		},
		{
			name:  "the key field of a keyed entry",
			src:   `{"apiVersion":"ruralz/v1alpha1","kind":"Upstream","metadata":{"name":"u"},"spec":{"protocol":"http","endpoints":[{"address":"` + userinfo + `"}]}}`,
			creds: []string{userinfo}, want: []string{"spec.endpoints[0].address"},
		},
		{
			name:  "the name of a headers set entry",
			src:   policy("headers", `{"request":{"set":[{"name":"`+aws+`","value":"v"}]}}`),
			creds: []string{aws}, want: []string{"spec.config.request.set[0].name"},
		},
		{
			name:  "a JWT audiences element",
			src:   policy("auth.jwt", `{"issuers":[{"issuer":"a","jwksUrl":"https://a/k","audiences":["`+aws+`"]}]}`),
			creds: []string{aws}, want: []string{"spec.config.issuers[issuer=a].audiences[0]"},
		},
		{
			name:  "a credential key holding a credential value",
			src:   route(`{"name":"r","annotations":{"`+aws+`":"`+aws+`"}}`, `{}`),
			creds: []string{aws}, want: []string{"metadata.annotations", "metadata.annotations"},
		},
		{
			name:  "a value below a credential key",
			src:   policy("plugin", `{"`+scheme+`":{"x":"`+aws+`"}}`),
			creds: []string{scheme, aws}, want: []string{"spec.config", "spec.config"},
		},
		{
			name: "a secret-like name below a credential key",
			src:  route(`{"name":"r","annotations":{"`+aws+`":"${DB_PASSWORD}"}}`, `{}`), vars: Map{"DB_PASSWORD": "x"},
			creds: []string{aws}, want: []string{"metadata.annotations", "metadata.annotations"},
		},
		{
			name:  "a value in a keyed entry whose key is a credential",
			src:   route(`{"name":"r"}`, `{"match":{"headers":[{"name":"`+aws+`","exact":"`+aws+`"}]}}`),
			creds: []string{aws}, want: []string{"spec.match.headers[0].exact", "spec.match.headers[0].name"},
		},
		{
			name: "a key holding ${ shows a credential to the warnings below it",
			src:  policy("plugin", `{"`+aws+`${A}":{"x":"`+userinfo+`"}}`), vars: Map{"A": "a"},
			creds: []string{aws, userinfo}, want: []string{"spec.config"},
		},
		{
			name:  "an ordinary key keeps its name",
			src:   route(`{"name":"r","annotations":{"note":"`+aws+`"}}`, `{}`),
			creds: []string{aws}, want: []string{"metadata.annotations.note"},
		},
		{
			name:  "an ordinary keyed entry keeps its key",
			src:   policy("headers", `{"request":{"set":[{"name":"authorization","value":"hunter2"}]}}`),
			creds: []string{"hunter2"}, want: []string{"spec.config.request.set[name=authorization].value"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, tc.vars)
			if got := warningPaths(ds); !slices.Equal(got, tc.want) {
				t.Errorf("warning paths %q, want %q: %q", got, tc.want, texts(ds))
			}
			if s := revealed(t, ds, tc.creds); s != "" {
				t.Errorf("a diagnostic shows %q: %q", s, texts(ds))
			}
		})
	}
}

// TestDiagnosticsBounded covers unbounded diagnostic text: one long key or
// name repeated by every diagnostic below it would make their text grow
// with the content, up to 10,001 times over. Each row is one member of the
// class, a long text the walk names in a path element, a resource identity
// or a message, or a newline the one-line text form must not hold, and
// asserts that every diagnostic's text and JSON forms are bounded whatever
// the content's size, so the rows hold for 1 MiB texts.
func TestDiagnosticsBounded(t *testing.T) {
	long := strings.Repeat("k", 1<<20)
	undefined := strings.Repeat("${U}", 50)
	route := func(name, spec string) string {
		return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":` + quote(name) + `},"spec":` + spec + `}`
	}
	cases := []struct {
		name, src string
		// diags is the number of diagnostics the row makes.
		diags int
	}{
		{"a long mapping key", `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"` + long + `":"` + undefined + `"}},"spec":{}}`, 50},
		{"a long mapping key holding ${", `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","annotations":{"` + long + `${K}":"x"}},"spec":{}}`, 1},
		{"a long key deep in free content", `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","plugin":"g","config":{"a":{"` + long + `":{"b":"` + undefined + `"}}}}}`, 50},
		{"a long keyed entry key", route("r", `{"match":{"headers":[{"name":"`+long+`","exact":"`+undefined+`"}]}}`), 50},
		{"a long string set element", `{"apiVersion":"ruralz/v1alpha1","kind":"Consumer","metadata":{"name":"c"},"spec":{"tags":["` + long + undefined + `"]}}`, 50},
		{"a long composite set element", `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"auth.mtls","config":{"subjects":[{"subject":"` + long + undefined + `"}]}}}`, 50},
		{"a long resource name", route(long, `{"timeout":"`+undefined+`"}`), 50},
		{"a long kind", `{"apiVersion":"ruralz/v1alpha1","kind":"` + long + `","metadata":{"name":"r"},"spec":{}}`, 1},
		{"a long apiVersion", `{"apiVersion":"` + long + `","kind":"Route","metadata":{"name":"r"},"spec":{}}`, 1},
		{"a resource name holding a newline", route("a\nb", `{"timeout":"${U}"}`), 1},
		{"a kind holding a newline", `{"apiVersion":"ruralz/v1alpha1","kind":"Ro\nute","metadata":{"name":"r"},"spec":{}}`, 1},
		{"an apiVersion holding a newline", `{"apiVersion":"ruralz/v1\nalpha1","kind":"Route","metadata":{"name":"r"},"spec":{}}`, 1},
	}
	const perDiagnostic = 1024
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, nil)
			if len(ds) != tc.diags {
				t.Fatalf("%d diagnostics, want %d", len(ds), tc.diags)
			}
			for _, d := range ds {
				text := d.AppendText(nil)
				if len(text) > perDiagnostic || bytes.ContainsAny(text, "\r\n") {
					t.Fatalf("a text line of %d bytes, holding a newline %v, want one line of at most %d: %.300q",
						len(text), bytes.ContainsAny(text, "\r\n"), perDiagnostic, text)
				}
				if d.Resource != nil && (len(d.Resource.Kind) > maxShown+5 || len(d.Resource.Name) > maxShown+5) {
					t.Errorf("resource %.100s/%.100s: %d and %d bytes", d.Resource.Kind, d.Resource.Name, len(d.Resource.Kind), len(d.Resource.Name))
				}
			}
			var js bytes.Buffer
			if err := diag.WriteJSON(&js, ds); err != nil {
				t.Fatal(err)
			}
			if js.Len() > 2*perDiagnostic*len(ds) {
				t.Errorf("JSON form of %d bytes for %d diagnostics", js.Len(), len(ds))
			}
		})
	}
}

// TestUnserved covers the RZ-CFG-007 of a resource without schema, which
// stage C reports and drops first: an unserved apiVersion is reported at
// its value with path apiVersion, a kind the served apiVersion lacks at
// its value with path kind, each value quoted and clipped, as overlay
// Apply reports them; a resource without positions is reported at its
// start.
func TestUnserved(t *testing.T) {
	long := strings.Repeat("K", 300)
	for _, tc := range []struct {
		name, src, want string
	}{
		{
			"an unserved apiVersion", `{"apiVersion":"ruralz/v9","kind":"Route","metadata":{"name":"r"},"spec":{}}`,
			`r.yaml:1:15 error RZ-CFG-007 Route/r apiVersion: apiVersion "ruralz/v9" is not served`,
		},
		{
			"a kind the apiVersion does not serve", `{"apiVersion":"ruralz/v1alpha1","kind":"Rout","metadata":{"name":"r"},"spec":{}}`,
			`r.yaml:1:40 error RZ-CFG-007 Rout/r kind: kind "Rout" is not served by apiVersion "ruralz/v1alpha1"`,
		},
		{
			"a long kind is clipped", `{"apiVersion":"ruralz/v1alpha1","kind":"` + long + `","metadata":{"name":"r"},"spec":{}}`,
			`r.yaml:1:40 error RZ-CFG-007 ` + long[:maxShown] + `.../r kind: kind "` + long[:maxShown] + `..." is not served by apiVersion "ruralz/v1alpha1"`,
		},
		{
			"an apiVersion holding a newline is quoted", `{"apiVersion":"ruralz/v1\nalpha1","kind":"Route","metadata":{"name":"r"},"spec":{}}`,
			`r.yaml:1:15 error RZ-CFG-007 Route/r apiVersion: apiVersion "ruralz/v1\nalpha1" is not served`,
		},
		{
			"a kind holding a newline is quoted", `{"apiVersion":"ruralz/v1alpha1","kind":"Ro\nute","metadata":{"name":"r"},"spec":{}}`,
			`r.yaml:1:40 error RZ-CFG-007 "Ro\nute"/r kind: kind "Ro\nute" is not served by apiVersion "ruralz/v1alpha1"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ds := apply(t, tc.src, nil)
			if got := texts(ds); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
	t.Run("a resource without positions is reported at its start", func(t *testing.T) {
		files := &tree.FileTable{}
		file := files.Add(tree.File{Path: "r.yaml"})
		res := &tree.Resource{
			ID: tree.ID{Kind: "Rout", Name: "r"}, APIVersion: v1, Start: tree.Pos{File: file, Line: 3, Column: 1},
			Root: &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "kind", Value: &tree.Node{Kind: tree.KindString, Text: "Rout"}}}},
		}
		ds := New(Options{Schemas: schemas(t), Files: files}).Apply(res)
		want := `r.yaml:3:1 error RZ-CFG-007 Rout/r kind: kind "Rout" is not served by apiVersion "ruralz/v1alpha1"`
		if got := texts(ds); !slices.Equal(got, []string{want}) {
			t.Errorf("diagnostics = %q, want %q", got, want)
		}
	})
}

// TestMalformedUndefined covers one RZ-CFG-010 per occurrence in a
// malformed scalar, against CM "Validation and diff semantics" (every
// error in one run): a scalar holding a malformed ${ reports, besides its
// RZ-CFG-005, RZ-CFG-010 with its hint for each well-formed use of an
// undefined variable without default, read leniently as the secret-like
// name warning reads it, so the author learns of both errors in one run.
// A use with a default, a defined variable, an expression inside the
// malformed one and a forbidden position take none.
func TestMalformedUndefined(t *testing.T) {
	malformed := "r.yaml:1:106 error RZ-CFG-005 Route/r spec.match.path.prefix: malformed substitution (%s); write $${ for a literal ${"
	undefined := `r.yaml:1:106 error RZ-CFG-010 Route/r spec.match.path.prefix: variable "%s" is not defined and has no default`
	f := func(format, arg string) string { return strings.Replace(format, "%s", arg, 1) }
	for _, tc := range []struct {
		name, text string
		vars       Map
		hints      []string
		want       []string
	}{
		{
			name: "an undefined variable before a malformed ${", text: "${UNDEF} ${1X}",
			want: []string{f(malformed, reasonName), f(undefined, "UNDEF")},
		},
		{
			name: "an undefined variable after a malformed ${", text: "${1X} ${UNDEF}",
			want: []string{f(malformed, reasonName), f(undefined, "UNDEF")},
		},
		{
			name: "one diagnostic per occurrence", text: "${U} ${U} ${",
			want: []string{f(malformed, reasonUnterminated), f(undefined, "U"), f(undefined, "U")},
		},
		{
			name: "the hint names the nearest variable", text: "${UNDEFX} ${", hints: []string{"UNDEF"},
			want: []string{f(malformed, reasonUnterminated), f(undefined, "UNDEFX") + ` (did you mean "UNDEF"?)`},
		},
		{
			name: "the expression after a malformed default opening is a use", text: "${A:-${B}}",
			want: []string{f(malformed, reasonNested), f(undefined, "B")},
		},
		{
			name: "a default is taken", text: "${U:-d} ${",
			want: []string{f(malformed, reasonUnterminated)},
		},
		{
			name: "a defined variable", text: "${A} ${E} ${", vars: Map{"A": "a", "E": ""},
			want: []string{f(malformed, reasonUnterminated)},
		},
		{
			name: "a name inside a malformed expression is no use", text: "${UNDEF-d}",
			want: []string{f(malformed, reasonOperator)},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, ds := apply(t, prefixRoute(tc.text), tc.vars, tc.hints...)
			if got := texts(ds); !slices.Equal(got, tc.want) {
				t.Errorf("diagnostics =\n%q\nwant\n%q", got, tc.want)
			}
			if got := at(t, r.Root, "spec.match.path.prefix").Text; got != tc.text {
				t.Errorf("text %q, want the authored %q", got, tc.text)
			}
		})
	}
	t.Run("a forbidden position takes RZ-CFG-011 alone", func(t *testing.T) {
		_, ds := apply(t, `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":"'${U}' ${"}}}`, nil)
		if got := codes(ds); !slices.Equal(got, []string{CodeForbidden}) {
			t.Errorf("codes %v, want [%s]: %q", got, CodeForbidden, texts(ds))
		}
	})
}

// TestShown pins how a diagnostic shows a resource kind or name, so an
// identity never carries the whole input and a newline never breaks the
// one-line text form: clipped to maxShown bytes, Go-quoted when it holds
// a character that is not graphic, a quote, a backslash or invalid UTF-8,
// and "" quoted when it is empty, as config/canonical shows it.
func TestShown(t *testing.T) {
	long := strings.Repeat("k", 300)
	for _, tc := range []struct{ in, want string }{
		{"", `""`},
		{"Route", "Route"},
		{"é", "é"},
		{"a\nb", `"a\nb"`},
		{`a"b`, `"a\"b"`},
		{`a\b`, `"a\\b"`},
		{"a\x7fb", `"a\x7fb"`},
		{"a\u0085b", `"a\u0085b"`},
		{"a\u202eb", `"a\u202eb"`},
		{"a\xffb", `"a\xffb"`},
		{long, long[:maxShown] + "..."},
		{long[:100] + "\n", long[:maxShown] + "..."},
		{long[:maxShown-1] + "\n" + long, `"` + long[:maxShown-1] + `\n..."`},
	} {
		if got := shown(tc.in); got != tc.want {
			t.Errorf("shown(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestBoundElem pins the bounding of path elements, so up to 10,001
// diagnostics never each repeat one long key in full: a member name, a
// keyed entry's key and a string set element are clipped, other elements
// are kept, and a short text is copied, so a diagnostic never keeps the
// source it was read from alive (clip).
func TestBoundElem(t *testing.T) {
	long := strings.Repeat("k", 300)
	clipped := long[:maxShown] + "..."
	raw := json.RawMessage(`{"a":1}`)
	for _, tc := range []struct{ in, want diag.PathElem }{
		{diag.Field(long), diag.Field(clipped)},
		{diag.Field("name"), diag.Field("name")},
		{diag.Keyed("name", long), diag.Keyed("name", clipped)},
		{diag.Item(long), diag.Item(clipped)},
		{diag.Item(raw), diag.Item(raw)},
		{diag.Index(3), diag.Index(3)},
	} {
		got := boundElem(tc.in)
		if g, w := (diag.Path{got}).String(), (diag.Path{tc.want}).String(); g != w {
			t.Errorf("boundElem(%s) = %s, want %s", diag.Path{tc.in}, g, w)
		}
	}
	short := long[:10]
	var got string
	if a := testing.AllocsPerRun(10, func() { got = clip(short) }); a < 1 || got != short {
		t.Errorf("clip of a short text: %v allocations, %q; want a copy", a, got)
	}
}
