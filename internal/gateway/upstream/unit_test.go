// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// TestClassify checks the error kind and ejection cause of attempt
// errors: the pool's dial and handshake failures are connect and tls
// whatever their cause, a deadline is timeout, a reset is reset, and an
// HTTP/2 ping close is reset whose cause bypasses the ejection cap like a
// connect error.
func TestClassify(t *testing.T) {
	live := context.Background()
	expired, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		err   error
		kind  resilience.Kind
		cause health.Cause
	}{
		{"refused dial", live, &dialError{err: refused}, resilience.KindConnect, health.CauseConnect},
		{"no address", live, &dialError{err: discovery.ErrNoAddresses}, resilience.KindConnect, health.CauseConnect},
		{"dial budget", live, &dialError{err: fmt.Errorf("budget: %w", discovery.ErrDialBudget)}, resilience.KindConnect, health.CauseConnect},
		{"handshake verification", live, &handshakeError{err: x509.UnknownAuthorityError{}}, resilience.KindTLS, health.CauseOther},
		{"handshake EOF", live, &handshakeError{err: io.EOF}, resilience.KindTLS, health.CauseOther},
		{"handshake timeout", live, &handshakeError{err: fmt.Errorf("%w: %w", errHandshakeTimeout, context.Canceled)}, resilience.KindTLS, health.CauseOther},
		{"reset", live, io.ErrUnexpectedEOF, resilience.KindReset, health.CauseOther},
		{"ping close", live, errors.New("http2: client connection lost"), resilience.KindReset, health.CauseConnect},
		{"deadline", expired, refused, resilience.KindTimeout, health.CauseTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kind, cause := classify(tc.ctx, tc.err)
			if kind != tc.kind || cause != tc.cause {
				t.Errorf("classify(%v) = %v, %v; want %v, %v", tc.err, kind, cause, tc.kind, tc.cause)
			}
		})
	}
}

// TestSetPath checks that the normalized path reaches the request line
// unchanged: percent-encodings such as %2F stay, sub-delimiters are not
// escaped, and an empty path is "/".
func TestSetPath(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"/v1/orders/a%2Fb", "/v1/orders/a%2Fb"},
		{"/a(b)!*'", "/a(b)!*'"},
		{"/x/%E2%82%AC", "/x/%E2%82%AC"},
		{"//double//", "//double//"},
		{"", "/"},
	} {
		var u url.URL
		setPath(&u, tc.in)
		if got := u.EscapedPath(); got != tc.want {
			t.Errorf("setPath(%q): EscapedPath = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNewTable checks the published Endpoint table: identities that remain
// keep their state (in-flight counts, pools), those that left are
// returned, the dialing view follows address changes, and all-zero
// weights weigh 1 each.
func TestNewTable(t *testing.T) {
	a := netip.MustParseAddrPort("10.0.0.1:80")
	b := netip.MustParseAddrPort("10.0.0.2:80")
	set := func(eps ...discovery.Endpoint) *discovery.Set { return &discovery.Set{Endpoints: eps} }
	ep := func(id string, w uint32, addrs ...netip.AddrPort) discovery.Endpoint {
		return discovery.Endpoint{Identity: id, Host: "orders.svc", Port: 80, Addrs: addrs, Weight: w}
	}
	t1, gone := newTable(nil, set(ep("x:80", 0, a), ep("y:80", 0, b)))
	if len(gone) != 0 || t1.total != 2 || t1.eps[0].weight != 1 || t1.eps[1].weight != 1 {
		t.Fatalf("all-zero table = %+v, gone %d; want weights 1 1 and total 2", t1.eps, len(gone))
	}
	if n := t1.eps[0].st.net.Load(); n.host != "orders.svc" || n.authority != "orders.svc:80" {
		t.Errorf("dialing view = %+v, want host orders.svc and authority orders.svc:80", n)
	}
	t1.eps[1].st.inflight.Add(3)
	t2, gone := newTable(t1, set(ep("y:80", 5, a), ep("z:80", 1, b)))
	if len(gone) != 1 || gone[0].identity != "x:80" {
		t.Errorf("gone = %v, want x:80", gone)
	}
	if t2.eps[0].st != t1.eps[1].st || t2.eps[0].st.inflight.Load() != 3 || t2.total != 6 {
		t.Errorf("y:80 state not carried, or total %d != 6", t2.total)
	}
	if got := t2.eps[0].st.net.Load().addrs; len(got) != 1 || got[0] != a {
		t.Errorf("y:80 addresses = %v, want the new answer %v", got, a)
	}
	v6 := netip.MustParseAddrPort("[2001:db8::1]:8443")
	lit, _ := newTable(nil, set(discovery.Endpoint{Identity: v6.String(), Port: 8443, Addrs: []netip.AddrPort{v6}, Weight: 1}))
	if n := lit.eps[0].st.net.Load(); n.host != "2001:db8::1" || n.authority != "[2001:db8::1]:8443" {
		t.Errorf("IP literal view = %+v, want host 2001:db8::1 and authority [2001:db8::1]:8443", n)
	}
}

// TestAttemptViewGrades checks the per-attempt grading: Down from health,
// Avoided for an Endpoint the leg tried or one at its in-flight cap, and
// the least-request load of in-flight attempts plus recent failures; an
// Endpoint the Tracker does not hold yet is healthy.
func TestAttemptViewGrades(t *testing.T) {
	clk := clocktest.New(epoch())
	tr := health.NewTracker(health.Config{Upstream: "orders", Clock: clk, Passive: health.PassivePolicy{ConsecutiveErrors: 1}})
	if err := tr.SetEndpoints([]string{"a:80", "b:80", "c:80"}); err != nil {
		t.Fatal(err)
	}
	// Eject a:80 (the cap is floor(50% × 3) = 1).
	var at health.Attempt
	now := clk.Now()
	tr.View(now).Begin(&at, 0, now)
	at.End(now, health.Outcome{Failed: true})
	tab, _ := newTable(nil, &discovery.Set{Endpoints: []discovery.Endpoint{
		{Identity: "a:80", Port: 80, Weight: 1},
		{Identity: "b:80", Port: 80, Weight: 1},
		{Identity: "c:80", Port: 80, Weight: 1},
		{Identity: "d:80", Port: 80, Weight: 1},
	}})
	tab.eps[2].st.inflight.Store(8)
	v := &attemptView{tab: tab, hv: tr.View(clk.Now()), tried: []string{"b:80"}, maxConns: 16}
	want := []balance.Grade{balance.Down, balance.Avoided, balance.Avoided, balance.Eligible}
	for i, g := range want {
		if got := v.Grade(i); got != g {
			t.Errorf("Grade(%s) = %v, want %v", tab.eps[i].st.identity, got, g)
		}
	}
	if got := v.Load(0); got != 1 {
		t.Errorf("Load(a:80) = %d, want 1 (one recent failure)", got)
	}
	if got := v.Load(2); got != 8 {
		t.Errorf("Load(c:80) = %d, want 8 in flight", got)
	}
}

// TestStatus checks the /debug/upstreams view of a runtime: its Endpoints
// with health, weight and addresses, its balancer, breaker and source.
func TestStatus(t *testing.T) {
	f := newFixture(t)
	f.compile(nil, Config{Upstream: newUpstream("orders", "10.0.0.2:80", "10.0.0.1:80")})
	st := f.m.Status()
	if len(st) != 1 || st[0].Name != "orders" || len(st[0].Endpoints) != 2 || len(st[0].Balancers) != 1 {
		t.Fatalf("Status = %+v, want orders with 2 Endpoints and 1 balancer", st)
	}
	e := st[0].Endpoints[0]
	if e.Identity != "10.0.0.1:80" || !e.Healthy || e.Weight != 1 || len(e.Addresses) != 1 || e.Addresses[0] != "10.0.0.1:80" {
		t.Errorf("first Endpoint = %+v, want 10.0.0.1:80 healthy, weight 1, its address", e)
	}
	if st[0].Breaker.State != resilience.StateClosed || st[0].Balancers[0].Algorithm != balance.LeastRequest || st[0].Source.Type != "static" {
		t.Errorf("breaker %v, algorithm %v, source %q; want closed, least-request, static", st[0].Breaker.State, st[0].Balancers[0].Algorithm, st[0].Source.Type)
	}
}
