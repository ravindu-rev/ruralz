// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// encodeString encodes docs and fails the test on error.
func encodeString(t testing.TB, docs []*tree.Node, o EncodeOptions) string {
	t.Helper()
	var b bytes.Buffer
	if err := EncodeWith(&b, docs, o); err != nil {
		t.Fatalf("EncodeWith: %v", err)
	}
	return b.String()
}

// roots returns the roots of parsed documents.
func roots(docs []Document) []*tree.Node {
	out := make([]*tree.Node, len(docs))
	for i, d := range docs {
		out[i] = d.Root
	}
	return out
}

// TestEncodeGolden pins the restricted-profile output (01 req 31; 02 req
// 44-46): block style, two-space indent, "---" separators, a final
// newline, the quoting rule and "$${".
func TestEncodeGolden(t *testing.T) {
	src := "apiVersion: ruralz/v1alpha1\n" +
		"kind: Route\n" +
		"metadata:\n" +
		"  name: shop\n" +
		"  labels: {team: platform, ruralz.io/patch: x}\n" +
		"spec:\n" +
		"  hosts: [shop.example.com, \"*.example.com\"]\n" +
		"  port: 0777\n" +
		"  ratio: .5\n" +
		"  whole: 1.\n" +
		"  version: \"1.3\"\n" +
		"  flag: yes\n" +
		"  on: true\n" +
		"  nothing: ~\n" +
		"  empty: {}\n" +
		"  none: []\n" +
		"  text: |\n" +
		"    line one\n" +
		"    line \"two\"\n" +
		"  var: \"${HOST} and $${LITERAL}\"\n" +
		"  \"${key}\": 1\n" +
		"  $patch: delete\n" +
		"  nested:\n" +
		"  - - a\n" +
		"    - b\n" +
		"  - k: v\n" +
		"    l: [1]\n" +
		"  - {}\n" +
		"  - []\n" +
		"---\n" +
		"b: false\n"
	docs, diags := parseYAML(t, src)
	if len(diags) != 0 || len(docs) != 2 {
		t.Fatalf("diagnostics %q", codes(diags))
	}
	want := "apiVersion: ruralz/v1alpha1\n" +
		"kind: Route\n" +
		"metadata:\n" +
		"  name: shop\n" +
		"  labels:\n" +
		"    team: platform\n" +
		"    ruralz.io/patch: x\n" +
		"spec:\n" +
		"  hosts:\n" +
		"    - shop.example.com\n" +
		"    - \"*.example.com\"\n" +
		"  port: 777\n" +
		"  ratio: 0.5\n" +
		"  whole: 1.\n" +
		"  version: \"1.3\"\n" +
		"  flag: yes\n" +
		"  on: true\n" +
		"  nothing: null\n" +
		"  empty: {}\n" +
		"  none: []\n" +
		"  text: \"line one\\nline \\\"two\\\"\\n\"\n" +
		"  var: \"$${HOST} and $$${LITERAL}\"\n" +
		"  \"${key}\": 1\n" +
		"  \"$patch\": delete\n" +
		"  nested:\n" +
		"    - - a\n" +
		"      - b\n" +
		"    - k: v\n" +
		"      l:\n" +
		"        - 1\n" +
		"    - {}\n" +
		"    - []\n" +
		"---\n" +
		"b: false\n"
	var b bytes.Buffer
	if err := Encode(&b, roots(docs)); err != nil {
		t.Fatal(err)
	}
	if b.String() != want {
		t.Errorf("Encode:\n%s\nwant:\n%s", b.String(), want)
	}
	// Verbatim keeps "${" as it is.
	if got := encodeString(t, roots(docs), EncodeOptions{Verbatim: true}); !strings.Contains(got, "  var: \"${HOST} and $${LITERAL}\"\n") {
		t.Errorf("Verbatim:\n%s", got)
	}
	// The output parses back to the same values once "$${" is read as
	// "${", as substitution does.
	back, diags := parseYAML(t, b.String())
	if len(diags) != 0 {
		t.Fatal(codes(diags))
	}
	for i := range back {
		unescapeSubstitution(back[i].Root)
		if a, c := show(docs[i].Root, false), show(back[i].Root, false); a != c {
			t.Errorf("document %d:\n got %s\nwant %s", i, c, a)
		}
	}
}

// unescapeSubstitution reads every "$${" of string values as "${", which
// is all substitution does to a rendered tree without variables (01 req
// 26).
func unescapeSubstitution(n *tree.Node) {
	switch n.Kind {
	case tree.KindString:
		n.Text = strings.ReplaceAll(n.Text, "$${", "${")
	case tree.KindMap:
		for _, m := range n.Members {
			unescapeSubstitution(m.Value)
		}
	case tree.KindList:
		for _, it := range n.Items {
			unescapeSubstitution(it)
		}
	default:
	}
}

// TestEncodeQuoting covers the quoting rule of 02 req 45: plain only for
// ^[A-Za-z_/][A-Za-z0-9_./@-]*$ minus the null and boolean words, so every
// string reads back as the same string.
func TestEncodeQuoting(t *testing.T) {
	tests := []struct{ in, out string }{
		{"shop", "shop"},
		{"_x", "_x"},
		{"/path/to", "/path/to"},
		{"a.b-c_d@e/f", "a.b-c_d@e/f"},
		{"yes", "yes"},
		{"on", "on"},
		{"Off", "Off"},
		{"nil", "nil"},
		{"inf", "inf"},
		{"null", `"null"`},
		{"Null", `"Null"`},
		{"NULL", `"NULL"`},
		{"true", `"true"`},
		{"True", `"True"`},
		{"FALSE", `"FALSE"`},
		{"", `""`},
		{"1.3", `"1.3"`},
		{"0777", `"0777"`},
		{".5", `".5"`},
		{"-a", `"-a"`},
		{"~", `"~"`},
		{"a b", `"a b"`},
		{"a:b", `"a:b"`},
		{"a#b", `"a#b"`},
		{"*.x", `"*.x"`},
		{"é", `"é"`},
		{"---", `"---"`},
		{"a\"b\\c", `"a\"b\\c"`},
		{"t\tn\nr\rb\bf\f", `"t\tn\nr\rb\bf\f"`},
		{"\x00\x01\x1f\x7f", `"\u0000\u0001\u001f\u007f"`},
		{"\u0080\u0085\u009f", `"\u0080\u0085\u009f"`},
		{"\u2028\u2029\uFEFF\uFFFE\uFFFF", `"\u2028\u2029\ufeff\ufffe\uffff"`},
		{"\u00a0😀", "\"\u00a0😀\""},
		{"${X}", `"$${X}"`},
		{"$${X}", `"$$${X}"`},
		{"$", `"$"`},
	}
	for _, tt := range tests {
		n := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "k", Value: &tree.Node{Kind: tree.KindString, Text: tt.in}}}}
		got := encodeString(t, []*tree.Node{n}, EncodeOptions{})
		if want := "k: " + tt.out + "\n"; got != want {
			t.Errorf("Encode(%q) = %q, want %q", tt.in, got, want)
			continue
		}
		docs, diags := parseYAML(t, got)
		if len(diags) != 0 || len(docs) != 1 {
			t.Errorf("Parse(Encode(%q)): %q", tt.in, codes(diags))
			continue
		}
		v, _ := docs[0].Root.Get("k")
		unescapeSubstitution(v)
		if v.Kind != tree.KindString || v.Text != tt.in {
			t.Errorf("Parse(Encode(%q)) = %s", tt.in, show(v, false))
		}
	}
}

// TestEncodeScalars covers numbers, booleans, null and root scalars (02
// req 44-45; architecture R-64).
func TestEncodeScalars(t *testing.T) {
	docs := []*tree.Node{
		{Kind: tree.KindFloat, Text: "1"},
		{Kind: tree.KindFloat, Text: "-0"},
		{Kind: tree.KindFloat, Text: "1e400"},
		{Kind: tree.KindInt, Text: "-9223372036854775808"},
		{Kind: tree.KindBool, Bool: true},
		{Kind: tree.KindNull},
		nil,
		{Kind: tree.KindMap},
		{Kind: tree.KindList},
		{Kind: tree.KindList, Items: []*tree.Node{nil, {Kind: tree.KindString, Text: "x"}}},
	}
	want := "1.\n---\n-0.\n---\n1e400\n---\n-9223372036854775808\n---\ntrue\n---\nnull\n---\nnull\n---\n{}\n---\n[]\n---\n- null\n- x\n"
	if got := encodeString(t, docs, EncodeOptions{}); got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
	back, diags, err := parse(context.Background(), []byte(want), 0, "f", FormatYAML, Options{}, true)
	if err != nil || len(diags) != 0 {
		t.Fatal(err, codes(diags))
	}
	// Null documents are skipped on parse.
	if got := len(back); got != 8 {
		t.Fatalf("%d documents", got)
	}
	if back[0].Root.Kind != tree.KindFloat || back[0].Root.Text != "1" || back[1].Root.Text != "-0" {
		t.Errorf("floats read back as %s, %s", show(back[0].Root, false), show(back[1].Root, false))
	}
}

// TestEncodeRoundTrip checks Parse(Encode(x)) == x and that Encode is
// idempotent over every YAML Test Suite stream the profile accepts and
// this package's samples (01 test plan P2/P4; Done when: Encode(Parse(x))
// idempotent).
func TestEncodeRoundTrip(t *testing.T) {
	inputs := append(suiteInputs(t), nest("mixed", 64), "a:\n  - - - x\n    - {}\n  - k: [1, {b: [c]}]\n")
	n := 0
	for _, in := range inputs {
		docs, diags, err := parse(context.Background(), []byte(in), 0, "f", FormatYAML, Options{}, true)
		if err != nil || len(diags) != 0 || len(docs) == 0 {
			continue
		}
		n++
		if msg := roundTrip(t, roots(docs)); msg != "" {
			t.Errorf("%s\ninput:\n%s", msg, in)
		}
	}
	if n < 200 {
		t.Fatalf("round-tripped %d streams", n)
	}
}

// TestEncodeLongKeys covers keys longer than YAML 1.2's 1,024-character
// limit on implicit keys (02 req 44-45; finding 7 of the WP-33 review):
// they are written as explicit keys, in every position, and read back
// unchanged.
func TestEncodeLongKeys(t *testing.T) {
	limit := strings.Repeat("k", maxImplicitKey)
	long := limit + "k"
	quoted := strings.Repeat("\n", maxImplicitKey/2) // 1,026 characters once escaped and quoted
	str := func(s string) *tree.Node { return &tree.Node{Kind: tree.KindString, Text: s} }
	m := func(ms ...tree.Member) *tree.Node { return &tree.Node{Kind: tree.KindMap, Members: ms} }
	l := func(items ...*tree.Node) *tree.Node { return &tree.Node{Kind: tree.KindList, Items: items} }
	doc := m(
		tree.Member{Key: limit, Value: str("implicit")},
		tree.Member{Key: long, Value: str("explicit")},
		tree.Member{Key: quoted, Value: m(tree.Member{Key: long, Value: l(str("x"), m(tree.Member{Key: long, Value: str("y")}, tree.Member{Key: "z", Value: str("w")}))})},
		tree.Member{Key: "list", Value: l(m(tree.Member{Key: long, Value: m()}), l(m(tree.Member{Key: long, Value: l()})))},
		tree.Member{Key: long + "é", Value: &tree.Node{Kind: tree.KindNull}},
	)
	out := encodeString(t, []*tree.Node{doc}, EncodeOptions{})
	want := limit + ": implicit\n" +
		"? " + long + "\n: explicit\n" +
		"? \"" + strings.Repeat(`\n`, maxImplicitKey/2) + "\"\n:\n" +
		"  ? " + long + "\n  :\n    - x\n    - ? " + long + "\n      : y\n      z: w\n" +
		"list:\n  - ? " + long + "\n    : {}\n  - - ? " + long + "\n      : []\n" +
		"? \"" + long + "é\"\n: null\n"
	if out != want {
		t.Errorf("output:\n%s\nwant:\n%s", out, want)
	}
	for _, line := range strings.Split(out, "\n") {
		if key, _, ok := strings.Cut(line, ": "); ok && !strings.Contains(key, "?") && utf8.RuneCountInString(strings.TrimLeft(key, " -")) > maxImplicitKey {
			t.Errorf("implicit key of %d characters", utf8.RuneCountInString(key))
		}
	}
	if msg := roundTrip(t, []*tree.Node{doc}); msg != "" {
		t.Error(msg)
	}
}

// roundTrip returns a description of the first round-trip failure of
// docs, or "". The encoding is parsed back without the depth and token
// limits: it writes what a source may leave out (an empty entry is
// "- null"), so it can hold more tokens than the source it came from.
func roundTrip(t testing.TB, docs []*tree.Node) string {
	t.Helper()
	for _, o := range []EncodeOptions{{Verbatim: true}, {}} {
		first := encodeString(t, docs, o)
		back, diags, err := parse(context.Background(), []byte(first), 0, "f", FormatYAML, Options{MaxDepth: 1 << 10, MaxDocumentTokens: 1 << 30}, true)
		if err != nil || len(diags) != 0 {
			return "Parse(Encode(x)) failed: " + codes(diags) + "\noutput:\n" + first
		}
		var want []*tree.Node
		for _, d := range docs {
			if d != nil && d.Kind != tree.KindNull {
				want = append(want, d)
			}
		}
		if len(back) != len(want) {
			return "document count changed\noutput:\n" + first
		}
		for i := range back {
			if !o.Verbatim {
				unescapeSubstitution(back[i].Root)
			}
			if a, b := show(want[i], false), show(back[i].Root, false); a != b {
				return "values changed:\n got " + b + "\nwant " + a + "\noutput:\n" + first
			}
		}
		if o.Verbatim {
			if second := encodeString(t, roots(back), o); second != first {
				return "Encode not idempotent:\n" + first + "\nthen:\n" + second
			}
		}
	}
	return ""
}

// TestEncodeJSON covers the JSON form (02 req 47): two-space indent, tree
// order, no HTML escaping, final newline, "$${" unless Verbatim; it reads
// back through the JSON front end.
func TestEncodeJSON(t *testing.T) {
	docs, diags := parseYAML(t, "kind: Route\nspec:\n  expr: response.status >= 400 && x < 2\n  list: [1, 1.5, true, null, {}, []]\n  var: ${X}\n  ctl: \"\\x01\\t\\u2028\"\n")
	if len(diags) != 0 {
		t.Fatal(codes(diags))
	}
	list := &tree.Node{Kind: tree.KindList, Items: []*tree.Node{docs[0].Root}}
	var b bytes.Buffer
	if err := EncodeJSON(&b, list, EncodeOptions{}); err != nil {
		t.Fatal(err)
	}
	want := "[\n" +
		"  {\n" +
		"    \"kind\": \"Route\",\n" +
		"    \"spec\": {\n" +
		"      \"expr\": \"response.status >= 400 && x < 2\",\n" +
		"      \"list\": [\n" +
		"        1,\n" +
		"        1.5,\n" +
		"        true,\n" +
		"        null,\n" +
		"        {},\n" +
		"        []\n" +
		"      ],\n" +
		"      \"var\": \"$${X}\",\n" +
		"      \"ctl\": \"\\u0001\\t\u2028\"\n" +
		"    }\n" +
		"  }\n" +
		"]\n"
	if b.String() != want {
		t.Errorf("EncodeJSON:\n%s\nwant:\n%s", b.String(), want)
	}
	var one bytes.Buffer
	if err := EncodeJSON(&one, docs[0].Root, EncodeOptions{Verbatim: true}); err != nil {
		t.Fatal(err)
	}
	back, diags := parseJSON(t, one.String())
	if len(diags) != 0 || show(back[0].Root, false) != show(docs[0].Root, false) {
		t.Errorf("JSON round trip: %q\n%s", codes(diags), one.String())
	}
	if err := EncodeJSON(&one, nil, EncodeOptions{}); err != nil {
		t.Error(err)
	}
}

type failWriter struct{}

var errWrite = errors.New("disk full")

func (failWriter) Write([]byte) (int, error) { return 0, errWrite }

// TestEncodeErrors covers trees the encoders refuse and writer failures
// (02 req 44-47).
func TestEncodeErrors(t *testing.T) {
	bad := []*tree.Node{
		{Kind: tree.KindString, Text: "\xff"},
		{Kind: tree.KindMap, Members: []tree.Member{{Key: "\xff", Value: &tree.Node{}}}},
		{Kind: tree.Kind(99)},
	}
	for _, n := range bad {
		if err := Encode(&bytes.Buffer{}, []*tree.Node{n}); !errors.Is(err, ErrEncode) {
			t.Errorf("Encode(%+v) = %v", n, err)
		}
		if err := EncodeJSON(&bytes.Buffer{}, n, EncodeOptions{}); !errors.Is(err, ErrEncode) {
			t.Errorf("EncodeJSON(%+v) = %v", n, err)
		}
	}
	ok := &tree.Node{Kind: tree.KindBool}
	if err := Encode(failWriter{}, []*tree.Node{ok}); !errors.Is(err, errWrite) {
		t.Errorf("Encode to a failing writer = %v", err)
	}
	if err := EncodeJSON(failWriter{}, ok, EncodeOptions{}); !errors.Is(err, errWrite) {
		t.Errorf("EncodeJSON to a failing writer = %v", err)
	}
	if err := Encode(failWriter{}, nil); err != nil {
		t.Errorf("no documents: %v", err)
	}
}
