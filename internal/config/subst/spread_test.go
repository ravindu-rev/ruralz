// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"math/rand/v2"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// needles holds a fragment of every credential detector's pattern, and
// none matches: a detector that runs a regular expression whenever its
// fragment occurs reads all of a long text around it.
const needles = " -----BEGIN PRIVATE KEY- :// eyJ AKIA ASIA ghp_ github_pat_ xoxb- sk- Bearer Basic "

// maxStep is the most work units one counted step charges (textStep
// bytes, a window's overlap and a carry of less than one unit).
const maxStep = (textStep+byteUnit)/byteUnit + 1

// spreadShape is a stage E input of size n for TestYieldSpread: one member
// of the class of the review finding on bursty Yield calls (work charged
// in one bulk charge before or after a pass over the whole input, or not
// charged at all).
type spreadShape struct {
	name  string
	vars  func(n int) Map
	hints func(n int) []string
	build func(n int) string
	// nlogn marks the shape whose work grows as n log n (a sort).
	nlogn bool
	// size, when positive, replaces TestYieldSpread's size.
	size int
}

func (sh spreadShape) options(t testing.TB, n int, yield func()) Options {
	t.Helper()
	vars := Map{"A": "a"}
	if sh.vars != nil {
		vars = sh.vars(n)
	}
	var hints []string
	if sh.hints != nil {
		hints = sh.hints(n)
	}
	return Options{Schemas: schemas(t), Variables: vars, HintNames: hints, Yield: yield}
}

// filler is n*byteUnit bytes of text no detector matches.
func filler(n int) string { return strings.Repeat("a", n*byteUnit) }

// policySpec returns a Policy document of the given spec members.
func policySpec(members string) string {
	return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"p"},"spec":{` + members + `}}`
}

// spreadShapes are the class members of the review finding, one per code
// path, each at size n (elements, or n*byteUnit bytes of text).
func spreadShapes() []spreadShape {
	return []spreadShape{
		{name: "detect: filler, then every needle", build: func(n int) string { return prefixRoute(filler(n) + needles) }},
		{name: "detect: every needle, then filler", build: func(n int) string { return prefixRoute(needles + filler(n)) }},
		{name: "detectRuns: a long default holding every needle", build: func(n int) string {
			return prefixRoute("${A:-" + needles + filler(n) + "}")
		}},
		{name: "detectRuns and secretNames: malformed text", build: func(n int) string {
			return prefixRoute(needles + filler(n) + "${TOKEN} ${")
		}},
		{name: "render: a long value", vars: func(n int) Map { return Map{"B": filler(n)} }, build: func(int) string { return prefixRoute("x${B}") }},
		{name: "render: long literal parts", build: func(n int) string { return prefixRoute(filler(n) + "${A}" + filler(n)) }},
		{name: "eachRun: two long defaults in one run", build: func(n int) string {
			return prefixRoute("${U:-" + filler(n) + "}${V:-" + filler(n) + "}")
		}},
		{name: "eachRun: three long defaults in one run", build: func(n int) string {
			return prefixRoute("${U:-" + filler(n) + "}${V:-" + filler(n) + "}${W:-" + filler(n) + "}")
		}},
		{name: "keys: a long key", build: func(n int) string {
			return labelsRoute(1, func(int) string { return filler(n) + needles }, func(int) string { return "v" })
		}},
		{name: "secretLike: a long name with a default", build: func(n int) string {
			return prefixRoute("${" + strings.Repeat("A", n*byteUnit/4) + ":-d}")
		}},
		{name: "selectOf: a wide PolicySpec, type last", build: func(n int) string {
			return policySpec(repeat(n, ",", func(i int) string { return `"m` + strconv.Itoa(i) + `":"x"` }) + `,"type":"plugin","config":{"a":"${A}"}`)
		}},
		{name: "headerSlot: a wide header entry, name and value last", build: func(n int) string {
			entry := "{" + repeat(n, ",", func(i int) string { return `"m` + strconv.Itoa(i) + `":1` }) + `,"name":"authorization","value":"${A}"}`
			return policySpec(`"type":"headers","config":{"request":{"set":[` + entry + `]}}`)
		}},
		{name: "itemElem: a wide keyed entry, its key last", build: func(n int) string {
			entry := "{" + repeat(n, ",", func(i int) string { return `"m` + strconv.Itoa(i) + `":"x"` }) + `,"name":"h","port":"${A}"}`
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[` + entry + `]}}`
		}},
		{name: "itemElem: a wide composite set element", build: func(n int) string {
			elem := "{" + repeat(n, ",", func(i int) string { return `"m` + strconv.Itoa(i) + `":"x"` }) + `,"z":"${A}"}`
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"methods":[` + elem + `]}}}`
		}},
		{
			name: "hints: the index over many names", nlogn: true,
			hints: func(n int) []string {
				out := make([]string, n)
				for i := range out {
					out[i] = "NAME" + strconv.Itoa(n-i)
				}
				return out
			},
			build: func(int) string { return prefixRoute("${UNDEFINED_X}") },
		},
		{
			name: "retype: a long integer in an integer field", vars: func(n int) Map { return Map{"B": strings.Repeat("0", n*byteUnit) + "1"} },
			build: func(int) string {
				return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[{"name":"h","port":"${B}"}]}}`
			},
		},
		{name: "retype: a long float in a number field", vars: func(n int) Map {
			k := n * byteUnit / 4
			return Map{"B": strings.Repeat("0", k) + "1." + strings.Repeat("5", k) + "e" + strings.Repeat("0", k)}
		}, build: func(int) string {
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Gateway","metadata":{"name":"g"},"spec":{"telemetry":{"traceSampling":"${B}"}}}`
		}},
		{name: "literal: a long framed credential header value", vars: func(int) Map { return Map{"A": "t"} }, build: func(n int) string {
			value := "${A}" + strings.Repeat("; ", n*byteUnit/2)
			return policySpec(`"type":"headers","config":{"request":{"set":[{"name":"authorization","value":` + quote(value) + `}]}}`)
		}},
		{name: "envelope: a long kind without schema", build: func(n int) string {
			return `{"apiVersion":"other/v1","kind":` + quote("K"+filler(n)+"${A}") + `,"metadata":{"name":"x"}}`
		}},
		{name: "forbidden: a long CEL text with an escape", build: func(n int) string {
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":` + quote(filler(n)+"$${x}"+needles) + `}}}`
		}},
	}
}

// spreadRun substitutes the document src with a probe and returns it
// with the Yield calls.
func spreadRun(t testing.TB, sh spreadShape, n int) (*probe, int) {
	t.Helper()
	calls := 0
	s := New(sh.options(t, n, nil))
	r := docs(t, &tree.FileTable{}, "r.yaml", sh.build(n))[0]
	p := &probe{}
	_ = s.apply(r, func() { calls++ }, p)
	return p, calls
}

// TestYieldSpread covers 01 req 53 (review finding: Yield ran every 256
// units by count but in bursts, because the credential detectors ran
// their regular expressions over a whole text and were charged after,
// copies and visits were charged in bulk, and the schemaidx Select reads
// went uncounted): for every scale shape (at 4096 items, where a bulk
// pass over one scalar already passes the bound) and every spreadShapes
// class member at 1<<16 elements or 1 to 4 MiB of text, no more than yieldEvery
// work units plus one counted step are charged between two Yield calls,
// no step reads more than textStep bytes and a window's overlap, and
// schemaidx Select never reads an object of more than a few members. A
// bulk charge of the old kind charges thousands of units between two
// calls, and an uncounted Select reads 1<<16 members.
func TestYieldSpread(t *testing.T) {
	const n = 1 << 16
	var rows []spreadShape
	for _, sh := range scaleShapes() {
		sh := sh
		rows = append(rows, spreadShape{
			name: "scale: " + sh.name, size: 4096, build: sh.build,
			vars: func(int) Map {
				if sh.vars == nil {
					return Map{"A": "a"}
				}
				return sh.vars
			},
			hints: sh.hints,
		})
	}
	rows = append(rows, spreadShapes()...)
	for _, sh := range rows {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			size := n
			if sh.size > 0 {
				size = sh.size
			}
			p, calls := spreadRun(t, sh, size)
			if p.last() > yieldEvery+maxStep {
				t.Errorf("%d work units between two Yield calls, want at most %d", p.last(), yieldEvery+maxStep)
			}
			if p.read > textStep+len(needles) {
				t.Errorf("one counted read of %d bytes, want at most %d", p.read, textStep+len(needles))
			}
			if p.selectWidth > 2*linearScan {
				t.Errorf("schemaidx Select read an object of %d members, want at most %d", p.selectWidth, 2*linearScan)
			}
			if calls < size/yieldEvery {
				t.Errorf("%d Yield calls for size %d, want at least %d", calls, size, size/yieldEvery)
			}
		})
	}
}

// TestSpreadShapesLinear: the spreadShapes class members cost work linear
// in their size (01 req 54), but for the sort of the hint index, which
// costs n log n comparisons.
func TestSpreadShapesLinear(t *testing.T) {
	const n = 2000
	for _, sh := range spreadShapes() {
		t.Run(sh.name, func(t *testing.T) {
			t.Parallel()
			_, w1 := spreadRun(t, sh, n)
			_, w4 := spreadRun(t, sh, 4*n)
			if w1 < n/yieldEvery {
				t.Fatalf("%d Yield calls at n=%d: the shape does too little to measure", w1, n)
			}
			limit := 4*w1 + w1/2 + 4
			if sh.nlogn {
				limit = 6*w1 + 4
			}
			if w4 > limit {
				t.Errorf("work grows faster than the input: %d Yield calls at n=%d, %d at n=%d", w1, n, w4, 4*n)
			}
		})
	}
}

// specDetectors returns the patterns of the 01 J row for RZ-CFG-013 as Go
// regular expressions, in the order and with the names of newDetectors.
func specDetectors() []struct {
	name string
	re   *regexp.Regexp
} {
	return []struct {
		name string
		re   *regexp.Regexp
	}{
		{"PEM private key", regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`)},
		{"URL with a password", regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9+.-]*://[^/@:\s]+:[^/@\s]+@`)},
		{"JSON Web Token", regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`)},
		{"AWS access key ID", regexp.MustCompile(`(AKIA|ASIA)[0-9A-Z]{16}`)},
		{"GitHub token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{36,}`)},
		{"GitHub fine-grained token", regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`)},
		{"Slack token", regexp.MustCompile(`xox[abposr]-[A-Za-z0-9-]{10,}`)},
		{"API secret key", regexp.MustCompile(`sk-(ant-)?[A-Za-z0-9_-]{20,}`)},
		{"Authorization header credentials", regexp.MustCompile(`(Bearer|Basic) [A-Za-z0-9._~+/-]{16,}=*`)},
	}
}

// checkDetectors fails when a matcher of newDetectors and its pattern
// disagree on s.
func checkDetectors(t testing.TB, ds []detector, s string) {
	t.Helper()
	for i, spec := range specDetectors() {
		if got, want := ds[i].match(s, noMeter), spec.re.MatchString(s); got != want {
			t.Fatalf("%s on %q: matcher %v, pattern %v", spec.name, s, got, want)
		}
	}
}

// detectorRows are the edge cases of each pattern: the shortest match and
// one byte short, runs ending at the required separator, overlapping and
// repeated prefixes, a match after a near miss, the anchor of the URL
// pattern, invalid UTF-8 and bytes outside each class.
func detectorRows() []string {
	a := func(n int) string { return strings.Repeat("a", n) }
	A := func(n int) string { return strings.Repeat("A", n) }
	return []string{
		"", "x", needles,
		"-----BEGIN PRIVATE KEY-----", "-----BEGIN RSA PRIVATE KEY-----", "-----BEGIN  PRIVATE KEY-----", "-----BEGINPRIVATE KEY-----",
		"-----BEGIN PRIVATE KEY----", "------BEGIN PRIVATE KEY-----x", "-----BEGIN rsa PRIVATE KEY-----", "-----BEGIN -----BEGIN PRIVATE KEY-----",
		"-----BEGIN PRIVATE KEYPRIVATE KEY-----", "-----BEGIN PRIVATE KEY PRIVATE KEY-----", "-----BEGIN X-----BEGIN PRIVATE KEY-----",
		"https://u:p@h", "https://u:p@", "https://u:@h", "https://:p@h", "1https://u:p@h", "x https://u:p@h", "h+t.t-p://u:p:q@h",
		"https://u:p/@h", "https://u/x:p@h", "https://u:p", "https:/u:p@h", "a://u:p@", "a://u u:p@", "a://u:p p@", "a://\xffu:\xfe@",
		"eyJ" + a(8) + ".eyJ" + a(8) + "." + a(8), "eyJ" + a(7) + ".eyJ" + a(8) + "." + a(8), "eyJ" + a(8) + ".eyJ" + a(8) + "." + a(7),
		"eyJeyJ" + a(5) + ".eyJ" + a(8) + "." + a(8), "eyJ" + a(8) + ".eyJ" + a(7) + "." + a(8), "eyJ" + a(8) + "..eyJ" + a(8) + "." + a(8),
		"x eyJ" + a(9) + ".eyJ" + a(8) + ".eyJ" + a(8) + "." + a(8), "eyJ" + a(8) + ".eyJ" + a(8) + ".eyJ" + a(8), "eyJ" + a(8) + ".eyJ" + a(8) + "." + a(3) + ".x",
		"AKIA" + A(16), "AKIA" + A(15), "ASIA" + A(16), "AKIAKIA" + A(13), "AKIA" + A(15) + "a", "xAKIA0123456789ABCDEF", "AKI" + A(17),
		"ghp_" + a(36), "ghp_" + a(35), "ghx_" + a(36), "gh" + "ghs_" + a(36), "gho_" + a(35) + "_", "ghr_" + a(40),
		"github_pat_" + a(22), "github_pat_" + a(21), "github_pat_github_pat_" + a(11), "github_pat" + a(30),
		"xoxb-" + a(10), "xoxb-" + a(9), "xoxz-" + a(10), "xoxoxb-" + a(10), "xox-" + a(11), "xoxb-xoxb-" + a(5),
		"sk-" + a(20), "sk-" + a(19), "sk-ant-" + a(20), "sk-ant-" + a(13), "sk-sk-" + a(17), "sk-ant" + a(15),
		"Bearer " + a(16), "Bearer " + a(15), "Basic " + a(16) + "==", "Basic " + a(15) + "=", "bearer " + a(16), "Bearer  " + a(16),
		"BearerBasic " + a(16), "Basic Bearer " + a(16), "Bearer " + a(15) + "\xff",
	}
}

// TestDetectorsMatchSpec covers the matchers that replace the regular
// expressions of the 01 J row (review finding: a regular expression run
// over a whole text could not report its work as it ran): each finds a
// match exactly when its pattern does, on every edge case and inside
// longer text, and the list keeps the row's order, so detect names the
// first detector that matches.
func TestDetectorsMatchSpec(t *testing.T) {
	ds := newDetectors()
	specs := specDetectors()
	if len(ds) != len(specs) {
		t.Fatalf("%d detectors, want %d", len(ds), len(specs))
	}
	for i := range ds {
		if ds[i].name != specs[i].name {
			t.Errorf("detector %d is %q, want %q", i, ds[i].name, specs[i].name)
		}
	}
	for _, row := range detectorRows() {
		for _, s := range []string{row, "pre " + row, row + " post", filler(20) + row + filler(20), row + row} {
			checkDetectors(t, ds, s)
			want := ""
			for _, spec := range specs {
				if spec.re.MatchString(s) {
					want = spec.name
					break
				}
			}
			if got := detect(ds, s, noMeter); got != want {
				t.Errorf("detect(%q) = %q, want %q", s, got, want)
			}
		}
	}
	r := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // G404: deterministic test data
	pieces := []string{
		"-----BEGIN ", "PRIVATE KEY", "-----", "://", ":", "@", "/", " ", "eyJ", ".", "AKIA", "ASIA", "gh", "p_", "_",
		"github_pat_", "xox", "b-", "-", "sk-", "ant-", "Bearer ", "Basic ", "=", "a", "A", "0", "aaaaaaaa", "AAAAAAAA", "\xff", "é", "\t",
	}
	for range raceScaled(20_000, 4_000) {
		var b strings.Builder
		for range r.IntN(12) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		checkDetectors(t, ds, b.String())
	}
}

// FuzzDetectors is the differential property of the matchers: on any
// text, each matcher of newDetectors agrees with its pattern of the 01 J
// row, and the bytes it reports are linear in the text.
func FuzzDetectors(f *testing.F) {
	for _, row := range detectorRows() {
		f.Add(row)
	}
	ds := newDetectors()
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 1<<12 {
			return
		}
		checkDetectors(t, ds, s)
		read, largest := 0, 0
		for i := range ds {
			ds[i].match(s, func(n int) { read += n; largest = max(largest, n) })
		}
		if read > 64*len(s)+64 || largest > textStep+len(needles) {
			t.Fatalf("%q: %d bytes reported (largest %d)", s, read, largest)
		}
	})
}

// TestMeteredText checks each counted text helper against the library
// call it replaces, on texts that cross window boundaries inside runes,
// and that it reports at most textStep and a window's overlap at a time.
func TestMeteredText(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // G404: deterministic test data
	alphabet := []string{"a", "A", "Z", "é", "€", "𝔄", "ǅ", "\xff", "\x80", " ", "${", "$", "K", "İ"}
	for range 300 {
		var b strings.Builder
		for range r.IntN(3 * textStep) {
			b.WriteString(alphabet[r.IntN(len(alphabet))])
		}
		s := b.String()
		largest := 0
		read := func(n int) { largest = max(largest, n) }
		if got, want := lower(s, read), strings.ToLower(s); got != want {
			t.Fatalf("lower(%q) differs from strings.ToLower", s)
		}
		if got, want := hasAlnum(s, read), strings.IndexFunc(s, isAlnum) >= 0; got != want {
			t.Fatalf("hasAlnum(%q) = %v, want %v", s, got, want)
		}
		for _, sub := range []string{"${", "é", "aa", "x"} {
			want := strings.Index(s, sub)
			if got := indexFrom(s, sub, 0, read); got != want {
				t.Fatalf("indexFrom(%q, %q) = %d, want %d", s, sub, got, want)
			}
		}
		var out strings.Builder
		appendText(&out, s, read)
		if out.String() != s {
			t.Fatal("appendText changed the text")
		}
		if largest > textStep+4 {
			t.Fatalf("a read of %d bytes, want at most %d", largest, textStep+4)
		}
		if end := runeCut(s, 0); end < len(s) && end > 0 && !utf8.RuneStart(s[end]) && !strings.ContainsRune(s[max(0, end-3):end+1], utf8.RuneError) {
			// a cut inside a valid rune
			if _, size := utf8.DecodeRuneInString(s[end-1:]); size > 1 {
				t.Fatalf("runeCut cut inside a rune at %d", end)
			}
		}
	}
	strs := []string{"b", "a", "", "a", "ab", strings.Repeat("x", 3*textStep) + "b", strings.Repeat("x", 3*textStep) + "a", strings.Repeat("x", 3*textStep)}
	want := slices.Compact(slices.Sorted(slices.Values(slices.Clone(strs))))
	if got := sortStrings(slices.Clone(strs), noMeter); !slices.Equal(got, want) {
		t.Errorf("sortStrings = %q, want %q", got, want)
	}
	for _, x := range strs {
		for _, y := range strs {
			if got, want := compareText(x, y, noMeter), strings.Compare(x, y); got != want {
				t.Errorf("compareText(%q, %q) = %d, want %d", x, y, got, want)
			}
		}
	}
	for _, s := range []string{"", "a", strings.Repeat("a", maxShown), strings.Repeat("a", maxShown) + "b", strings.Repeat("a", maxShown-1) + "é"} {
		got := clip(s)
		if len(s) <= maxShown && got != s || len(s) > maxShown && (!strings.HasSuffix(got, "...") || !utf8.ValidString(got) || len(got) > maxShown+3) {
			t.Errorf("clip(%q) = %q", s, got)
		}
	}
}

// TestItemElemBounded: itemElem names a keyed entry by its key, found by
// a counted scan, a string set element by its text and a composite one
// by its canonical JSON when that is at most maxShown bytes, else by its
// index, as config/canonical names it (review finding class:
// schemaidx.ItemElem encodes and sorts a whole set element, uncounted).
func TestItemElemBounded(t *testing.T) {
	x, _ := schemas(t).Index(v1)
	route, _ := x.Resource("Route")
	methods := route
	for _, name := range []string{"spec", "match", "methods"} {
		methods, _ = memberSchema(methods, name)
	}
	small, err := parseJSON(`{"b":"x","a":1}`, 0)
	if err != nil {
		t.Fatal(err)
	}
	wide := &tree.Node{Kind: tree.KindMap}
	for i := range 1 << 16 {
		wide.Members = append(wide.Members, tree.Member{Key: "m" + strconv.Itoa(i), Value: &tree.Node{Kind: tree.KindInt, Text: "1"}})
	}
	str := &tree.Node{Kind: tree.KindString, Text: strings.Repeat("s", 1<<16)}
	for _, tc := range []struct {
		name string
		item *tree.Node
		want diag.PathElem
	}{
		{"small", small, schemaidx.ItemElem(methods, small, 2)},
		{"wide", wide, diag.Index(2)},
		{"long string", str, diag.Item(str.Text)},
	} {
		w := &walker{s: New(Options{})}
		got := w.itemElem(methods, tc.item, 2)
		if g, want := (diag.Path{got}).String(), (diag.Path{tc.want}).String(); g != want {
			t.Errorf("%s: %.80s, want %.80s", tc.name, g, want)
		}
		if w.units > 4*maxShown {
			t.Errorf("%s: %d units, want the walk to stop past maxShown bytes", tc.name, w.units)
		}
	}
	// A wide element is never encoded: schemaidx.ItemElem would copy and
	// sort its members and write all of its JSON.
	w := &walker{s: New(Options{})}
	if b := allocated(func() { _ = w.itemElem(methods, wide, 2) }); b > 64<<10 {
		t.Errorf("naming a wide set element allocated %d bytes, want it not encoded", b)
	}
}

// TestSelectOf covers the bounded dispatch of selectOf (review finding:
// schemaidx Select read every member once per rule, uncounted): on every
// dispatching node of the committed schema and on a synthetic schema
// whose dispatch takes two rounds, selectOf returns what Select returns,
// for short and long objects with repeated keys, and Select reads at most
// the declared members.
func TestSelectOf(t *testing.T) {
	r := rand.New(rand.NewPCG(9, 10)) //nolint:gosec // G404: deterministic test data
	committed, _ := schemas(t).Index(v1)
	two, err := schemaidx.Load("test/v1", []byte(`{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
  "$defs": {
    "Thing": {"type": "object", "properties": {"kind": {"const": "Thing"}, "spec": {"$ref": "#/$defs/Spec"}}},
    "Spec": {"type": "object", "properties": {"mode": {"type": "string"}},
      "allOf": [{"if": {"properties": {"mode": {"const": "a"}}, "required": ["mode"]},
        "then": {"properties": {"sub": {"type": "string"}},
          "allOf": [{"if": {"properties": {"sub": {"const": "x"}}, "required": ["sub"]}, "then": {"properties": {"ref": {"type": "string", "x-ruralz-ref": "Thing"}}}}]}}]}
  }
}`))
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, x := range []*schemaidx.Index{committed, two} {
		nodes := map[*schemaidx.Node]map[string][]string{}
		for f := range x.FieldsSeq() {
			for _, name := range f.Node.PropertyNames() {
				if vs := f.Node.DispatchValues(name); len(vs) > 0 {
					if nodes[f.Node] == nil {
						nodes[f.Node] = map[string][]string{}
					}
					nodes[f.Node][name] = vs
				}
			}
		}
		for static, tested := range nodes {
			var names []string
			for name := range tested {
				names = append(names, name)
			}
			names = append(names, "sub", "other")
			slices.Sort(names)
			declared := static.PropertyNames()
			for range 300 {
				inst := &tree.Node{Kind: tree.KindMap}
				for range []int{2, linearScan, linearScan + 1, 3 * linearScan}[r.IntN(4)] {
					key := "u" + strconv.Itoa(r.IntN(50))
					switch k := r.IntN(10); {
					case k < 4:
						key = names[r.IntN(len(names))]
					case k < 6 && len(declared) > 0:
						key = declared[r.IntN(len(declared))]
					}
					text := "x"
					if vs := tested[key]; len(vs) > 0 && r.IntN(3) > 0 {
						text = vs[r.IntN(len(vs))]
					}
					inst.Members = append(inst.Members, tree.Member{Key: key, Value: &tree.Node{Kind: tree.KindString, Text: text}})
				}
				w := &walker{s: New(Options{}), probe: &probe{}}
				got, want := w.selectOf(static, inst), static.Select(inst)
				if got != want && (got.Def() != want.Def() || !slices.Equal(got.PropertyNames(), want.PropertyNames())) {
					t.Fatalf("selectOf(%s) differs from Select", dump(inst))
				}
				if w.probe.selectWidth > max(linearScan, len(declared)+2) {
					t.Errorf("Select read %d members", w.probe.selectWidth)
				}
				checked++
			}
		}
	}
	// PolicySpec in the committed schema, Spec in the synthetic one.
	if checked < 600 {
		t.Errorf("checked %d instances, want 300 for each dispatching node", checked)
	}
	spec, _ := two.Spec("Thing")
	inst := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "mode", Value: &tree.Node{Kind: tree.KindString, Text: "a"}}}}
	for i := range linearScan {
		inst.Members = append(inst.Members, tree.Member{Key: "u" + strconv.Itoa(i), Value: &tree.Node{Kind: tree.KindString}})
	}
	inst.Members = append(inst.Members, tree.Member{Key: "sub", Value: &tree.Node{Kind: tree.KindString, Text: "x"}})
	w := &walker{s: New(Options{})}
	if ref, ok := w.selectOf(spec, inst).Property("ref"); !ok || ref.Keywords().Ref == "" {
		t.Error("two rounds: ref is not a reference after mode a and sub x")
	}
}
