// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"fmt"
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
// dial, and the sentinel client's periodic refresh, so only its events
// and closed connections make it ask the sentinels again.
func quietLimits() statestore.Limits {
	lim := statestore.DefaultLimits()
	lim.MemoryPollSlow, lim.MemoryPollFast = time.Hour, time.Hour
	lim.ShardsRefresh = time.Hour
	return lim
}

// TestSentinelFindsThePrimary: topology sentinel asks the URL's sentinel
// for master_set's primary, checks its ROLE and sends every call there;
// the sentinel connection gets no credentials and no script load, which
// the primary's gets before its first EVALSHA, and the primary is one
// shard whatever its address.
func TestSentinelFindsThePrimary(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, limits: quietLimits()})
	f.ready()
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil || !c.Done {
		t.Fatalf("call through the sentinel: Done %v Err %v", c.Done, c.Err)
	}
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
