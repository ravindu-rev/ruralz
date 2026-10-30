// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// openBreaker runs one failing leg to the Upstream name of s, whose
// breaker opens after one failed leg.
func openBreaker(t *testing.T, s *upstream.Set, name string) {
	t.Helper()
	u, ok := s.Upstream(name)
	if !ok {
		t.Fatalf("Upstream %q not compiled", name)
	}
	r, err := u.Leg(t.Context(), outbound(nil), upstream.Target{})
	if err != nil {
		t.Fatalf("failing leg to %s: %v", name, err)
	}
	readAll(t, r)
	if _, err := u.Leg(t.Context(), outbound(nil), upstream.Target{}); codeOf(err) != "RZ-UP-005" {
		t.Fatalf("leg to %s after the failure = %v, want RZ-UP-005", name, err)
	}
}

// TestForwardRealRefusals checks the pick again over real Upstream legs:
// an open breaker (503 RZ-UP-005), a full bulkhead with no pending room
// (503 RZ-UP-006) and an empty Endpoint set (503 RZ-UP-008) each refuse
// the picked entry's leg before any attempt; the request then runs a leg
// to the other entry, which answers, and only that leg's Phases ran.
func TestForwardRealRefusals(t *testing.T) {
	t.Run("breaker open", func(t *testing.T) {
		failing := newServer(t, http.StatusServiceUnavailable, "down")
		other := newServer(t, http.StatusOK, "b")
		fx := newFixture(t)
		s := fx.activate(breakerAfterOne(newUpstream("a", failing.addr)), newUpstream("b", other.addr))
		openBreaker(t, s, "a")
		f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
		if err != nil {
			t.Fatal(err)
		}
		h := &hooks{}
		r, err := f.Forward(t.Context(), outbound(h))
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if body := readAll(t, r); body != "b" || r.Upstream != "b" || failing.hits.Load() != 1 {
			t.Errorf("Forward = %q from %s with %d requests to a, want b from b and none past the first", body, r.Upstream, failing.hits.Load())
		}
		if got := h.list(); !slices.Equal(got, []string{"b"}) {
			t.Errorf("leg Phases began for %v, want [b]", got)
		}
	})
	t.Run("bulkhead full", func(t *testing.T) {
		busy := newServer(t, http.StatusOK, "a")
		other := newServer(t, http.StatusOK, "b")
		fx := newFixture(t)
		a := newUpstream("a", busy.addr)
		a.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](0)}
		s := fx.activate(a, newUpstream("b", other.addr))
		f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
		if err != nil {
			t.Fatal(err)
		}
		// The first response's open body holds a's only slot.
		held, err := f.Forward(t.Context(), outbound(nil))
		if err != nil || held.Upstream != "a" {
			t.Fatalf("first Forward = %+v, %v; want a", held, err)
		}
		h := &hooks{}
		r, err := f.Forward(t.Context(), outbound(h))
		if err != nil {
			t.Fatalf("Forward with a's slot held: %v", err)
		}
		if body := readAll(t, r); body != "b" || busy.hits.Load() != 1 {
			t.Errorf("Forward with a's slot held = %q with %d requests to a, want b and 1", body, busy.hits.Load())
		}
		if got := h.list(); !slices.Equal(got, []string{"b"}) {
			t.Errorf("leg Phases began for %v, want [b]", got)
		}
		if body := readAll(t, held); body != "a" {
			t.Errorf("held response = %q, want a", body)
		}
		r, err = f.Forward(t.Context(), outbound(nil))
		if err != nil || readAll(t, r) != "a" {
			t.Errorf("Forward after the slot freed = %+v, %v; want a", r, err)
		}
	})
	t.Run("no Endpoint", func(t *testing.T) {
		other := newServer(t, http.StatusOK, "b")
		fx := newFixture(t)
		s := fx.activate(newUpstream("a"), newUpstream("b", other.addr))
		f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
		if err != nil {
			t.Fatal(err)
		}
		h := &hooks{}
		r, err := f.Forward(t.Context(), outbound(h))
		if err != nil {
			t.Fatalf("Forward: %v", err)
		}
		if body := readAll(t, r); body != "b" {
			t.Errorf("Forward = %q, want b", body)
		}
		if got := h.list(); !slices.Equal(got, []string{"b"}) {
			t.Errorf("leg Phases began for %v, want [b]", got)
		}
	})
}

// TestForwardRealLastRefusal checks that a request whose every entry is
// refused answers with the last refusal: the weight-0 entry's open
// breaker, tried after the empty set of the weighted one.
func TestForwardRealLastRefusal(t *testing.T) {
	failing := newServer(t, http.StatusServiceUnavailable, "down")
	fx := newFixture(t)
	s := fx.activate(newUpstream("a"), breakerAfterOne(newUpstream("b", failing.addr)))
	openBreaker(t, s, "b")
	f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
	if err != nil {
		t.Fatal(err)
	}
	h := &hooks{}
	r, err := f.Forward(t.Context(), outbound(h))
	var le *upstream.LegError
	if r != nil || !errors.As(err, &le) || le.Upstream != "b" || le.Attempts != 0 || codeOf(err) != "RZ-UP-005" {
		t.Fatalf("Forward = %+v, %v; want b's RZ-UP-005 without an attempt", r, err)
	}
	if got := h.list(); len(got) != 0 || failing.hits.Load() != 1 {
		t.Errorf("leg Phases began for %v with %d requests to b, want none past the first", got, failing.hits.Load())
	}
}

// TestForwardRealNoRepickAfterAttempt checks that a leg that sent an
// attempt is final however it ends: a refused connection is 502 RZ-UP-001,
// and with a retry on the same Endpoint 502 RZ-UP-007, and the other entry
// never runs.
func TestForwardRealNoRepickAfterAttempt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		retries int32
		code    string
	}{
		{"no retry", 0, "RZ-UP-001"},
		{"retry ran", 1, "RZ-UP-007"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := newServer(t, http.StatusOK, "b")
			fx := newFixture(t)
			a := newUpstream("a", refusedAddr)
			a.Spec.Retries.Attempts = ptr(tc.retries)
			s := fx.activate(a, newUpstream("b", other.addr))
			f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
			if err != nil {
				t.Fatal(err)
			}
			h := &hooks{}
			_, err = f.Forward(t.Context(), outbound(h))
			var le *upstream.LegError
			if !errors.As(err, &le) || le.Upstream != "a" || codeOf(err) != tc.code {
				t.Fatalf("Forward = %v, want a's %s", err, tc.code)
			}
			if other.hits.Load() != 0 || !slices.Equal(h.list(), []string{"a"}) {
				t.Errorf("b got %d requests and leg Phases began for %v, want none and [a]", other.hits.Load(), h.list())
			}
		})
	}
}

// TestForwardRealRouteDeadline checks that a Route deadline passed before
// the leg's first attempt answers 504 RZ-RT-007 at once, without trying
// another entry.
func TestForwardRealRouteDeadline(t *testing.T) {
	first := newServer(t, http.StatusOK, "a")
	other := newServer(t, http.StatusOK, "b")
	fx := newFixture(t)
	s := fx.activate(newUpstream("a", first.addr), newUpstream("b", other.addr))
	f, err := New("orders", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 0)}, s, noDraw{t})
	if err != nil {
		t.Fatal(err)
	}
	o := outbound(nil)
	o.Deadline = epoch()
	if _, err := f.Forward(t.Context(), o); codeOf(err) != "RZ-RT-007" || first.hits.Load()+other.hits.Load() != 0 {
		t.Errorf("Forward past the Route deadline = %v with %d requests, want RZ-RT-007 and none", err, first.hits.Load()+other.hits.Load())
	}
}

// TestNew checks the Forwarder New builds from Route.spec.upstreams: an
// absent weight is 1, so it outweighs an entry of weight 0; a Route
// without entries or naming an Upstream twice is RZ-CFG-005; an entry
// naming an Upstream the snapshot does not hold, or no Upstream layer at
// all, is an error.
func TestNew(t *testing.T) {
	srvA := newServer(t, http.StatusOK, "a")
	srvB := newServer(t, http.StatusOK, "b")
	fx := newFixture(t)
	s := fx.activate(newUpstream("a", srvA.addr), newUpstream("b", srvB.addr))
	for _, tc := range []struct {
		ups  []v1alpha1.RouteUpstream
		want string
	}{
		{[]v1alpha1.RouteUpstream{entry("a", -1), entry("b", 0)}, "a"},
		{[]v1alpha1.RouteUpstream{entry("a", 0), entry("b", -1)}, "b"},
		{[]v1alpha1.RouteUpstream{entry("a", -1)}, "a"},
	} {
		f, err := New("orders", tc.ups, s, noDraw{t})
		if err != nil {
			t.Fatalf("New(%+v): %v", tc.ups, err)
		}
		for range 5 {
			r, err := f.Forward(t.Context(), outbound(nil))
			if err != nil {
				t.Fatalf("Forward: %v", err)
			}
			if body := readAll(t, r); body != tc.want {
				t.Fatalf("Forward over %+v = %q, want %q", tc.ups, body, tc.want)
			}
		}
	}
	for _, tc := range []struct {
		name string
		ups  []v1alpha1.RouteUpstream
		set  *upstream.Set
		code string
		is   error
	}{
		{"no entry", nil, s, "RZ-CFG-005", nil},
		{"Upstream named twice", []v1alpha1.RouteUpstream{entry("a", 1), entry("b", 1), entry("a", 2)}, s, "RZ-CFG-005", nil},
		{"unknown Upstream", []v1alpha1.RouteUpstream{entry("a", 1), entry("missing", 1)}, s, "", errUnknownUpstream},
		{"no Upstream layer", []v1alpha1.RouteUpstream{entry("a", 1)}, nil, "", errUnknownUpstream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := New("orders", tc.ups, tc.set, nil)
			if f != nil || err == nil || codeOf(err) != tc.code || (tc.is != nil && !errors.Is(err, tc.is)) {
				t.Errorf("New = %v, %v; want code %q, cause %v", f, err, tc.code, tc.is)
			}
		})
	}
}
