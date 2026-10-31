// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"math"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestMaxDiagnosticsOption checks the caller's cap: with
// Options.MaxDiagnostics c, ValidateWith returns the first min(n, c+1) of
// a resource's n findings in sorted order, for n at the cap, one past it,
// at 2c+1 and 2c+2 (where the mapping prunes) and far past it, for small
// caps; zero and negative caps mean MaxDiagnostics. The finding past the
// cap is kept, so a truncated list never looks complete.
func TestMaxDiagnosticsOption(t *testing.T) {
	v := testView(t)
	for _, c := range []int{1, 7} {
		for _, n := range []int{c - 1, c, c + 1, 2*c + 1, 2*c + 2, 5*c + 3} {
			t.Run("cap "+strconv.Itoa(c)+" findings "+strconv.Itoa(n), func(t *testing.T) {
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", largeRoute(n))
				all := v.Validate(res, files)
				if len(all) != n {
					t.Fatalf("Validate() returned %d diagnostics, want %d", len(all), n)
				}
				got := v.ValidateWith(res, files, Options{MaxDiagnostics: c})
				if want := all[:min(n, c+1)]; !slices.EqualFunc(got, want, sameDiagnostic) {
					t.Errorf("ValidateWith(cap %d) = %d diagnostics, want the first %d of %d", c, len(got), len(want), n)
				}
			})
		}
	}
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", largeRoute(30))
	want := v.Validate(res, files)
	for _, c := range []int{0, -1, -10000, math.MinInt} {
		if got := v.ValidateWith(res, files, Options{MaxDiagnostics: c}); !slices.EqualFunc(got, want, sameDiagnostic) {
			t.Errorf("ValidateWith(cap %d) = %d diagnostics, want the default cap's %d", c, len(got), len(want))
		}
	}
	// Caps from math.MaxInt/2 up return every finding: no count derived from
	// the cap, such as 2*(cap+1), wraps and panics on the first finding.
	for _, c := range []int{math.MaxInt/2 - 1, math.MaxInt / 2, math.MaxInt/2 + 1, math.MaxInt - 1, math.MaxInt} {
		for _, n := range []int{1, 3, 30} {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", largeRoute(n))
			want := v.Validate(res, files)
			if got := v.ValidateWith(res, files, Options{MaxDiagnostics: c}); len(want) != n || !slices.EqualFunc(got, want, sameDiagnostic) {
				t.Errorf("ValidateWith(cap %d) = %d diagnostics, want all %d", c, len(got), n)
			}
		}
	}
}

// TestMapperKeep checks the kept count newMapper derives from the cap,
// limit+1 with limit taken as at least 1 and at most math.MaxInt-1, and
// that recording entries under a cap of math.MaxInt/2 or more never
// prunes: keep+1 and 2*keep never overflow, which would prune on the
// first entry and slice out of range.
func TestMapperKeep(t *testing.T) {
	v := testView(t)
	res := mustResource(t, &tree.FileTable{}, "r.json", largeRoute(0))
	for _, tc := range []struct {
		limit, keep int
	}{
		{math.MinInt, 2},
		{-1, 2},
		{0, 2},
		{1, 2},
		{2, 3},
		{MaxDiagnostics, MaxDiagnostics + 1},
		{math.MaxInt/2 - 1, math.MaxInt / 2},
		{math.MaxInt / 2, math.MaxInt/2 + 1},
		{math.MaxInt/2 + 1, math.MaxInt/2 + 2},
		{math.MaxInt - 2, math.MaxInt - 1},
		{math.MaxInt - 1, math.MaxInt},
		{math.MaxInt, math.MaxInt},
	} {
		m := newMapper(v, res, nil, tc.limit, Options{})
		if m.keep != tc.keep {
			t.Errorf("newMapper(limit %d).keep = %d, want %d", tc.limit, m.keep, tc.keep)
		}
		if tc.limit < math.MaxInt/2 {
			continue
		}
		for i := range 5 {
			m.emit(CodeSchema, m.positionAt(position{}, diag.Field("f"+strconv.Itoa(i))), tree.Pos{}, "x", "")
		}
		if len(m.out) != 5 || m.bound != nil {
			t.Errorf("limit %d: record kept %d entries with bound %v, want 5 and no prune", tc.limit, len(m.out), m.bound)
		}
		if got := m.result(); len(got) != 5 {
			t.Errorf("limit %d: result() = %d diagnostics, want 5", tc.limit, len(got))
		}
	}
}

// TestCollectorCapSignal feeds Validate's result to a diag.Collector of
// the same cap, as the pipeline does: the Collector appends the run's one
// RZ-CFG-001 "too many diagnostics" for a resource with more findings
// than the cap (10,001 and 15,000 at the default cap, 8 and 30 at a cap of
// 7), and none at or below it, so a resource with 15,000 findings never
// looks like one with 10,000.
func TestCollectorCapSignal(t *testing.T) {
	if testing.Short() {
		t.Skip("validates resources with 15,000 findings")
	}
	v := testView(t)
	for _, tc := range []struct {
		cap, findings int
		full          bool
	}{
		{0, MaxDiagnostics - 1, false},
		{0, MaxDiagnostics, false},
		{0, MaxDiagnostics + 1, true},
		{0, 15_000, true},
		{7, 7, false},
		{7, 8, true},
		{7, 30, true},
	} {
		limit := tc.cap
		if limit == 0 {
			limit = MaxDiagnostics
		}
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", largeRoute(tc.findings))
		got := v.ValidateWith(res, files, Options{MaxDiagnostics: tc.cap})
		c := diag.NewCollector(limit)
		c.AddAll(got)
		list := c.List()
		tooMany := slices.ContainsFunc(list, func(d diag.Diagnostic) bool { return d.Code == "RZ-CFG-001" })
		if c.Full() != tc.full || tooMany != tc.full || len(list) != min(tc.findings, limit)+btoi(tc.full) {
			t.Errorf("cap %d, %d findings: Validate() returned %d, collector full %v with %d diagnostics, RZ-CFG-001 %v; want full %v",
				limit, tc.findings, len(got), c.Full(), len(list), tooMany, tc.full)
		}
	}
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}
