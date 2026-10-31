// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"bytes"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/canary"
)

// TestHandleSetsBearerToken checks onUpstreamRequest: the first request
// fetches, every request continues with "Authorization: Bearer <token>"
// replacing the client's own Authorization, and later requests reuse the
// token without a fetch.
func TestHandleSetsBearerToken(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	for range 3 {
		x := leg()
		r := b.Handle(t.Context(), phase.OnUpstreamRequest, x)
		wantBearer(t, r, x.Msg.Header().Values("Authorization"), "tok-1")
		if a := x.Msg.Header().Get("Accept"); a != "application/json" {
			t.Errorf("Accept = %q, want the request's own", a)
		}
	}
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests, want 1", c)
	}
}

// TestBuildWithoutTelemetry checks that a BuildEnv without metric handles
// or a logger still yields a working Filter: failures are logged to the
// Registry's logger and recorded nowhere, without a panic.
func TestBuildWithoutTelemetry(t *testing.T) {
	h := running(t, fail(http.StatusServiceUnavailable, "temporarily_unavailable"))
	f, err := Factory{Registry: h.reg}.Build(t.Context(), filter.BuildEnv{Config: h.config(), Secrets: h.secrets})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.(filter.Closer).Close() })
	r, _ := handle(t.Context(), f)
	wantUndecided(t, r, nil)
	h.waitFetches(1)
	if !bytes.Contains(h.logs.Bytes(), []byte(msgFetchFailed)) {
		t.Error("the failure did not reach the Registry's logger")
	}
	h.ts.setResponder(issue(300))
	h.clk.Advance(MinFetchInterval)
	r, auth := handle(t.Context(), f)
	wantBearer(t, r, auth, "tok-2")
}

// TestOtherPhasesContinue checks that every Phase but onUpstreamRequest
// continues without touching the message or fetching.
func TestOtherPhasesContinue(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	for p := range phase.Count {
		if p == phase.OnUpstreamRequest {
			continue
		}
		x := leg()
		r := b.Handle(t.Context(), p, x)
		if r.Outcome != filter.Continue || r.Err != nil {
			t.Errorf("Handle(%s) = %+v, want Continue", p, r)
		}
		if got := x.Msg.Header().Get("Authorization"); got != "Bearer client-jwt" {
			t.Errorf("Handle(%s) changed Authorization to %q", p, got)
		}
	}
	if c := h.ts.count(); c != 0 {
		t.Errorf("token server got %d requests, want 0", c)
	}
}

// TestOutgoingRequest checks where the token goes: the Phase's message,
// else the Leg's request (its header created when nil); a leg view with
// neither is CannotDecide with RZ-AUTH-020 and fetches nothing.
func TestOutgoingRequest(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	for _, hdr := range []http.Header{{"Authorization": {"Basic Y2xpZW50"}}, nil} {
		req := &http.Request{Header: hdr}
		x := &filtertest.Exchange{LegV: &filter.Leg{Upstream: "orders", Attempt: 2, Request: req}}
		r := b.Handle(t.Context(), phase.OnUpstreamRequest, x)
		wantBearer(t, r, req.Header.Values("Authorization"), "tok-1")
	}
	for _, x := range []*filtertest.Exchange{{}, {LegV: &filter.Leg{Upstream: "orders"}}} {
		r := b.Handle(t.Context(), phase.OnUpstreamRequest, x)
		wantUndecided(t, r, errNoRequest)
	}
	if c := h.ts.count(); c != 1 {
		t.Errorf("token server got %d requests, want 1", c)
	}
}

// TestTokenAgeGauge checks ruralz_auth_upstream_token_age_seconds: the
// age of the token sent, in whole seconds, written once per change rather
// than once per request.
func TestTokenAgeGauge(t *testing.T) {
	h := running(t, issue(300))
	b := h.build(testPolicy, h.config())
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, "tok-1")
	// The fetch records the age after its waiters are released.
	h.waitFetches(1)
	if got := b.age.v.Load(); got != 0 {
		t.Errorf("age after the fetch = %d, want 0", got)
	}
	h.clk.Advance(90*time.Second + 500*time.Millisecond)
	handle(t.Context(), b)
	if got := b.age.v.Load(); got != 90 {
		t.Errorf("age = %d, want 90", got)
	}
	sets := b.age.sets.Load()
	handle(t.Context(), b)
	if b.age.sets.Load() != sets {
		t.Error("an unchanged age was written again")
	}
}

// TestSecretsNeverLeak checks Secrets rule 2 for this hop: with a canary
// client secret and a canary token, and a token endpoint that echoes them
// in error codes, descriptions and token types, neither reaches a log line
// or a Filter error in any encoding. The token endpoint and the Upstream
// are their designated destinations and are not scanned.
func TestSecretsNeverLeak(t *testing.T) {
	secretCanary := canary.Canary{Value: canary.Prefix + "00112233445566778899aabbccddeeff"}
	tokenCanary := canary.Canary{Value: canary.Prefix + "ffeeddccbbaa99887766554433221100"}
	h := running(t, func(n int, w http.ResponseWriter, _ *http.Request) {
		switch n {
		case 1:
			writeJSON(w, http.StatusBadRequest, `{"error":"`+secretCanary.Value+`","error_description":"`+secretCanary.Value+`"}`)
		case 2:
			writeJSON(w, http.StatusOK, `{"access_token":"`+tokenCanary.Value+`","token_type":"`+secretCanary.Value+`"}`)
		case 3:
			writeJSON(w, http.StatusOK, tokenBody(tokenCanary.Value, 300))
		default:
			writeJSON(w, http.StatusInternalServerError, `{"error":"server_error","access_token":"`+tokenCanary.Value+`"}`)
		}
	})
	h.secrets.set(secret.RefOf(testRef()), secretCanary.Value)
	b := h.build(testPolicy, h.config())
	var outputs [][]byte
	r, _ := handle(t.Context(), b)
	wantUndecided(t, r, nil)
	outputs = append(outputs, []byte(r.Err.Error()))
	h.clk.Advance(MinFetchInterval)
	r, _ = handle(t.Context(), b)
	wantUndecided(t, r, errTokenType)
	outputs = append(outputs, []byte(r.Err.Error()))
	h.clk.Advance(MinFetchInterval)
	r, auth := handle(t.Context(), b)
	wantBearer(t, r, auth, tokenCanary.Value)
	h.advanceArmed(200 * time.Second)
	h.waitFetches(4)
	if got := h.ts.request(t, 1).pass; got != secretCanary.Value {
		t.Fatal("the token endpoint did not get the canary secret")
	}
	outputs = append(outputs, h.logs.Bytes())
	if len(h.logs.Bytes()) == 0 {
		t.Fatal("no log lines to scan")
	}
	for i, out := range outputs {
		for _, c := range []canary.Canary{secretCanary, tokenCanary} {
			for _, f := range c.Scan("output", out) {
				t.Errorf("output %d leaks a canary: %s", i, f)
			}
		}
	}
}
