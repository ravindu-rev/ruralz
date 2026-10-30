// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package property

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/manager"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Modes of FuzzRateLimit.
const (
	// rlHealthy: a healthy State Store; per key at most requests + burst
	// units in any window.
	rlHealthy = iota
	// rlFailing: every State Store call fails under failureMode open; per
	// key at most N × ceiling units per epoch-aligned window.
	rlFailing
	// rlLocalOnly: a localOnly Policy; per Node and key at most the
	// ceiling plus its refill over any span.
	rlLocalOnly
	// rlFirstSeen: a healthy State Store and a flood of first-seen keys on
	// each Node past its first-seen budget; keys past the budget hold a
	// local-only entry and keep the localOnly bound, keys within it the
	// healthy one.
	rlFirstSeen
	numRLModes
)

// File-mode first-seen budget and local-only entry life (docs/architecture/
// 09-traffic-management-and-resilience.md "Decision path": 200 keys per
// second per Node without a Node count, 10 s, both targets).
const (
	firstSeenBudget = 200
	localOnlyLife   = 10 * time.Second
)

// rlAdmission is one admitted request.
type rlAdmission struct {
	node int
	key  string
	at   time.Duration
	cost int64
}

// FuzzRateLimit is the Rate Limit row of the Required properties: with a
// deterministic clock and a healthy State Store, admission per key stays
// within requests + burst per window, except for localOnly Policies and
// first-seen keys on local-only entries, which stay within N × per-Node
// ceiling per window (target); while failing open, within N × per-Node
// ceiling per window (target), the ceiling being perNodeCeiling or, in
// file mode, the full limit (foundation pack section 8.8, ADR-0008,
// docs/architecture/09-traffic-management-and-resilience.md "Decision
// path"). A Bundle holding one ratelimit Policy with fuzzed requests,
// window, burst, perNodeCeiling and localOnly, its key and cost read from
// request headers, runs on N simulated file-mode Nodes: each compiles it
// with its own snapshot compiler and key table, and every Node's snapshot
// holds the one State Store the Nodes share, opened by one State Store
// manager over the memory driver, which fails every call while the mode
// says so. Requests run through the Filter Chain executor in
// onRequestHeaders on a shared fake clock, as the request handler runs
// them, and the admissions keep the mode's bound. A localOnly bound is
// each Node's bucket law: at most the ceiling plus its refill over any
// span from one admission to a later one, so N Nodes admit N × ceiling per
// window beyond their full buckets.
func FuzzRateLimit(f *testing.F) {
	f.Add(uint8(10), uint8(10), uint16(1000), uint8(3), uint8(rlHealthy), uint8(0), []byte{0, 4, 8, 1, 5, 9, 2, 6, 10, 3, 7, 11, 255, 0, 4, 8})
	f.Add(uint8(5), uint8(0), uint16(500), uint8(2), uint8(rlHealthy), uint8(3), []byte{0, 0, 0, 0, 0, 0, 1, 1, 1, 1, 252, 0, 1, 2, 3})
	f.Add(uint8(8), uint8(2), uint16(1000), uint8(4), uint8(rlFailing), uint8(0), []byte{0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3, 200, 0, 1, 2, 3, 0, 1})
	f.Add(uint8(9), uint8(9), uint16(300), uint8(2), uint8(rlFailing), uint8(5), []byte{0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 1, 64, 0, 1})
	f.Add(uint8(4), uint8(4), uint16(1000), uint8(2), uint8(rlLocalOnly), uint8(0), []byte{0, 0, 0, 0, 0, 0, 40, 40, 40, 40, 40, 40, 0, 0})
	f.Add(uint8(12), uint8(3), uint16(1500), uint8(3), uint8(rlLocalOnly), uint8(7), []byte{1, 5, 9, 13, 17, 21, 25, 29, 33})
	f.Add(uint8(3), uint8(1), uint16(1000), uint8(2), uint8(rlFirstSeen), uint8(0), []byte{0, 16, 32, 1, 17, 33, 2, 18, 34, 3, 19, 35, 8, 24, 40})
	f.Add(uint8(6), uint8(6), uint16(800), uint8(1), uint8(rlFirstSeen), uint8(3), []byte{4, 4, 4, 4, 20, 20, 20, 36, 36, 36, 52, 52, 52})
	// Three Nodes at one instant hold 5 tokens each, more than the 10
	// units GCRA admits: the healthy bound is reached exactly.
	saturate := []byte{0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3, 0, 1, 2, 3}
	f.Add(uint8(4), uint8(5), uint16(900), uint8(2), uint8(rlHealthy), uint8(0), saturate)
	// Failing open, the Nodes admit their full buckets in each aligned
	// window: N × ceiling exactly.
	f.Add(uint8(4), uint8(0), uint16(900), uint8(2), uint8(rlFailing), uint8(0), append(append(saturate, 240), saturate...))
	f.Fuzz(func(t *testing.T, reqRaw, burstRaw uint8, windowRaw uint16, nodesRaw, modeRaw, ceilRaw uint8, events []byte) {
		p := rlParams{
			requests: int64(reqRaw%20) + 1,
			window:   time.Duration(windowRaw%2000+100) * time.Millisecond,
			nodes:    int(nodesRaw%4) + 1,
			mode:     int(modeRaw % numRLModes),
		}
		p.burst = int64(burstRaw) % (p.requests + 1)
		if ceilRaw%2 == 1 {
			p.perNode = int64(ceilRaw)%p.requests + 1
		}
		if len(events) > 300 {
			events = events[:300]
		}
		c := newRLCluster(t, p)
		admitted := c.drive(t, events)
		ceiling := p.requests
		if p.perNode > 0 {
			ceiling = p.perNode
		}
		byKey := map[string][]rlAdmission{}
		for _, a := range admitted {
			byKey[a.key] = append(byKey[a.key], a)
		}
		for key, as := range byKey {
			switch {
			case p.mode == rlFailing:
				checkAligned(t, key, as, p.window, int64(p.nodes)*ceiling)
			case p.mode == rlLocalOnly || p.mode == rlFirstSeen && pastBudget(key):
				checkBucket(t, key, as, p.window, ceiling, p.nodes)
			default:
				checkSliding(t, key, as, p.window, p.requests+p.burst)
			}
		}
	})
}

// rlParams are one input's limit and cluster.
type rlParams struct {
	requests, burst, perNode int64
	window                   time.Duration
	nodes, mode              int
}

// rlCluster is N simulated Nodes over one State Store.
type rlCluster struct {
	p     rlParams
	clk   *clocktest.Fake
	exec  *executor.Executor
	pool  *exchange.Pool
	nodes []*snapshot.Snapshot
	fail  *atomic.Bool
}

// policyYAML returns the Bundle of p: a Gateway, an Upstream, and a Route
// with the ratelimit Policy.
func (p rlParams) bundle() map[string][]byte {
	limit := fmt.Sprintf("{requests: %d, window: %dms, burst: %d", p.requests, p.window.Milliseconds(), p.burst)
	if p.perNode > 0 {
		limit += ", perNodeCeiling: " + strconv.FormatInt(p.perNode, 10)
	}
	limit += "}"
	localOnly := p.mode == rlLocalOnly
	return map[string][]byte{
		"ruralz.yaml": []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge}\nspec:\n  listeners:\n    - {name: http, protocol: http, port: 8080}\n"),
		"rl.yaml": []byte(strings.Join([]string{
			"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: u}\nspec:\n  protocol: http\n  endpoints: [{address: \"127.0.0.1:9\"}]\n",
			"apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata: {name: rl}\nspec:\n  match: {path: {prefix: /}}\n  upstreams: [{name: u}]\n  policies: [{name: rl}]\n",
			"apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: rl}\nspec:\n  type: ratelimit\n  failureMode: open\n  config:\n" +
				"    key: 'request.headers[\"x-key\"]'\n    cost: 'int(request.headers[\"x-cost\"])'\n" +
				"    localOnly: " + strconv.FormatBool(localOnly) + "\n    limits: [" + limit + "]\n",
		}, "---\n")),
	}
}

// newRLCluster builds p.nodes Nodes: each compiles p's Bundle with its own
// key table into a snapshot whose State Store handle comes from the one
// manager every Node shares.
func newRLCluster(t *testing.T, p rlParams) *rlCluster {
	t.Helper()
	tc := tools(t)
	clk := clocktest.New(epoch())
	c := &rlCluster{p: p, clk: clk, fail: &atomic.Bool{}, exec: executor.New(executor.Deps{Clock: clk}), pool: exchange.NewPool(exchange.Deps{})}
	c.fail.Store(p.mode == rlFailing)
	mgr := manager.New(context.Background(), manager.Config{
		Deps: statestore.Deps{Clock: clk},
		Openers: manager.Openers{Memory: func(ctx context.Context, cfg statestore.Config, deps statestore.Deps) (statestore.Driver, error) {
			d, err := memory.Open(ctx, cfg, deps)
			if err != nil {
				return nil, err
			}
			return &failingDriver{Driver: d, fail: c.fail}, nil
		}},
	})
	t.Cleanup(func() { _ = mgr.Close(context.Background()) })
	files := p.bundle()
	res := tc.run(t, memBundle{}.withFiles(files), noEnv, nil, false)
	if res.Validated == nil || res.HasErrors() {
		t.Skipf("the limit is not valid configuration (RZ-CFG-005 rules): %s", texts(res.Diagnostics))
	}
	var seed uint64
	for n := range p.nodes {
		seed++
		tbl, err := ratelimit.NewTable(clk, ratelimit.FileMode{}, ratelimit.TableOptions{Entries: 8192, LocalOnlyEntries: 2048, Shards: 4, Seed: seed})
		if err != nil {
			t.Fatal(err)
		}
		reg := filter.NewRegistry(map[v1alpha1.PolicyType]filter.Factory{v1alpha1.PolicyTypeRateLimit: ratelimit.Factory{Table: tbl}}, tbl)
		node := newNodeCompiler(t, clk, nodeParts{Filters: reg, Stores: mgr})
		compiled, ds, err := node.comp.Compile(t.Context(), compile.Input{Validated: res.Validated, Files: res.Files})
		if err != nil || ds.HasErrors() || compiled == nil {
			t.Fatalf("Node %d does not compile the Bundle: %v\n%s", n, err, texts(ds))
		}
		t.Cleanup(compiled.Discard)
		if compiled.Snapshot.StateStore == nil || len(compiled.Snapshot.Routes) != 1 {
			t.Fatalf("Node %d compiled no State Store or not one Route", n)
		}
		c.nodes = append(c.nodes, compiled.Snapshot)
	}
	return c
}

// drive runs the events and returns the admissions in time order. An
// event byte selects a Node (its low two bits), a key (the next two bits)
// and how far time moves first (its high bits: 0 to 63 steps of window /
// 16); its cost is the byte modulo 3, plus 1. In rlFirstSeen every Node
// first takes a flood of firstSeenBudget + 1 to firstSeenBudget + 40
// keys of its own at the start, all first-seen at once, and later events
// request only flood keys of the Node they select, within localOnlyLife.
func (c *rlCluster) drive(t *testing.T, events []byte) []rlAdmission {
	t.Helper()
	start := c.clk.Now()
	var out []rlAdmission
	request := func(node int, key string, cost int64) {
		if c.admit(t, node, key, cost) {
			out = append(out, rlAdmission{node: node, key: key, at: c.clk.Now().Sub(start), cost: cost})
		}
	}
	flood := 0
	if c.p.mode == rlFirstSeen {
		flood = firstSeenBudget + 1 + len(events)%40
		for n := range c.p.nodes {
			for i := range flood {
				request(n, floodKey(n, i), 1)
			}
		}
	}
	for i, b := range events {
		step := time.Duration(b>>4) * c.p.window / 16
		if c.p.mode == rlFirstSeen && c.clk.Now().Add(step).Sub(start) >= localOnlyLife-time.Second {
			break
		}
		c.clk.Advance(step)
		node := int(b&3) % c.p.nodes
		cost := int64(b%3) + 1
		key := "k" + strconv.Itoa(int(b>>2&3))
		if c.p.mode == rlFirstSeen {
			// The flood's keys alternate within and past the budget.
			key = floodKey(node, (i*97+int(b))%flood)
		}
		request(node, key, cost)
	}
	return out
}

// floodKey is key i of Node n's first-seen flood.
func floodKey(n, i int) string { return "f" + strconv.Itoa(n) + "-" + strconv.Itoa(i) }

// pastBudget reports a flood key past its Node's first-seen budget, whose
// entry is local-only.
func pastBudget(key string) bool {
	_, idx, ok := strings.Cut(key, "-")
	if !ok || !strings.HasPrefix(key, "f") {
		return false
	}
	i, err := strconv.Atoi(idx)
	return err == nil && i >= firstSeenBudget
}

// admit runs one request of key and cost through Node node's chain in
// onRequestHeaders, as the request handler does, and reports whether the
// Policy admitted it: no generated response. A 429 denies it; anything
// else fails t.
func (c *rlCluster) admit(t *testing.T, node int, key string, cost int64) bool {
	t.Helper()
	snap := c.nodes[node]
	rt := snap.Routes[0]
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "http://gw.example/orders", nil)
	r.Header.Set("X-Key", key)
	r.Header.Set("X-Cost", strconv.FormatInt(cost, 10))
	x := c.pool.Get()
	x.Begin(r, &exchange.Start{
		Time: c.clk.Now(), Listener: "http", Scheme: "http", Path: "/orders",
		Source: filter.Source{IP: netip.MustParseAddr("192.0.2.10"), Port: 40000}, Limits: snap.Gateway.Limits,
	})
	x.SetRoute(rt, nil)
	run := c.exec.Begin(x, &rt.Chain, snap.StateStore.Store())
	gen := run.Request(t.Context(), phase.OnRequestHeaders)
	status := http.StatusOK
	if gen != nil {
		status = gen.Status
	}
	x.SetFinal(filter.Final{Status: status}, 0)
	run.Log(context.WithoutCancel(t.Context()))
	run.Release()
	c.pool.Put(x)
	switch {
	case gen == nil:
		return true
	case gen.Status == http.StatusTooManyRequests && (gen.Code == ratelimit.CodeLocalDeny || gen.Code == ratelimit.CodeGlobalDeny):
		return false
	default:
		t.Fatalf("Node %d key %s: the chain answered %d %s", node, key, gen.Status, gen.Code)
		return false
	}
}

// failingDriver is the memory driver, every Consume failing with a
// timeout while fail is set.
type failingDriver struct {
	statestore.Driver
	fail *atomic.Bool
}

func (d *failingDriver) Consume(ctx context.Context, rb *statestore.RequestBudget, calls []*statestore.Call) int {
	if d.fail.Load() {
		for _, c := range calls {
			c.Done, c.Err = false, statestore.ErrTimeout
		}
		return len(calls)
	}
	return d.Driver.Consume(ctx, rb, calls)
}

// checkSliding fails t when some span of one window, [s, s + window),
// admits more than limit units of key.
func checkSliding(t *testing.T, key string, as []rlAdmission, window time.Duration, limit int64) {
	t.Helper()
	for i := range as {
		var units int64
		for j := i; j < len(as) && as[j].at < as[i].at+window; j++ {
			units += as[j].cost
		}
		if units > limit {
			t.Fatalf("key %s: %d units admitted in the window from %v, want at most requests + burst = %d", key, units, as[i].at, limit)
		}
	}
}

// checkAligned fails t when some epoch-aligned window admits more than
// limit units of key across the Nodes.
func checkAligned(t *testing.T, key string, as []rlAdmission, window time.Duration, limit int64) {
	t.Helper()
	sums := map[int64]int64{}
	for _, a := range as {
		ns := epoch().Add(a.at).UnixNano()
		w := ns - ns%int64(window)
		sums[w] += a.cost
		if sums[w] > limit {
			t.Fatalf("key %s: %d units admitted in the aligned window at %v while failing open, want at most N × ceiling = %d",
				key, sums[w], time.Unix(0, w).UTC(), limit)
		}
	}
}

// checkBucket fails t when a Node's bucket admits more of key than its
// ceiling plus its refill: over the span L from one admission to a later
// one, at most ceiling + L × ceiling / window units.
func checkBucket(t *testing.T, key string, as []rlAdmission, window time.Duration, ceiling int64, nodes int) {
	t.Helper()
	for n := range nodes {
		var mine []rlAdmission
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
					t.Fatalf("key %s: Node %d admitted %d units over %v, want at most %d from a bucket of %d per %v",
						key, n, units, span, limit, ceiling, window)
				}
			}
		}
	}
}
