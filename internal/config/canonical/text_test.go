// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestClip: clip keeps at most 64 bytes, cut before a rune, and marks a
// cut, so a clipped message never holds a split UTF-8 sequence.
func TestClip(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"at the limit", strings.Repeat("a", 64), strings.Repeat("a", 64)},
		{"one past", strings.Repeat("a", 65), strings.Repeat("a", 64) + "..."},
		{"two-byte rune across the cut", "x" + strings.Repeat("é", 40), "x" + strings.Repeat("é", 31) + "..."},
		{"four-byte runes ending at the cut", strings.Repeat("😀", 20), strings.Repeat("😀", 16) + "..."},
		{"four-byte rune across the cut", "a" + strings.Repeat("😀", 20), "a" + strings.Repeat("😀", 15) + "..."},
		{"invalid bytes", strings.Repeat("\xff", 70), strings.Repeat("\xff", 64) + "..."},
		{"continuation bytes only", strings.Repeat("\x80", 70), "..."},
	} {
		got := clip(tc.in)
		if got != tc.want {
			t.Errorf("%s: clip = %q, want %q", tc.name, got, tc.want)
		}
		if utf8.ValidString(tc.in) && !utf8.ValidString(got) {
			t.Errorf("%s: clip split a rune: %q", tc.name, got)
		}
	}
}

// TestShown: a resource kind or name from the content is shown as it is
// when it needs no escape, else Go-quoted, and always clipped, so text
// output carries no control or format character.
func TestShown(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"plain", "orders-v2", "orders-v2"},
		{"graphic", "café ü", "café ü"},
		{"escape sequence", "a\x1b[2J", `"a\x1b[2J"`},
		{"newline", "a\nRZ-CFG", `"a\nRZ-CFG"`},
		{"tab", "a\tb", `"a\tb"`},
		{"quote", `a"b`, `"a\"b"`},
		{"backslash", `a\b`, `"a\\b"`},
		{"right-to-left override", "a\u202eb", `"a\u202eb"`},
		{"invalid UTF-8", "a\xffb", `"a\xffb"`},
		{"long", strings.Repeat("n", 100), strings.Repeat("n", 64) + "..."},
		{"control past the cut", strings.Repeat("n", 64) + "\x1b", strings.Repeat("n", 64) + "..."},
		{"long with a control", "\x1b" + strings.Repeat("n", 100), strconv.QuoteToGraphic("\x1b" + strings.Repeat("n", 63) + "...")},
		// WP-39 review: an empty kind or name printed as "/r".
		{"empty", "", `""`},
		{"C1 control", "\u009b31m", `"\u009b31m"`},
		{"delete", "a\x7fb", `"a\x7fb"`},
		{"zero-width space", "a\u200bb", `"a\u200bb"`},
		{"line separator", "a\u2028b", `"a\u2028b"`},
		{"no-break space", "a\u00a0b", "a\u00a0b"},
		{"replacement character", "a\ufffdb", "a\ufffdb"},
		{"space", "a b", "a b"},
	} {
		if got := shown(tc.in); got != tc.want {
			t.Errorf("%s: shown = %q, want %q", tc.name, got, tc.want)
		}
		// needsQuote decides as strconv.QuoteToGraphic escapes.
		if got, want := needsQuote(tc.in), strconv.QuoteToGraphic(tc.in) != `"`+tc.in+`"`; got != want {
			t.Errorf("%s: needsQuote = %v, want %v", tc.name, got, want)
		}
	}
	// Deciding that a name needs no escape allocates nothing beyond the
	// copy clip makes (WP-39 review: shown quoted every name to compare).
	var sink string
	for _, in := range []string{"orders-v2", "café ü", strings.Repeat("n", 100)} {
		if s, c := testing.AllocsPerRun(10, func() { sink = shown(in) }), testing.AllocsPerRun(10, func() { sink = clip(in) }); s != c || c != 1 {
			t.Errorf("shown(%q) allocates %.0f times, clip %.0f; want one copy each", in, s, c)
		}
	}
	_ = sink
	if got := idText(tree.ID{Kind: v1alpha1.KindRoute, Name: "a\x1b"}); got != `Route/"a\x1b"` {
		t.Errorf("idText = %q", got)
	}
	if got := idText(tree.ID{Kind: v1alpha1.KindRoute}); got != `Route/""` {
		t.Errorf("idText of an empty name = %q", got)
	}
}

// TestIdentityEscapedPathsCarried: an identity from the content is
// escaped in every finding, a blank kind or name shows as "", and the
// member names, keys and set elements of a path carry the content's
// text exactly (clipped), for diag to quote (WP-39 review: C1 controls
// and U+202E in one diagnostic line were escaped in the identity but not
// in the path). diag.Path.String leaves those runes as they are (WP-39
// contract request for internal/config/diag), so the text line is checked
// to hold no character that is not graphic outside the path, and in the
// path too once diag escapes them.
func TestIdentityEscapedPathsCarried(t *testing.T) {
	diagEscapes := !strings.ContainsRune(diag.Path{diag.Field("\u009b")}.String(), '\u009b')
	for _, bad := range []string{"\u009b31m", "a\u202eb"} {
		js := strconv.QuoteToASCII(bad)
		for _, tc := range []struct {
			name, content, res string
			path               diag.Path
		}{
			{"field name", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{` + js + `:1}}`), "Route/r", diag.Path{diag.Field("spec"), diag.Field(bad)}},
			{"keyed list key", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":` + js + `,"zz":1}]}}`), "Route/r", diag.Path{diag.Field("spec"), diag.Field("upstreams"), diag.Keyed("name", bad), diag.Field("zz")}},
			{
				"set element", doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"caCertificate":{"secretRef":{"key":"k","name":"n","provider":"env"}},"subjects":[{"subject":` + js + `,"zz":1}]},"type":"auth.mtls"}}`), "Policy/p",
				diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("subjects"), diag.Item(json.RawMessage(`{"subject":"` + bad + `","zz":1}`)), diag.Field("zz")},
			},
			{"resource name", doc(`{"kind":"Route","metadata":{"name":` + js + `},"spec":{"zz":1}}`), "Route/" + strconv.QuoteToGraphic(bad), diag.Path{diag.Field("spec"), diag.Field("zz")}},
			{"resource kind", doc(`{"kind":` + js + `,"metadata":{"name":"r"}}`), strconv.QuoteToGraphic(bad) + "/r", diag.Path{diag.Field("kind")}},
			{"empty kind", doc(`{"kind":"","metadata":{"name":"r"}}`), `""/r`, diag.Path{diag.Field("kind")}},
			{"empty name", doc(`{"kind":"Route","metadata":{"name":""},"spec":{"zz":1}}`), `Route/""`, diag.Path{diag.Field("spec"), diag.Field("zz")}},
		} {
			ds := DiagnosticsOf(decodeErr(t, tc.content))
			if len(ds) != 1 {
				t.Fatalf("%s %q: %v", tc.name, bad, ds)
			}
			d := ds[0]
			if resText(d) != tc.res || d.Path.String() != tc.path.String() {
				t.Errorf("%s %q: %s %s, want %s %s", tc.name, bad, resText(d), d.Path, tc.res, tc.path)
			}
			line := string(d.AppendText(nil))
			outside := strings.Replace(line, d.Path.String(), "", 1)
			for _, text := range []string{outside, line} {
				if text == line && !diagEscapes {
					continue
				}
				for _, r := range text {
					if !strconv.IsGraphic(r) {
						t.Errorf("%s %q: %q holds %U", tc.name, bad, text, r)
					}
				}
			}
		}
	}
}

// TestEncodeErrorBounded: an encoder failure names its path with every
// member name bounded, as the decoder's re-encoding findings do (WP-39
// review: a 1 MiB member name made a 2 MiB error message), through
// Encode, EncodeResources and AppendNode, at depth 1 and 2.
func TestEncodeErrorBounded(t *testing.T) {
	k := strings.Repeat("k", 1<<20)
	nest := func(depth int) string {
		return strings.Repeat(`{"`+k+`":`, depth) + `null` + strings.Repeat(`}`, depth)
	}
	enc := NewEncoder(hubIndex(t))
	for _, depth := range []int{1, 2} {
		root := jsonTree(t, `{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":`+nest(depth)+`}}`)
		_, encErr := enc.Encode(context.Background(), hub.NewBundle([]*hub.Resource{resourceOf(t, root)}), Options{})
		_, resErr := enc.EncodeResources(context.Background(), []tree.Resource{{ID: tree.ID{Kind: v1alpha1.KindPlugin, Name: "p"}, Root: root}}, Options{})
		_, nodeErr := AppendNode(nil, jsonTree(t, nest(depth)))
		// A member name at the resource root has no place there.
		top := jsonTree(t, `{"kind":"Plugin","metadata":{"name":"p"},"`+k+`":1}`)
		_, topErr := enc.EncodeResources(context.Background(), []tree.Resource{{ID: tree.ID{Kind: v1alpha1.KindPlugin, Name: "p"}, Root: top}}, Options{})
		for name, err := range map[string]error{"Encode": encErr, "EncodeResources": resErr, "AppendNode": nodeErr, "EncodeResources member": topErr} {
			if !errors.Is(err, ErrEncode) || len(err.Error()) > 512 || !strings.Contains(err.Error(), "...") {
				t.Errorf("%s, depth %d: %d-byte error %.200q", name, depth, len(err.Error()), err)
			}
		}
	}
}

// TestBoundElem: every content text of a path element is bounded; a set
// element too long to show is named by its index.
func TestBoundElem(t *testing.T) {
	long := strings.Repeat("k", 100)
	clipped := strings.Repeat("k", 64) + "..."
	longJSON := json.RawMessage(`{"a":"` + long + `"}`)
	for _, tc := range []struct {
		name string
		in   diag.PathElem
		want string
	}{
		{"short field", diag.Field("spec"), "spec"},
		{"long field", diag.Field(long), `["` + clipped + `"]`},
		{"short key", diag.Keyed("name", "a"), "[name=a]"},
		{"long key", diag.Keyed("name", long), "[name=" + clipped + "]"},
		{"short string item", diag.Item("GET"), "[item=GET]"},
		{"long string item", diag.Item(long), "[item=" + clipped + "]"},
		{"short object item", diag.Item(json.RawMessage(`{"a":1}`)), `[item={"a":1}]`},
		{"long object item", diag.Item(longJSON), "[7]"},
		{"long number item", diag.Item(json.Number(strings.Repeat("1", 100))), "[7]"},
		{"bool item", diag.Item(true), "[item=true]"},
		{"index", diag.Index(3), "[3]"},
	} {
		if got := (diag.Path{boundElem(tc.in, 7)}).String(); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.name, got, tc.want)
		}
	}
	p := boundPath(diag.Path{diag.Field("spec"), diag.Field(long), diag.Index(2), diag.Keyed("name", long)})
	if got := p.String(); got != `spec["`+clipped+`"][2][name=`+clipped+`]` {
		t.Errorf("boundPath = %s", got)
	}
}

// textSize returns the bytes the text and JSON forms of ds take.
func textSize(t *testing.T, ds diag.List) (text, js int) {
	t.Helper()
	var a, b bytes.Buffer
	if err := diag.WriteText(&a, ds); err != nil {
		t.Fatal(err)
	}
	if err := diag.WriteJSON(&b, ds); err != nil {
		t.Fatal(err)
	}
	return a.Len(), b.Len()
}

// TestBoundedFindingText: the diagnostics of a decode print in space
// linear in their number, however long the text from the content under
// them (WP-39 review: findings below a large name, key or set element
// printed it once each, so the output grew with the square of the
// input). Each case puts k unknown members under one long text: a set
// element (auth.mtls subjects), a keyed entry's key (a Gateway listener's
// name), a resource name, a typed-map key (synthetic schema); and one
// long text in each other place a finding quotes content: an unknown
// field, kind, resource member and document member, another format, and
// the path of a value the re-encoding refuses. Every diagnostic stays
// under 512 bytes of text and 1 KiB of JSON, at k = 500 and at k = 2,000.
func TestBoundedFindingText(t *testing.T) {
	big := strings.Repeat("n", 20_000)
	for _, tc := range []struct {
		name string
		idx  func(testing.TB) any
		gen  func(k int) string
	}{
		{"set element", nil, func(k int) string {
			return doc(`{"kind":"Policy","metadata":{"name":"p"},"spec":{"config":{"caCertificate":{"secretRef":{"key":"k","name":"n","provider":"env"}},"subjects":[{` + members(k, "x") + `,"subject":"CN=` + big + `"}]},"type":"auth.mtls"}}`)
		}},
		{"keyed entry key", nil, func(k int) string {
			return doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[{` + members(k, "x") + `,"name":"` + big + `"}]}}`)
		}},
		{"resource name", nil, func(k int) string {
			return doc(`{"kind":"Route","metadata":{"name":"` + big + `"},"spec":{` + members(k, "x") + `}}`)
		}},
		{"typed-map key", func(tb testing.TB) any { return syntheticIndex(tb) }, func(k int) string {
			return doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":{"byName":{"` + big + `":{` + members(k, "x") + `}}}}`)
		}},
		{"unknown member name", nil, func(int) string {
			return doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"` + big + `":1}}`)
		}},
		{"unknown kind", nil, func(int) string { return doc(`{"kind":"` + big + `","metadata":{"name":"` + big + `"}}`) }},
		{"unknown resource member", nil, func(int) string {
			return doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{},"` + big + `":1}`)
		}},
		{"document member", nil, func(int) string { return `{"` + big + `":1,"format":"ruralz.canonical.v1","resources":[]}` }},
		{"other format", nil, func(int) string { return `{"format":"` + big + `","resources":[]}` }},
		{"re-encoding path", nil, func(int) string {
			return doc(`{"kind":"Plugin","metadata":{"name":"` + big + `"},"spec":{"configSchema":{"` + big + `":1e400}}}`)
		}},
	} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{})
		if tc.idx != nil {
			dec = NewDecoder(syntheticIndex(t), DecoderOptions{})
		}
		for _, k := range []int{500, 2000} {
			_, err := dec.Decode(context.Background(), []byte(tc.gen(k)), Source{Name: "c.json"})
			ds := DiagnosticsOf(err)
			if c := codeOf(err); (c != CodeNewer && c != CodeMismatch) || len(ds) == 0 {
				t.Fatalf("%s, k %d: error = %v", tc.name, k, err)
			}
			text, js := textSize(t, ds)
			if text > 512*len(ds) || js > 1024*len(ds) {
				t.Errorf("%s, k %d: %d findings print %d bytes of text and %d of JSON", tc.name, k, len(ds), text, js)
			}
			for _, d := range ds {
				if n := len(d.AppendText(nil)); n > 512 {
					t.Errorf("%s, k %d: one diagnostic prints %d bytes: %.200s", tc.name, k, n, d.AppendText(nil))
					break
				}
			}
		}
	}
}

// TestContentTextEscaped: text from the content reaches no diagnostic
// unescaped (WP-39 review): a resource name or kind holding a control
// sequence is shown Go-quoted in the resource identity, in text output
// and in the error text, the revision of a dump is quoted in its message,
// and a clipped value keeps whole runes.
func TestContentTextEscaped(t *testing.T) {
	evil := "a\x1b[2J\nRZ-CFG-027 fake"
	quoted := strconv.QuoteToGraphic(evil)
	js := func(s string) string {
		b, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	for _, tc := range []struct {
		name, content, res string
	}{
		{"resource name", doc(`{"kind":"Route","metadata":{"name":` + js(evil) + `},"spec":{"zz":1}}`), "Route/" + quoted},
		{"unknown kind", doc(`{"kind":` + js(evil) + `,"metadata":{"name":"w"}}`), quoted + "/w"},
		{"form finding", doc(`{"apiVersion":"x","kind":"Route","metadata":{"name":` + js(evil) + `},"spec":{}}`), "Route/" + quoted},
		{"re-encoding", doc(`{"kind":"Route","metadata":{"name":` + js(evil) + `},"spec": {}}`), "Route/" + quoted},
	} {
		err := decodeErr(t, tc.content)
		ds := DiagnosticsOf(err)
		if len(ds) == 0 {
			t.Fatalf("%s: error = %v", tc.name, err)
		}
		if got := resText(ds[0]); got != tc.res {
			t.Errorf("%s: resource %q, want %q", tc.name, got, tc.res)
		}
		var out bytes.Buffer
		if err := diag.WriteText(&out, ds); err != nil {
			t.Fatal(err)
		}
		line := strings.TrimSuffix(out.String(), "\n")
		if strings.ContainsAny(line, "\x1b\n") || strings.ContainsAny(err.Error(), "\x1b\n") {
			t.Errorf("%s: raw control characters in %q or %q", tc.name, line, err.Error())
		}
	}
	// The revision of a dump document is quoted and clipped in its message.
	content := []byte(doc())
	d := revision.Sum(content)
	for _, rev := range []string{"\x1b[2J\x1b[31mrev-" + strings.Repeat("é", 40), "x\x1b[2J\nRZ fake"} {
		body := `{"content":` + string(content) + `,"digest":"` + d.String() + `","revision":` + js(rev) + `}`
		_, _, err := NewDecoder(hubIndex(t), DecoderOptions{}).ReadDump(context.Background(), strings.NewReader(body), 0, Source{Name: "dump.json"})
		ds := DiagnosticsOf(err)
		if codeOf(err) != CodeMismatch || len(ds) != 1 {
			t.Fatalf("revision %q: error = %v", rev, err)
		}
		msg := ds[0].Message
		if strings.ContainsAny(msg, "\x1b\n") || !utf8.ValidString(msg) || !strings.HasPrefix(msg, "revision "+strconv.Quote(clip(rev))+" does not name digest ") {
			t.Errorf("revision %q: message %q", rev, msg)
		}
		// The finding is located at the revision value.
		if want := strings.Index(body, `"revision":`) + len(`"revision":`) + 1; ds[0].Line != 1 || ds[0].Column != want || ds[0].Resource != nil || ds[0].Path.String() != "revision" {
			t.Errorf("revision %q: at %d:%d %v %s, want 1:%d", rev, ds[0].Line, ds[0].Column, ds[0].Resource, ds[0].Path, want)
		}
	}
	// A clipped format keeps whole runes.
	err := decodeErr(t, `{"format":"x`+strings.Repeat("é", 40)+`","resources":[]}`)
	if ds := DiagnosticsOf(err); len(ds) != 1 || !strings.Contains(ds[0].Message, `"x`+strings.Repeat("é", 31)+`..."`) {
		t.Errorf("clipped format: %v", ds)
	}
	// Encoder errors show identities the same way.
	_, err = NewEncoder(hubIndex(t)).EncodeResources(context.Background(), []tree.Resource{{ID: tree.ID{Kind: v1alpha1.KindRoute, Name: evil}, Root: jsonTree(t, `{"kind": "Route", "metadata": {"name": "x"}}`)}}, Options{})
	if !errors.Is(err, ErrEncode) || strings.ContainsAny(err.Error(), "\x1b\n") || !strings.Contains(err.Error(), "Route/"+quoted) {
		t.Errorf("encoder error = %q", err)
	}
}
