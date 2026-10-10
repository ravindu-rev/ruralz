// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestKeepChompingMatchesGoccy parses every YAML Test Suite stream, and
// variants of each with long blank runs, with the keep-chomping rewrite
// and with it turned off (goccy's own clipping), and requires the same
// trees, positions and findings (01 req 12; 11 req 26): the rewrite
// changes neither goccy's tokens nor a value, for tagged scalars and
// explicit keys too.
func TestKeepChompingMatchesGoccy(t *testing.T) {
	inputs := append(suiteInputs(t), chompSamples()...)
	// Variants with long blank runs wherever a blank line or the end is,
	// so the rewrite applies, and with lines of spaces in those runs,
	// which can be content of a block scalar.
	spaced := "\n\n   \n" + strings.Repeat("\n", longBlankRun) + "      \n \n"
	run := strings.Repeat("\n", longBlankRun+1)
	for _, in := range inputs[:len(inputs):len(inputs)] {
		inputs = append(inputs,
			strings.ReplaceAll(in, "\n\n", strings.Repeat("\n", 2+longBlankRun)),
			in+run,
			strings.ReplaceAll(in, "\n\n", spaced),
			in+spaced+"  ",
			// A comment after the run ends a block scalar, where the
			// rewrite must still apply (finding 1 of the second WP-33
			// review).
			in+run+"# c\n",
			in+run+" # c\n"+run,
		)
	}
	same, rewritten := 0, 0
	for _, in := range inputs {
		if _, n := keepChomping(in); n > 0 {
			rewritten++
		}
		want, wd, err := parse(context.Background(), []byte(in), 0, "f.yaml", FormatYAML, Options{noRewrite: true}, true)
		if err != nil {
			t.Fatal(err)
		}
		got, gd, err := parse(context.Background(), []byte(in), 0, "f.yaml", FormatYAML, Options{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if codes(wd) != codes(gd) {
			t.Errorf("findings differ: %q, rewritten %q\ninput:\n%s", codes(wd), codes(gd), in)
			continue
		}
		if len(want) != len(got) {
			t.Errorf("%d documents, rewritten %d\ninput:\n%s", len(want), len(got), in)
			continue
		}
		for i := range want {
			if a, b := show(want[i].Root, true), show(got[i].Root, true); a != b {
				t.Errorf("document %d:\n  clipped %s\nrewritten %s\ninput:\n%s", i, a, b, in)
			}
		}
		same++
	}
	if same < 1000 || rewritten < 50 {
		t.Fatalf("compared %d streams, %d of them rewritten", same, rewritten)
	}
}

// chompSamples are block scalars the keep-chomping rewrite applies to
// once their blank runs are long: tagged headers, which goccy reports at
// the blank before the indicator, sequence entries, explicit and tagged
// keys, comments and indentation indicators.
func chompSamples() []string {
	return []string{
		"a: |\n  x\n\n\n\n\nb: >\n  y\n  z\n\n\n\nc: |2\n   w\n\n\nd: |-\n  s\n\n\ne: |+\n  k\n\n\n",
		"a: |\n\n\n\n",
		"a: >\n  t  \n\n\n\n",
		"- |\n  x\n\n\n\n- |\n  y",
		"--- |\n  root\n\n\n\n",
		"a: |  # comment\n  x\n\n\n\n\n",
		"a: !!str |\n  x\n\n\nb: 1\n",
		"a: !!str >\n  x\n  y\n\n\n",
		"a: !!str   |2  # c\n   x\n\n\nb: !!str\t>\n  y\n\n\n",
		"- !!str |\n  x\n\n\n- !!str >\n  y\n\n\n",
		"--- !!str |\n  root\n\n\n",
		"? |\n  k\n\n\n: v\n",
		"? !!str |\n  k\n\n\n: v\n",
		"? >\n  k\n\n\n: |\n  v\n\n\n",
		"a:\n  ? !!str >\n    k\n\n\n  : v\n",
		// Comments after the blank run, which end the scalar.
		"a: |\n  x\n\n\n# c\nb: 1\n",
		"a: >\n  x\n\n\n # c\n\n\nb: 1\n",
		"- !!str |\n  x\n\n\n# c\n- y\n",
		"a: |\n  x\n\n\n  # c\n\n\n",
		"a: |\n\n\n# c\nb: 1\n",
		"a:\n  b: |\n    x\n\n\n # c\n  c: 1\n",
		// A plain scalar after a block scalar and a long blank run, whose
		// start the rewrite moved (finding of the eighth WP-33 review).
		"|\n 0\n\n\n\n\n\n\n\n\n0\n 0",
		"k: |\n x\n\n\n\n\n\n\n\n\n0\n 0\n",
	}
}

// TestPlainScalarAfterBlockScalar covers a plain scalar over several lines
// that starts on the line ending a block scalar (01 req 12, 14; finding of
// the eighth WP-33 review): the token pass found its start from goccy's
// value of the block scalar's content, which is not the source text and
// which the keep-chomping rewrite lengthens, so the finding at the plain
// scalar was reported at 2:12 with the rewrite and 10:1 without, where the
// scalar starts at 11:1. It starts at the line that ends the block scalar
// now, with and without the rewrite.
func TestPlainScalarAfterBlockScalar(t *testing.T) {
	for _, blank := range []int{1, 7, 8, 12} {
		for _, head := range []string{"k: |\n x\n", "|\n 0\n", "k: >-\n  x\n  y\n"} {
			src := head + strings.Repeat("\n", blank) + "0\n 0\n"
			want := "RZ-CFG-001@" + itoa(strings.Count(head, "\n")+blank+1) + ":1"
			for _, o := range []Options{{}, {noRewrite: true}} {
				_, diags, err := parse(t.Context(), []byte(src), 0, "f.yaml", FormatYAML, o, true)
				if err != nil {
					t.Fatal(err)
				}
				if got := codes(diags); got != want {
					t.Errorf("%q, %+v: %s, want %s", src, o, got, want)
				}
			}
		}
	}
}

// TestKeepChompingTaggedHeaders checks that the rewrite finds the header
// of a tagged block scalar, which goccy reports one column early, and
// that the value is the clipped one (01 req 12; findings 2 and 4 of the
// WP-33 review).
func TestKeepChompingTaggedHeaders(t *testing.T) {
	run := strings.Repeat("\n", 2*longBlankRun)
	tests := []struct {
		src, rewritten, want string
	}{
		{"a: !!str |\n  x" + run + "b: 1\n", "a: !!str |+\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str >\n  x" + run + "b: 1\n", "a: !!str >+\n", `{a:s:"x\n",b:i:1}`},
		{"a: !!str  \t >\n  x" + run, "a: !!str  \t >+\n", `{a:s:"x\n"}`},
		{"- !!str |\n  x" + run + "- y\n", "- !!str |+\n", `[s:"x\n",s:"y"]`},
		{"? |\n  k" + run + ": v\n", "? |+\n", "{k\n:s:\"v\"}"},
		{"? !!str |\n  k" + run + ": v\n", "? !!str |+\n", "{k\n:s:\"v\"}"},
		{"a: !!str |\n" + run + "b: 1\n", "a: !!str |-\n", `{a:s:"",b:i:1}`},
	}
	for _, tt := range tests {
		text, _ := keepChomping(tt.src)
		if !strings.HasPrefix(text, tt.rewritten) {
			t.Errorf("%q rewritten to %q, want prefix %q", tt.src, text, tt.rewritten)
		}
		docs, diags, err := parse(context.Background(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{}, true)
		if err != nil || len(diags) != 0 || len(docs) != 1 {
			t.Fatalf("%q: %v %q", tt.src, err, codes(diags))
		}
		if got := show(docs[0].Root, false); got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
	}
	if headerIndex("a: |", 4) != 3 || headerIndex("a: x |", 4) != -1 || headerIndex("a:", 4) != -1 {
		t.Error("headerIndex")
	}
}

// TestBlankLinesAfterBlockScalarAreLinear checks that blank lines after a
// block scalar cost linear work (11 req 26): twice the lines take about
// twice the allocations, where goccy's clipping takes four times.
func TestBlankLinesAfterBlockScalarAreLinear(t *testing.T) {
	gen := func(n int) string {
		return "a: >\n  x\n" + strings.Repeat("  \n", n/2) + strings.Repeat("\n", n/2) + "b: |\n  y" + strings.Repeat("\n", n) + "c: 1\n"
	}
	measure := func(n int) uint64 {
		src := gen(n)
		before := allocBytes()
		docs, diags := parseYAML(t, src)
		if len(diags) != 0 || len(docs) != 1 {
			t.Fatal(codes(diags))
		}
		if a, _ := docs[0].Root.Get("a"); a.Text != "x\n" {
			t.Fatalf("a = %q", a.Text)
		}
		if b, _ := docs[0].Root.Get("b"); b.Text != "y\n" {
			t.Fatalf("b = %q", b.Text)
		}
		return allocBytes() - before
	}
	a, b := measure(20000), measure(40000)
	if b > 3*a {
		t.Errorf("40,000 blank lines allocate %d bytes, 20,000 allocate %d: not linear", b, a)
	}
}

// longBlockScalar is a block scalar of n bytes of content followed by m
// blank lines, under the given header and line prefix ("a: " or "- "),
// then tail: the shape whose clipping costs goccy O(m·L) (01 test plan
// FuzzProfileYAML, 2 s per input).
func longBlockScalar(prefix, header string, n, m int, tail string) string {
	line := strings.Repeat("x", 63)
	var b strings.Builder
	b.WriteString(prefix + header + "\n")
	for b.Len() < n {
		b.WriteString("  " + line + "\n")
	}
	b.WriteString(strings.Repeat("\n", m))
	b.WriteString(tail)
	return b.String()
}

// longTaggedScalar is longBlockScalar followed by one more entry.
func longTaggedScalar(prefix, header string, n, m int) string {
	if prefix == "- " {
		return longBlockScalar(prefix, header, n, m, "- z\n")
	}
	return longBlockScalar(prefix, header, n, m, "b: z\n")
}

// checkLinear parses gen(n, m) for a block scalar of n bytes and m blank
// lines, and requires the work to stay within a small multiple of the
// input and to grow linearly with the scalar and the blank run together,
// where goccy's own clipping costs O(m·L). The scalar must parse to its
// content and one line break.
func checkLinear(t *testing.T, name string, gen func(n, m int) string) {
	t.Helper()
	measure := func(n, m int) uint64 {
		src := gen(n, m)
		least := uint64(0)
		for i := range 3 {
			// Two collections empty the sync.Pool of goccy's scanner
			// contexts, whose reuse of a large buffer otherwise makes a
			// parse allocate one of two amounts.
			runtime.GC()
			runtime.GC()
			before := allocBytes()
			docs, diags, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, true)
			got := allocBytes() - before
			if err != nil || len(diags) != 0 || len(docs) != 1 {
				t.Fatalf("%s: %v %q", name, err, codes(diags))
			}
			if v := firstScalar(docs[0].Root); !strings.HasSuffix(v, "x\n") {
				t.Fatalf("%s: value ends %q, want one line break", name, v[max(0, len(v)-8):])
			}
			if i == 0 || got < least {
				least = got
			}
		}
		if limit := uint64(256*len(src) + 8<<20); least > limit {
			t.Errorf("%s with %d bytes and %d blank lines allocates %d bytes, over %d", name, n, m, least, limit)
		}
		return least
	}
	// Eight times the scalar and the blank run: about 8 times the work
	// when linear, 64 times when quadratic. Buffer growth by doubling
	// makes a single doubling too noisy to compare.
	a, b := measure(16<<10, 2000), measure(128<<10, 16000)
	t.Logf("%s: %d and %d bytes allocated, ratio %.1f", name, a, b, float64(b)/float64(a))
	if b > 24*a {
		t.Errorf("%s: eight times the scalar and the blank run allocate %d bytes against %d: not linear", name, b, a)
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

// TestTaggedBlockScalarBlankLinesAreLinear checks the keep-chomping rewrite
// on tagged block scalars and sequence entries (01 req 12; 11 req 26:
// linear cost; finding 2 of the WP-33 review).
func TestTaggedBlockScalarBlankLinesAreLinear(t *testing.T) {
	for _, tt := range []struct{ prefix, header string }{
		{"a: ", "!!str |"}, {"a: ", "!!str >"}, {"- ", "!!str |"}, {"a: ", "!!str   >2"},
	} {
		checkLinear(t, tt.prefix+tt.header, func(n, m int) string { return longTaggedScalar(tt.prefix, tt.header, n, m) })
	}
}

// TestBlockScalarBeforeCommentIsLinear checks the keep-chomping rewrite on
// a block scalar that a comment ends after a long blank run (finding 1 of
// the second WP-33 review; 01 test plan FuzzProfileYAML, 11 req 26): the
// comment, not the next entry, ends the scalar, so the rewrite applies
// whether the comment is at column 0, less indented than the scalar,
// followed by the end of the file or by more blank lines.
func TestBlockScalarBeforeCommentIsLinear(t *testing.T) {
	run := strings.Repeat("\n", 2*longBlankRun)
	for _, tt := range []struct{ name, prefix, header, tail string }{
		{"comment at column 0", "a: ", "|", "# c\nb: 1\n"},
		{"less indented comment", "a: ", "|", " # c\nb: 1\n"},
		{"comment then end of file", "a: ", ">", "# c\n"},
		{"comment then blank lines", "a: ", "|", "# c\n" + run + "b: 1\n"},
		{"sequence entry", "- ", "|", "# c\n- z\n"},
		{"tagged sequence entry", "- ", "!!str >", " # c\n- z\n"},
		{"tagged scalar", "a: ", "!!str |2", "# c\nb: 1\n"},
	} {
		checkLinear(t, tt.name, func(n, m int) string { return longBlockScalar(tt.prefix, tt.header, n, m, tt.tail) })
	}
}

// TestCutBlankRuns checks the cut that keeps the first scan of the
// keep-chomping rewrite linear (11 req 26): a run keeps its first lines,
// its longest line and its last line, and a line holding a tab is not
// blank.
func TestCutBlankRuns(t *testing.T) {
	tests := []struct {
		text, want string
		lines      []int
	}{
		{"a\n\n\n\n \nb\n\t\nc\n", "a\n\n\n \nb\n\t\nc\n", []int{1, 2, 3, 5, 6, 7, 8, 9}},
		{"a\n\n\n   \n\n\n\nb", "a\n\n\n   \n\nb", []int{1, 2, 3, 4, 7, 8}},
		{"|1\n\n\n\n\n\n\n\n  ", "|1\n\n\n  ", []int{1, 2, 3, 9}},
		{"a\n  \n \n\n\n\n", "a\n  \n \n\n", []int{1, 2, 3, 6, 7}},
		{"", "", []int{1}},
	}
	for _, tt := range tests {
		got, lineOf := cutBlankRuns(tt.text, 2)
		if got != tt.want {
			t.Errorf("cut %q: %q, want %q", tt.text, got, tt.want)
		}
		if fmt.Sprint(lineOf) != fmt.Sprint(tt.lines) {
			t.Errorf("cut %q: lines %v, want %v", tt.text, lineOf, tt.lines)
		}
	}
	text := tests[0].text
	if !blankRuns(text, 4) || blankRuns(text, 5) || blankRuns("a\n\t\n\t\n", 1) {
		t.Error("blankRuns")
	}
}

// TestKeepChompingSpaceContent covers a block scalar whose only content is
// spaces past its indentation, after a long run of empty lines (01 req
// 12): the first scan must see that content, or the rewrite strips it
// (found by FuzzLoadYAML).
func TestKeepChompingSpaceContent(t *testing.T) {
	for _, src := range []string{
		"|1\n\n\n\n\n\n\n\n  ",
		"|1\n\n\n\n\n\n\n\n\n   \n\n\n\n\n\n\n\n\n\n",
		"a: >1\n\n\n\n\n\n\n\n\n    \n\n\n\n\n\n\n\n\n\nb: 1\n",
		"a: |\n\n\n\n\n\n\n\n\n      \n\n\n\n\n\n\n\n\n\n  x\n",
		"a: |\n\n\n      \n\n\n\n\n\n\n\n\n\n  x\n",
	} {
		want, wd, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{noRewrite: true}, true)
		if err != nil {
			t.Fatal(err)
		}
		got, gd, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, true)
		if err != nil {
			t.Fatal(err)
		}
		if codes(wd) != codes(gd) || len(want) != len(got) {
			t.Errorf("%q: findings %q, rewritten %q", src, codes(wd), codes(gd))
			continue
		}
		for i := range want {
			if a, b := show(want[i].Root, true), show(got[i].Root, true); a != b {
				t.Errorf("%q document %d:\n  clipped %s\nrewritten %s", src, i, a, b)
			}
		}
	}
}
