// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package redis

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// The State Store integration suites
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"): the conformance suite on every kind RURALZ_TEST_STATESTORE
// selects, which runs ruralz_v1.lua itself, and the scenarios of real
// servers: a script cache flush, a scripted failover, TLS with ACL users,
// a cluster's slots, a black hole and the memory readings.

// liveTimeout bounds one live operation of a test.
const liveTimeout = 30 * time.Second

// openLive opens a driver on url with the real clock; the test closes it.
func openLive(t *testing.T, topo statestore.Topology, url string, deps statestore.Deps, opts Options) *Driver {
	t.Helper()
	if deps.Clock == nil {
		deps.Clock = clock.Real()
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	d, err := New(context.Background(), statestore.Config{Driver: statestore.DriverRedis, Topology: topo, URL: url}, deps, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), liveTimeout)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return d
}

// owner returns the server holding key: the primary, or the cluster
// primary that does not redirect it.
func owner(t *testing.T, dep *redisserver.Deployment, key string) *redisserver.Server {
	t.Helper()
	for _, n := range dep.Nodes() {
		if _, err := n.Do(t.Context(), "EXISTS", key); err == nil {
			return n
		}
	}
	t.Fatalf("no server owns the key")
	return nil
}

// liveHarness adapts a deployment to the conformance suite: real time,
// keys read and tampered with through the redisserver admin client.
func liveHarness(dep *redisserver.Deployment, opts Options) statestoretest.Harness {
	var mu sync.Mutex
	nodes := map[statestore.Driver]*skewClock{}
	return statestoretest.Harness{
		Parallel: true,
		Open: func(t *testing.T, o statestoretest.OpenOptions) statestore.Driver {
			node := &skewClock{base: clock.Real()}
			d := openLive(t, dep.Topology(), dep.URL(), statestore.Deps{Clock: node, MACKey: o.MACKey}, opts)
			mu.Lock()
			nodes[d] = node
			mu.Unlock()
			return d
		},
		OpenPeer: func(t *testing.T, _ statestore.Driver, o statestoretest.OpenOptions) statestore.Driver {
			// Another Node of the Cell on the same deployment.
			node := &skewClock{base: clock.Real()}
			d := openLive(t, dep.Topology(), dep.URL(), statestore.Deps{Clock: node, MACKey: o.MACKey}, opts)
			mu.Lock()
			nodes[d] = node
			mu.Unlock()
			return d
		},
		Advance: func(t *testing.T, _ statestore.Driver, by time.Duration) {
			if err := clock.Real().Sleep(t.Context(), by); err != nil {
				t.Fatal(err)
			}
		},
		DigestSlot: keys.DigestSlot,
		SkewNode: func(_ *testing.T, d statestore.Driver, off time.Duration) {
			mu.Lock()
			defer mu.Unlock()
			nodes[d].off.Store(int64(off))
		},
		Inspect: func(t *testing.T, _ statestore.Driver, k statestoretest.Key) (statestoretest.Stored, bool) {
			t.Helper()
			key, field := inspectKey(k)
			srv := owner(t, dep, key)
			var v redisserver.Value
			var err error
			if field != "" {
				v, err = srv.Do(t.Context(), "HGET", key, field)
			} else {
				v, err = srv.Do(t.Context(), "GET", key)
			}
			if err != nil {
				t.Fatal(err)
			}
			if v.Null {
				return statestoretest.Stored{}, false
			}
			ttl, err := srv.Do(t.Context(), "PTTL", key)
			if err != nil {
				t.Fatal(err)
			}
			return statestoretest.Stored{Value: v.Str, TTL: time.Duration(ttl.Int) * time.Millisecond}, true
		},
		Tamper: func(t *testing.T, _ statestore.Driver, k statestore.CacheKey, v statestore.Digest, edit func([]byte) []byte) {
			t.Helper()
			part := string(keys.AppendCachePartition(nil, k))
			field := string(keys.AppendField(nil, keys.FieldEntry, v))
			srv := owner(t, dep, part)
			cur, err := srv.Do(t.Context(), "HGET", part, field)
			if err != nil || cur.Null {
				t.Fatalf("no stored variant to tamper with: %v", err)
			}
			if _, err := srv.Do(t.Context(), "HSET", part, field, string(edit([]byte(cur.Str)))); err != nil {
				t.Fatal(err)
			}
		},
	}
}

// TestIntegrationConformance runs the driver conformance suite on every
// kind RURALZ_TEST_STATESTORE selects: Redis, Valkey, Dragonfly and
// clusters run ruralz_v1.lua itself.
func TestIntegrationConformance(t *testing.T) {
	for _, k := range redisserver.Kinds(t) {
		t.Run(string(k), func(t *testing.T) {
			dep := redisserver.Start(t, k, redisserver.Options{})
			statestoretest.RunConformance(t, liveHarness(dep, Options{}))
		})
	}
}

// eventually retries fn every 10 ms until it reports done or the live
// timeout passes.
func eventually(t *testing.T, what string, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(liveTimeout)
	for !fn() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// liveCall runs one GCRA call of 1,000 per second on digest name.
func liveCall(d *Driver, name string) *statestore.Call {
	c := gcraCall(statestore.DigestOf(name), statestore.GCRALimit{Requests: 1000, Window: time.Second, Burst: 1000})
	c.Timeout = time.Second
	d.Consume(context.Background(), budget(), []*statestore.Call{c})
	return c
}

// proxiedServer puts a counting RESP proxy in front of srv.
func proxiedServer(t *testing.T, addr string) *respproxy.Proxy {
	t.Helper()
	p, err := respproxy.Start(t.Context(), respproxy.Config{Fault: faultproxy.Config{Upstream: addr}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// TestIntegrationNoScriptAfterFlush checks the required scenario: after
// a script cache flush, EVALSHA meets NOSCRIPT, the request fails without
// a second round trip, the Node reloads off the request path and the next
// EVALSHA succeeds.
func TestIntegrationNoScriptAfterFlush(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{})
	p := proxiedServer(t, dep.Primary().Addr())
	d := openLive(t, statestore.TopologyStandalone, "redis://"+p.Addr(), statestore.Deps{}, Options{})
	for i := 0; d.conns.len() < 2; i++ {
		if c := liveCall(d, "warm"+strconv.Itoa(i)); c.Err != nil || i > 200 {
			t.Fatalf("warm-up call %d: Err %v", i, c.Err)
		}
	}
	if _, err := dep.Primary().Do(t.Context(), "SCRIPT", "FLUSH"); err != nil {
		t.Fatal(err)
	}
	before := p.Snapshot()
	if c := liveCall(d, "k"); c.Err != statestore.ErrFailed {
		t.Fatalf("call after SCRIPT FLUSH: Err %v, want RZ-STS-002", c.Err)
	}
	got := p.Snapshot().Sub(before)
	if got.Errors["NOSCRIPT"] != 1 || got.Count("EVALSHA") != 1 || got.Count("EVAL") != 0 {
		t.Fatalf("after the flush: commands %v, errors %v; want one EVALSHA meeting NOSCRIPT", got.ByName, got.Errors)
	}
	eventually(t, "the reload", func() bool { return liveCall(d, "k2").Err == nil })
}

// TestIntegrationFailover checks CE-5's mechanism on a scripted
// failover: the primary dies, the replica is promoted behind the same
// endpoint, and the Node reconnects, reloads its scripts and serves again.
// The time it took is logged, not checked: the 15 s target belongs to the
// chaos experiment, and a loaded machine would stretch it.
func TestIntegrationFailover(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{Replica: true})
	p := proxiedServer(t, dep.Primary().Addr())
	d := openLive(t, statestore.TopologyStandalone, "redis://"+p.Addr(), statestore.Deps{}, Options{})
	if c := liveCall(d, "before"); c.Err != nil {
		t.Fatalf("call before the failover: Err %v", c.Err)
	}
	if err := dep.Primary().Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := dep.Promote(t.Context()); err != nil {
		t.Fatal(err)
	}
	p.SetUpstream(dep.Primary().Addr())
	if err := p.SetMode(faultproxy.Reset{}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetMode(faultproxy.Pass{}); err != nil {
		t.Fatal(err)
	}
	before := p.Snapshot()
	start := time.Now()
	eventually(t, "a call on the promoted primary", func() bool { return liveCall(d, "after").Err == nil })
	t.Logf("served again after %v (target 15 s)", time.Since(start))
	if got := p.Snapshot().Sub(before); got.Count("SCRIPT LOAD") == 0 || got.Count("EVAL") != 0 {
		t.Fatalf("after the failover: %v; want SCRIPT LOAD and no EVAL", got.ByName)
	}
}

// TestIntegrationSentinelFailover checks topology sentinel on real
// sentinels: the Node finds the primary through the sentinel, and after
// SENTINEL FAILOVER follows the promoted replica, loading its scripts on
// it; the time it took is logged against the 15 s target, not checked. It
// runs on the process sentinel kind, and on the Redis 8 and Valkey 9 ones
// when RURALZ_TEST_STATESTORE is container.
func TestIntegrationSentinelFailover(t *testing.T) {
	for _, k := range []redisserver.Kind{redisserver.ProcessSentinel, redisserver.Redis8Sentinel, redisserver.Valkey9Sentinel} {
		t.Run(string(k), func(t *testing.T) {
			dep := redisserver.Start(t, k, redisserver.Options{})
			d := openLive(t, statestore.TopologySentinel, dep.URL(), statestore.Deps{}, Options{})
			if c := liveCall(d, "before"); c.Err != nil {
				t.Fatalf("call before the failover: Err %v", c.Err)
			}
			promoted := dep.Replica()
			if err := dep.Failover(t.Context()); err != nil {
				t.Fatal(err)
			}
			start := time.Now()
			eventually(t, "a call on the promoted primary", func() bool {
				_, ok := d.loadClient().Nodes()[promoted.ClientAddr()]
				return ok && liveCall(d, "after").Err == nil
			})
			t.Logf("served again after %v (target 15 s)", time.Since(start))
			info, err := promoted.Info(t.Context(), "commandstats")
			if err != nil {
				t.Fatal(err)
			}
			if info["cmdstat_evalsha"] == "" || info["cmdstat_script|load"] == "" {
				t.Errorf("the promoted primary ran no EVALSHA or SCRIPT LOAD: %v", info)
			}
		})
	}
}

// TestIntegrationTLSAndACL checks rediss:// with an ACL user against a
// TLS-only server: verified, authenticated, neither cleartext nor
// unauthenticated.
func TestIntegrationTLSAndACL(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{
		TLS: true, Users: []redisserver.User{{Name: "ruralz", Password: "s3cret pw"}},
	})
	d := openLive(t, statestore.TopologyStandalone, dep.URL(), statestore.Deps{}, Options{RootCAs: dep.ClientTLS().RootCAs})
	if c := liveCall(d, "k"); c.Err != nil || !c.Done {
		t.Fatalf("call over TLS with ACL: Done %v Err %v", c.Done, c.Err)
	}
	if st := d.Status(); st.Cleartext || st.Unauthenticated {
		t.Fatalf("Status %+v", st)
	}
}

// TestIntegrationClusterSlots checks a three-primary cluster behind
// counting proxies: every primary loads the library, consumptive calls go
// to their slot's primary in one script per slot, and no CROSSSLOT occurs.
func TestIntegrationClusterSlots(t *testing.T) {
	var mu sync.Mutex
	proxies := map[int]*respproxy.Proxy{}
	dep := redisserver.Start(t, redisserver.ProcessCluster, redisserver.Options{Announce: func(node int, addr string) string {
		p := proxiedServer(t, addr)
		mu.Lock()
		proxies[node] = p
		mu.Unlock()
		return p.Addr()
	}})
	d := openLive(t, statestore.TopologyCluster, dep.URL(), statestore.Deps{}, Options{})
	for i := range 30 {
		if c := liveCall(d, "slot"+strconv.Itoa(i)); c.Err != nil {
			t.Fatalf("call %d: Err %v", i, c.Err)
		}
	}
	a, b := statestore.DigestOf("a"), statestore.DigestOf("b")
	ga := gcraCall(a, statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10})
	gb := gcraCall(b, statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10})
	ga.Timeout, gb.Timeout = time.Second, time.Second
	calls := []*statestore.Call{ga, gb}
	for len(calls) > 0 {
		calls = calls[d.Consume(context.Background(), budget(), calls):]
	}
	if ga.Err != nil || gb.Err != nil {
		t.Fatalf("calls in two slots: %v, %v", ga.Err, gb.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	for node, p := range proxies {
		s := p.Snapshot()
		if s.Count("SCRIPT LOAD") == 0 {
			t.Errorf("primary %d never loaded the library", node)
		}
		if s.Errors["CROSSSLOT"] != 0 {
			t.Errorf("primary %d answered CROSSSLOT", node)
		}
	}
}

// TestIntegrationBlackhole checks CE-4's mechanism: a black-holed State
// Store costs at most one timeout per call until the breaker opens, then
// calls are skipped; once the path returns, the breaker closes again.
func TestIntegrationBlackhole(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{})
	p := proxiedServer(t, dep.Primary().Addr())
	d := openLive(t, statestore.TopologyStandalone, "redis://"+p.Addr(), statestore.Deps{}, Options{})
	if c := liveCall(d, "before"); c.Err != nil {
		t.Fatalf("call before the black hole: Err %v", c.Err)
	}
	if err := p.SetMode(faultproxy.Blackhole{}); err != nil {
		t.Fatal(err)
	}
	for i := 0; d.single.brk.State() != breaker.Open; i++ {
		if i > 40 {
			t.Fatal("the breaker did not open")
		}
		c := gcraCall(statestore.DigestOf("k"), oneLimit())
		c.Timeout = 20 * time.Millisecond
		d.Consume(context.Background(), budget(), []*statestore.Call{c})
		if c.Err != statestore.ErrTimeout && c.Err != statestore.ErrBreakerOpen {
			t.Fatalf("call into the black hole: Err %v, want RZ-STS-001", c.Err)
		}
	}
	c := liveCall(d, "skipped")
	if !errors.Is(c.Err, statestore.ErrBreakerOpen) {
		t.Fatalf("call with the breaker open: Err %v, want RZ-STS-003", c.Err)
	}
	if err := p.SetMode(faultproxy.Pass{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the breaker to close", func() bool { return d.single.brk.State() == breaker.Closed })
}

// TestIntegrationMemoryReadings checks the INFO memory readings against a
// real server: the eviction policy Status reports and the 70% store rule.
func TestIntegrationMemoryReadings(t *testing.T) {
	dep := redisserver.Start(t, redisserver.Process, redisserver.Options{Config: []string{"maxmemory 64mb", "maxmemory-policy noeviction"}})
	lim := statestore.DefaultLimits()
	lim.MemoryPollSlow, lim.MemoryPollFast = time.Second, time.Second
	d := openLive(t, statestore.TopologyStandalone, dep.URL(), statestore.Deps{Limits: lim}, Options{})
	eventually(t, "a noeviction reading", func() bool { return d.Status().EvictionPolicy == statestore.Yes })
	k := statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
	eventually(t, "a store admitted", func() bool { return d.AdmitStoreBytes(k, 1) })
	if _, err := dep.Primary().Do(t.Context(), "CONFIG", "SET", "maxmemory-policy", "allkeys-lru"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the eviction reading", func() bool { return d.Status().EvictionPolicy == statestore.No })
	if _, err := dep.Primary().Do(t.Context(), "CONFIG", "SET", "maxmemory", "1"); err != nil {
		t.Fatal(err)
	}
	eventually(t, "stores to skip above 70%", func() bool { return !d.AdmitStoreBytes(k, 1) })
}
