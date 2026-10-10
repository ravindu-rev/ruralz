// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestSplitEdgeCases runs streams the YAML Test Suite does not cover
// through the split parse, the default, and goccy's whole-document parse
// (01 req 8, 10, 11, 12, 14; minor finding of the sixth WP-33 review). Each
// row gives the split parse's result, its findings or its accepted tree,
// and goccy's own parse must give the same, with the same positions when
// both accept, unless goccy gives its own reading.
func TestSplitEdgeCases(t *testing.T) {
	tests := []struct {
		src  string
		want string // the split parse's findings, or its tree
		// goccy is goccy's whole parse where it differs: its findings
		// (findings are compared by code only when both start with RZ-) or
		// its tree; "" means the same as want.
		goccy string
	}{
		{"a: - x\nb: 1\n", "RZ-CFG-001@1:4", ""},
		{"a: b: c\nd: 1\n", "RZ-CFG-001@1:4", ""},
		{"a: [1\nb: 2\n", "RZ-CFG-001@2:1", ""},
		{"a: 1\nb: {x: [1}\n", "RZ-CFG-001@2:10", ""},
		{"? [x]\n: y\nz: 1\n", "RZ-CFG-001@1:3", ""},
		// "v z" is one plain scalar over two lines, so the ':' on line 2
		// would make it a multi-line implicit key.
		{": v\nz: 1\n", "RZ-CFG-001@2:2", ""},
		{"? \n: v\nz: 1\n", "RZ-CFG-001@2:1", ""},
		{"!!seq\na: 1\nb: 2\n", "RZ-CFG-001@1:1", ""},
		{"!!map\na: 1\nb: 2\n", "{a:i:1,b:i:2}", ""},
		{"--- a: 1\nb: 2\n", "RZ-CFG-001@2:1", ""},
		{"? a\n? b\nc: 1\n", "{a:n,b:n,c:i:1}", ""},
		{"a: 1\nb: 2\na: 3\n", "RZ-CFG-002@3:1", ""},
		{"a: 1\na: 2\nb: .inf\na: 3\nc: !!int x\n", "RZ-CFG-002@2:1 RZ-CFG-005@3:4 RZ-CFG-002@4:1 RZ-CFG-001@5:4", ""},
		{"!!int x: 1\ny: 2\n", "RZ-CFG-001@1:1", ""},
		{"? |\n  k\n: v\nw: 1\n", "{k\n:s:\"v\",w:i:1}", ""},
		{"a:\n  - x\n  - y: 1\n    z:\n      - !!str 1\nb: |\n  t\n", `{a:[s:"x",{y:i:1,z:[s:"1"]}],b:s:"t\n"}`, ""},
		{"a:\n- 1\n- - 2\n  -\n- k: !!seq\n  - x\n", `{a:[i:1,[i:2,n],{k:[s:"x"]}]}`, ""},
		{"a: !!map\n  b: 1\n  c: 2\nd: !!seq\n- 1\n", "{a:{b:i:1,c:i:2},d:[i:1]}", ""},
		{"a: !!str\nb: 1\n", "RZ-CFG-001@1:4", ""},
		{"- !!str\n- x\n", "RZ-CFG-001@1:3", ""},
		{"a:\n  b: !!str\nc: 1\n", "RZ-CFG-001@2:6", ""},
		{"a: !!str # c\nb: 1\n", "RZ-CFG-001@1:4", ""},
		{"a:\n  - !!str\n  - x\n", "RZ-CFG-001@2:5", ""},
		{"a: !!null\n# c\n", "{a:n}", ""},
		{"- !!str\n", `[s:""]`, ""},
		{"a: 1 # c\n# c\nb: # c\n  # c\n  c: 2\n", "{a:i:1,b:{c:i:2}}", ""},
		{"%\n---\n0:\n", "RZ-CFG-001@1:1", ""},
		{"%FOO bar\n---\na: 1\nb: 2\n", "{a:i:1,b:i:2}", ""},
		{"? |+0\n", "RZ-CFG-001@1:3", ""},
		// goccy nests the key in the empty entry; the split parse keeps
		// YAML 1.2's block structure.
		{"0:\n-\n1:\n", "{0:[n],1:n}", "{0:[{1:n}]}"},
		// goccy pairs a '?' with the next token only: a second node on
		// the '?' line became the entry's value with no ':', and "? k:"
		// (the complex key {k: null}) the key "k"; the split parse
		// refused both (minor finding of the fifth WP-33 review).
		{"? \"q\"k\n", "RZ-CFG-001@1:6", ""},
		{"a:\n  ? \"q\"''\n", "RZ-CFG-001@2:8", ""},
		{"? k:\n", "RZ-CFG-001@1:4", ""},
		{"? \"q\"k\nz: 1\n", "RZ-CFG-001@1:6", ""},
		// An explicit key of more than one node (blocker of the fifth
		// WP-33 review).
		{"? \"a\"\n  - \"b\"\n:\nz: 1\n", "RZ-CFG-001@3:1", ""},
		{"? \"a\"\n  \"b\"\n:\nz: 1\n", "RZ-CFG-001@3:1", ""},
		{"? !!str \"a\"\n: v\n? a\n  b\n: w\nz: 1\n", `{a:s:"v",a b:s:"w",z:i:1}`, ""},
		// Explicit entries goccy misreads in other ways (fifth WP-33
		// review, found by the width check): a node after the key that is
		// no new entry, an empty key, a tag that goccy does not group with
		// its content.
		{"? a\nb\nz: 1\n", "RZ-CFG-001@2:1", ""},
		{"? \nz: 1\n", "RZ-CFG-001@1:1", ""},
		{"? !!str {k: v}\nz: 1\n", "RZ-CFG-001@1:9", ""},
		// A block collection on its tag's line, which goccy tags in a
		// value or an entry but refuses on its own.
		{"k: !!seq - a\nz: 1\n", "RZ-CFG-001@1:10", ""},
		{"- !!map a: b\n- 1\n", "RZ-CFG-001@1:3", ""},
		{"k: !!map ? a\nz: 1\n", "RZ-CFG-001@1:10", ""},
		{"k: !!seq\n- a\nz: 1\n", `{k:[s:"a"],z:i:1}`, ""},
		// A flow mapping at its key's column on a later line, which goccy
		// reads as a key; a flow sequence there, which goccy reads as the
		// value, is refused too: YAML 1.2 indents a value on a later line
		// past its key (minor finding of the eighth WP-33 review).
		{"k:\n{a: b}\nz: 1\n", "RZ-CFG-001@2:1", ""},
		{"k:\n[a]\nz: 1\n", "RZ-CFG-001@2:1", `{k:[s:"a"],z:i:1}`},
		// A directive after "---", and keys that are never typed.
		{"--- %0\n-\n", "RZ-CFG-001@1:5", ""},
		{"10000000000000000000: 1\n.inf: 2\n0x: 3\n", "{10000000000000000000:i:1,.inf:i:2,0x:i:3}", ""},
		// A value on a later line at or left of its entry's column, which
		// YAML 1.2 refuses (s-l+block-node indents it by at least n+1) and
		// goccy read as the value; only a block sequence may be a mapping
		// value at its key's column. A mapping entry at a block sequence's
		// column belongs to the mapping holding the sequence, or is
		// refused (minor finding of the eighth WP-33 review).
		{"k:\nv\n", "RZ-CFG-001@2:1", `{k:s:"v"}`},
		{"a:\n  k:\n  v\n", "RZ-CFG-001@3:3", `{a:{k:s:"v"}}`},
		{"spec:\n  name:\n  [a]\n", "RZ-CFG-001@3:3", `{spec:{name:[s:"a"]}}`},
		{"x:\n  a:\n  'v'\nb: 1\n", "RZ-CFG-001@3:3", `{x:{a:s:"v"},b:i:1}`},
		{"-\nv\n", "RZ-CFG-001@2:1", `[s:"v"]`},
		{"k:\n-\nv\n", "RZ-CFG-001@3:1", `{k:[s:"v"]}`},
		{"a:\n  -\n  v\n", "RZ-CFG-001@3:3", `{a:[s:"v"]}`},
		{"spec:\n  items:\n  -\n  x\n  - y\n", "RZ-CFG-001@4:3", `{spec:{items:[s:"x",s:"y"]}}`},
		{"? a\n:\nv\n", "RZ-CFG-001@3:1", `{a:s:"v"}`},
		{"x:\n  k2:\n  -\n  -? a\n   -b\n  - x\n", "RZ-CFG-001@4:3", `{x:{k2:[s:"-? a -b",s:"x"]}}`},
		{"-\nk: v\n", "RZ-CFG-001@2:1", ""},
		{"x:\n  -\n  k: v\n  m: 1\ny: 2\n", "RZ-CFG-001@3:3", ""},
		{"- -\n  k: v\n", "RZ-CFG-001@2:3", ""},
		{"-\n? k\n: v\n", "RZ-CFG-001@2:1", ""},
		{"rules:\n    -\n    match: /api\n", "RZ-CFG-001@3:5", ""},
		{"- a\n- b\nk: v\n", "RZ-CFG-001@3:1", ""},
		{"? a\n:\n- x\nk: v\n", `{a:[s:"x"],k:s:"v"}`, ""},
		{"k:\n- a\nz:\n  - b\n", `{k:[s:"a"],z:[s:"b"]}`, ""},
		// Messages come from one parse, whatever the width (minor finding
		// of the sixth WP-33 review).
		{"!!str k: c\n  x\n", "RZ-CFG-001@2:3", ""},
		{"k: a\nb\nz: 1\n", "RZ-CFG-001@2:1", ""},
	}
	result := func(docs []Document, findings string) string {
		if findings != "" || len(docs) != 1 {
			return findings
		}
		return show(docs[0].Root, false)
	}
	for _, tt := range tests {
		split, sd := parseWith(t, tt.src, 0)
		if got := result(split, sd); got != tt.want {
			t.Errorf("%q: %s, want %s", tt.src, got, tt.want)
		}
		whole, wd := parseWith(t, tt.src, -1)
		got, want := result(whole, wd), tt.want
		if tt.goccy != "" {
			want = tt.goccy
		}
		if strings.HasPrefix(got, "RZ-") && strings.HasPrefix(want, "RZ-") {
			got, want = strings.Fields(got)[0][:10], strings.Fields(want)[0][:10]
		}
		if got != want {
			t.Errorf("%q, goccy's parse: %s, want %s", tt.src, got, want)
		}
		if tt.goccy == "" && sd == "" && wd == "" {
			for i := range whole {
				if a, b := show(whole[i].Root, true), show(split[i].Root, true); a != b {
					t.Errorf("%q:\nwhole %s\nsplit %s", tt.src, a, b)
				}
			}
		}
	}
	// The two messages that differed between goccy's parse of a narrow
	// mapping and the split parse of a wide one are one message now, and
	// a value at its key's column is refused at every width.
	for _, src := range []string{"!!str k: c\n  x\n", "k: a\nb\nz: 1\n", "k:\nv\n", "a:\n  k:\n  v\n", "k:\n-\nv\n", "rules:\n    -\n    match: /api\n"} {
		widthIndependent(t, []byte(src), 300)
	}
}

// TestNullPositionsFixed covers the implicit null of an empty value (01
// req 14; minor finding of the eighth WP-33 review): it sits in the column
// after its ':', '-' or explicit key's first token, whatever follows the
// entry, in goccy's parse and the split parse alike. goccy put it one
// column further at the end of its tokens, so "k1:" alone and before
// "w0: v" placed it apart, and the null of "? a\n  -b" moved to the end of
// the key with a blank line after it.
func TestNullPositionsFixed(t *testing.T) {
	tests := []struct{ src, want string }{
		{"k1:\n", "{k1@1:1:n@1:4}"},
		{"k1: # c\n", "{k1@1:1:n@1:4}"},
		{"? a\n  -b\n", "{a -b@1:3:n@1:4}"},
		{"? a\n:\n", "{a@1:3:n@2:2}"},
		{"x:\n- \n", "{x@1:1:[n@2:2]}"},
		{"x:\n  ? !!str c\n", "{x@1:1:{c@2:5:n@2:6}}"},
		{"x: {a, b: }\n", "{x@1:1:{a@1:5:n@1:6,b@1:8:n@1:10}}"},
		{"x:\n  a:\n  # c\n", "{x@1:1:{a@2:3:n@2:5}}"},
	}
	for _, tt := range tests {
		for _, tail := range []string{"", "\n", "# c\n", "...\n", "zz: 1\n"} {
			for _, splitAt := range []int{-1, 0, 3} {
				if splitAt < 0 && tail == "zz: 1\n" {
					// goccy nests a key after an empty last sequence entry
					// in that entry (TestEmptyEntryBeforeKeyIndependentOfWidth).
					continue
				}
				docs, got := parseWith(t, tt.src+tail, splitAt)
				if got != "" || len(docs) != 1 {
					t.Errorf("%q, split at %d: %q, %d documents", tt.src+tail, splitAt, got, len(docs))
					continue
				}
				root := docs[0].Root
				if tail == "zz: 1\n" {
					root = &tree.Node{Kind: tree.KindMap, Members: root.Members[:len(root.Members)-1]}
				}
				if got := show(root, true); got != tt.want {
					t.Errorf("%q, split at %d: %s, want %s", tt.src+tail, splitAt, got, tt.want)
				}
			}
		}
	}
}

// TestEmptyEntryBeforeKeyIndependentOfWidth checks shapes goccy reads
// otherwise than YAML 1.2 after an empty value (01 req 8, 10; 01 test plan
// P1): an empty last sequence entry before a key at the sequence's column,
// which goccy reads as a mapping inside the entry (finding 6 of the WP-33
// review), and a tagged key at or left of a mapping whose last entry has
// no value (an implicit entry with nothing after its ':', a '?' entry with
// no ':', a bare ':'), which goccy refuses with "tag is not allowed in
// this context" (blocker of the sixth WP-33 review). Every block
// collection takes the split parse, which reads YAML 1.2's tree, so the
// tree is the same alone, after 300 sibling entries, and with the split
// threshold raised to 2; goccy's own parse (splitAt -1) gives goccy's
// reading, recorded in goccy. A key's order does not change the result
// either. An empty last entry before a key takes the split parse at any
// threshold (tokenPass.block): the rows marked any are checked at 256 too,
// where nothing else is split (minor finding of the seventh WP-33 review).
func TestEmptyEntryBeforeKeyIndependentOfWidth(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	wideRoot := mustRoot(t, wide.String())
	tests := []struct {
		src, want, goccy string
		any              bool
	}{
		{"a:\n-\nb: 1\n", "{a:[n],b:i:1}", "{a:[{b:i:1}]}", true},
		{"a:\n- # c\n# d\nb: 1\n", "{a:[n],b:i:1}", "{a:[{b:i:1}]}", true},
		{"a:\n- x\n-\nb: 1\n", `{a:[s:"x",n],b:i:1}`, `{a:[s:"x",{b:i:1}]}`, true},
		{"a:\n-\n? b\n: 1\n", "{a:[n],b:i:1}", "{a:[{b:i:1}]}", true},
		{"a:\n-\n!!str b: 1\n", `{a:[n],b:i:1}`, "RZ-CFG-001@3:1", true},
		{"x:\n  a:\n  -\n  b: 1\nc: 2\n", "{x:{a:[n],b:i:1},c:i:2}", "{x:{a:[{b:i:1}]},c:i:2}", true},
		{"- a:\n  -\n  b: 1\n- 2\n", "[{a:[n],b:i:1},i:2]", "[{a:[{b:i:1}]},i:2]", true},
		{"a:\n- x\nb: 1\n", `{a:[s:"x"],b:i:1}`, "", true},
		// A tagged key left of the sequence, which goccy reads as the
		// empty entry's value and refuses (fifth WP-33 review).
		{"a:\n -\n!!str b: 1\n", "{a:[n],b:i:1}", "RZ-CFG-001@3:1", true},
		{"x:\n  a:\n   -\n!!str b: 1\n", "{x:{a:[n]},b:i:1}", "RZ-CFG-001@4:1", true},
		{"a:\n - # c\n# d\n!!str b: 1\n", "{a:[n],b:i:1}", "RZ-CFG-001@4:1", true},
		// A tagged key after a mapping whose last entry has no value
		// (sixth WP-33 review).
		{"a:\n  b:\n!!str c: 1\n", "{a:{b:n},c:i:1}", "RZ-CFG-001@3:1", false},
		{"e:\n  ? k\n!!str x: v\n", `{e:{k:n},x:s:"v"}`, "RZ-CFG-001@3:1", false},
		{"a:\n  ? b\n!!str c: 1\n", "{a:{b:n},c:i:1}", "RZ-CFG-001@3:1", false},
		{"x:\n  a: 1\n  ? b\n  :\n!!str d: 1\n", "{x:{a:i:1,b:n},d:i:1}", "RZ-CFG-001@5:1", false},
		{"a:\n  b:\n    c:\n  !!str d: 1\n", "{a:{b:{c:n},d:i:1}}", "RZ-CFG-001@4:3", false},
		{"- a:\n    b:\n  !!str c: 1\n", "[{a:{b:n},c:i:1}]", "RZ-CFG-001@3:3", false},
		{"a:\n  b:\n  # c\n!!str c: 1\n", "{a:{b:n},c:i:1}", "RZ-CFG-001@4:1", false},
		{"a:\n  b:\n\n!!str c: 1\n", "{a:{b:n},c:i:1}", "RZ-CFG-001@4:1", false},
		{"a:\n  b:\n!!int 1: 1\n", "{a:{b:n},1:i:1}", "RZ-CFG-001@3:1", false},
		{"a:\n  b:\n!!str 'c': 1\n", "{a:{b:n},c:i:1}", "RZ-CFG-001@3:1", false},
		{"e:\n ? k\n!!str x: v", `{e:{k:n},x:s:"v"}`, "RZ-CFG-001@3:1", false},
		{"  e:e:\n   ? k  \n  !!str x: v\n  #c", `{e:e:{k:n},x:s:"v"}`, "RZ-CFG-001@3:3", false},
		{"!!str c: 1\na:\n  b:\n", "{c:i:1,a:{b:n}}", "", false},
		// Invalid YAML is refused at every width.
		{"   :[ !!str k:\n    ,y z :\n   !!str x {x: y}\n", "RZ-CFG-001@2:5", "RZ-CFG-001@2:5", false},
	}
	for _, tt := range tests {
		thresholds := []int{-1, 0, 2}
		if tt.any {
			thresholds = append(thresholds, 256)
		}
		for _, splitAt := range thresholds {
			docs, got := parseWith(t, tt.src, splitAt)
			if got == "" && len(docs) == 1 {
				got = show(docs[0].Root, false)
			}
			want := tt.want
			if splitAt < 0 && tt.goccy != "" {
				want = tt.goccy
			}
			if got != want {
				t.Errorf("%q, split at %d: %s, want %s", tt.src, splitAt, got, want)
			}
		}
		if strings.HasPrefix(tt.src, "- ") || strings.HasPrefix(tt.src, " ") {
			continue
		}
		docs, wd := parseWith(t, wide.String()+tt.src, 0)
		if strings.HasPrefix(tt.want, "RZ-") {
			if want := shiftCodes(t, tt.want, 300); wd != want {
				t.Errorf("%q wide: %q, want %q", tt.src, wd, want)
			}
			continue
		}
		if wd != "" || len(docs) != 1 {
			t.Errorf("%q wide: %q", tt.src, wd)
			continue
		}
		want := strings.TrimSuffix(show(wideRoot, false), "}") + "," + strings.TrimPrefix(tt.want, "{")
		if got := show(docs[0].Root, false); got != want {
			t.Errorf("%q wide: %s, want %s", tt.src, got, want)
		}
		widthIndependent(t, []byte(tt.src), 300)
	}
}

// shiftCodes moves the lines of codes output down by shift.
func shiftCodes(t *testing.T, codes string, shift int) string {
	t.Helper()
	var out []string
	for _, c := range strings.Fields(codes) {
		code, pos, _ := strings.Cut(c, "@")
		line, col, _ := strings.Cut(pos, ":")
		n, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("codes %q", codes)
		}
		out = append(out, code+"@"+strconv.Itoa(n+shift)+":"+col)
	}
	return strings.Join(out, " ")
}

// mustRoot parses src and returns its one document's root.
func mustRoot(t *testing.T, src string) *tree.Node {
	t.Helper()
	docs, diags := parseYAML(t, src)
	if len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("%q", codes(diags))
	}
	return docs[0].Root
}

// TestWidthIndependence runs generated streams of YAML indicators,
// scalars, tags, flow collections and comments through widthIndependent:
// each must parse alone and after 300 sibling entries alike, findings and
// positions included (01 req 8; fifth and sixth WP-33 reviews, whose
// findings were shapes of this kind: tagged keys after empty values, '?'
// entries, plain scalars continued by a line starting with '-'). Every
// stream starts with a key at column 1, so the siblings join its mapping.
// A fixed generator makes every run see the same inputs; FuzzLoadYAML
// checks the same property on its own inputs.
func TestWidthIndependence(t *testing.T) {
	n := 8000
	if testing.Short() || raceEnabled {
		n = 1000
	}
	atoms := []string{
		"? ", "- ", ": ", "a", "\"b\"", "'c'", "!!str ", "!!map ", "!!seq ", "!!int 1", "|", ">",
		"[x]", "{k: v}", "[", "]", ",", "k: ", "", " ", "# c", "\n",
		"-b #c", "- z #c", "!!str c: 1", "k:", "\"q\": 1", "|-", ">+",
	}
	g := splitMix(0x5eed)
	var b strings.Builder
	for range n {
		b.Reset()
		b.WriteString("k: ")
		for range 1 + g.below(6) {
			for range 1 + g.below(4) {
				b.WriteString(atoms[g.below(len(atoms))])
			}
			b.WriteByte('\n')
			b.WriteString(strings.Repeat(" ", g.below(7)))
		}
		widthIndependent(t, []byte(b.String()), 300)
	}
}

// splitMix is the SplitMix64 generator: a deterministic source of test
// inputs, not of anything secret.
type splitMix uint64

// below returns a number in [0, n).
func (g *splitMix) below(n int) int {
	*g += 0x9e3779b97f4a7c15
	z := uint64(*g)
	z = (z ^ z>>30) * 0xbf58476d1ce4e5b9
	z = (z ^ z>>27) * 0x94d049bb133111eb
	z ^= z >> 31
	return int(z % uint64(n)) //nolint:gosec // G115: n is a small positive int, so the remainder fits
}

// TestPartialSplitNullPositions covers the implicit null that ends the
// tokens a split value hands goccy (01 req 14; fifth WP-33 review, found
// by FuzzLoadYAML's width check): goccy places it one column further at
// the end of its tokens than inside a document, after a ':', a '-' or the
// first token of an explicit key with no ':' (sixth WP-33 review), so the
// split parse moves it to the position goccy's whole parse gives it. With
// the threshold raised to 2 or 3 the outer mappings are split and the
// inner ones are not; at the default every one is.
func TestPartialSplitNullPositions(t *testing.T) {
	var entries strings.Builder
	entries.WriteString("x:\n")
	for range 300 {
		entries.WriteString("  - ? b\n")
	}
	entries.WriteString("y: 1\n")
	for _, src := range []string{
		"x:\n  a:\ny: 1\n",
		"x:\n  a: 1\n  b:\ny: 1\n",
		"x:\n  a: # c\ny: 1\n",
		"x:\n  a:\n# c\ny: 1\n",
		"s:\n  - 1\n  -\nt: 1\n",
		"x:\n  !!str a:\ny: 1\n",
		"? a\n:\n  b:\nc: 1\n",
		"x:\n  a:\n",
		"x:\n  ? c\ny: 1\n",
		"x:\n- ? c\ny: 1\n",
		"x:\n  ? !!str c\ny: 1\n",
		"x:\n  ? |\n    c\ny: 1\n",
		"x:\n  ? c # d\n# e\ny: 1\n",
		"x:\n  ? c\n",
		entries.String(),
	} {
		whole, wd := parseWith(t, src, -1)
		for _, splitAt := range []int{0, 2, 3} {
			split, sd := parseWith(t, src, splitAt)
			if wd != "" || sd != "" || len(whole) != 1 || len(split) != 1 {
				t.Errorf("%q, split at %d: %q, %q", src, splitAt, wd, sd)
				continue
			}
			if a, b := show(whole[0].Root, true), show(split[0].Root, true); a != b {
				t.Errorf("%q, split at %d:\nwhole %s\nsplit %s", src, splitAt, a, b)
			}
		}
	}
	// The fuzz input: an empty last entry before a key takes the split
	// parse at any threshold (tokenPass.block), whose entries end in
	// nulls; and the shape of the sixth WP-33 review, whose null moved
	// with the threshold. The default and a raised threshold agree.
	for _, src := range []string{"0:\n- 0:\n-\n00:", "b2:\n- ? c\n- \n\"n\": \n"} {
		widthIndependent(t, []byte(src), 300)
		a, ad := parseWith(t, src, 0)
		b, bd := parseWith(t, src, 256)
		if ad != bd || len(a) != len(b) || (len(a) == 1 && show(a[0].Root, true) != show(b[0].Root, true)) {
			t.Errorf("%q: split at the default and at 256 differ: %q %q", src, ad, bd)
		}
	}
}

// TestExplicitEntries covers the explicit entries ('?') goccy pairs
// otherwise than YAML 1.2 (01 req 8, 9, 10, 12; 11 req 17, 26; fifth WP-33
// review): each is RZ-CFG-001 with its message, split or not,
// at the entry's ':' when one comes and otherwise at the first offending
// token, while one-node keys parse. In a flow collection (blocker of the
// seventh WP-33 review) goccy pairs a '?' with a tag alone when the tag
// ends its line, reading the next line as the entry's value or a nested
// mapping ("k: {? !!str\n  x}" as {k: {"": x}}): a tag whose content is
// not on its line, or that goccy does not group with it, and a key of
// more than one node are RZ-CFG-001 at the offending node.
func TestExplicitEntries(t *testing.T) {
	tests := []struct{ src, want, msg string }{
		// A node at or left of the '?' column that is no new entry.
		{"? a\nb\n", "RZ-CFG-001@2:1", msgAfterExplicitKey},
		{"? a\n[x]\n", "RZ-CFG-001@2:1", msgAfterExplicitKey},
		{"- ? a\n  b\n", "RZ-CFG-001@2:3", msgAfterExplicitKey},
		{"k:\n  ? a\nb\n", "RZ-CFG-001@3:1", msgAfterExplicitKey},
		// An empty key.
		{"? \n", "RZ-CFG-001@1:1", msgEmptyExplicitKey},
		{"? \n: v\n", "RZ-CFG-001@2:1", msgEmptyExplicitKey},
		{"? \nz: 1\n", "RZ-CFG-001@1:1", msgEmptyExplicitKey},
		{"? # c\nz: 1\n", "RZ-CFG-001@1:1", msgEmptyExplicitKey},
		{"? \n!!str k: |\n", "RZ-CFG-001@1:1", msgEmptyExplicitKey},
		// A key of more than one node, or a collection.
		{"? a\n- b\n", "RZ-CFG-001@2:1", msgExplicitKey},
		{"? \n  k: \n", "RZ-CFG-001@2:4", msgExplicitKey},
		{"? \"b\"\n {k: v}\n", "RZ-CFG-001@2:2", msgExplicitKey},
		{"? ? a\n: b\n", "RZ-CFG-001@2:1", msgExplicitKey},
		{"? : v\n", "RZ-CFG-001@1:3", msgExplicitKey},
		// A tag goccy does not group with its content on the '?' line.
		{"? !!str {k: v}\n", "RZ-CFG-001@1:9", msgExplicitKey},
		{"? !!map a\n", "RZ-CFG-001@1:9", msgExplicitKey},
		{"? !!seq [a]\n: b\n", "RZ-CFG-001@2:1", msgExplicitKey},
		// One-node keys, and entries the next entry ends.
		{"? a\n? b\n", "{a:n,b:n}", ""},
		{"? a\nz: 1\n", "{a:n,z:i:1}", ""},
		{"- ? a\n- b\n", `[{a:n},s:"b"]`, ""},
		{"? !!int 1\n: v\n", `{1:s:"v"}`, ""},
		{"? !!str |\n  x\n: v\n", "{x\n:s:\"v\"}", ""},
		{"? a\n: b\n? c\n: d\n", `{a:s:"b",c:s:"d"}`, ""},
		{"k:\n  ? a\n  : b\nm: 1\n", `{k:{a:s:"b"},m:i:1}`, ""},
		{"? 'a'\n: - b\n", `{a:[s:"b"]}`, ""},
		{"? a\n--- \nb: 1\n", "{a:n} {b:i:1}", ""},
		// After an RZ-CFG-003 or RZ-CFG-004 the checks are skipped, so the
		// scan goes on collecting those findings (01 req 8).
		{"? &x a\n  b\nc: !e 1\n", "RZ-CFG-003@1:3 RZ-CFG-004@3:4", ""},
		// Explicit keys in flow collections.
		{"k: {? !!str\n  x}\n", "RZ-CFG-001@2:3", msgExplicitKey},
		{"k: {p: q, ? !!str\n  x}\n", "RZ-CFG-001@2:3", msgExplicitKey},
		{"k: {p: q,\n ? !!str\n  x,\n  r: s}\n", "RZ-CFG-001@3:3", msgExplicitKey},
		{"k: {p: q,\n ? !!str\n  x\n  r: s}\n", "RZ-CFG-001@3:3", msgExplicitKey},
		{"k: [p,\n ? !!str\n  x\n  r: s]\n", "RZ-CFG-001@3:3", msgExplicitKey},
		{"{p: q,\n? !!str\n  x\n  r: s}\n", "RZ-CFG-001@3:3", msgExplicitKey},
		{"k: {p: q,\n ? !!int\n  1}\n", "RZ-CFG-001@3:3", msgExplicitKey},
		{"k: {? !!str # c\n  x: v}\n", "RZ-CFG-001@2:3", msgExplicitKey},
		{"k: {? !!str {a: b}}\n", "RZ-CFG-001@1:13", msgExplicitKey},
		{"k: {? \"a\" \"b\": c}\n", "RZ-CFG-001@1:11", msgExplicitKey},
		{"k: {? [a] b: c}\n", "RZ-CFG-001@1:11", msgExplicitKey},
		{"k: {? ? a: b}\n", "RZ-CFG-001@1:9", msgExplicitKey},
		{"k: {? a: b}\nm: {? !e\n  x}\n", "RZ-CFG-004@2:7", ""},
		{"k: {? !!str x}\n", "{k:{x:n}}", ""},
		{"k: {p: q,\n ? x\n  r: s}\n", `{k:{p:s:"q",x r:s:"s"}}`, ""},
		{"k: {? !!str , a: 1}\n", "{k:{:n,a:i:1}}", ""},
		{"k: {? !!str\n  : v}\n", `{k:{:s:"v"}}`, ""},
		{"k: [? a: b, c]\n", `{k:[{a:s:"b"},s:"c"]}`, ""},
		{"k: {? !!int 1: a, ? 'b': c}\n", `{k:{1:s:"a",b:s:"c"}}`, ""},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, true)
			if err != nil {
				t.Fatal(err)
			}
			got := codes(diags)
			if got == "" {
				var trees []string
				for _, d := range docs {
					trees = append(trees, show(d.Root, false))
				}
				got = strings.Join(trees, " ")
			}
			if got != tt.want {
				t.Errorf("%q, split at %d: %q, want %q", tt.src, splitAt, got, tt.want)
			}
			if tt.msg != "" && (len(diags) != 1 || diags[0].Message != tt.msg) {
				t.Errorf("%q, split at %d: messages %+v, want %q", tt.src, splitAt, diags, tt.msg)
			}
		}
	}
}
