// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// whenStep returns a step to upstream with a literal path and a when.
func whenStep(name, upstreamName, path, when string) v1alpha1.CompositionStep {
	s := stepTo(name, upstreamName, path)
	s.When = when
	return s
}

// TestSequentialSteps checks that later steps read earlier results
// through steps: status, headers and the body of a step a later
// expression reads, decoded as JSON.
func TestSequentialSteps(t *testing.T) {
	f := newFixture(t, "users", "orders")
	f.leg("users").fn = answer(func() *snapshot.UpstreamResponse {
		r := jsonResp(200, `{"id":"u7","tier":"gold"}`)
		r.Header["X-Region"] = []string{"eu"}
		return r
	})
	f.leg("orders").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"orders":[]}`) })
	orders := v1alpha1.CompositionStep{
		Name: "orders", Upstream: "orders",
		PathExpression: `"/users/" + steps.user.body.id + "/orders?region=" + steps.user.headers["x-region"]`,
		When:           `steps.user.status == 200 && steps.user.body.tier == "gold"`,
	}
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("user", "users", "/me"), orders))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"orders":[]}` {
		t.Errorf("body = %s", got)
	}
	c := f.leg("orders").list()
	if len(c) != 1 || c[0].target.Path != "/users/u7/orders" || c[0].target.RawQuery != "region=eu" {
		t.Errorf("orders calls = %+v", c)
	}
}

// TestSequentialPassthrough checks the response of a sequential
// composition whose last executed step merges nothing: that step's own
// response, status, headers and body streamed as received, whatever its
// status; a step whose when is false is skipped.
func TestSequentialPassthrough(t *testing.T) {
	f := newFixture(t, "a", "b", "c")
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse {
		return respWith(404, http.Header{"Content-Type": {"text/plain"}, "X-From": {"b"}}, "not here")
	})
	comp := f.compile(composition(v1alpha1.CompositionModeSequential,
		stepTo("a", "a", "/a"),
		stepTo("b", "b", "/b"),
		whenStep("c", "c", "/c", `steps.b.status == 200`)))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); resp.Status != 404 || got != "not here" || resp.Header.Get("X-From") != "b" || resp.Partial {
		t.Errorf("response = %d %q %v, want b's 404", resp.Status, got, resp.Header)
	}
	if n := len(f.leg("c").list()); n != 0 {
		t.Errorf("step c ran %d times, want skipped", n)
	}
	if resp.Upstream != "b" {
		t.Errorf("response describes %q, want the last step that ran", resp.Upstream)
	}
}

// TestSequentialNonFinal checks a non-2xx result of a step that merges
// nothing: it fails the step once a later step runs, as a non-final step,
// with 502 RZ-UP-011 (or its generated response); optional leaves it out
// with ruralz-partial: true; and its body is closed before the later step
// runs.
func TestSequentialNonFinal(t *testing.T) {
	t.Run("required", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(500, `{}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
		if _, err := f.forward(comp); codeOf(err) != CodeStepStatus || stepOf(err) != "a" {
			t.Errorf("Forward = %v, want RZ-UP-011 for step a", err)
		}
		if n := len(f.leg("b").list()); n != 0 {
			t.Errorf("step b ran %d times after a failed", n)
		}
	})
	t.Run("generated", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		gen := &filter.Response{Status: 503, Code: "RZ-RT-011"}
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return &snapshot.UpstreamResponse{Status: 503, Generated: gen} })
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), stepTo("b", "b", "/b")))
		resp, err := f.forward(comp)
		if err != nil || resp.Generated != gen {
			t.Errorf("Forward = %+v, %v; want a's generated 503", resp, err)
		}
	})
	t.Run("optional", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		aBody := newBody(`{"e":1}`)
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return &snapshot.UpstreamResponse{Status: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: aBody}
		})
		f.leg("b").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
			if !aBody.closed.Load() {
				t.Error("step a's body was still open when step b ran")
			}
			return respWith(200, http.Header{"Content-Type": {"text/plain"}}, "b"), nil
		}
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, optional(stepTo("a", "a", "/a")), stepTo("b", "b", "/b")))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); got != "b" || !resp.Partial || resp.Header.Get("Ruralz-Partial") != "true" {
			t.Errorf("response = %q %v, want b's response marked partial", got, resp.Header)
		}
	})
	t.Run("final passes through", func(t *testing.T) {
		f := newFixture(t, "a", "b")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(500, `{"e":1}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), whenStep("b", "b", "/b", `steps.a.status == 200`)))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); resp.Status != 500 || got != `{"e":1}` {
			t.Errorf("response = %d %s, want a's 500 as the final step", resp.Status, got)
		}
	})
}

// TestSequentialMerge checks a sequential composition whose last executed
// step merges: every executed step that merges merges, in list order; a
// step that merges nothing and a skipped step do not.
func TestSequentialMerge(t *testing.T) {
	f := newFixture(t, "a", "b", "c", "d")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"a":1}`) })
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"b":2}`) })
	f.leg("d").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"d":4}`) })
	a := stepTo("a", "a", "/a")
	a.Group = "first"
	c := whenStep("c", "c", "/c", "false")
	c.Group = "skipped"
	d := stepTo("d", "d", "/d")
	d.Select = []string{"d"}
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, a, stepTo("b", "b", "/b"), c, d))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"first":{"a":1},"d":4}` {
		t.Errorf("body = %s", got)
	}
}

// TestNoStepRan checks 404 RZ-RT-010 when a sequential composition runs no
// step, as when no conditional step's when is true.
func TestNoStepRan(t *testing.T) {
	for _, mode := range []v1alpha1.CompositionMode{v1alpha1.CompositionModeSequential, v1alpha1.CompositionModeConditional} {
		t.Run(string(mode), func(t *testing.T) {
			f := newFixture(t, "a")
			comp := f.compile(composition(mode, whenStep("a", "a", "/a", "false"), whenStep("b", "a", "/b", `request.method == "DELETE"`)))
			resp, err := f.forward(comp)
			if resp != nil || codeOf(err) != CodeNoStep {
				t.Errorf("Forward = %v, %v; want RZ-RT-010", resp, err)
			}
		})
	}
}

// TestWhenError checks a runtime error in a step's when: the step fails
// with 502 RZ-RT-015, and an optional step's when counts as false.
func TestWhenError(t *testing.T) {
	bad := `request.pathParams.missing == "x"`
	for _, mode := range []v1alpha1.CompositionMode{v1alpha1.CompositionModeSequential, v1alpha1.CompositionModeConditional} {
		t.Run(string(mode)+"/required", func(t *testing.T) {
			f := newFixture(t, "a", "b")
			comp := f.compile(composition(mode, whenStep("a", "a", "/a", bad), stepTo("b", "b", "/b")))
			if _, err := f.forward(comp); codeOf(err) != CodeStepFailed || stepOf(err) != "a" {
				t.Errorf("Forward = %v, want RZ-RT-015 for step a", err)
			}
		})
		t.Run(string(mode)+"/optional", func(t *testing.T) {
			f := newFixture(t, "a", "b")
			f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return respWith(200, http.Header{}, "b") })
			comp := f.compile(composition(mode, optional(whenStep("a", "a", "/a", bad)), stepTo("b", "b", "/b")))
			resp, err := f.forward(comp)
			if err != nil {
				t.Fatal(err)
			}
			if got := read(t, resp); got != "b" || resp.Partial {
				t.Errorf("response = %q partial %v, want b's, the errored when counted false", got, resp.Partial)
			}
			if n := len(f.leg("a").list()); n != 0 {
				t.Errorf("step a ran %d times", n)
			}
		})
	}
}

// TestFinalStreamingStepFailure checks that a final step that streams
// answers its own failure even when optional, since nothing is left to
// return.
func TestFinalStreamingStepFailure(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("b").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
		return nil, &upstream.LegError{Upstream: "b", Err: codedErr("RZ-UP-006")}
	}
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), optional(stepTo("b", "b", "/b"))))
	resp, err := f.forward(comp)
	if resp != nil || codeOf(err) != "RZ-UP-006" || stepOf(err) != "b" {
		t.Errorf("Forward = %v, %v; want step b's RZ-UP-006", resp, err)
	}
}

// TestStreamedFinalBody checks the final streamed step's maxBodyBytes: a
// declared Content-Length over it fails the step before commit with
// RZ-RT-015, and a body without one that passes it is cut there and ends
// with an error carrying RZ-UP-009.
func TestStreamedFinalBody(t *testing.T) {
	cap8 := func() v1alpha1.CompositionStep {
		s := stepTo("a", "a", "/a")
		n := v1alpha1.ByteSize(8)
		s.MaxBodyBytes = &n
		return s
	}
	t.Run("declared", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return respWith(200, http.Header{"Content-Length": {"9"}}, "123456789")
		})
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, cap8()))
		if _, err := f.forward(comp); codeOf(err) != CodeStepFailed {
			t.Errorf("Forward = %v, want RZ-RT-015", err)
		}
	})
	t.Run("streamed", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return respWith(200, http.Header{}, "123456789") })
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, cap8()))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if string(got) != "12345678" || codeOf(err) != CodeAfterCommit {
			t.Errorf("read %q, %v; want 8 bytes then RZ-UP-009", got, err)
		}
	})
	t.Run("HEAD", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return respWith(200, http.Header{"Content-Length": {"1000"}}, "")
		})
		comp := f.compile(composition(v1alpha1.CompositionModeSequential, cap8()))
		resp, err := comp.Forward(t.Context(), f.outbound(exchange(http.MethodHead, "/v1", ""), ""))
		if err != nil || resp.Status != 200 {
			t.Fatalf("Forward = %+v, %v; a HEAD response's Content-Length declares no body", resp, err)
		}
		read(t, resp)
	})
}

// TestGatedFinalPassthrough checks a final step gated for a later step
// that never ran: its buffered body passes through with its headers.
func TestGatedFinalPassthrough(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"next":false}`) })
	b := v1alpha1.CompositionStep{Name: "b", Upstream: "b", Path: "/b", When: `steps.a.body.next == true`}
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), b))
	if !comp.steps[0].gate() {
		t.Fatal("step a should be gated for step b's when")
	}
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"next":false}` || resp.Header.Get("Content-Length") != "14" {
		t.Errorf("response = %s %v, want a's own", got, resp.Header)
	}
	if got := f.budget.Used(); got != 0 {
		t.Errorf("budget used after Close = %d, want 0", got)
	}
}

// TestStepBodyNotJSONReadsNull checks that a later step reads the body of
// a step that is not JSON as null.
func TestStepBodyNotJSONReadsNull(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
		return respWith(200, http.Header{"Content-Type": {"text/plain"}}, "plain")
	})
	b := v1alpha1.CompositionStep{Name: "b", Upstream: "b", Path: "/b", When: `steps.a.body == null`}
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, stepTo("a", "a", "/a"), b))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	read(t, resp)
	if n := len(f.leg("b").list()); n != 1 {
		t.Errorf("step b ran %d times, want once: a's body reads null", n)
	}
}

// TestConditional checks the conditional mode: the first step whose when
// is true runs (a step without when is true), its response passing
// through whatever its status, or merged alone when it merges; an
// optional merged step that fails answers 200 {} marked partial.
func TestConditional(t *testing.T) {
	t.Run("first true runs", func(t *testing.T) {
		f := newFixture(t, "a", "b", "c")
		f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return respWith(418, http.Header{}, "teapot") })
		comp := f.compile(composition(v1alpha1.CompositionModeConditional,
			whenStep("a", "a", "/a", `request.method == "POST"`),
			whenStep("b", "b", "/b", `request.method == "GET"`),
			stepTo("c", "c", "/c")))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); resp.Status != 418 || got != "teapot" {
			t.Errorf("response = %d %q, want b's", resp.Status, got)
		}
		if len(f.leg("a").list())+len(f.leg("c").list()) != 0 {
			t.Error("a step other than b ran")
		}
	})
	t.Run("default step", func(t *testing.T) {
		f := newFixture(t, "a", "c")
		comp := f.compile(composition(v1alpha1.CompositionModeConditional, whenStep("a", "a", "/a", "false"), stepTo("c", "c", "/c")))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		read(t, resp)
		if len(f.leg("c").list()) != 1 {
			t.Error("the step without when did not run")
		}
	})
	t.Run("merged alone", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"x":{"y":1}}`) })
		s := stepTo("a", "a", "/a")
		s.Target = "x"
		comp := f.compile(composition(v1alpha1.CompositionModeConditional, s))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); got != `{"y":1}` {
			t.Errorf("body = %s", got)
		}
	})
	t.Run("optional merged step fails", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(500, `{}`) })
		s := stepTo("a", "a", "/a")
		s.Group = "a"
		comp := f.compile(composition(v1alpha1.CompositionModeConditional, optional(s)))
		resp, err := f.forward(comp)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, resp); got != `{}` || !resp.Partial {
			t.Errorf("response = %s partial %v, want {} partial", got, resp.Partial)
		}
	})
	t.Run("streamed step fails even optional", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
			return nil, &upstream.LegError{Upstream: "a", Err: codedErr("RZ-UP-008")}
		}
		comp := f.compile(composition(v1alpha1.CompositionModeConditional, optional(stepTo("a", "a", "/a"))))
		if _, err := f.forward(comp); codeOf(err) != "RZ-UP-008" {
			t.Errorf("Forward = %v, want RZ-UP-008", err)
		}
	})
}

// TestPartialPassthroughGenerated checks that a generated final response
// carries ruralz-partial: true when an optional step was left out, on a
// copy, so the leg Filter's response is untouched.
func TestPartialPassthroughGenerated(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.leg("a").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
		return nil, errors.New("down")
	}
	gen := &filter.Response{Status: 200, Header: http.Header{"X": {"1"}}, Body: []byte("hit")}
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return &snapshot.UpstreamResponse{Status: 200, Generated: gen} })
	comp := f.compile(composition(v1alpha1.CompositionModeSequential, optional(stepTo("a", "a", "/a")), stepTo("b", "b", "/b")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Generated == gen || resp.Generated.Header.Get("Ruralz-Partial") != "true" || gen.Header.Get("Ruralz-Partial") != "" {
		t.Errorf("generated = %+v, want a partial copy", resp.Generated)
	}
	if !strings.EqualFold(string(resp.Generated.Body), "hit") {
		t.Errorf("body = %q", resp.Generated.Body)
	}
}
