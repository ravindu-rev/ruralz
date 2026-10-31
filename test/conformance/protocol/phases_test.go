// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// phasesDoc is the document of the HTTP column of the Phase mapping.
const phasesDoc = "docs/architecture/07-multi-protocol.md#filter-chain-applicability-per-protocol"

// phasesBundle renders the Bundle of the Phase mapping: a Route whose
// effective Filter Chain subscribes every M1 Phase, to an Upstream that
// answers 503 to a case's first attempt and 200 to the retry, and OTLP
// trace export to sink. The Route timeout runs on the Node clock, which
// the test moves while the request runs, so it is set well beyond it.
func phasesBundle(o *origin, sink *otlpsink.Sink, apiKey string) []byte {
	return fmt.Appendf(nil, `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: phases
spec:
  admin:
    port: 9901
  listeners:
    - name: web
      protocol: http
      port: 8080
  telemetry:
    otlp:
      endpoint: "%s"
    traceSampling: 1
---
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: alice
spec:
  credentials:
    apiKeys:
      - name: primary
        hash: "%s"
  quotas:
    - name: daily
      unit: requests
      limit: 1000
      window: 24h
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-auth}
spec:
  type: auth.api-key
  config: {header: x-api-key}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-quota}
spec:
  type: quota
  config: {consumerQuota: daily}
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-route-headers}
spec:
  type: headers
  config:
    request:
      set: [{name: x-phase-request, value: route}]
    response:
      set: [{name: x-phase-response, value: route}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-body}
spec:
  type: transform.request
  config:
    set:
      - target: header
        name: x-phase-body
        valueExpression: '"route"'
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-up-headers}
spec:
  type: headers
  config:
    request:
      set: [{name: x-phase-attempt, value: upstream}]
    response:
      set: [{name: x-phase-attempt-response, value: upstream}]
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: ph-up-body}
spec:
  type: transform.response
  config:
    set:
      - target: header
        name: x-phase-upstream-body
        valueExpression: '"upstream"'
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: phases
spec:
  protocol: http
  endpoints:
    - address: "%s"
  retries:
    attempts: 1
  policies:
    - name: ph-up-headers
    - name: ph-up-body
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: phases
spec:
  match:
    path: {prefix: /phases/}
  timeout: 120s
  policies:
    - name: ph-auth
    - name: ph-quota
    - name: ph-route-headers
    - name: ph-body
  upstreams:
    - name: phases
`, sink.URL(), apiKeyHash(apiKey), o.addr(upH1))
}

// phaseCall is one ruralz.filter span as the Phase mapping reads it.
type phaseCall struct {
	policy, phase string
	// attempt is the ruralz.upstream attempt the span runs under, 0 for a
	// span of the request.
	attempt int
}

// String prints the call for a failure.
func (c phaseCall) String() string {
	if c.attempt > 0 {
		return c.policy + " " + c.phase + " #" + strconv.Itoa(c.attempt)
	}
	return c.policy + " " + c.phase
}

// spanWait bounds the wait for a trace's spans, a hang guard only.
const spanWait = 60 * time.Second

// phaseTraceSpans is how many spans the Phase mapping's trace holds at
// least: the server span, one ruralz.upstream span per attempt and the ten
// ruralz.filter spans.
const phaseTraceSpans = 13

// TestPhaseMapping checks the HTTP column of
// docs/architecture/07-multi-protocol.md "Filter Chain applicability per
// protocol", read from the Phase and order of the ruralz.filter spans of
// one sampled request whose first attempt is retried: onRequestHeaders and
// onRequestBody once, onUpstreamRequest and onUpstreamResponseHeaders per
// attempt, under each attempt's ruralz.upstream span, onUpstreamResponseBody
// once, for the final response, and onResponse once, in the fixed Phase
// order with Policies in Filter class order; onChunk never. onLog, which
// creates no span (docs/architecture/10-observability.md "Span model"),
// runs once: the request's one access record, which the Node writes in
// onLog. onRoute, which no M1 Policy type uses, joins with Plugins in M2.
func TestPhaseMapping(t *testing.T) {
	t.Parallel()
	t.Logf("rule: %s, column HTTP", phasesDoc)
	top := newTopology(t)
	o := startOrigin(t, top)
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	apiKey := top.Canary()
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: phasesBundle(o, sink, apiKey)})
	if err != nil {
		t.Fatal(err)
	}
	g := &gateway{top: top, node: nodes[0], origin: o, tls: top.ClientTLS()}
	ctx := guard(t, spanWait)
	trace := newTraceID(t)
	id := o.newCase()
	var r *response
	if err := advancing(ctx, top, func() error {
		r = g.do(t, h1, request{Target: "/phases/" + bFlaky, Header: withCase(id,
			[2]string{"X-Api-Key", apiKey}, [2]string{"Traceparent", "00-" + trace + "-00f067aa0ba902b7-01"})})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantOK(t, r)
	if n := len(o.requests(id)); n != 2 {
		t.Fatalf("the origin saw %d attempts, want the 503 and its retry", n)
	}
	// The Node exports the trace's spans in batches on its own clock.
	if err := advancing(ctx, top, func() error {
		_, err := sink.WaitSpans(ctx, phaseTraceSpans, func(s otlpsink.Span) bool { return s.TraceID() == trace })
		return err
	}); err != nil {
		t.Fatalf("spans of trace %s: %v\n%s", trace, err, sink.FormatTree(trace))
	}
	calls, attempts := phaseCalls(t, sink.Trace(trace))
	want := []phaseCall{
		{"ph-auth", "onRequestHeaders", 0},
		{"ph-quota", "onRequestHeaders", 0},
		{"ph-route-headers", "onRequestHeaders", 0},
		{"ph-body", "onRequestBody", 0},
		{"ph-up-headers", "onUpstreamRequest", 1},
		{"ph-up-headers", "onUpstreamResponseHeaders", 1},
		{"ph-up-headers", "onUpstreamRequest", 2},
		{"ph-up-headers", "onUpstreamResponseHeaders", 2},
		{"ph-up-body", "onUpstreamResponseBody", 2},
		{"ph-route-headers", "onResponse", 0},
	}
	if !slices.Equal(calls, want) {
		t.Errorf("ruralz.filter spans in order:\n  %v\nwant\n  %v\ntrace:\n%s", calls, want, sink.FormatTree(trace))
	}
	if !slices.Equal(attempts, []int{1, 2}) {
		t.Errorf("ruralz.upstream attempts %v, want 1 and its retry 2", attempts)
	}
	for _, c := range calls {
		if c.phase == "onChunk" || c.phase == "onLog" {
			t.Errorf("a ruralz.filter span for %v", c)
		}
	}
	re := regexp.MustCompile(`"msg":"access","trace_id":"` + trace + `"`)
	if _, err := g.node.Process().WaitOutput(guard(t, spanWait), re); err != nil {
		t.Fatalf("no access record of trace %s: %v", trace, err)
	}
	out, _ := g.node.Process().Output()
	if n := len(re.FindAll(out, -1)); n != 1 {
		t.Errorf("%d access records of trace %s, want one: onLog runs once", n, trace)
	}
}

// phaseCalls returns the ruralz.filter spans of a trace in start order as
// Policy, Phase and the attempt whose ruralz.upstream span parents them,
// and the attempts of the ruralz.upstream spans in start order.
func phaseCalls(t testing.TB, spans []otlpsink.Span) ([]phaseCall, []int) {
	t.Helper()
	attemptOf := map[string]int{}
	var attempts []int
	for _, s := range spans {
		if strings.HasPrefix(s.Name(), catalog.SpanUpstreamPrefix) {
			n, err := strconv.Atoi(s.AttrString(catalog.AttrAttempt))
			if err != nil {
				t.Fatalf("%s carries attempt %q", s.Name(), s.AttrString(catalog.AttrAttempt))
			}
			attemptOf[s.SpanID()] = n
			attempts = append(attempts, n)
		}
	}
	var calls []phaseCall
	for _, s := range spans {
		if policy, ok := strings.CutPrefix(s.Name(), catalog.SpanFilterPrefix); ok {
			calls = append(calls, phaseCall{policy: policy, phase: s.AttrString(catalog.AttrPhase), attempt: attemptOf[s.ParentSpanID()]})
		}
	}
	return calls, attempts
}
