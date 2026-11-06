// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// hostileSeeds are small instances of the hostile shapes and of the inputs
// the parser's tests pin.
func hostileSeeds() []string {
	return []string{
		"a: &a [x, x]\nb: &b [*a, *a]\nc: [*b, *b]\n",
		"base: &base {a: 1}\nderived:\n  <<: *base\n",
		"a: !include other.yaml\nb: !env HOME\n",
		"%TAG !e! tag:example.com,2000:\n---\na: !e!x 1\n",
		"%YAML 1.1\n---\na: yes\n",
		"%YAML 1.2\n---\na: 1\n",
		"%FOO bar\n---\na: 1\n",
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
		// Block sequence entries inside flow collections, and tag chains.
		"a: [" + strings.Repeat("- ", 1600) + "x]\n",
		"a: {k: " + strings.Repeat("- ", 400) + "x}\n",
		"a: [-, -]\nb: [-: x]\n",
		"a: " + strings.Repeat("!!str ", 200) + "x\n",
		"spec: !!map\n  !!str name: !!map\n    !!str : 1\n",
		// Block scalars before runs of empty lines and comments.
		longTaggedScalar("a: ", "!!str |", 1<<10, 100),
		longTaggedScalar("- ", "!!str >", 1<<10, 100),
		longBlockScalar("a: ", "|", 1<<10, 100, "# c\nb: 1\n"),
		// Empty tagged nodes before siblings, and keys over lines.
		taggedLadder(4, 8, 0),
		"a: !!map\nb: 1\n? c\n: !!seq\n- x\n",
		keyLadder(3, 6),
		"a:\n  - \"x\"\n:\n",
		"a:\n  - >\n:\n",
		"? |\n  k\n: v\n? \"x\"\n: y\n",
		"x: [!!str a, !!str b, c]\n",
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
		"? a\nb\n? \nz: 1\n",
		"? !!str {k: v}\n",
		"k: !!seq - a\nm: !!map ? b\n",
		"k:\n{a: b}\n",
		"a:\n -\n!!str k: 1\n",
		"10000000000000000000: 1\n",
		"--- %0\n-\n",
		"0:\n- 0:\n-\n00:\n",
		"a: " + strings.Repeat("[k: ", 40) + "v" + strings.Repeat("]", 40) + "\n",
		// Plain scalars continued by a line starting with '-', quoted and
		// block scalars, the line after an empty block scalar, a tag at the
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
		// Document markers, entries between two columns, tagged empty
		// nodes in flow collections, flow explicit keys whose tag ends
		// its line, tabs inside and before scalars, and a lone '?'.
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
		// A ':' inside a flow collection, flow entries without ',',
		// ladders, a key after a tab, a plain scalar after a block scalar
		// and empty lines, values at their entry's column, tabs in quoted
		// scalars, keep chomping before a comment, indentation indicators
		// before empty lines, and a tagged node continued on its next line.
		"labels: {app:web, tier: db}\nk: {x: [10:30]}\nl: [a:, b]\n",
		"x: [\nk:\n k:\n  k: v\n]\ny: [\n a: 1\n b: 2\n]\n",
		"x: [\n? a\n: b\n? c\n: d\n]\n",
		flowLadder(3, 4, 8),
		commaLess(40, func(i int) string { return " k" + itoa(i) + ":" }, " z: v"),
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
		// The rows of the parser's strictness: a closing bracket at its
		// key's column, a comment right after a quoted scalar, quoted
		// lines at the mapping's column, ':' and '?' before a flow
		// indicator, tabs after a block scalar, a merge key after a tab,
		// a ':' kept in a plain scalar after a tab, and a key of 1,025
		// characters.
		"hosts: [\n  a,\n  b\n]\n",
		"a: \"x\"#c\n",
		"\"\t0\"#:",
		"quoted: \"a\nb\nc\"\n",
		"x: {a:[]}\ny: [a:{}]\nz: {app:web}\n",
		"x: [?]\ny: {?}\n",
		"foo: |\n  x\n\t# c\nbar: 1\n",
		"foo: |\n  x\n\t\n",
		"<<:\tx\n? <<\n<<<: y\n",
		"k: a\"\t:b\n",
		strings.Repeat("k", 1025) + ": v\n",
		"x: a\u00a0b\n",
		"%FOO {\n---\n!!map {\n...\n",
		// Flow pairs whose key's tag or anchor ends an earlier line, and
		// properties that end their line before a flow node or a value.
		"x: [!!str\n  0: a]\n",
		"x: [a, !!str\n  \"k\": b]\n",
		"x: [&a\n  : v]\n",
		"x: [!!str\n  0, 0: !!str\n  v]\n",
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

// FuzzLoadYAML is the restricted YAML loader target: no panic or hang, only
// RZ-CFG-001 to RZ-CFG-005 with positions, mapping roots no deeper than
// MaxDepth, bounded memory and time, every key and value at its first
// character, and accepted documents round-trip through Encode.
func FuzzLoadYAML(f *testing.F) {
	corpus.AddYAML(f)
	for _, s := range hostileSeeds() {
		f.Add([]byte(s))
	}
	// The 1 MiB shapes of the hostile table (TestHostileInputsBounded).
	f.Add([]byte(taggedLadder(63, 255, 1<<20)))
	f.Add([]byte(keyLadder(16, 254)))
	f.Add([]byte(explicitKeyLadder(31, 32)))
	f.Add([]byte(explicitKeyLadder(-1, 62)))
	f.Add([]byte(longKeyFlow(16384, 16384)))
	f.Add([]byte(longKeysNested(62, 1000, 4096)))
	f.Add([]byte(longKeysInFlow(60, 1000, 16384)))
	f.Add([]byte(keysWithoutValues("a", 400000)))
	f.Add([]byte(keysWithoutValues("k:", 400000)))
	f.Add([]byte("a: [" + strings.Repeat("1,", 499990) + "1]\n"))
	f.Add([]byte("a: [" + strings.Repeat("[],", 300000) + "1]\n"))
	f.Add([]byte("a: [" + strings.Repeat("k: 1,", 200000) + "1]\n"))
	f.Add([]byte("a:\n" + strings.Repeat("-\n", 500000)))
	var wide strings.Builder
	for i := 0; wide.Len() < 1<<20-16; i++ {
		wide.WriteString("k" + itoa(i) + ": v\n")
	}
	f.Add([]byte(wide.String() + "...x: 1\n"))
	f.Add([]byte("...#\n" + wide.String()))
	f.Add([]byte(dedented(strings.Repeat("k: v\n", 125000))))
	f.Add([]byte(taggedEmpties(99000, false)))
	f.Add([]byte(taggedEmpties(64000, true)))
	f.Add([]byte("a: {" + strings.Repeat("a,", 150000) + "\n"))
	f.Add([]byte(flowLadder(1, 836, 836)))
	f.Add([]byte(flowLadder(62, 100, 100)))
	f.Add([]byte(commaLess(80000, func(i int) string { return " k" + itoa(i) + ": v" }, "")))
	f.Add([]byte(strings.Repeat("%FOO { !!map [\n", 70000) + "---\na: 1\n"))
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
			// 2 s per input when fuzzing, where the bound catches a parse
			// that hangs; the seeds a plain go test runs are bounded by
			// their heap alone, and smaller inputs by their allocations
			// below.
			if took := time.Since(start); fuzzing() && len(data) <= 1<<20 && took > 2*time.Second {
				t.Fatalf("took %v for %d bytes", took, len(data))
			}
		} else {
			before := allocBytes()
			run()
			// Small inputs: work linear in the input, so an expansion or
			// a quadratic parse shows up at once; the findings, at most
			// MaxDiagnostics+1, take up to 16 MiB.
			if got := allocBytes() - before; got > 16<<20+uint64(len(data))<<8 {
				t.Fatalf("allocated %d MiB for %d bytes", got>>20, len(data))
			}
		}
		checkDiagnostics(t, diags, "f.yaml", codeParse, codeDuplicate, codeAnchor, codeTag, codeSchema)
		for _, d := range docs {
			if d.Root == nil || d.Root.Kind != tree.KindMap || !d.Start.Known() {
				t.Fatalf("document %+v", d)
			}
			// No accepted tree is deeper than MaxDepth.
			if depth := treeDepth(d.Root); depth > DefaultMaxDepth {
				t.Fatalf("document of depth %d, over %d", depth, DefaultMaxDepth)
			}
		}
		if len(diags) == 0 && len(docs) > 0 {
			if msg := roundTrip(t, roots(docs)); msg != "" {
				t.Fatal(msg)
			}
		}
		if len(data) <= 4<<10 {
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

// FuzzProfileJSON is the JSON front end target: no panic, only RZ-CFG-001,
// -002 and -005 with positions, and every accepted document reads back from
// its JSON and YAML encodings unchanged.
func FuzzProfileJSON(f *testing.F) {
	corpus.AddJSON(f)
	for _, s := range []string{
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "spec": {"a": [1, 2.5, true, null, {}, []]}}`,
		`{"a": 1, "a": 2}`, `{"a": "\ud800"}`, `{"a": 01}`, `[1]`, `{} {}`, `{"a": 1,}`, `{"x": "é\/\t"}`,
		`{"n": -0, "big": 9223372036854775808, "f": 1e400}`, "\uFEFF{}", "{\"a\":" + strings.Repeat("[", 70) + strings.Repeat("]", 70) + "}",
		// Tabs between a key and its ':', and a final line of a tab.
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
// strings, keys and numbers: Parse(Encode(x)) == x, Encode is idempotent,
// and "${" survives rendering once substitution reads "$${".
func FuzzEncodeRoundTrip(f *testing.F) {
	f.Add("shop", "key", "1.5", int64(7), true)
	f.Add("${HOST}", "$patch", ".5", int64(-1), false)
	f.Add("yes", "1", "1.", int64(0), true)
	f.Add("a\n\"b\"\t \x7f", "é", "1e3", int64(9223372036854775807), false)
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
