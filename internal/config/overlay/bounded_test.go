// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// perDiagnostic bounds the text form of one diagnostic, whatever the
// size of the content it names.
const perDiagnostic = 1024

// checkBounded fails when a diagnostic of ds has a text line longer than
// perDiagnostic or holding a newline, a resource kind or name longer than
// a clipped and quoted text, or when the JSON form of ds is longer than
// twice perDiagnostic per diagnostic.
func checkBounded(t *testing.T, ds diag.List) {
	t.Helper()
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
}

// TestDiagnosticsBounded checks that diagnostic text stays bounded: one
// long key or name repeated by every diagnostic below it would otherwise
// make their text grow with the content, up to 10,001 times over. Each
// row is one member of the class, a long text the merge names in a path
// element (a member key of an overlay or a base document, a keyed entry's
// key), a resource identity (each code's diagnostics) or a message
// (RZ-CFG-008), or a newline the one-line text form must not hold, and
// asserts that every diagnostic's text and JSON forms are bounded
// whatever the content's size, so the rows hold for 1 MiB texts.
func TestDiagnosticsBounded(t *testing.T) {
	long := strings.Repeat("k", 1<<20)
	var patches []string
	for i := range 50 {
		patches = append(patches, `"a`+strconv.Itoa(i)+`":{"$patch":1}`)
	}
	misplaced := `{` + strings.Join(patches, ",") + `}`
	doc := func(apiVersion, kind, name, spec string) string {
		return `{"apiVersion":` + strconv.Quote(apiVersion) + `,"kind":` + strconv.Quote(kind) + `,"metadata":{"name":` + strconv.Quote(name) + `},"spec":` + spec + `}`
	}
	plugin := func(config string) string { return `{"type":"plugin","plugin":"g","config":` + config + `}` }
	cases := []struct {
		name       string
		base, over string
		versions   []string
		// diags is the number of diagnostics the row makes.
		diags int
	}{
		{
			name: "a long member key in an overlay", base: doc(v1, "Policy", "p", plugin(`{}`)),
			over: doc(v1, "Policy", "p", plugin(`{"`+long+`":`+misplaced+`}`)), diags: 50,
		},
		{
			name: "a long member key in a base document", base: doc(v1, "Policy", "p", plugin(`{"`+long+`":`+misplaced+`}`)), diags: 50,
		},
		{
			name: "a long key below a long key", base: doc(v1, "Policy", "p", plugin(`{}`)),
			over: doc(v1, "Policy", "p", plugin(`{"`+long+`":{"`+long+`":`+misplaced+`}}`)), diags: 50,
		},
		{
			name: "a long keyed entry key", base: doc(v1, "Route", "r", `{}`),
			over: doc(v1, "Route", "r", `{"match":{"headers":[{"name":"`+long+`","x":`+misplaced+`}]}}`), diags: 50,
		},
		{
			name: "a long resource name", base: doc(v1, "Route", long, `{}`),
			over: doc(v1, "Route", long, `{"x":`+misplaced+`}`), diags: 50,
		},
		{
			name: "a long name declared twice in the overlay",
			over: doc(v1, "Route", long, `{}`) + "\n---\n" + doc(v1, "Route", long, `{}`), diags: 1,
		},
		{name: "a long name under an unserved apiVersion", over: doc("ruralz/v9", "Route", long, `{}`), diags: 1},
		{name: "a long kind", over: doc(v1, long, "r", `{}`), diags: 1},
		{
			name: "a long name patched under another apiVersion", base: doc(v1, "Route", long, `{}`),
			over: doc("ruralz/v1alpha2", "Route", long, `{}`), versions: []string{"ruralz/v1alpha2"}, diags: 1,
		},
		{name: "a resource name holding a newline", over: doc(v1, "Route", "a\nb", `{"$patch":1}`), diags: 1},
		{name: "a kind holding a newline", over: doc(v1, "Ro\nute", "r", `{}`), diags: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var base, over []file
			if tc.base != "" {
				base = []file{{"b.yaml", tc.base}}
			}
			if tc.over != "" {
				over = []file{{"o.yaml", tc.over}}
			}
			_, ds, _ := run(t, base, over, tc.versions...)
			if len(ds) != tc.diags {
				t.Fatalf("%d diagnostics, want %d", len(ds), tc.diags)
			}
			checkBounded(t, ds)
		})
	}
}

// TestDiagnosticsBoundedTexts pins the forms the bounded texts take: a
// clipped member name, keyed entry key and resource name end in "..." (a
// clipped member name is then written ["..."]), a name or kind that is
// not graphic is Go-quoted in the identity and in the RZ-CFG-008
// message, and a kind the served apiVersion lacks is reported at the
// kind value with path kind.
func TestDiagnosticsBoundedTexts(t *testing.T) {
	long := strings.Repeat("k", 300)
	clipped := long[:maxShown] + "..."
	for _, tc := range []struct {
		name, base, over string
		want             []string
	}{
		{
			name: "a clipped member name",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"type":"plugin","config":{}}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"` + long + `":{"$patch":1}}}}`,
			want: []string{`o.yaml:1:395 error RZ-CFG-005 Policy/p spec.config["` + clipped + `"].$patch: $patch is not valid in an object (content without a schema merges as data, objects member by member and lists replaced whole; remove $patch)`},
		},
		{
			name: "a clipped keyed entry key",
			base: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{}}`,
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"headers":[{"name":"` + long + `","x":{"$patch":1}}]}}}`,
			want: []string{`o.yaml:1:416 error RZ-CFG-005 Route/r spec.match.headers[name=` + clipped + `].x.$patch: $patch is not valid in an object (content without a schema merges as data, objects member by member and lists replaced whole; remove $patch)`},
		},
		{
			name: "a name holding a newline declared twice",
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"a\nb"},"spec":{}}` + "\n---\n" +
				`{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"a\nb"},"spec":{}}`,
			want: []string{`o.yaml:3:67 error RZ-CFG-008 Route/"a\nb" metadata.name: Route/"a\nb" is declared twice in the overlay; results never depend on file order (first declared in o.yaml:1:67)`},
		},
		{
			name: "a kind the apiVersion does not serve is reported at the kind",
			over: `{"apiVersion":"ruralz/v1alpha1","kind":"Rout","metadata":{"name":"r"}}`,
			want: []string{`o.yaml:1:40 error RZ-CFG-007 Rout/r kind: kind "Rout" is not served by apiVersion "ruralz/v1alpha1"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var base []file
			if tc.base != "" {
				base = []file{{"b.yaml", tc.base}}
			}
			_, ds, _ := run(t, base, []file{{"o.yaml", tc.over}})
			if got := texts(ds); strings.Join(got, "\n") != strings.Join(tc.want, "\n") {
				t.Errorf("diagnostics =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestAbsentBounded covers the RZ-CFG-009 of Selection.Absent, a member
// of the bounded diagnostic text class (TestDiagnosticsBounded): the
// overlay name its message quotes and the Environment name of its
// identity are bounded, and a name that is not graphic is quoted.
func TestAbsentBounded(t *testing.T) {
	long := strings.Repeat("e", 1<<20)
	for name, s := range map[string]Selection{
		"a long overlay and Environment name":   {Name: long, Explicit: true, Environment: long},
		"an Environment name holding a newline": {Name: "prod", Explicit: true, Environment: "a\nb"},
	} {
		t.Run(name, func(t *testing.T) {
			ds := s.Absent([]string{"prod", long})
			if len(ds) != 1 {
				t.Fatalf("%d diagnostics", len(ds))
			}
			checkBounded(t, ds)
		})
	}
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
