// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter/cache"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// CE-6's State Stores hold 64 MiB, and the stale check's header carries
// the fetch number the mock echoes.
const (
	ce6MaxMemory   = "64mb"
	ce6FetchHeader = "X-Chaos-Fetch"
	// EvictionReason is the degraded reason of an eviction policy a
	// connection's Policies cannot use.
	EvictionReason = "state_store_eviction_policy"
	// StoreSkipShare is the used share of maxmemory above which a Node
	// skips Response Cache stores on the main connection (target).
	StoreSkipShare = 0.70
)

// TestCE6StateStoreFull is CE-6: a State Store filled to maxmemory under
// noeviction, on the main connection and on a dedicated stateStore.cache
// connection, then the variants under allkeys-lru on either connection
// and under volatile-ttl on the cache connection
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"). The runs also assert the failure catalog row FC-13
// (FC13Criteria).
func TestCE6StateStoreFull(t *testing.T) {
	t.Run("main-noeviction", func(t *testing.T) { ce6(t, "main-noeviction", false, "noeviction") })
	t.Run("cache-noeviction", func(t *testing.T) { ce6(t, "cache-noeviction", true, "noeviction") })
	t.Run("main-allkeys-lru", func(t *testing.T) { ce6(t, "main-allkeys-lru", false, "allkeys-lru") })
	t.Run("cache-allkeys-lru", func(t *testing.T) { ce6(t, "cache-allkeys-lru", true, "allkeys-lru") })
	t.Run("cache-volatile-ttl", func(t *testing.T) { ce6(t, "cache-volatile-ttl", true, "volatile-ttl") })
}

// ce6 runs one variant: the filled State Store is the cache
// connection's when onCache is set, else the main one, under policy.
func ce6(t *testing.T, variant string, onCache bool, policy string) {
	s := saturation(t)
	plan := PlanLimits(suite.Profile.LoadShare*s*0.6, suite.Profile.Nodes)
	filled := topology.StateStoreOptions{Config: []string{"maxmemory " + ce6MaxMemory, "maxmemory-policy " + policy}}
	spec := Spec{
		Name: CE6 + "/" + variant, Bundle: plan.Apply, SampleEvery: 250 * time.Millisecond,
		Mock: func(_ int, c *mockup.Config) {
			c.Behavior = mockup.Behavior{Echo: true, Header: http.Header{"Cache-Control": {"max-age=600"}}}
		},
	}
	if onCache {
		spec.CacheStore = &filled
	} else {
		spec.StateStore = filled
	}
	r := suite.Start(t, spec)
	ctx := r.Context()
	target := r.Primaries()[0]
	if onCache {
		target = r.Cache.Deployment().Primary()
	}
	// Once a noeviction deployment is full, the cached stream asks for
	// URIs it never asked for, whose stores meet the full deployment:
	// until then the entries of its first 2,000 serve as hits.
	var uris atomic.Uint64
	cached := func(q *http.Request, seq uint64) {
		q.URL.Path = PathCached + "/c-" + strconv.FormatUint(uris.Load(), 10) + "-" + strconv.FormatUint(seq%2000, 10)
	}
	streams := append(plan.Streams(), StreamSpec{Name: "cached", Path: PathCached, Rate: suite.Profile.LoadShare * s * 0.4, Prepare: cached})
	l := r.StartLoad(LoadSpec{Duration: Baseline + FaultPhase + RecoveryTime, Streams: streams})
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)
	evictedBefore, err := EvictedKeys(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	ttl := time.Duration(0)
	if policy == "volatile-ttl" {
		// Filler outlives every entry and generation key, so the
		// entries and generation keys go first.
		ttl = 7 * 24 * time.Hour
	}
	var checker *staleChecker
	var primed []string
	if policy == "volatile-ttl" {
		checker = startStaleChecker(ctx, r)
		primed = primeEvictable(ctx, t, r)
	}
	start := time.Now()
	var skipAt time.Time
	if !onCache && policy == "noeviction" {
		// Above 70% the Nodes skip stores on the main connection.
		r.Inject("fill-75%", target.Name(), func() error {
			_, _, err := Fill(ctx, target, 0.75, ttl, 0)
			return err
		})
		skipAt = time.Now()
		r.Hold(l, "above-70%", 20*time.Second)
	}
	var limit int64
	fullAt := r.Inject("fill-full", target.Name(), func() error {
		if _, _, err := Fill(ctx, target, 1.01, ttl, 1<<20); err != nil {
			return err
		}
		if policy != "noeviction" {
			return nil
		}
		// Hold the deployment full while the Nodes' short-lived keys expire.
		var err error
		if limit, err = Pin(ctx, target); err != nil {
			return err
		}
		uris.Add(1)
		return nil
	})
	var gen genEvictions
	if primed != nil {
		gen = checkEvictedGenerations(ctx, t, r, target, primed)
	}
	fault := r.Hold(l, PhaseFault, FaultPhase-fullAt.Sub(start))
	evicted, err := EvictedKeys(ctx, target)
	if err != nil {
		t.Fatal(err)
	}
	if checker != nil {
		checker.stop()
	}
	end := r.Inject("unfill", target.Name(), func() error {
		if limit > 0 {
			if err := Unpin(ctx, target, limit); err != nil {
				return err
			}
		}
		return Unfill(ctx, target)
	})
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	for _, st := range l.Streams {
		r.IntervalLog(st.Spec.Name, st)
	}
	r.Baseline(l, base)
	nodes := r.NodeNames()
	r.Breakdown("evicted_keys", float64(evicted-evictedBefore))
	skipped := func(from, to time.Time) float64 {
		return r.SumIncrease("ruralz_cache_store_skipped_total", promtext.L("reason", "memory"), from, to)
	}
	closed := l.StreamNamed("closed")
	switch variant {
	case "main-noeviction":
		r.Add(
			Check("stores skipped for memory above 70%", Functional, skipped(skipAt, fullAt), GT, 0),
			Check("keys evicted", Functional, float64(evicted-evictedBefore), EQ, 0),
			Check("ratelimit fail-open decisions once full", Functional, r.SumIncrease("ruralz_ratelimit_decisions_total", promtext.L("policy", PolicyLimited, "result", "fail_open"), fullAt, fault.End), GT, 0),
			Check("closed-Policy RZ-STS-002 responses once full", Functional, float64(CodesIn(closed, fullAt, fault.End, CodeSTSError)), GT, 0),
		)
		r.RecoveryCriteria(l, base, rec, end)
	case "cache-noeviction":
		r.Add(
			Check("stores skipped for memory", Functional, skipped(base.Start, rec.End), EQ, 0),
			Check("refused stores counted as call errors", Functional, r.SumIncrease("ruralz_state_calls_total", promtext.L("op", "cache_set", "result", "error"), fullAt, fault.End), GT, 0),
			Check("failed requests", Functional, float64(LoadStats(l, base.Start, rec.End).Failed), EQ, 0),
			Check("main-connection RZ-STS responses", Functional, float64(CodesIn(closed, base.Start, rec.End, STSCodes()...)), EQ, 0),
			Check("main-connection fail-open decisions", Functional, r.SumIncrease("ruralz_ratelimit_decisions_total", promtext.L("result", "fail_open"), base.Start, rec.End), EQ, 0),
		)
	case "main-allkeys-lru", "cache-allkeys-lru":
		r.Add(Check("Nodes raising state_store_eviction_policy", Functional, float64(len(r.Sampler.ReasonRaised(nodes, EvictionReason, base.Start, rec.End))), EQ, float64(len(nodes))))
	case "cache-volatile-ttl":
		stale, checks := checker.result()
		r.Breakdown("stale_checks", float64(checks))
		r.Breakdown("primed_uris", float64(len(primed)))
		r.Add(
			Check("keys evicted while full", Functional, float64(evicted-evictedBefore), GT, 0),
			Check("Nodes raising state_store_eviction_policy", Functional, float64(len(r.Sampler.ReasonRaised(nodes, EvictionReason, base.Start, rec.End))), EQ, 0),
			Check("invalidated entries served", Functional, float64(stale), EQ, 0),
			Check("stale checks made", Functional, float64(checks), GT, 0),
			Check("failed requests", Functional, float64(LoadStats(l, base.Start, rec.End).Failed), EQ, 0),
			Check("stored URIs whose generation key the fill evicted", Functional, float64(gen.evicted), GT, 0),
			Check("hits on a URI after its generation key was evicted", Functional, float64(gen.hits), EQ, 0),
			Check("misses on such a URI not answered by the Upstream", Functional, float64(gen.failed), EQ, 0),
		)
	}
	if variant != "main-noeviction" {
		// Every run's recovery: errors back to zero and no degraded
		// reason left that the fill raised; main-noeviction checks its
		// breakers too (RecoveryCriteria).
		r.Add(Check("recovery errors", Functional, float64(ErrorsAfter(l, rec.Start, rec.End, STSCodes()...)), EQ, 0))
		r.StaleReasons(nodes, base.End, rec.End)
	}
	r.Add(FC13Criteria(policy, onCache, evicted-evictedBefore, len(r.Sampler.ReasonRaised(nodes, EvictionReason, base.Start, rec.End)))...)
	r.Finish()
}

// ce6EvictURIs is how many URIs the volatile-ttl run stores before the
// fill, whose generation keys the fill evicts; ce6HotEntry outlasts the
// hot-entry layer's 1 s (target), after which a lookup reads the
// generation key from the State Store again.
const (
	ce6EvictURIs = 64
	ce6HotEntry  = 2 * time.Second
)

// ce6Client fetches through the http balancer without keep-alives, so
// consecutive requests reach the Nodes in turn.
func ce6Client() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{DisableKeepAlives: true}}
}

// cacheGet GETs path through the http balancer and returns the status and
// the ruralz Cache-Status member's hit and fwd parameters.
func cacheGet(ctx context.Context, c *http.Client, r *Run, path string) (status int, hit bool, fwd string, err error) {
	q, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+r.HTTP.Addr()+path, nil)
	if err != nil {
		return 0, false, "", err
	}
	resp, err := c.Do(q)
	if err != nil {
		return 0, false, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	if _, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20)); err != nil {
		return 0, false, "", err
	}
	hit, fwd = cacheMember(resp.Header)
	return resp.StatusCode, hit, fwd, nil
}

// cacheMember returns the ruralz member of a response's Cache-Status
// fields (RFC 9211): whether it reports a hit, and its fwd parameter.
func cacheMember(h http.Header) (hit bool, fwd string) {
	for _, v := range h.Values("Cache-Status") {
		for m := range strings.SplitSeq(v, ",") {
			params := strings.Split(strings.TrimSpace(m), ";")
			if strings.TrimSpace(params[0]) != "ruralz" {
				continue
			}
			for _, p := range params[1:] {
				k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
				switch k {
				case "hit":
					hit = val == "" || val == "?1"
				case "fwd":
					fwd = val
				}
			}
		}
	}
	return hit, fwd
}

// primeEvictable stores ce6EvictURIs URIs before the fill and returns
// those whose second GET was a hit: each then has an entry and a
// generation key.
func primeEvictable(ctx context.Context, t *testing.T, r *Run) []string {
	t.Helper()
	c := ce6Client()
	var primed []string
	for i := range ce6EvictURIs {
		path := PathCached + "/evict-" + strconv.Itoa(i)
		if _, _, _, err := cacheGet(ctx, c, r, path); err != nil {
			t.Logf("chaos: priming %s: %v", path, err)
			continue
		}
		status, hit, _, err := cacheGet(ctx, c, r, path)
		if err == nil && status == http.StatusOK && hit {
			primed = append(primed, path)
		}
	}
	t.Logf("chaos: %d of %d URIs stored and hit before the fill", len(primed), ce6EvictURIs)
	return primed
}

// genEvictions is what checkEvictedGenerations found.
type genEvictions struct {
	// evicted counts the primed URIs whose generation key the fill
	// evicted; hits, those of them a later GET hit; failed, those whose
	// GET did not get the Upstream's 200.
	evicted, hits, failed int
}

// checkEvictedGenerations GETs, once the hot-entry layer has let go, each
// primed URI whose generation key the cache State Store no longer holds:
// an evicted generation key only turns its URI's entries into misses,
// never into hits (docs/architecture/09-traffic-management-and-resilience.md
// "Response caching").
func checkEvictedGenerations(ctx context.Context, t *testing.T, r *Run, srv *redisserver.Server, primed []string) genEvictions {
	t.Helper()
	select {
	case <-ctx.Done():
		return genEvictions{}
	case <-time.After(ce6HotEntry):
	}
	c := ce6Client()
	var g genEvictions
	for _, path := range primed {
		u := cache.URIDigest("http", "127.0.0.1", r.HTTP.Addr(), path, "")
		v, err := srv.Do(ctx, "EXISTS", string(keys.AppendCacheGeneration(nil, u)))
		if err != nil {
			t.Fatalf("chaos: EXISTS of %s's generation key: %v", path, err)
		}
		if v.Int != 0 {
			continue
		}
		g.evicted++
		status, hit, fwd, err := cacheGet(ctx, c, r, path)
		switch {
		case err != nil || status != http.StatusOK:
			g.failed++
			t.Logf("chaos: GET %s after its generation key was evicted: %d, %v", path, status, err)
		case hit:
			g.hits++
			t.Logf("chaos: GET %s hit after its generation key was evicted", path)
		case fwd != "uri-miss":
			t.Logf("chaos: GET %s after its generation key was evicted: fwd=%s", path, fwd)
		}
	}
	return g
}

// Stale checks: ce6Checkers clients, each cycling through ce6CheckURIs
// URIs of its own, and the wait between an invalidating POST and the read
// that checks it, past the 1 s a hot-entry layer may serve the old
// generation (target) and the post-commit queue's delay, so the read
// follows the bump (docs/architecture/09-traffic-management-and-resilience.md
// "Response caching").
const (
	ce6Checkers           = 8
	ce6CheckURIs          = 4
	ce6InvalidationSettle = 2 * time.Second
)

// staleChecker repeatedly stores, invalidates and reads URIs through the
// http balancer: a read once an invalidation has landed must never echo
// a fetch made before it, an invalidated entry served.
type staleChecker struct {
	mu            sync.Mutex
	stale, checks int64
	cancel        context.CancelFunc
	wg            sync.WaitGroup
}

func startStaleChecker(ctx context.Context, r *Run) *staleChecker {
	ctx, cancel := context.WithCancel(ctx)
	sc := &staleChecker{cancel: cancel}
	client := &http.Client{Timeout: 5 * time.Second}
	var fetches atomic.Int64
	fetch := func(method, uri string, n int64) (int64, error) {
		q, err := http.NewRequestWithContext(ctx, method, uri, nil)
		if err != nil {
			return 0, err
		}
		q.Header.Set(ce6FetchHeader, strconv.FormatInt(n, 10))
		resp, err := client.Do(q)
		if err != nil {
			return 0, err
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		if err != nil || resp.StatusCode != http.StatusOK {
			return 0, fmt.Errorf("status %d: %w", resp.StatusCode, err)
		}
		var e mockup.Echo
		if err := json.Unmarshal(b, &e); err != nil {
			return 0, err
		}
		vs := e.Header[ce6FetchHeader]
		if len(vs) == 0 {
			return 0, fmt.Errorf("no %s echoed", ce6FetchHeader)
		}
		return strconv.ParseInt(vs[0], 10, 64)
	}
	for c := range ce6Checkers {
		sc.wg.Go(func() {
			base := "http://" + r.HTTP.Addr() + PathCached + "/check-" + strconv.Itoa(c) + "-"
			for i := 0; ctx.Err() == nil; i++ {
				uri := base + strconv.Itoa(i%ce6CheckURIs)
				if _, err := fetch(http.MethodGet, uri, fetches.Add(1)); err != nil {
					continue
				}
				invalidated := fetches.Add(1)
				if _, err := fetch(http.MethodPost, uri, invalidated); err != nil {
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(ce6InvalidationSettle):
				}
				got, err := fetch(http.MethodGet, uri, fetches.Add(1))
				if err != nil {
					continue
				}
				sc.mu.Lock()
				sc.checks++
				if got < invalidated {
					sc.stale++
				}
				sc.mu.Unlock()
			}
		})
	}
	return sc
}

func (sc *staleChecker) stop() {
	sc.cancel()
	sc.wg.Wait()
}

func (sc *staleChecker) result() (stale, checks int64) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.stale, sc.checks
}
