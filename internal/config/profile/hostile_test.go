// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// heapBudget is the peak heap growth allowed for an input of up to 1 MiB.
const heapBudget = 256 << 20

// numbered returns the n lines that line(i) gives for i from 0.
func numbered(n int, line func(i int) string) string {
	var b strings.Builder
	for i := range n {
		b.WriteString(line(i))
	}
	return b.String()
}

// TestHostileInputsBounded parses hostile inputs within the heap budget,
// goroutine stacks included, with the expected first finding, without
// expanding anything and without accepting a tree deeper than MaxDepth.
// Every row also allocates at most a fixed multiple of its
// size and of the diagnostics cap, so a parse whose cost goes into
// short-lived garbage, which the peak heap misses, shows up too. The
// densest documents at the node limit are accepted and one more node is
// refused; those inputs of 2 to 3 MiB stay within the budget too.
func TestHostileInputsBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("parses several MiB")
	}
	if raceEnabled {
		// The race detector slows these single-goroutine parses twentyfold
		// and changes their allocations; the run without it measures them.
		t.Skip("measures memory, which the race detector distorts")
	}
	const mib = 1 << 20
	var wideMap, docs, wideMarker, routes strings.Builder
	for wideMap.Len() < mib {
		wideMap.WriteString("k" + strconv.Itoa(wideMap.Len()) + ": v\n")
	}
	for i := 0; wideMarker.Len() < mib-16; i++ {
		wideMarker.WriteString("k" + strconv.Itoa(i) + ": v\n")
	}
	for docs.Len() < mib {
		docs.WriteString("a: 1\n---\n")
	}
	for i := 0; routes.Len() < 3*mib; i++ {
		routes.WriteString("---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: r" + strconv.Itoa(i) + "\nspec:\n  hosts: [a.example]\n")
	}
	// The node limit: the rows "at" it hold exactly that many nodes, the
	// rows "past" it one more.
	const limit = DefaultMaxDocumentTokens
	tests := []struct {
		name string
		src  string
		code string // first finding; "" for none
	}{
		{"billion laughs", billionLaughs(7 * mib / 8), codeAnchor},
		{"billion laughs of 1 MiB", billionLaughs(mib), codeAnchor},
		{"merge keys", strings.Repeat("- <<: {a: 1}\n", mib/26), codeAnchor},
		{"brackets", strings.Repeat("[", mib), codeParse},
		{"flow sequence", "a: [" + strings.Repeat("1,", mib/2-4) + "1]\n", ""},
		{"block sequence", strings.Repeat("- a\n", mib/4), codeSchema},
		// The densest documents at the node limit: the root, the key a and
		// the sequence are three nodes.
		{"one-character scalars at the node limit", "a: [" + strings.Repeat("1,", limit-4) + "1]\n", ""},
		{"one-character scalars past the node limit", "a: [" + strings.Repeat("1,", limit-3) + "1]\n", codeParse},
		{"empty flow collections at the node limit", "a: [" + strings.Repeat("[],", limit-4) + "1]\n", ""},
		{"single-pair mappings at the node limit", "a: [" + strings.Repeat("k: 1,", (limit-4)/3) + "1]\n", ""},
		{"empty entries at the node limit", "a:\n" + strings.Repeat("-\n", limit-3), ""},
		{"empty entries past the node limit", "a:\n" + strings.Repeat("-\n", limit-2), codeParse},
		// Documents whose trees stay live while a later one is parsed:
		// the file's documents hold at most five fourths of the limit.
		{"documents past the file node limit", strings.Repeat("---\na: ["+strings.Repeat("1,", 420000)+"1]\n", 3), codeParse},
		{"documents at the file node limit", "---\na:\n" + strings.Repeat("-\n", limit-3) + "---\na: [" + strings.Repeat("1,", limit/4-4) + "1]\n", ""},
		{"Routes of 3 MiB", routes.String(), ""},
		// Long keys above many nodes. An implicit key holds at most 1,024
		// characters; a flow mapping's key has no such limit.
		{"long key over a flow sequence", longKeyFlow(16384, 16384), codeParse},
		{"long flow mapping key over a flow sequence", "a: {" + strings.Repeat("k", 16384) + ": [" + strings.Repeat("1,", 16384) + "1]}\n", ""},
		{"long keys nested over a flow sequence", longKeysNested(62, 1000, 4096), ""},
		{"long keys nested over a long flow sequence", longKeysNested(62, 1000, 16384), ""},
		{"long keys nested at the key limit", longKeysNested(62, 1024, 16384), ""},
		{"long keys nested past the key limit", longKeysNested(62, 1025, 16384), codeParse},
		{"long keys in flow mappings", longKeysInFlow(60, 1000, 16384), ""},
		// Flow mapping entries without a value.
		{"keys without values", keysWithoutValues("a", 400000), codeDuplicate},
		{"keys and colons without values", keysWithoutValues("k:", 400000), codeDuplicate},
		{"keys without values under the diagnostics cap", keysWithoutValues("a", DefaultMaxDiagnostics-1), codeDuplicate},
		{"wide mapping", wideMap.String(), ""},
		{"documents", docs.String(), ""},
		{"long scalar", "a: " + strings.Repeat("x", mib) + "\n", ""},
		{"long quoted scalar", "a: \"" + strings.Repeat("x\\t", mib/3) + "\"\n", ""},
		{"long folded lines", "a: >\n" + strings.Repeat("  x\n", mib/4), ""},
		{"deep block", nest("block map", 65) + strings.Repeat("#\n", mib/2), codeParse},
		{"comments", strings.Repeat("# c\n", mib/4), ""},
		{"empty lines", strings.Repeat("\n", mib), ""},
		{"white lines", strings.Repeat(" \t \n", mib/4), ""},
		{"dashes in a flow sequence", "a: [" + strings.Repeat("- ", mib/2) + "x]\n", codeParse},
		{"dashes in a flow mapping", "a: {k: " + strings.Repeat("- ", mib/2) + "x}\n", codeParse},
		{"tag chain", "a: " + strings.Repeat("!!str ", mib/6) + "x\n", codeParse},
		{"tagged block scalar", longTaggedScalar("a: ", "!!str |", mib/2, mib/2), ""},
		{"tagged block scalar entry", longTaggedScalar("- ", "!!str >", mib/2, mib/2), codeSchema},
		{"block scalar before a comment", longBlockScalar("a: ", "|", mib/2, mib/2, "# c\nb: 1\n"), ""},
		{"block scalar entry before a comment", longBlockScalar("- ", "|", mib/2, mib/2, " # c\n"), codeSchema},
		{"block scalars after empty lines", strings.Repeat("a: |\n\n\n\n  x\n", mib/16), codeDuplicate},
		{"tab lines after block scalars", strings.Repeat("k: |\n  x\n\t# c\n", mib/14) + "z: 1\n", codeParse},
		{"comment lines after a block scalar", "k: |\n  x\n\t# c\n" + strings.Repeat("# c\n", mib/4), ""},
		// Empty tagged mappings before sibling keys: one finding each, up
		// to the diagnostics cap.
		{"tagged empty mappings", taggedLadder(63, 255, mib), codeParse},
		// Implicit keys over two lines, and explicit keys over several.
		{"implicit keys over two lines", keyLadder(16, 254), codeDuplicate},
		{"explicit keys over several lines", explicitKeyLadder(31, 32), codeParse},
		{"explicit keys of varying width", explicitKeyLadder(-1, 62), codeParse},
		// Document markers after content; "...x" and "...#" are no
		// markers but plain scalars, a key and a second root node.
		{"wide mapping before \"...x\"", wideMarker.String() + "...x: 1\n", ""},
		{"wide mapping before \"... x\"", wideMarker.String() + "... x: 1\n", codeParse},
		{"\"...#\" before a wide mapping", "...#\n" + wideMap.String(), codeParse},
		{"\"...!!map\" before a wide mapping", "...!!map\n" + wideMap.String(), codeParse},
		{"wide mapping before \"... {}\"", wideMarker.String() + "... {}\n", codeParse},
		{"long keys nested before \"...x\"", longKeysNested(62, 1000, 16384) + "...x: 1\n", ""},
		{"\"...#\" before long keys nested", "...#\n" + longKeysNested(62, 1000, 16384), codeParse},
		{"empty entries before \"...x\"", numbered(10000, func(i int) string { return "k" + itoa(i) + ":\n-\n" }) + "...x: 1\n", ""},
		{"empty entries before \"... x\"", numbered(10000, func(i int) string { return "k" + itoa(i) + ":\n-\n" }) + "... x: 1\n", codeParse},
		{"duplicate empty entries", strings.Repeat("k:\n-\n", 10000), codeDuplicate},
		// Entries indented between two collections' columns.
		{"dedented entry after a wide mapping", dedented(numbered(125000, func(i int) string { return "k" + itoa(i) + ": v\n" })), codeParse},
		{"dedented entry after empty values", dedented(numbered(40000, func(i int) string { return "k" + itoa(i) + ":\n" })), codeParse},
		{"dedented entry at the root", numbered(40000, func(i int) string { return "  k" + itoa(i) + ": v\n" }) + "b: 2\n", codeParse},
		{"dedented entry after empty entries", dedented(numbered(10000, func(i int) string { return "k" + itoa(i) + ":\n-\n" })), codeParse},
		{"dedented entry after long keys", dedented(longKeysNested(61, 1000, 16384)), codeParse},
		{"dedented duplicate entries", dedented(strings.Repeat("k: v\n", 125000)), codeDuplicate},
		// Tagged empty nodes in flow collections.
		{"tagged empty nodes in a sequence", taggedEmpties(99000, false), ""},
		{"tagged empty values in a mapping", taggedEmpties(64000, true), ""},
		{"tagged empty values of one key", "a: {" + strings.Repeat("k: !!str ,", 60000) + "z: x}\n", codeDuplicate},
		{"tagged empty nodes before a closing bracket", "a: [" + strings.Repeat("!!str ,", 80000) + "!!str ]\n", ""},
		{"keys without values left open", "a: {" + strings.Repeat("a,", 150000) + "\n", codeDuplicate},
		// Tabs in linear time.
		{"tabs between flow entries", "a: [" + strings.Repeat("1,\t", mib/3) + "1]\n", ""},
		{"tabs in a wide mapping", strings.Repeat("k\tk:\tv\t# c\n", mib/10), codeDuplicate},
		{"tabs after tags", "a: [" + strings.Repeat("!!str\tx,", mib/8) + "1]\n", ""},
		// Flow entries as ladders or without ','.
		{"flow ladder of long keys", flowLadder(1, 836, 836), codeParse},
		{"flow ladders nested", flowLadder(62, 100, 100), codeParse},
		{"comma-less flow entries", commaLess(80000, func(i int) string { return " k" + itoa(i) + ": v" }, ""), codeParse},
		{"comma-less flow keys without values", commaLess(80000, func(i int) string { return " k" + itoa(i) + ":" }, " z: v"), codeParse},
		{"flow pairs at the depth limit", "a: " + strings.Repeat("[k: ", 31) + "v" + strings.Repeat("]", 31) + "\n", ""},
		{"flow pairs past the depth limit", "a: " + strings.Repeat("[k: ", 32) + "v" + strings.Repeat("]", 32) + "\n", codeParse},
		{"flow pairs of long keys", "a: " + strings.Repeat("["+strings.Repeat("k", 1000)+": ", 31) + "v" + strings.Repeat("]", 31) + "\n", ""},
		{"long implicit keys", strings.Repeat(strings.Repeat("k", 1024)+": v\n", mib/1029), codeDuplicate},
		// Directive lines holding flow indicators and tags, before and
		// between documents.
		{"reserved directives", strings.Repeat("%FOO { !!map [\n", mib/15) + "---\na: 1\n", ""},
		{"reserved directive before tag lines", "%FOO {\n---\n" + strings.Repeat("!!map\n", mib/6), codeParse},
		{"documents after reserved directives", strings.Repeat("%FOO {\n---\n!!map {\n...\n", mib/24), codeParse},
		{"percent lines in a document", "a:\n" + strings.Repeat("%FOO {\n", mib/7), codeParse},
	}
	for _, tt := range tests {
		var first string
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
				first = d[0].Code
			}
		})
		took := time.Since(start)
		allocs := allocBytes() - before
		if first != tt.code {
			t.Errorf("%s: first finding %q, want %q", tt.name, first, tt.code)
		}
		// No accepted tree is deeper than MaxDepth.
		for i, d := range docs {
			if depth := treeDepth(d.Root); depth > DefaultMaxDepth {
				t.Errorf("%s: document %d has depth %d, over %d", tt.name, i, depth, DefaultMaxDepth)
			}
		}
		if peak > heapBudget {
			t.Errorf("%s (%d bytes): peak heap growth %d MiB, over %d MiB", tt.name, len(tt.src), peak>>20, heapBudget>>20)
		}
		// The findings, at most MaxDiagnostics+1, take up to 16 MiB more.
		if limit := 192*uint64(len(tt.src)) + 16<<20; allocs > limit {
			t.Errorf("%s (%d bytes): allocates %d MiB, over %d MiB", tt.name, len(tt.src), allocs>>20, limit>>20)
		}
		t.Logf("%s: %d bytes, peak heap growth %d MiB, %d MiB allocated, %v", tt.name, len(tt.src), peak>>20, allocs>>20, took.Round(time.Millisecond))
	}
}

// TestDepthBound covers the depth bound: the first collection deeper than
// MaxDepth is RZ-CFG-001 at its start and stops the file, so nothing after
// it is reported, inside a wide mapping and after a tag alike, a flow pair
// counting as the mapping it is; and sibling collections, each closed
// before the next opens, never add up to a depth.
func TestDepthBound(t *testing.T) {
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
		{"before a sibling", "a:\n" + indent(nest("block seq", 64)) + "  - x\nb: 1\n", "RZ-CFG-001@65:129"},
		{"before siblings with findings", "a:\n" + indent(nest("block seq", 64)) + "  - .inf\na: 1\nb: !!int x\n", "RZ-CFG-001@65:129"},
		{"before flow siblings with findings", "a: " + strings.Repeat("[", 64) + strings.Repeat("]", 64) + "\nb: [.inf]\na: 1\n", "RZ-CFG-001@1:67"},
		{"inside a flow sequence before siblings", "a: [" + strings.Repeat("[", 63) + strings.Repeat("]", 63) + ", .inf, {k: 1, k: 2}]\n", "RZ-CFG-001@1:67"},
		// The fatal finding drops the value finding, as the anchor would.
		{"after findings", "a: .inf\nb: &x 1\nc:\n" + indent(nest("block map", 64)), "RZ-CFG-003@2:4 RZ-CFG-001@67:129"},
		{"after a value finding", "a: .inf\nc:\n" + indent(nest("block map", 64)), "RZ-CFG-001@66:129"},
		{"inside a wide mapping", wide.String() + "x:\n" + indent(nest("block map", 64)), "RZ-CFG-001@365:129"},
		{"two documents", nest("block map", 65) + "---\nb: &x 1\n", "RZ-CFG-001@65:129"},
		{"flow pairs", "a: " + strings.Repeat("[k: ", 40) + "v" + strings.Repeat("]", 40) + "\n", "RZ-CFG-001@1:129"},
		{"at the limit", nest("block map", 64), ""},
		{"wide at the limit", wide.String() + "x:\n" + indent(nest("block map", 63)), ""},
	}
	for _, tt := range tests {
		docs, diags, err := parse(t.Context(), []byte(tt.src), 0, "f.yaml", FormatYAML, Options{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if got := codes(diags); got != tt.want {
			t.Errorf("%s: %q, want %q", tt.name, got, tt.want)
			continue
		}
		if tt.want != "" && (len(docs) != 0 || diags[len(diags)-1].Message != tooDeep) {
			t.Errorf("%s: %d documents, last message %q", tt.name, len(docs), diags[len(diags)-1].Message)
		}
		if tt.want == "" && (len(docs) != 1 || treeDepth(docs[0].Root) != DefaultMaxDepth) {
			t.Errorf("%s: %d documents", tt.name, len(docs))
		}
	}
	var flowSeqs, flowMaps, blockSeqs strings.Builder
	for i := range 3 * DefaultMaxDepth {
		flowSeqs.WriteString("[" + itoa(i) + "],")
		flowMaps.WriteString("{k: " + itoa(i) + "},")
		blockSeqs.WriteString("- [" + itoa(i) + "]\n")
	}
	for _, src := range []string{"a: [" + flowSeqs.String() + "x]\n", "a: [" + flowMaps.String() + "x]\n", "a:\n" + blockSeqs.String()} {
		docs, diags, err := parse(t.Context(), []byte(src), 0, "f.yaml", FormatYAML, Options{}, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(diags) != 0 || len(docs) != 1 || treeDepth(docs[0].Root) != 3 {
			t.Errorf("%.40q: %q, %d documents", src, codes(diags), len(docs))
		}
	}
}
