// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"bytes"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
)

// Tests of the transport-time signing hook (foundation pack section 8.12
// rule 6): snapshot.LegRun.Sign runs after onUpstreamRequest, as the last
// step before the round trip, on every attempt.

// signature is what the test signer writes: the attempt, its Endpoint, the
// edited header and the body the request carries when it is signed.
func signature(leg *filter.Leg) (string, error) {
	r := leg.Request
	var b []byte
	if r.GetBody != nil {
		rc, err := r.GetBody()
		if err != nil {
			return "", err
		}
		b, err = io.ReadAll(rc)
		_ = rc.Close()
		if err != nil {
			return "", err
		}
	}
	return "attempt=" + strconv.Itoa(leg.Attempt) + ";endpoint=" + leg.Endpoint +
		";edited=" + r.Header.Get("X-Edited") + ";body=" + string(b), nil
}

// signer is an onSign script that signs with signature, failing the test on
// a body it cannot read.
func signer(t *testing.T) func(leg *filter.Leg) *filter.Response {
	return func(leg *filter.Leg) *filter.Response {
		s, err := signature(leg)
		if err != nil {
			t.Errorf("read the body to sign: %v", err)
		}
		leg.Request.Header.Set("X-Signature", s)
		return nil
	}
}

// TestSigningHookRunsLastBeforeRoundTrip checks that the hook runs after
// onUpstreamRequest and before the round trip: the signer sees the header
// and the body an onUpstreamRequest Filter set, and the Upstream receives
// the request exactly as it was signed.
func TestSigningHookRunsLastBeforeRoundTrip(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{
		onRequest: func(leg *filter.Leg) *filter.Response {
			// A header Policy and a body transform of the attempt.
			leg.Request.Header.Set("X-Edited", "yes")
			body := []byte(`{"rewritten":true}`)
			leg.Request.Body = io.NopCloser(bytes.NewReader(body))
			leg.Request.ContentLength = int64(len(body))
			leg.Request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
			return nil
		},
		onSign: signer(t),
	}
	o := outbound(exchange(http.MethodPost, "/orders"), h)
	o.Body = gatedBody(t, `{"client":true}`)
	r, err := f.upstream(s, "orders").Leg(t.Context(), o, Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	want := []string{`begin orders ""`, "request 1 " + srv.addr, "sign 1 " + srv.addr, "headers 1 200", "body 200", "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Fatalf("leg calls = %q, want %q", ev, want)
	}
	got := srv.last()
	if got == nil {
		t.Fatal("the Upstream received no request")
	}
	wantSig := "attempt=1;endpoint=" + srv.addr + `;edited=yes;body={"rewritten":true}`
	if sig := got.Header.Get("X-Signature"); sig != wantSig {
		t.Errorf("X-Signature = %q, want %q (signed after the attempt's edits)", sig, wantSig)
	}
	if under := h.listUnder(); !slices.Contains(under, "sign 1 under 1") {
		t.Errorf("Phase spans = %q, want the hook under its attempt's span", under)
	}
}

// TestSigningHookResignsEveryAttempt checks that a retried leg runs the hook
// again on the retry's own request: each attempt reaches the Upstream with
// its own signature.
func TestSigningHookResignsEveryAttempt(t *testing.T) {
	var (
		mu   sync.Mutex
		sigs []string
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		sigs = append(sigs, r.Header.Get("X-Signature"))
		first := len(sigs) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	h := &hooks{onSign: signer(t)}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/orders"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if r.Status != http.StatusOK || r.Attempts != 2 {
		t.Fatalf("Leg = %d after %d attempts, want 200 after a retry", r.Status, r.Attempts)
	}
	want := []string{"attempt=1;endpoint=" + srv.addr + ";edited=;body=", "attempt=2;endpoint=" + srv.addr + ";edited=;body="}
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(sigs, want) {
		t.Errorf("signatures received = %q, want one per attempt %q", sigs, want)
	}
	if ev := h.list(); !slices.Contains(ev, "sign 1 "+srv.addr) || !slices.Contains(ev, "sign 2 "+srv.addr) {
		t.Errorf("leg calls = %q, want the hook on both attempts", ev)
	}
}

// TestSigningHookFailureEndsLeg checks that a signing failure ends the leg
// with its response before any round trip, with no retry: the request is
// never sent unsigned.
func TestSigningHookFailureEndsLeg(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	gen := &filter.Response{Status: http.StatusUnauthorized, Code: "RZ-AUTH-020"}
	h := &hooks{onSign: func(*filter.Leg) *filter.Response { return gen }}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/orders"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	if r.Generated != gen || r.Status != http.StatusUnauthorized {
		t.Fatalf("Leg = %+v, want the signing failure's response", r)
	}
	if hits := srv.hits.Load(); hits != 0 {
		t.Errorf("the Upstream received %d requests, want none", hits)
	}
	want := []string{`begin orders ""`, "request 1 " + srv.addr, "sign 1 " + srv.addr, "end"}
	if ev := h.list(); !slices.Equal(ev, want) {
		t.Errorf("leg calls = %q, want %q (no response Phase, no retry)", ev, want)
	}
}

// TestSigningHookSignsReplacedRequest checks that the hook signs the request
// an onUpstreamRequest Filter put in place of the attempt's, which is the
// request the transport sends.
func TestSigningHookSignsReplacedRequest(t *testing.T) {
	srv := newServer(t, status(http.StatusOK, "ok"))
	f := newFixture(t)
	s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr)})
	var signed *http.Request
	h := &hooks{
		onRequest: func(leg *filter.Leg) *filter.Response {
			r := leg.Request.Clone(leg.Request.Context())
			r.Header.Set("X-Edited", "replaced")
			leg.Request = r
			return nil
		},
		onSign: func(leg *filter.Leg) *filter.Response {
			signed = leg.Request
			leg.Request.Header.Set("X-Signature", "over "+leg.Request.Header.Get("X-Edited"))
			return nil
		},
	}
	r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodGet, "/orders"), h), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if signed == nil || signed.Context() == nil {
		t.Fatal("the hook got no request")
	}
	got := srv.last()
	if got == nil || got.Header.Get("X-Signature") != "over replaced" || !strings.EqualFold(got.Header.Get("X-Edited"), "replaced") {
		t.Errorf("the Upstream received %v, want the replaced request with its signature", got)
	}
}
