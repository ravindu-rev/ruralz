// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
)

// primaryAt reports whether the sentinel client sends to addr.
func (f *fixture) primaryAt(addr string) bool {
	c := f.d.loadClient()
	if c == nil {
		return false
	}
	_, ok := c.Nodes()[addr]
	return ok
}

// quietLimits parks the memory readings, so only calls and the client
// dial, and the driver's periodic sentinel check, so only the sentinels'
// events and closed connections make the client ask them again.
func quietLimits() statestore.Limits {
	lim := statestore.DefaultLimits()
	lim.MemoryPollSlow, lim.MemoryPollFast = time.Hour, time.Hour
	lim.ShardsRefresh = time.Hour
	return lim
}

// TestSentinelFindsThePrimary: topology sentinel asks the URL's sentinel
// for master_set's primary, checks its ROLE and sends every call there,
// and subscribes to its events, which the sentinel client does on a
// goroutine of its own; the sentinel connection gets no credentials and
// no script load, which the primary's gets before its first EVALSHA, and
// the primary is one shard whatever its address.
func TestSentinelFindsThePrimary(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, limits: quietLimits()})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call through the sentinel: Done %v Err %v", c.Done, c.Err)
	}
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	if f.nodes[0].count("EVALSHA") != 1 || f.nodes[1].count("EVALSHA") != 0 {
		t.Fatalf("EVALSHA on the primary %d, on the other server %d; want 1, 0", f.nodes[0].count("EVALSHA"), f.nodes[1].count("EVALSHA"))
	}
	if f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME") == 0 || f.sent.count("SUBSCRIBE") == 0 {
		t.Fatalf("the sentinel was not asked for the primary: %v", f.sent.counts)
	}
	if f.nodes[0].count("ROLE") == 0 || f.nodes[0].count("SCRIPT LOAD") == 0 {
		t.Fatal("the primary's connection was not checked with ROLE or got no SCRIPT LOAD")
	}
	for _, cmd := range []string{"AUTH", "HELLO AUTH", "SCRIPT"} {
		if n := f.sent.count(cmd); n != 0 {
			t.Errorf("the sentinel got %d %s", n, cmd)
		}
	}
	if n := len(f.d.reg.byAddr); n != 0 {
		t.Errorf("%d shards by address; the sentinel primary is one shard", n)
	}
	if st := f.d.Status(); !st.Cleartext || !st.Unauthenticated || st.BreakerNotClosed {
		t.Errorf("Status = %+v, want cleartext, unauthenticated, breakers closed", st)
	}
}

// TestSentinelAuthenticates: the URL's credentials authenticate to the
// sentinels as well as to the primary: a sentinel that requires the
// default user's password, or a named user's, serves the client, and a
// sentinel without a password takes them for its default user.
func TestSentinelAuthenticates(t *testing.T) {
	for _, tc := range []struct {
		name, user, password, url string
	}{
		{"default user's password", "", "pw", "redis://:pw@%s?master_set=main"},
		{"named user", "ruralz", "pw", "redis://ruralz:pw@%s?master_set=main"},
		{"sentinel without a password", "", "", "redis://:pw@%s?master_set=main"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{
				sentinel: 1, limits: quietLimits(), fake: fakeOptions{user: tc.user, password: "pw"},
				sentinelUser: tc.user, sentinelPassword: tc.password,
				sentinelURL: func(addr string) string { return fmt.Sprintf(tc.url, addr) },
			})
			f.ready()
			c := gcraCall(statestore.DigestOf("k"), oneLimit())
			if f.consume(c); c.Err != nil {
				t.Fatalf("call through an authenticating sentinel: Err %v", c.Err)
			}
			if f.sent.count("HELLO AUTH") == 0 {
				t.Fatal("the sentinel got no credentials")
			}
		})
	}
}

// TestSentinelFollowsSwitchMaster: a +switch-master event moves every
// later call to the new primary, whose connection loads the scripts.
func TestSentinelFollowsSwitchMaster(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, limits: quietLimits()})
	f.ready()
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	f.clk.Advance(time.Second) // a fresh dial budget for the new primary
	f.sent.switchMaster("main", f.nodes[1].addr)
	waitFor(t, "the switch to the new primary", func() bool { return f.primaryAt(f.nodes[1].addr) })
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call after the switch: Err %v", c.Err)
	}
	if f.nodes[1].count("EVALSHA") != 1 || f.nodes[1].count("SCRIPT LOAD") == 0 {
		t.Fatalf("new primary: EVALSHA %d, SCRIPT LOAD %d", f.nodes[1].count("EVALSHA"), f.nodes[1].count("SCRIPT LOAD"))
	}
}

// TestSentinelDialFailuresBeforeTheClient: while no sentinel answers and
// no client exists, the failed sentinel dials count toward the primary's
// breaker, so calls get RZ-STS-003 instead of each waiting out its
// timeout.
func TestSentinelDialFailuresBeforeTheClient(t *testing.T) {
	// The sentinel address closes every connection: the TLS handshake of
	// each sentinel dial fails at once.
	f := newFixture(t, fixtureOptions{
		topology: statestore.TopologySentinel, limits: quietLimits(), fake: fakeOptions{reject: true},
		urlFor: func(s *fakeServer) string { return "rediss://:pw@" + s.addr + "?master_set=main" },
	})
	f.advanceWhile("the primary's breaker to open", func() bool { return f.d.single.brk.State() == breaker.Open }, 300*time.Millisecond)
	if f.d.loadClient() != nil {
		t.Fatal("a client exists without a sentinel")
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != statestore.ErrBreakerOpen {
		t.Fatalf("call with no sentinel reachable: Err %v, want RZ-STS-003", c.Err)
	}
	if f.d.sentinels.brk.State() != breaker.Closed {
		t.Error("the sentinels' pacing shard's breaker recorded the dials")
	}
}

// checkLimits are quietLimits with the driver's sentinel check due every
// second.
func checkLimits() statestore.Limits {
	lim := quietLimits()
	lim.ShardsRefresh = time.Second
	return lim
}

// checkSentinelsNow moves the driver's clock a check period on, so the
// next maintenance pass starts a sentinel check, and waits until the
// sentinel answered more SENTINEL GET-MASTER-ADDR-BY-NAME requests than
// asked and holds only the connections of the State client again: the
// check, which dials its own connection, ended.
func (f *fixture) checkSentinelsNow(asked, clientConns int) {
	f.t.Helper()
	f.awaitIdle()
	f.clk.Jump(time.Second)
	waitFor(f.t, "a sentinel check", func() bool {
		return f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME") > asked && f.sent.connections() == clientConns
	})
}

// TestSentinelFrozenPrimaryDoesNotHoldTheSwitch: with the primary frozen,
// its connections open and unanswered as a SIGSTOPped or black-holed
// primary's are, a refresh falling due changes nothing and leaves the
// failover the sentinel then announces to take effect at once: the driver
// asks the sentinel itself, which names the frozen primary the client
// holds, so it keeps the client as it is, and the client never checks its
// primary with ROLE on a schedule, which would hold the lock its
// +switch-master handling needs until the connection's write timeout, an
// hour here (docs/architecture/11-scalability-and-distributed-state.md
// "State client and RZ-STS error codes").
func TestSentinelFrozenPrimaryDoesNotHoldTheSwitch(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		sentinel: 2, limits: checkLimits(),
		sentinelURL: func(addr string) string { return "redis://" + addr + "?master_set=main&write_timeout=1h" },
	})
	f.ready()
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	subscribed := f.sent.count("SUBSCRIBE")
	f.nodes[0].inject(&fault{hold: make(chan struct{})})
	t.Cleanup(f.nodes[0].clearFaults)
	f.checkSentinelsNow(f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME"), 1)
	f.sent.switchMaster("main", f.nodes[1].addr)
	waitFor(t, "the switch to the primary the sentinel announced", func() bool { return f.primaryAt(f.nodes[1].addr) })
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || f.nodes[1].count("EVALSHA") != 1 {
		t.Fatalf("call after the switch: Err %v, EVALSHA on the new primary %d; want an answer there", c.Err, f.nodes[1].count("EVALSHA"))
	}
	if n := f.sent.count("SUBSCRIBE"); n != subscribed {
		t.Errorf("the client subscribed %d more times; the check naming the primary it holds must not make it ask again", n-subscribed)
	}
	if n := f.logs.count("state store sentinels name another primary; the client asks them again"); n != 0 {
		t.Errorf("%d prompts for the primary the client holds", n)
	}
}

// TestSentinelCheckFollowsAMissedSwitch: when the client misses
// +switch-master, the driver's check every ShardsRefresh finds the
// sentinel naming another primary, closes the client's sentinel
// connections, which makes the client ask the sentinels again, and logs
// it once by shard; the client then sends to the new primary.
func TestSentinelCheckFollowsAMissedSwitch(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, limits: checkLimits()})
	f.ready()
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	f.sent.setMaster("main", f.nodes[1].addr)
	asked := f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME")
	f.awaitIdle()
	f.clk.Jump(time.Second)
	waitFor(t, "the client to follow the sentinel to the new primary", func() bool { return f.primaryAt(f.nodes[1].addr) })
	if n := f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME"); n < asked+2 {
		t.Errorf("the sentinel was asked %d more times, want the check's and the client's", n-asked)
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || f.nodes[1].count("EVALSHA") != 1 {
		t.Fatalf("call after the switch: Err %v, EVALSHA on the new primary %d", c.Err, f.nodes[1].count("EVALSHA"))
	}
	if n := f.logs.count("state store sentinels name another primary; the client asks them again"); n != 1 {
		t.Errorf("%d prompt logs, want 1", n)
	}
	if text := f.logs.text(); !strings.Contains(text, "shard="+f.d.single.id) || strings.Contains(text, f.nodes[1].addr) {
		t.Errorf("the prompt log names no shard or carries an address:\n%s", text)
	}
}

// TestSentinelCheckGivesUpOnASilentSentinel: a sentinel seed that accepts
// connections and never answers costs the driver's check the admin
// timeout, after which it asks the next seed, whose answer counts.
func TestSentinelCheckGivesUpOnASilentSentinel(t *testing.T) {
	silent := silentServer(t)
	f := newFixture(t, fixtureOptions{
		sentinel: 2, limits: checkLimits(), opts: Options{AdminTimeout: 50 * time.Millisecond},
		sentinelURL: func(addr string) string {
			return "redis://" + silent + "?master_set=main&dial_timeout=100ms&addr=" + addr
		},
	})
	f.ready()
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	f.sent.setMaster("main", f.nodes[1].addr)
	asked := f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME")
	f.awaitIdle()
	f.clk.Jump(time.Second)
	waitFor(t, "the client to follow the answer of the second seed", func() bool { return f.primaryAt(f.nodes[1].addr) })
	if n := f.sent.count("SENTINEL GET-MASTER-ADDR-BY-NAME"); n < asked+2 {
		t.Errorf("the answering sentinel was asked %d more times, want the check's and the client's", n-asked)
	}
}

// silentServer listens on loopback, accepts every connection and never
// writes; it closes when t ends.
func silentServer(t *testing.T) string {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var conns []net.Conn
	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			wg.Go(func() { _, _ = io.Copy(io.Discard, c) })
		}
	})
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}
