// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwks

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// harness is a Manager on a fake clock fetching from a mock IdP over TLS
// on loopback, which the guard admits through RURALZ_FETCH_ALLOW.
type harness struct {
	t      *testing.T
	ca     *pki.CA
	clk    *clocktest.Fake
	idp    *mockidp.Server
	m      *Manager
	status *status
	src    Source
}

// newHarness starts a mock IdP with cfg (TLS added) and a Manager for it.
func newHarness(t *testing.T, cfg mockidp.Config) *harness {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TLS, err = leaf.ServerTLS(nil); err != nil {
		t.Fatal(err)
	}
	idp, err := mockidp.Start(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = idp.Close() })
	h := &harness{t: t, ca: ca, clk: clocktest.New(t0), idp: idp, status: &status{}}
	h.m = h.manager(true)
	h.src = Source{Issuer: idp.Issuer(), URL: idp.JWKSURL()}
	return h
}

// manager returns a Manager trusting the harness CA; allow admits
// loopback through the guard.
func (h *harness) manager(allow bool) *Manager {
	h.t.Helper()
	var guard *egress.Guard
	if allow {
		g, err := egress.ParseAllow("127.0.0.1")
		if err != nil {
			h.t.Fatal(err)
		}
		guard = g
	}
	return New(Options{Guard: guard, TLS: tlsconf.Client(h.ca.Pool()), Clock: h.clk, Status: h.status})
}

// run runs m until the test ends.
func (h *harness) run(m *Manager) {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()
	h.t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			h.t.Errorf("Run = %v", err)
		}
	})
}

// fetches returns the number of JWKS requests the IdP answered.
func (h *harness) fetches() int64 { return h.idp.Stats().JWKSFetches }

// holder is the jwks_stale holder of src.
func holder(src Source) string { return "jwks " + src.Issuer + " " + src.URL }

// acquire acquires src and waits for its prefetch.
func (h *harness) acquire(src Source) *Handle {
	h.t.Helper()
	hs := h.m.Acquire([]Source{src})
	h.m.Wait(h.t.Context(), h.m.NewRound(), hs)
	return hs[0]
}

// waitTimer waits until the Run loop has armed its timer.
func (h *harness) waitTimer() {
	h.t.Helper()
	waitFor(h.t, "an armed timer", func() bool { return h.clk.Pending() == 1 })
}

// TestPrefetchFetchesAndShares checks that a new Source is fetched at
// once and Wait returns when the fetch completes, and that another
// acquisition of the Source (the next snapshot) reuses its keys without
// a fetch.
func TestPrefetchFetchesAndShares(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.run(h.m)
	first := h.acquire(h.src)
	ks := first.Keys()
	if ks == nil || ks.Len() != 2 {
		t.Fatalf("Keys after prefetch = %v", ks)
	}
	rs, _ := h.idp.Key(mockidp.RS256)
	if _, ok := ks.Lookup(rs.ID, FamilyRSA); !ok {
		t.Fatal("the IdP's RS256 key is missing")
	}
	if !ks.Fetched().Equal(t0) || !ks.Expires().Equal(t0.Add(time.Hour)) {
		t.Fatalf("Fetched, Expires = %v, %v; mock IdP sends max-age=3600", ks.Fetched(), ks.Expires())
	}
	second := h.acquire(h.src)
	if second.Keys() != ks || h.fetches() != 1 {
		t.Fatalf("second acquisition: same keys %v, fetches %d", second.Keys() == ks, h.fetches())
	}
	if first.Source() != h.src {
		t.Fatalf("Source = %v", first.Source())
	}
}

// TestPrefetchFailureActivates checks "activate even if a fetch fails":
// Wait returns once the failed fetch completes, the Source has no keys,
// jwks_stale is raised for it, and the fetch is retried after
// RetryInterval.
func TestPrefetchFailureActivates(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusInternalServerError})
	h.run(h.m)
	hd := h.acquire(h.src)
	if hd.Keys() != nil {
		t.Fatal("keys from a failed fetch")
	}
	if !h.status.stale(holder(h.src)) {
		t.Fatal("jwks_stale not raised")
	}
	h.waitTimer()
	h.clk.Advance(RetryInterval - time.Second)
	if h.fetches() != 1 {
		t.Fatalf("fetches before RetryInterval = %d", h.fetches())
	}
	h.idp.SetJWKSOverride(mockidp.Override{})
	h.clk.Advance(time.Second)
	waitFor(t, "the retry", func() bool { return hd.Keys() != nil })
	if h.fetches() != 2 {
		t.Fatalf("fetches = %d, want 2", h.fetches())
	}
	waitFor(t, "jwks_stale cleared", func() bool { return !h.status.stale(holder(h.src)) })
}

// TestRefreshAtHalfLife checks that a key set is refetched at half its
// lifetime and the new keys replace the old.
func TestRefreshAtHalfLife(t *testing.T) {
	h := newHarness(t, mockidp.Config{CacheControl: "max-age=600"})
	h.run(h.m)
	hd := h.acquire(h.src)
	old := hd.Keys()
	if old == nil || !old.Expires().Equal(t0.Add(10*time.Minute)) {
		t.Fatalf("Keys = %v", old)
	}
	k, err := h.idp.Rotate(mockidp.RS256, false)
	if err != nil {
		t.Fatal(err)
	}
	h.waitTimer()
	h.clk.Advance(5*time.Minute - time.Second)
	if hd.Keys() != old || h.fetches() != 1 {
		t.Fatal("refreshed before half-life")
	}
	h.clk.Advance(time.Second)
	waitFor(t, "the refresh", func() bool { return hd.Keys() != old })
	if _, ok := hd.Keys().Lookup(k.ID, FamilyRSA); !ok {
		t.Fatal("the rotated key is missing after the refresh")
	}
}

// TestFailedRefreshServesUntilExpiry checks "after failed refreshes,
// serve degraded until expiry": the last keys stay while jwks_stale is
// raised, they expire at their lifetime, and a successful retry replaces
// them and clears jwks_stale.
func TestFailedRefreshServesUntilExpiry(t *testing.T) {
	h := newHarness(t, mockidp.Config{CacheControl: "max-age=600"})
	h.run(h.m)
	hd := h.acquire(h.src)
	old := hd.Keys()
	h.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusBadGateway})
	h.waitTimer()
	h.clk.Advance(5 * time.Minute)
	waitFor(t, "the failed refresh", func() bool { return h.status.stale(holder(h.src)) })
	if hd.Keys() != old {
		t.Fatal("a failed refresh replaced the keys")
	}
	if old.Expired(t0.Add(10*time.Minute-time.Second)) || !old.Expired(t0.Add(10*time.Minute)) {
		t.Fatal("the served keys do not expire at their lifetime")
	}
	h.idp.SetJWKSOverride(mockidp.Override{})
	h.waitTimer()
	h.clk.Advance(RetryInterval)
	waitFor(t, "the retry", func() bool { return hd.Keys() != old })
	waitFor(t, "jwks_stale cleared", func() bool { return !h.status.stale(holder(h.src)) })
}

// TestRetryAfterDefersFetches checks "honoring Retry-After": a failed
// fetch whose response names a later Retry-After is retried no earlier,
// and nudges before it fetch nothing.
func TestRetryAfterDefersFetches(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"120"}}})
	h.run(h.m)
	hd := h.acquire(h.src)
	h.waitTimer()
	h.clk.Advance(RefreshInterval + time.Second)
	hd.Nudge(h.clk.Now())
	h.waitTimer()
	if h.fetches() != 1 {
		t.Fatalf("fetches before Retry-After = %d", h.fetches())
	}
	h.idp.SetJWKSOverride(mockidp.Override{})
	h.clk.Advance(120*time.Second - RefreshInterval - time.Second)
	waitFor(t, "the deferred fetch", func() bool { return hd.Keys() != nil })
}

// TestNudgeRateLimit checks that an unknown kid triggers "at most one
// refresh per issuer per 30 s": a nudge within RefreshInterval of the
// last fetch fetches nothing, a later one fetches at once, and a nudge
// right after it fetches nothing.
func TestNudgeRateLimit(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.run(h.m)
	hd := h.acquire(h.src)
	h.waitTimer()
	h.clk.Advance(RefreshInterval - time.Second)
	hd.Nudge(h.clk.Now())
	h.waitTimer()
	if h.fetches() != 1 {
		t.Fatalf("fetches after an early nudge = %d", h.fetches())
	}
	h.clk.Advance(time.Second)
	old := hd.Keys()
	hd.Nudge(h.clk.Now())
	waitFor(t, "the nudged fetch", func() bool { return hd.Keys() != old })
	hd.Nudge(h.clk.Now())
	h.waitTimer()
	if h.fetches() != 2 {
		t.Fatalf("fetches = %d, want 2", h.fetches())
	}
}

// TestReleaseRemovesSource checks that a Source lives while any Handle
// holds it, and that releasing the last one drops its keys, clears its
// jwks_stale and makes the next acquisition fetch again.
func TestReleaseRemovesSource(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.run(h.m)
	a := h.acquire(h.src)
	b := h.acquire(h.src)
	a.Release()
	a.Release() // idempotent: b still holds the Source
	if c := h.acquire(h.src); c.Keys() == nil || h.fetches() != 1 {
		t.Fatal("the Source was dropped while a Handle held it")
	} else {
		c.Release()
	}
	h.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusInternalServerError})
	other := Source{Issuer: "https://other.example", URL: h.src.URL + "?other"}
	o := h.acquire(other)
	if !h.status.stale(holder(other)) {
		t.Fatal("jwks_stale not raised for the failing Source")
	}
	o.Release()
	if h.status.stale(holder(other)) {
		t.Fatal("releasing the last Handle kept jwks_stale")
	}
	b.Release()
	h.idp.SetJWKSOverride(mockidp.Override{})
	n := h.fetches()
	if d := h.acquire(h.src); d.Keys() == nil || h.fetches() != n+1 {
		t.Fatal("a released Source was not fetched again")
	}
}

// TestWaitEndsAtRoundDeadline checks that a compile's waits end
// PrefetchTimeout after its round began, and at once for a later wait of
// an ended round, so a compile never waits longer whatever its issuers
// do.
func TestWaitEndsAtRoundDeadline(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	// No Run: nothing is fetched, so only the deadline ends the wait.
	r := h.m.NewRound()
	hs := h.m.Acquire([]Source{h.src})
	done := make(chan struct{})
	go func() {
		h.m.Wait(context.Background(), r, hs)
		close(done)
	}()
	h.waitTimer()
	h.clk.Advance(PrefetchTimeout)
	<-done
	other := h.m.Acquire([]Source{{Issuer: "https://other.example", URL: h.src.URL + "?other"}})
	h.m.Wait(context.Background(), r, other) // returns at once
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	h.m.Wait(ctx, h.m.NewRound(), other) // returns at once
	if h.clk.Pending() != 0 {
		t.Fatalf("Wait left %d timers", h.clk.Pending())
	}
}

// TestGuardRefusesLoopback checks that every fetch goes through the egress
// guard: without RURALZ_FETCH_ALLOW, a loopback jwksUrl is refused, the
// Source has no keys and jwks_stale is raised.
func TestGuardRefusesLoopback(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	m := h.manager(false)
	h.run(m)
	hs := m.Acquire([]Source{h.src})
	m.Wait(t.Context(), m.NewRound(), hs)
	if hs[0].Keys() != nil || h.fetches() != 0 {
		t.Fatalf("keys %v, fetches %d through a denied destination", hs[0].Keys(), h.fetches())
	}
	if !h.status.stale(holder(h.src)) {
		t.Fatal("jwks_stale not raised")
	}
}

// TestFetchRefusals checks that a jwksUrl that is not https, a document
// over MaxDocumentBytes and a document that is not a JWK Set are failed
// fetches.
func TestFetchRefusals(t *testing.T) {
	tests := []struct {
		name string
		over mockidp.Override
		url  func(string) string
	}{
		{"http URL", mockidp.Override{}, func(u string) string { return "http" + strings.TrimPrefix(u, "https") }},
		{"over 1 MiB", mockidp.Override{Body: bytes.Repeat([]byte(" "), MaxDocumentBytes+1)}, nil},
		{"not a JWK Set", mockidp.Override{Body: []byte(`{"keys": "no"}`)}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, mockidp.Config{})
			h.idp.SetJWKSOverride(tt.over)
			h.run(h.m)
			src := h.src
			if tt.url != nil {
				src.URL = tt.url(src.URL)
			}
			if hd := h.acquire(src); hd.Keys() != nil {
				t.Fatal("keys from a refused fetch")
			}
			if !h.status.stale(holder(src)) {
				t.Fatal("jwks_stale not raised")
			}
		})
	}
}

// tlsServer starts an https server on loopback with a certificate of the
// harness CA, until the test ends.
func (h *harness) tlsServer(handler http.Handler) *httptest.Server {
	h.t.Helper()
	srv := httptest.NewUnstartedServer(handler)
	leaf, err := h.ca.Server("127.0.0.1")
	if err != nil {
		h.t.Fatal(err)
	}
	if srv.TLS, err = leaf.ServerTLS(nil); err != nil {
		h.t.Fatal(err)
	}
	srv.StartTLS()
	h.t.Cleanup(srv.Close)
	return srv
}

// TestFetchRedirects checks that a fetch follows at most MaxRedirects
// https redirects: a key set three hops away is fetched, one four hops
// away is a failed fetch.
func TestFetchRedirects(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	jwksDoc := h.idp.JWKS()
	// /r<n> redirects to /r<n-1>; /r0 serves the key set.
	next := map[string]string{}
	for i := 1; i <= MaxRedirects+1; i++ {
		next[fmt.Sprint("/r", i)] = fmt.Sprint("/r", i-1)
	}
	srv := h.tlsServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if to, ok := next[r.URL.Path]; ok {
			http.Redirect(w, r, to, http.StatusFound)
			return
		}
		_, _ = w.Write(jwksDoc)
	}))
	h.run(h.m)
	three := Source{Issuer: "https://three.example", URL: fmt.Sprint(srv.URL, "/r", MaxRedirects)}
	four := Source{Issuer: "https://four.example", URL: fmt.Sprint(srv.URL, "/r", MaxRedirects+1)}
	if hd := h.acquire(three); hd.Keys() == nil {
		t.Fatalf("no keys %d redirects away", MaxRedirects)
	}
	if hd := h.acquire(four); hd.Keys() != nil || !h.status.stale(holder(four)) {
		t.Fatalf("keys fetched %d redirects away", MaxRedirects+1)
	}
}

// TestFailureRedactsURLPassword checks Secrets rule 2 for the one place a
// jwksUrl appears outside the Bundle: a password in the URL never reaches
// the failure log or the jwks_stale holder.
func TestFailureRedactsURLPassword(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	h.idp.SetJWKSOverride(mockidp.Override{Status: http.StatusInternalServerError})
	var logs lockedBuffer
	guard, err := egress.ParseAllow("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	m := New(Options{
		Guard: guard, TLS: tlsconf.Client(h.ca.Pool()), Clock: h.clk, Status: h.status,
		Logger: slog.New(slog.NewTextHandler(&logs, nil)),
	})
	h.run(m)
	src := Source{Issuer: h.src.Issuer, URL: strings.Replace(h.src.URL, "https://", "https://user:hunter2@", 1)}
	hs := m.Acquire([]Source{src})
	m.Wait(t.Context(), m.NewRound(), hs)
	if h.status.staleCount() != 1 {
		t.Fatal("jwks_stale not raised")
	}
	for _, hd := range h.status.holders(catalog.ReasonJWKSStale) {
		if strings.Contains(hd, "hunter2") || !strings.Contains(hd, "user:xxxxx@") {
			t.Fatalf("holder %q", hd)
		}
	}
	if out := logs.String(); strings.Contains(out, "hunter2") || !strings.Contains(out, "jwks fetch failed") {
		t.Fatalf("log %q", out)
	}
}

// lockedBuffer is a bytes.Buffer safe for concurrent writes.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestConcurrentFetchBound checks "16 at once": with more due Sources
// than MaxConcurrentFetches, no more fetches run at once, and the rest
// start as fetches complete.
func TestConcurrentFetchBound(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	jwksDoc := h.idp.JWKS()
	release := make(chan struct{})
	var inflight, peak, total atomic.Int64
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	srv := h.tlsServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := inflight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		total.Add(1)
		<-release
		inflight.Add(-1)
		_, _ = w.Write(jwksDoc)
	}))
	t.Cleanup(unblock)
	h.run(h.m)
	const n = MaxConcurrentFetches + 4
	srcs := make([]Source, n)
	for i := range srcs {
		srcs[i] = Source{Issuer: fmt.Sprint("https://issuer-", i), URL: fmt.Sprint(srv.URL, "/jwks?n=", i)}
	}
	hs := h.m.Acquire(srcs)
	waitFor(t, "a full set of fetches", func() bool { return inflight.Load() == MaxConcurrentFetches })
	if total.Load() != MaxConcurrentFetches {
		t.Fatalf("%d fetches started with %d slots", total.Load(), MaxConcurrentFetches)
	}
	unblock()
	waitFor(t, "every Source", func() bool {
		for _, hd := range hs {
			if hd.Keys() == nil {
				return false
			}
		}
		return true
	})
	if peak.Load() > MaxConcurrentFetches || total.Load() != n {
		t.Fatalf("peak %d, total %d", peak.Load(), total.Load())
	}
}

// TestRunLifecycle checks the Component contract: Name, one Run at a
// time, and Run returning nil after canceling a fetch in flight without
// recording it as a failure.
func TestRunLifecycle(t *testing.T) {
	h := newHarness(t, mockidp.Config{})
	if h.m.Name() != "jwks" {
		t.Fatalf("Name = %q", h.m.Name())
	}
	h.idp.SetJWKSOverride(mockidp.Override{Delay: time.Minute})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.m.Run(ctx) }()
	waitFor(t, "Run to start", func() bool { return h.m.running.Load() })
	if err := h.m.Run(context.Background()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run = %v, want ErrRunning", err)
	}
	h.m.Acquire([]Source{h.src})
	waitFor(t, "the fetch to reach the IdP", func() bool { return h.fetches() == 1 })
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v", err)
	}
	if h.status.staleCount() != 0 {
		t.Fatal("a fetch canceled by shutdown raised jwks_stale")
	}
}
