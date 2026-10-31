// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// The required State Store scenarios of
// docs/engineering/03-testing-and-quality-strategy.md "Integration tests"
// that M1 serves: the script cache flush, the full post-commit queue and
// the sentinel failover. The vector search scenarios belong to
// ai.semantic-cache, Planned (M3).

// TestNoScriptAfterFlush checks, on every kind, that after a script cache
// flush the EVALSHA of the next request meets NOSCRIPT and that request
// applies its Policy's failureMode without a second round trip: open
// admits, closed answers 503 RZ-STS-002; the Node reloads the script off
// the request path, and the next EVALSHA succeeds.
func TestNoScriptAfterFlush(t *testing.T) {
	for _, k := range allKinds() {
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k})
			b := bundle{
				stateStore: redisStore(topologyName(k), urlRef),
				upstream:   e.up.Addr(),
				docs: []string{
					rateLimit("rl-open", 1000, "1m", "", "open"),
					rateLimit("rl-closed", 1000, "1m", "", "closed"),
					route("open", "/open", "rl-open"),
					route("closed", "/closed", "rl-closed"),
				},
			}
			e.start(b.render(), mainURLs())
			m := e.meter("main")
			// The flushed scripts must meet the connections the calls use:
			// a connection the State client opens loads them first.
			e.warmUp("/closed", "x-client", "c")
			for _, c := range []struct {
				path   string
				status int
				code   string
			}{
				{"/closed", http.StatusServiceUnavailable, "RZ-STS-002"},
				{"/open", http.StatusOK, ""},
			} {
				flushScripts(e)
				before := m.read()
				d := m.phase(func() { expect(t, c.path+" after a script flush", e.get(c.path, "x-client", "c"), c.status, c.code) })
				expectCommands(t, c.path+" after a script flush", d, evalsha(1), 1)
				if d.errors["NOSCRIPT"] != 1 || d.requestPath["EVAL"] != 0 {
					t.Errorf("%s: NOSCRIPT replies %d, EVAL calls %d; want one NOSCRIPT and no EVAL retry", c.path, d.errors["NOSCRIPT"], d.requestPath["EVAL"])
				}
				e.waitFor("the script reload", func() bool { return m.reloaded(before) })
				d = m.phase(func() { expect(t, c.path+" after the reload", e.get(c.path, "x-client", "c"), http.StatusOK, "") })
				expectCommands(t, c.path+" after the reload", d, evalsha(1), 1)
				if d.errors["NOSCRIPT"] != 0 {
					t.Errorf("%s: the EVALSHA after the reload met NOSCRIPT", c.path)
				}
			}
		})
	}
}

// flushScripts flushes the script cache of every primary.
func flushScripts(e *env) {
	e.t.Helper()
	for _, srv := range e.stores["main"].Deployment().Nodes() {
		if _, err := srv.Do(e.t.Context(), "SCRIPT", "FLUSH"); err != nil {
			e.t.Fatalf("SCRIPT FLUSH on %s: %v", srv.Name(), err)
		}
	}
}

// TestFullPostCommitQueue checks that a full post-commit queue class drops
// writes with a counter, never delaying a response, and that no other
// class drops a write: on a Route whose only State Store Policy is cache,
// the counting RESP proxy holds every reply to the scripts that write, so
// the eight writers wait on their batches, while unsafe requests, which
// make no request-path call and queue a generation bump each, complete at
// once; ruralz_state_writes_dropped_total{kind="cache_invalidate"} counts
// once the invalidation class fills, kind="cache_set" and kind="refund"
// stay 0, and the queue drains once the replies pass. The class is the
// invalidations', not the stores': a store follows a lookup, which the
// State client pipelines on the connections whose replies the proxy
// holds, so the lookup waits behind them and queues no store.
func TestFullPostCommitQueue(t *testing.T) {
	for _, k := range allKinds() {
		if !counted(k) {
			continue
		}
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k})
			// The bumps' stateStoreTimeout outlasts the Node clock's moves
			// before each metrics scrape, so the held batches never time out.
			b := bundle{
				stateStore: redisStore(topologyName(k), urlRef),
				upstream:   e.up.Addr(),
				docs: []string{
					policy("cache", "cache", "config: {}", "stateStoreTimeout: 10m"),
					route("cached", "/cached", "cache"),
				},
			}
			e.start(b.render(), mainURLs())
			m := e.meter("main")
			for range 2 {
				expect(t, "a warm-up invalidation", e.do(http.MethodPost, "/cached/warm-up", nil), http.StatusOK, "")
			}
			e.waitFor("the warm-up bumps", func() bool { return answered(m.since(reading{}).log) >= 2 })
			before := e.metrics()
			release := m.hold(queueHold)
			defer release()
			start := time.Now()
			var sent atomic.Int64
			dropped := 0.0
			for batch := 0; dropped == 0; batch++ {
				if batch == maxQueueBatches {
					t.Fatalf("no bump dropped after %d unsafe requests", sent.Load())
				}
				postBatch(t, e, &sent, queueBatch)
				dropped = promtext.Delta(before, e.metrics(), "ruralz_state_writes_dropped_total", promtext.L("kind", "cache_invalidate"))
			}
			if since := time.Since(start); since > queueHold-time.Second {
				t.Fatalf("filling the queue took %v, too long for the held replies: the State client's 10 s keepalive ends held connections", since)
			}
			after := e.metrics()
			for _, kind := range []string{"cache_set", "refund"} {
				if d := promtext.Delta(before, after, "ruralz_state_writes_dropped_total", promtext.L("kind", kind)); d != 0 {
					t.Errorf("ruralz_state_writes_dropped_total{kind=%q} rose by %v; only the full class drops", kind, d)
				}
			}
			release()
			e.waitFor("the queue to drain", func() bool {
				v, err := e.metrics().Value("ruralz_state_write_queue_items", nil)
				return err == nil && v == 0
			})
			if got := promtext.Delta(before, e.metrics(), "ruralz_state_writes_dropped_total", promtext.L("kind", "cache_invalidate")); got != dropped {
				t.Errorf("the drained queue dropped %v more bumps; the held batches must be written", got-dropped)
			}
		})
	}
}

// Bounds of the full-queue scenario: the counting proxy holds the
// writers' replies for queueHold, below the State client's 10 s keepalive,
// and the test sends unsafe requests queueBatch at a time; the class holds
// 6,400 items or 1 MiB, about 2,180 bumps, and the eight writers 512 more.
const (
	queueHold       = 9 * time.Second
	queueBatch      = 500
	maxQueueBatches = 12
)

// postBatch sends n unsafe requests to distinct URIs, 32 at a time, and
// fails the test unless each completes with the mock Upstream's 200.
func postBatch(t *testing.T, e *env, sent *atomic.Int64, n int) {
	t.Helper()
	var wg sync.WaitGroup
	next := make(chan int)
	var failed atomic.Value
	for range 32 {
		wg.Go(func() {
			for i := range next {
				r, err := e.send(e.client, http.MethodPost, "/cached/"+strconv.Itoa(i), nil)
				if err == nil && r.status != http.StatusOK {
					err = statusError{r}
				}
				if err != nil {
					failed.CompareAndSwap(nil, err)
				}
			}
		})
	}
	for range n {
		next <- int(sent.Add(1))
	}
	close(next)
	wg.Wait()
	if err, _ := failed.Load().(error); err != nil {
		t.Fatalf("an unsafe request failed while the writers waited: %v", err)
	}
}

// statusError reports a response with an unexpected status.
type statusError struct{ r response }

func (e statusError) Error() string { return "unexpected response " + e.r.String() }

// TestSentinelFailover checks, on the sentinel kinds, that after SENTINEL
// FAILOVER the Node follows the promoted replica within 15 s (target),
// loading its scripts there: a ratelimit call that the old primary served
// reaches the new one, which saw SCRIPT LOAD first.
func TestSentinelFailover(t *testing.T) {
	for _, k := range []redisserver.Kind{redisserver.ProcessSentinel, redisserver.Redis8Sentinel, redisserver.Valkey9Sentinel} {
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k})
			b := bundle{
				stateStore: redisStore("sentinel", urlRef),
				upstream:   e.up.Addr(),
				docs:       []string{rateLimit("rl", 100000, "1m", "", "closed"), route("limited", "/limited", "rl")},
			}
			e.start(b.render(), mainURLs())
			expect(t, "a request before the failover", e.get("/limited", "x-client", "f"), http.StatusOK, "")
			dep := e.stores["main"].Deployment()
			promoted := dep.Replica()
			before := commandStats(e, promoted)
			if err := dep.Failover(e.t.Context()); err != nil {
				t.Fatal(err)
			}
			// The Node follows within 15 s (target): its reconnect pacing
			// and refreshes run on the Node clock, which moves 1 s a step,
			// the sentinel and the servers in real time, which the steps
			// leave pollEvery of each.
			start := time.Now()
			followed, advanced := false, time.Duration(0)
			for !followed && time.Since(start) < failoverWithin {
				if advanced < failoverWithin {
					e.advance(time.Second)
					advanced += time.Second
				}
				r, err := e.send(e.client, http.MethodGet, "/limited", nil, "x-client", "f")
				after := commandStats(e, promoted)
				followed = err == nil && r.status == http.StatusOK && after["EVALSHA"] > before["EVALSHA"]
				time.Sleep(10 * pollEvery)
			}
			if !followed {
				t.Fatalf("the Node did not follow the promoted replica within %v", failoverWithin)
			}
			if got := commandStats(e, promoted); got["SCRIPT LOAD"] <= before["SCRIPT LOAD"] {
				t.Errorf("the promoted replica served EVALSHA without a SCRIPT LOAD")
			}
		})
	}
}

// failoverWithin is the bound within which a Node follows a sentinel
// failover (target).
const failoverWithin = 15 * time.Second

// commandStats returns srv's INFO commandstats calls by command name.
func commandStats(e *env, srv *redisserver.Server) map[string]int64 {
	e.t.Helper()
	info, err := srv.Info(e.t.Context(), "commandstats")
	if err != nil {
		e.t.Fatalf("INFO commandstats on %s: %v", srv.Name(), err)
	}
	out := map[string]int64{}
	for k, v := range info {
		if name, ok := cutStat(k); ok {
			out[name] += statField(v, "calls")
		}
	}
	return out
}
