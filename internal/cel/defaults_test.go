// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// outcome is one attempt result: an error kind or a response status.
type outcome struct {
	errKind string
	status  int
}

// vars returns the leg activation of method and o.
func (o outcome) vars(method string) *expr.Vars {
	v := &expr.Vars{Request: &expr.Request{Method: method}, Upstream: &expr.Upstream{Name: "u"}, Attempt: 1}
	if o.errKind != "" {
		v.Error = &expr.AttemptError{Kind: o.errKind}
	} else {
		v.Response = &expr.Response{Status: o.status}
	}
	return v
}

// TestDefaultsFollowTheTrafficTable evaluates the compiled defaults over
// every method and outcome against
// docs/architecture/09-traffic-management-and-resilience.md: retryOn
// retries GET, HEAD, OPTIONS, PUT and DELETE on connect or reset errors or
// 503, other methods on connect errors; failureWhen counts any error and
// 502, 503 and 504.
func TestDefaultsFollowTheTrafficTable(t *testing.T) {
	c := newCompiler(t)
	retryOn, failureWhen := c.Default(expr.PlaceRetryOn), c.Default(expr.PlaceFailureWhen)
	if retryOn == nil || failureWhen == nil {
		t.Fatal("a default is missing")
	}
	if retryOn.Source() != expr.DefaultRetryOn || failureWhen.Source() != expr.DefaultFailureWhen {
		t.Errorf("default sources = %q, %q", retryOn.Source(), failureWhen.Source())
	}
	idempotent := []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE"}
	outcomes := []outcome{{errKind: "connect"}, {errKind: "reset"}, {errKind: "timeout"}, {errKind: "tls"}, {status: 200}, {status: 502}, {status: 503}, {status: 504}}
	for _, method := range append(slices.Clone(idempotent), "POST", "PATCH") {
		for _, o := range outcomes {
			wantRetry := o.errKind == "connect"
			if slices.Contains(idempotent, method) {
				wantRetry = wantRetry || o.errKind == "reset" || o.status == 503
			}
			wantFailure := o.errKind != "" || o.status == 502 || o.status == 503 || o.status == 504
			if got, err := retryOn.EvalBool(t.Context(), o.vars(method)); err != nil || got != wantRetry {
				t.Errorf("retryOn(%s, %+v) = %v, %v; want %v", method, o, got, err, wantRetry)
			}
			if got, err := failureWhen.EvalBool(t.Context(), o.vars(method)); err != nil || got != wantFailure {
				t.Errorf("failureWhen(%s, %+v) = %v, %v; want %v", method, o, got, err, wantFailure)
			}
		}
	}
}

// TestDefaultOfOtherPlaces checks that only retryOn and failureWhen have a
// default.
func TestDefaultOfOtherPlaces(t *testing.T) {
	c := newCompiler(t)
	for _, p := range expr.Places() {
		got := c.Default(p.ID)
		has := p.ID == expr.PlaceRetryOn || p.ID == expr.PlaceFailureWhen
		if (got != nil) != has {
			t.Errorf("Default(%s) = %v, want present %v", p.ID, got, has)
		}
	}
}
