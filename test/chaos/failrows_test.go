// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"strings"
	"testing"
	"time"
)

// TestHalfOpenProbes checks FC-10's allowance once every breaker opened:
// one probe per Node and started BreakerHalfOpenMin, the first at once.
func TestHalfOpenProbes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nodes int
		d     time.Duration
		want  float64
	}{
		{"none", 3, 0, 0},
		{"no Node", 0, time.Minute, 0},
		{"one probe each within the first delay", 3, 500 * time.Millisecond, 3},
		{"a whole fault phase", 10, 60 * time.Second, 600},
		{"a partial last delay counts", 2, 2500 * time.Millisecond, 6},
	} {
		if got := HalfOpenProbes(tc.nodes, tc.d); got != tc.want {
			t.Errorf("%s: HalfOpenProbes(%d, %v) = %v, want %v", tc.name, tc.nodes, tc.d, got, tc.want)
		}
	}
}

// TestScriptStats checks how FC-12 reads a server's script loads: the
// script|load subcommand a Redis 7 or Valkey server counts apart, else the
// script command, and the NOSCRIPT error replies of INFO errorstats.
func TestScriptStats(t *testing.T) {
	sub := map[string]CommandStat{"script|load": {Calls: 7}, "script|exists": {Calls: 9}, "evalsha": {Calls: 1000}}
	if got := ScriptLoadCalls(sub); got != 7 {
		t.Errorf("ScriptLoadCalls(subcommands) = %d, want 7", got)
	}
	if got := ScriptLoadCalls(map[string]CommandStat{"script": {Calls: 4}}); got != 4 {
		t.Errorf("ScriptLoadCalls(script) = %d, want 4", got)
	}
	if got := ScriptLoadCalls(nil); got != 0 {
		t.Errorf("ScriptLoadCalls(nil) = %d", got)
	}
	info := map[string]string{"errorstat_NOSCRIPT": "count=3", "errorstat_ERR": "count=12"}
	if got := ErrorReplies(info, "NOSCRIPT"); got != 3 {
		t.Errorf("ErrorReplies(NOSCRIPT) = %d, want 3", got)
	}
	if got := ErrorReplies(info, "OOM"); got != 0 {
		t.Errorf("ErrorReplies(OOM) = %d, want 0", got)
	}
	if got := ErrorReplies(map[string]string{"errorstat_NOSCRIPT": "count=x"}, "NOSCRIPT"); got != 0 {
		t.Errorf("ErrorReplies of a malformed count = %d, want 0", got)
	}
}

// TestFC12Criteria checks FC-12's Detection: every Node loads its scripts
// on the promoted primary, counted from before the kill.
func TestFC12Criteria(t *testing.T) {
	before := ScriptStats{Loads: 2}
	for _, tc := range []struct {
		after ScriptStats
		pass  bool
	}{
		{ScriptStats{Loads: 12}, true},
		{ScriptStats{Loads: 11, NoScript: 4}, false},
	} {
		cs := FC12Criteria(10, before, tc.after)
		if len(cs) != 1 || !strings.HasPrefix(cs[0].Name, FC12+": ") || (cs[0].Verdict == VerdictPass) != tc.pass {
			t.Errorf("FC12Criteria(10, %+v, %+v) = %+v, want pass %v", before, tc.after, cs, tc.pass)
		}
	}
}

// TestFC13Criteria checks FC-13's criteria per CE-6 run: no eviction
// reason under noeviction, keys evicted on an evicting main connection,
// and nothing more for the runs whose criteria CE-6 states alone.
func TestFC13Criteria(t *testing.T) {
	for _, tc := range []struct {
		name     string
		policy   string
		onCache  bool
		evicted  int64
		raising  int
		criteria int
		pass     bool
	}{
		{"main noeviction", "noeviction", false, 0, 0, 1, true},
		{"cache noeviction raising", "noeviction", true, 0, 2, 1, false},
		{"main allkeys-lru", "allkeys-lru", false, 120, 3, 1, true},
		{"main allkeys-lru without evictions", "allkeys-lru", false, 0, 3, 1, false},
		{"cache allkeys-lru", "allkeys-lru", true, 120, 3, 0, true},
		{"cache volatile-ttl", "volatile-ttl", true, 120, 0, 0, true},
	} {
		cs := FC13Criteria(tc.policy, tc.onCache, tc.evicted, tc.raising)
		if len(cs) != tc.criteria {
			t.Errorf("%s: FC13Criteria = %+v, want %d criteria", tc.name, cs, tc.criteria)
			continue
		}
		for _, c := range cs {
			if !strings.HasPrefix(c.Name, FC13+": ") || (c.Verdict == VerdictPass) != tc.pass {
				t.Errorf("%s: %+v, want pass %v", tc.name, c, tc.pass)
			}
		}
	}
}
