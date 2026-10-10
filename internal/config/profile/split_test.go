// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strings"
	"testing"
)

// parseWith parses YAML with the split threshold set.
func parseWith(t testing.TB, src string, splitAt int) ([]Document, string) {
	t.Helper()
	docs, diags, err := parse(context.Background(), []byte(src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, true)
	if err != nil {
		t.Fatal(err)
	}
	return docs, codes(diags)
}

// TestSplitParseMatchesGoccy parses every YAML Test Suite stream twice,
// with every block collection split entry by entry and with goccy's
// whole-document parse, and requires the same trees, positions and finding
// codes (01 req 10, 14; 11 req 18): the split parse, which every block
// collection takes, never changes a result goccy gives for a stream it
// reads as YAML 1.2 does.
func TestSplitParseMatchesGoccy(t *testing.T) {
	inputs := append(suiteInputs(t), splitSamples()...)
	same := 0
	for _, in := range inputs {
		whole, wd := parseWith(t, in, -1)
		split, sd := parseWith(t, in, 0)
		if (wd == "") != (sd == "") {
			t.Errorf("findings differ: whole %q, split %q\ninput:\n%s", wd, sd, in)
			continue
		}
		if wd != "" {
			// Both parses report the same codes in the same order. The
			// positions agree too, except that the split parse refuses a
			// flow collection as a key at the key, where goccy's whole
			// parse refuses it at the ':' ("found an invalid key").
			if a, b := findingCodes(wd), findingCodes(sd); a != b {
				t.Errorf("finding codes differ: whole %q, split %q\ninput:\n%s", wd, sd, in)
			}
			continue
		}
		if len(whole) != len(split) {
			t.Errorf("%d documents whole, %d split\ninput:\n%s", len(whole), len(split), in)
			continue
		}
		for i := range whole {
			if a, b := show(whole[i].Root, true), show(split[i].Root, true); a != b {
				t.Errorf("document %d:\nwhole %s\nsplit %s\ninput:\n%s", i, a, b, in)
			}
			if whole[i].Start != split[i].Start {
				t.Errorf("document %d start whole %+v, split %+v\ninput:\n%s", i, whole[i].Start, split[i].Start, in)
			}
		}
		same++
	}
	if same < 200 {
		t.Fatalf("compared %d streams", same)
	}
}

// findingCodes returns the codes of codes output, without positions.
func findingCodes(codes string) string {
	var out []string
	for _, c := range strings.Fields(codes) {
		code, _, _ := strings.Cut(c, "@")
		out = append(out, code)
	}
	return strings.Join(out, " ")
}

func splitSamples() []string {
	return []string{
		"apiVersion: v\nkind: Route\nspec:\n  a: 1\n  b:\n  - x\n  - y: [1, 2]\n    z: |\n      t\n  c: !!str 0777\n  ? d\n  : e\n  f:\n  # comment\n  g: \"q\"\n",
		"a:\n- 1\n- - 2\n  - 3\n- k: v\n  l:\n    m: n\nb: {x: 1}\n",
		"%YAML 1.2\n--- # c\na: 1\nb:\n  c: 2\n...\n",
		"!!map\na: 1\nb: !!seq\n- 1\n",
		"- a\n- b: c\n  d: e\n-\n- - f\n",
	}
}

// TestSplitWideMappingIsLinear checks that a mapping of many entries
// parses in linear work (11 req 26): twice the entries take about twice
// the allocations, where goccy's whole-mapping parse takes four times.
func TestSplitWideMappingIsLinear(t *testing.T) {
	gen := func(n int) string {
		var b strings.Builder
		b.WriteString("spec:\n")
		for i := range n {
			b.WriteString("  k")
			b.WriteString(strings.Repeat("x", i%7))
			b.WriteString(itoa(i))
			b.WriteString(": v\n")
		}
		return b.String()
	}
	measure := func(src string) uint64 {
		before := allocBytes()
		docs, diags, err := Parse(context.Background(), []byte(src), 0, "f", FormatYAML, Options{})
		if err != nil || len(diags) != 0 || len(docs) != 1 {
			t.Fatal(err, codes(diags))
		}
		return allocBytes() - before
	}
	a, b := measure(gen(4000)), measure(gen(8000))
	if b > 3*a {
		t.Errorf("8,000 entries allocate %d bytes, 4,000 allocate %d: not linear", b, a)
	}
	docs, _ := parseWith(t, gen(4000), 0)
	spec, _ := docs[0].Root.Get("spec")
	if len(spec.Members) != 4000 {
		t.Fatalf("%d members", len(spec.Members))
	}
}

// TestDedentedEntries covers an entry that ends block collections deeper
// than its column without returning to an enclosing one (01 req 9, 10; 11
// req 17, 26; blocker of the seventh WP-33 review). The token pass opened
// a sibling collection at the entry's column, so the frame at the start of
// the value no longer covered it, and the split parse handed the whole
// value, every nested block collection included, to one goccy parse:
// 125,000 entries took 37 s before goccy refused the line. Such an entry is
// RZ-CFG-001 at its start now, split or not, while the shapes YAML 1.2
// allows still parse.
func TestDedentedEntries(t *testing.T) {
	tests := []struct{ src, want string }{
		{"a:\n  k: v\n c: 2\n", "RZ-CFG-001@3:2"},
		{"a:\n  k:\n c: 2\n", "RZ-CFG-001@3:2"},
		{"  k: v\nb: 2\n", "RZ-CFG-001@2:1"},
		{"a:\n  k:\n  -\n c: 2\n", "RZ-CFG-001@4:2"},
		{"a:\n  - x\n  - b: 1\n    c: 2\n  d: 3\n", "RZ-CFG-001@5:3"},
		{"a:\n  b:\n    c: 1\n  - x\n", "RZ-CFG-001@4:3"},
		{"a:\n  b: 1\n - x\n", "RZ-CFG-001@3:2"},
		{"x:\n  ? a\n  : - b\n    - c\n ? d\n", "RZ-CFG-001@5:2"},
		{"- - a\n  - b\n - c\n", "RZ-CFG-001@3:2"},
		{dedented(longKeysNested(3, 10, 2)), "RZ-CFG-001@6:2"},
		// Valid: back to an enclosing collection's column, and a mapping
		// entry after a sequence at its key's column.
		{"a:\n  k: v\nc: 2\n", `{a:{k:s:"v"},c:i:2}`},
		{"a:\n- b:\n    c: 1\nd: 2\n", "{a:[{b:{c:i:1}}],d:i:2}"},
		{"a:\n  b:\n  - x\n  c: 1\n", `{a:{b:[s:"x"],c:i:1}}`},
		{"- - a\n  - b\n- c\n", `[[s:"a",s:"b"],s:"c"]`},
		{"? a\n: - b\n  - c\nd: 1\n", `{a:[s:"b",s:"c"],d:i:1}`},
		{"- ? a\n  : b\n- c\n", `[{a:s:"b"},s:"c"]`},
		{"a:\n    b: 1\n    c:\n        - d\ne: 1\n", `{a:{b:i:1,c:[s:"d"]},e:i:1}`},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, tt.want)
			}
		}
		if _, diags := parseYAML(t, tt.src); strings.HasPrefix(tt.want, "RZ-") && (len(diags) != 1 || diags[0].Message != msgIndent) {
			t.Errorf("%q: %+v, want the message %q", tt.src, diags, msgIndent)
		}
	}
}

// TestSplitNeverParsesBlockFrames covers the split parse's own guard (01
// req 10; 11 req 17, 26; seventh WP-33 review): a value range that holds a
// recorded block frame the frame does not cover, which the token pass no
// longer records, is refused at the first token past the frame without
// goccy's parse, which took 1.6 s and 1.6 GiB for 20,000 entries. The
// token pass runs here with its indentation check off, as it ran before
// the fix.
func TestSplitNeverParsesBlockFrames(t *testing.T) {
	var wide strings.Builder
	for range 20000 {
		wide.WriteString("  k: v\n")
	}
	tests := []struct{ src, want string }{
		{"a:\n" + wide.String() + " c: 2\n", "RZ-CFG-001@20002:2"},
		{"a: !!map\n" + wide.String() + " c: 2\n", "RZ-CFG-001@20002:2"},
		{wide.String() + "b: 2\n", "RZ-CFG-001@20001:1"},
		{"a:\n  ? k\n  : v\n ? c\n", "RZ-CFG-001@4:2"},
	}
	for _, tt := range tests {
		tks, err := scanString(tt.src)
		if err != nil {
			t.Fatal(err)
		}
		p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
		pass := &tokenPass{p: p, maxDepth: 64, maxTokens: 1 << 20, splitAt: 1}
		pass.errs = 1 // checks that stop only a file without findings are off
		cols := columnsOf(tks, tt.src)
		pass.run(tks, 0, cols, tt.src)
		if len(p.diags) != 0 || len(pass.splits) == 0 {
			t.Fatalf("token pass: %q", codes(p.diags))
		}
		before := allocBytes()
		p.parseSegment(tks, segment{text: tt.src, startLine: 1}, pass.splits, cols, pass.fixes)
		if got := codes(p.diags); got != tt.want || p.diags[0].Message != "syntax error: value is not allowed in this context" {
			t.Errorf("%d bytes: %s %+v, want %s", len(tt.src), got, p.diags, tt.want)
		}
		if alloc := allocBytes() - before; alloc > 64<<20 {
			t.Errorf("%d bytes: allocated %d MiB", len(tt.src), alloc>>20)
		}
	}
	// A block collection inside a key range is not a scalar key.
	s := splitter{frames: map[int]*blockFrame{0: {}, 2: {}}}
	if s.blockInside(1, 2) != -1 || s.blockInside(1, 3) != 2 || s.blockInside(3, 9) != -1 {
		t.Error("blockInside")
	}
}
