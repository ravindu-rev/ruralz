// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestBlockScalars covers block scalar values (YAML 1.2.2 section 8.1): the
// trailing white space of a line stays with every chomping, an empty scalar
// keeps no line break, lines of spaces up to the content indentation are
// empty lines and longer ones content, and a text that does not end with a
// line break reads as if it did, as the YAML Test Suite reads it (L24T/01,
// JEF9/02). PyYAML and the YAML Test Suite agree on every value.
func TestBlockScalars(t *testing.T) {
	run := strings.Repeat("\n", 5000)
	tests := []struct{ src, want string }{
		{"a: |-\n  x  \nb: 1\n", `{a:s:"x  ",b:i:1}`},
		{"a: >-\n  x  \nb: 1\n", `{a:s:"x  ",b:i:1}`},
		{"a: |\n  x  ", `{a:s:"x  \n"}`},
		{"a: |-\n : \n", `{a:s:": "}`},
		{"foo: |\n  x\n   ", `{foo:s:"x\n \n"}`},
		{"foo: |\n  x\n   \n", `{foo:s:"x\n \n"}`},
		{"a: |+\n", `{a:s:""}`},
		{"a: |-\n  x\n   \n", `{a:s:"x\n "}`},
		{"a: |-1\n  \n", `{a:s:" "}`},
		{"a: >-\n  a\n   \n \n", `{a:s:"a\n "}`},
		{"? >\n    \n \n:\n", `{:n}`},
		{"- |+\n   ", `[s:"\n"]`},
		{"a: |+\n  x\n\n\nb: 1\n", `{a:s:"x\n\n\n",b:i:1}`},
		{"a: |\n  x\n\n\nb: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: >\n  x\n  y\n\n  z\n   w\n  v\n", `{a:s:"x y\nz\n w\nv\n"}`},
		{"a: >\n \t\n detected\n", `{a:s:"\t\ndetected\n"}`},
		{"a: |2\n   x\n  y\n", `{a:s:" x\ny\n"}`},
		{"- |1\n  x\n", `[s:" x\n"]`},
		{"--- |\nroot\n# no comment\n...\n", `s:"root\n# no comment\n"`},
		{"a: |\n  x\n # c\nb: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str |\n  x  \n", `{a:s:"x  \n"}`},
		{"a: !!str  \t >-\n  x  \n", `{a:s:"x  "}`},
		{"a: |\n  x" + run + "b: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: |-\n  x  " + run + "b: 1\n", `{a:s:"x  ",b:i:1}`},
		{"a: |+\nb: 1\n", `{a:s:"",b:i:1}`},
		{"keep: |+\n\n# c\n", `{keep:s:"\n"}`},
		{"keep: |+\n\n\n# c\nb: 1\n", `{keep:s:"\n\n",b:i:1}`},
		{"keep: !!str >2+\n\nb: 1\n", `{keep:s:"\n",b:i:1}`},
		{"a: >2\n  x\nb: >2\n\n  y\n\n", `{a:s:"x\n",b:s:"\ny\n"}`},
		{"k:\n- |-2\n  y\n\n", `{k:[s:"y"]}`},
		// Tagged headers, explicit keys and sequence entries before a long
		// run of empty lines.
		{"a: !!str |\n  x" + run + "b: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str >\n  x" + run + "b: 1\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str  \t >\n  x" + run, `{a:s:"x\n"}`},
		{"- !!str |\n  x" + run + "- y\n", `[s:"x\n",s:"y"]`},
		{"? |\n  k" + run + ": v\n", "{k\n:s:\"v\"}"},
		{"? !!str |\n  k" + run + ": v\n", "{k\n:s:\"v\"}"},
		{"a: !!str |\n" + run + "b: 1\n", `{a:s:"",b:i:1}`},
		// Content of spaces only past the indentation, after empty lines.
		{"|1\n\n\n\n  ", `s:"\n\n\n  \n"`},
		{"a: >1\n\n\n    \n\n\nb: 1\n", `{a:s:"\n\n   \n",b:i:1}`},
		{"a: |\n\n\n      \n\n  x\n", "RZ-CFG-001@1:4"},
		// A line indented past the collection is content even when it
		// starts with '#' (YAML Test Suite S98Z), so the empty lines before
		// it may not hold more spaces.
		{"k: >\n # c\n", `{k:s:"# c\n"}`},
		{"k: |\n\n # c\n", `{k:s:"\n# c\n"}`},
		{"k: >\n \n  \n   \n # comment\n", "RZ-CFG-001@1:4"},
		// A leading empty line with more spaces than the first line of
		// content.
		{"a: |\n     \n  x\n", "RZ-CFG-001@1:4"},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src[:min(len(tt.src), 80)], got, tt.want)
		}
	}
	for _, src := range []string{"a: |\n     \n  x\n", "k: >\n \n  \n   \n # comment\n"} {
		if _, diags := parseYAML(t, src); len(diags) != 1 || diags[0].Message != msgBlockLeadingSpaces {
			t.Errorf("%q: %+v", src, diags)
		}
	}
}

// TestBlockScalarTabs covers tabs after a block scalar (YAML 1.2.2 rules
// 165-177; YAML Test Suite Y79Y/000, Y79Y/001): the empty lines that end it
// and the first comment after them are indented with spaces, the lines
// after that comment take any white space, and a line that breaks the rule
// is refused only when the document goes on after it, since before the next
// document or the end of the stream it is a comment of the stream
// (l-document-prefix).
func TestBlockScalarTabs(t *testing.T) {
	tests := []struct{ src, want string }{
		{"foo: |\n\t\nbar: 1\n", "RZ-CFG-001@2:1"},
		{"foo: |\n  x\n\t\nbar: 1\n", "RZ-CFG-001@3:1"},
		{"foo: |\n  x\n \t\nbar: 1\n", "RZ-CFG-001@3:2"},
		{"foo: |\n  x\n\n \t\nbar: 1\n", "RZ-CFG-001@4:2"},
		{"foo: |\n  x\n\t# c\nbar: 1\n", "RZ-CFG-001@3:1"},
		{"foo: |\n  x\n \t# c\nbar: 1\n", "RZ-CFG-001@3:2"},
		{"foo: |\n  x\n\t# c\n\n# d\nbar: 1\n", "RZ-CFG-001@3:1"},
		{"- |\n  x\n\t\n- y\n", "RZ-CFG-001@3:1"},
		{"a:\n  b: >\n    x\n\t\n  c: 1\n", "RZ-CFG-001@4:1"},
		// After a comment indented with spaces, any white space.
		{"foo: |\n  x\n# c\n\t\n\t# d\nbar: 1\n", `{foo:s:"x\n",bar:i:1}`},
		{"foo: |\n  x\n # c\n \t# d\nbar: 1\n", `{foo:s:"x\n",bar:i:1}`},
		// Before the next document or the end of the stream.
		{"foo: |\n  x\n\t\n", `{foo:s:"x\n"}`},
		{"foo: |\n  x\n\t# c\n", `{foo:s:"x\n"}`},
		{"foo: |\n  x\n\t# c\n---\nbar: 1\n", `{foo:s:"x\n"} {bar:i:1}`},
		{"foo: |\n  x\n\t\n...\n", `{foo:s:"x\n"}`},
		{"--- |\n  x\n\t\n--- y\n", `s:"x\n" s:"y"`},
		// A line of a space and a tab is content when it is the first.
		{"foo: |\n \t\nbar: 1\n", `{foo:s:"\t\n",bar:i:1}`},
		// Tabs inside the content, after its indentation.
		{"foo: |\n  x\n\n  \t\nbar: 1\n", `{foo:s:"x\n\n\t\n",bar:i:1}`},
		{"foo: |\n  \tx\n  y\t\n", `{foo:s:"\tx\ny\t\n"}`},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" {
			var trees []string
			for _, d := range docs {
				trees = append(trees, show(d.Root, false))
			}
			got = strings.Join(trees, " ")
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); strings.HasPrefix(tt.want, "RZ") && (len(diags) != 1 || diags[0].Message != msgBlockScalarTab) {
			t.Errorf("%q: %+v", tt.src, diags)
		}
	}
	for _, tt := range []struct {
		text string
		want int
	}{
		{"", -1},
		{"\n\n", -1},
		{"  \n\t\n", 3},
		{" # c\n\t\n", -1},
		{"\t# c\n", 0},
		{"b: 1\n\t\n", -1},
		{"  \t # c\n", 2},
	} {
		if got := chompedTab(tt.text, 0); got != tt.want {
			t.Errorf("chompedTab(%q) = %d, want %d", tt.text, got, tt.want)
		}
	}
}

// TestReadBlockScalar covers the block scalar reader on its own: the
// indentation of the collection around it, explicit indentation, the lines
// it holds, and document markers, which end it.
func TestReadBlockScalar(t *testing.T) {
	tests := []struct {
		text  string
		n     int
		value string
		lines int
	}{
		{"|\n x\n", -1, "x\n", 1},
		{"|\nx\n...\n", -1, "x\n", 1},
		{"|1\n x\n", -1, " x\n", 1},
		{"|2+\n   x\n\n", 0, " x\n\n", 2},
		{"|-\n  a\n\n  b\nc\n", 0, "a\n\nb", 3},
		{">\n  a\n  b\n\n\n  c\n", 1, "a b\n\nc\n", 5},
		{">+\n", 0, "", 0},
		{"|", 0, "", 0},
		{"| # c\n  x\n", 0, "x\n", 1},
		{"|\n  x\n---\n", 0, "x\n", 1},
		{"|\n  x\n--- \n", 0, "x\n", 1},
		{"|\n---x\n", -1, "---x\n", 1},
	}
	for _, tt := range tests {
		value, lines, msg := readBlockScalar(tt.text, 0, tt.n)
		if value != tt.value || lines != tt.lines || msg != "" {
			t.Errorf("readBlockScalar(%q, %d) = %q, %d, %q; want %q, %d", tt.text, tt.n, value, lines, msg, tt.value, tt.lines)
		}
	}
	for _, tt := range []struct {
		body   string
		n      int
		indent int
		ok     bool
	}{
		{"  x\n", 0, 2, true},
		{"\n\n  x\n", 0, 2, true},
		{"   \n  x\n", 0, 2, false},
		{"  \n  x\n", 0, 2, true},
		{" \n  \n   \n # c\n", 0, 0, false},
		{"\n # c\n", 0, 1, true},
		{"x\n", -1, 0, true},
		{"x\n", 1, 2, true},
		{"   \n", 0, 3, true},
		{"", 3, 4, true},
		{"  \n---\n    x\n", 0, 2, true},
	} {
		indent, ok := detectIndent(tt.body, tt.n)
		if ok != tt.ok || (ok && indent != tt.indent) {
			t.Errorf("detectIndent(%q, %d) = %d, %v; want %d, %v", tt.body, tt.n, indent, ok, tt.indent, tt.ok)
		}
	}
}

// TestBlockScalarHeaders covers what YAML 1.2 refuses in a block scalar's
// header (YAML 1.2.2 rule 162): two chomping or indentation indicators,
// text after them, a comment without white space before it, and a block
// scalar inside a flow collection.
func TestBlockScalarHeaders(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		{"a: [|\n x]\n", "RZ-CFG-001@1:5", msgBlockInFlow},
		{"a: {k: >\n x}\n", "RZ-CFG-001@1:8", msgBlockInFlow},
		{"k: |--\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |-- # c\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |+-\n  a\n\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: !!str >+-\n \n \n", "RZ-CFG-001@1:10", msgBlockHeaderRest},
		{"k: |22\n   a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |0\n   a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |1#c\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"k: |x\n  a\n", "RZ-CFG-001@1:4", msgBlockHeaderRest},
		{"? |++", "RZ-CFG-001@1:3", msgBlockHeaderRest},
		// A header on a later line at its entry's collection's column.
		{"k:\n|\n x\n", "RZ-CFG-001@2:1", msgBlockScalarPlace},
		{"?\n>\n a\n", "RZ-CFG-001@2:1", msgBlockScalarPlace},
		{"k:\n|\n", "RZ-CFG-001@2:1", msgBlockScalarPlace},
		{"k:\n |\n  x\n", `{k:s:"x\n"}`, ""},
		{"k: |2- # c\n  a\nl: >+1\n  b\nm: |\t# c\n  c\n", `{k:s:"a",l:s:" b\n",m:s:"c\n"}`, ""},
		// A profile finding and a syntax error later in its document.
		{"a: &x 1\nb: [|\n x]\nc: !e 1\n", "RZ-CFG-003@1:4 RZ-CFG-001@2:5", ""},
	}
	for _, tt := range tests {
		docs, got := parseWith(t, tt.src)
		if got == "" && len(docs) == 1 {
			got = show(docs[0].Root, false)
		}
		if got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		if _, diags := parseYAML(t, tt.src); tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
			t.Errorf("%q: messages %+v", tt.src, diags)
		}
	}
	valid := []string{"|", "|-", "|+2", "|2+", "| # c", "|\t#c"}
	invalid := []string{"|--", "|22", "|0", "|#", "|-x", "|2 x"}
	for _, text := range valid {
		if !headerValid(text, 0) {
			t.Errorf("headerValid(%q) = false", text)
		}
	}
	for _, text := range invalid {
		if headerValid(text, 0) {
			t.Errorf("headerValid(%q) = true", text)
		}
	}
}

// TestPlainScalarAfterBlockScalar covers a plain scalar over several lines
// that starts on the line ending a block scalar: its finding is at the line
// where it starts, after any number of empty lines.
func TestPlainScalarAfterBlockScalar(t *testing.T) {
	for _, blank := range []int{1, 7, 8, 12} {
		for _, head := range []string{"k: |\n x\n", "|\n 0\n", "k: >-\n  x\n  y\n"} {
			src := head + strings.Repeat("\n", blank) + "0\n 0\n"
			want := "RZ-CFG-001@" + itoa(strings.Count(head, "\n")+blank+1) + ":1"
			if _, got := parseWith(t, src); got != want {
				t.Errorf("%q: %s, want %s", src, got, want)
			}
		}
	}
}

// TestBlankLinesAfterBlockScalarAreLinear checks that empty lines after a
// block scalar cost linear work: twice the lines take about twice the
// allocations.
func TestBlankLinesAfterBlockScalarAreLinear(t *testing.T) {
	gen := func(n int) string {
		return "a: >\n  x\n" + strings.Repeat("  \n", n/2) + strings.Repeat("\n", n/2) + "b: |\n  y" + strings.Repeat("\n", n) + "c: 1\n"
	}
	measure := func(n int) uint64 {
		src := gen(n)
		var docs []Document
		var diags diag.List
		got := allocatedBy(func() { docs, diags = parseYAML(t, src) })
		if len(diags) != 0 || len(docs) != 1 {
			t.Fatal(codes(diags))
		}
		if a, _ := docs[0].Root.Get("a"); a.Text != "x\n" {
			t.Fatalf("a = %q", a.Text)
		}
		if b, _ := docs[0].Root.Get("b"); b.Text != "y\n" {
			t.Fatalf("b = %q", b.Text)
		}
		return got
	}
	a, b := measure(20000), measure(40000)
	if b > 3*a {
		t.Errorf("40,000 empty lines allocate %d bytes, 20,000 allocate %d: not linear", b, a)
	}
}

// checkLinear parses gen(n, m) for a block scalar of n bytes and m empty
// lines, and requires the work to stay within a small multiple of the
// input and to grow linearly with the scalar and the empty lines together.
// The scalar must parse to its content and one line break.
func checkLinear(t *testing.T, name string, gen func(n, m int) string) {
	t.Helper()
	measure := func(n, m int) uint64 {
		src := gen(n, m)
		var docs []Document
		var diags diag.List
		var err error
		got := allocatedBy(func() {
			docs, diags, err = parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, true)
		})
		if err != nil || len(diags) != 0 || len(docs) != 1 {
			t.Fatalf("%s: %v %q", name, err, codes(diags))
		}
		if v := firstScalar(docs[0].Root); !strings.HasSuffix(v, "x\n") {
			t.Fatalf("%s: value ends %q, want one line break", name, v[max(0, len(v)-8):])
		}
		if limit := uint64(16*len(src) + 1<<20); got > limit {
			t.Errorf("%s with %d bytes and %d empty lines allocates %d bytes, over %d", name, n, m, got, limit)
		}
		return got
	}
	// Eight times the scalar and the empty lines: about 8 times the work
	// when linear, 64 times when quadratic.
	a, b := measure(128<<10, 16000), measure(1<<20, 128000)
	t.Logf("%s: %d and %d bytes allocated, ratio %.1f", name, a, b, float64(b)/float64(a))
	if b > 24*a {
		t.Errorf("%s: eight times the scalar and the empty lines allocate %d bytes against %d: not linear", name, b, a)
	}
}

// firstScalar returns the text of the first entry of a mapping or list.
func firstScalar(n *tree.Node) string {
	switch {
	case n == nil:
		return ""
	case len(n.Items) > 0:
		return n.Items[0].Text
	case len(n.Members) > 0:
		return n.Members[0].Value.Text
	}
	return n.Text
}

// TestTaggedBlockScalarsAreLinear checks tagged block scalars and sequence
// entries before long runs of empty lines.
func TestTaggedBlockScalarsAreLinear(t *testing.T) {
	for _, tt := range []struct{ prefix, header string }{
		{"a: ", "!!str |"}, {"a: ", "!!str >"}, {"- ", "!!str |"}, {"a: ", "!!str   >2"},
	} {
		checkLinear(t, tt.prefix+tt.header, func(n, m int) string { return longTaggedScalar(tt.prefix, tt.header, n, m) })
	}
}

// TestBlockScalarBeforeCommentIsLinear checks block scalars that a comment
// ends after a long run of empty lines, whether the comment is at column 1,
// less indented than the scalar, followed by the end of the file or by more
// empty lines.
func TestBlockScalarBeforeCommentIsLinear(t *testing.T) {
	run := strings.Repeat("\n", 5000)
	for _, tt := range []struct{ name, prefix, header, tail string }{
		{"comment at column 1", "a: ", "|", "# c\nb: 1\n"},
		{"less indented comment", "a: ", "|", " # c\nb: 1\n"},
		{"comment then end of file", "a: ", ">", "# c\n"},
		{"comment then empty lines", "a: ", "|", "# c\n" + run + "b: 1\n"},
		{"sequence entry", "- ", "|", "# c\n- z\n"},
		{"tagged sequence entry", "- ", "!!str >", " # c\n- z\n"},
		{"tagged scalar", "a: ", "!!str |2", "# c\nb: 1\n"},
	} {
		checkLinear(t, tt.name, func(n, m int) string { return longBlockScalar(tt.prefix, tt.header, n, m, tt.tail) })
	}
}
