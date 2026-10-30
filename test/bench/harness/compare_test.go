// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

func usageOf(cpu time.Duration) proc.Usage { return proc.Usage{UserCPU: cpu} }

// outcome is a run outcome whose external and internal views hold one
// sample each at ext and in.
func outcome(t *testing.T, name string, ext, in time.Duration, valid bool) *RunOutcome {
	t.Helper()
	rec := validRecord()
	return &RunOutcome{
		Record: rec, Path: name + ".json", Valid: valid,
		Measures: Measures{External: hist(t, ext), Internal: hist(t, in), CPUPerRequest: 80 * time.Microsecond, GCShare: 0.04, RSSLoad: 100 << 20},
	}
}

func pairsOf(t *testing.T, head, base []time.Duration) []Pair {
	t.Helper()
	var ps []Pair
	for i := range head {
		n := strconv.Itoa(i + 1)
		ps = append(ps, Pair{Head: outcome(t, "run-"+n+"-head", head[i], head[i]/2, true), Baseline: outcome(t, "run-"+n+"-baseline", base[i], base[i]/2, true)})
	}
	return ps
}

// TestSummarize covers the comparison's summary: verdicts, pooled
// budgets, slower pairs, noise and the record it fills.
func TestSummarize(t *testing.T) {
	ms := func(xs ...float64) []time.Duration {
		var out []time.Duration
		for _, x := range xs {
			out = append(out, time.Duration(x*float64(time.Millisecond)))
		}
		return out
	}
	sc, _ := Find(Catalog(), "S2")
	base := ms(1, 1, 1, 1, 1)
	tests := []struct {
		name    string
		pairs   []Pair
		reasons []string
		verdict string
		valid   string
	}{
		{"pass", pairsOf(t, ms(1, 1, 1, 1, 1), base), nil, benchrecord.ABPass, benchrecord.VerdictValid},
		{"regression", pairsOf(t, ms(1.2, 1.2, 1.2, 1.2, 1.2), base), nil, benchrecord.ABFail, benchrecord.VerdictValid},
		{"noisy baseline", pairsOf(t, ms(1, 1, 1, 1, 1), ms(0.8, 1.2, 1, 0.9, 1.1)), nil, benchrecord.ABInconclusive, benchrecord.VerdictInconclusive},
		{"refused baseline", pairsOf(t, ms(1, 1, 1, 1, 1), base), []string{"the baseline refuses the head's Revision"}, benchrecord.ABInconclusive, benchrecord.VerdictInconclusive},
	}
	for _, tt := range tests {
		sum, err := Summarize(sc, tt.pairs, true, tt.reasons)
		if err != nil {
			t.Fatalf("%s: %v", tt.name, err)
		}
		if sum.Verdict != tt.verdict {
			t.Errorf("%s: verdict %q, want %q", tt.name, sum.Verdict, tt.verdict)
		}
		rec := validRecord()
		rec.Identity.Kind = benchrecord.KindSummary
		rec.Identity.Scenario = "S2"
		sum.fill(rec)
		if err := rec.Validate(); err != nil {
			t.Errorf("%s: the summary record is invalid: %v", tt.name, err)
		}
		if rec.Validity.Verdict != tt.valid || len(rec.Summary.Runs) != 10 || rec.Summary.HeadRuns != 5 {
			t.Errorf("%s: validity %q runs %d head %d", tt.name, rec.Validity.Verdict, len(rec.Summary.Runs), rec.Summary.HeadRuns)
		}
		if len(rec.Budgets) != len(sc.Budgets) {
			t.Errorf("%s: %d budgets, want %d", tt.name, len(rec.Budgets), len(sc.Budgets))
		}
	}
	sum, err := Summarize(sc, pairsOf(t, ms(1.2, 1.2, 1.2, 1.2, 1.2), base), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.External.Slower != 5 || sum.Internal.Slower != 5 || sum.External.BaseCV != 0 {
		t.Errorf("regression summary = %+v", sum.External)
	}
	// PB-2 reads the pooled internal view: 0.6ms ≤ 1ms passes; PB-3's
	// 150µs p50 fails.
	for _, b := range sum.Budgets {
		switch b.ID {
		case "PB-2":
			if b.Verdict != benchrecord.BudgetPass {
				t.Errorf("PB-2 = %+v", b)
			}
		case "PB-3":
			if b.Verdict != benchrecord.BudgetFail {
				t.Errorf("PB-3 = %+v", b)
			}
		}
	}
	// An invalid head run makes the comparison inconclusive.
	ps := pairsOf(t, ms(1, 1, 1, 1, 1), base)
	ps[2].Head.Valid = false
	sum, err = Summarize(sc, ps, true, nil)
	if err != nil || sum.Verdict != benchrecord.ABInconclusive || !strings.Contains(strings.Join(sum.Reasons, ";"), "4 of 5 head runs valid") {
		t.Errorf("invalid head run = %+v, %v", sum, err)
	}
	// Without a baseline the verdict is none and budgets still judge.
	sum, err = Summarize(sc, []Pair{{Head: outcome(t, "run-1-head", time.Millisecond, 500*time.Microsecond, true)}}, false, nil)
	if err != nil || sum.Verdict != benchrecord.ABNone || len(sum.Budgets) == 0 {
		t.Errorf("no baseline = %+v, %v", sum, err)
	}
	if _, err := Summarize(sc, nil, true, nil); err == nil {
		t.Error("Summarize(no runs) = nil error")
	}
}
