// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for the zone label of ruralz_state_call_duration_seconds
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"): the State client looks up the host of the
// server each call went to, the standalone endpoint, the cluster shard's
// primary in the slot map or the primary the sentinels name, in its
// connection's zone map and compares that zone with RURALZ_ZONE. The fake
// servers listen on distinct 127/8 loopback addresses so zone maps can
// tell them apart.

// onlyZone fails unless op has one duration observation, labeled zone.
func (f *fixture) onlyZone(op, zone int) {
	f.t.Helper()
	for z := range emit.NumStateZones {
		want := uint64(0)
		if z == zone {
			want = 1
		}
		if got := f.m.zoned(op, z); got != want {
			f.t.Errorf("%s durations labeled %s = %d, want %d", catalog.Ops()[op], catalog.Zones()[z], got, want)
		}
	}
}

// splitKey returns a cache key whose generation key the cluster stores on
// node gen and whose partition it stores on node part.
func (f *fixture) splitKey(gen, part int) statestore.CacheKey {
	f.t.Helper()
	for i := range 100_000 {
		k := statestore.CacheKey{URI: statestore.DigestOf("u" + strconv.Itoa(i)), Partition: statestore.DigestOf("p")}
		if f.cl.owner(int(keys.DigestSlot(k.URI))) == f.nodes[gen] && f.cl.owner(int(keys.PartitionSlot(k))) == f.nodes[part] {
			return k
		}
	}
	f.t.Fatal("no key with that split")
	return statestore.CacheKey{}
}

// TestZoneTable checks the comparison a zone table holds: same when the
// server's host maps to the Node's zone, cross for another zone, unknown
// for a host without an entry, a shard without a server host, a Node
// without RURALZ_ZONE or a connection without a zone map.
func TestZoneTable(t *testing.T) {
	zones := statestore.Zones{"valkey-0.state.internal": "eu-west-1a", "10.0.2.15": "eu-west-1b"}
	host := func(s string) *string { return &s }
	tests := []struct {
		name, node string
		zones      statestore.Zones
		host       *string
		want       int
	}{
		{"server in the Node's zone", "eu-west-1a", zones, host("valkey-0.state.internal"), emit.StateZoneSame},
		{"server in another zone", "eu-west-1a", zones, host("10.0.2.15"), emit.StateZoneCross},
		{"host without an entry", "eu-west-1a", zones, host("valkey-9.state.internal"), emit.StateZoneUnknown},
		{"shard without a server host", "eu-west-1a", zones, nil, emit.StateZoneUnknown},
		{"Node without RURALZ_ZONE", "", zones, host("valkey-0.state.internal"), emit.StateZoneUnknown},
		{"connection without a zone map", "eu-west-1a", nil, host("valkey-0.state.internal"), emit.StateZoneUnknown},
	}
	for _, tc := range tests {
		if got := newZoneTable(tc.node, tc.zones).label(tc.host); got != tc.want {
			t.Errorf("%s: label = %s, want %s", tc.name, catalog.Zones()[got], catalog.Zones()[tc.want])
		}
	}
}

// TestHostOf checks the server host a shard's calls are labeled by: an
// address's host in netaddr.CanonicalHost form, the zone map's key form,
// so a hostname compares case-insensitively and an IP literal in its
// canonical text; the driver's own shards (unmapped, overflow, the
// sentinel topology's primary and sentinels) have none.
func TestHostOf(t *testing.T) {
	for addr, want := range map[string]string{
		"valkey-0.state.internal:6379": "valkey-0.state.internal",
		"Valkey-0.STATE.internal:6379": "valkey-0.state.internal",
		"10.0.2.15:6379":               "10.0.2.15",
		"[2001:0DB8:0:0::1]:6379":      "2001:db8::1",
	} {
		if got := hostOf(addr); got == nil || *got != want {
			t.Errorf("hostOf(%q) = %v, want %q", addr, got, want)
		}
	}
	for _, addr := range []string{"unmapped", "overflow", "primary/main@127.0.0.1:26379", "sentinels/main@127.0.0.1:26379", ""} {
		if got := hostOf(addr); got != nil {
			t.Errorf("hostOf(%q) = %q, want none", addr, *got)
		}
	}
}

// TestZoneStandalone checks the standalone endpoint's label on every kind
// of round trip, a consumptive script, a lookup pipeline and a write:
// same when the URL's host maps to RURALZ_ZONE, cross for another zone,
// unknown without RURALZ_ZONE, without a zone map or without an entry for
// the host.
func TestZoneStandalone(t *testing.T) {
	tests := []struct {
		name, node string
		zones      statestore.Zones
		want       int
	}{
		{"server in the Node's zone", "lab-a", statestore.Zones{"127.0.0.1": "lab-a"}, emit.StateZoneSame},
		{"server in another zone", "lab-a", statestore.Zones{"127.0.0.1": "lab-b"}, emit.StateZoneCross},
		{"Node without RURALZ_ZONE", "", statestore.Zones{"127.0.0.1": "lab-a"}, emit.StateZoneUnknown},
		{"connection without a zone map", "lab-a", nil, emit.StateZoneUnknown},
		{"host without an entry", "lab-a", statestore.Zones{"127.0.0.2": "lab-a"}, emit.StateZoneUnknown},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{zone: tc.node, zones: tc.zones})
			f.ready()
			d := statestore.DigestOf("k")
			c := gcraCall(d, oneLimit())
			if f.consume(c); c.Err != nil || !c.Done {
				t.Fatalf("GCRA call: Done %v Err %v", c.Done, c.Err)
			}
			f.onlyZone(emit.StateOpGCRA, tc.want)
			l := lookupCall(statestore.CacheKey{URI: d, Partition: d}, d)
			if f.read(l); l.Err != nil {
				t.Fatalf("lookup: Err %v", l.Err)
			}
			f.onlyZone(emit.StateOpPipeline, tc.want)
			w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d, Bump: statestore.Token{1}}
			if f.write(w); w.Err != nil {
				t.Fatalf("invalidation: Err %v", w.Err)
			}
			f.onlyZone(emit.StateOpCacheInvalidate, tc.want)
		})
	}
}

// TestZoneTimeoutLabeled checks that a round trip that misses its timeout
// records its duration under its server's zone like any other.
func TestZoneTimeoutLabeled(t *testing.T) {
	f := newFixture(t, fixtureOptions{zone: "lab-a", zones: statestore.Zones{"127.0.0.1": "lab-b"}})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 1})
	defer close(hold.hold)
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.advanceUntil(background(func() { f.consume(c) }), 10*time.Millisecond)
	if c.Err != statestore.ErrTimeout {
		t.Fatalf("held reply: Err %v, want RZ-STS-001", c.Err)
	}
	f.onlyZone(emit.StateOpGCRA, emit.StateZoneCross)
}

// TestZoneCluster checks the cluster topology: a call takes the zone of
// its slot's primary in the slot map; a pipelined lookup over two
// primaries is cross when either is in another zone; and once the slot
// map moves a slot to another primary, the next call to it takes that
// primary's zone.
func TestZoneCluster(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		cluster: 2, hosts: []string{"127.0.0.1", "127.0.0.2"},
		zone: "lab-a", zones: statestore.Zones{"127.0.0.1": "lab-a", "127.0.0.2": "lab-b"},
	})
	f.ready()
	near, far := f.digestOn(0, "near"), f.digestOn(1, "far")
	for _, d := range []statestore.Digest{near, far} {
		if c := gcraCall(d, oneLimit()); f.consume(c) == 0 || c.Err != nil {
			t.Fatalf("GCRA call: Err %v", c.Err)
		}
	}
	if f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame) != 1 || f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross) != 1 {
		t.Fatalf("GCRA durations: same %d, cross %d; want 1 and 1",
			f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame), f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross))
	}
	v := statestore.DigestOf("v")
	local := lookupCall(f.splitKey(0, 0), v)
	if f.read(local); local.Err != nil {
		t.Fatalf("lookup on the near primary: Err %v", local.Err)
	}
	f.onlyZone(emit.StateOpPipeline, emit.StateZoneSame)
	split := lookupCall(f.splitKey(0, 1), v)
	if f.read(split); split.Err != nil {
		t.Fatalf("lookup over both primaries: Err %v", split.Err)
	}
	if got := f.m.zoned(emit.StateOpPipeline, emit.StateZoneCross); got != 1 {
		t.Fatalf("a pipeline to both zones: %d cross durations, want 1", got)
	}
	// Every slot moves to the near primary; the slot map follows within
	// its refresh, and the far digest's calls are same-zone from then on.
	f.cl.mu.Lock()
	f.cl.ranges = []fakeRange{{first: 0, last: keys.Slots - 1, srv: f.nodes[0]}}
	f.cl.mu.Unlock()
	want := f.d.shardAt(f.nodes[0].addr)
	f.advanceWhile("the slot map refresh", func() bool { return f.d.shardOfSlot(keys.DigestSlot(far)) == want }, time.Second)
	if c := gcraCall(far, oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("GCRA call after the move: Err %v", c.Err)
	}
	if f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame) != 2 || f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross) != 1 {
		t.Fatalf("GCRA durations after the move: same %d, cross %d; want 2 and 1",
			f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame), f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross))
	}
}

// TestZoneClusterFold checks the label of a pipelined round trip to two
// primaries: cross when either is in another zone, else unknown when
// either has no entry, else same.
func TestZoneClusterFold(t *testing.T) {
	tests := []struct {
		name  string
		zones statestore.Zones
		want  int
	}{
		{"both in the Node's zone", statestore.Zones{"127.0.0.1": "lab-a", "127.0.0.2": "lab-a"}, emit.StateZoneSame},
		{"one in another zone", statestore.Zones{"127.0.0.1": "lab-a", "127.0.0.2": "lab-b"}, emit.StateZoneCross},
		{"one without an entry", statestore.Zones{"127.0.0.1": "lab-a"}, emit.StateZoneUnknown},
		{"one without an entry, one in another zone", statestore.Zones{"127.0.0.2": "lab-b"}, emit.StateZoneCross},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, fixtureOptions{cluster: 2, hosts: []string{"127.0.0.1", "127.0.0.2"}, zone: "lab-a", zones: tc.zones})
			f.ready()
			l := lookupCall(f.splitKey(0, 1), statestore.DigestOf("v"))
			if f.read(l); l.Err != nil {
				t.Fatalf("lookup over both primaries: Err %v", l.Err)
			}
			f.onlyZone(emit.StateOpPipeline, tc.want)
		})
	}
}

// TestZoneClusterPseudoShards checks that the driver's own shards have no
// server host: a call to a slot without a known owner is unknown.
func TestZoneClusterPseudoShards(t *testing.T) {
	f := newFixture(t, fixtureOptions{cluster: 1, zone: "lab-a", zones: statestore.Zones{"127.0.0.1": "lab-a"}})
	f.ready()
	if f.d.unmapped.host.Load() != nil {
		t.Fatalf("the unmapped shard has the server host %q", *f.d.unmapped.host.Load())
	}
	if h := f.d.shardAt(f.nodes[0].addr).host.Load(); h == nil || *h != "127.0.0.1" {
		t.Fatalf("the primary's server host = %v, want 127.0.0.1", h)
	}
}

// TestZoneSentinelFollowsSwitchMaster checks the sentinel topology: calls
// take the zone of the primary the sentinels name, and a +switch-master
// answer moves the next call to the new primary's zone at once.
func TestZoneSentinelFollowsSwitchMaster(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		sentinel: 2, hosts: []string{"127.0.0.1", "127.0.0.2"}, limits: quietLimits(),
		zone: "lab-a", zones: statestore.Zones{"127.0.0.1": "lab-a", "127.0.0.2": "lab-b"},
	})
	f.ready()
	if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
		t.Fatalf("first call: Err %v", c.Err)
	}
	f.onlyZone(emit.StateOpGCRA, emit.StateZoneSame)
	waitFor(t, "the event subscription", func() bool { return f.sent.subscribers() > 0 })
	f.clk.Advance(time.Second) // a fresh dial budget for the new primary
	f.sent.switchMaster("main", f.nodes[1].addr)
	waitFor(t, "the switch to the new primary", func() bool { return f.primaryAt(f.nodes[1].addr) })
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	if f.consume(c); c.Err != nil {
		t.Fatalf("call after the switch: Err %v", c.Err)
	}
	if f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame) != 1 || f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross) != 1 {
		t.Fatalf("GCRA durations: same %d, cross %d; want 1 before the switch and 1 after",
			f.m.zoned(emit.StateOpGCRA, emit.StateZoneSame), f.m.zoned(emit.StateOpGCRA, emit.StateZoneCross))
	}
}

// TestZoneSentinelPassKeepsASwitchInProgress checks a maintenance pass
// racing a sentinel answer: the switch's dial records the new primary's
// host before the client sends there, and a pass that still finds the
// client on the old primary leaves it, so the call after the switch is
// labeled by the new primary at once. A recording two passes in a row
// find unconfirmed is corrected to the primary the client sends to, and a
// dial between two passes, of the recorded host too, starts over.
func TestZoneSentinelPassKeepsASwitchInProgress(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 2, hosts: []string{"127.0.0.1", "127.0.0.2"}, limits: quietLimits()})
	f.ready()
	f.awaitIdle()
	c := f.d.loadClient()
	host := func() string {
		if h := f.d.single.host.Load(); h != nil {
			return *h
		}
		return ""
	}
	steps := []struct {
		name string
		dial string // the address a primary dial records first, if any
		want string
	}{
		{"a pass during the switch", f.nodes[1].addr, "127.0.0.2"},
		{"a pass after a redial of the recorded host", f.nodes[1].addr, "127.0.0.2"},
		{"a pass after a dial of another host", "127.0.0.3:6379", "127.0.0.3"},
		{"a second pass on one recording", "", "127.0.0.1"},
		{"a pass on the primary", "", "127.0.0.1"},
	}
	for _, s := range steps {
		if s.dial != "" {
			f.d.single.recordDial(s.dial)
		}
		f.d.followPrimary(c)
		if got := host(); got != s.want {
			t.Fatalf("%s: primary host %q, want %q", s.name, got, s.want)
		}
	}
}

// TestZoneSentinelTickFollowsPrimary checks that the maintenance passes
// set the primary shard's server host to the primary the sentinel client
// sends to, correcting a host a dial recorded for a server that did not
// become the primary.
func TestZoneSentinelTickFollowsPrimary(t *testing.T) {
	f := newFixture(t, fixtureOptions{sentinel: 1, limits: quietLimits()})
	f.ready()
	stale := "127.0.0.9"
	f.d.single.host.Store(&stale)
	f.advanceWhile("the primary's host", func() bool {
		h := f.d.single.host.Load()
		return h != nil && *h == "127.0.0.1"
	}, MaintenanceTick)
}

// TestSetZonesRelabelsLaterCalls checks a Hot Reload that changes only the
// zone map: the driver keeps its connections and scripts, and calls after
// SetZones take the new map's label; SetZones is safe beside calls.
func TestSetZonesRelabelsLaterCalls(t *testing.T) {
	f := newFixture(t, fixtureOptions{zone: "lab-a", zones: statestore.Zones{"127.0.0.1": "lab-a"}})
	f.ready()
	call := func() {
		t.Helper()
		if c := gcraCall(statestore.DigestOf("k"), oneLimit()); f.consume(c) == 0 || c.Err != nil {
			t.Fatalf("GCRA call: Err %v", c.Err)
		}
	}
	call()
	// The open connections: rueidis may dial a shard's second pipelined
	// connection later, but SetZones never closes one.
	open := func() map[*trackedConn]bool {
		f.d.conns.mu.Lock()
		defer f.d.conns.mu.Unlock()
		m := make(map[*trackedConn]bool, len(f.d.conns.conns))
		for c := range f.d.conns.conns {
			m[c] = true
		}
		return m
	}
	before := open()
	if len(before) == 0 {
		t.Fatal("no open connection after a call")
	}
	f.d.SetZones(statestore.Zones{"127.0.0.1": "lab-b"})
	call()
	f.d.SetZones(nil)
	call()
	for z := range emit.NumStateZones {
		if got := f.m.zoned(emit.StateOpGCRA, z); got != 1 {
			t.Errorf("GCRA durations labeled %s = %d, want 1", catalog.Zones()[z], got)
		}
	}
	after := open()
	for c := range before {
		if !after[c] {
			t.Error("SetZones closed a connection; the driver keeps its connections")
		}
	}
	done := background(func() {
		for i := range 200 {
			f.d.SetZones(statestore.Zones{"127.0.0.1": "lab-" + strconv.Itoa(i%2)})
		}
	})
	for range 20 {
		call()
	}
	<-done
	if got := f.m.durs(emit.StateOpGCRA); got != 23 {
		t.Errorf("%d GCRA durations, want 23", got)
	}
}
