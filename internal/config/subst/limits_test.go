// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// repeat joins n texts f(i) with sep.
func repeat(n int, sep string, f func(i int) string) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = f(i)
	}
	return strings.Join(parts, sep)
}

// labelsRoute is a Route whose metadata.labels hold n members k<i>: v(i),
// both JSON-safe.
func labelsRoute(n int, key, v func(i int) string) string {
	labels := repeat(n, ",", func(i int) string { return quote(key(i)) + ":" + quote(v(i)) })
	return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r","labels":{` + labels + `}},"spec":{}}`
}

func keyN(i int) string { return "k" + strconv.Itoa(i) }

// raceScaled returns full, or race in a test binary built with -race
// (raceEnabled): a size the asymptotic checks still measure.
func raceScaled(full, race int) int {
	if raceEnabled {
		return race
	}
	return full
}

// testBudget is the diagnostic budget TestDiagnosticBudget sets:
// DefaultMaxDiagnostics, or a tenth of it under -race, where the budget
// check, which needs some multiple of it in occurrences, runs at that
// size (TestMaxDiagnosticsOption covers a budget other than the default).
func testBudget() int { return raceScaled(DefaultMaxDiagnostics, DefaultMaxDiagnostics/10) }

// substProducer builds a resource with k occurrences of one stage E
// diagnostic. list marks a producer whose occurrences are list elements
// past which, once the budget is spent, the walk does no work.
type substProducer struct {
	name, code string
	build      func(k int) string
	list       bool
}

func substProducers() []substProducer {
	return []substProducer{
		{"req27 undefined variables in one scalar", CodeUndefined, func(k int) string {
			return prefixRoute(strings.Repeat("${X}", k))
		}, false},
		{"req27 undefined variables in many scalars", CodeUndefined, func(k int) string {
			return labelsRoute(k, keyN, func(int) string { return "${U}" })
		}, false},
		{"req30 secret-like names in one scalar", CodeWarning, func(k int) string {
			return prefixRoute(strings.Repeat("${TOKEN:-}", k))
		}, false},
		{"req26 malformed scalars", CodeMalformed, func(k int) string {
			return labelsRoute(k, keyN, func(int) string { return "${" })
		}, false},
		{"req28 expressions in mapping keys", CodeForbidden, func(k int) string {
			return labelsRoute(k, func(i int) string { return "${K" + strconv.Itoa(i) + "}" }, func(int) string { return "v" })
		}, false},
		{"req28 expressions in reference fields", CodeForbidden, func(k int) string {
			ups := repeat(k, ",", func(i int) string { return `{"name":"${A` + strconv.Itoa(i) + `}"}` })
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[` + ups + `]}}`
		}, true},
		{"J013 credential-shaped literals", CodeWarning, func(k int) string {
			return labelsRoute(k, keyN, func(int) string { return "AKIAABCDEFGHIJKLMNOP" })
		}, false},
		{"J013 credential header literals", CodeWarning, func(k int) string {
			set := repeat(k, ",", func(i int) string { return `{"name":"authorization","value":"v` + strconv.Itoa(i) + `"}` })
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"set":[` + set + `]}}}}`
		}, true},
	}
}

// TestDiagnosticBudget covers 01 req 50 and risk 1 (review finding): one
// Apply call records the first MaxDiagnostics+1 diagnostics in walk
// order, warnings included, so the caller's cap appends its RZ-CFG-001,
// and stops, so a resource with many more occurrences allocates no more.
// Every producer of a per-occurrence diagnostic is one row. The budget is
// the default one, but under -race (testBudget).
func TestDiagnosticBudget(t *testing.T) {
	budget := testBudget()
	k := budget + 50
	for _, p := range substProducers() {
		t.Run(p.name, func(t *testing.T) {
			s := New(Options{Schemas: schemas(t), MaxDiagnostics: budget})
			parse := func(k int) *tree.Resource {
				return docs(t, &tree.FileTable{}, "r.yaml", p.build(k))[0]
			}
			ds := s.Apply(parse(k))
			if len(ds) != budget+1 {
				t.Fatalf("%d diagnostics, want %d", len(ds), budget+1)
			}
			for i, d := range ds {
				if d.Code != p.code {
					t.Fatalf("diagnostic %d is %s, want %s", i, d.AppendText(nil), p.code)
				}
			}
			c := diag.NewCollector(budget)
			c.AddAll(ds)
			if l := c.List(); len(l) != budget+1 || !c.Full() || l.FirstErrorCode() != "RZ-CFG-001" {
				t.Errorf("a collector with the same cap kept %d diagnostics (full %v, first error %s), want %d and RZ-CFG-001",
					len(l), c.Full(), l.FirstErrorCode(), budget+1)
			}
			allocs := func(k int) float64 {
				rs := []*tree.Resource{parse(k), parse(k)}
				i := 0
				return testing.AllocsPerRun(1, func() {
					_ = s.Apply(rs[i])
					i++
				})
			}
			if a1, a2 := allocs(k), allocs(2*k); a2 > a1+float64(k/8) {
				t.Errorf("%.0f allocations for %d occurrences, %.0f for %d: the walk goes on past the budget", a1, k, a2, 2*k)
			}
			if !p.list {
				return
			}
			units := func(k int) int {
				calls := 0
				_ = New(Options{Schemas: schemas(t), MaxDiagnostics: budget, Yield: func() { calls++ }}).Apply(parse(k))
				return calls
			}
			if w1, w2 := units(k), units(2*k); w2 > w1+2 {
				t.Errorf("%d Yield calls for %d occurrences, %d for %d: the walk goes on past the budget", w1, k, w2, 2*k)
			}
		})
	}
}

// TestReportAfterBudget: once the diagnostic beyond the budget is
// recorded, report records nothing more, whatever still calls it.
func TestReportAfterBudget(t *testing.T) {
	w := &walker{s: New(Options{MaxDiagnostics: 1})}
	for range 3 {
		w.report(diag.Diagnostic{Code: CodeUndefined})
	}
	if len(w.diags) != 2 || !w.full {
		t.Errorf("%d diagnostics recorded (full %v), want 2 and full", len(w.diags), w.full)
	}
}

// TestMaxDiagnosticsOption: Options.MaxDiagnostics sets the budget and the
// first diagnostics in walk order are kept, one more than the budget when
// the input has more; the scalar that reached it keeps its authored text.
func TestMaxDiagnosticsOption(t *testing.T) {
	src := prefixRoute("${A}${B}${C}${D}${E}")
	for _, tc := range []struct {
		max  int
		want string
	}{
		{3, "A B C D"},
		{5, "A B C D E"},
		{0, "A B C D E"},
	} {
		r := docs(t, &tree.FileTable{}, "r.yaml", src)[0]
		ds := New(Options{Schemas: schemas(t), MaxDiagnostics: tc.max}).Apply(r)
		var got []string
		for _, d := range ds {
			got = append(got, strings.TrimSuffix(strings.TrimPrefix(d.Message, `variable "`), `" is not defined and has no default`))
		}
		if g := strings.Join(got, " "); g != tc.want {
			t.Errorf("MaxDiagnostics %d: %s, want %s", tc.max, g, tc.want)
		}
		if text := at(t, r.Root, "spec.match.path.prefix").Text; text != "${A}${B}${C}${D}${E}" {
			t.Errorf("MaxDiagnostics %d: text %q, want the authored text", tc.max, text)
		}
	}
}

// scaleShape is a stage E input of size n for the scale tests.
type scaleShape struct {
	name string
	// code is the only error code the shape raises, "" for none, and also
	// a second one it raises beside it.
	code, also string
	build      func(n int) string
	// vars are the variables (Map{"A": "a"} when nil) and hints the
	// HintNames.
	vars  Map
	hints func(n int) []string
}

// nearNames returns the 64 names diag.Nearest compares, of length l,
// that differ from "A"*l in their last byte, so each is one edit away
// from an undefined "A"*(l-1)+"C" and none matches it case-insensitively.
func nearNames(l int) []string {
	out := make([]string, hintCompared)
	for i := range out {
		out[i] = strings.Repeat("A", l-1) + string(rune('D'+i%20)) + strings.Repeat("Q", i/20)
	}
	return out
}

// scaleShapes are stage E inputs of size n that cost quadratic time when
// a lookup or a search re-reads earlier work per item (review findings:
// the Vars dedupe scanned every earlier name per expression; the lenient
// re-read of a malformed scalar searched to its end at each ${; the
// RZ-CFG-010 hint compared names in a full table, at every occurrence).
// Each shape's size is linear in n, in bytes as in items.
func scaleShapes() []scaleShape {
	return []scaleShape{
		{name: "distinct variables in one scalar", build: func(n int) string {
			return prefixRoute(repeat(n, "", func(i int) string { return "${V" + strconv.Itoa(i) + ":-}" }))
		}},
		{name: "one variable repeated in one scalar", build: func(n int) string {
			return prefixRoute(strings.Repeat("${A}", n))
		}},
		{name: "labels with expressions", build: func(n int) string {
			return labelsRoute(n, keyN, func(i int) string { return "a${A}" + strconv.Itoa(i) })
		}},
		{name: "credential header entries", build: func(n int) string {
			set := repeat(n, ",", func(int) string { return `{"name":"x-api-key","value":"${A}"}` })
			return `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"h"},"spec":{"type":"headers","config":{"request":{"set":[` + set + `]}}}}`
		}},
		// 01 req 54, S14 hostile input: malformed scalars, which the
		// credential detectors re-read leniently.
		{name: "S14 unterminated defaults", code: CodeMalformed, build: func(n int) string {
			return prefixRoute(strings.Repeat("${A:-", 16*n))
		}},
		{name: "S14 nested defaults closed at the end", code: CodeMalformed, build: func(n int) string {
			return prefixRoute(strings.Repeat("${A:-", 16*n) + "}")
		}},
		{name: "S14 nested defaults each closed", code: CodeMalformed, build: func(n int) string {
			return prefixRoute(strings.Repeat("${A:-", 8*n) + strings.Repeat("}", 8*n))
		}},
		{name: "S14 long names without a brace", code: CodeMalformed, build: func(n int) string {
			return prefixRoute(repeat(4*n, " ", func(int) string { return "${" + strings.Repeat("N", 64) }))
		}},
		{name: "S14 one long name without a brace", code: CodeMalformed, build: func(n int) string {
			return prefixRoute("${" + strings.Repeat("N", 256*n))
		}},
		// B is defined: the lenient read of ${A:-${B}} uses it (01 req 27),
		// which the next shape covers.
		{name: "S14 every malformed form repeated", code: CodeMalformed, vars: Map{"A": "a", "B": "b"}, build: func(n int) string {
			return prefixRoute(strings.Repeat("${1X} ${X-d} ${X:=d} ${A:-${B}} ${ ", 4*n))
		}},
		// 01 req 27 in a malformed scalar (review finding): each
		// well-formed use of an undefined variable, read leniently, is one
		// RZ-CFG-010 beside the scalar's RZ-CFG-005.
		{name: "req27 undefined uses in a malformed scalar", code: CodeMalformed, also: CodeUndefined, build: func(n int) string {
			return prefixRoute(strings.Repeat("${U} ${A:-${V}} ", n) + "${")
		}},
		{name: "S14 malformed scalars in many labels", code: CodeMalformed, build: func(n int) string {
			return labelsRoute(n, keyN, func(int) string { return strings.Repeat("${A:-", 16) })
		}},
		// 01 req 27 and 54: the RZ-CFG-010 hint over long names.
		{
			name: "req27 one long undefined name with near hints", code: CodeUndefined,
			build: func(n int) string { return prefixRoute("${" + strings.Repeat("A", 4*n-1) + "C}") },
			hints: func(n int) []string { return nearNames(4 * n) },
		},
		{
			name: "req27 one long undefined name differing at both ends from every hint", code: CodeUndefined,
			build: func(n int) string { return prefixRoute("${C" + strings.Repeat("A", 4*n-2) + "C}") },
			hints: func(n int) []string { return endNames(4 * n) },
		},
		{
			name: "req27 a long undefined name used many times", code: CodeUndefined,
			build: func(n int) string {
				return labelsRoute(n/8, keyN, func(int) string { return "${" + strings.Repeat("A", 1023) + "C}" })
			},
			hints: func(int) []string { return nearNames(1024) },
		},
		{
			name: "req27 distinct undefined names with near hints", code: CodeUndefined,
			build: func(n int) string {
				return labelsRoute(n, keyN, func(i int) string { return "${" + strings.Repeat("A", 59) + "C" + fmt4(i) + "}" })
			},
			hints: func(int) []string { return nearNames(64) },
		},
		// 01 req 6 and 54: the text written grows with the uses of a value.
		{name: "req6 a 1 KiB value used many times", vars: Map{"A": strings.Repeat("v", 1024)}, build: func(n int) string {
			return prefixRoute(strings.Repeat("${A}", n))
		}},
	}
}

// fmt4 is i as four decimal digits.
func fmt4(i int) string {
	s := strconv.Itoa(10000 + i%10000)
	return s[1:]
}

// work returns the Yield calls of one Apply over shape at size n: Yield
// runs every 256 work units, so the count measures the work without a
// clock.
func work(t *testing.T, sh scaleShape, n int) int {
	t.Helper()
	calls := 0
	s := New(sh.options(t, n, func() { calls++ }))
	r := docs(t, &tree.FileTable{}, "r.yaml", sh.build(n))[0]
	for _, d := range s.Apply(r) {
		if d.Severity == diag.SeverityError && d.Code != sh.code && (sh.also == "" || d.Code != sh.also) {
			t.Fatalf("%s", d.AppendText(nil))
		}
	}
	return calls
}

// options returns the Options of shape sh at size n.
func (sh scaleShape) options(t testing.TB, n int, yield func()) Options {
	t.Helper()
	vars := sh.vars
	if vars == nil {
		vars = Map{"A": "a"}
	}
	var hints []string
	if sh.hints != nil {
		hints = sh.hints(n)
	}
	return Options{Schemas: schemas(t), Variables: vars, HintNames: hints, Yield: yield}
}

// TestScaleLinear covers the review findings on quadratic stage E work
// (01 req 54, S14 hostile input): four times the input costs at most
// about four times the work units, which count every node, segment,
// variable name compared and byte read, written or compared, so a search
// re-reading earlier text or a dedupe scanning every earlier name costs
// sixteen times.
func TestScaleLinear(t *testing.T) {
	n := raceScaled(2000, 500)
	for _, sh := range scaleShapes() {
		t.Run(sh.name, func(t *testing.T) {
			w1, w4 := work(t, sh, n), work(t, sh, 4*n)
			t.Logf("%d Yield calls at n=%d, %d at n=%d (ratio %.2f)", w1, n, w4, 4*n, float64(w4)/float64(w1))
			if w1 < n/yieldEvery {
				t.Fatalf("%d work units at n=%d: the shape does too little to measure", w1*yieldEvery, n)
			}
			if w4 > 4*w1+w1/2+4 {
				t.Errorf("work grows faster than the input: %d Yield calls at n=%d, %d at n=%d (ratio %.1f, want about 4)",
					w1, n, w4, 4*n, float64(w4)/float64(w1))
			}
		})
	}
}

// TestYield covers 01 req 53 (review finding): Apply calls Options.Yield
// every 256 work units inside one resource, a node, a segment, a name
// compared or byteUnit bytes each counting one or more, and never without
// a Yield set.
func TestYield(t *testing.T) {
	const n = 1024
	for _, sh := range scaleShapes() {
		calls := 0
		s := New(sh.options(t, n, func() { calls++ }))
		_ = s.Apply(docs(t, &tree.FileTable{}, "r.yaml", sh.build(n))[0])
		if calls < n/yieldEvery {
			t.Errorf("%s: %d Yield calls for %d items, want at least %d", sh.name, calls, n, n/yieldEvery)
		}
	}
	if ds := New(Options{Schemas: schemas(t)}).Apply(docs(t, &tree.FileTable{}, "r.yaml", scaleShapes()[0].build(n))[0]); len(ds) != 0 {
		t.Errorf("without Yield: %q", texts(ds))
	}
}

// TestYieldInsideLongScalar covers 01 req 53 inside one node (review
// finding: a malformed scalar of several MiB was one unit of work, so
// Yield never ran while it was scanned): the scan reports the bytes it
// reads at least every searchWindow bytes, so a single scalar of 4 MiB
// yields many times before the walk reaches its next node.
func TestYieldInsideLongScalar(t *testing.T) {
	size := raceScaled(4<<20, 1<<20)
	for name, text := range map[string]string{
		"unterminated defaults": strings.Repeat("${A:-", size/5),
		"one long name":         "${" + strings.Repeat("N", size),
		"a literal":             strings.Repeat("x", size) + "${A}",
		"nested defaults":       strings.Repeat("${A:-", size/5) + "}",
	} {
		var calls []int
		s := New(Options{Schemas: schemas(t), Variables: Map{"A": "a"}})
		r := docs(t, &tree.FileTable{}, "r.yaml", prefixRoute(text))[0]
		n := at(t, r.Root, "spec.match.path.prefix")
		w := &walker{s: s, id: r.ID, yield: func() { calls = append(calls, len(calls)) }}
		w.scalar(n, place{})
		if least := size / byteUnit / yieldEvery; len(calls) < least {
			t.Errorf("%s: %d Yield calls inside one %d-byte scalar, want at least %d", name, len(calls), size, least)
		}
	}
}

// TestSearchWindow: every search of the scan reports what it read at
// least every searchWindow bytes, so a long scalar yields while one
// search crosses it, not only after (01 req 53), and the reports add up
// to a bounded number of reads per byte, in both modes.
func TestSearchWindow(t *testing.T) {
	const size = 1 << 20
	for name, text := range map[string]string{
		"a literal":                   strings.Repeat("x", size),
		"one unterminated default":    "${A:-" + strings.Repeat("x", size),
		"a default closed at the end": "${A:-" + strings.Repeat("x", size) + "}",
		"one long name":               "${" + strings.Repeat("N", size),
	} {
		for _, lenient := range []bool{false, true} {
			largest, total := 0, 0
			scanEach(text, lenient, func(n int) {
				largest = max(largest, n)
				total += n
			}, func(segment) bool { return true })
			if largest > searchWindow+1 {
				t.Errorf("%s (lenient %v): one report of %d bytes, want at most %d", name, lenient, largest, searchWindow+1)
			}
			if total > 3*len(text) {
				t.Errorf("%s (lenient %v): %d bytes read for %d", name, lenient, total, len(text))
			}
		}
	}
}

// BenchmarkScaleShapes substitutes each scaleShapes input at 10,000 items.
func BenchmarkScaleShapes(b *testing.B) {
	for _, sh := range scaleShapes() {
		b.Run(sh.name, func(b *testing.B) {
			s := New(sh.options(b, 10_000, nil))
			r := docs(b, &tree.FileTable{}, "r.yaml", sh.build(10_000))[0]
			b.ReportAllocs()
			for range b.N {
				b.StopTimer()
				c := &tree.Resource{ID: r.ID, APIVersion: r.APIVersion, Root: r.Root.Clone()}
				b.StartTimer()
				_ = s.Apply(c)
			}
		})
	}
}
