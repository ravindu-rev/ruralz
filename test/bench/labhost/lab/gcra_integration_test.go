// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package lab

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
)

// TestGCRALoad runs G1's load generator against a live server: open-loop
// GCRA calls through the State client of each emulated Node, the keys
// cycled, every call decided, and each emulated Node holding its own
// pipelined connections to the shard.
func TestGCRALoad(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{})
	const nodes = 3
	res, err := RunLoad(t.Context(), LoadSpec{
		Rate: 200, Duration: time.Second,
		Keys: &KeySpec{Kind: KeysCycle, Prefix: "key-", N: 50},
		GCRA: &GCRASpec{URL: dep.URL(), Nodes: nodes, Requests: 20000, Window: time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Offered == 0 || res.Achieved != res.Offered || len(res.Failures) != 0 {
		t.Fatalf("GCRA load = %d offered, %d achieved, failures %v", res.Offered, res.Achieved, res.Failures)
	}
	if res.KeyCardinality != 50 || res.KeyDistribution != "uniform" {
		t.Errorf("keys = %d %s, want 50 uniform", res.KeyCardinality, res.KeyDistribution)
	}
	cmds, err := dep.Primary().Info(t.Context(), "commandstats")
	if err != nil {
		t.Fatal(err)
	}
	if calls := evalshaCalls(cmds["cmdstat_evalsha"]); calls < res.Offered {
		t.Errorf("the server ran %d EVALSHA, want at least the %d calls offered", calls, res.Offered)
	}
	// Each emulated Node's State client dials its own 2 pipelined
	// connections.
	stats, err := dep.Primary().Info(t.Context(), "stats")
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := strconv.Atoi(stats["total_connections_received"]); n < 2*nodes {
		t.Errorf("the server received %d connections, want at least %d from %d emulated Nodes", n, 2*nodes, nodes)
	}
}

// TestGCRALoadRefuses covers the GCRA loads RunLoad rejects.
func TestGCRALoadRefuses(t *testing.T) {
	keys := &KeySpec{Kind: KeysCycle, Prefix: "key-", N: 5}
	for _, c := range []struct {
		name string
		spec LoadSpec
	}{
		{"no keys", LoadSpec{Rate: 10, Duration: time.Second, GCRA: &GCRASpec{URL: "redis://127.0.0.1:1", Requests: 1, Window: time.Second}}},
		{"no URL", LoadSpec{Rate: 10, Duration: time.Second, Keys: keys, GCRA: &GCRASpec{Requests: 1, Window: time.Second}}},
		{"no limit", LoadSpec{Rate: 10, Duration: time.Second, Keys: keys, GCRA: &GCRASpec{URL: "redis://127.0.0.1:1", Window: time.Second}}},
		{"no window", LoadSpec{Rate: 10, Duration: time.Second, Keys: keys, GCRA: &GCRASpec{URL: "redis://127.0.0.1:1", Requests: 1}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := RunLoad(t.Context(), c.spec); err == nil {
				t.Errorf("RunLoad(%+v) succeeded", c.spec)
			}
		})
	}
}

// evalshaCalls reads the calls of an INFO commandstats value,
// "calls=N,usec=...".
func evalshaCalls(v string) int64 {
	for field := range strings.SplitSeq(v, ",") {
		if n, ok := strings.CutPrefix(field, "calls="); ok {
			calls, _ := strconv.ParseInt(n, 10, 64)
			return calls
		}
	}
	return 0
}
