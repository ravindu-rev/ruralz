// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"fmt"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// requestHandles are the handles one request records through.
type requestHandles struct {
	l        *emit.ListenerMetrics
	route    *emit.RouteMetrics
	up       *emit.UpstreamMetrics
	policies []*emit.PolicyMetrics
	state    *emit.StateMetrics
}

// bindRequest binds the handles one request records through: auth.jwt and
// ratelimit at the Gateway, ten headers Policies after them, one listener,
// Route and Upstream, and the Node's State Store calls.
func bindRequest(tb testing.TB, r *Registry) (requestHandles, emit.Binding) {
	tb.Helper()
	req := phases(phase.OnRequestHeaders)
	pols := []emit.PolicyShape{
		{Name: "jwt", Type: "auth.jwt", Phases: req, Codes: []string{"RZ-AUTH-001"}, Gateway: true},
		{Name: "rl", Type: "ratelimit", Phases: req, Gateway: true},
	}
	for i := range 10 {
		pols = append(pols, emit.PolicyShape{
			Name: fmt.Sprintf("f%02d", i), Type: "headers", Phases: phases(phase.OnRequestHeaders, phase.OnResponse), Gateway: true,
		})
	}
	pl, b := admitBind(tb, r, emit.Shape{Listeners: []string{"public"}, Routes: []string{"orders"}, Upstreams: []string{"orders-svc"}, Policies: pols})
	h := requestHandles{l: pl.Listener("public"), route: pl.Route("orders"), up: pl.Upstream("orders-svc"), state: &r.Node().State}
	for _, p := range pols {
		h.policies = append(h.policies, pl.Policy(p.Name))
	}
	return h, b
}

// request15 records the 15 metric operations of a request through auth.jwt
// and ratelimit.
func (h *requestHandles) request15(s emit.Stripe, v uint64) {
	h.l.Active.Add(s, 1)
	h.policies[0].Duration[phase.OnRequestHeaders].Record(s, v)
	h.policies[0].Auth.Allow(s)
	h.policies[1].Duration[phase.OnRequestHeaders].Record(s, v)
	h.policies[1].RateLimit[emit.RateLimitAllow].Add(s, 1)
	h.state.Calls[emit.StateOpGCRA][emit.StateResultOK].Add(s, 1)
	h.up.Attempts.Inc(s, 200, emit.ErrNone)
	h.up.AttemptDuration.Record(s, v*10)
	h.route.Requests.Inc(s, 200)
	h.route.Duration.Record(s, v*12)
	h.l.Requests.Inc(s, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	h.l.GatewayDuration.Record(s, v)
	h.l.RequestBody.Record(s, 512)
	h.l.ResponseBody.Record(s, 2048)
	h.l.Active.Add(s, -1)
}

// request40 records 40 operations: the 10 operations of the listener,
// Route and Upstream, plus three per Filter for ten Filters.
func (h *requestHandles) request40(s emit.Stripe, v uint64) {
	h.l.Active.Add(s, 1)
	h.up.Attempts.Inc(s, 200, emit.ErrNone)
	h.up.AttemptDuration.Record(s, v*10)
	h.route.Requests.Inc(s, 200)
	h.route.Duration.Record(s, v*12)
	h.l.Requests.Inc(s, emit.ProtoHTTP1, 200, emit.OriginUpstream)
	h.l.GatewayDuration.Record(s, v)
	h.l.RequestBody.Record(s, 512)
	h.l.ResponseBody.Record(s, 2048)
	h.l.Active.Add(s, -1)
	for _, p := range h.policies[2:12] {
		p.Duration[phase.OnRequestHeaders].Record(s, v)
		p.Duration[phase.OnResponse].Record(s, v)
		p.ShortCircuits[phase.OnRequestHeaders].Inc(s, 200)
	}
}

// TestHotPathZeroAllocs checks that hot-path recording allocates nothing,
// on the plain, exemplar, folded and code paths.
func TestHotPathZeroAllocs(t *testing.T) {
	r, _ := newTestRegistry(t, func(o *Options) { o.Limits = Limits{HistogramFamily: 1} })
	h, b := bindRequest(t, r)
	defer b.Release()
	ex := emit.Exemplar{TraceID: [16]byte{1}, SpanID: [8]byte{2}, Time: epoch()}
	h.policies[0].Auth.Deny(0, "RZ-AUTH-001") // first record installs nothing: pre-created
	r.Node().NodeResponses.Inc(0, "RZ-RT-005")
	cases := map[string]func(){
		"15 operations": func() { h.request15(1, 100_000) },
		"40 operations": func() { h.request40(2, 100_000) },
		"exemplar":      func() { h.route.Duration.RecordExemplar(3, 1_000_000, ex) },
		"folded":        func() { h.policies[5].Duration[phase.OnRequestHeaders].Record(0, 1) },
		"deny code":     func() { h.policies[0].Auth.Deny(0, "RZ-AUTH-001") },
		"node code":     func() { r.Node().NodeResponses.Inc(0, "RZ-RT-005") },
		"gauge set":     func() { h.up.HealthyEndpoints.Set(3) },
		"gateway timer": func() {
			var g emit.GatewayTimer
			g.Reset(epoch())
			g.Enter(epoch().Add(time.Microsecond))
			g.Leave(epoch().Add(2 * time.Microsecond))
			g.Observe(epoch().Add(3*time.Microsecond), 0, h.l.GatewayDuration, r.Node().GatewayDurationSkipped)
		},
	}
	for name, f := range cases {
		if n := testing.AllocsPerRun(1000, f); n != 0 {
			t.Errorf("%s: %v allocations per run, want 0", name, n)
		}
	}
}
