// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
)

// retryOnProse is the retryOn default as 05 req 4 and the design document
// state it in words: "GET, HEAD, OPTIONS, PUT, DELETE: connect or reset
// errors, or status 503; other methods: connect errors".
func retryOnProse(method string, kind Kind, status int) bool {
	idempotent := slices.Contains([]string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE"}, method)
	if idempotent {
		return kind == KindConnect || kind == KindReset || (kind == KindNone && status == 503)
	}
	return kind == KindConnect
}

// failureWhenProse is "error != null || response.status in [502, 503, 504]".
func failureWhenProse(kind Kind, status int) bool {
	return kind != KindNone || slices.Contains([]int{502, 503, 504}, status)
}

// legVars builds the leg Vars of one matrix cell: a response when kind is
// none, else an error and no response (05 req 32).
func legVars(method string, kind Kind, status int) *expr.Vars {
	v := &expr.Vars{Request: &expr.Request{Method: method}, Attempt: 1, Upstream: &expr.Upstream{Name: "u", Endpoint: "10.0.0.1:80"}}
	if kind == KindNone {
		v.Response = &expr.Response{Status: status}
	} else {
		v.Error = &expr.AttemptError{Kind: kind.String()}
	}
	return v
}

// TestDefaultPredicates_05Req4 is 05 section 6 test 5 for the native
// forms: DefaultRetryOn and DefaultFailureWhen, and EvalRetryOn and
// EvalFailureWhen without a program, equal the default rules over the full
// matrix of 05 req 4. The CEL sources they stand for are pinned so a change
// to either form fails here.
func TestDefaultPredicates_05Req4(t *testing.T) {
	const (
		wantRetryOn     = `error != null ? (error.kind == "connect" || (error.kind == "reset" && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])) : (response.status == 503 && request.method in ["GET", "HEAD", "OPTIONS", "PUT", "DELETE"])`
		wantFailureWhen = `error != null || response.status in [502, 503, 504]`
	)
	if expr.DefaultRetryOn != wantRetryOn || expr.DefaultFailureWhen != wantFailureWhen {
		t.Fatal("the CEL default sources changed; re-derive the native forms")
	}
	// The matrix of 05 req 4: {GET, HEAD, OPTIONS, PUT, DELETE, POST,
	// PATCH} × {connect, reset, timeout, tls, none} × {200, 429, 500, 502,
	// 503, 504}.
	methods := []string{"GET", "HEAD", "OPTIONS", "PUT", "DELETE", "POST", "PATCH"}
	kinds := []Kind{KindConnect, KindReset, KindTimeout, KindTLS, KindNone}
	statuses := []int{200, 429, 500, 502, 503, 504}
	ctx := context.Background()
	cells := 0
	for _, m := range methods {
		for _, k := range kinds {
			for _, s := range statuses {
				cells++
				want := retryOnProse(m, k, s)
				if got := DefaultRetryOn(m, k, s); got != want {
					t.Errorf("DefaultRetryOn(%s, %v, %d) = %v, want %v", m, k, s, got, want)
				}
				got, err := EvalRetryOn(ctx, nil, legVars(m, k, s))
				if err != nil || got != want {
					t.Errorf("EvalRetryOn(nil, %s, %v, %d) = %v, %v; want %v", m, k, s, got, err, want)
				}
				wantF := failureWhenProse(k, s)
				if got := DefaultFailureWhen(k, s); got != wantF {
					t.Errorf("DefaultFailureWhen(%v, %d) = %v, want %v", k, s, got, wantF)
				}
				got, err = EvalFailureWhen(ctx, nil, legVars(m, k, s))
				if err != nil || got != wantF {
					t.Errorf("EvalFailureWhen(nil, %s, %v, %d) = %v, %v; want %v", m, k, s, got, err, wantF)
				}
			}
		}
	}
	if cells != 7*5*6 {
		t.Fatalf("matrix has %d cells", cells)
	}
}

// TestDefaultPredicatesNulls follows CEL's null and error semantics outside
// the matrix: a select on a null variable is a runtime error unless the
// other side of && or || decides.
func TestDefaultPredicatesNulls(t *testing.T) {
	tests := []struct {
		name       string
		v          *expr.Vars
		retry      bool
		retryErr   bool
		failure    bool
		failureErr bool
	}{
		{name: "nil Vars", v: nil, retry: false, retryErr: true, failure: true, failureErr: true},
		{name: "no response, no error, POST", v: &expr.Vars{Request: &expr.Request{Method: "POST"}}, retry: false, failure: true, failureErr: true},
		{name: "no response, no error, GET", v: &expr.Vars{Request: &expr.Request{Method: "GET"}}, retryErr: true, failure: true, failureErr: true},
		{name: "reset without request", v: &expr.Vars{Error: &expr.AttemptError{Kind: "reset"}}, retryErr: true, failure: true},
		{name: "connect without request", v: &expr.Vars{Error: &expr.AttemptError{Kind: "connect"}}, retry: true, failure: true},
		{name: "timeout without request", v: &expr.Vars{Error: &expr.AttemptError{Kind: "timeout"}}, retry: false, failure: true},
		{name: "503 without request", v: &expr.Vars{Response: &expr.Response{Status: 503}}, retryErr: true, failure: true},
		{name: "200 without request", v: &expr.Vars{Response: &expr.Response{Status: 200}}, retry: false, failure: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := EvalRetryOn(context.Background(), nil, tt.v)
			var ee *expr.EvalError
			if (err != nil) != tt.retryErr || got != tt.retry {
				t.Fatalf("retryOn = %v, %v", got, err)
			}
			if err != nil && (!errors.As(err, &ee) || ee.Place != expr.PlaceRetryOn || ee.Kind != expr.KindNull) {
				t.Fatalf("retryOn error %#v", err)
			}
			got, err = EvalFailureWhen(context.Background(), nil, tt.v)
			if (err != nil) != tt.failureErr || got != tt.failure {
				t.Fatalf("failureWhen = %v, %v", got, err)
			}
			if err != nil && (!errors.As(err, &ee) || ee.Place != expr.PlaceFailureWhen) {
				t.Fatalf("failureWhen error %#v", err)
			}
		})
	}
}

// TestEvalPrograms_05Req32 runs authored programs: a retryOn runtime error
// means no retry, a failureWhen runtime error counts as a failure.
func TestEvalPrograms_05Req32(t *testing.T) {
	ctx := context.Background()
	v := legVars("POST", KindNone, 429)
	boom := expr.NewEvalError(expr.PlaceRetryOn, expr.KindNoSuchKey, "no such key")
	tests := []struct {
		name    string
		eval    func(context.Context, expr.Program, *expr.Vars) (bool, error)
		prog    *fakeProgram
		want    bool
		wantErr bool
	}{
		{"retryOn true", EvalRetryOn, &fakeProgram{ok: true}, true, false},
		{"retryOn false", EvalRetryOn, &fakeProgram{ok: false}, false, false},
		{"retryOn runtime error is no retry", EvalRetryOn, &fakeProgram{ok: true, err: boom}, false, true},
		{"failureWhen true", EvalFailureWhen, &fakeProgram{ok: true}, true, false},
		{"failureWhen false", EvalFailureWhen, &fakeProgram{ok: false}, false, false},
		{"failureWhen runtime error is a failure", EvalFailureWhen, &fakeProgram{ok: false, err: boom}, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.eval(ctx, tt.prog, v)
			if got != tt.want || (err != nil) != tt.wantErr || tt.prog.calls != 1 {
				t.Fatalf("got %v, %v after %d calls", got, err, tt.prog.calls)
			}
			if tt.wantErr && !errors.Is(err, boom) {
				t.Fatalf("error %v does not wrap the runtime error", err)
			}
		})
	}
}
