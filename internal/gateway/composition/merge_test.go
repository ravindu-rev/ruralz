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
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// mergeStep returns an aggregate step to upstream a with the merge fields
// set.
func mergeStep(target string, collection bool, group string, sel []string, rename map[string]string) v1alpha1.CompositionStep {
	s := stepTo("m", "a", "/m")
	s.Target, s.Group, s.Select, s.Rename = target, group, sel, rename
	if collection {
		s.Collection = ptr(true)
	}
	return s
}

// TestMergeRules checks the merge rules of one step
// (docs/architecture/03-data-plane.md "Merge rules"): target unwraps a
// nested value by a dot path (a numeric segment indexes an array, "*"
// collects the matches); with collection the value must be an array of
// objects, without it an object; select keeps the named members and
// rename renames them, per element of a collection; anything else fails
// the step with 502 RZ-RT-015.
func TestMergeRules(t *testing.T) {
	const doc = `{"data":{"order":{"id":"o1","status":"paid","total":12,"secret":"x"},` +
		`"items":[{"sku":"a","qty":1,"x":0},{"sku":"b","qty":2,"x":0}],"tags":["t"],"name":"n"}}`
	tests := []struct {
		name string
		step v1alpha1.CompositionStep
		body string
		want string // merged body, or "" for RZ-RT-015
	}{
		{"whole object", mergeStep("", false, "", nil, nil), `{"a":1,"b":{"c":2}}`, `{"a":1,"b":{"c":2}}`},
		{"target object", mergeStep("data.order", false, "", nil, nil), doc, `{"id":"o1","status":"paid","total":12,"secret":"x"}`},
		{"target with select and rename", mergeStep("data.order", false, "order", []string{"id", "status", "total"}, map[string]string{"total": "amount"}), doc, `{"order":{"id":"o1","status":"paid","amount":12}}`},
		{"collection with select and rename", mergeStep("data.items", true, "items", []string{"sku", "qty"}, map[string]string{"qty": "quantity"}), doc, `{"items":[{"sku":"a","quantity":1},{"sku":"b","quantity":2}]}`},
		{"collection as is", mergeStep("data.items", true, "items", nil, nil), doc, `{"items":[{"sku":"a","qty":1,"x":0},{"sku":"b","qty":2,"x":0}]}`},
		{"numeric segment indexes an array", mergeStep("data.items.1", false, "second", nil, nil), doc, `{"second":{"sku":"b","qty":2,"x":0}}`},
		{"numeric segment names a member", mergeStep("data.1", false, "", nil, nil), `{"data":{"1":{"k":"v"}}}`, `{"k":"v"}`},
		{"escaped dot names one member", mergeStep(`data.a\.b`, false, "", nil, nil), `{"data":{"a.b":{"k":"v"}}}`, `{"k":"v"}`},
		{"star collects the matches", mergeStep("data.items.*", true, "items", []string{"sku"}, nil), doc, `{"items":[{"sku":"a"},{"sku":"b"}]}`},
		{"star over a missing array matches nothing", mergeStep("data.none.*", true, "items", nil, nil), doc, `{"items":[]}`},
		{"rename onto a kept name: the later value at the first position", mergeStep("", false, "", nil, map[string]string{"a": "b"}), `{"b":1,"a":2}`, `{"b":2}`},
		{"select of a missing member", mergeStep("", false, "", []string{"z"}, nil), `{"a":1}`, `{}`},
		{"missing target", mergeStep("data.nope", false, "", nil, nil), doc, ""},
		{"index past the array", mergeStep("data.items.5", false, "", nil, nil), doc, ""},
		{"target through a scalar", mergeStep("data.name.x", false, "", nil, nil), doc, ""},
		{"name on an array", mergeStep("data.items.sku", false, "", nil, nil), doc, ""},
		{"star on an object", mergeStep("data.order.*", true, "o", nil, nil), doc, ""},
		{"array without collection", mergeStep("data.items", false, "items", nil, nil), doc, ""},
		{"object with collection", mergeStep("data.order", true, "order", nil, nil), doc, ""},
		{"collection element not an object", mergeStep("data.tags", true, "tags", nil, nil), doc, ""},
		{"scalar body", mergeStep("", false, "", nil, nil), `"text"`, ""},
		{"empty body", mergeStep("", false, "", nil, nil), ``, ""},
		{"invalid JSON", mergeStep("", false, "", nil, nil), `{"a":`, ""},
		{"duplicate member names", mergeStep("", false, "", nil, nil), `{"a":1,"a":2}`, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, tt.body) })
			comp := f.compile(composition(v1alpha1.CompositionModeAggregate, tt.step))
			resp, err := f.forward(comp)
			if tt.want == "" {
				if resp != nil || codeOf(err) != CodeStepFailed || stepOf(err) != "m" {
					t.Errorf("Forward = %v, %v; want RZ-RT-015 for step m", resp, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := read(t, resp); got != tt.want {
				t.Errorf("body = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestMergeContentType checks that a merged body is JSON by its one
// Content-Type field line: application/json or a +json type, parameters
// allowed; another type, no field or two lines fail the step.
func TestMergeContentType(t *testing.T) {
	for _, tt := range []struct {
		name  string
		lines []string
		ok    bool
	}{
		{"application/json", []string{"application/json"}, true},
		{"+json with a parameter", []string{"application/problem+json; charset=utf-8"}, true},
		{"another type", []string{"text/plain"}, false},
		{"no Content-Type", nil, false},
		{"two lines", []string{"application/json", "application/json"}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
				h := http.Header{}
				if tt.lines != nil {
					h["Content-Type"] = tt.lines
				}
				return respWith(200, h, `{"a":1}`)
			})
			comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
			resp, err := f.forward(comp)
			if tt.ok != (err == nil) || (!tt.ok && codeOf(err) != CodeStepFailed) {
				t.Errorf("Forward = %v, %v; want ok %v", resp, err, tt.ok)
			}
			read(t, resp)
		})
	}
}

// TestMergedStepStatus checks that a non-2xx result fails a merged step:
// an Upstream's with 502 RZ-UP-011, a leg Filter's generated response
// with that response's own status and code, whatever optional says for
// the request (here not optional).
func TestMergedStepStatus(t *testing.T) {
	t.Run("Upstream", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(404, `{"error":"x"}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
		if _, err := f.forward(comp); codeOf(err) != CodeStepStatus {
			t.Errorf("Forward = %v, want RZ-UP-011", err)
		}
	})
	t.Run("generated", func(t *testing.T) {
		f := newFixture(t, "a")
		gen := &filter.Response{Status: 401, Code: "RZ-AUTH-020", Header: http.Header{"Www-Authenticate": {"Bearer"}}}
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return &snapshot.UpstreamResponse{Status: 401, Generated: gen} })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
		resp, err := f.forward(comp)
		if err != nil || resp.Generated != gen || resp.Status != 401 {
			t.Errorf("Forward = %+v, %v; want the generated 401 RZ-AUTH-020", resp, err)
		}
	})
}

// TestStepBodyLimits checks the caps of a gated step body: a declared
// Content-Length over maxBodyBytes fails the step before any byte is read,
// so does a body that passes it, and a decoded body over 4 times the raw
// limit, all with 502 RZ-RT-015; a spent buffer budget is 503 RZ-RT-004.
func TestStepBodyLimits(t *testing.T) {
	small := func() v1alpha1.CompositionStep {
		s := stepTo("m", "a", "/m")
		n := v1alpha1.ByteSize(64)
		s.MaxBodyBytes = &n
		return s
	}
	t.Run("declared over the cap", func(t *testing.T) {
		f := newFixture(t, "a")
		b := newBody(strings.Repeat(" ", 10))
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{"Content-Type": {"application/json"}, "Content-Length": {"65"}}, Body: b}
		})
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, small()))
		if _, err := f.forward(comp); codeOf(err) != CodeStepFailed {
			t.Errorf("Forward = %v, want RZ-RT-015", err)
		}
		if rest, _ := io.ReadAll(b.Reader); len(rest) != 10 || !b.closed.Load() {
			t.Errorf("body read %d bytes before the refusal (closed %v), want none", 10-len(rest), b.closed.Load())
		}
	})
	t.Run("undeclared over the cap", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return respWith(200, http.Header{"Content-Type": {"application/json"}}, `{"a":"`+strings.Repeat("x", 100)+`"}`)
		})
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, small()))
		if _, err := f.forward(comp); codeOf(err) != CodeStepFailed || !errors.Is(err, body.ErrTooLarge) {
			t.Errorf("Forward = %v, want RZ-RT-015 for a body over maxBodyBytes", err)
		}
	})
	t.Run("decoded over 4 times the cap", func(t *testing.T) {
		f := newFixture(t, "a")
		// 28 one-digit numbers in 63 bytes decode to far more than 256.
		dense := `{"a":[` + strings.Repeat("0,", 27) + `0]}`
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, dense) })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, small()))
		if _, err := f.forward(comp); codeOf(err) != CodeStepFailed || !errors.Is(err, jsonval.ErrTooLarge) {
			t.Errorf("Forward = %v, want RZ-RT-015 for a decoded body over its cap", err)
		}
	})
	t.Run("budget spent", func(t *testing.T) {
		f := newFixture(t, "a")
		f.e.budget = body.NewBudget(0, nil)
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"a":1}`) })
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
		if _, err := f.forward(comp); codeOf(err) != CodeBudget {
			t.Errorf("Forward = %v, want RZ-RT-004", err)
		}
	})
}

// TestMergedBodyCap checks that the merged body is capped by
// limits.maxResponseBodyBytes: a larger one fails the request with 502
// RZ-RT-015, as the merge rules cannot take it.
func TestMergedBodyCap(t *testing.T) {
	f := newFixture(t, "a", "b")
	f.limits.MaxResponseBodyBytes = 200
	// Each 7-byte step body fits its 100-byte share; the renamed members
	// make the merged body longer than 200 bytes.
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"k":1}`) })
	f.leg("b").fn = answer(func() *snapshot.UpstreamResponse { return jsonResp(200, `{"k":2}`) })
	a := stepTo("a", "a", "/a")
	a.Group, a.Rename = "g1", map[string]string{"k": strings.Repeat("a", 100)}
	b := stepTo("b", "b", "/b")
	b.Group, b.Rename = "g2", map[string]string{"k": strings.Repeat("b", 100)}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, a, b))
	resp, err := f.forward(comp)
	if resp != nil || codeOf(err) != CodeStepFailed || stepOf(err) != "" {
		t.Errorf("Forward = %v, %v; want RZ-RT-015 for the merged body", resp, err)
	}
	if got := f.budget.Used(); got != 0 {
		t.Errorf("budget used after the refusal = %d, want 0", got)
	}
}

// failingReader returns some bytes, then err.
type failingReader struct {
	data string
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// TestStepBodyReadFails checks a gated body that fails mid-read: an
// Upstream error, 502 RZ-UP-004, or 504 RZ-UP-003 once the Route deadline
// ended the read.
func TestStepBodyReadFails(t *testing.T) {
	t.Run("reset", func(t *testing.T) {
		f := newFixture(t, "a")
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(&failingReader{data: `{"a":`, err: io.ErrUnexpectedEOF})}
		})
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
		if _, err := f.forward(comp); codeOf(err) != "RZ-UP-004" {
			t.Errorf("Forward = %v, want RZ-UP-004", err)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		f := newFixture(t, "a")
		// A deadline long past: the read finds the context ended.
		ctx, cancel := context.WithDeadline(t.Context(), time.Unix(1, 0))
		defer cancel()
		f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
			return &snapshot.UpstreamResponse{Status: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(&failingReader{err: context.DeadlineExceeded})}
		})
		comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
		_, err := comp.Forward(ctx, f.outbound(exchange(http.MethodGet, "/v1", ""), ""))
		if codeOf(err) != "RZ-UP-003" || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("Forward = %v, want RZ-UP-003", err)
		}
	})
}

// TestResolveCollectsInOrder checks resolve directly: the matches of a
// "*" path in order, an element without the rest of the path matching
// nothing.
func TestResolveCollectsInOrder(t *testing.T) {
	f := newFixture(t, "a")
	f.leg("a").fn = answer(func() *snapshot.UpstreamResponse {
		return jsonResp(200, `{"o":[{"l":[{"s":1},{"s":2}]},{"x":0},{"l":[{"s":3}]}]}`)
	})
	s := stepTo("m", "a", "/m")
	s.Target, s.Group = "o.*.l.*", "all"
	s.Collection = ptr(true)
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, s))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"all":[{"s":1},{"s":2},{"s":3}]}` {
		t.Errorf("body = %s", got)
	}
}

// TestGeneratedBodyMerges checks that a leg Filter's 2xx response with a
// literal JSON body merges like an Upstream's.
func TestGeneratedBodyMerges(t *testing.T) {
	f := newFixture(t, "a")
	f.leg("a").fn = func(context.Context, upstream.Target) (*snapshot.UpstreamResponse, error) {
		return &snapshot.UpstreamResponse{Status: 200, Generated: &filter.Response{Status: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"cached":true}`)}}, nil
	}
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("m", "a", "/m")))
	resp, err := f.forward(comp)
	if err != nil {
		t.Fatal(err)
	}
	if got := read(t, resp); got != `{"cached":true}` {
		t.Errorf("body = %s", got)
	}
}
