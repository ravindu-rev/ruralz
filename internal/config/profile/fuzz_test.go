// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/goccy/go-yaml/scanner"
	"github.com/goccy/go-yaml/token"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// hostileSeeds are small instances of the hostile shapes (11 req 15, 17).
func hostileSeeds() []string {
	return []string{
		"a: &a [x, x]\nb: &b [*a, *a]\nc: [*b, *b]\n",
		"base: &base {a: 1}\nderived:\n  <<: *base\n",
		"a: !include other.yaml\nb: !env HOME\n",
		"%TAG !e! tag:example.com,2000:\n---\na: !e!x 1\n",
		"%YAML 1.1\n---\na: yes\n",
		"a: " + strings.Repeat("[", 70) + strings.Repeat("]", 70) + "\n",
		nest("block map", 65),
		"a: 1\na: 2\n",
		"\uFEFFa: 1\n",
		"a: \xff\n",
		"? [a]\n: b\n",
		"a: .inf\nb: 99999999999999999999\n",
		"a: !!int x\n",
		"--- a: 1\n",
		"a: 1\r\nb: |\r\n  x\r\n",
		strings.Repeat("k: v\n", 300),
		strings.Repeat("- k: v\n  l: [1]\n", 300),
		// Block sequence entries inside flow collections, which goccy
		// nests past the depth limit in quadratic memory, and tag
		// chains, which its parser recurses on (WP-33 review).
		"a: [" + strings.Repeat("- ", 16000) + "x]\n",
		"a: {k: " + strings.Repeat("- ", 4000) + "x}\n",
		"a: [-, -]\nb: [-: x]\n",
		"a: " + strings.Repeat("!!str ", 2000) + "x\n",
		"spec: !!map\n  !!str name: !!map\n    !!str : 1\n",
		// A tagged block scalar before a long blank run, which goccy
		// clips in O(m·L) without the keep-chomping rewrite.
		longTaggedScalar("a: ", "!!str |", 30<<10, 30000),
		longTaggedScalar("- ", "!!str >", 1<<10, 100),
		// A block scalar that a comment ends after a long blank run
		// (second WP-33 review): 36 KB that took goccy 2.3 s.
		longBlockScalar("a: ", "|", 32<<10, 4000, "# c\nb: 1\n"),
		// Empty tagged nodes before siblings, which goccy nests one level
		// per line (third WP-33 review).
		taggedLadder(4, 8, 0),
		"a: !!map\nb: 1\n? c\n: !!seq\n- x\n",
		// Implicit keys over two lines, which goccy nests one level
		// deeper per line (fourth WP-33 review).
		keyLadder(3, 6),
		"a:\n  - \"x\"\n:\n",
		"a:\n  - >\n:\n",
		"? |\n  k\n: v\n? \"x\"\n: y\n",
		"x: [!!str a, !!str b, c]\n",
		// Explicit keys whose ':' goccy groups with a later node, and
		// '?' lines holding more than the key (fifth WP-33 review).
		explicitKeyLadder(3, 4),
		"? \"a\"\n  - \"b\"\n:\n",
		"? \"a\"\n  \"b\"\n:\n",
		"? \"a\"\n  !!str\n:\n",
		"x:\n  ? \"a\"\n    - \"b\"\n  :\n",
		"- ? \"a\"\n    - \"b\"\n  :\n",
		"? !!str \"a\"\n: v\n? a\n  b\n: w\n? !!str\n: x\n",
		"? \"q\"k\n",
		"a:\n  ? \"q\"''\n",
		"? k:\n",
		// Shapes the width check found (fifth WP-33 review), and flow
		// pairs, a level the token pass does not count.
		"? a\nb\n? \nz: 1\n",
		"? !!str {k: v}\n",
		"k: !!seq - a\nm: !!map ? b\n",
		"k:\n{a: b}\n",
		"a:\n -\n!!str k: 1\n",
		"10000000000000000000: 1\n",
		"--- %0\n-\n",
		"0:\n- 0:\n-\n00:\n",
		"a: " + strings.Repeat("[k: ", 40) + "v" + strings.Repeat("]", 40) + "\n",
		// Shapes of the sixth WP-33 review: plain scalars continued by a
		// line starting with '-', quoted and block scalars goccy decodes
		// otherwise, the line after an empty block scalar, a tag at the
		// end of the input, tagged keys after empty values, and the null
		// of a trailing '?' entry.
		"description: run the job\n  -v # verbose\nimage: x\n",
		"k: a\n  - b: c\nx:\n- a\n  - b #c\n",
		"k: a\n  -b #c\n  d\n",
		"k: \"a\\ \n\n  b\"\nl: 'a\t\n  b'\nm: \"\\xZZ\"\n",
		"x: |\n\n&a q: 1\n",
		"x: |\n\n\"q\": 1\n",
		"k: !foo",
		"a: |-\n  x  \nb: |+\n\nc: >-\n  a\n   \n \n",
		"? >\n    \n \n:\n",
		"a:\n  b:\n!!str c: 1\ne:\n  ? k\n!!str x: v\n",
		"x:\n  ? c\ny: 1\n",
		"a: {a, b: , c: 1, : , ? d}\n",
		// Shapes of the seventh WP-33 review: document markers goccy reads
		// where the cut does not, entries indented between two columns,
		// tagged empty nodes in flow collections, flow explicit keys whose
		// tag ends its line, tabs inside and before scalars, and a lone
		// '?'.
		"a: 1\nb: 2\n...x: 1\n",
		"a:\n-\nb: 1\n...x: 1\n",
		"a: 1\n...#\n",
		"a: 1\n... {}\n",
		"...!!map\nb: 1\n",
		"a:\n  k: v\n c: 2\n",
		"  k: v\nb: 2\n",
		"a: [!!str , !!str , x]\n",
		"a: {k: !!str , !!int : v}\n",
		"k: {p: q,\n ? !!str\n  x\n  r: s}\n",
		"x: 1\t2\nbc: 1\nb\tc: 2\n",
		"{\n\t\"a\": 1,\n\t\"b\": [\n\t\t\"x\"\n\t]\n}\n",
		"a: !!str\tx\nb:\t'y'\n",
		"b:\n?\nx: [?]\n",
		// Shapes of the eighth WP-33 review: a ':' goccy reads otherwise
		// inside a flow collection, flow entries without ',' and the
		// ladders goccy nests by column, the inputs that failed this target
		// (a key after a tab, a plain scalar after a block scalar and a
		// blank run), values at their entry's column, and the scanner faults
		// the appended-content check found (a tab in a double-quoted
		// scalar, a keep-chomping scalar of blank lines, an indentation
		// indicator before trailing blank lines, a tag whose content goccy
		// reports on a later line).
		"labels: {app:web, tier: db}\nk: {x: [10:30]}\nl: [a:, b]\n",
		"x: [\nk:\n k:\n  k: v\n]\ny: [\n a: 1\n b: 2\n]\n",
		"x: [\n? a\n: b\n? c\n: d\n]\n",
		flowLadder(3, 4, 8),
		commaLess(40, func(i int) string { return "k" + itoa(i) + ":" }, "z: v"),
		"\t\"\":",
		"|\n 0\n\n\n\n\n\n\n\n\n0\n 0",
		"k: |\n x\n\n\n\n\n\n\n\n\n0\n 0\n",
		"spec:\n  args:\n  - run the job\n    -v\n\n",
		"spec:\n  name:\n value \n",
		"k:\nv\nl:\n[a]\n-\nk: v\n",
		"a: \"x\ty\"\nb: 1\n",
		"0: |+\n00: |+\n# c\n",
		"a: >2\n  x\n\n",
		"0: !!str 0\n -\n",
		"0: !!str\n0\n -",
		"? |++",
		"{\"a\"\t: 1}\n",
	}
}

// checkDiagnostics fails on a diagnostic outside the profile's codes or
// without a position.
func checkDiagnostics(t *testing.T, diags diag.List, path string, allowed ...string) {
	t.Helper()
	for _, d := range diags {
		if _, ok := errcode.Lookup(d.Code); !ok {
			t.Fatalf("unregistered code %q", d.Code)
		}
		ok := false
		for _, c := range allowed {
			ok = ok || d.Code == c
		}
		if !ok {
			t.Fatalf("code %s outside %v: %+v", d.Code, allowed, d)
		}
		if d.Severity != diag.SeverityError || d.File != path || d.Line < 1 || d.Column < 1 || d.Message == "" {
			t.Fatalf("diagnostic without severity, position or message: %+v", d)
		}
	}
}

// FuzzLoadYAML is the restricted YAML loader target (01 test plan
// FuzzProfileYAML, 11 req 26; WP-33 FuzzLoadYAML): no panic or hang, only
// RZ-CFG-001 to RZ-CFG-005 with positions, mapping roots, bounded memory
// under alias bombs, the token bound of the byte pre-scan never below
// goccy's count, and accepted documents round-trip through Encode.
func FuzzLoadYAML(f *testing.F) {
	corpus.AddYAML(f)
	for _, s := range hostileSeeds() {
		f.Add([]byte(s))
	}
	// The 1 MiB shape of the third WP-33 review's blocker, which parsed
	// to depth 16,066 in 1,679 MiB before the token pass stopped it.
	f.Add([]byte(taggedLadder(63, 255, 1<<20)))
	// The 1 MiB shape of the fourth WP-33 review's blocker, multi-line
	// implicit keys that parsed to depth 4,319.
	f.Add([]byte(keyLadder(16, 254)))
	// The shapes of the fifth WP-33 review's blocker, explicit keys that
	// parsed to depth 1,056 (100 KB) and 2,139 (515 KB).
	f.Add([]byte(explicitKeyLadder(31, 32)))
	f.Add([]byte(explicitKeyLadder(-1, 62)))
	// The shapes of the sixth WP-33 review's blockers: long keys above
	// many nodes (298 MiB and 263 MiB), flow mapping entries without
	// values (minutes), and the densest documents at and past the token
	// limit (up to 485 MiB at 1,000,000 tokens).
	f.Add([]byte(longKeyFlow(16384, 16384)))
	f.Add([]byte(longKeysNested(62, 1000, 4096)))
	f.Add([]byte(longKeysInFlow(60, 1000, 16384)))
	f.Add([]byte(keysWithoutValues("a", 400000)))
	f.Add([]byte(keysWithoutValues("k:", 400000)))
	f.Add([]byte(keysWithoutValues("a", 150000)))
	f.Add([]byte("a: [" + strings.Repeat("1,", 499990) + "1]\n"))
	f.Add([]byte("a: [" + strings.Repeat("1,", DefaultMaxDocumentTokens/2-3) + "1]\n"))
	f.Add([]byte("a: [" + strings.Repeat("[],", DefaultMaxDocumentTokens/3-2) + "1]\n"))
	f.Add([]byte("a: [" + strings.Repeat("k: 1,", DefaultMaxDocumentTokens/4-2) + "1]\n"))
	f.Add([]byte("a:\n" + strings.Repeat("-\n", DefaultMaxDocumentTokens-4)))
	// The shapes of the seventh WP-33 review's blockers: tokens after a
	// document end (38 s for 1 MiB), entries between two columns (37 s),
	// tagged empty nodes (10 s), and a flow collection left open (31 s).
	var wide strings.Builder
	for i := 0; wide.Len() < 1<<20-16; i++ {
		wide.WriteString("k" + itoa(i) + ": v\n")
	}
	f.Add([]byte(wide.String() + "...x: 1\n"))
	f.Add([]byte("...#\n" + wide.String()))
	f.Add([]byte(longKeysNested(62, 1000, 16384) + "...x: 1\n"))
	f.Add([]byte(strings.Repeat("k:\n-\n", 10000) + "...x: 1\n"))
	f.Add([]byte(dedented(strings.Repeat("k: v\n", 125000))))
	f.Add([]byte(dedented(longKeysNested(61, 1000, 16384))))
	f.Add([]byte(taggedEmpties(99000, false)))
	f.Add([]byte(taggedEmpties(64000, true)))
	f.Add([]byte("a: {" + strings.Repeat("a,", 150000) + "\n"))
	// The 1 MiB shapes of the eighth WP-33 review's blocker: flow ladders
	// (567 MiB, 3,747 MiB) and comma-less flow entries (17.9 s), and
	// documents whose trees stay live (up to 276 MiB).
	f.Add([]byte(flowLadder(1, 836, 836)))
	f.Add([]byte(flowLadder(62, 100, 100)))
	f.Add([]byte(commaLess(80000, func(i int) string { return "k" + itoa(i) + ": v" }, "")))
	f.Add([]byte(commaLess(80000, func(i int) string { return "k" + itoa(i) + ":" }, "z: v")))
	f.Add([]byte(strings.Repeat("---\na: ["+strings.Repeat("1,", 174000)+"1]\n", 3)))
	for _, s := range suiteInputs(f) {
		if len(s) <= 4096 {
			f.Add([]byte(s))
		}
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if raceEnabled && len(data) > 64<<10 {
			// The race detector slows the parse of these single-goroutine
			// inputs twentyfold and distorts the heap they are measured
			// by; the run without it checks them (TestHostileInputsBounded
			// holds their shapes too).
			t.Skip("measures memory, which the race detector distorts")
		}
		var docs []Document
		var diags diag.List
		run := func() {
			var err error
			docs, diags, err = Parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{})
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(data) > 64<<10 {
			start := time.Now()
			if peak := peakHeap(run); len(data) <= 1<<20 && peak > heapBudget {
				t.Fatalf("peak heap growth %d MiB for %d bytes", peak>>20, len(data))
			}
			// 2 s per input (01 test plan FuzzProfileYAML); smaller
			// inputs are bounded by their allocations below.
			if took := time.Since(start); len(data) <= 1<<20 && took > 2*time.Second {
				t.Fatalf("took %v for %d bytes", took, len(data))
			}
		} else {
			before := allocBytes()
			run()
			// Small inputs: work linear in the input, so an expansion
			// or a quadratic parse shows up at once.
			if got := allocBytes() - before; got > 32<<20+uint64(len(data))<<10 {
				t.Fatalf("allocated %d MiB for %d bytes", got>>20, len(data))
			}
		}
		checkDiagnostics(t, diags, "f.yaml", codeParse, codeDuplicate, codeAnchor, codeTag, codeSchema)
		for _, d := range docs {
			if d.Root == nil || d.Root.Kind != tree.KindMap || !d.Start.Known() {
				t.Fatalf("document %+v", d)
			}
			// No accepted tree is deeper than MaxDepth (01 req 9).
			if depth := treeDepth(d.Root); depth > DefaultMaxDepth {
				t.Fatalf("document of depth %d, over %d", depth, DefaultMaxDepth)
			}
		}
		// The byte bound holds for every text the pre-checks pass.
		pre := &fileParser{opts: Options{}.withDefaults()}
		if text, ok := pre.precheckYAML(data); ok {
			for _, seg := range splitDocuments(text) {
				if len(seg.text) > 1<<16 {
					continue
				}
				// Scan the text the loader scans: goccy's own clipping
				// of a long blank run is quadratic (chomp.go).
				text := seg.text
				if blankRuns(text, longBlankRun) {
					text, _ = keepChomping(text)
				}
				var s scanner.Scanner
				s.Init(text)
				n := 0
				for {
					tks, err := s.Scan()
					n += len(tks)
					if err != nil || len(tks) == 0 {
						break
					}
				}
				if est, _ := estimateTokens(seg.text, 1<<30); est < n {
					t.Fatalf("token bound %d below goccy's %d tokens for %q", est, n, seg.text)
				}
			}
		}
		if len(diags) == 0 && len(docs) > 0 {
			if msg := roundTrip(t, roots(docs)); msg != "" {
				t.Fatal(msg)
			}
		}
		// Every parsed segment holds one document: goccy's document
		// markers lie on its own marker lines (01 req 9).
		if len(diags) == 0 {
			if err := markersAligned(string(data)); err != nil {
				t.Fatal(err)
			}
		}
		if len(data) <= 4<<10 {
			differential(t, data)
			widthIndependent(t, data, 8)
			appendIndependent(t, data)
			splitMatchesWhole(t, data)
			// Every key and value is reported at its first character
			// (TestPositionsPointAtNodes).
			text := strings.ReplaceAll(strings.ReplaceAll(string(data), "\r\n", "\n"), "\r", "\n")
			lines := strings.Split(text, "\n")
			for _, d := range docs {
				checkPositions(t, text, d.Root, lines)
			}
		}
	})
}

// widthIndependent parses data alone and as the last entries of a mapping
// of siblings more, and fails unless both accept or refuse it alike, with
// the same findings siblings lines further down, and accept the same
// members there (01 req 8; findings of the fifth and sixth WP-33 reviews):
// a document's result never depends on the width of the mapping around
// it. It applies to data whose first node is an implicit key at column 1,
// a block mapping's first key, and that holds no document marker or
// directive. The tests pass 300 siblings, past the split threshold of
// earlier reviews; FuzzLoadYAML passes fewer, which keeps it fast.
func widthIndependent(t *testing.T, data []byte, siblings int) {
	t.Helper()
	if !startsWithKey(data) {
		return
	}
	var b strings.Builder
	for i := range siblings {
		b.WriteString("w\u2603" + itoa(i) + ": v\n")
	}
	prefix := b.String()
	narrow, nd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{}, true)
	wide, wd, _ := parse(context.Background(), append([]byte(prefix), data...), 0, "f.yaml", FormatYAML, Options{}, true)
	if a, b := findings(nd, siblings), findings(wd, 0); a != b {
		t.Fatalf("width, %q: findings %q, after %d entries %q", data, a, siblings, b)
	}
	if len(nd) > 0 {
		return
	}
	if len(narrow) != len(wide) || len(narrow) == 0 || len(wide[0].Root.Members) < siblings {
		t.Fatalf("width, %q: %d documents, after %d entries %d", data, len(narrow), siblings, len(wide))
	}
	rest := &tree.Node{Kind: tree.KindMap, Members: wide[0].Root.Members[siblings:]}
	if a, b := showShifted(narrow[0].Root, siblings), show(rest, true); a != b {
		t.Fatalf("width, %q:\n     %s\nwide %s", data, a, b)
	}
}

// startsWithKey reports whether data starts with an implicit key at line 1,
// column 1, and holds no line that starts with "---" or a directive, so the
// entries of a mapping can go before it; a line starting with "..." ends
// the first document, or is refused, alike at both widths. Data that
// starts with blank or comment lines is left out: before the document
// they are outside every node, after an entry they are between entries,
// where goccy refuses a line of tabs that YAML 1.2 allows. So is data that
// starts with white space: goccy does not count a leading tab, so its
// column 1 is not the key's (minor finding of the eighth WP-33 review).
func startsWithKey(data []byte) bool {
	text := string(data)
	if !utf8.ValidString(text) || text == "" || isWhite(text[0]) {
		return false
	}
	// A carriage return is a line break too (precheckYAML).
	for line := range strings.Lines(strings.ReplaceAll(text, "\r", "\n")) {
		if strings.HasPrefix(line, "---") || strings.HasPrefix(line, "%") {
			return false
		}
	}
	var s scanner.Scanner
	s.Init(text + "\n")
	tks, _ := s.Scan()
	if len(tks) < 2 || tks[0].Position.Line != 1 || tks[0].Position.Column != 1 || !isKeyScalar(tks[0]) {
		return false
	}
	return tks[1].Type == token.MappingValueType && tks[1].Position.Line == 1
}

// appendIndependent parses data alone and with a blank line, a "..."
// line, a comment line and, when data starts with a key at column 1, a
// sibling entry appended after it, and fails unless data the profile
// accepts stays accepted with the same trees and positions (01 req 8, 14;
// minor findings of the eighth WP-33 review): a document's result never
// depends on what follows it. The split parse took goccy's raw positions,
// so a blank line at the end refused "- run the job\n    -v" and moved the
// null of "k1:", and goccy's scanner refused "a: |2\n  x" before a blank
// line and "a: \"x\ty\"" before a sibling. A keep-chomping block scalar
// keeps a blank line after it, so data holding one gets no blank line. An
// empty tagged node before a sibling ("a: !!str\nb: 1") is the one shape
// refused only with a sibling after it, which goccy's parser refuses and
// the profile keeps refused (tokenPass.tagWithoutValue).
func appendIndependent(t *testing.T, data []byte) {
	t.Helper()
	text := string(data)
	if !utf8.ValidString(text) {
		return
	}
	alone, ad, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{}, false)
	if len(ad) > 0 {
		return
	}
	if text != "" && !strings.HasSuffix(text, "\n") && !strings.HasSuffix(text, "\r") {
		text += "\n"
	}
	var trailers []string
	if !rootBlockScalar(text) {
		// A comment line at column 1 is content of a block scalar that is a
		// document's root.
		trailers = append(trailers, "# c\n")
	}
	if !keepsBlankLines(text) {
		trailers = append(trailers, "\n")
	}
	ended := false
	for line := range strings.Lines(strings.ReplaceAll(text, "\r", "\n")) {
		ended = ended || strings.HasPrefix(line, "...")
	}
	if !ended {
		trailers = append(trailers, "...\n")
	}
	if !ended && startsWithKey(data) && !endsWithTag(text) {
		trailers = append(trailers, "w\u2603z: v\n")
	}
	for _, tr := range trailers {
		got, gd, _ := parse(context.Background(), []byte(text+tr), 0, "f.yaml", FormatYAML, Options{}, false)
		if len(gd) > 0 || len(got) != len(alone) {
			t.Fatalf("%q, then %q: %d documents, findings %q; alone %d documents", data, tr, len(got), codes(gd), len(alone))
		}
		for i := range alone {
			root := got[i].Root
			if i == len(alone)-1 && strings.Contains(tr, ":") {
				if root.Kind != tree.KindMap || len(root.Members) == 0 {
					t.Fatalf("%q, then %q: root %s", data, tr, show(root, true))
				}
				root = &tree.Node{Kind: tree.KindMap, Pos: root.Pos, Members: root.Members[:len(root.Members)-1]}
			}
			if a, b := show(alone[i].Root, true), show(root, true); a != b || alone[i].Start != got[i].Start {
				t.Fatalf("%q, then %q, document %d:\nalone %s\nthen  %s", data, tr, i, a, b)
			}
		}
	}
}

// rootBlockScalar reports whether a document of text has a block scalar
// as its root: its first token other than a property or a comment is a
// block scalar header.
func rootBlockScalar(text string) bool {
	tks, _ := scanString(text)
	start := true
	for _, tk := range tks {
		switch tk.Type {
		case token.DocumentHeaderType, token.DocumentEndType:
			start = true
		case token.CommentType, token.TagType, token.AnchorType:
		case token.LiteralType, token.FoldedType:
			if start {
				return true
			}
		default:
			start = false
		}
	}
	return false
}

// keepsBlankLines reports whether text holds a block scalar header with
// keep chomping ('+').
func keepsBlankLines(text string) bool {
	tks, _ := scanString(text)
	return slices.ContainsFunc(tks, func(tk *token.Token) bool {
		return (tk.Type == token.LiteralType || tk.Type == token.FoldedType) && strings.Contains(tk.Value, "+")
	})
}

// endsWithTag reports whether the last token of text, comments aside, is a
// tag.
func endsWithTag(text string) bool {
	tks, _ := scanString(text)
	for i := len(tks) - 1; i >= 0; i-- {
		if tks[i].Type != token.CommentType {
			return tks[i].Type == token.TagType
		}
	}
	return false
}

// splitMatchesWhole parses data entry by entry, as production does, and as
// goccy's parse reads it whole (splitAt -1), and fails unless the two give
// the same trees and positions where both accept (01 req 10, 14; minor
// finding of the eighth WP-33 review): every block collection takes the
// split parse, so a fault in it would otherwise go unseen. The split parse
// is stricter where goccy accepts invalid YAML, and it reads YAML 1.2's
// tree where goccy misreads an empty last sequence entry before a key: the
// token pass splits such a collection at any threshold (tokenPass.block),
// so a document whose parse with only those collections split differs from
// goccy's whole parse is left out.
func splitMatchesWhole(t *testing.T, data []byte) {
	t.Helper()
	whole, wd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{splitAt: -1}, true)
	split, sd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{}, true)
	if len(wd) > 0 || len(sd) > 0 {
		return
	}
	forced, fd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{splitAt: 1 << 30}, true)
	if len(fd) > 0 || !sameDocuments(forced, whole) {
		return
	}
	if !sameDocuments(split, whole) {
		var a, b []string
		for _, d := range whole {
			a = append(a, show(d.Root, true))
		}
		for _, d := range split {
			b = append(b, show(d.Root, true))
		}
		t.Fatalf("split parse of %q:\nwhole %s\nsplit %s", data, strings.Join(a, " ;; "), strings.Join(b, " ;; "))
	}
}

// sameDocuments reports whether two parses gave the same documents, with
// the same positions.
func sameDocuments(a, b []Document) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Start != b[i].Start || show(a[i].Root, true) != show(b[i].Root, true) {
			return false
		}
	}
	return true
}

// findings renders diagnostics as "code@line:column message", their lines
// moved down by shift.
func findings(l diag.List, shift int) string {
	var parts []string
	for _, d := range l {
		parts = append(parts, fmt.Sprintf("%s@%d:%d %s", d.Code, d.Line+shift, d.Column, d.Message))
	}
	return strings.Join(parts, "\n")
}

// differential parses data with the keep-chomping rewrite (chomp.go) and
// with it turned off, so goccy clips every block scalar itself, and fails
// unless both agree exactly: the rewrite never changes a result. (The split
// parse, which every block collection takes, is compared with goccy's
// whole parse in TestSplitParseMatchesGoccy and TestSplitEdgeCases: on
// input goccy misparses, such as an empty sequence entry followed by a key
// at the sequence's column, it follows the block structure of YAML 1.2
// instead. widthIndependent compares a document alone and inside a wide
// mapping.)
func differential(t *testing.T, data []byte) {
	t.Helper()
	whole, wd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{noRewrite: true}, true)
	got, gd, _ := parse(context.Background(), data, 0, "f.yaml", FormatYAML, Options{}, true)
	if codes(wd) != codes(gd) {
		t.Fatalf("keep-chomping rewrite: findings %q, without %q", codes(gd), codes(wd))
	}
	if len(got) != len(whole) {
		t.Fatalf("keep-chomping rewrite: %d documents, without %d", len(got), len(whole))
	}
	for i := range whole {
		if a, b := show(whole[i].Root, true), show(got[i].Root, true); a != b {
			t.Fatalf("keep-chomping rewrite, document %d:\n got %s\nwant %s", i, b, a)
		}
	}
}

// FuzzProfileJSON is the JSON front end target (01 test plan): no panic,
// only RZ-CFG-001, -002 and -005 with positions, and every accepted
// document reads back from its JSON and YAML encodings unchanged.
func FuzzProfileJSON(f *testing.F) {
	corpus.AddJSON(f)
	for _, s := range []string{
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "spec": {"a": [1, 2.5, true, null, {}, []]}}`,
		`{"a": 1, "a": 2}`, `{"a": "\ud800"}`, `{"a": 01}`, `[1]`, `{} {}`, `{"a": 1,}`, `{"x": "é\/\t"}`,
		`{"n": -0, "big": 9223372036854775808, "f": 1e400}`, "\uFEFF{}", "{\"a\":" + strings.Repeat("[", 70) + strings.Repeat("]", 70) + "}",
		// Tabs between a key and its ':', and a final line of a tab, which
		// the YAML front end read otherwise (eighth WP-33 review).
		"{\"a\"\t: 1}\n", "{\"a\": 1,\n\t\"b\"\t:\t2}\n", "{\"a\": 1}\n\t\n",
	} {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		docs, diags, err := Parse(context.Background(), data, 0, "f.json", FormatJSON, Options{})
		if err != nil {
			t.Fatal(err)
		}
		checkDiagnostics(t, diags, "f.json", codeParse, codeDuplicate, codeSchema)
		if len(docs) > 1 || (len(docs) == 1) == (len(diags) > 0) {
			t.Fatalf("%d documents with %d diagnostics", len(docs), len(diags))
		}
		if len(docs) == 0 {
			return
		}
		var b bytes.Buffer
		if err := EncodeJSON(&b, docs[0].Root, EncodeOptions{Verbatim: true}); err != nil {
			t.Fatal(err)
		}
		back, diags, err := Parse(context.Background(), b.Bytes(), 0, "f.json", FormatJSON, Options{MaxDepth: 1 << 10})
		if err != nil || len(diags) != 0 || showValues(back[0].Root) != showValues(docs[0].Root) {
			t.Fatalf("JSON round trip: %v %q\n%s", err, codes(diags), b.String())
		}
		if msg := roundTrip(t, roots(docs)); msg != "" {
			t.Fatal(msg)
		}
	})
}

// FuzzEncodeRoundTrip checks the emitter over trees built from arbitrary
// strings, keys and numbers (01 test plan FuzzEncodeRoundTrip; 02 req
// 45-46): Parse(Encode(x)) == x, Encode is idempotent, and "${" survives
// rendering once substitution reads "$${".
func FuzzEncodeRoundTrip(f *testing.F) {
	f.Add("shop", "key", "1.5", int64(7), true)
	f.Add("${HOST}", "$patch", ".5", int64(-1), false)
	f.Add("yes", "1", "1.", int64(0), true)
	f.Add("a\n\"b\"\t \x7f", "é", "1e3", int64(9223372036854775807), false)
	f.Add("", "", "-0.0", int64(-9223372036854775808), true)
	f.Fuzz(func(t *testing.T, s, key, float string, i int64, flag bool) {
		if !utf8.ValidString(s) || !utf8.ValidString(key) {
			return
		}
		ft := resolvePlain(float)
		n := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: key, Value: &tree.Node{Kind: tree.KindString, Text: s}},
			{Key: key + "_list", Value: &tree.Node{Kind: tree.KindList, Items: []*tree.Node{
				{Kind: tree.KindInt, Text: itoa64(i)},
				{Kind: tree.KindBool, Bool: flag},
				{Kind: tree.KindMap, Members: []tree.Member{{Key: s, Value: &tree.Node{Kind: tree.KindNull}}}},
				{Kind: tree.KindList, Items: []*tree.Node{{Kind: tree.KindString, Text: key}}},
			}}},
		}}
		if ft.code == "" && ft.kind == tree.KindFloat {
			n.Members = append(n.Members, tree.Member{Key: key + "_f", Value: &tree.Node{Kind: tree.KindFloat, Text: ft.text}})
		}
		if msg := roundTrip(t, []*tree.Node{n}); msg != "" {
			t.Fatal(msg)
		}
		var b bytes.Buffer
		if err := EncodeJSON(&b, n, EncodeOptions{}); err != nil {
			t.Fatal(err)
		}
		back, diags, err := Parse(context.Background(), b.Bytes(), 0, "f.json", FormatJSON, Options{})
		if err != nil || len(diags) != 0 {
			t.Fatalf("JSON output does not parse: %v %q\n%s", err, codes(diags), b.String())
		}
		unescapeSubstitution(back[0].Root)
		// A float written with ".0" reads back with that text.
		if showValues(back[0].Root) != showValues(n) {
			t.Fatalf("JSON round trip:\n got %s\nwant %s", showValues(back[0].Root), showValues(n))
		}
	})
}
