// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/redis"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// GCRASpec makes a load generator run call GCRA through the State client
// (internal/statestore/redis) instead of sending HTTP requests: G1's
// open-loop EVALSHA calls against one Valkey shard, keys from the run's
// KeySpec (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Scenarios").
type GCRASpec struct {
	// URL is the State Store URL, rediss://user:password@host:port; the
	// LoadSpec's CA verifies the server.
	URL string `json:"url"`
	// Nodes is the emulated Nodes, each a State client with connections of
	// its own (2 pipelined plus 8 dedicated per shard), the calls spread
	// over them in turn: 1 by default, 1,000 in G1's connection variant.
	Nodes int `json:"nodes,omitempty"`
	// Requests and Window are the one limit each call checks; S2's
	// ratelimit-bench, 20,000 per 1 s, denies none at G1's key rates.
	Requests int64         `json:"requests"`
	Window   time.Duration `json:"windowNs"`
	// Timeout is each call's stateStoreTimeout; default 1 s.
	Timeout time.Duration `json:"timeoutNs,omitempty"`
}

// gcraTarget is a loadgen Target calling GCRA on its drivers in turn.
type gcraTarget struct {
	drivers []*redis.Driver
	keys    loadgen.KeySource
	limit   []statestore.GCRALimit
	timeout time.Duration
	next    atomic.Uint64
}

// noStatus discards the drivers' degraded reasons and cleartext hops.
type noStatus struct{}

func (noStatus) SetDegraded(catalog.Reason, string, bool) {}
func (noStatus) SetCleartextHops(catalog.Hop, int)        {}

// newGCRATarget opens the emulated Nodes' State clients.
func newGCRATarget(ctx context.Context, g GCRASpec, caPEM string, keys loadgen.KeySource) (*gcraTarget, error) {
	if g.URL == "" || g.Requests < 1 || g.Window <= 0 {
		return nil, errors.New("lab: gcra: a State Store URL and a positive limit are needed")
	}
	if keys == nil {
		return nil, errors.New("lab: gcra: the calls need keys")
	}
	nodes := max(g.Nodes, 1)
	timeout := g.Timeout
	if timeout <= 0 {
		timeout = time.Second
	}
	opts := redis.DefaultOptions()
	if caPEM != "" {
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM([]byte(caPEM)) {
			return nil, errors.New("lab: gcra: the CA bundle holds no certificate")
		}
		opts.RootCAs = pool
	}
	t := &gcraTarget{
		keys: keys, timeout: timeout,
		limit: []statestore.GCRALimit{{Requests: g.Requests, Window: g.Window, Burst: g.Requests}},
	}
	for i := range nodes {
		cfg := statestore.Config{
			Role: statestore.RoleMain, Driver: statestore.DriverRedis, Topology: statestore.TopologyStandalone,
			URL: g.URL, DefaultTimeout: timeout,
		}
		deps := statestore.Deps{
			Clock: clock.Real(), Status: noStatus{}, NodeID: fmt.Sprintf("g1-node-%d", i),
			Limits: statestore.DefaultLimits(),
		}
		d, err := redis.New(ctx, cfg, deps, opts)
		if err != nil {
			t.close(ctx)
			return nil, fmt.Errorf("lab: gcra: emulated Node %d: %w", i, err)
		}
		t.drivers = append(t.drivers, d)
	}
	return t, nil
}

// Do runs one GCRA call: an allow or a deny is a Success, a State Store
// failure a ServerError carrying its RZ-STS code.
func (t *gcraTarget) Do(ctx context.Context, r loadgen.Request) loadgen.Response {
	d := t.drivers[t.next.Add(1)%uint64(len(t.drivers))]
	key := t.keys.Key(r.Seq)
	out := make([]statestore.GCRAOutcome, len(t.limit))
	call := &statestore.Call{
		Kind: statestore.OpGCRA, Timeout: t.timeout,
		GCRA: statestore.GCRA{Policy: "g1", Digest: statestore.DigestOf(key), Limits: t.limit, Cost: 1, Out: out},
	}
	rb := statestore.NewRequestBudget(t.timeout, 0)
	d.Consume(ctx, &rb, []*statestore.Call{call})
	if call.Err != nil {
		return loadgen.Response{Outcome: loadgen.ServerError, Status: 503, Code: call.Err.Code(), Key: key, Err: call.Err}
	}
	if !call.Done {
		return loadgen.Response{Outcome: loadgen.ServerError, Status: 503, Key: key, Err: errors.New("lab: gcra: the call was not decided")}
	}
	return loadgen.Response{Outcome: loadgen.Success, Status: 200, Key: key}
}

// close closes every driver.
func (t *gcraTarget) close(ctx context.Context) {
	for _, d := range t.drivers {
		_ = d.Close(ctx)
	}
}
