// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"math"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// limitText is the text of LimitDiagnostic at a limit of limit bytes.
func limitText(limit string) string {
	return "- error RZ-CFG-001: substitution writes more than " + limit + " bytes of text in one render " +
		"(each use of a variable repeats its value: keep large values out of variables used often)"
}

// allocated returns the bytes f allocates: runtime.MemStats.TotalAlloc
// counts every allocation exactly (it is not sampled), and the package's
// tests run one at a time.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// TestOutputLimitHostile covers the review finding on substitution
// amplification (01 req 6 limits, 01 J row 003 "bounded heap", 01 req
// 54): k uses of a v-byte variable would write k*v bytes from a source of
// a few KiB, so the text one render writes is bounded by
// Options.MaxOutputBytes (64 MiB by default): the call returns the
// RZ-CFG-001 limit error alone, the scalar keeps its authored text and
// nothing near the amplified size is allocated. Each row is one way to
// multiply text.
func TestOutputLimitHostile(t *testing.T) {
	big := strings.Repeat("v", 256<<10)
	kib := strings.Repeat("w", 1<<10)
	dflt := strings.Repeat("d", 64<<10)
	cases := []struct {
		name, src string
		vars      Map
		// max is Options.MaxOutputBytes (0: the default); allowed the
		// bytes Apply may allocate.
		max     int64
		allowed uint64
	}{
		{"a 256 KiB value used 1024 times in one scalar", prefixRoute(strings.Repeat("${A}", 1024)), Map{"A": big}, 0, 16 << 20},
		{"a 1 KiB value used 70000 times in one scalar", prefixRoute(strings.Repeat("${A}", 70_000)), Map{"A": kib}, 0, 16 << 20},
		{"a 256 KiB value in 1024 labels", labelsRoute(1024, keyN, func(int) string { return "${A}" }), Map{"A": big}, 0, (64 + 16) << 20},
		{"a 64 KiB default of 20 expressions", prefixRoute(strings.Repeat("${U:-"+dflt+"}", 20)), nil, 1 << 20, 4 << 20},
		{"an empty variable's 64 KiB default", prefixRoute(strings.Repeat("${E:-"+dflt+"}", 20)), Map{"E": ""}, 1 << 20, 4 << 20},
	}
	for _, tc := range cases {
		t.Run("req6 "+tc.name, func(t *testing.T) {
			r := docs(t, &tree.FileTable{}, "r.yaml", tc.src)[0]
			authored := dump(r.Root)
			s := New(Options{Schemas: schemas(t), Variables: tc.vars, MaxOutputBytes: tc.max})
			var ds diag.List
			bytes := allocated(func() { ds = s.Apply(r) })
			if g, want := texts(ds), limitText(itoa(s.opts.MaxOutputBytes)); !slices.Equal(g, []string{want}) {
				t.Fatalf("diagnostics %q, want the limit error alone", g)
			}
			if !s.Spent() {
				t.Error("Spent() = false after the limit error")
			}
			if !strings.Contains(tc.name, "labels") && dump(r.Root) != authored {
				t.Error("the scalar past the limit was rewritten")
			}
			if bytes > tc.allowed {
				t.Errorf("Apply allocated %d bytes, want at most %d (the limit stops the text before it is built)", bytes, tc.allowed)
			}
		})
	}
}

// TestOutputLimitBoundary pins what the limit counts and when it is
// spent: the substituted text of every scalar substitution rewrites
// (literal parts, values and defaults, and the unescaped text of a
// forbidden position), summed over every call on the Substituter; a
// total equal to the limit is allowed, one byte more is not; and once it
// is spent, every call returns LimitDiagnostic alone, the diagnostics the
// call had recorded included, whatever resource it walks.
func TestOutputLimitBoundary(t *testing.T) {
	route := func(text string) *tree.Resource { return docs(t, &tree.FileTable{}, "r.yaml", prefixRoute(text))[0] }
	cel := func(text string) *tree.Resource {
		return docs(t, &tree.FileTable{}, "r.yaml", `{"apiVersion":"ruralz/v1alpha1","kind":"Route","metadata":{"name":"r"},"spec":{"match":{"when":`+quote(text)+`}}}`)[0]
	}
	vars := Map{"A": "aaaa", "E": ""}
	labels := func(vs ...string) *tree.Resource {
		return docs(t, &tree.FileTable{}, "r.yaml", labelsRoute(len(vs), keyN, func(i int) string { return vs[i] }))[0]
	}
	for _, tc := range []struct {
		name  string
		max   int64
		calls []*tree.Resource
		// want holds the diagnostics of each call ("limit" for the limit
		// error alone); spent the final state.
		want  [][]string
		spent bool
	}{
		{"values and literal parts at the limit", 10, []*tree.Resource{route("xx${A}${A}")}, [][]string{nil}, false},
		{"one byte past the limit", 9, []*tree.Resource{route("xx${A}${A}")}, [][]string{{"limit"}}, true},
		{"defaults count", 5, []*tree.Resource{route("${U:-dddddd}")}, [][]string{{"limit"}}, true},
		{"an empty variable's default counts", 5, []*tree.Resource{route("${E:-dddddd}")}, [][]string{{"limit"}}, true},
		{"escapes in a free position count", 5, []*tree.Resource{route("$${XXXXX}")}, [][]string{{"limit"}}, true},
		{"escapes in a forbidden position count", 5, []*tree.Resource{cel("$${XXXXX}")}, [][]string{{"limit"}}, true},
		{"plain strings do not count", 1, []*tree.Resource{route("no expression at all")}, [][]string{nil}, false},
		{"summed over calls", 12, []*tree.Resource{route("${A}${A}"), route("${A}"), route("${A}")}, [][]string{nil, nil, {"limit"}}, true},
		{"every later call returns it alone", 4, []*tree.Resource{route("${A}${A}"), route("plain"), route("${U}")}, [][]string{{"limit"}, {"limit"}, {"limit"}}, true},
		{"the call's other diagnostics are dropped", 4, []*tree.Resource{labels("${U}", "${A}${A}")}, [][]string{{"limit"}}, true},
		{"a scalar left with its authored text writes nothing", 4, []*tree.Resource{route("xxxxxxxx${U}${A}")}, [][]string{{
			`- error RZ-CFG-010 Route/r spec.match.path.prefix: variable "U" is not defined and has no default`,
		}}, false},
	} {
		t.Run("req6 "+tc.name, func(t *testing.T) {
			s := New(Options{Schemas: schemas(t), Variables: vars, MaxOutputBytes: tc.max})
			for i, r := range tc.calls {
				want := tc.want[i]
				if slices.Equal(want, []string{"limit"}) {
					want = []string{limitText(itoa(tc.max))}
				}
				if got := texts(s.Apply(r)); !slices.Equal(got, want) {
					t.Errorf("call %d: diagnostics %q, want %q", i, got, want)
				}
			}
			if s.Spent() != tc.spent {
				t.Errorf("Spent() = %v, want %v", s.Spent(), tc.spent)
			}
		})
	}
	t.Run("req6 the largest limit never overflows", func(t *testing.T) {
		s := New(Options{Schemas: schemas(t), Variables: Map{"A": strings.Repeat("a", 1<<10)}, MaxOutputBytes: math.MaxInt64})
		for range 4 {
			if ds := s.Apply(route(strings.Repeat("${A}", 1024))); len(ds) != 0 || s.Spent() {
				t.Fatalf("diagnostics %q, spent %v", texts(ds), s.Spent())
			}
		}
		if !s.take(math.MaxInt64-4<<20) || s.take(1) || !s.Spent() {
			t.Error("the count at the largest limit is not exact")
		}
	})
	t.Run("req6 nothing fits once the limit is spent", func(t *testing.T) {
		s := New(Options{MaxOutputBytes: 10})
		if !s.take(4) || s.take(7) || s.take(1) || s.take(0) || !s.Spent() {
			t.Error("a count fit after the limit was spent")
		}
	})
	t.Run("req6 the limit diagnostic names no file, resource or path", func(t *testing.T) {
		d := New(Options{}).LimitDiagnostic()
		if d.Location != (diag.Location{}) || d.Resource != nil || len(d.Path) != 0 || d.Code != CodeLimit || d.Severity != diag.SeverityError {
			t.Errorf("LimitDiagnostic() = %+v", d)
		}
	})
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 10 {
		b = append([]byte{byte('0' + n%10)}, b...)
	}
	return string(b)
}

// TestOutputLimitWorkers covers the determinism of the shared limit (01
// req 50 identical output whatever the worker count): whether the limit
// is spent depends only on the text every resource would write, not on
// how many workers run or in which order, and every call that reports it
// returns the same single diagnostic.
func TestOutputLimitWorkers(t *testing.T) {
	const resources = 64
	srcs := make([]string, resources)
	for i := range srcs {
		srcs[i] = prefixRoute(strings.Repeat("${A}", 1+i%7))
	}
	total := int64(0)
	for i := range srcs {
		total += int64(4 * (1 + i%7))
	}
	for _, tc := range []struct {
		max   int64
		spent bool
	}{{total, false}, {total - 1, true}, {total / 2, true}} {
		for _, workers := range []int{1, 3, 8, resources} {
			s := New(Options{Schemas: schemas(t), Variables: Map{"A": "aaaa"}, MaxOutputBytes: tc.max})
			rs := make([]*tree.Resource, resources)
			for i, src := range srcs {
				rs[i] = docs(t, &tree.FileTable{}, "r.yaml", src)[0]
			}
			results := make([]diag.List, resources)
			var wg sync.WaitGroup
			next := make(chan int, resources)
			for i := range rs {
				next <- i
			}
			close(next)
			for range workers {
				wg.Go(func() {
					for i := range next {
						results[i] = s.ApplyWith(rs[i], nil)
					}
				})
			}
			wg.Wait()
			if s.Spent() != tc.spent {
				t.Errorf("limit %d, %d workers: Spent() = %v, want %v", tc.max, workers, s.Spent(), tc.spent)
			}
			reported := 0
			for _, ds := range results {
				for _, d := range ds {
					if d.Code == CodeLimit {
						reported++
						if len(ds) != 1 || string(d.AppendText(nil)) != string(s.LimitDiagnostic().AppendText(nil)) {
							t.Errorf("limit %d, %d workers: a call returned %q", tc.max, workers, texts(ds))
						}
					}
				}
			}
			if (reported > 0) != tc.spent {
				t.Errorf("limit %d, %d workers: %d calls reported the limit, spent %v", tc.max, workers, reported, tc.spent)
			}
		}
	}
}
