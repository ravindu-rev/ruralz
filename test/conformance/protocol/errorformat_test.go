// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// errorFormatDoc is the document of the error response format.
const errorFormatDoc = "docs/architecture/03-data-plane.md#error-response-format"

// TestErrorFormat checks the error response format of
// docs/architecture/03-data-plane.md "Error response format" on every
// client protocol: a generated error is an RFC 9457 problem document
// (application/problem+json) with title, status, code and requestId, the
// request's trace ID, which is the trace ID of the client's traceparent
// when one is valid, sampled or not, and a fresh one per request
// otherwise, without a traceparent or after an invalid one; the body never
// echoes request content.
func TestErrorFormat(t *testing.T) {
	t.Parallel()
	t.Logf("rule: %s", errorFormatDoc)
	g := startGateway(t, bundleOptions{})
	marker := "echo-probe-" + newTraceID(t)
	decode := func(t *testing.T, r *response) problem {
		t.Helper()
		wantProblem(t, r, http.StatusNotFound, "RZ-RT-001")
		var doc problem
		if err := json.Unmarshal(r.Body, &doc); err != nil {
			t.Fatalf("problem document %q: %v", r.Body, err)
		}
		if doc.Title == "" {
			t.Errorf("problem document %+v without a title", doc)
		}
		return doc
	}
	eachProto(t, protos(), func(t *testing.T, p proto) {
		trace := newTraceID(t)
		r := g.do(t, p, request{Target: "/no-route/" + marker + "?q=" + marker, Header: [][2]string{
			{"Traceparent", "00-" + trace + "-00f067aa0ba902b7-00"}, {"X-Echo-Probe", marker},
		}})
		if doc := decode(t, r); doc.RequestID != trace {
			t.Errorf("requestId %s, want the traceparent's trace ID %s", doc.RequestID, trace)
		}
		if strings.Contains(string(r.Body), marker) {
			t.Errorf("the problem document echoes request content: %s", r.Body)
		}
		for name, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, marker) {
					t.Errorf("response field %s echoes request content: %q", name, v)
				}
			}
		}
		a, b := decode(t, g.do(t, p, request{Target: "/no-route"})), decode(t, g.do(t, p, request{Target: "/no-route"}))
		if a.RequestID == b.RequestID || strings.Trim(a.RequestID, "0") == "" {
			t.Errorf("requestIds %s and %s without a traceparent, want two fresh trace IDs", a.RequestID, b.RequestID)
		}
		// An all-zero trace ID makes a traceparent invalid (W3C Trace
		// Context), so the request starts a trace of its own.
		invalid := "00-" + strings.Repeat("0", 32) + "-00f067aa0ba902b7-01"
		c := decode(t, g.do(t, p, request{Target: "/no-route", Header: [][2]string{{"Traceparent", invalid}}}))
		if strings.Trim(c.RequestID, "0") == "" || c.RequestID == a.RequestID || c.RequestID == b.RequestID {
			t.Errorf("requestId %s after an invalid traceparent, want a fresh trace ID", c.RequestID)
		}
	})
}
