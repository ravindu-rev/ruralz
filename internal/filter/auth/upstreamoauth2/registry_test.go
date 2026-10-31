// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// result is one Handle outcome observed on another goroutine.
type result struct {
	r    filter.Result
	auth []string
}

// handleAsync runs handle on a new goroutine; the result arrives on the
// returned channel.
func handleAsync(ctx context.Context, f filter.Filter) <-chan result {
	ch := make(chan result, 1)
	go func() {
		r, auth := handle(ctx, f)
		ch <- result{r, auth}
	}()
	return ch
}

// await returns the result of handleAsync, failing the test when it does
// not arrive.
func await(t *testing.T, ch <-chan result, what string) result {
	t.Helper()
	select {
	case res := <-ch:
		return res
	case <-time.After(waitLimit):
		t.Fatalf("%s: Handle did not return", what)
		return result{}
	}
}

// TestSingleFlight checks that requests missing together share one fetch
// per Policy and Node (Security and identity, "Upstream
// authentication"), and all of them send its token.
func TestSingleFlight(t *testing.T) {
	g := newGate(t)
	h := running(t, held(g, issue(300)))
	b := h.build(testPolicy, h.config())
	const n = 32
	var wg sync.WaitGroup
	results := make(chan result, n)
	for range n {
		wg.Go(func() {
			r, auth := handle(t.Context(), b)
			results <- result{r, auth}
		})
	}
	h.ts.waitArrival(t, 1)
	g.open()
	wg.Wait()
	close(results)
	for res := range results {
		wantBearer(t, res.r, res.auth, "tok-1")
	}
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests, want 1", c)
	}
}

// TestRefreshAtTwoThirds checks that a token is refreshed in the
// background at two thirds of its lifetime (target), counted from the
// request that obtained it, and that requests then send the new token.
func TestRefreshAtTwoThirds(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(200 * time.Second)) {
		t.Fatalf("refresh at %v, want two thirds of 300 s after the fetch", at.Sub(testStart()))
	}
	h.advanceArmed(200 * time.Second)
	h.waitFetches(2)
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-2")
	if at, _ := entryState(b.e); !at.Equal(testStart().Add(400 * time.Second)) {
		t.Errorf("next refresh at %v, want 400 s", at.Sub(testStart()))
	}
	if c := h.ts.count(); c != 2 {
		t.Errorf("token server got %d requests, want 2", c)
	}
}

// TestRequestsDoNotWaitForRefresh checks the non-blocking request path:
// while a refresh is in flight, requests send the token still held at
// once.
func TestRequestsDoNotWaitForRefresh(t *testing.T) {
	g := newGate(t)
	h := running(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			issue(300)(n, w, r)
			return
		}
		held(g, issue(300))(n, w, r)
	})
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	h.advanceArmed(200 * time.Second)
	h.ts.waitArrival(t, 2)
	res := await(t, handleAsync(t.Context(), b), "request during a refresh")
	wantBearer(t, res.r, res.auth, "tok-1")
	g.open()
	h.waitFetches(2)
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-2")
}

// TestFailedRefreshKeepsToken checks a failed refresh: the held token keeps
// serving until it expires, the failure is counted and logged once, the
// retry comes at two thirds of the remaining lifetime, and a successful
// retry replaces the token.
func TestFailedRefreshKeepsToken(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	h.ts.setResponder(fail(http.StatusInternalServerError, "server_error"))
	h.advanceArmed(200 * time.Second)
	h.waitFetches(2)
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	retry := testStart().Add(200*time.Second + 100*time.Second - 100*time.Second/3)
	at, lastErr := entryState(b.e)
	var se *statusError
	if !at.Equal(retry) || !errors.As(lastErr, &se) || se.code != "server_error" {
		t.Fatalf("after the failed refresh: retry at %v, failure %v; want %v and status 500", at.Sub(testStart()), lastErr, retry.Sub(testStart()))
	}
	if n := b.failures.n.Load(); n != 1 {
		t.Errorf("refresh failures = %d, want 1", n)
	}
	if got := b.age.v.Load(); got != 200 {
		t.Errorf("token age after the failed refresh = %d s, want 200", got)
	}
	logs := h.logs.Bytes()
	if bytes.Count(logs, []byte(msgFetchFailed)) != 1 || !bytes.Contains(logs, []byte(testPolicy)) {
		t.Errorf("logs = %s, want one warning naming the Policy", logs)
	}
	h.ts.setResponder(issue(300))
	h.advanceArmed(retry.Sub(h.clk.Now()))
	h.waitFetches(3)
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-3")
}

// TestExpiredTokenIsNotSent checks that a token is never sent after it
// expires: a request then joins the refresh in flight instead of starting
// another fetch, and sends its token.
func TestExpiredTokenIsNotSent(t *testing.T) {
	g := newGate(t)
	h := running(t, func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 1 {
			issue(3)(n, w, r)
			return
		}
		held(g, issue(300))(n, w, r)
	})
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	h.advanceArmed(2 * time.Second)
	h.ts.waitArrival(t, 2)
	h.clk.Advance(time.Second)
	ch := handleAsync(t.Context(), b)
	g.open()
	res := await(t, ch, "request after expiry")
	wantBearer(t, res.r, res.auth, "tok-2")
	if c := h.ts.count(); c != 2 {
		t.Errorf("token server got %d requests, want 2", c)
	}
}

// TestTokenExpiredOnArrival checks that a token whose lifetime ended
// before its response arrived is never sent: the fetch fails with
// RZ-AUTH-020 and stores nothing.
func TestTokenExpiredOnArrival(t *testing.T) {
	g := newGate(t)
	h := running(t, held(g, issue(1)))
	b := h.build(testPolicy, h.config())
	ch := handleAsync(t.Context(), b)
	h.ts.waitArrival(t, 1)
	h.clk.Advance(time.Second)
	g.open()
	res := await(t, ch, "request whose token expired in flight")
	wantUndecided(t, res.r, errExpired)
	if tok := b.e.tok.Load(); tok != nil {
		t.Error("an expired token was stored")
	}
}

// TestHoldoffAfterFailure checks that a failing token endpoint gets at
// most one fetch per MinFetchInterval: a miss within it of a failed fetch
// fails at once with RZ-AUTH-020, naming the failure, and the first miss
// after it fetches again.
func TestHoldoffAfterFailure(t *testing.T) {
	h := running(t, fail(http.StatusUnauthorized, "invalid_client"))
	b := h.build(testPolicy, h.config())
	var se *statusError
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, nil)
	if !errors.As(r.Err, &se) || se.status != http.StatusUnauthorized {
		t.Fatalf("first miss = %v, want the 401", r.Err)
	}
	h.clk.Advance(MinFetchInterval - time.Nanosecond)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, errHoldoff)
	if !errors.As(r.Err, &se) || h.ts.count() != 1 {
		t.Fatalf("miss in the hold-off = %v after %d requests; want the 401 again without a request", r.Err, h.ts.count())
	}
	h.clk.Advance(time.Nanosecond)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, nil)
	if errors.Is(r.Err, errHoldoff) || h.ts.count() != 2 {
		t.Fatalf("miss after the hold-off = %v after %d requests; want a new fetch", r.Err, h.ts.count())
	}
	// A fetch counts its failure after its waiters are released.
	h.waitFetches(2)
	if n := b.failures.n.Load(); n != 2 {
		t.Errorf("refresh failures = %d, want 2", n)
	}
}

// TestWaiterContextEnds checks that a request stops waiting when its own
// context ends, with RZ-AUTH-020, while the fetch it joined goes on and
// serves later requests.
func TestWaiterContextEnds(t *testing.T) {
	g := newGate(t)
	h := running(t, held(g, issue(300)))
	b := h.build(testPolicy, h.config())
	ctx, cancel := context.WithCancel(t.Context())
	ch := handleAsync(ctx, b)
	h.ts.waitArrival(t, 1)
	cancel()
	res := await(t, ch, "canceled waiter")
	wantUndecided(t, res.r, errWait)
	if !errors.Is(res.r.Err, context.Canceled) {
		t.Errorf("error = %v, want the context's", res.r.Err)
	}
	g.open()
	h.waitFetches(1)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests, want 1", c)
	}
}

// TestScheduleTimes checks the schedule rules: a refresh at two thirds of
// the lifetime and a retry at two thirds of what is left, both at least
// MinFetchInterval after the fetch started, and no retry once the held
// token expires first.
func TestScheduleTimes(t *testing.T) {
	t0 := testStart()
	tok := func(expires time.Duration) *token { return &token{obtained: t0, expires: t0.Add(expires)} }
	for _, tc := range []struct {
		name          string
		start, expire time.Duration
		want          time.Duration
	}{
		{"two thirds", 0, 300 * time.Second, 200 * time.Second},
		{"floor", 0, time.Second, MinFetchInterval},
		{"no lifetime", 0, 0, MinFetchInterval},
		{"later start", 10 * time.Second, 40 * time.Second, 30 * time.Second},
	} {
		if got := refreshTime(t0.Add(tc.start), t0.Add(tc.expire)); !got.Equal(t0.Add(tc.want)) {
			t.Errorf("refreshTime %s = %v, want %v", tc.name, got.Sub(t0), tc.want)
		}
	}
	for _, tc := range []struct {
		name       string
		now, start time.Duration
		cur        *token
		want       time.Duration
		none       bool
	}{
		{"no token", 200 * time.Second, 200 * time.Second, nil, 0, true},
		{"expired", 300 * time.Second, 299 * time.Second, tok(300 * time.Second), 0, true},
		{"two thirds left", 200 * time.Second, 200 * time.Second, tok(300 * time.Second), 200*time.Second + 100*time.Second - 100*time.Second/3, false},
		{"floor", 290 * time.Second, 290 * time.Second, tok(291200 * time.Millisecond), 291 * time.Second, false},
		{"floor past expiry", 299500 * time.Millisecond, 299500 * time.Millisecond, tok(300 * time.Second), 0, true},
		{"slow fetch", 250 * time.Second, 248 * time.Second, tok(300 * time.Second), 250*time.Second + 50*time.Second - 50*time.Second/3, false},
	} {
		got := retryTime(t0.Add(tc.now), t0.Add(tc.start), tc.cur)
		switch {
		case tc.none && !got.IsZero():
			t.Errorf("retryTime %s = %v, want none", tc.name, got.Sub(t0))
		case !tc.none && !got.Equal(t0.Add(tc.want)):
			t.Errorf("retryTime %s = %v, want %v", tc.name, got.Sub(t0), tc.want)
		}
	}
}

// TestRunLifecycle checks the component contract: one Run at a time; on
// cancel Run cancels its running fetch and returns only after the fetch
// goroutine exited, the waiting request fails with RZ-AUTH-020, a later
// miss fails at once without a request, and a new Run serves again.
func TestRunLifecycle(t *testing.T) {
	g := newGate(t)
	h := running(t, held(g, issue(300)))
	b := h.build(testPolicy, h.config())
	ch := handleAsync(t.Context(), b)
	// The fetch at the server proves the first Run active. The second
	// gets an ended context, so a Run that wrongly started returns.
	h.ts.waitArrival(t, 1)
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	if err := h.reg.Run(ended); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run = %v, want ErrRunning", err)
	}
	if err := h.stop(); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if n := h.fetched.Load(); n != 1 {
		t.Fatalf("Run returned with %d finished fetches, want 1", n)
	}
	res := await(t, ch, "waiter at shutdown")
	wantUndecided(t, res.r, context.Canceled)
	if n := b.failures.n.Load(); n != 0 {
		t.Errorf("the fetch canceled by shutdown counted %d failures, want 0", n)
	}
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, errStopped)
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests after Run returned, want 1", c)
	}
	h.ts.setResponder(issue(300))
	h.start()
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-2")
	var c filter.Component = h.reg
	if c.Name() != ComponentName {
		t.Errorf("Name = %q", c.Name())
	}
}

// TestMissBeforeRun checks that a miss before Run starts leaves its fetch
// pending, Run starts it, and its token then serves without another fetch.
func TestMissBeforeRun(t *testing.T) {
	h := newHarness(t, issue(300))
	b := h.build(testPolicy, h.config())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, _ := handle(ctx, b)
	wantUndecided(t, r, errWait)
	if c := h.ts.count(); c != 0 {
		t.Fatalf("token server got %d requests before Run", c)
	}
	h.start()
	h.waitFetches(1)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests, want 1", c)
	}
}

// TestEntrySharing checks the entry key: Filters of the same Policy and
// fetch parameters, such as those of consecutive snapshots, share one
// token; changed parameters get another; the entry lives until its last
// Filter closes, and a Policy built again afterwards fetches anew. Close
// is idempotent.
func TestEntrySharing(t *testing.T) {
	h := running(t, issue(300))
	b1 := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b1)
	wantBearer(t, r, auth, "tok-1")
	b2 := h.build(testPolicy, h.config())
	if b2.e != b1.e {
		t.Fatal("equal Policy and config got a second entry")
	}
	r, auth = handle(t.Context(), b2)
	wantBearer(t, r, auth, "tok-1")
	b3 := h.build(testPolicy, h.config(func(c *v1alpha1.AuthUpstreamOAuth2Config) { c.Scopes = []string{"orders.read"} }))
	if b3.e == b1.e {
		t.Fatal("changed scopes share the entry")
	}
	r, auth = handle(t.Context(), b3)
	wantBearer(t, r, auth, "tok-2")
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}
	r, auth = handle(t.Context(), b2)
	wantBearer(t, r, auth, "tok-1")
	_ = b2.Close()
	b1.e.mu.Lock()
	closed := b1.e.closed
	b1.e.mu.Unlock()
	h.reg.mu.Lock()
	_, kept := h.reg.entries[b1.e.key]
	h.reg.mu.Unlock()
	if kept || !closed {
		t.Fatal("the last Close left the entry in the registry")
	}
	b4 := h.build(testPolicy, h.config())
	r, auth = handle(t.Context(), b4)
	wantBearer(t, r, auth, "tok-3")
	if c := h.ts.count(); c != 3 {
		t.Errorf("token server got %d requests, want 3", c)
	}
}

// TestEntryPerPolicy checks that two Policies with equal configs hold a
// token each: single flight is per Policy.
func TestEntryPerPolicy(t *testing.T) {
	h := running(t, issue(300))
	a := h.build("orders-token", h.config())
	b := h.build("billing-token", h.config())
	r, auth := handle(t.Context(), a)
	wantBearer(t, r, auth, "tok-1")
	r, auth = handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-2")
}

// TestCloseCancelsFetch checks that the last Close cancels a fetch in
// flight and unschedules the entry, so a removed Policy stops contacting
// its token endpoint.
func TestCloseCancelsFetch(t *testing.T) {
	g := newGate(t)
	canceled := make(chan struct{})
	h := running(t, func(n int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-g.ch:
			issue(300)(n, w, r)
		case <-r.Context().Done():
			close(canceled)
		}
	})
	b := h.build(testPolicy, h.config())
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	r, _ := handle(ctx, b)
	wantUndecided(t, r, errWait)
	h.ts.waitArrival(t, 1)
	_ = b.Close()
	select {
	case <-canceled:
	case <-time.After(waitLimit):
		t.Fatal("Close did not cancel the fetch")
	}
	h.waitFetches(1)
	if tok := b.e.tok.Load(); tok != nil {
		t.Error("a closed entry stored a token")
	}
	if n := len(h.reg.list()); n != 0 {
		t.Errorf("registry holds %d entries after the last Close", n)
	}
	if n := b.failures.n.Load(); n != 0 {
		t.Errorf("a canceled fetch of a closed entry counted %d failures", n)
	}
}

// TestSecretRotation checks that each fetch reads the clientSecret from the
// secret table, so a rotated value is sent from the next fetch on.
func TestSecretRotation(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	h.secrets.set(secret.RefOf(testRef()), "rotated secret")
	h.advanceArmed(200 * time.Second)
	h.waitFetches(2)
	if got := h.ts.request(t, 1).pass; got != testSecret {
		t.Errorf("first fetch sent another secret")
	}
	if got := h.ts.request(t, 2).pass; got != "rotated secret" {
		t.Errorf("fetch after the rotation did not send the rotated secret")
	}
}
