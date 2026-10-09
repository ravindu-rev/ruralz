// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// addrs formats a Set's Endpoints as identity=addr,addr/weight lines.
func describe(s *Set) []string {
	out := make([]string, 0, s.Len())
	for _, e := range s.Endpoints {
		line := e.Identity + "="
		for i, a := range e.Addrs {
			if i > 0 {
				line += ","
			}
			line += a.String()
		}
		line += fmt.Sprintf("/%d host=%s", e.Weight, e.Host)
		out = append(out, line)
	}
	return out
}

func wantSet(t *testing.T, s *Set, want ...string) {
	t.Helper()
	if got := describe(s); !slices.Equal(got, want) {
		t.Fatalf("set = %q, want %q", got, want)
	}
}

// TestStaticLiteralsNeedNoRefresh: an IP literal is one Endpoint with no
// lookup (05 req 5); the set is sorted by identity (05 req 8).
func TestStaticLiteralsNeedNoRefresh(t *testing.T) {
	h := newHarness(t, Spec{Static: []StaticEndpoint{
		{Address: "10.0.0.2:80", Weight: 1},
		{Address: "[2001:db8::1]:80", Weight: 2},
		{Address: "10.0.0.1:80", Weight: 3},
		{Address: "10.0.0.1:80", Weight: 4},
	}}, nil)
	if h.src.Dynamic() {
		t.Fatal("IP literals reported dynamic")
	}
	wantSet(t, h.src.Current(),
		"10.0.0.1:80=10.0.0.1:80/7 host=",
		"10.0.0.2:80=10.0.0.2:80/1 host=",
		"[2001:db8::1]:80=[2001:db8::1]:80/2 host=")
	set, next := h.src.Refresh(t.Context())
	if next != 0 || set != h.src.Current() || set.Version != 1 {
		t.Fatalf("Refresh = %v, %v", set.Version, next)
	}
	h.src.Run(canceled(t), nil)
	if h.stale(t) {
		t.Fatal("stale without lookups")
	}
}

func canceled(t *testing.T) context.Context {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	return ctx
}

// TestStaticHostReResolution covers 05 req 5: a host name is one Endpoint
// named by the configured address, resolved at compile time and on every
// refresh, keeping the last good answer when a lookup fails (05 req 7:
// discovery_stale while it does).
func TestStaticHostReResolution(t *testing.T) {
	h := newHarness(t, Spec{Static: []StaticEndpoint{
		{Address: "api.internal:8443", Weight: 1},
		{Address: "api.internal:9443", Weight: 1},
		{Address: "10.0.0.9:80", Weight: 1},
	}}, nil)
	if !h.src.Dynamic() {
		t.Fatal("host name not dynamic")
	}
	// Before the compile-time refresh the host has no address yet.
	wantSet(t, h.src.Current(),
		"10.0.0.9:80=10.0.0.9:80/1 host=",
		"api.internal:8443=/1 host=api.internal",
		"api.internal:9443=/1 host=api.internal")

	h.res.setHost("api.internal", "10.1.0.2", "10.1.0.1", "2001:db8::5")
	set, next := h.src.Refresh(t.Context())
	wantSet(t, set,
		"10.0.0.9:80=10.0.0.9:80/1 host=",
		"api.internal:8443=10.1.0.2:8443,10.1.0.1:8443,[2001:db8::5]:8443/1 host=api.internal",
		"api.internal:9443=10.1.0.2:9443,10.1.0.1:9443,[2001:db8::5]:9443/1 host=api.internal")
	if next < 27*time.Second || next > 33*time.Second {
		t.Fatalf("next refresh %v outside 27-33 s", next)
	}
	if c := h.res.count("api.internal"); c != 1 {
		t.Fatalf("host looked up %d times per refresh, want 1", c)
	}
	if set.Status.LastRefresh != epoch() || set.Status.Stale || set.Status.Type != "static" {
		t.Fatalf("status %+v", set.Status)
	}
	v := set.Version

	// A failed lookup keeps the last good answer and raises the reason.
	h.res.failHost("api.internal", servfail("api.internal"))
	h.clk.Advance(30 * time.Second)
	set, next = h.src.Refresh(t.Context())
	if set.Version != v {
		t.Fatalf("version moved on a failed refresh")
	}
	if _, ok := set.Lookup("api.internal:8443"); !ok || len(set.Endpoints[1].Addrs) != 3 {
		t.Fatalf("last good answer lost: %q", describe(set))
	}
	if !set.Status.Stale || set.Status.Failures != 1 || set.Status.Err == nil || !h.stale(t) {
		t.Fatalf("status %+v, stale %v", set.Status, h.stale(t))
	}
	if next < BackoffMin || next > 2*BackoffMin {
		t.Fatalf("first backoff %v outside 1-2 s", next)
	}
	// NXDOMAIN for a static host keeps the last answer however often it
	// repeats: the Endpoint is configured, only its addresses are looked up.
	h.res.failHost("api.internal", nxdomain("api.internal"))
	for range 5 {
		set, _ = h.src.Refresh(t.Context())
	}
	if len(set.Endpoints[1].Addrs) != 3 || !h.stale(t) {
		t.Fatalf("NXDOMAIN dropped a static host's last answer: %q", describe(set))
	}

	// A new answer replaces the addresses and clears the reason.
	h.res.setHost("api.internal", "10.1.0.3")
	h.clk.Advance(time.Minute)
	set, _ = h.src.Refresh(t.Context())
	wantSet(t, set,
		"10.0.0.9:80=10.0.0.9:80/1 host=",
		"api.internal:8443=10.1.0.3:8443/1 host=api.internal",
		"api.internal:9443=10.1.0.3:9443/1 host=api.internal")
	if set.Version != v+1 || set.Status.Stale || set.Status.Failures != 0 || set.Status.Err != nil || h.stale(t) {
		t.Fatalf("recovery status %+v version %d", set.Status, set.Version)
	}
	if set.Status.LastRefresh != h.clk.Now() {
		t.Fatalf("LastRefresh %v, want %v", set.Status.LastRefresh, h.clk.Now())
	}
	if !set.SameMembers(set) {
		t.Fatal("SameMembers of itself")
	}
}

// TestStaticHostNeverResolved: a name that fails at compile time is still
// an Endpoint, without addresses, and the source is stale (05 reqs 5, 7).
func TestStaticHostNeverResolved(t *testing.T) {
	h := newHarness(t, Spec{Static: []StaticEndpoint{{Address: "gone.internal:80", Weight: 1}}}, nil)
	set, _ := h.src.Refresh(t.Context())
	wantSet(t, set, "gone.internal:80=/1 host=gone.internal")
	if !h.stale(t) || !notFound(set.Status.Err) {
		t.Fatalf("status %+v", set.Status)
	}
}

// TestDNSNumericPort covers 05 req 6: A and AAAA lookups of the service,
// each IP one Endpoint ip:port of weight 1, Host the service name.
func TestDNSNumericPort(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "orders.svc.", Port: 8080}}, nil)
	if h.src.Current().Len() != 0 {
		t.Fatal("DNS source starts with Endpoints")
	}
	h.res.setHost("orders.svc.", "10.0.0.3", "::ffff:10.0.0.1", "2001:db8::2", "10.0.0.3", "fe80::1%eth0")
	set, _ := h.src.Refresh(t.Context())
	wantSet(t, set,
		"10.0.0.1:8080=10.0.0.1:8080/1 host=orders.svc",
		"10.0.0.3:8080=10.0.0.3:8080/1 host=orders.svc",
		"[2001:db8::2]:8080=[2001:db8::2]:8080/1 host=orders.svc",
		"[fe80::1%eth0]:8080=[fe80::1%eth0]:8080/1 host=orders.svc")
	if set.Version != 1 || set.Status.Type != "dns" {
		t.Fatalf("version %d type %q", set.Version, set.Status.Type)
	}
	// The same answer in another order changes nothing (05 req 8).
	h.res.setHost("orders.svc.", "fe80::1%eth0", "2001:db8::2", "10.0.0.1", "10.0.0.3")
	set2, _ := h.src.Refresh(t.Context())
	if set2.Version != 1 || !slices.Equal(set2.Identities(), set.Identities()) {
		t.Fatalf("reordered answer moved the set: %d %q", set2.Version, set2.Identities())
	}
}

// TestDNSSRV covers 05 req 6: SRV _<port>._tcp.<service>, lowest-priority
// group, SRV weights, each target resolved to IPs like a static host name.
func TestDNSSRV(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "http"}}, nil)
	name := "_http._tcp.orders.svc"
	h.res.setSRV(name,
		srv("b.orders.svc.", 8080, 10, 30),
		srv("a.orders.svc.", 8081, 10, 10),
		srv("backup.orders.svc.", 8080, 20, 50),
		srv("z.orders.svc.", 8080, 10, 0))
	h.res.setHost("a.orders.svc", "10.0.1.1")
	h.res.setHost("b.orders.svc", "10.0.1.2", "10.0.1.3")
	set, next := h.src.Refresh(t.Context())
	wantSet(t, set,
		"a.orders.svc:8081=10.0.1.1:8081/10 host=a.orders.svc",
		"b.orders.svc:8080=10.0.1.2:8080,10.0.1.3:8080/30 host=b.orders.svc")
	if next < 27*time.Second || next > 33*time.Second || h.stale(t) {
		t.Fatalf("next %v stale %v", next, h.stale(t))
	}

	// A target lookup failure keeps that target's last good addresses; a new
	// target that does not resolve is left out; the refresh counts as failed.
	h.res.failHost("a.orders.svc", servfail("a.orders.svc"))
	h.res.setSRV(name,
		srv("a.orders.svc.", 8081, 10, 10),
		srv("b.orders.svc.", 8080, 10, 30),
		srv("c.orders.svc.", 8080, 10, 5))
	set, next = h.src.Refresh(t.Context())
	wantSet(t, set,
		"a.orders.svc:8081=10.0.1.1:8081/10 host=a.orders.svc",
		"b.orders.svc:8080=10.0.1.2:8080,10.0.1.3:8080/30 host=b.orders.svc")
	if !h.stale(t) || next > 2*time.Second || set.Status.Failures != 1 {
		t.Fatalf("partial failure: stale %v next %v status %+v", h.stale(t), next, set.Status)
	}
	h.res.setHost("c.orders.svc", "10.0.1.4")
	h.res.setHost("a.orders.svc", "10.0.1.1")
	set, _ = h.src.Refresh(t.Context())
	wantSet(t, set,
		"a.orders.svc:8081=10.0.1.1:8081/10 host=a.orders.svc",
		"b.orders.svc:8080=10.0.1.2:8080,10.0.1.3:8080/30 host=b.orders.svc",
		"c.orders.svc:8080=10.0.1.4:8080/5 host=c.orders.svc")
	if h.stale(t) {
		t.Fatal("stale after a good answer")
	}

	// Every target failing without a last answer fails the refresh and
	// keeps the whole last set.
	v := set.Version
	h.res.setSRV(name, srv("d.orders.svc.", 8080, 0, 1))
	set, _ = h.src.Refresh(t.Context())
	if set.Version != v || set.Len() != 3 || !h.stale(t) {
		t.Fatalf("unresolvable targets replaced the set: %q", describe(set))
	}

	// IP literal targets need no lookup.
	h.res.setSRV(name, srv("10.9.9.9", 9000, 0, 1))
	set, _ = h.src.Refresh(t.Context())
	wantSet(t, set, "10.9.9.9:9000=10.9.9.9:9000/1 host=10.9.9.9")
}

// TestRefreshInterval: after a good answer the next refresh is due 30 s
// ±10% (05 req 6), spread over the whole window.
func TestRefreshInterval(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	h.res.setHost("s", "10.0.0.1")
	lo, hi := time.Hour, time.Duration(0)
	for range 2000 {
		_, next := h.src.Refresh(t.Context())
		if next < 27*time.Second || next > 33*time.Second {
			t.Fatalf("next %v outside 27-33 s", next)
		}
		lo, hi = min(lo, next), max(hi, next)
	}
	if lo > 27*time.Second+100*time.Millisecond || hi < 33*time.Second-100*time.Millisecond {
		t.Fatalf("jitter spread %v-%v does not cover the window", lo, hi)
	}
	if got := uniform(fixedSource(0), 6*time.Second); got != 0 {
		t.Fatalf("uniform low end %v", got)
	}
	if got := uniform(fixedSource(^uint64(0)), 6*time.Second); got != 6*time.Second {
		t.Fatalf("uniform high end %v", got)
	}
	if uniform(fixedSource(5), 0) != 0 {
		t.Fatal("uniform of 0")
	}
}

// TestBackoffBounds: the n-th consecutive failure waits a full-jitter
// delay from 1 s to min(60 s, 2^n s) (05 req 7).
func TestBackoffBounds(t *testing.T) {
	src := newRand(7)
	for n := 0; n <= 12; n++ {
		ceiling := BackoffMax
		if n < 6 {
			ceiling = min(BackoffMax, time.Second<<max(n, 1))
		}
		hi := time.Duration(0)
		for range 500 {
			d := Backoff(src, n)
			if d < BackoffMin || d > ceiling {
				t.Fatalf("n=%d: delay %v outside [1s, %v]", n, d, ceiling)
			}
			hi = max(hi, d)
		}
		if hi < ceiling*9/10 {
			t.Fatalf("n=%d: highest delay %v never approaches %v", n, hi, ceiling)
		}
	}
	if Backoff(fixedSource(^uint64(0)), 100) != BackoffMax || Backoff(fixedSource(0), 3) != BackoffMin {
		t.Fatal("backoff ends")
	}
}

// TestFailureKeepsLastSet covers 05 req 7: a lookup failure keeps the last
// set, raises discovery_stale on the Upstream and Node gauges, backs off,
// and never empties the set however long it lasts.
func TestFailureKeepsLastSet(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	h.res.setHost("s", "10.0.0.1", "10.0.0.2")
	good, _ := h.src.Refresh(t.Context())
	h.res.failHost("s", servfail("s"))
	for n := 1; n <= 10; n++ {
		set, next := h.src.Refresh(t.Context())
		if !slices.Equal(set.Identities(), good.Identities()) || set.Version != good.Version {
			t.Fatalf("failure %d changed the set", n)
		}
		if !set.Status.Stale || set.Status.Failures != n || set.Status.Err == nil {
			t.Fatalf("failure %d status %+v", n, set.Status)
		}
		if next < BackoffMin || next > BackoffMax {
			t.Fatalf("failure %d backoff %v", n, next)
		}
		if set.Status.LastRefresh != good.Status.LastRefresh {
			t.Fatal("LastRefresh moved on failure")
		}
	}
	if !h.stale(t) {
		t.Fatal("discovery_stale not raised")
	}
	// The reason is raised once, under the Source's own holder named
	// after the Upstream.
	events := h.status.list()
	if len(events) != 1 || events[0] != (statusEvent{catalog.ReasonDiscoveryStale, h.src.holder, true}) {
		t.Fatalf("events %+v", events)
	}
	if !strings.HasPrefix(h.src.holder, "orders#") {
		t.Fatalf("holder %q", h.src.holder)
	}
	h.res.setHost("s", "10.0.0.1", "10.0.0.2")
	set, next := h.src.Refresh(t.Context())
	if set.Status.Stale || h.stale(t) || next < 27*time.Second {
		t.Fatal("good answer did not clear discovery_stale")
	}
}

// TestNotFoundEmptiesAfterThree covers 05 req 7: NXDOMAIN or an empty answer
// is a failure until 3 consecutive refreshes repeat it; then the set
// empties (selection returns RZ-UP-008) and the source is no longer stale.
// Any other outcome in between restarts the count.
func TestNotFoundEmptiesAfterThree(t *testing.T) {
	cases := []struct {
		name string
		spec Spec
		fail func(r *fakeResolver)
		good func(r *fakeResolver)
	}{
		{
			name: "A NXDOMAIN",
			spec: Spec{DNS: &DNSSpec{Service: "s", Port: 80}},
			fail: func(r *fakeResolver) { r.failHost("s", nxdomain("s")) },
			good: func(r *fakeResolver) { r.setHost("s", "10.0.0.1") },
		},
		{
			name: "A empty answer",
			spec: Spec{DNS: &DNSSpec{Service: "s", Port: 80}},
			fail: func(r *fakeResolver) { r.setHost("s") },
			good: func(r *fakeResolver) { r.setHost("s", "10.0.0.1") },
		},
		{
			name: "SRV NXDOMAIN",
			spec: Spec{DNS: &DNSSpec{Service: "s", PortName: "http"}},
			fail: func(r *fakeResolver) { r.failSRV("_http._tcp.s", nxdomain("_http._tcp.s")) },
			good: func(r *fakeResolver) {
				r.setSRV("_http._tcp.s", srv("t.s.", 80, 0, 1))
				r.setHost("t.s", "10.0.0.1")
			},
		},
		{
			name: "SRV empty answer",
			spec: Spec{DNS: &DNSSpec{Service: "s", PortName: "http"}},
			fail: func(r *fakeResolver) { r.setSRV("_http._tcp.s") },
			good: func(r *fakeResolver) {
				r.setSRV("_http._tcp.s", srv("t.s.", 80, 0, 1))
				r.setHost("t.s", "10.0.0.1")
			},
		},
		{
			name: "SRV only unusable targets",
			spec: Spec{DNS: &DNSSpec{Service: "s", PortName: "http"}},
			fail: func(r *fakeResolver) { r.setSRV("_http._tcp.s", srv(".", 80, 0, 1)) },
			good: func(r *fakeResolver) {
				r.setSRV("_http._tcp.s", srv("t.s.", 80, 0, 1))
				r.setHost("t.s", "10.0.0.1")
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, tc.spec, nil)
			tc.good(h.res)
			if set, _ := h.src.Refresh(t.Context()); set.Len() != 1 {
				t.Fatalf("good answer: %q", describe(set))
			}
			tc.fail(h.res)
			for n := 1; n < NotFoundLimit; n++ {
				set, _ := h.src.Refresh(t.Context())
				if set.Len() != 1 || !h.stale(t) {
					t.Fatalf("not-found %d emptied the set or kept it fresh", n)
				}
			}
			// A different failure restarts the streak.
			if tc.spec.DNS.PortName == "" {
				h.res.failHost("s", servfail("s"))
			} else {
				h.res.failSRV("_http._tcp.s", servfail("s"))
			}
			h.src.Refresh(t.Context())
			tc.fail(h.res)
			for n := 1; n < NotFoundLimit; n++ {
				if set, _ := h.src.Refresh(t.Context()); set.Len() != 1 {
					t.Fatalf("streak not restarted: emptied after %d", n)
				}
			}
			set, next := h.src.Refresh(t.Context())
			if set.Len() != 0 || set.Status.Stale || h.stale(t) || set.Status.Err != nil {
				t.Fatalf("third repeat: %q status %+v", describe(set), set.Status)
			}
			// While the accepted answer keeps the set empty, retries stay
			// on the failure backoff (05 req 7), its ceiling growing with
			// the streak, so a name that comes back is found within
			// seconds rather than after the 30 s schedule.
			if next < BackoffMin || next > 8*time.Second {
				t.Fatalf("accepted empty answer schedules %v, want 1-8 s", next)
			}
			// Further repeats keep it empty and fresh, still on backoff.
			for n := NotFoundLimit + 1; n <= NotFoundLimit+5; n++ {
				set, next = h.src.Refresh(t.Context())
				if set.Len() != 0 || h.stale(t) {
					t.Fatalf("repeat %d changed state", n)
				}
				if ceiling := min(BackoffMax, BackoffMin<<n); next < BackoffMin || next > ceiling {
					t.Fatalf("repeat %d schedules %v, want 1 s to %v", n, next, ceiling)
				}
			}
			tc.good(h.res)
			if set, next = h.src.Refresh(t.Context()); set.Len() != 1 || h.stale(t) {
				t.Fatalf("recovery: %q", describe(set))
			}
			if next < 27*time.Second || next > 33*time.Second {
				t.Fatalf("non-empty answer schedules %v, want 27-33 s", next)
			}
		})
	}
}

// TestPreviousSeeding: a Source replacing another after a Hot Reload keeps
// the last good answers of the names both resolve (05 reqs 2 and 5).
func TestPreviousSeeding(t *testing.T) {
	old := newHarness(t, Spec{Static: []StaticEndpoint{{Address: "api.internal:80", Weight: 1}}}, nil)
	old.res.setHost("api.internal", "10.0.0.1", "10.0.0.2")
	prev, _ := old.src.Refresh(t.Context())

	// A changed static list: the host keeps its addresses while DNS is down.
	h := newHarness(t, Spec{Static: []StaticEndpoint{
		{Address: "api.internal:80", Weight: 1},
		{Address: "api.internal:81", Weight: 2},
	}}, func(o *Options) { o.Previous = prev })
	wantSet(t, h.src.Current(),
		"api.internal:80=10.0.0.1:80,10.0.0.2:80/1 host=api.internal",
		"api.internal:81=10.0.0.1:81,10.0.0.2:81/2 host=api.internal")
	h.res.failHost("api.internal", servfail("api.internal"))
	set, _ := h.src.Refresh(t.Context())
	if len(set.Endpoints[0].Addrs) != 2 || !h.stale(t) {
		t.Fatalf("seeded answer lost: %q", describe(set))
	}

	// An unchanged DNS source starts from the previous Endpoints.
	d1 := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	d1.res.setHost("s", "10.0.0.7")
	p1, _ := d1.src.Refresh(t.Context())
	d2 := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, func(o *Options) { o.Previous = p1 })
	wantSet(t, d2.src.Current(), "10.0.0.7:80=10.0.0.7:80/1 host=s")
	// A different DNS source does not.
	d3 := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 81}}, func(o *Options) { o.Previous = p1 })
	if d3.src.Current().Len() != 0 {
		t.Fatal("changed DNS source inherited Endpoints")
	}
	// SRV targets keep their last good addresses across a reload.
	s1 := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", PortName: "http"}}, nil)
	s1.res.setSRV("_http._tcp.s", srv("t.s.", 80, 0, 1))
	s1.res.setHost("t.s", "10.0.0.8")
	ps, _ := s1.src.Refresh(t.Context())
	s2 := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", PortName: "https"}}, func(o *Options) { o.Previous = ps })
	s2.res.setSRV("_https._tcp.s", srv("t.s.", 443, 0, 1))
	s2.res.failHost("t.s", servfail("t.s"))
	set, _ = s2.src.Refresh(t.Context())
	wantSet(t, set, "t.s:443=10.0.0.8:443/1 host=t.s")
}

// TestSetMetricsAndClose: a new snapshot's gauge shows the current state;
// Close clears the Node reason the Source holds and leaves the Upstream
// series to the Source that replaces it (05 req 7).
func TestSetMetricsAndClose(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	h.src.Refresh(t.Context()) // NXDOMAIN: stale
	if !h.stale(t) {
		t.Fatal("not stale")
	}
	m2, g2 := newMetrics()
	h.src.SetMetrics(m2)
	if g2[emit.UpDegradedDiscoveryStale].v.Load() != 1 {
		t.Fatal("new handles not set to the current state")
	}
	h.src.SetMetrics(nil)
	h.src.SetMetrics(&emit.UpstreamMetrics{}) // nil gauges are ignored
	h.src.SetMetrics(m2)
	h.src.Close()
	h.src.Close()
	if h.status.node(catalog.ReasonDiscoveryStale) {
		t.Fatal("Close did not clear the Node reason")
	}
	if g2[emit.UpDegradedDiscoveryStale].v.Load() != 1 {
		t.Fatal("Close wrote the Upstream gauge it no longer owns")
	}
	m3, g3 := newMetrics()
	g3[emit.UpDegradedDiscoveryStale].v.Store(1)
	h.src.SetMetrics(m3) // a closed Source writes nothing
	if g3[emit.UpDegradedDiscoveryStale].v.Load() != 1 {
		t.Fatal("closed Source wrote new handles")
	}
	set, next := h.src.Refresh(t.Context())
	if next != 0 || h.res.count("s") != 1 || !set.Status.Stale {
		t.Fatalf("Refresh after Close looked up (%d) or scheduled %v", h.res.count("s"), next)
	}
	if h.src.Spec().DNS.Service != "s" {
		t.Fatal("Spec")
	}
}

// sleepClock counts Sleep calls so a test knows when Run waits.
type sleepClock struct {
	*clocktest.Fake
	sleeps atomic.Int64
}

func (c *sleepClock) Sleep(ctx context.Context, d time.Duration) error {
	c.sleeps.Add(1)
	return c.Fake.Sleep(ctx, d)
}

// TestRunSchedule drives Run with the fake clock: refreshes every 27-33 s
// after good answers (05 req 6), 1-2 s after a first failure (05 req 7),
// onChange on Endpoint changes only, and a clean stop.
func TestRunSchedule(t *testing.T) {
	clk := &sleepClock{Fake: clocktest.New(epoch())}
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, func(o *Options) {
		o.Rand = fixedSource(0)
		o.Clock = clk
	})
	h.res.setHost("s", "10.0.0.1")
	changes := make(chan *Set, 8)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.src.Run(ctx, func(s *Set) { changes <- s })
	}()

	// After the n-th Sleep call every lookup timer is stopped, so the one
	// pending timer is the sleep's.
	waitSleep := func(n int64) {
		t.Helper()
		waitFor(t, fmt.Sprintf("sleep %d", n), func() bool { return clk.sleeps.Load() >= n && clk.Pending() == 1 })
	}
	waitSleep(1)
	if s := <-changes; s.Len() != 1 {
		t.Fatalf("first change %q", describe(s))
	}
	// fixedSource(0) draws the low end: 27 s after a good answer.
	clk.Advance(27*time.Second - time.Millisecond)
	if h.res.count("s") != 1 {
		t.Fatal("refreshed before 27 s")
	}
	clk.Advance(time.Millisecond)
	waitSleep(2)
	if h.res.count("s") != 2 {
		t.Fatalf("lookups %d, want 2", h.res.count("s"))
	}
	select {
	case s := <-changes:
		t.Fatalf("onChange without a change: %q", describe(s))
	default:
	}
	h.res.failHost("s", servfail("s"))
	clk.Advance(27 * time.Second)
	waitSleep(3)
	// First failure: the backoff's low end, 1 s.
	h.res.setHost("s", "10.0.0.2")
	clk.Advance(time.Second)
	waitSleep(4)
	if s := <-changes; s.Identities()[0] != "10.0.0.2:80" {
		t.Fatalf("change %q", describe(s))
	}
	cancel()
	<-done
}

// TestRefreshCanceled: a refresh interrupted by its context changes
// nothing and schedules nothing.
func TestRefreshCanceled(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	h.res.block = make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	type result struct {
		set  *Set
		next time.Duration
	}
	out := make(chan result, 1)
	go func() {
		s, n := h.src.Refresh(ctx)
		out <- result{s, n}
	}()
	waitFor(t, "lookup", func() bool { return h.res.count("s") == 1 })
	cancel()
	r := <-out
	if r.next != 0 || r.set.Status.Failures != 0 || h.stale(t) {
		t.Fatalf("canceled refresh changed state: %+v", r.set.Status)
	}
	h.src.Run(ctx, nil) // returns at once on a canceled context
}

// TestLookupTimeout: a lookup that outlives the lookup timeout on the
// Source's clock fails the refresh.
func TestLookupTimeout(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, func(o *Options) { o.LookupTimeout = 3 * time.Second })
	h.res.block = make(chan struct{})
	out := make(chan *Set, 1)
	go func() {
		s, _ := h.src.Refresh(t.Context())
		out <- s
	}()
	waitFor(t, "lookup", func() bool { return h.res.count("s") == 1 && h.clk.Pending() == 1 })
	h.clk.Advance(3 * time.Second)
	set := <-out
	if !errors.Is(set.Status.Err, errLookupTimeout) || !h.stale(t) || notFound(set.Status.Err) {
		t.Fatalf("status %+v", set.Status)
	}
}

// TestLookupsBounded: one refresh runs at most MaxConcurrentLookups lookups
// at a time and joins them all.
func TestLookupsBounded(t *testing.T) {
	var eps []StaticEndpoint
	for i := range 40 {
		eps = append(eps, StaticEndpoint{Address: fmt.Sprintf("h%02d.internal:80", i), Weight: 1})
	}
	h := newHarness(t, Spec{Static: eps}, nil)
	for i := range 40 {
		h.res.setHost(fmt.Sprintf("h%02d.internal", i), fmt.Sprintf("10.0.0.%d", i+1))
	}
	h.res.block = make(chan struct{})
	out := make(chan *Set, 1)
	go func() {
		s, _ := h.src.Refresh(t.Context())
		out <- s
	}()
	waitFor(t, "lookups in flight", func() bool { return h.res.inFlight.Load() == MaxConcurrentLookups })
	close(h.res.block)
	set := <-out
	if h.res.peak.Load() > MaxConcurrentLookups || set.Len() != 40 || h.res.inFlight.Load() != 0 {
		t.Fatalf("peak %d, %d Endpoints", h.res.peak.Load(), set.Len())
	}
	for _, e := range set.Endpoints {
		if len(e.Addrs) != 1 {
			t.Fatalf("%s unresolved", e.Identity)
		}
	}
}

func TestSetHelpers(t *testing.T) {
	var nilSet *Set
	if nilSet.Len() != 0 || nilSet.Identities() != nil {
		t.Fatal("nil Set")
	}
	if _, ok := nilSet.Lookup("x"); ok {
		t.Fatal("nil Lookup")
	}
	a := &Set{Endpoints: []Endpoint{{Identity: "a:1", Weight: 1}, {Identity: "b:1", Weight: 2}}}
	b := &Set{Endpoints: []Endpoint{{Identity: "a:1", Weight: 1, Addrs: []netip.AddrPort{netip.MustParseAddrPort("10.0.0.1:1")}}, {Identity: "b:1", Weight: 2}}}
	c := &Set{Endpoints: []Endpoint{{Identity: "a:1", Weight: 1}, {Identity: "b:1", Weight: 3}}}
	d := &Set{Endpoints: []Endpoint{{Identity: "a:1", Weight: 1}}}
	if !a.SameMembers(b) || a.SameMembers(c) || a.SameMembers(d) || sameEndpoints(a.Endpoints, b.Endpoints) {
		t.Fatal("SameMembers")
	}
	if e, ok := a.Lookup("b:1"); !ok || e.Weight != 2 {
		t.Fatal("Lookup")
	}
	if _, ok := a.Lookup("c:1"); ok {
		t.Fatal("Lookup of a missing identity")
	}
	if withPort(nil, 80) != nil {
		t.Fatal("withPort(nil)")
	}
	if NewResolver() == nil {
		t.Fatal("NewResolver")
	}
	if !notFound(fmt.Errorf("wrapped: %w", nxdomain("x"))) || notFound(servfail("x")) || notFound(errors.New("x")) {
		t.Fatal("notFound")
	}
	if got := ipsOf([]net.IPAddr{{IP: nil}, {IP: net.ParseIP("10.0.0.1")}}); len(got) != 1 {
		t.Fatalf("ipsOf dropped nothing: %v", got)
	}
	if trimDot(".") != "." || trimDot("a.") != "a" {
		t.Fatal("trimDot")
	}
}

func TestDefaultOptions(t *testing.T) {
	s, err := New(Spec{Static: []StaticEndpoint{{Address: "10.0.0.1:80", Weight: 1}}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.clk == nil || s.res == nil || s.rnd == nil || s.lookupTimeout != DefaultLookupTimeout {
		t.Fatal("defaults not applied")
	}
	if a, b := s.rnd.Uint64(), s.rnd.Uint64(); a == b {
		t.Fatal("global source repeats")
	}
}

// TestNoWaitForLookups: SetMetrics (activation) and Close (teardown) never
// wait for a refresh blocked in DNS, and a refresh that ends after Close
// neither raises discovery_stale nor schedules another (05 reqs 2 and 7).
func TestNoWaitForLookups(t *testing.T) {
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, nil)
	h.res.block = make(chan struct{})
	type result struct {
		set  *Set
		next time.Duration
	}
	out := make(chan result, 1)
	go func() {
		s, n := h.src.Refresh(t.Context())
		out <- result{s, n}
	}()
	waitFor(t, "lookup", func() bool { return h.res.count("s") == 1 })
	m2, g2 := newMetrics()
	h.src.SetMetrics(m2) // would deadlock if it waited for the lookup
	if g2[emit.UpDegradedDiscoveryStale].v.Load() != 0 {
		t.Fatal("gauge raised before any failure")
	}
	h.src.Close()
	close(h.res.block) // the lookup now ends with NXDOMAIN
	r := <-out
	if r.next != 0 || h.stale(t) || g2[emit.UpDegradedDiscoveryStale].v.Load() != 0 {
		t.Fatalf("refresh after Close: next %v, stale %v", r.next, h.stale(t))
	}
	if len(h.status.list()) != 0 {
		t.Fatalf("reason reported after Close: %+v", h.status.list())
	}
}

// TestSourceIsProvider pins the M2 extension point (spec 05 section 8):
// the Upstream layer holds a Provider, which *Source implements.
func TestSourceIsProvider(t *testing.T) {
	h := newHarness(t, Spec{Static: []StaticEndpoint{{Address: "10.0.0.1:80", Weight: 1}}}, nil)
	var p Provider = h.src
	if p.Current().Len() != 1 {
		t.Fatal("Provider.Current")
	}
	p.Run(canceled(t), nil)
	p.SetMetrics(nil)
	p.Close()
}

// TestRunAfterCompileRefresh: the Upstream layer refreshes at compile time
// (05 req 5) and then runs Run, which waits for the delay that refresh
// returned before its own first lookup: no back-to-back lookups after a
// good answer, and no NXDOMAIN repeats counted milliseconds apart, which
// would defeat the 3-refresh tolerance of 05 req 7.
func TestRunAfterCompileRefresh(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(r *fakeResolver)
		wait time.Duration // fixedSource(0): the low end of the window
	}{
		{"good answer", func(r *fakeResolver) { r.setHost("s", "10.0.0.1") }, RefreshInterval - RefreshJitter},
		{"NXDOMAIN", func(*fakeResolver) {}, BackoffMin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := &sleepClock{Fake: clocktest.New(epoch())}
			h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, func(o *Options) {
				o.Rand = fixedSource(0)
				o.Clock = clk
			})
			tc.set(h.res)
			if _, next := h.src.Refresh(t.Context()); next != tc.wait {
				t.Fatalf("compile-time refresh schedules %v, want %v", next, tc.wait)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.src.Run(ctx, nil)
			}()
			waitFor(t, "Run's first sleep", func() bool { return clk.sleeps.Load() == 1 && clk.Pending() == 1 })
			if n := h.res.count("s"); n != 1 {
				t.Fatalf("Run looked up at once: %d lookups", n)
			}
			clk.Advance(tc.wait - time.Millisecond)
			if n := h.res.count("s"); n != 1 {
				t.Fatalf("Run looked up before the returned delay: %d lookups", n)
			}
			clk.Advance(time.Millisecond)
			waitFor(t, "Run's second sleep", func() bool { return clk.sleeps.Load() == 2 && clk.Pending() == 1 })
			if n := h.res.count("s"); n != 2 {
				t.Fatalf("%d lookups when the delay ran out, want 2", n)
			}
			cancel()
			<-done
		})
	}
}

// TestRunFollowsLatestRefresh: a Refresh called while Run sleeps that
// moves the due time later sends Run back to sleep until that Refresh's
// delay, with no lookup in between.
func TestRunFollowsLatestRefresh(t *testing.T) {
	clk := &sleepClock{Fake: clocktest.New(epoch())}
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "s", Port: 80}}, func(o *Options) {
		o.Rand = fixedSource(0)
		o.Clock = clk
	})
	h.res.setHost("s", "10.0.0.1")
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.src.Run(ctx, nil)
	}()
	waitFor(t, "first sleep", func() bool { return clk.sleeps.Load() == 1 && clk.Pending() == 1 })
	clk.Advance(20 * time.Second)
	h.src.Refresh(t.Context()) // due 27 s from now: 47 s
	clk.Advance(7 * time.Second)
	// Run wakes at 27 s and sleeps again until 47 s without a lookup.
	waitFor(t, "second sleep", func() bool { return clk.sleeps.Load() == 2 && clk.Pending() == 1 })
	if n := h.res.count("s"); n != 2 {
		t.Fatalf("%d lookups at 27 s, want 2", n)
	}
	clk.Advance(20 * time.Second)
	waitFor(t, "third sleep", func() bool { return clk.sleeps.Load() == 3 && clk.Pending() == 1 })
	if n := h.res.count("s"); n != 3 {
		t.Fatalf("%d lookups at 47 s, want 3", n)
	}
	cancel()
	<-done
	// A closed Source has nothing due, so Run would just wait for its
	// context.
	h.src.Close()
	if _, ok := h.src.untilDue(); ok {
		t.Fatal("closed Source still due")
	}
}

// TestSRVMalformedRecords: since Go 1.20 LookupSRV drops records whose
// target is not a valid domain name and returns the others with a
// *net.DNSError; the remaining records are a good answer (05 req 6), so
// the set follows them and discovery_stale stays clear (05 req 7). Without
// remaining records the error is an ordinary failure.
func TestSRVMalformedRecords(t *testing.T) {
	const name = "_http._tcp.orders.svc"
	malformed := &net.DNSError{Err: "DNS response contained records which contain invalid names", Name: name}
	h := newHarness(t, Spec{DNS: &DNSSpec{Service: "orders.svc", PortName: "http"}}, nil)
	h.res.setHost("a.orders.svc", "10.0.1.1")
	h.res.setHost("b.orders.svc", "10.0.1.2")
	h.res.partialSRV(name, malformed, srv("a.orders.svc.", 8080, 10, 10))
	set, next := h.src.Refresh(t.Context())
	wantSet(t, set, "a.orders.svc:8080=10.0.1.1:8080/10 host=a.orders.svc")
	if set.Status.Stale || set.Status.Err != nil || h.stale(t) || next < 27*time.Second {
		t.Fatalf("records with a malformed-records error: status %+v next %v", set.Status, next)
	}
	// The set follows SRV changes while malformed records persist.
	h.res.partialSRV(name, malformed, srv("a.orders.svc.", 8080, 10, 10), srv("b.orders.svc.", 8080, 10, 30))
	set, _ = h.src.Refresh(t.Context())
	wantSet(t, set,
		"a.orders.svc:8080=10.0.1.1:8080/10 host=a.orders.svc",
		"b.orders.svc:8080=10.0.1.2:8080/30 host=b.orders.svc")
	// Every record malformed: a failure that keeps the set, not NXDOMAIN.
	h.res.partialSRV(name, malformed)
	set, _ = h.src.Refresh(t.Context())
	if set.Len() != 2 || !set.Status.Stale || notFound(set.Status.Err) || !h.stale(t) {
		t.Fatalf("no remaining records: %q %+v", describe(set), set.Status)
	}
	// Other errors that come with records are not trusted.
	for _, err := range []error{
		servfail(name),
		&net.DNSError{Err: "i/o timeout", Name: name, IsTimeout: true},
		nxdomain(name),
		errors.New("resolver broke"),
	} {
		if malformedOnly(err, []*net.SRV{srv("a.orders.svc.", 8080, 10, 10)}) {
			t.Fatalf("malformedOnly(%v) kept the records", err)
		}
	}
	if !malformedOnly(fmt.Errorf("wrapped: %w", malformed), []*net.SRV{nil}) || malformedOnly(malformed, nil) {
		t.Fatal("malformedOnly")
	}
}
