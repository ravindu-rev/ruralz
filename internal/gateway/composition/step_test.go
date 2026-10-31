// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestStepRequest checks what each step sends
// (docs/architecture/03-data-plane.md "Composition engine"): the method is
// the step's, else the incoming one; the target is the path or the
// pathExpression result, whose first "?" starts the raw query, sent as
// is, the client's raw query being forwarded without one; a step whose
// method is the incoming one sends the client body and any other none;
// the step's maxBodyBytes caps its leg.
func TestStepRequest(t *testing.T) {
	post := stepTo("post", "a", "/audit")
	post.Method = http.MethodPost
	put := stepTo("put", "a", "/same")
	put.Method = http.MethodPut
	tests := []struct {
		name               string
		step               v1alpha1.CompositionStep
		method, path, rawQ string
		sendsBody          bool
	}{
		{"literal path forwards the client query", stepTo("s", "a", "/orders/1"), http.MethodPut, "/orders/1", "tag=a&tag=b", true},
		{"literal path with its own query", stepTo("s", "a", "/orders?expand=items&x=%41"), http.MethodPut, "/orders", "expand=items&x=%41", true},
		{"literal path with an empty query", stepTo("s", "a", "/orders?"), http.MethodPut, "/orders", "", true},
		{"pathExpression without a query", v1alpha1.CompositionStep{Name: "s", Upstream: "a", PathExpression: `"/orders/" + request.pathParams.id`}, http.MethodPut, "/orders/42", "tag=a&tag=b", true},
		{"pathExpression with a query", v1alpha1.CompositionStep{Name: "s", Upstream: "a", PathExpression: `"/stock?order=" + request.pathParams.id + "&q=a b"`}, http.MethodPut, "/stock", "order=42&q=a b", true},
		{"another method sends no body", post, http.MethodPost, "/audit", "tag=a&tag=b", false},
		{"the incoming method set explicitly sends the body", put, http.MethodPut, "/same", "tag=a&tag=b", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			comp := f.compile(composition(v1alpha1.CompositionModeSequential, tt.step))
			x := exchange(http.MethodPut, "/v1/orders/42", "tag=a&tag=b", expr.Param{Name: "id", Value: "42", Raw: "42"})
			resp, err := comp.Forward(t.Context(), f.outbound(x, `{"order":42}`))
			if err != nil {
				t.Fatal(err)
			}
			read(t, resp)
			calls := f.leg("a").list()
			if len(calls) != 1 {
				t.Fatalf("calls = %d, want 1", len(calls))
			}
			c := calls[0]
			if c.target.Method != tt.method || c.target.Path != tt.path || c.target.RawQuery != tt.rawQ || c.target.Step != "s" && c.target.Step != tt.step.Name {
				t.Errorf("target = %+v, want %s %s ? %q", c.target, tt.method, tt.path, tt.rawQ)
			}
			if c.sendsBody != tt.sendsBody || (tt.sendsBody && c.body != `{"order":42}`) {
				t.Errorf("sends body = %v (%q), want %v", c.sendsBody, c.body, tt.sendsBody)
			}
			if c.target.MaxBodyBytes != 64<<10 {
				t.Errorf("leg MaxBodyBytes = %d, want the step's share", c.target.MaxBodyBytes)
			}
		})
	}
}

// TestComputedPathRefused checks that a pathExpression result that is not
// a hardened path fails the step with 502 RZ-RT-015 before any leg runs:
// no leading slash, a "." or ".." segment in any spelling, %00, a raw or
// encoded backslash, another control byte or an invalid escape, and a raw
// control byte in the query, which no request line can carry.
func TestComputedPathRefused(t *testing.T) {
	for _, src := range []string{
		`"orders/1"`,
		`"/orders/.."`,
		`"/orders/./1"`,
		`"/orders/%2E%2e/admin"`,
		`"/orders/.%2E"`,
		`"/orders/%00"`,
		`"/orders/%5C1"`,
		`"/orders\\1"`,
		`"/orders/\u0001"`,
		`"/orders/%zz"`,
		`"/orders?q=\r\n"`,
	} {
		t.Run(src, func(t *testing.T) {
			f := newFixture(t, "a")
			comp := f.compile(composition(v1alpha1.CompositionModeSequential, v1alpha1.CompositionStep{Name: "s", Upstream: "a", PathExpression: src}))
			resp, err := f.forward(comp)
			if resp != nil || codeOf(err) != CodeStepFailed || stepOf(err) != "s" {
				t.Errorf("Forward = %v, %v; want RZ-RT-015 for step s", resp, err)
			}
			if n := len(f.leg("a").list()); n != 0 {
				t.Errorf("leg ran %d times, want none", n)
			}
		})
	}
}

// TestComputedPathEscapes checks the functions a pathExpression splices
// decoded captures with: url.pathEscape keeps one path segment and
// url.queryEscape one query value, and request.rawPathParams gives the
// captures as the path holds them (docs/architecture/02-configuration-model.md
// "Limits" and "Variables").
func TestComputedPathEscapes(t *testing.T) {
	tests := []struct {
		src, path, query string
	}{
		{`"/orders/" + url.pathEscape(request.pathParams.id)`, "/orders/a%2Fb%3F%23%25", "tag=a"},
		{`"/orders/" + request.rawPathParams.id`, "/orders/a%2Fb%3F%23%25", "tag=a"},
		{`"/search?q=" + url.queryEscape(request.pathParams.id)`, "/search", "q=a%2Fb%3F%23%25"},
	}
	for _, tt := range tests {
		t.Run(tt.src, func(t *testing.T) {
			f := newFixture(t, "a")
			comp := f.compile(composition(v1alpha1.CompositionModeSequential, v1alpha1.CompositionStep{Name: "s", Upstream: "a", PathExpression: tt.src}))
			x := exchange(http.MethodGet, "/v1/orders/a%2Fb%3F%23%25", "tag=a", expr.Param{Name: "id", Value: "a/b?#%", Raw: "a%2Fb%3F%23%25"})
			resp, err := comp.Forward(t.Context(), f.outbound(x, ""))
			if err != nil {
				t.Fatal(err)
			}
			read(t, resp)
			c := f.leg("a").list()[0]
			if c.target.Path != tt.path || c.target.RawQuery != tt.query {
				t.Errorf("target %q ? %q, want %q ? %q", c.target.Path, c.target.RawQuery, tt.path, tt.query)
			}
		})
	}
}

// TestCELErrorFailsStep checks a runtime error in a step's pathExpression:
// it fails the step with 502 RZ-RT-015, optional deciding.
func TestCELErrorFailsStep(t *testing.T) {
	bad := v1alpha1.CompositionStep{Name: "bad", Upstream: "a", PathExpression: `"/x/" + request.pathParams.missing`}
	t.Run("required", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, bad, stepTo("b", "b", "/b")))
		if _, err := f.forward(comp); codeOf(err) != CodeStepFailed || stepOf(err) != "bad" {
			t.Errorf("Forward = %v, want RZ-RT-015 for step bad", err)
		}
	})
	t.Run("optional", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, optional(bad), stepTo("b", "b", "/b")))
		resp, err := f.forward(comp)
		if err != nil || !resp.Partial || resp.Header.Get("Ruralz-Partial") != "true" {
			t.Fatalf("Forward = %+v, %v; want a partial merged response", resp, err)
		}
		if got := read(t, resp); got != `{}` {
			t.Errorf("body = %s, want {}", got)
		}
	})
}

// TestLegFailureCodes checks that a step whose leg ends without a response
// fails with the leg's code at its registered status: an Upstream cause's
// RZ-UP code, and 504 RZ-RT-007 for a step started past the Route
// deadline; an error without a code is RZ-RT-015.
func TestLegFailureCodes(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code string
	}{
		{"connect", &upstream.LegError{Upstream: "a", Endpoint: "10.0.0.1:80", Attempts: 2, Err: codedErr("RZ-UP-007")}, "RZ-UP-007"},
		{"breaker open", &upstream.LegError{Upstream: "a", Err: codedErr("RZ-UP-005")}, "RZ-UP-005"},
		{"Route deadline before the step", &upstream.LegError{Upstream: "a", Err: codedErr("RZ-RT-007")}, "RZ-RT-007"},
		{"no code", errors.New("broken"), CodeStepFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			f.leg("a").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) { return nil, tt.err }
			comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("s", "a", "/s")))
			_, err := f.forward(comp)
			var ce *Error
			if !errors.As(err, &ce) || codeOf(err) != tt.code || ce.Step != "s" || ce.Upstream != "a" {
				t.Errorf("Forward = %v, want %s for step s naming Upstream a", err, tt.code)
			}
		})
	}
}
