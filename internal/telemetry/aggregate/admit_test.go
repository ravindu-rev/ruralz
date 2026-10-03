// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Spec 09 req 45: listener families exist at 0 once the listener is
// admitted; req 55: they never fold.
func TestListenerFamiliesPrecreated_Req45(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{CounterFamily: 1, HistogramFamily: 1, Revision: 1} })
	pl, bind := admitBind(t, r, emit.Shape{Listeners: []string{"admin-b", "public-a"}})
	defer bind.Release()
	pts := collectNow(t, r, clk)
	for _, l := range []string{"admin-b", "public-a"} {
		want := map[string]int{
			catalog.HTTPListenerRequestsTotal:           2 * 5 * 3,
			catalog.HTTPGatewayDurationSeconds:          1,
			catalog.HTTPRequestBodyBytes:                1,
			catalog.HTTPResponseBodyBytes:               1,
			catalog.HTTPActiveRequests:                  1,
			catalog.ListenerOpenConnections:             2,
			catalog.ListenerConnectionsTotal:            6,
			catalog.ListenerTLSHandshakeDurationSeconds: 1,
		}
		for name, n := range want {
			got := 0
			for k, p := range familySeries(pts, name) {
				if strings.Contains(k, fmt.Sprintf("listener=%q", l)) {
					got++
					if p.value != 0 || p.count != 0 {
						t.Errorf("%s not at 0: %+v", k, p)
					}
				}
			}
			if got != n {
				t.Errorf("%s{listener=%q}: %d label sets, want %d", name, l, got, n)
			}
		}
	}
	for _, f := range r.fams {
		if f.role == roleListener && pl.folded[f.idx] != 0 {
			t.Errorf("listener family %s folded", f.cat.Name)
		}
	}
	m := pl.Listener("public-a")
	m.Requests.Inc(1, emit.ProtoHTTP2, 503, emit.OriginDependency)
	m.Requests.Inc(1, 7, 200, emit.OriginNode) // invalid protocol: dropped
	m.Requests.Inc(1, emit.ProtoHTTP1, 200, 9) // invalid origin: dropped
	m.Conns[emit.ProtoHTTP1][emit.ConnTLSFailure].Add(0, 2)
	m.OpenConns[emit.ProtoHTTP2].Add(0, 3)
	pts = collectNow(t, r, clk)
	for k, v := range map[string]float64{
		catalog.HTTPListenerRequestsTotal + `{listener="public-a",origin="dependency",protocol="http2",status_class="5xx"}`: 1,
		catalog.ListenerConnectionsTotal + `{listener="public-a",protocol="http1",result="tls_failure"}`:                    2,
		catalog.ListenerOpenConnections + `{listener="public-a",protocol="http2"}`:                                          3,
	} {
		if got := mustGet(t, pts, k).value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	var total float64
	for _, p := range familySeries(pts, catalog.HTTPListenerRequestsTotal) {
		total += p.value
	}
	if total != 1 {
		t.Errorf("listener requests total = %v, want 1 (invalid indexes dropped)", total)
	}
}

// Spec 09 req 43 and 45: each Policy records only its type's families,
// per Phase where the family has a phase label, pre-created at 0.
func TestPolicyFamiliesByType_Req45(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	all := phases(phase.OnRequestHeaders, phase.OnRequestBody, phase.OnUpstreamResponseHeaders, phase.OnResponse, phase.OnLog)
	shape := emit.Shape{Policies: []emit.PolicyShape{
		{Name: "jwt", Type: "auth.jwt", Phases: all, Codes: []string{"RZ-AUTH-003", "RZ-AUTH-001", "RZ-AUTH-003"}},
		{Name: "oauth", Type: "auth.upstream-oauth2", Phases: phases(phase.OnUpstreamRequest)},
		{Name: "ipallow", Type: "authz.ip", Phases: phases(phase.OnRequestHeaders), Codes: []string{"RZ-AUTH-013"}},
		{Name: "rl", Type: "ratelimit", Phases: phases(phase.OnRequestHeaders)},
		{Name: "q", Type: "quota", Phases: phases(phase.OnRequestHeaders, phase.OnLog)},
		{Name: "c", Type: "cache", Phases: phases(phase.OnRequestHeaders, phase.OnResponse)},
		{Name: "h", Type: "headers", Phases: phases(phase.OnResponse)},
		{Name: "none", Type: "cors"},
	}}
	pl, bind := admitBind(t, r, shape)
	defer bind.Release()
	pts := collectNow(t, r, clk)
	count := func(name, policy string) int {
		n := 0
		for k := range familySeries(pts, name) {
			if strings.Contains(k, fmt.Sprintf("policy=%q", policy)) {
				n++
			}
		}
		return n
	}
	tests := []struct {
		name, policy string
		want         int
	}{
		{catalog.FilterDurationSeconds, "jwt", 5},
		{catalog.FilterShortCircuitsTotal, "jwt", 2 * 5}, // onRequestHeaders, onRequestBody
		{catalog.FilterFailuresTotal, "jwt", 4 * 2},      // not onLog
		{catalog.AuthDecisionsTotal, "jwt", 1},           // allow; codes appear on first record
		{catalog.AuthJWKSAgeSeconds, "jwt", 1},
		{catalog.AuthUpstreamTokenAgeSeconds, "jwt", 0},
		{catalog.AuthDecisionsTotal, "oauth", 0},
		{catalog.AuthUpstreamTokenAgeSeconds, "oauth", 1},
		{catalog.AuthUpstreamRefreshFailuresTotal, "oauth", 1},
		{catalog.FilterShortCircuitsTotal, "oauth", 5},
		{catalog.AuthDecisionsTotal, "ipallow", 1},
		{catalog.RateLimitDecisionsTotal, "rl", 4},
		{catalog.RateLimitBucketEvictionsTotal, "rl", 1},
		{catalog.QuotaDecisionsTotal, "q", 4},
		{catalog.FilterFailuresTotal, "q", 2},
		{catalog.CacheStoreSkippedTotal, "c", 3},
		{catalog.FilterShortCircuitsTotal, "c", 5},
		{catalog.FilterDurationSeconds, "h", 1},
		{catalog.FilterShortCircuitsTotal, "h", 0},
		{catalog.RateLimitDecisionsTotal, "h", 0},
		{catalog.FilterDurationSeconds, "none", 0},
	}
	for _, tt := range tests {
		if got := count(tt.name, tt.policy); got != tt.want {
			t.Errorf("%s{policy=%q}: %d series, want %d", tt.name, tt.policy, got, tt.want)
		}
	}
	// Recording through the handles lands on the right label sets; Phases
	// a Policy does not run in are no-ops.
	jwt := pl.Policy("jwt")
	jwt.Duration[phase.OnRequestHeaders].Record(0, 50_000)
	jwt.Duration[phase.OnRoute].Record(0, 50_000)
	jwt.ShortCircuits[phase.OnRequestHeaders].Inc(2, 401)
	jwt.Failures[phase.OnResponse][emit.ModeClosed].Add(1, 1)
	jwt.Auth.Allow(3)
	jwt.Auth.Deny(0, "RZ-AUTH-003")
	jwt.Auth.Deny(0, "RZ-AUTH-003")
	jwt.Auth.Deny(0, "RZ-AUTH-002") // registered but undeclared: installed in the spare room
	jwt.Auth.Deny(0, "RZ-NOPE-001") // unregistered: dropped
	jwt.JWKSAge.Set(42)
	pl.Policy("rl").RateLimit[emit.RateLimitDenyGlobal].Add(0, 1)
	pl.Policy("q").Quota[emit.QuotaNoQuota].Add(0, 1)
	pl.Policy("c").StoreSkipped[emit.SkipMemory].Add(0, 1)
	pl.Policy("oauth").UpstreamRefreshes.Add(0, 1)
	pl.Policy("oauth").UpstreamTokenAge.Set(5)
	pl.Policy("rl").BucketEvictions.Add(0, 3)
	pts = collectNow(t, r, clk)
	for k, v := range map[string]float64{
		catalog.FilterShortCircuitsTotal + `{phase="onRequestHeaders",policy="jwt",status_class="4xx"}`: 1,
		catalog.FilterFailuresTotal + `{mode="closed",phase="onResponse",policy="jwt"}`:                 1,
		catalog.AuthDecisionsTotal + `{code="",policy="jwt",result="allow"}`:                            1,
		catalog.AuthDecisionsTotal + `{code="RZ-AUTH-003",policy="jwt",result="deny"}`:                  2,
		catalog.AuthDecisionsTotal + `{code="RZ-AUTH-002",policy="jwt",result="deny"}`:                  1,
		catalog.AuthJWKSAgeSeconds + `{policy="jwt"}`:                                                   42,
		catalog.RateLimitDecisionsTotal + `{policy="rl",result="deny_global"}`:                          1,
		catalog.QuotaDecisionsTotal + `{policy="q",result="no_quota"}`:                                  1,
		catalog.CacheStoreSkippedTotal + `{policy="c",reason="memory"}`:                                 1,
		catalog.AuthUpstreamRefreshFailuresTotal + `{policy="oauth"}`:                                   1,
		catalog.AuthUpstreamTokenAgeSeconds + `{policy="oauth"}`:                                        5,
		catalog.RateLimitBucketEvictionsTotal + `{policy="rl"}`:                                         3,
	} {
		if got := mustGet(t, pts, k).value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	if n := count(catalog.AuthDecisionsTotal, "jwt"); n != 3 {
		t.Errorf("auth decision series for jwt = %d, want 3 (allow and two codes)", n)
	}
	if p := mustGet(t, pts, catalog.FilterDurationSeconds+`{phase="onRequestHeaders",policy="jwt"}`); p.count != 1 {
		t.Errorf("duration count = %d", p.count)
	}
	if _, ok := pts[catalog.FilterDurationSeconds+`{phase="onRoute",policy="jwt"}`]; ok {
		t.Error("onRoute label set exists for a Policy without that Phase")
	}
}

// Spec 09 req 41, 45 and 55: beyond the family limit, resources fold into
// _overflow, whose label sets exist at 0 from the family's first
// admission; Plan.Folded and the folded gauge count them.
func TestFoldingIntoOverflow_Req55(t *testing.T) {
	r, clk := newTestRegistry(t, func(o *Options) { o.Limits = Limits{CounterFamily: 10, HistogramFamily: 1} })
	pl, bind := admitBind(t, r, emit.Shape{Routes: []string{"c", "a", "b"}})
	defer bind.Release()
	// Two Routes of five status classes fit in 10; the histogram family
	// admits one Route.
	if got := pl.Folded(catalog.HTTPRequestsTotal); got != 5 {
		t.Errorf("requests folded = %d, want 5", got)
	}
	if got := pl.Folded(catalog.HTTPRequestDurationSeconds); got != 2 {
		t.Errorf("duration folded = %d, want 2", got)
	}
	if got := pl.Folded("ruralz_bogus"); got != 0 {
		t.Errorf("unknown family folded = %d", got)
	}
	pts := collectNow(t, r, clk)
	for _, cls := range []string{"1xx", "2xx", "3xx", "4xx", "5xx"} {
		mustGet(t, pts, fmt.Sprintf(`%s{route="_overflow",status_class=%q}`, catalog.HTTPRequestsTotal, cls))
		mustGet(t, pts, fmt.Sprintf(`%s{route="a",status_class=%q}`, catalog.HTTPRequestsTotal, cls))
		mustGet(t, pts, fmt.Sprintf(`%s{route="b",status_class=%q}`, catalog.HTTPRequestsTotal, cls))
		if _, ok := pts[fmt.Sprintf(`%s{route="c",status_class=%q}`, catalog.HTTPRequestsTotal, cls)]; ok {
			t.Errorf("folded Route c exported")
		}
	}
	mustGet(t, pts, catalog.HTTPRequestDurationSeconds+`{route="_overflow"}`)
	if got := mustGet(t, pts, catalog.TelemetryFoldedLabelSets+`{instrument="ruralz_http_requests_total"}`).value; got != 5 {
		t.Errorf("folded gauge = %v, want 5", got)
	}
	pl.Route("c").Requests.Inc(0, 200)
	pl.Route("c").Duration.Record(0, 1)
	pl.Route("b").Duration.Record(0, 1)
	pts = collectNow(t, r, clk)
	if got := mustGet(t, pts, catalog.HTTPRequestsTotal+`{route="_overflow",status_class="2xx"}`).value; got != 1 {
		t.Errorf("overflow 2xx = %v, want 1", got)
	}
	if got := mustGet(t, pts, catalog.HTTPRequestDurationSeconds+`{route="_overflow"}`).count; got != 2 {
		t.Errorf("overflow duration count = %v, want 2", got)
	}
}

// Spec 09 req 43 and 49: attempts without a response count as 5xx with
// their error; error series are unsharded, error="none" ones striped.
func TestUpstreamAttempts_Req49(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, bind := admitBind(t, r, emit.Shape{Upstreams: []string{"svc"}})
	defer bind.Release()
	a := pl.Upstream("svc").Attempts
	a.Inc(0, 200, emit.ErrNone)
	a.Inc(1, 503, emit.ErrNone)
	a.Inc(2, 0, emit.ErrConnect)
	a.Inc(3, 0, emit.ErrTLS)
	a.Inc(3, 0, emit.ErrTLS)
	a.Inc(0, 200, 99) // outside the enumeration: dropped
	a.Inc(0, 200, -1)
	pts := familySeries(collectNow(t, r, clk), catalog.UpstreamAttemptsTotal)
	if len(pts) != 2*9 {
		t.Fatalf("attempt label sets = %d, want 9 for svc and 9 for _overflow", len(pts))
	}
	want := map[string]float64{
		`{error="none",status_class="2xx",upstream="svc"}`:    1,
		`{error="none",status_class="5xx",upstream="svc"}`:    1,
		`{error="connect",status_class="5xx",upstream="svc"}`: 1,
		`{error="tls",status_class="5xx",upstream="svc"}`:     2,
		`{error="timeout",status_class="5xx",upstream="svc"}`: 0,
	}
	for k, v := range want {
		if got := pts[catalog.UpstreamAttemptsTotal+k].value; got != v {
			t.Errorf("%s = %v, want %v", k, got, v)
		}
	}
	g := r.byName[catalog.UpstreamAttemptsTotal].groups[groupKey{res: "svc", phase: -1}]
	if g.attempts.none.c.stride != lineWords || g.attempts.errs.c.stride != 0 {
		t.Errorf("strides none %d errs %d, want one line and unsharded", g.attempts.none.c.stride, g.attempts.errs.c.stride)
	}
}

// Spec 09 req 55: the Revision limit bounds the admitted series.
func TestRevisionLimit_Req55(t *testing.T) {
	r, _ := newTestRegistry(t, func(o *Options) { o.Limits = Limits{Revision: 40} })
	units := decideShape(t, r, emit.Shape{Upstreams: []string{"u1", "u2"}})
	series := 0
	for _, u := range units {
		if u.admitted {
			series += u.series
		}
	}
	if series > 40 {
		t.Errorf("admitted %d series, limit 40", series)
	}
	folded := false
	for _, u := range units {
		folded = folded || !u.admitted
	}
	if !folded {
		t.Error("nothing folded under a 40-series Revision limit")
	}
}

// Spec 09 req 41: requests no Route matched record route="_unmatched".
func TestUnmatchedRoute_Req41(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, bind := admitBind(t, r, emit.Shape{})
	defer bind.Release()
	pl.Route(Unmatched).Requests.Inc(0, 404)
	pl.Route(Unmatched).Duration.Record(0, 1000)
	pl.Route(Unmatched).Cache[emit.CacheHit].Add(0, 1)
	pts := collectNow(t, r, clk)
	if got := mustGet(t, pts, catalog.HTTPRequestsTotal+`{route="_unmatched",status_class="4xx"}`).value; got != 1 {
		t.Errorf("unmatched 4xx = %v", got)
	}
	mustGet(t, pts, catalog.HTTPRequestDurationSeconds+`{route="_unmatched"}`)
	if got := familySeries(pts, catalog.CacheRequestsTotal); len(got) != 0 {
		t.Errorf("cache series for _unmatched: %v", got)
	}
}

// Unknown names record nothing; reserved, empty and duplicate names are
// rejected.
func TestAdmitNamesAndErrors(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, bind := admitBind(t, r, emit.Shape{Routes: []string{"a"}})
	defer bind.Release()
	pl.Route("zz").Requests.Inc(0, 200)
	pl.Listener("zz").Requests.Inc(0, 0, 200, 0)
	pl.Upstream("zz").Attempts.Inc(0, 200, 0)
	pl.Policy("zz").Auth.Deny(0, "RZ-AUTH-001")
	pts := collectNow(t, r, clk)
	for k, p := range pts {
		if strings.Contains(k, `"zz"`) || (p.value != 0 && strings.Contains(k, "_overflow")) {
			t.Errorf("unknown name recorded: %s", k)
		}
	}
	bad := []emit.Shape{
		{Routes: []string{""}},
		{Routes: []string{"_overflow"}},
		{Listeners: []string{"a", "a"}},
		{Upstreams: []string{"_x"}},
		{Policies: []emit.PolicyShape{{Name: "p"}, {Name: "p"}}},
	}
	for _, s := range bad {
		if _, err := r.Admit(s); !errors.Is(err, ErrShape) {
			t.Errorf("Admit(%+v) err = %v, want ErrShape", s, err)
		}
	}
}

// Spec 09 req 49: striping of Gateway-scoped Policies and of the first
// admitted Routes and Upstreams, within caps.
func TestStripingDecisions_Req49(t *testing.T) {
	// The Node-wide and _overflow/_unmatched histogram label sets striped
	// at start, plus room for one listener histogram.
	base, _ := newTestRegistry(t, nil)
	r, _ := newTestRegistry(t, func(o *Options) {
		o.Limits = Limits{
			StripedRoutes: 2, StripedUpstreams: 1, PolicyStripedSeries: 4, PolicyStripedHistograms: 1,
			ListenerStripedHistograms: base.nodeStripedHists + 1,
		}
	})
	shape := emit.Shape{
		Listeners: []string{"l1", "l2"},
		Routes:    []string{"r3", "r1", "r2"},
		Upstreams: []string{"u2", "u1"},
		Policies: []emit.PolicyShape{
			{Name: "g-rl", Type: "ratelimit", Phases: phases(phase.OnRequestHeaders), Gateway: true},
			{Name: "g-q", Type: "quota", Phases: phases(phase.OnRequestHeaders), Gateway: true},
			{Name: "route-rl", Type: "ratelimit", Phases: phases(phase.OnRequestHeaders)},
		},
	}
	_, bind := admitBind(t, r, shape)
	defer bind.Release()
	striped := map[string]bool{}
	for _, u := range decideShape(t, r, shape) {
		if u.admitted {
			striped[u.fam.cat.Name+"/"+u.res] = u.striped
		}
	}
	for key, want := range map[string]bool{
		catalog.HTTPRequestsTotal + "/r1":                   true,
		catalog.HTTPRequestsTotal + "/r2":                   true,
		catalog.HTTPRequestsTotal + "/r3":                   false,
		catalog.HTTPRequestDurationSeconds + "/r2":          true,
		catalog.CacheRequestsTotal + "/r1":                  false,
		catalog.UpstreamAttemptsTotal + "/u1":               true,
		catalog.UpstreamAttemptsTotal + "/u2":               false,
		catalog.UpstreamRetriesTotal + "/u1":                false,
		catalog.FilterDurationSeconds + "/g-q":              true,
		catalog.FilterDurationSeconds + "/g-rl":             false, // one striped histogram label set
		catalog.QuotaDecisionsTotal + "/g-q":                true,
		catalog.RateLimitDecisionsTotal + "/g-rl":           false, // 4 + 4 > 4
		catalog.RateLimitDecisionsTotal + "/route-rl":       false,
		catalog.FilterFailuresTotal + "/g-q":                false,
		catalog.HTTPGatewayDurationSeconds + "/l1":          true, // reserved histograms + 1 fit
		catalog.HTTPRequestBodyBytes + "/l1":                false,
		catalog.HTTPGatewayDurationSeconds + "/l2":          false,
		catalog.HTTPListenerRequestsTotal + "/l1":           true,
		catalog.ListenerConnectionsTotal + "/l1":            false,
		catalog.HTTPActiveRequests + "/l2":                  true,
		catalog.ListenerTLSHandshakeDurationSeconds + "/l1": false,
	} {
		got, ok := striped[key]
		if !ok || got != want {
			t.Errorf("%s striped = %v (admitted %v), want %v", key, got, ok, want)
		}
	}
	// A striped group really is striped.
	g := r.byName[catalog.HTTPRequestsTotal].groups[groupKey{res: "r1", phase: -1}]
	if g.cell.stride == 0 {
		t.Error("r1 requests cell not striped")
	}
}

// Spec 09 req 49: the _overflow and _unmatched groups of Striped families
// are hot label sets, striped within the listener and enumeration-only
// caps, which they share with the Node-wide groups; past the caps, or
// with one stripe, they stay unstriped.
func TestFixedGroupsStriped_Req49(t *testing.T) {
	shape := emit.Shape{Routes: []string{"a"}, Upstreams: []string{"u"}, Policies: gatewayPolicies()}
	fixed := func(r *Registry) map[string]*group {
		out := map[string]*group{}
		for _, name := range []string{catalog.HTTPRequestsTotal, catalog.HTTPRequestDurationSeconds} {
			out[name+"/"+Unmatched] = r.byName[name].unmatched
		}
		for _, name := range []string{
			catalog.HTTPRequestsTotal, catalog.HTTPRequestDurationSeconds, catalog.UpstreamAttemptsTotal,
			catalog.FilterDurationSeconds, catalog.FilterShortCircuitsTotal, catalog.RateLimitDecisionsTotal,
		} {
			f := r.byName[name]
			out[name+"/"+Overflow] = f.overflow[f.overflowPhases()[0]]
		}
		return out
	}
	for _, tt := range []struct {
		name    string
		mod     func(*Options)
		striped bool
	}{
		{"default caps", nil, true},
		{"caps used up by the Node-wide groups", func(o *Options) { o.Limits = Limits{ListenerStripedSeries: 1, ListenerStripedHistograms: 1} }, false},
		{"one stripe", func(o *Options) { o.Stripes = 1 }, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r, _ := newTestRegistry(t, tt.mod)
			_, b := admitBind(t, r, shape)
			defer b.Release()
			for key, g := range fixed(r) {
				if g == nil {
					t.Fatalf("%s not created", key)
				}
				if got := g.cell.stride != 0; got != tt.striped {
					t.Errorf("%s striped = %v, want %v", key, got, tt.striped)
				}
			}
			// Attempt error series stay unsharded even in _overflow.
			if a := r.byName[catalog.UpstreamAttemptsTotal].overflow[-1]; a.slots[numClasses].c.stride != 0 {
				t.Error("_overflow attempt error series striped")
			}
		})
	}
	// The reservation does not depend on admission history: a registry
	// that admitted nothing reserves the same counts.
	r1, _ := newTestRegistry(t, nil)
	r2, _ := newTestRegistry(t, nil)
	admitBind(t, r2, shape)
	if r1.nodeStripedSeries != r2.nodeStripedSeries || r1.nodeStripedHists != r2.nodeStripedHists {
		t.Errorf("reservations %d/%d and %d/%d differ", r1.nodeStripedSeries, r1.nodeStripedHists, r2.nodeStripedSeries, r2.nodeStripedHists)
	}
}

// randomShape returns a random Shape of up to n resources per kind.
func randomShape(rng *rand.Rand, n int) emit.Shape {
	names := func(prefix string, k int) []string {
		out := make([]string, 0, k)
		seen := map[string]bool{}
		for range k {
			name := fmt.Sprintf("%s%d", prefix, rng.IntN(4*n+1))
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
		return out
	}
	types := []string{"auth.jwt", "auth.api-key", "authz.cel", "auth.upstream-oauth2", "ratelimit", "quota", "cache", "headers", "cors"}
	codes := []string{"RZ-AUTH-001", "RZ-AUTH-002", "RZ-AUTH-003", "RZ-AUTH-010"}
	s := emit.Shape{
		Listeners: names("l", rng.IntN(3)),
		Routes:    names("r", rng.IntN(n+1)),
		Upstreams: names("u", rng.IntN(n+1)),
	}
	for _, name := range names("p", rng.IntN(n+1)) {
		p := emit.PolicyShape{
			Name:    name,
			Type:    types[rng.IntN(len(types))],
			Gateway: rng.IntN(2) == 0,
		}
		for ph := range phase.Count {
			if rng.IntN(2) == 0 {
				p.Phases = p.Phases.Add(ph)
			}
		}
		for _, c := range codes {
			if rng.IntN(2) == 0 {
				p.Codes = append(p.Codes, c)
			}
		}
		s.Policies = append(s.Policies, p)
	}
	return s
}

// permute returns s with every list shuffled.
func permute(rng *rand.Rand, s emit.Shape) emit.Shape {
	shuffle := func(in []string) []string {
		out := slices.Clone(in)
		rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
		return out
	}
	o := emit.Shape{Listeners: shuffle(s.Listeners), Routes: shuffle(s.Routes), Upstreams: shuffle(s.Upstreams)}
	for _, p := range s.Policies {
		p.Codes = shuffle(p.Codes)
		o.Policies = append(o.Policies, p)
	}
	rng.Shuffle(len(o.Policies), func(i, j int) { o.Policies[i], o.Policies[j] = o.Policies[j], o.Policies[i] })
	return o
}

// decideShape returns the admission decisions of s.
func decideShape(t testing.TB, r *Registry, s emit.Shape) []unit {
	t.Helper()
	n, err := normalize(s)
	if err != nil {
		t.Fatal(err)
	}
	return r.decide(n)
}

// decisionString renders admission decisions.
func decisionString(units []unit) string {
	var b strings.Builder
	for _, u := range units {
		fmt.Fprintf(&b, "%s|%s|%v|%v|%v|%d\n", u.fam.cat.Name, u.res, u.phases, u.admitted, u.striped, u.fan)
	}
	return b.String()
}

// planSignature renders what a Plan admitted (its groups in order) and
// how many label sets it folded per family.
func planSignature(p *Plan) string {
	var b strings.Builder
	for _, e := range p.entries {
		fmt.Fprintf(&b, "%s|%s|%d\n", e.g.fam.cat.Name, e.g.key.res, e.g.key.phase)
	}
	for i, n := range p.folded {
		if n > 0 {
			fmt.Fprintf(&b, "folded %s %d\n", p.r.fams[i].cat.Name, n)
		}
	}
	return b.String()
}

// checkPlanLimits verifies spec 09 req 55 on one admission: family and
// Revision limits hold, listener families never fold.
func checkPlanLimits(t testing.TB, r *Registry, units []unit) {
	t.Helper()
	fam := make([]int, len(r.fams))
	series, listenerSeries := 0, 0
	for _, u := range units {
		if u.kind == roleListener && !u.admitted {
			t.Fatalf("listener family %s folded", u.fam.cat.Name)
		}
		if !u.admitted {
			continue
		}
		series += u.series
		if u.kind == roleListener {
			listenerSeries += u.series
			continue
		}
		fam[u.fam.idx] += u.fan
		limit := r.limits.CounterFamily
		if u.fam.hist {
			limit = r.limits.HistogramFamily
		}
		if fam[u.fam.idx] > limit {
			t.Fatalf("%s admitted %d label sets, limit %d", u.fam.cat.Name, fam[u.fam.idx], limit)
		}
	}
	// Listener families are admitted first and never fold, so only they
	// may take a Revision past its limit.
	if series > max(r.limits.Revision, listenerSeries) {
		t.Fatalf("admitted %d series, limit %d", series, r.limits.Revision)
	}
}

// Spec 09 req 55 and test 21 (property): any permutation of a Shape's
// input order yields an identical Plan; limits are never exceeded;
// listener and enumeration families never fold.
func TestAdmissionDeterminism_Test21(t *testing.T) {
	rng := testRand(21, 55)
	for i := range 200 {
		limits := Limits{CounterFamily: 1 + rng.IntN(40), HistogramFamily: 1 + rng.IntN(10), Revision: 50 + rng.IntN(400)}
		r, _ := newTestRegistry(t, func(o *Options) { o.Limits = limits })
		s := randomShape(rng, 12)
		a, err := r.Admit(s)
		if err != nil {
			t.Fatalf("case %d: %v", i, err)
		}
		want := planSignature(a.(*Plan))
		wantDecisions := decisionString(decideShape(t, r, s))
		checkPlanLimits(t, r, decideShape(t, r, s))
		// A second registry (different history) and permutations agree.
		r2, _ := newTestRegistry(t, func(o *Options) { o.Limits = limits; o.Stripes = 8 })
		_, bind := admitBind(t, r2, randomShape(rng, 12))
		bind.Retire()
		for range 3 {
			ps := permute(rng, s)
			b, err := r2.Admit(ps)
			if err != nil {
				t.Fatal(err)
			}
			if got := planSignature(b.(*Plan)); got != want {
				t.Fatalf("case %d: permutation changed the plan:\n%s\nwant\n%s", i, got, want)
			}
			if got := decisionString(decideShape(t, r2, ps)); got != wantDecisions {
				t.Fatalf("case %d: permutation changed the decisions", i)
			}
		}
	}
}

// FuzzAdmission is spec 09 test 30: random shapes never panic and admit
// deterministically.
func FuzzAdmission(f *testing.F) {
	f.Add(uint64(1), uint8(5), uint16(10))
	f.Add(uint64(2), uint8(20), uint16(3))
	f.Add(uint64(3), uint8(0), uint16(0))
	f.Add(uint64(0xfeed), uint8(64), uint16(500))
	f.Fuzz(func(t *testing.T, seed uint64, n uint8, limit uint16) {
		rng := testRand(seed, uint64(n))
		lim := Limits{CounterFamily: int(limit%200) + 1, HistogramFamily: int(limit%50) + 1, Revision: int(limit) + 1}
		r, _ := newTestRegistry(t, func(o *Options) { o.Limits = lim })
		s := randomShape(rng, int(n%64))
		a, err := r.Admit(s)
		if err != nil {
			t.Fatal(err)
		}
		checkPlanLimits(t, r, decideShape(t, r, s))
		ps := permute(rng, s)
		b, err := r.Admit(ps)
		if err != nil {
			t.Fatal(err)
		}
		if planSignature(a.(*Plan)) != planSignature(b.(*Plan)) ||
			decisionString(decideShape(t, r, s)) != decisionString(decideShape(t, r, ps)) {
			t.Fatal("permutation changed the plan")
		}
		bind := r.Bind(b)
		bind.Release()
	})
}
