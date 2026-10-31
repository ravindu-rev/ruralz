// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rec is a record of the fake records directory.
type rec struct {
	file, scenario, kind, commit, baseline, validity string
	budgets                                          []benchBudget
	of, verdict                                      string
	runs                                             []string
}

// write lays the record out as JSON below dir.
func (r rec) write(t *testing.T, dir string) {
	t.Helper()
	doc := map[string]any{
		"format": benchFormat,
		"identity": map[string]any{
			"scenario": r.scenario, "kind": r.kind, "commit": r.commit, "baseline": r.baseline, "version": "0.1.0",
		},
		"validity": map[string]any{"verdict": r.validity, "reasons": []string{}},
		"budgets":  r.budgets,
	}
	if r.kind == kindSummary {
		doc["summary"] = map[string]any{"of": r.of, "verdict": r.verdict, "runs": r.runs}
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, filepath.FromSlash(r.file))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pass(id string) benchBudget {
	return benchBudget{ID: id, Value: "v", Result: "r", Verdict: "pass"}
}

// releaseRecords is a passing release run's records at commitA: S1 and S2
// A/B comparisons of macro runs against baseline (none for the first
// release), a component comparison, an S2 ladder, a single PB-15 run with
// its invalid first attempt superseded by the re-run, and the S2 soak.
func releaseRecords(baseline string) []rec {
	commit := commitA
	verdict := "pass"
	if baseline == "" {
		verdict = "none"
	}
	return []rec{
		{file: "latency/S1/run-1-head.json", scenario: "S1", kind: kindMacro, commit: commit, baseline: baseline, validity: "valid", budgets: []benchBudget{pass("PB-1")}},
		{file: "latency/S1/run-1-baseline.json", scenario: "S1", kind: kindMacro, commit: commit, baseline: baseline, validity: "invalid"},
		{
			file: "latency/S1/summary-comparison.json", scenario: "S1", kind: kindSummary, commit: commit, baseline: baseline, validity: "valid",
			budgets: []benchBudget{pass("PB-1"), pass("PB-4")}, of: ofComparison, verdict: verdict, runs: []string{"run-1-head.json", "run-1-baseline.json"},
		},
		{file: "latency/S2/run-1-head.json", scenario: "S2", kind: kindMacro, commit: commit, baseline: baseline, validity: "valid"},
		{
			file: "latency/S2/summary-comparison.json", scenario: "S2", kind: kindSummary, commit: commit, baseline: baseline, validity: "valid",
			budgets: []benchBudget{pass("PB-2"), pass("PB-3")}, of: ofComparison, verdict: verdict, runs: []string{"run-1-head.json"},
		},
		{file: "latency/S2/ladder-step-1.json", scenario: "S2", kind: kindMacro, commit: commit, validity: "invalid"},
		{
			file: "latency/S2/summary-ladder.json", scenario: "S2", kind: kindSummary, commit: commit, validity: "valid",
			budgets: []benchBudget{pass("S2-saturation")}, of: ofLadder, verdict: "none", runs: []string{"ladder-step-1.json"},
		},
		{file: "latency/component-jwt/run-1-head.json", scenario: "component-jwt", kind: kindComponent, commit: commit, validity: "valid"},
		{
			file: "latency/component-jwt/summary-comparison.json", scenario: "component-jwt", kind: kindSummary, commit: commit, validity: "valid",
			of: ofComparison, verdict: "none", runs: []string{"run-1-head.json"},
		},
		{file: "latency/S2-pb15/run-1.json", scenario: "S2-pb15", kind: kindMacro, commit: commit, validity: "invalid"},
		{file: "latency/S2-pb15/run-1-rerun.json", scenario: "S2-pb15", kind: kindMacro, commit: commit, validity: "valid", budgets: []benchBudget{pass("PB-15")}},
		{file: "soak/S2-soak/soak.json", scenario: "S2-soak", kind: kindMacro, commit: commit, validity: "valid"},
		{
			file: "soak/S2-soak/summary-soak.json", scenario: "S2-soak", kind: kindSummary, commit: commit, validity: "valid",
			budgets: []benchBudget{pass("PB-2"), pass("PB-3"), pass("soak-rss-drift")}, of: ofSoak, verdict: "none", runs: []string{"soak.json"},
		},
	}
}

// writeRecords writes records into a new directory, with the Latency
// job's rotation state beside them.
func writeRecords(t *testing.T, records []rec) string {
	t.Helper()
	dir := t.TempDir()
	for _, r := range records {
		r.write(t, dir)
	}
	if err := os.WriteFile(filepath.Join(dir, "latency", stateFile), []byte(`{"format":"ruralz.bench.state.v1","nextSlot":2}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestReadRecords: every record below the directory, sorted, the rotation
// state skipped; a foreign JSON file or a summary without its summary is
// an error; a missing directory holds none.
func TestReadRecords(t *testing.T) {
	dir := writeRecords(t, releaseRecords(""))
	got, err := readRecords(dir)
	if err != nil || len(got) != len(releaseRecords("")) {
		t.Fatalf("readRecords = %d records, %v", len(got), err)
	}
	if got[0].file != "latency/S1/run-1-baseline.json" {
		t.Errorf("first record %s, want the sorted first path", got[0].file)
	}
	if recs, err := readRecords(filepath.Join(dir, "absent")); err != nil || recs != nil {
		t.Errorf("missing directory: %v, %v", recs, err)
	}
	for name, content := range map[string]string{
		"other.json":   `{"format":"other"}`,
		"broken.json":  `{`,
		"summary.json": `{"format":"ruralz.bench.v1","identity":{"kind":"summary"}}`,
		"kind.json":    `{"format":"ruralz.bench.v1","identity":{"kind":"ladder"}}`,
	} {
		bad := t.TempDir()
		if err := os.WriteFile(filepath.Join(bad, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRecords(bad); err == nil {
			t.Errorf("%s: readRecords accepted %s", name, content)
		}
	}
}

// TestBenchmarkFindings: the records part of the Benchmarks gate.
func TestBenchmarkFindings(t *testing.T) {
	prev := &baseline{Tag: "v0.1.0", Commit: commitB}
	read := func(t *testing.T, records []rec) []benchRecord {
		t.Helper()
		out, err := readRecords(writeRecords(t, records))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	if got := benchmarkFindings(read(t, releaseRecords("")), commitA, nil); len(got) != 0 {
		t.Errorf("first release on absolute budgets: findings %q", got)
	}
	if got := benchmarkFindings(read(t, releaseRecords(commitB)), commitA, prev); len(got) != 0 {
		t.Errorf("release compared with the previous release: findings %q", got)
	}
	edit := func(file string, change func(*rec)) func([]rec) []rec {
		return func(rs []rec) []rec {
			for i := range rs {
				if rs[i].file == file {
					change(&rs[i])
				}
			}
			return rs
		}
	}
	drop := func(file string) func([]rec) []rec {
		return func(rs []rec) []rec {
			var out []rec
			for _, r := range rs {
				if r.file != file {
					out = append(out, r)
				}
			}
			return out
		}
	}
	cases := []struct {
		name     string
		baseline string
		prev     *baseline
		change   func([]rec) []rec
		want     string
	}{
		{"budget missed", "", nil, edit("latency/S2/summary-comparison.json", func(r *rec) {
			r.budgets[0] = benchBudget{ID: "PB-2", Value: "<= 1ms", Result: "1.2ms", Verdict: "fail"}
		}), "budget PB-2 fail: 1.2ms, want <= 1ms"},
		{"budget not measured", "", nil, edit("soak/S2-soak/summary-soak.json", func(r *rec) {
			r.budgets[2].Verdict = "skipped"
		}), "budget soak-rss-drift (v) was not measured"},
		{
			"another commit", "", nil, edit("latency/S1/summary-comparison.json", func(r *rec) { r.commit = commitC }),
			"measured commit " + commitC + ", not the release commit " + commitA,
		},
		{
			"invalid summary", "", nil, edit("latency/S2/summary-ladder.json", func(r *rec) { r.validity = "inconclusive" }),
			"inconclusive summary",
		},
		{
			"inconclusive A/B", commitB, prev, edit("latency/S2/summary-comparison.json", func(r *rec) { r.verdict = "inconclusive" }),
			"A/B verdict inconclusive",
		},
		{"another baseline", commitC, prev, nil, "not with the previous release v0.1.0 (" + commitB + ")"},
		{"no comparison with the previous release", "", prev, nil, "no comparison with the previous release v0.1.0"},
		{"failed single run", "", nil, edit("latency/S2-pb15/run-1-rerun.json", func(r *rec) {
			r.budgets[0] = benchBudget{ID: "PB-15", Value: "<= 1ms", Result: "3ms", Verdict: "fail"}
		}), "S2-pb15 (latency/S2-pb15/run-1-rerun.json): budget PB-15 fail"},
		{
			"invalid single run without a re-run", "", nil, drop("latency/S2-pb15/run-1-rerun.json"),
			"S2-pb15 (latency/S2-pb15/run-1.json): invalid run",
		},
		{"no soak", "", nil, drop("soak/S2-soak/summary-soak.json"), "no S2-soak soak summary record from the 12. Soak job"},
		{"no S1 comparison", "", nil, drop("latency/S1/summary-comparison.json"), "no S1 comparison summary record from the 11. Latency job"},
		{
			"soak without PB-3", "", nil, edit("soak/S2-soak/summary-soak.json", func(r *rec) { r.budgets = r.budgets[:1] }),
			"the soak holds no PB-3 result",
		},
		{"unknown summary type", "", nil, edit("latency/S2/summary-ladder.json", func(r *rec) { r.of = "trend" }), `summary of "trend"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rs := releaseRecords(c.baseline)
			if c.change != nil {
				rs = c.change(rs)
			}
			got := benchmarkFindings(read(t, rs), commitA, c.prev)
			if !strings.Contains(strings.Join(got, "\n"), c.want) {
				t.Errorf("findings %q\nlack %q", got, c.want)
			}
		})
	}
}
