// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package filtertest

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for architecture section 2.13 (WP-01): the in-memory Exchange
// (SetIdentity second binding, PolicyState per R-39), Message.SetBody
// (spec 04 req 48: Content-Length recomputed, Content-Encoding and digests
// dropped), the Store fake's trip counting and answers, and ReplayFunc
// (R-43).

var (
	_ filter.Exchange    = (*Exchange)(nil)
	_ filter.Message     = (*Message)(nil)
	_ filter.Tee         = (*Tee)(nil)
	_ statestore.Store   = (*Store)(nil)
	_ filter.Replayer    = ReplayFunc(nil)
	_ filter.Filter      = policyStateFilter{}
	_ filter.Consumptive = (*consumptive)(nil)
)

func TestSetIdentitySecondBinding(t *testing.T) {
	var x Exchange
	if x.Identity() != nil {
		t.Fatal("a new Exchange has an identity")
	}
	first := &filter.Identity{Method: filter.MethodJWT, Policy: "jwt", Principal: "jwt:iss#sub"}
	if err := x.SetIdentity(first); err != nil {
		t.Fatalf("first SetIdentity = %v", err)
	}
	second := &filter.Identity{Method: filter.MethodAPIKey, Policy: "keys", Principal: "acme"}
	if err := x.SetIdentity(second); !errors.Is(err, filter.ErrSecondBinding) {
		t.Fatalf("second SetIdentity = %v, want ErrSecondBinding (401 RZ-AUTH-002)", err)
	}
	if x.Identity() != first {
		t.Fatal("a refused binding replaced the identity")
	}
}

// policyStateFilter counts its calls in the per-request slot (R-39).
type policyStateFilter struct{}

type counter struct{ calls int }

func (policyStateFilter) Handle(_ context.Context, _ phase.Phase, x filter.Exchange) filter.Result {
	slot := x.PolicyState()
	c, _ := (*slot).(*counter)
	if c == nil {
		c = &counter{}
		*slot = c
	}
	c.calls++
	return filter.Next()
}

func TestPolicyStateKeepsValue(t *testing.T) {
	var x Exchange
	if *x.PolicyState() != nil {
		t.Fatal("the slot is not nil at the first call")
	}
	f := policyStateFilter{}
	for range 3 {
		f.Handle(t.Context(), 0, &x)
	}
	if c, ok := x.State.(*counter); !ok || c.calls != 3 {
		t.Fatalf("PolicyState = %#v, want the same counter across 3 calls", x.State)
	}
	if a, b := x.PolicyState(), x.PolicyState(); a != b {
		t.Fatal("PolicyState returned different slots")
	}
}

func TestMessageSetBody(t *testing.T) {
	m := &Message{Hdr: http.Header{
		"Content-Encoding": {"gzip"}, "Content-Digest": {"sha-256=:x:"}, "Repr-Digest": {"sha-256=:y:"},
		"Content-Length": {"999"}, "Content-Type": {"application/json"},
	}, Raw: []byte("old"), DecodedValue: fakeValue{}}
	if err := m.SetBody(t.Context(), []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	h := m.Header()
	if h.Get("Content-Length") != "7" {
		t.Errorf("Content-Length = %q, want 7", h.Get("Content-Length"))
	}
	for _, k := range []string{"Content-Encoding", "Content-Digest", "Repr-Digest"} {
		if _, ok := h[k]; ok {
			t.Errorf("SetBody kept %s", k)
		}
	}
	if h.Get("Content-Type") != "application/json" {
		t.Error("SetBody dropped an unrelated header")
	}
	if b, err := m.Body(t.Context()); err != nil || string(b) != `{"a":1}` {
		t.Fatalf("Body = %q, %v", b, err)
	}
	if v, _ := m.Decoded(t.Context()); v != nil {
		t.Fatal("SetBody kept the decoded body")
	}
}

func TestMessageSetBodyBudget(t *testing.T) {
	b := &Budget{Limit: 8}
	m := &Message{Budget: b, Raw: []byte("keep")}
	if err := m.SetBody(t.Context(), []byte("12345")); err != nil {
		t.Fatal(err)
	}
	if err := m.SetBody(t.Context(), []byte("6789")); !errors.Is(err, filter.ErrBudget) {
		t.Fatalf("SetBody past the budget = %v, want ErrBudget", err)
	}
	if string(m.Raw) != "12345" || b.Reserved != 5 {
		t.Fatalf("a refused SetBody changed the body %q or reservation %d", m.Raw, b.Reserved)
	}
}

func TestMessageViews(t *testing.T) {
	m := &Message{Unavailable: true, Query: "a=1", Code: 502, Gen: true}
	if _, err := m.Body(t.Context()); !errors.Is(err, filter.ErrNotAvailable) {
		t.Fatalf("Body of an unavailable message = %v", err)
	}
	if m.Header() == nil {
		t.Fatal("Header of a zero message is nil")
	}
	m.Header().Set("X-A", "1")
	if m.Header().Get("X-A") != "1" {
		t.Fatal("Header is not live")
	}
	if m.RawQuery() != "a=1" {
		t.Fatal("RawQuery")
	}
	m.SetRawQuery("b=2")
	if m.RawQuery() != "b=2" || m.Status() != 502 || !m.Generated() {
		t.Fatalf("message = %+v", m)
	}
	var empty Message
	if b, err := empty.Body(t.Context()); err != nil || b != nil {
		t.Fatalf("empty Body = %v, %v", b, err)
	}
}

func TestBudget(t *testing.T) {
	var unlimited Budget
	if err := unlimited.Reserve(1 << 40); err != nil {
		t.Fatalf("Limit 0 refused: %v", err)
	}
	b := &Budget{Limit: 10}
	if err := b.Reserve(10); err != nil {
		t.Fatal(err)
	}
	if err := b.Reserve(1); !errors.Is(err, filter.ErrBudget) {
		t.Fatalf("Reserve past Limit = %v", err)
	}
	b.Release(4)
	if err := b.Reserve(4); err != nil || b.Reserved != 10 {
		t.Fatalf("Reserve after Release = %v, reserved %d", err, b.Reserved)
	}
	ex := &Budget{Exhausted: true}
	if err := ex.Reserve(1); !errors.Is(err, filter.ErrBudget) {
		t.Fatalf("Reserve on an exhausted budget = %v", err)
	}
}

func TestExchangeAccessors(t *testing.T) {
	start := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	route := &expr.Route{Name: "orders"}
	msg := &Message{}
	leg := &filter.Leg{Upstream: "inventory", Attempt: 1}
	tlsState := &filter.ConnTLS{ClientCertRequested: true}
	budget := statestore.NewRequestBudget(50*time.Millisecond, 3)
	rm := &emit.RouteMetrics{}
	x := &Exchange{
		ID: "0af7651916cd43dd8448eb211c80319c", Start: start, ListenerV: "public", RouteV: route,
		MethodV: "GET", SchemeV: "https", HostV: "api.example", PathV: "/orders/7",
		SourceV: filter.Source{Port: 443}, TLSV: tlsState, Msg: msg, LegV: leg,
		StateDeadline: &budget, FinalV: filter.Final{Status: 200, Committed: true},
		StripeV: 3, RouteMetricsV: rm,
	}
	checks := []struct {
		name string
		ok   bool
	}{
		{"RequestID", x.RequestID() == "0af7651916cd43dd8448eb211c80319c"},
		{"Now", x.Now().Equal(start)},
		{"Listener", x.Listener() == "public"},
		{"Route", x.Route() == route},
		{"Method", x.Method() == "GET"},
		{"Scheme", x.Scheme() == "https"},
		{"Host", x.Host() == "api.example"},
		{"Path", x.Path() == "/orders/7"},
		{"Source", x.Source().Port == 443},
		{"TLS", x.TLS() == tlsState},
		{"Message", x.Message() == msg},
		{"Leg", x.Leg() == leg},
		{"StateBudget", x.StateBudget() == &budget},
		{"Final", x.Final().Status == 200 && x.Final().Committed},
		{"Stripe", x.Stripe() == 3},
		{"RouteMetrics", x.RouteMetrics() == rm},
	}
	for _, c := range checks {
		if !c.ok {
			t.Errorf("%s returned the wrong value", c.name)
		}
	}
}

func TestExchangeZeroValue(t *testing.T) {
	var x Exchange
	if x.Header() == nil || x.Vars() == nil || x.RouteMetrics() == nil {
		t.Fatal("the zero Exchange returns nil views")
	}
	x.Header().Set("Authorization", "Bearer t")
	if x.Header().Get("Authorization") != "Bearer t" {
		t.Fatal("Header is not live")
	}
	vars, rm := x.Vars(), x.RouteMetrics()
	if x.Vars() != vars || x.RouteMetrics() != rm {
		t.Fatal("lazily built views are not kept")
	}
	if x.TeeResponse(1024) != nil {
		t.Fatal("TeeResponse without TeeV is not nil")
	}
	if x.Leg() != nil || x.TLS() != nil || x.StateBudget() != nil {
		t.Fatal("unset views are not nil")
	}

	// Recorded effects.
	if err := x.Reserve(100); err != nil {
		t.Fatal(err)
	}
	x.Release(40)
	if x.Budget.Reserved != 60 {
		t.Fatalf("Reserved = %d, want 60", x.Budget.Reserved)
	}
	resp := &filter.Response{Status: 200, Body: []byte("stale")}
	x.ReplaceResponse(resp)
	x.AddRateLimitField(filter.RateLimitField{Name: "rl.1", Q: 100, W: 60})
	x.AddRateLimitField(filter.RateLimitField{Name: "rl.2", Q: 10, W: 1, R: 3, T: 1, Known: true})
	x.Annotate("ruralz.state.op", slog.StringValue("gcra"))
	if x.Replaced != resp || len(x.RateLimits) != 2 || x.RateLimits[1].Name != "rl.2" {
		t.Fatalf("effects: replaced %v, rate limits %+v", x.Replaced, x.RateLimits)
	}
	if v := x.Annotations["ruralz.state.op"]; v.String() != "gcra" {
		t.Fatalf("Annotations = %v", x.Annotations)
	}

	x.TeeV = &Tee{Body: []byte("copy"), Complete: true}
	tee := x.TeeResponse(1024)
	if tee == nil {
		t.Fatal("TeeResponse with TeeV is nil")
	}
	if body, complete := tee.Result(); string(body) != "copy" || !complete {
		t.Fatalf("Tee.Result = %q, %v", body, complete)
	}
}

func TestStoreAnswersEveryCall(t *testing.T) {
	var s Store
	calls := []*statestore.Call{
		{Kind: statestore.OpGCRA, GCRA: statestore.GCRA{Denied: 5}},
		{Kind: statestore.OpQuota},
		{Kind: statestore.OpGCRA},
	}
	if n := s.Consume(t.Context(), nil, calls); n != 3 {
		t.Fatalf("Consume took %d calls, want 3", n)
	}
	for i, c := range calls {
		if !c.Done || c.Err != nil || !c.GCRA.Allowed || !c.Quota.Allowed || c.GCRA.Denied != -1 {
			t.Errorf("call %d = %+v, want answered and allowed", i, c)
		}
	}
	if s.Trips != 1 {
		t.Fatalf("Trips = %d after one Consume, want 1", s.Trips)
	}
	reads := []*statestore.Call{{Kind: statestore.OpCacheGet}, {Kind: statestore.OpCacheGet}}
	s.Read(t.Context(), nil, reads)
	if s.Trips != 2 || !reads[0].Done || !reads[1].Done {
		t.Fatalf("Read: trips %d, calls %+v", s.Trips, reads)
	}
	writes := []*statestore.Write{{Kind: statestore.OpRefund}, {Kind: statestore.OpCacheSet}}
	s.Write(t.Context(), writes)
	s.Write(t.Context(), []*statestore.Write{{Kind: statestore.OpCacheInvalidate}})
	if s.Writes != 2 || len(s.Written) != 3 || !writes[0].Applied || !s.Written[2].Applied {
		t.Fatalf("Write: batches %d, written %d", s.Writes, len(s.Written))
	}
	if s.Trips != 2 {
		t.Fatal("Write counted a round trip")
	}
}

func TestStoreScripted(t *testing.T) {
	s := &Store{
		// One slot per round trip: the fake takes one call at a time and
		// denies the second.
		ConsumeFn: func(calls []*statestore.Call) int {
			calls[0].Done = true
			calls[0].GCRA.Allowed = calls[0].GCRA.Policy != "deny"
			return 1
		},
		ReadFn: func(calls []*statestore.Call) {
			for _, c := range calls {
				c.Err = statestore.ErrTimeout
			}
		},
	}
	calls := []*statestore.Call{{GCRA: statestore.GCRA{Policy: "a"}}, {GCRA: statestore.GCRA{Policy: "deny"}}}
	for len(calls) > 0 {
		n := s.Consume(t.Context(), nil, calls)
		calls = calls[n:]
	}
	if s.Trips != 2 {
		t.Fatalf("Trips = %d, want one per Consume", s.Trips)
	}
	zero := &Store{ConsumeFn: func([]*statestore.Call) int { return 0 }}
	if n := zero.Consume(t.Context(), nil, []*statestore.Call{{}}); n != 1 {
		t.Fatalf("Consume returned %d, want at least 1", n)
	}
	reads := []*statestore.Call{{}}
	s.Read(t.Context(), nil, reads)
	if !errors.Is(reads[0].Err, statestore.ErrTimeout) || s.Trips != 3 {
		t.Fatalf("scripted Read: %+v, trips %d", reads[0], s.Trips)
	}
}

func TestStoreCapabilities(t *testing.T) {
	var s Store
	if !s.Supports(statestore.CapScripts) || s.Supports(statestore.CapVectorSets) {
		t.Fatal("default capabilities are not scripts only")
	}
	s.Caps = statestore.CapScripts | statestore.CapValkeySearch
	if !s.Supports(statestore.CapValkeySearch) || !s.Supports(statestore.CapScripts|statestore.CapValkeySearch) ||
		s.Supports(statestore.CapVectorSets) || s.Supports(statestore.CapScripts|statestore.CapVectorSets) {
		t.Fatal("Supports does not test every requested capability")
	}
	if !s.AdmitStoreBytes(statestore.CacheKey{}, 1<<20) || !s.RoundTrips() {
		t.Fatal("AdmitStoreBytes or RoundTrips")
	}
}

func TestStoreConcurrent(t *testing.T) {
	var s Store
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.Consume(context.Background(), nil, []*statestore.Call{{}})
				s.Read(context.Background(), nil, []*statestore.Call{{}})
				s.Write(context.Background(), []*statestore.Write{{}})
			}
		}()
	}
	wg.Wait()
	if s.Trips != 800 || s.Writes != 400 || len(s.Written) != 400 {
		t.Fatalf("Trips %d, Writes %d, Written %d", s.Trips, s.Writes, len(s.Written))
	}
}

func TestReplayFunc(t *testing.T) {
	var gotRoute, gotUpstream string
	var r filter.Replayer = ReplayFunc(func(_ context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
		gotRoute, gotUpstream = route, upstream
		if upstream == "gone" {
			return nil, filter.ErrNotAvailable
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(req.URL.Path))}, nil
	})
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://inventory/items/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := r.Replay(t.Context(), "orders", "inventory", req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if gotRoute != "orders" || gotUpstream != "inventory" || string(body) != "/items/7" {
		t.Fatalf("Replay passed %s/%s, body %q", gotRoute, gotUpstream, body)
	}
	gone, err := r.Replay(t.Context(), "orders", "gone", req)
	if gone != nil {
		_ = gone.Body.Close()
	}
	if !errors.Is(err, filter.ErrNotAvailable) {
		t.Fatalf("Replay of a gone Upstream = %v", err)
	}
}

// consumptive exercises the Store through a Consumptive Filter the way the
// executor batches it: Prepare fills the call, one Consume answers the run.
type consumptive struct{ policy string }

func (c *consumptive) Handle(context.Context, phase.Phase, filter.Exchange) filter.Result {
	return filter.Next()
}

func (c *consumptive) Prepare(_ context.Context, _ filter.Exchange, call *statestore.Call) (filter.Result, bool) {
	call.Kind = statestore.OpGCRA
	call.GCRA.Policy = c.policy
	return filter.Result{}, false
}

func (c *consumptive) Complete(_ context.Context, _ filter.Exchange, call *statestore.Call) filter.Result {
	if call.Err != nil {
		return filter.Undecided("", call.Err)
	}
	if !call.GCRA.Allowed {
		return filter.Deny(429, "RZ-RL-002", nil)
	}
	return filter.Next()
}

func (*consumptive) Undo(filter.Exchange) {}

func TestConsumptiveBatchOneTrip(t *testing.T) {
	var s Store
	var x Exchange
	members := []filter.Consumptive{&consumptive{"a"}, &consumptive{"b"}, &consumptive{"c"}}
	calls := make([]*statestore.Call, len(members))
	for i, m := range members {
		calls[i] = &statestore.Call{}
		if _, done := m.Prepare(t.Context(), &x, calls[i]); done {
			t.Fatal("Prepare decided locally")
		}
	}
	s.Consume(t.Context(), x.StateBudget(), calls)
	for i, m := range members {
		if r := m.Complete(t.Context(), &x, calls[i]); r.Outcome != filter.Continue {
			t.Fatalf("member %d = %+v", i, r)
		}
	}
	if s.Trips != 1 {
		t.Fatalf("a batch of 3 took %d round trips, want 1 (pack 8.7 rule 3)", s.Trips)
	}
}

type fakeValue struct{}

func (fakeValue) IsNull() bool                          { return false }
func (fakeValue) AppendBody(dst []byte) ([]byte, error) { return dst, nil }
func (fakeValue) Native() (any, error)                  { return map[string]any{}, nil }
