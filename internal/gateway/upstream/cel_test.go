// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestNativeDefaultsEqualCEL holds the native forms of the retryOn and
// failureWhen defaults (resilience.EvalRetryOn and EvalFailureWhen without
// a program) equal to expr.DefaultRetryOn and expr.DefaultFailureWhen
// compiled by the CEL engine, over every method, error kind and status of
// the matrix, for the activation the attempt loop builds. It admits the
// native forms, which Compile uses when the fields are absent.
func TestNativeDefaultsEqualCEL(t *testing.T) {
	c := celtest.Compiler(t)
	retryOn, failureWhen := c.Default(expr.PlaceRetryOn), c.Default(expr.PlaceFailureWhen)
	if retryOn == nil || failureWhen == nil {
		t.Fatal("the CEL compiler has no default program")
	}
	if retryOn.Source() != expr.DefaultRetryOn || failureWhen.Source() != expr.DefaultFailureWhen {
		t.Fatalf("default sources = %q, %q; want expr.DefaultRetryOn and expr.DefaultFailureWhen", retryOn.Source(), failureWhen.Source())
	}
	methods := []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE", "POST", "PATCH"}
	kinds := []resilience.Kind{resilience.KindConnect, resilience.KindReset, resilience.KindTimeout, resilience.KindTLS, resilience.KindNone}
	statuses := []int{200, 429, 500, 502, 503, 504}
	ls := &legState{u: &Upstream{name: "orders"}}
	for _, m := range methods {
		ls.base = &expr.Vars{Request: &expr.Request{Method: m, Header: http.Header{}}}
		ls.method = m
		for _, k := range kinds {
			for _, s := range statuses {
				var resp *http.Response
				if k == resilience.KindNone {
					resp = &http.Response{StatusCode: s, Header: http.Header{}}
				}
				v := ls.legVars(2, resp, k, "10.0.0.1:8080")
				gotR, errR := resilience.EvalRetryOn(t.Context(), nil, v)
				wantR, wantErrR := retryOn.EvalBool(t.Context(), v)
				if gotR != wantR || (errR != nil) != (wantErrR != nil) {
					t.Errorf("retryOn(%s, %v, %d): native %v, %v; CEL %v, %v", m, k, s, gotR, errR, wantR, wantErrR)
				}
				gotF, errF := resilience.EvalFailureWhen(t.Context(), nil, v)
				wantF, wantErrF := failureWhen.EvalBool(t.Context(), v)
				if gotF != wantF || (errF != nil) != (wantErrF != nil) {
					t.Errorf("failureWhen(%s, %v, %d): native %v, %v; CEL %v, %v", m, k, s, gotF, errF, wantF, wantErrF)
				}
			}
		}
	}
}

// TestLegRetryOnProgram checks a configured retryOn: it decides the retry
// over the leg activation, and a runtime error means no retry, counted in
// ruralz_upstream_cel_errors_total{field="retryOn"}.
func TestLegRetryOnProgram(t *testing.T) {
	for _, tc := range []struct {
		name     string
		src      string
		attempts int
		celErrs  uint64
	}{
		{"status 500 retried", `response != null && response.status == 500 && attempt == 1 && upstream.name == "orders"`, 2, 0},
		{"runtime error", `response.headers["x-missing"] == "y"`, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, status(http.StatusInternalServerError, ""))
			f := newFixture(t)
			met := newMetrics()
			prg := celtest.Program(t, celtest.At(expr.PlaceRetryOn), tc.src)
			s := f.compile(nil, Config{Upstream: newUpstream("orders", srv.addr), RetryOn: prg, Metrics: met.m})
			r, err := f.upstream(s, "orders").Leg(t.Context(), outbound(exchange(http.MethodPost, "/"), nil), Target{})
			if err != nil {
				t.Fatalf("Leg: %v", err)
			}
			readAll(t, r)
			if r.Attempts != tc.attempts || met.cel[emit.CELRetryOn].get() != tc.celErrs {
				t.Errorf("Leg after %d attempts with %d retryOn errors, want %d and %d", r.Attempts, met.cel[emit.CELRetryOn].get(), tc.attempts, tc.celErrs)
			}
		})
	}
}

// TestLegFailureWhenError checks that a failureWhen runtime error counts the
// attempt as a failure (it feeds passive ejection) and in
// ruralz_upstream_cel_errors_total{field="failureWhen"}.
func TestLegFailureWhenError(t *testing.T) {
	srvs, _ := orderedServers(t, 2)
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srvs[0].addr, srvs[1].addr)
	u.Spec.HealthCheck = &v1alpha1.HealthCheck{Passive: &v1alpha1.PassiveHealthCheck{ConsecutiveErrors: ptr[int32](1)}}
	prg := celtest.Program(t, celtest.At(expr.PlaceFailureWhen), `response.headers["x-missing"] == "y"`)
	s := f.compile(nil, Config{Upstream: u, FailureWhen: prg, Metrics: met.m})
	up := f.upstream(s, "orders")
	r, err := up.Leg(t.Context(), outbound(exchange(http.MethodGet, "/"), nil), Target{})
	if err != nil {
		t.Fatalf("Leg: %v", err)
	}
	readAll(t, r)
	if st := up.rt.tracker.Status(f.clk.Now()); !st[0].Ejected || met.cel[emit.CELFailureWhen].get() != 1 {
		t.Errorf("ejected = %v, failureWhen errors = %d; want true and 1", st[0].Ejected, met.cel[emit.CELFailureWhen].get())
	}
}

// TestLegRingHash checks ring-hash: the hashKey value maps every leg of
// one key to the same Endpoint; a hashKey runtime error picks a weighted
// random Endpoint and counts in
// ruralz_upstream_cel_errors_total{field="hashKey"}.
func TestLegRingHash(t *testing.T) {
	srvs, _ := orderedServers(t, 3)
	f := newFixture(t)
	met := newMetrics()
	u := newUpstream("orders", srvs[0].addr, srvs[1].addr, srvs[2].addr)
	u.Spec.LoadBalancing = &v1alpha1.LoadBalancing{Algorithm: ptr(v1alpha1.LoadBalancingRingHash), HashKey: `request.headers["x-user"]`}
	prg := celtest.Program(t, celtest.At(expr.PlaceHashKey), u.Spec.LoadBalancing.HashKey)
	s := f.compile(nil, Config{Upstream: u, HashKey: prg, Metrics: met.m})
	up := f.upstream(s, "orders")
	leg := func(user string) string {
		t.Helper()
		x := exchange(http.MethodGet, "/")
		if user != "" {
			x.Hdr.Set("X-User", user)
		}
		x.VarsV.Request.Header = x.Hdr
		r, err := up.Leg(t.Context(), outbound(x, nil), Target{})
		if err != nil {
			t.Fatalf("Leg: %v", err)
		}
		readAll(t, r)
		return r.Endpoint
	}
	for _, user := range []string{"alice", "bob", "carol", "dave"} {
		first := leg(user)
		for range 4 {
			if got := leg(user); got != first {
				t.Errorf("user %s went to %s then %s, want one Endpoint", user, first, got)
			}
		}
	}
	if met.cel[emit.CELHashKey].get() != 0 {
		t.Fatalf("hashKey errors = %d, want 0", met.cel[emit.CELHashKey].get())
	}
	if leg("") == "" || met.cel[emit.CELHashKey].get() != 1 {
		t.Errorf("hashKey errors after a missing key = %d, want 1", met.cel[emit.CELHashKey].get())
	}
}
