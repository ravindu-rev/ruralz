// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the transport-time signing hook, legRun.Sign (foundation pack
// section 8.12 rule 6).

// signFilter is a request-signing Filter (filter.Signer): onUpstreamRequest
// records "params#<attempt>" in its PolicyState, as a signing type records
// its parameters, and Sign logs what it signs with, answering from sign.
type signFilter struct {
	name  string
	ev    *events
	sign  result
	signs atomic.Int64
	// handle answers onUpstreamRequest when set.
	handle result
}

var _ filter.Signer = (*signFilter)(nil)

func (f *signFilter) Handle(ctx context.Context, ph phase.Phase, x filter.Exchange) filter.Result {
	f.ev.add(f.name + "." + ph.String())
	if f.handle != nil {
		return f.handle(ctx, x)
	}
	*x.PolicyState() = fmt.Sprintf("params#%d", x.Leg().Attempt)
	return filter.Next()
}

func (f *signFilter) Sign(ctx context.Context, x filter.Exchange) filter.Result {
	f.signs.Add(1)
	f.ev.add(fmt.Sprintf("%s.sign#%d with %v", f.name, x.Leg().Attempt, *x.PolicyState()))
	if f.sign != nil {
		return f.sign(ctx, x)
	}
	return filter.Next()
}

// signChain returns a leg chain to "up": the signer (upstream-auth, with
// probe handles), then a header Policy that edits the attempt after it in
// onUpstreamRequest.
func signChain(h *harness, sig *signFilter, mods ...func(*spec)) (*snapshot.Chain, *probe) {
	s := spec{
		name: sig.name, class: phase.ClassUpstreamAuth, scope: phase.ScopeUpstream,
		typ: v1alpha1.PolicyTypeAuthUpstreamSigV4, phases: []phase.Phase{phase.OnUpstreamRequest}, f: sig,
	}
	for _, m := range mods {
		m(&s)
	}
	p := policy(s)
	m, pr := newProbe()
	p.Metrics = m
	hdr := policy(spec{
		name: "hdr", class: phase.ClassTransform, scope: phase.ScopeUpstream,
		phases: []phase.Phase{phase.OnUpstreamRequest}, f: &fakeFilter{name: "hdr", ev: h.ev},
	})
	return chain(nil, map[string][]*snapshot.Policy{"up": {p, hdr}}), pr
}

// TestSignRunsSignersAfterOnUpstreamRequest checks that Sign calls the
// signing Policy after every onUpstreamRequest Policy of the attempt ran,
// entered for that Policy (its PolicyState holds what it recorded), once
// per attempt, and that a Policy that is no Signer is not called.
func TestSignRunsSignersAfterOnUpstreamRequest(t *testing.T) {
	h := newHarness(t)
	sig := &signFilter{name: "sig", ev: h.ev}
	ch, pr := signChain(h, sig)
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "up", "")
	for attempt := 1; attempt <= 2; attempt++ {
		leg := &filter.Leg{Upstream: "up", Attempt: attempt, Request: &http.Request{Header: http.Header{}}}
		if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
			t.Fatalf("OnUpstreamRequest = %s", got(resp))
		}
		if resp := l.Sign(ctx, leg); resp != nil {
			t.Fatalf("Sign = %s", got(resp))
		}
	}
	l.End(ctx)
	want := []string{
		"sig.onUpstreamRequest", "hdr.onUpstreamRequest", "sig.sign#1 with params#1",
		"sig.onUpstreamRequest", "hdr.onUpstreamRequest", "sig.sign#2 with params#2",
	}
	if evs := h.ev.list(); !slices.Equal(evs, want) {
		t.Fatalf("calls:\n got %q\nwant %q", evs, want)
	}
	// The hook call is one more call of the Policy in onUpstreamRequest:
	// timed, and under a span of its own.
	if n := pr.dur[phase.OnUpstreamRequest].n.Load(); n != 4 {
		t.Errorf("onUpstreamRequest durations = %d, want 4 (Handle and Sign per attempt)", n)
	}
	spans := h.tr.named("sig")
	if len(spans) != 4 || spans[1].ph != phase.OnUpstreamRequest || spans[1].ended != 1 {
		t.Errorf("sig spans = %d, want 4 ended onUpstreamRequest spans", len(spans))
	}
}

// TestSignFailureIsClosedFailure checks that a signer that cannot decide
// fails like its Policy in onUpstreamRequest: a closed upstream-auth
// Policy ends the leg with 401 RZ-AUTH-020, recorded and counted as a
// closed failure of onUpstreamRequest, its span ended with the code.
func TestSignFailureIsClosedFailure(t *testing.T) {
	h := newHarness(t)
	sig := &signFilter{name: "sig", ev: h.ev, sign: undecided("RZ-AUTH-020")}
	ch, pr := signChain(h, sig)
	ctx := context.Background()
	r := h.e.Begin(h.st, ch, nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "up", "")
	leg := &filter.Leg{Upstream: "up", Attempt: 1, Request: &http.Request{Header: http.Header{}}}
	if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
		t.Fatalf("OnUpstreamRequest = %s", got(resp))
	}
	resp := l.Sign(ctx, leg)
	if g := got(resp); g != "401 RZ-AUTH-020" {
		t.Fatalf("Sign = %s, want 401 RZ-AUTH-020", g)
	}
	l.End(ctx)
	ls := h.st.legs[0]
	if len(ls.fails) != 1 || ls.fails[0] != (failRec{"sig", phase.OnUpstreamRequest, v1alpha1.FailureModeClosed}) {
		t.Errorf("failures recorded = %+v, want one closed failure of sig in onUpstreamRequest", ls.fails)
	}
	if n := pr.failures(phase.OnUpstreamRequest, emit.ModeClosed); n != 1 {
		t.Errorf("closed failures counted = %d, want 1", n)
	}
	spans := h.tr.named("sig")
	if last := spans[len(spans)-1]; last.code != "RZ-AUTH-020" || last.status != http.StatusUnauthorized {
		t.Errorf("signing span = %d %s, want 401 RZ-AUTH-020", last.status, last.code)
	}
}

// TestSignRecoversAndRefuses checks that a panic in Sign, and an outcome
// the hook does not allow (a response, a retry), are cannot decide: 401
// with the upstream-auth code, never a request sent with a broken
// signature.
func TestSignRecoversAndRefuses(t *testing.T) {
	for _, tc := range []struct {
		name string
		sign result
	}{
		{"panic", panics},
		{"respond", respondWith(http.StatusOK, "")},
		{"retry", outcome(filter.Retry)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sig := &signFilter{name: "sig", ev: h.ev, sign: tc.sign}
			ch, _ := signChain(h, sig)
			ctx := context.Background()
			r := h.e.Begin(h.st, ch, nil)
			defer r.Release()
			l := r.BeginLeg(ctx, "up", "")
			defer l.End(ctx)
			leg := &filter.Leg{Upstream: "up", Attempt: 1, Request: &http.Request{Header: http.Header{}}}
			_ = l.OnUpstreamRequest(ctx, leg)
			if g := got(l.Sign(ctx, leg)); g != "401 RZ-AUTH-020" {
				t.Errorf("Sign = %s, want 401 RZ-AUTH-020", g)
			}
		})
	}
}

// TestSignSkipsPoliciesThatDoNotRun checks that a signer whose spec.when
// skipped it on the leg, or that failed open, signs nothing.
func TestSignSkipsPoliciesThatDoNotRun(t *testing.T) {
	for _, tc := range []struct {
		name string
		mod  func(*spec)
		sig  func(h *harness) *signFilter
	}{
		{
			name: "when false",
			mod:  func(s *spec) { s.when = &constProgram{ok: false} },
			sig:  func(h *harness) *signFilter { return &signFilter{name: "sig", ev: h.ev} },
		},
		{
			name: "failed open",
			mod:  func(s *spec) { s.class, s.mode = phase.ClassTransform, v1alpha1.FailureModeOpen },
			sig: func(h *harness) *signFilter {
				return &signFilter{name: "sig", ev: h.ev, handle: undecided("")}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			sig := tc.sig(h)
			ch, _ := signChain(h, sig, tc.mod)
			ctx := context.Background()
			r := h.e.Begin(h.st, ch, nil)
			defer r.Release()
			l := r.BeginLeg(ctx, "up", "")
			defer l.End(ctx)
			leg := &filter.Leg{Upstream: "up", Attempt: 1, Request: &http.Request{Header: http.Header{}}}
			if resp := l.OnUpstreamRequest(ctx, leg); resp != nil {
				t.Fatalf("OnUpstreamRequest = %s", got(resp))
			}
			if resp := l.Sign(ctx, leg); resp != nil {
				t.Fatalf("Sign = %s, want no response", got(resp))
			}
			if n := sig.signs.Load(); n != 0 {
				t.Errorf("Sign called %d times, want none", n)
			}
		})
	}
}

// TestSignWithoutLegPolicies checks the hook of an Upstream whose leg chain
// has no Policy: nothing signs and the attempt goes on.
func TestSignWithoutLegPolicies(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	r := h.e.Begin(h.st, chain(nil, nil), nil)
	defer r.Release()
	l := r.BeginLeg(ctx, "up", "")
	defer l.End(ctx)
	if resp := l.Sign(ctx, &filter.Leg{Upstream: "up", Attempt: 1}); resp != nil {
		t.Fatalf("Sign = %s, want no response", got(resp))
	}
}
