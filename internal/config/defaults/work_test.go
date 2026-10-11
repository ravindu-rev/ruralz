// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// hostileQuota returns a quota Policy named name whose open config member
// holds n numbers outside the double range: n RZ-CFG-005 diagnostics in
// one resource that passes stage F.
func hostileQuota(name string, n int) string {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "` + name +
		`"}, "spec": {"type": "quota", "config": {"consumerQuota": "c", "x": [`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("1e999")
	}
	b.WriteString(`]}}}`)
	return b.String()
}

// TestRunDiagnosticBudget covers 01 req 50 for the whole run: many
// hostile resources, each under the per-resource cap, never make Run
// return more than MaxDiagnostics diagnostics plus one RZ-CFG-001, for any
// worker count, and the workers stop taking resources once the budget is
// spent, so the run records no more than the cap.
func TestRunDiagnosticBudget(t *testing.T) {
	const resources, each = 40, MaxDiagnostics / 8
	for _, workers := range []int{1, 3, 8} {
		f := newFixture()
		var rs []*tree.Resource
		for i := range resources {
			rs = append(rs, f.resource(t, "q"+strconv.Itoa(i)+".json", hostileQuota("q"+strconv.Itoa(i), each)))
		}
		b, ds, err := newStage(t).Run(t.Context(), rs, f.files, workers)
		if err != nil || b != nil {
			t.Fatalf("workers %d: Run = %v, %v", workers, b, err)
		}
		if len(ds) != MaxDiagnostics+1 {
			t.Errorf("workers %d: %d diagnostics, want %d", workers, len(ds), MaxDiagnostics+1)
		}
		limits := 0
		for _, d := range ds {
			switch d.Code {
			case CodeLimit:
				limits++
				if d.Message != "too many diagnostics; stopped after 10000" || d.Severity != diag.SeverityError {
					t.Errorf("workers %d: limit diagnostic %+v", workers, d)
				}
			case CodeSchema:
			default:
				t.Errorf("workers %d: unexpected %s", workers, d.AppendText(nil))
			}
		}
		if limits != 1 || ds.FirstErrorCode() != CodeLimit {
			t.Errorf("workers %d: %d RZ-CFG-001 entries, first code %s", workers, limits, ds.FirstErrorCode())
		}
		// Later resources were never started: no worker took one after
		// the budget was spent, and at most one per worker was running.
		untouched := 0
		for _, r := range rs {
			if q, _ := lookup(r.Root, "spec.config.consumerQuota"); q != nil {
				if _, materialized := lookup(r.Root, "spec.slot"); !materialized {
					untouched++
				}
			}
		}
		started := resources - untouched
		t.Logf("workers %d: %d of %d resources started", workers, started, resources)
		if started > MaxDiagnostics/each+workers {
			t.Errorf("workers %d: %d resources started, want at most %d", workers, started, MaxDiagnostics/each+workers)
		}
	}
}

// TestRunBudgetExact covers the edge of 01 req 50: a run with exactly
// MaxDiagnostics diagnostics is complete and has no RZ-CFG-001.
func TestRunBudgetExact(t *testing.T) {
	f := newFixture()
	rs := []*tree.Resource{
		f.resource(t, "a.json", hostileQuota("a", MaxDiagnostics/2)),
		f.resource(t, "b.json", hostileQuota("b", MaxDiagnostics/2)),
	}
	_, ds, err := newStage(t).Run(t.Context(), rs, f.files, 2)
	if err != nil || len(ds) != MaxDiagnostics || ds.FirstErrorCode() != CodeSchema {
		t.Errorf("Run = %d diagnostics, first %s, %v; want %d RZ-CFG-005", len(ds), ds.FirstErrorCode(), err, MaxDiagnostics)
	}
}

// TestResourceDiagnosticBudget covers the budget of one Resource call: at
// most MaxDiagnostics diagnostics in tree order, then RZ-CFG-001, and no
// hub.Resource.
func TestResourceDiagnosticBudget(t *testing.T) {
	f := newFixture()
	h, ds := newStage(t).Resource(f.resource(t, "q.json", hostileQuota("q", MaxDiagnostics+5)), f.files)
	if h != nil || len(ds) != MaxDiagnostics+1 || ds[len(ds)-1].Code != CodeLimit || ds[0].Path.String() != "spec.config.x[0]" {
		t.Errorf("Resource = %v, %d diagnostics, last %v", h, len(ds), ds[len(ds)-1])
	}
}

// TestSpentBudgetStillRejects checks that a resource whose errors the run
// budget dropped is still rejected, and a clean one still accepted.
func TestSpentBudgetStillRejects(t *testing.T) {
	s := newStage(t)
	f := newFixture()
	budget := new(atomic.Int64)
	w := newWork(budget)
	for _, src := range []string{
		hostileQuota("q", 3),
		route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`),
		`["x"]`,
		`{"apiVersion": "ruralz/v9", "kind": "Route", "metadata": {"name": "r"}, "spec": {}}`,
	} {
		if h, ds := s.resource(f.resource(t, "r.json", src), f.files, w); h != nil || len(ds) != 0 {
			t.Errorf("spent budget: Resource(%.40s) = %v, %q; want nil and nothing recorded", src, h, texts(ds))
		}
	}
	if !w.spent() {
		t.Error("budget not reported spent")
	}
	if h, ds := s.resource(f.resource(t, "ok.json", route(`{"match": {}}`)), f.files, w); h == nil || len(ds) != 0 {
		t.Errorf("clean resource on a spent budget = %v, %q", h, texts(ds))
	}
	if obj, cfg, ds, failed := decode(hubIndex(t), nil, &tree.Resource{ID: tree.ID{Kind: "Nope"}, Root: mustParse(t, `{}`)}, nil, w); obj != nil || cfg != nil || len(ds) != 0 || !failed {
		t.Errorf("decode of an unknown kind on a spent budget = %v %v %q %v", obj, cfg, texts(ds), failed)
	}
}

// TestWorkAdmitAndLimit covers the budget helpers.
func TestWorkAdmitAndLimit(t *testing.T) {
	budget := new(atomic.Int64)
	budget.Store(2)
	w := newWork(budget)
	ds := diag.List{{Code: CodeSchema}, {Code: CodeSchema}, {Code: CodeUnserved}}
	if got := w.admit(ds); len(got) != 2 || w.spent() == false {
		t.Errorf("admit = %d, spent %v; want 2, true", len(got), w.spent())
	}
	if got := limit(nil, false); got != nil {
		t.Errorf("limit(nil, false) = %v", got)
	}
	if got := limit(nil, true); len(got) != 1 || got[0].Code != CodeLimit {
		t.Errorf("limit(nil, true) = %v", got)
	}
	if w := newWork(nil); w.budget.Load() != MaxDiagnostics {
		t.Errorf("own budget = %d", w.budget.Load())
	}
}

// TestWorkYield covers the yield timer of 01 req 53 without depending on
// the wall clock: the timer is read every yieldCheck units, a worker
// whose last yield lies far in the past yields and restarts its timer,
// and one whose last yield lies in the future does not.
func TestWorkYield(t *testing.T) {
	w := &work{budget: newBudget()} // last yield at the zero time: due
	for range yieldCheck - 1 {
		w.tick()
	}
	if !w.last.IsZero() {
		t.Fatal("timer read before yieldCheck units")
	}
	w.tick()
	if w.last.IsZero() || w.units != 0 {
		t.Fatalf("no yield after %d units of overdue work: last %v units %d", yieldCheck, w.last, w.units)
	}
	future := time.Now().Add(time.Hour)
	w.last = future
	w.check()
	if !w.last.Equal(future) {
		t.Error("yielded before yieldAfter had passed")
	}
	// A nil worker, as the test helpers pass, does nothing.
	var none *work
	none.tick()
	none.check()
}
