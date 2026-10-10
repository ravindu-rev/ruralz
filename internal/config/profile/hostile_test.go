// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goccy/go-yaml/ast"
	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// heapBudget is the peak heap growth allowed for an input of up to 1 MiB
// (01 test plan, FuzzProfileYAML: "heap growth under 256 MiB for inputs up
// to 1 MiB").
const heapBudget = 256 << 20

// billionLaughs is the alias-expansion shape of 11 req 15, sized to size
// bytes.
func billionLaughs(size int) string {
	var b strings.Builder
	b.WriteString("a: &a [\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\",\"lol\"]\n")
	prev := "a"
	for i := 0; b.Len() < size; i++ {
		name := "l" + strconv.Itoa(i)
		b.WriteString(name + ": &" + name + " [*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + ",*" + prev + "]\n")
		prev = name
	}
	return b.String()
}

// taggedLadder is the shape of the blocker of the third WP-33 review: at
// each of cols columns in turn, rows lines "kN: !!map", whose tag ends the
// line before a sibling key at the same column. goccy nested every such
// key under the tagged node before it, so the depth algorithm (01 req 9)
// counted one level per column while the tree grew one level per line:
// 63 columns of 255 lines, under the old split threshold of 256, parsed with
// no finding to a tree of depth 16,066 in 1,679 MiB, as goccy's recursion
// concatenates path strings. A last line "end: 1" gives the last tagged
// node a value. Keys, then a last comment, pad the input to size bytes;
// size 0 adds no padding.
func taggedLadder(cols, rows, size int) string {
	gen := func(pad string) string {
		var b strings.Builder
		for c := range cols {
			for r := range rows {
				b.WriteString(strings.Repeat(" ", c) + "k" + pad + itoa(c*rows+r) + ": !!map\n")
			}
		}
		// The last tagged node gets a value too.
		b.WriteString(strings.Repeat(" ", max(cols-1, 0)) + "end: 1\n")
		return b.String()
	}
	src := gen("")
	if n := cols * rows; size > len(src) && n > 0 {
		src = gen(strings.Repeat("x", (size-len(src))/n))
	}
	if size > len(src)+1 {
		src += "#" + strings.Repeat("x", size-len(src)-2) + "\n"
	}
	return src
}

// keyLadder is the shape of the blocker of the fourth WP-33 review:
// "a:\n", then reps repetitions of two lines: at column c, the given
// number of "- " indicators and an empty quoted scalar, then ":" at
// column 1. c starts at 3 and grows by 2*dashes each time.
// goccy reads each quoted scalar and the ':' under it as a multi-line
// implicit key, which YAML 1.2 forbids, and attaches the entry to the
// innermost open sequence. The tree then grows dashes+1 levels per
// repetition, while the depth algorithm (01 req 9) closes back to the ':'
// column. Before the fix, 16 dashes of 254 repetitions (1,038,101 bytes)
// parsed with no finding to depth 4,319, and 63 dashes (4,082,291 bytes)
// parsed to depth 16,257 with a 440 MiB peak heap. The 254 repetitions are
// historical: 255 gave the root mapping 256 entries, the split threshold of
// that review, and every block collection is split now.
func keyLadder(dashes, reps int) string {
	var b strings.Builder
	b.WriteString("a:\n")
	for r := range reps {
		c := 3 + 2*dashes*r
		b.WriteString(strings.Repeat(" ", c-1) + strings.Repeat("- ", dashes) + "\"\"\n:\n")
	}
	return b.String()
}

// explicitKeyLadder is the shape of the blocker of the fifth WP-33 review:
// reps repetitions of three lines: `? "a"` at column c, then at column c+2
// the given number of "- " indicators and `"b"`, then ":" at column c. c
// starts at 1 and grows by 2*dashes+4 each time; dashes < 0 gives
// repetition r 63-r dashes. goccy groups each ':' with the "b" before it,
// not with the '?', and nests that multi-line implicit key at its own
// column, while the depth algorithm (01 req 9) closed back to the '?'
// column and never counted more than 64. Before the fix, 31 dashes of 32
// repetitions (100,640 bytes) parsed with no finding to depth 1,056, and
// the variable ladder of 62 repetitions (515,468 bytes) to depth 2,139 in
// an 11 MiB peak heap.
func explicitKeyLadder(dashes, reps int) string {
	var b strings.Builder
	c := 1
	for r := range reps {
		k := dashes
		if k < 0 {
			k = 63 - r
		}
		b.WriteString(strings.Repeat(" ", c-1) + "? \"a\"\n" + strings.Repeat(" ", c+1) + strings.Repeat("- ", k) + "\"b\"\n" + strings.Repeat(" ", c-1) + ":\n")
		c += 2*k + 4
	}
	return b.String()
}

// longKeyFlow is the first shape of the path blocker of the sixth WP-33
// review: a key of keyLen bytes over a flow sequence of items+1 entries.
// goccy's parser builds every node's path from its parent's, so the key
// was copied into each entry's path: 16,384 of each (49,158 bytes) took a
// 298 MiB peak heap. Each value is now parsed on its own (split.go).
func longKeyFlow(keyLen, items int) string {
	return strings.Repeat("k", keyLen) + ": [" + strings.Repeat("1,", items) + "1]"
}

// longKeysNested is the second shape of that blocker, valid YAML 1.2 with
// every implicit key under 1,024 bytes: levels nested block mappings, each
// with a key of keyLen bytes, then a flow sequence of items+1 entries. 62
// levels of 1,000 bytes over 4,096 items (72,276 bytes) took 263 MiB.
func longKeysNested(levels, keyLen, items int) string {
	var b strings.Builder
	for d := range levels {
		b.WriteString(strings.Repeat(" ", d) + strings.Repeat("k", keyLen) + ":\n")
	}
	b.WriteString(strings.Repeat(" ", levels) + "v: [" + strings.Repeat("1,", items) + "1]\n")
	return b.String()
}

// longKeysInFlow nests the same shape in flow mappings, which goccy parses
// as one value: levels flow mappings with keys of keyLen bytes around a
// flow sequence of items+1 entries. The token pass bounds its paths
// (maxFlowPath).
func longKeysInFlow(levels, keyLen, items int) string {
	return "a: " + strings.Repeat("{"+strings.Repeat("k", keyLen)+": ", levels) + "[" + strings.Repeat("1,", items) + "1]" + strings.Repeat("}", levels) + "\n"
}

// keysWithoutValues is the shape of the null-insertion blocker of the
// sixth WP-33 review: a flow mapping of n+1 entries entry, such as "a" or
// "k:", that have no value. goccy inserts each one's implicit null into its
// token slice, moving every later token: 80,000 entries took 4 s and
// 160,000 took 35 s.
func keysWithoutValues(entry string, n int) string {
	return "a: {" + strings.Repeat(entry+",", n) + entry + "}"
}

// taggedEmpties is the shape of the tag-default blocker of the seventh
// WP-33 review: a flow sequence of n tagged empty nodes ("[!!str , ...]"),
// or with keys a flow mapping of n entries whose values are tagged empty
// nodes. goccy inserts each tag's default value into its token slice,
// moving every later token: 99,000 entries took 10 s.
func taggedEmpties(n int, keys bool) string {
	if !keys {
		return "a: [" + strings.Repeat("!!str ,", n) + "x]\n"
	}
	var b strings.Builder
	b.WriteString("a: {")
	for i := range n {
		b.WriteString("k" + itoa(i) + ": !!str ,")
	}
	b.WriteString("z: x}\n")
	return b.String()
}

// dedented is the shape of the indentation blocker of the seventh WP-33
// review: under "a:", body indented by two spaces, then " c: 2" at column
// 2, which returns to no enclosing collection. The token pass opened a
// sibling mapping there, and the split parse handed goccy the whole value:
// 125,000 entries "k: v" took 37 s.
func dedented(body string) string {
	return "a:\n  " + strings.ReplaceAll(strings.TrimSuffix(body, "\n"), "\n", "\n  ") + "\n c: 2\n"
}

// treeDepth returns the nesting depth of a tree: a collection is one more
// than its deepest child, so a document's root mapping is depth 1, as
// Options.MaxDepth counts it.
func treeDepth(n *tree.Node) int {
	if n == nil {
		return 0
	}
	d := 0
	for _, m := range n.Members {
		d = max(d, treeDepth(m.Value))
	}
	for _, it := range n.Items {
		d = max(d, treeDepth(it))
	}
	if n.Kind == tree.KindMap || n.Kind == tree.KindList {
		d++
	}
	return d
}

// TestHostileInputsBounded parses hostile inputs of up to 1 MiB within the
// heap budget, goroutine stacks included, and within 2 s each, with the
// expected findings, without expanding anything and without accepting a
// tree deeper than MaxDepth (11 req 15, 17, 26; 01 req 6, 9; 01 risk 1; 01
// test plan FuzzProfileYAML: heap growth under 256 MiB for inputs up to 1
// MiB, 2 s per input). The densest documents just under the token limit
// are parsed and accepted, which the heap budget then holds for (sixth
// WP-33 review); denser ones are refused before they are tokenized.
func TestHostileInputsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("parses several MiB")
	}
	if raceEnabled {
		// The race detector slows these single-goroutine parses twentyfold
		// and changes their allocations; the run without it measures them.
		t.Skip("measures time and memory, which the race detector distorts")
	}
	const mib = 1 << 20
	var wideMap, docs, wideMarker strings.Builder
	for wideMap.Len() < mib {
		wideMap.WriteString("k" + strconv.Itoa(wideMap.Len()) + ": v\n")
	}
	for i := 0; wideMarker.Len() < mib-16; i++ {
		wideMarker.WriteString("k" + strconv.Itoa(i) + ": v\n")
	}
	for docs.Len() < mib {
		docs.WriteString("a: 1\n---\n")
	}
	// Just under the token limit: one more of the repeated unit passes it.
	const limit = DefaultMaxDocumentTokens
	tests := []struct {
		name string
		src  string
		code string // first finding; "" for none
		// allocs bounds the bytes allocated, for the shapes whose cost
		// goes into short-lived garbage that the peak heap misses; 0 is
		// unbounded. The 2 s bound, for the shapes whose cost goes into
		// moving memory, which neither measure sees, holds for every row.
		allocs uint64
	}{
		{"billion laughs", billionLaughs(7 * mib / 8), codeAnchor, 0},
		{"billion laughs past the token limit", billionLaughs(mib), codeParse, 0},
		{"merge keys", strings.Repeat("- <<: {a: 1}\n", mib/26), codeAnchor, 0},
		{"brackets", strings.Repeat("[", mib), codeParse, 0},
		{"flow sequence", "a: [" + strings.Repeat("1,", mib/2-4) + "1]\n", codeParse, 0},
		{"block sequence", strings.Repeat("- a\n", limit/2-1), codeSchema, 0},
		{"block sequence past the token limit", strings.Repeat("- a\n", mib/4), codeParse, 0},
		// The densest shapes at the token limit, which took 340 to 485 MiB
		// at 1,000,000 tokens.
		{"one-character scalars at the token limit", "a: [" + strings.Repeat("1,", limit/2-3) + "1]\n", "", 0},
		{"empty flow collections at the token limit", "a: [" + strings.Repeat("[],", limit/3-2) + "1]\n", "", 0},
		{"single-pair mappings at the token limit", "a: [" + strings.Repeat("k: 1,", limit/4-2) + "1]\n", "", 0},
		{"empty entries at the token limit", "a:\n" + strings.Repeat("-\n", limit-4), "", 0},
		{"one-character scalars past the token limit", "a: [" + strings.Repeat("1,", 499990) + "1]\n", codeParse, 0},
		// Long keys above many nodes (path blocker of the sixth WP-33
		// review): block keys cost nothing now, and flow ones are bounded.
		{"long key over a flow sequence", longKeyFlow(16384, 16384), "", 64 << 20},
		{"long keys nested over a flow sequence", longKeysNested(62, 1000, 4096), "", 64 << 20},
		{"long keys nested over a long flow sequence", longKeysNested(62, 1000, 16384), "", 64 << 20},
		{"long keys in flow mappings", longKeysInFlow(60, 1000, 16384), codeParse, 64 << 20},
		// Flow mapping entries without a value (null-insertion blocker of
		// the sixth WP-33 review).
		{"keys without values", keysWithoutValues("a", 400000), codeParse, 0},
		{"keys and colons without values", keysWithoutValues("k:", 400000), codeParse, 0},
		{"keys without values under the token limit", keysWithoutValues("a", 150000), codeParse, 0},
		{"keys and colons without values under the token limit", keysWithoutValues("k:", 100000), codeParse, 0},
		{"wide mapping", wideMap.String(), "", 0},
		{"documents", docs.String(), "", 0},
		{"long scalar", "a: " + strings.Repeat("x", mib) + "\n", "", 0},
		{"deep block", nest("block map", 65) + strings.Repeat("#\n", mib/2), codeParse, 0},
		{"comments", strings.Repeat("# c\n", mib/4), "", 0},
		{"dashes in a flow sequence", "a: [" + strings.Repeat("- ", mib/2) + "x]\n", codeParse, 0},
		{"dashes in a flow mapping", "a: {k: " + strings.Repeat("- ", mib/2) + "x}\n", codeParse, 0},
		{"tag chain", "a: " + strings.Repeat("!!str ", mib/6) + "x\n", codeParse, 0},
		{"tagged block scalar", longTaggedScalar("a: ", "!!str |", mib/2, mib/2), "", 512 << 20},
		{"tagged block scalar entry", longTaggedScalar("- ", "!!str >", mib/2, mib/2), codeSchema, 512 << 20},
		// A comment after the blank run ends the scalar (finding 1 of the
		// second WP-33 review): goccy's clipping would run for an hour.
		{"block scalar before a comment", longBlockScalar("a: ", "|", mib/2, mib/2, "# c\nb: 1\n"), "", 512 << 20},
		{"block scalar entry before a comment", longBlockScalar("- ", "|", mib/2, mib/2, " # c\n"), codeSchema, 512 << 20},
		// Empty tagged mappings before sibling keys (blocker of the third
		// WP-33 review): stopped at the first tag, never nested 16,066
		// deep.
		{"tagged empty mappings", taggedLadder(63, 255, mib), codeParse, 64 << 20},
		// Implicit keys over two lines (blocker of the fourth WP-33
		// review): stopped at the first ':', never nested 4,319 deep.
		{"implicit keys over two lines", keyLadder(16, 254), codeParse, 64 << 20},
		// Explicit keys whose ':' goccy groups with a later node (blocker
		// of the fifth WP-33 review): stopped at the first ':', never
		// nested 1,056 or 2,139 deep.
		{"explicit keys over several lines", explicitKeyLadder(31, 32), codeParse, 64 << 20},
		{"explicit keys of varying width", explicitKeyLadder(-1, 62), codeParse, 64 << 20},
		// Document markers goccy reads where the document cut does not
		// (blocker of the seventh WP-33 review): the whole document went
		// to goccy's parse, 38 s for 1 MiB, or 1 GiB of paths.
		{"wide mapping before \"...x\"", wideMarker.String() + "...x: 1\n", codeParse, 256 << 20},
		{"wide mapping before \"... x\"", wideMarker.String() + "... x: 1\n", codeParse, 256 << 20},
		{"\"...#\" before a wide mapping", "...#\n" + wideMap.String(), codeParse, 256 << 20},
		{"\"...!!map\" before a wide mapping", "...!!map\n" + wideMap.String(), codeParse, 256 << 20},
		{"wide mapping before \"... {}\"", wideMarker.String() + "... {}\n", codeParse, 256 << 20},
		{"long keys nested before \"...x\"", longKeysNested(62, 1000, 16384) + "...x: 1\n", codeParse, 256 << 20},
		{"\"...#\" before long keys nested", "...#\n" + longKeysNested(62, 1000, 16384), codeParse, 256 << 20},
		{"\"...#\" before a long key flow", "...#\n" + longKeyFlow(16384, 16384), codeParse, 256 << 20},
		{"empty entries before \"...x\"", strings.Repeat("k:\n-\n", 10000) + "...x: 1\n", codeParse, 256 << 20},
		// Entries indented between two collections' columns (blocker of
		// the seventh WP-33 review): the whole value went to goccy's
		// parse, 37 s for 875 KB, or 1 GiB of paths.
		{"dedented entry after a wide mapping", dedented(strings.Repeat("k: v\n", 125000)), codeParse, 256 << 20},
		{"dedented entry after empty values", dedented(strings.Repeat("k:\n", 40000)), codeParse, 256 << 20},
		{"dedented entry at the root", strings.Repeat("  k: v\n", 40000) + "b: 2\n", codeParse, 256 << 20},
		{"dedented entry after empty entries", dedented(strings.Repeat("k:\n-\n", 10000)), codeParse, 256 << 20},
		{"dedented entry after long keys", dedented(longKeysNested(61, 1000, 16384)), codeParse, 256 << 20},
		// Tagged empty nodes in flow collections (blocker of the seventh
		// WP-33 review): goccy's inserted default values took 10 s.
		{"tagged empty nodes in a sequence", taggedEmpties(99000, false), codeParse, 0},
		{"tagged empty values in a mapping", taggedEmpties(64000, true), codeParse, 0},
		{"tagged empty values of one key", "a: {" + strings.Repeat("k: !!str ,", 60000) + "z: x}\n", codeParse, 0},
		{"tagged empty nodes before a closing bracket", "a: [" + strings.Repeat("!!str ,", 80000) + "!!str ]\n", codeParse, 0},
		// A flow collection left open is bounded as a closed one: goccy
		// inserted its nulls before it failed at the end, 31 s.
		{"keys without values left open", "a: {" + strings.Repeat("a,", 150000) + "\n", codeParse, 0},
		// Tabs, which make the token pass locate every token in the
		// source (columns.go), in linear time.
		{"tabs between flow entries", "a: [" + strings.Repeat("1,\t", limit/2-3) + "1]\n", "", 0},
		{"tabs in a wide mapping", strings.Repeat("k\tk:\tv\t# c\n", limit/10), codeDuplicate, 0},
		{"tabs after tags", "a: [" + strings.Repeat("!!str\tx,", limit/8) + "1]\n", "", 0},
		// Flow entries goccy nests by columns or parses without ','
		// (blocker of the eighth WP-33 review): a ladder of 1 MiB took a
		// 567 MiB peak heap and 62 levels 3,747 MiB and 16 s, all past the
		// token pass and refused only by the converter's depth bound, and
		// 80,000 comma-less entries took 17.9 s.
		{"flow ladder of long keys", flowLadder(1, 836, 836), codeParse, 64 << 20},
		{"flow ladders nested", flowLadder(62, 100, 100), codeParse, 64 << 20},
		{"comma-less flow entries", commaLess(80000, func(i int) string { return "k" + itoa(i) + ": v" }, ""), codeParse, 128 << 20},
		{"comma-less flow keys without values", commaLess(80000, func(i int) string { return "k" + itoa(i) + ":" }, "z: v"), codeParse, 128 << 20},
		{"comma-less duplicate flow entries", commaLess(40000, func(int) string { return "k: v" }, ""), codeParse, 128 << 20},
		// Flow pairs nested to the token pass's limit, which goccy nests two
		// levels each: the converter's depth bound refuses the tree, and
		// goccy's parse of it costs little (finding of the eighth review).
		{"flow pairs at the depth limit", "a: " + strings.Repeat("[k: ", 63) + "v" + strings.Repeat("]", 63) + "\n", codeParse, 0},
		{"flow pairs of long keys at the depth limit", "a: " + strings.Repeat("["+strings.Repeat("k", 1000)+": ", 63) + "v" + strings.Repeat("]", 63) + "\n", codeParse, 64 << 20},
		// Documents whose trees stay live while a later one is parsed
		// (minor finding of the eighth WP-33 review): three documents of
		// 348,000 tokens peaked at up to 276 MiB and pass the file's token
		// bound (maxFileTokens); a block document and a document of single
		// pairs at the document limit fill it.
		{"documents at the token limit", strings.Repeat("---\na: ["+strings.Repeat("1,", 174000)+"1]\n", 3), codeParse, 0},
		{"documents at the file token limit", "---\na:\n" + strings.Repeat("-\n", limit/4-5) + "---\na: [" + strings.Repeat("k: 1,", limit/4-3) + "1]\n", "", 0},
	}
	for _, tt := range tests {
		var diags string
		var docs []Document
		start := time.Now()
		before := allocBytes()
		peak := peakHeap(func() {
			var d diag.List
			var err error
			docs, d, err = Parse(context.Background(), []byte(tt.src), 0, "f", FormatYAML, Options{})
			if err != nil {
				t.Error(err)
			}
			if len(d) > 0 {
				diags = d[0].Code
			}
		})
		if diags != tt.code {
			t.Errorf("%s: first finding %q, want %q", tt.name, diags, tt.code)
		}
		// No accepted tree is deeper than MaxDepth (01 req 9).
		for i, d := range docs {
			if depth := treeDepth(d.Root); depth > DefaultMaxDepth {
				t.Errorf("%s: document %d has depth %d, over %d", tt.name, i, depth, DefaultMaxDepth)
			}
		}
		allocs := allocBytes() - before
		if peak > heapBudget {
			t.Errorf("%s (%d bytes): peak heap growth %d MiB, over %d MiB", tt.name, len(tt.src), peak>>20, heapBudget>>20)
		}
		if tt.allocs > 0 && allocs > tt.allocs {
			t.Errorf("%s (%d bytes): allocates %d MiB, over %d MiB", tt.name, len(tt.src), allocs>>20, tt.allocs>>20)
		}
		if took := time.Since(start); took > 2*time.Second {
			t.Errorf("%s (%d bytes): took %v, over 2 s", tt.name, len(tt.src), took)
		}
		t.Logf("%s: %d bytes, peak heap growth %d MiB, %d MiB allocated, %v", tt.name, len(tt.src), peak>>20, allocs>>20, time.Since(start).Round(time.Millisecond))
	}
}

// TestConverterDepthBound covers the second depth bound (01 req 9; 11 req
// 17, 26; blocker of the fifth WP-33 review): the converter counts the
// depth of the tree it builds and fails the document with RZ-CFG-001 at
// the first collection deeper than MaxDepth, in goccy's whole parse and the
// split parse alike, so no tree deeper than MaxDepth is returned even
// where the token pass undercounts goccy's nesting. The token pass is weakened here
// (passDepth) or bypassed, so these trees reach the converter.
func TestConverterDepthBound(t *testing.T) {
	var wide strings.Builder
	for i := range 300 {
		wide.WriteString("w" + itoa(i) + ": v\n")
	}
	indent := func(s string) string {
		return "  " + strings.ReplaceAll(strings.TrimSuffix(s, "\n"), "\n", "\n  ") + "\n"
	}
	const tooDeep = "nesting depth exceeds 64"
	tests := []struct{ name, src, want string }{
		{"block mappings", nest("block map", 65), "RZ-CFG-001@65:129"},
		{"block sequences", nest("block seq", 65), "RZ-CFG-001@65:129"},
		{"mappings and sequences", nest("mixed", 65), "RZ-CFG-001@65:129"},
		{"flow sequences", nest("flow", 65), "RZ-CFG-001@1:67"},
		{"compact sequences", nest("compact seq", 65), "RZ-CFG-001@1:129"},
		{"tagged mapping", "a: !!map\n" + indent(nest("block map", 64)), "RZ-CFG-001@65:129"},
		// A too deep entry before its siblings: nothing after it is
		// converted, so siblings that would report findings report none
		// (minor finding of the eighth WP-33 review).
		{"before a sibling", "a:\n" + indent(nest("block seq", 64)) + "  - x\nb: 1\n", "RZ-CFG-001@65:129"},
		{"before siblings with findings", "a:\n" + indent(nest("block seq", 64)) + "  - .inf\na: 1\nb: !!int x\n", "RZ-CFG-001@65:129"},
		{"before flow siblings with findings", "a: " + strings.Repeat("[", 64) + strings.Repeat("]", 64) + "\nb: [.inf]\na: 1\n", "RZ-CFG-001@1:67"},
		{"inside a flow sequence before siblings", "a: [" + strings.Repeat("[", 63) + strings.Repeat("]", 63) + ", .inf, {k: 1, k: 2}]\n", "RZ-CFG-001@1:67"},
		{"inside a wide mapping", wide.String() + "x:\n" + indent(nest("block map", 64)), "RZ-CFG-001@365:129"},
		{"two documents", nest("block map", 65) + "---\nb: 1\n", "RZ-CFG-001@65:129"},
		{"at the limit", nest("block map", 64), ""},
		{"wide at the limit", wide.String() + "x:\n" + indent(nest("block map", 63)), ""},
	}
	for _, tt := range tests {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt, passDepth: 1 << 20}, false)
			if err != nil {
				t.Fatal(err)
			}
			if got := codes(diags); got != tt.want {
				t.Errorf("%s, split at %d: %q, want %q", tt.name, splitAt, got, tt.want)
				continue
			}
			for _, d := range diags {
				if d.Message != tooDeep {
					t.Errorf("%s, split at %d: message %q", tt.name, splitAt, d.Message)
				}
			}
			for _, d := range docs {
				if depth := treeDepth(d.Root); depth > DefaultMaxDepth || (tt.want != "" && depth > 1) {
					t.Errorf("%s, split at %d: document of depth %d returned", tt.name, splitAt, depth)
				}
			}
			if tt.want == "" && (len(docs) != 1 || treeDepth(docs[0].Root) != DefaultMaxDepth) {
				t.Errorf("%s, split at %d: %d documents", tt.name, splitAt, len(docs))
			}
		}
		// At full strength the token pass stops the same input first.
		if _, diags := parseYAML(t, tt.src); (len(diags) == 0) != (tt.want == "") {
			t.Errorf("%s: token pass %q", tt.name, codes(diags))
		}
	}
	// More sibling collections than MaxDepth, each closed before the next
	// opens, never add up to a depth (minor finding of the eighth WP-33
	// review): the race-enabled run checks this too.
	var flowSeqs, flowMaps, blockSeqs strings.Builder
	for i := range 3 * DefaultMaxDepth {
		flowSeqs.WriteString("[" + itoa(i) + "],")
		flowMaps.WriteString("{k: " + itoa(i) + "},")
		blockSeqs.WriteString("- [" + itoa(i) + "]\n")
	}
	for _, src := range []string{"a: [" + flowSeqs.String() + "x]\n", "a: [" + flowMaps.String() + "x]\n", "a:\n" + blockSeqs.String()} {
		for _, splitAt := range []int{-1, 0, 3} {
			docs, diags, err := parse(t.Context(), []byte(src), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(diags) != 0 || len(docs) != 1 || treeDepth(docs[0].Root) != 3 {
				t.Errorf("%.40q, split at %d: %q, %d documents", src, splitAt, codes(diags), len(docs))
			}
		}
	}
	// Flow pairs: "[k: v]" is a sequence holding the mapping {k: v}, a
	// level the token pass does not count, so 40 nested pairs pass it at
	// 41 levels while the tree is 81 deep. The converter stops them at
	// full strength, whatever the mapping's width.
	pairs := "a: " + strings.Repeat("[k: ", 40) + "v" + strings.Repeat("]", 40) + "\n"
	for _, splitAt := range []int{-1, 0, 3} {
		docs, diags, err := parse(t.Context(), []byte(pairs), 0, "f.yaml", FormatYAML, Options{splitAt: splitAt}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(docs) != 0 || codes(diags) != "RZ-CFG-001@1:129" || diags[0].Message != tooDeep {
			t.Errorf("flow pairs, split at %d: %d documents, %q", splitAt, len(docs), codes(diags))
		}
	}
	// The ladders of the fourth and fifth reviews, which the token pass
	// undercounted before their fixes, go straight to goccy's parse and
	// the converter, without the token pass.
	for _, src := range []string{keyLadder(16, 20), explicitKeyLadder(31, 4), explicitKeyLadder(-1, 8)} {
		p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
		var s scanner.Scanner
		s.Init(src)
		var tks token.Tokens
		for {
			sub, err := s.Scan()
			tks.Add(sub...)
			if err != nil || len(sub) == 0 {
				break
			}
		}
		p.parseSegment(tks, segment{text: src, startLine: 1}, nil, columnsOf(tks, src), nil)
		if len(p.docs) != 0 || len(p.diags) != 1 || p.diags[0].Code != codeParse || p.diags[0].Message != tooDeep {
			t.Errorf("ladder of %d bytes: %d documents, %q", len(src), len(p.docs), codes(p.diags))
		}
	}
}

// TestConverterUnexpectedNodes covers nodes goccy v1.19.2's parser never
// returns as a value (minor finding of the eighth WP-33 review): a bare
// mapping value, which goccy keeps inside its MappingNode, and a scalar
// without a token. The converter refuses each with RZ-CFG-001 instead of
// reading it as a mapping or typing its ':' as a string.
func TestConverterUnexpectedNodes(t *testing.T) {
	p := &fileParser{path: "f.yaml", opts: Options{}.withDefaults()}
	c := converter{p: p}
	colon := &token.Token{Type: token.MappingValueType, Value: ":", Position: &token.Position{Line: 2, Column: 3}}
	if n := c.node(&ast.MappingValueNode{BaseNode: &ast.BaseNode{}, Start: colon}); n.Kind != tree.KindNull || !c.failed {
		t.Errorf("mapping value: %+v", n)
	}
	if n := c.node(&ast.StringNode{BaseNode: &ast.BaseNode{}}); n.Kind != tree.KindNull {
		t.Errorf("scalar without a token: %+v", n)
	}
	if got := codes(p.diags); got != "RZ-CFG-001@2:3 RZ-CFG-001@0:0" || p.diags[0].Message != "syntax error: unexpected mapping value" {
		t.Errorf("diagnostics %q %+v", got, p.diags)
	}
}
