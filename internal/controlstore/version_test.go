// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"strings"
	"testing"
	"time"
)

// TestParseVersion reads minors and product versions.
func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want Version
		ok   bool
	}{
		{"0.2", Version{0, 2}, true},
		{"0.2.7", Version{0, 2}, true},
		{"1.10.0-rc.1", Version{1, 10}, true},
		{"0.0.0-dev", Version{0, 0}, true},
		{"2.3.4+build.5", Version{2, 3}, true},
		{"1", Version{}, false},
		{"1.2.3.4", Version{}, false},
		{"01.2", Version{}, false},
		{"a.b", Version{}, false},
		{"", Version{}, false},
		{"1..2", Version{}, false},
	}
	for _, tc := range cases {
		got, err := ParseVersion(tc.in)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("ParseVersion(%q) = %v, %v; want %v, ok %v", tc.in, got, err, tc.want, tc.ok)
		}
	}
}

// TestSucceeds allows only the next release minor.
func TestSucceeds(t *testing.T) {
	cases := []struct {
		v, w Version
		want bool
	}{
		{Version{0, 3}, Version{0, 2}, true},
		{Version{1, 0}, Version{0, 9}, true},
		{Version{0, 4}, Version{0, 2}, false},
		{Version{0, 2}, Version{0, 2}, false},
		{Version{1, 1}, Version{0, 9}, false},
		{Version{0, 1}, Version{0, 2}, false},
	}
	for _, tc := range cases {
		if got := tc.v.Succeeds(tc.w); got != tc.want {
			t.Errorf("%v.Succeeds(%v) = %v, want %v", tc.v, tc.w, got, tc.want)
		}
	}
}

// TestEvaluateFinalize covers each finalize condition and the timed soak.
func TestEvaluateFinalize(t *testing.T) {
	members := []ServerID{"r1", "r2", "r3"}
	n := "0.3.0"
	all := func(since time.Time) map[ServerID]ReplicaReport {
		out := map[ServerID]ReplicaReport{}
		for _, id := range members {
			out[id] = ReplicaReport{ServerID: id, BinaryVersion: n, Since: since}
		}
		return out
	}
	soaked := t0.Add(FinalizeSoak)
	t.Run("ready after the soak", func(t *testing.T) {
		st := EvaluateFinalize(v02, nil, members, all(t0), soaked, 0)
		if !st.Ready || st.Target != (Version{0, 3}) || !st.SoakStart.Equal(t0) || !st.SoakEnds.Equal(soaked) {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("during the soak", func(t *testing.T) {
		st := EvaluateFinalize(v02, nil, members, all(t0), soaked.Add(-time.Second), 0)
		if st.Ready || len(st.Blockers) != 1 || !strings.Contains(st.Blockers[0], "soak") {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("the last replica starts the soak", func(t *testing.T) {
		r := all(t0)
		r["r2"] = ReplicaReport{ServerID: "r2", BinaryVersion: n, Since: t0.Add(time.Hour)}
		st := EvaluateFinalize(v02, nil, members, r, soaked, 0)
		if st.Ready || !st.SoakStart.Equal(t0.Add(time.Hour)) {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("a replica on N-1", func(t *testing.T) {
		r := all(t0)
		r["r3"] = ReplicaReport{ServerID: "r3", BinaryVersion: "0.2.5", Since: t0}
		st := EvaluateFinalize(v02, nil, members, r, soaked, 0)
		if st.Ready || !st.SoakStart.IsZero() {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("a replica without a report", func(t *testing.T) {
		r := all(t0)
		delete(r, "r1")
		if st := EvaluateFinalize(v02, nil, members, r, soaked, 0); st.Ready {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("no newer binary", func(t *testing.T) {
		r := all(t0)
		for id, x := range r {
			x.BinaryVersion = "0.2.0"
			r[id] = x
		}
		st := EvaluateFinalize(v02, nil, members, r, soaked, 0)
		if st.Ready || !st.Target.IsZero() {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("no members", func(t *testing.T) {
		if st := EvaluateFinalize(v02, nil, nil, nil, soaked, 0); st.Ready {
			t.Errorf("status = %+v", st)
		}
	})
	last := &VersionAdvance{Index: 40, From: Version{0, 1}, To: v02, At: t0}
	t.Run("a snapshot before the last advance", func(t *testing.T) {
		r := all(t0)
		for id, x := range r {
			x.SnapshotIndex, x.SnapshotSince = 40, t0
			r[id] = x
		}
		r["r2"] = ReplicaReport{ServerID: "r2", BinaryVersion: n, Since: t0, SnapshotIndex: 39}
		if st := EvaluateFinalize(v02, last, members, r, soaked, 0); st.Ready {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("timed soak", func(t *testing.T) {
		r := all(t0)
		for id, x := range r {
			x.SnapshotIndex, x.SnapshotSince = 40, t0.Add(30*time.Hour)
			r[id] = x
		}
		st := EvaluateFinalize(v02, last, members, r, t0.Add(31*time.Hour), 2*time.Hour)
		if !st.HeldSince.Equal(t0.Add(30*time.Hour)) || !st.FinalizesAt.Equal(t0.Add(32*time.Hour)) || !st.Ready {
			t.Errorf("status = %+v", st)
		}
	})
}

// TestReportRefusal is the leader's rule 4 check of a status report.
func TestReportRefusal(t *testing.T) {
	members := []ServerID{"r1", "r2"}
	last := &VersionAdvance{Index: 10}
	past := map[ServerID]ReplicaReport{"r1": {SnapshotIndex: 10}, "r2": {SnapshotIndex: 12}}
	behind := map[ServerID]ReplicaReport{"r1": {SnapshotIndex: 10}, "r2": {SnapshotIndex: 9}}
	cases := []struct {
		name    string
		binary  string
		last    *VersionAdvance
		reports map[ServerID]ReplicaReport
		refused bool
	}{
		{"same version", "0.2.4", last, behind, false},
		{"older binary", "0.1.9", last, past, true},
		{"skipped minor", "0.4.0", last, past, true},
		{"next minor, snapshots past the advance", "0.3.0", last, past, false},
		{"next minor, a snapshot behind the advance", "0.3.0", last, behind, true},
		{"next minor, no advance yet", "0.3.0", nil, behind, false},
		{"malformed", "x", last, past, true},
	}
	for _, tc := range cases {
		got := ReportRefusal(v02, tc.binary, tc.last, members, tc.reports)
		if (got != "") != tc.refused {
			t.Errorf("%s: ReportRefusal = %q, refused want %v", tc.name, got, tc.refused)
		}
	}
}
