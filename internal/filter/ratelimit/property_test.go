// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The Rate Limit row of docs/engineering/03-testing-and-quality-strategy.md
// "Required properties", at unit level: the ratelimit Filter on N
// simulated Nodes over one shared State Store, with a deterministic clock.

// Property modes.
const (
	// propHealthy: a healthy State Store; per key at most requests +
	// burst units in any window.
	propHealthy = iota
	// propFailing: every call fails under failureMode open; per key at
	// most N × ceiling per epoch-aligned window.
	propFailing
	// propLocalOnly: a localOnly Policy; per Node at most the ceiling plus
	// its refill over any span.
	propLocalOnly
	numPropModes
)

// admission is one admitted request: its Node, time and cost.
type admission struct {
	node int
	at   time.Duration
	cost int64
}

// FuzzAdmissionBound drives N Nodes, each with its own key table and
// Filter for one Policy, over one shared memory State Store, with requests
// of one key at fuzzed times and costs, and checks the bound of the mode.
func FuzzAdmissionBound(f *testing.F) {
	f.Add(uint8(10), uint8(10), uint16(1000), uint8(3), uint8(propHealthy), uint8(0), []byte{0, 4, 8, 1, 5, 9, 2, 6, 10, 3, 7, 11, 255, 0, 4, 8})
	f.Add(uint8(5), uint8(0), uint16(500), uint8(2), uint8(propHealthy), uint8(3), []byte{0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 252, 0, 1, 2, 3})
	f.Add(uint8(8), uint8(2), uint16(1000), uint8(4), uint8(propFailing), uint8(0), []byte{0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3, 200, 0, 1, 2, 3, 0, 1})
	f.Add(uint8(9), uint8(9), uint16(300), uint8(2), uint8(propFailing), uint8(5), []byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 64, 0, 1})
	f.Add(uint8(4), uint8(4), uint16(1000), uint8(2), uint8(propLocalOnly), uint8(0), []byte{0, 0, 0, 0, 0, 0, 40, 40, 40, 40, 40, 40, 0, 0})
	f.Add(uint8(12), uint8(3), uint16(1500), uint8(3), uint8(propLocalOnly), uint8(7), []byte{1, 5, 9, 13, 17, 21, 25, 29, 33})
	f.Fuzz(func(t *testing.T, reqRaw, burstRaw uint8, windowRaw uint16, nodesRaw, modeRaw, ceilRaw uint8, events []byte) {
		requests := int64(reqRaw%20) + 1
		burst := int64(burstRaw) % (requests + 1)
		window := time.Duration(windowRaw%2000+100) * time.Millisecond
		nodes := int(nodesRaw%4) + 1
		mode := int(modeRaw % numPropModes)
		var perNode int64
		if ceilRaw%2 == 1 {
			perNode = int64(ceilRaw)%requests + 1
		}
		if len(events) > 300 {
			events = events[:300]
		}
		admitted := simulate(t, requests, burst, window, nodes, mode, perNode, events)
		ceiling := requests
		if perNode > 0 {
			ceiling = perNode
		}
		switch mode {
		case propHealthy:
			checkSliding(t, admitted, window, requests+burst)
		case propFailing:
			checkAligned(t, admitted, window, int64(nodes)*ceiling)
		default:
			checkBucket(t, admitted, window, ceiling, nodes)
		}
	})
}

// simulate runs the events and returns the admissions in time order. An
// event byte selects a Node (its low two bits) and, in its high bits, how
// far time moves first: 0 to 63 steps of window / 16.
func simulate(t *testing.T, requests, burst int64, window time.Duration, nodes, mode int, perNode int64, events []byte) []admission {
	t.Helper()
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	clk := clocktest.New(start)
	mem, err := memory.New(context.Background(), statestore.Config{Driver: statestore.DriverMemory},
		statestore.Deps{Clock: clk, Logger: slog.New(slog.DiscardHandler)}, memory.Options{Shards: 4, MaxEntries: 4096})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = mem.Close(context.Background()) }()
	store := &countingStore{Store: mem}
	if mode == propFailing {
		store.failWith(statestore.ErrTimeout)
	}
	l := v1alpha1.RateLimit{Requests: requests, Window: v1alpha1.Duration(window), Burst: ptr(burst)}
	if perNode > 0 {
		l.PerNodeCeiling = ptr(perNode)
	}
	cfg := &v1alpha1.RateLimitConfig{
		Key: `request.headers["x-key"]`, Cost: ptr(`int(request.headers["x-cost"])`),
		Limits: []v1alpha1.RateLimit{l}, LocalOnly: ptr(mode == propLocalOnly),
	}
	compiler := celtest.Compiler(t)
	fs := make([]*Filter, nodes)
	fxs := make([]*fixture, nodes)
	for i := range fs {
		tbl, err := NewTable(clk, nil, TableOptions{Entries: 40, LocalOnlyEntries: 20, Shards: 1, Seed: uint64(i) + 1})
		if err != nil {
			t.Fatal(err)
		}
		fxs[i] = &fixture{clk: clk, table: tbl, store: store, cel: compiler}
		fs[i], _ = fxs[i].build(t, "rl", cfg)
	}
	var out []admission
	for _, b := range events {
		clk.Advance(time.Duration(b>>2) * window / 16)
		node := int(b&3) % nodes
		cost := int64(b%3) + 1
		v := vars("client", "")
		v.Request.Header.Set("X-Cost", string(rune('0'+cost)))
		o := fxs[node].request(t, fs[node], v)
		switch {
		case o.r.Outcome == filter.Continue:
		case o.r.Outcome == filter.CannotDecide && mode == propFailing && o.code() == "RZ-STS-001":
		case o.r.Outcome == filter.Respond:
			continue
		default:
			t.Fatalf("unexpected result %+v", o.r)
		}
		out = append(out, admission{node: node, at: clk.Now().Sub(start), cost: cost})
	}
	return out
}

// checkSliding checks that every span of one window, [t, t + window),
// admits at most limit units.
func checkSliding(t *testing.T, as []admission, window time.Duration, limit int64) {
	t.Helper()
	for i := range as {
		var units int64
		for j := i; j < len(as) && as[j].at < as[i].at+window; j++ {
			units += as[j].cost
		}
		if units > limit {
			t.Fatalf("%d units admitted in the window from %v, want at most requests + burst = %d", units, as[i].at, limit)
		}
	}
}

// checkAligned checks that every epoch-aligned window admits at most
// limit units across the Nodes.
func checkAligned(t *testing.T, as []admission, window time.Duration, limit int64) {
	t.Helper()
	sums := map[int64]int64{}
	start := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	for _, a := range as {
		w := alignDown(start.Add(a.at).UnixNano(), int64(window))
		sums[w] += a.cost
		if sums[w] > limit {
			t.Fatalf("%d units admitted in the aligned window at %v while failing open, want at most N × ceiling = %d",
				sums[w], time.Unix(0, w).UTC(), limit)
		}
	}
}

// checkBucket checks each Node's local bucket: any span of length L from
// one admission to a later one admits at most ceiling + L × ceiling /
// window units, a full bucket plus its refill.
func checkBucket(t *testing.T, as []admission, window time.Duration, ceiling int64, nodes int) {
	t.Helper()
	for n := range nodes {
		var mine []admission
		for _, a := range as {
			if a.node == n {
				mine = append(mine, a)
			}
		}
		for i := range mine {
			var units int64
			for j := i; j < len(mine); j++ {
				units += mine[j].cost
				span := mine[j].at - mine[i].at
				if limit := ceiling + int64(span)*ceiling/int64(window); units > limit {
					t.Fatalf("Node %d admitted %d units over %v, want at most %d from a bucket of %d per %v",
						n, units, span, limit, ceiling, window)
				}
			}
		}
	}
}
