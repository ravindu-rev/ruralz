// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// redirectRouteDoc returns the Route name matching path prefix prefix
// with the redirect spec lines redirect.
func redirectRouteDoc(name, prefix, redirect string) string {
	return doc("Route", name, "  match:\n    path:\n      prefix: "+prefix+"\n  redirect:\n"+redirect)
}

// routeOf returns the compiled Route name of s.
func routeOf(t *testing.T, s *snapshot.Snapshot, name string) *snapshot.Route {
	t.Helper()
	for _, rt := range s.Routes {
		if rt != nil && rt.Name == name {
			return rt
		}
	}
	t.Fatalf("Route %s not compiled", name)
	return nil
}

// TestRedirectRoute checks a redirect Route's compiled form: its redirect,
// no forwarder, mirrors or body needs, and a Location built by its fields.
func TestRedirectRoute(t *testing.T) {
	h := newHarness(t)
	res := h.validate(
		gateway(""),
		redirectRouteDoc("moved", "/v1", "    status: 308\n    scheme: https\n    prefix: /v2\n    query: drop\n"),
	)
	c := h.mustCompile(res, nil)
	rt := routeOf(t, c.Snapshot, "moved")
	if rt.Redirect == nil || rt.Forward != nil || rt.Mirrors != nil || rt.Body != (snapshot.BodyNeeds{}) {
		t.Fatalf("Route = redirect %v forward %v mirrors %v body %+v, want the redirect alone", rt.Redirect, rt.Forward, rt.Mirrors, rt.Body)
	}
	loc, err := rt.Redirect.Location(&snapshot.RedirectRequest{Scheme: "http", Host: "shop.example:8080", Path: "/v1/orders", RawQuery: "a=1"})
	if rt.Redirect.Status() != 308 || err != nil || loc != "https://shop.example/v2/orders" {
		t.Errorf("redirect = %d %q, %v; want 308 https://shop.example/v2/orders", rt.Redirect.Status(), loc, err)
	}
}

// TestRedirectRouteDefaults checks a redirect without fields: 302 to the
// request's own URL, query kept.
func TestRedirectRouteDefaults(t *testing.T) {
	h := newHarness(t)
	res := h.validate(gateway(""), redirectRouteDoc("moved", "/v1", "    host: www.shop.example\n"))
	rt := routeOf(t, h.mustCompile(res, nil).Snapshot, "moved")
	loc, err := rt.Redirect.Location(&snapshot.RedirectRequest{Scheme: "https", Host: "shop.example", Path: "/v1/orders", RawQuery: "a=1"})
	if rt.Redirect.Status() != 302 || err != nil || loc != "https://www.shop.example/v1/orders?a=1" {
		t.Errorf("redirect = %d %q, %v; want 302 https://www.shop.example/v1/orders?a=1", rt.Redirect.Status(), loc, err)
	}
}

// TestServeCheckRefusesLocationExpression checks that the serve check
// refuses redirect.locationExpression, planned for M2, with RZ-CFG-040.
func TestServeCheckRefusesLocationExpression(t *testing.T) {
	h := newHarness(t)
	res := h.validate(gateway(""), redirectRouteDoc("moved", "/v1", "    locationExpression: '\"https://shop.example\" + request.path'\n"))
	found := false
	for _, l := range h.rejects(res) {
		if strings.Contains(l, "RZ-CFG-040") && strings.Contains(l, "spec.redirect.locationExpression") {
			found = true
		}
	}
	if !found {
		t.Errorf("findings = %q, want RZ-CFG-040 at spec.redirect.locationExpression", h.rejects(res))
	}
}

// mirrorsOfRoute returns the Upstream, percent and maxInFlight of each
// compiled mirror of rt.
func mirrorsOfRoute(rt *snapshot.Route) []string {
	var out []string
	for _, m := range rt.Mirrors {
		out = append(out, m.Upstream+" "+strconv.FormatUint(uint64(m.Percent), 10)+" "+strconv.FormatInt(m.MaxInFlight, 10))
	}
	return out
}

// TestRouteMirrors checks a Route's compiled mirrors: in Upstream name
// order with their percent and maxInFlight (100 and 1,000 by default),
// their metric handles and label sets, and their Upstreams compiled into
// the Upstream layer though no Route forwards to them.
func TestRouteMirrors(t *testing.T) {
	h := newHarness(t)
	res := h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		upstreamDoc("shadow", "127.0.0.1:9002", ""),
		upstreamDoc("audit", "127.0.0.1:9003", ""),
		routeDoc("a", "/a", "orders", "  mirrors:\n    - upstream: shadow\n      percent: 30\n      maxInFlight: 5\n    - upstream: audit\n"),
	)
	c := h.mustCompile(res, nil)
	rt := routeOf(t, c.Snapshot, "a")
	if got, want := mirrorsOfRoute(rt), []string{"audit 100 1000", "shadow 30 5"}; !slices.Equal(got, want) {
		t.Errorf("mirrors = %q, want %q", got, want)
	}
	for _, m := range rt.Mirrors {
		if m.InFlight == nil || m.Metrics == nil || m.InFlight.Load() != 0 {
			t.Errorf("mirror %s: in flight %v, metrics %v; want a new counter and handles", m.Upstream, m.InFlight, m.Metrics)
		}
	}
	for _, name := range []string{"orders", "shadow", "audit"} {
		if _, ok := c.Upstreams.Upstream(name); !ok {
			t.Errorf("Upstream %s not compiled", name)
		}
	}
	if legs, ok := c.Snapshot.MirrorLegs.(*upstream.Set); !ok || legs != c.Upstreams {
		t.Errorf("MirrorLegs = %T, want the snapshot's Upstream layer", c.Snapshot.MirrorLegs)
	}
	shape := h.meter.lastShape(t)
	if want := []emit.MirrorShape{{Route: "a", Upstream: "audit"}, {Route: "a", Upstream: "shadow"}}; !slices.Equal(shape.Mirrors, want) {
		t.Errorf("shape mirrors = %+v, want %+v", shape.Mirrors, want)
	}
}

// TestRouteMirrorsCarryInFlight checks that a mirror of the same Route and
// Upstream keeps the previous snapshot's in-flight counter, so maxInFlight
// bounds the Node across Hot Reloads, while a new mirror starts its own.
func TestRouteMirrorsCarryInFlight(t *testing.T) {
	h := newHarness(t)
	docs := []string{
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		upstreamDoc("shadow", "127.0.0.1:9002", ""),
		upstreamDoc("audit", "127.0.0.1:9003", ""),
	}
	first := h.mustCompile(h.validate(append(docs, routeDoc("a", "/a", "orders", "  mirrors:\n    - upstream: shadow\n"))...), nil)
	prev := routeOf(t, first.Snapshot, "a").Mirrors[0]
	prev.InFlight.Store(7)
	second := h.mustCompile(h.validate(append(docs, routeDoc("a", "/a", "orders", "  mirrors:\n    - upstream: shadow\n      percent: 50\n    - upstream: audit\n"))...), first.Snapshot)
	ms := routeOf(t, second.Snapshot, "a").Mirrors
	if len(ms) != 2 || ms[0].Upstream != "audit" || ms[1].Upstream != "shadow" {
		t.Fatalf("mirrors = %q, want audit and shadow", mirrorsOfRoute(routeOf(t, second.Snapshot, "a")))
	}
	if ms[1].InFlight != prev.InFlight || ms[1].Percent != 50 {
		t.Errorf("shadow's counter carried %t, percent %d; want the previous counter and 50", ms[1].InFlight == prev.InFlight, ms[1].Percent)
	}
	if ms[0].InFlight == prev.InFlight || ms[0].InFlight.Load() != 0 {
		t.Errorf("audit's counter = %d, shared %t; want a new one", ms[0].InFlight.Load(), ms[0].InFlight == prev.InFlight)
	}
}
