// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// proxied returns a fixture whose driver reaches the fake server through
// the counting RESP proxy.
func proxied(t *testing.T) (*fixture, *respproxy.Proxy) {
	t.Helper()
	var p *respproxy.Proxy
	f := newFixture(t, fixtureOptions{urlFor: func(s *fakeServer) string {
		var err error
		p, err = respproxy.Start(t.Context(), respproxy.Config{Fault: faultproxy.Config{Upstream: s.addr}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return "redis://" + p.Addr()
	}})
	f.ready()
	return f, p
}

// tripsOf returns the distinct round trips of the logged commands named
// name after seq.
func tripsOf(p *respproxy.Proxy, seq uint64, name string) (n int, trips map[uint64]bool) {
	trips = map[uint64]bool{}
	for _, c := range p.LogSince(seq) {
		if c.Name == name {
			n++
			trips[c.RoundTrip] = true
		}
	}
	return n, trips
}

// TestLookupIsOnePipelinedRoundTrip checks pack 8.7 rule 3 for the
// Response Cache: a lookup's GET, HMGET and TIME, and the lookups of one
// Read, share one pipelined round trip of read-only commands; a lookup
// whose token the hot-entry layer holds sends no GET.
func TestLookupIsOnePipelinedRoundTrip(t *testing.T) {
	f, p := proxied(t)
	seq := p.Snapshot().Seq
	d := statestore.DigestOf("k")
	a := lookupCall(statestore.CacheKey{URI: d, Partition: d}, d)
	b := lookupCall(statestore.CacheKey{URI: statestore.DigestOf("u2"), Partition: d}, d)
	hot := lookupCall(statestore.CacheKey{URI: statestore.DigestOf("u3"), Partition: d}, d)
	hot.Lookup.SkipToken, hot.Lookup.HasToken, hot.Lookup.Token = true, true, statestore.Token{7}
	f.read(a, b, hot)
	if a.Err != nil || b.Err != nil || hot.Err != nil || a.Batch != 0 || b.Trip != statestore.TripPipeline {
		t.Fatalf("lookups: %+v %+v %+v", a, b, hot)
	}
	if !hot.Lookup.HasToken || hot.Lookup.Token != (statestore.Token{7}) {
		t.Fatalf("skip-token lookup lost its held token: %+v", hot.Lookup)
	}
	gets, gt := tripsOf(p, seq, "GET")
	hmgets, ht := tripsOf(p, seq, "HMGET")
	times, tt := tripsOf(p, seq, "TIME")
	if gets != 2 || hmgets != 3 || times != 3 || len(gt) != 1 || len(ht) != 1 || len(tt) != 1 {
		t.Fatalf("%d GET, %d HMGET and %d TIME in %d, %d and %d round trips; want 2, 3 and 3 in one", gets, hmgets, times, len(gt), len(ht), len(tt))
	}
	for trip := range gt {
		if !ht[trip] || !tt[trip] {
			t.Fatal("a lookup's commands went in different round trips")
		}
	}
	if n, _ := tripsOf(p, seq, "EVALSHA"); n != 0 {
		t.Fatal("a lookup ran a script")
	}
	if f.m.call(emit.StateOpPipeline, statestore.ResultOK) != 1 || f.m.ops[emit.StateOpCacheGet].n.Load() != 5 {
		t.Fatalf("the lookups are not counted as one pipeline of 5 key reads: %d", f.m.ops[emit.StateOpCacheGet].n.Load())
	}
}

// TestWriteBatchRoundTrips checks that one post-commit batch is one
// pipelined round trip, labeled pipeline, plus a second one for the
// partition steps of the stores whose generation step applied
// (statestore.CacheStore): the generation key and the partition hash to
// different cluster slots.
func TestWriteBatchRoundTrips(t *testing.T) {
	f, p := proxied(t)
	seq := p.Snapshot().Seq
	d := statestore.DigestOf("k")
	ws := []*statestore.Write{
		{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: statestore.DigestOf("other"), Bump: statestore.Token{1}},
		{Kind: statestore.OpRefund, Timeout: callTO, Refund: statestore.Refund{Name: "q", Window: time.Hour, WindowStart: start(), Digest: d}},
		{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
			Key: statestore.CacheKey{URI: d, Partition: d}, Names: "n", Variant: d, TTL: time.Hour, Entry: []byte("e"), Token: 1,
			Generation: statestore.Token{2}, Create: true,
		}},
	}
	f.write(ws...)
	for i, w := range ws {
		if w.Err != nil {
			t.Fatalf("write %d: Err %v", i, w.Err)
		}
	}
	if !ws[2].Applied {
		t.Fatal("store not applied")
	}
	n, trips := tripsOf(p, seq, "EVALSHA")
	if n != 4 || len(trips) != 2 {
		t.Fatalf("%d EVALSHA in %d round trips, want 4 in two", n, len(trips))
	}
	if f.m.call(emit.StateOpPipeline, statestore.ResultOK) != 1 || f.m.call(emit.StateOpCacheSet, statestore.ResultOK) != 1 ||
		f.m.ops[emit.StateOpRefund].n.Load() != 1 || f.m.ops[emit.StateOpCacheSet].n.Load() != 1 {
		t.Fatal("the batch is not counted as one pipeline and one cache_set round trip of one store")
	}
	// A store whose generation step does not apply sends no partition
	// step: one round trip.
	seq = p.Snapshot().Seq
	lost := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
		Key: statestore.CacheKey{URI: d, Partition: d}, Names: "n", Variant: d, TTL: time.Hour, Entry: []byte("e"), Token: 2,
		Generation: statestore.Token{3}, Create: true,
	}}
	f.write(lost)
	if lost.Err != nil || lost.Applied {
		t.Fatalf("losing create: Err %v Applied %v", lost.Err, lost.Applied)
	}
	if n, trips := tripsOf(p, seq, "EVALSHA"); n != 1 || len(trips) != 1 {
		t.Fatalf("%d EVALSHA in %d round trips for a losing create, want 1 in one", n, len(trips))
	}
}

// TestConsumeIsOneRoundTrip checks ADR-0008's round trip rule through the
// counting proxy: one EVALSHA per consumptive group, nothing else on the
// request's path.
func TestConsumeIsOneRoundTrip(t *testing.T) {
	f, p := proxied(t)
	before := p.Snapshot()
	d := statestore.DigestOf("k")
	f.consume(gcraCall(d, oneLimit()), quotaCall(time.Hour, 5, d))
	got := p.Snapshot().Sub(before)
	// Maintenance (INFO, PING) and the setup of a lazily dialed connection
	// (HELLO, CLIENT SETINFO, SCRIPT LOAD) may run beside it; no other
	// data command may.
	if got.Count("EVALSHA") != 1 || got.Count("EVAL") != 0 || got.Count("GET")+got.Count("HMGET")+got.Count("SET") != 0 {
		t.Fatalf("commands %v, want one EVALSHA", got.ByName)
	}
}

// TestMetricsLabels checks the op labels and results of
// ruralz_state_calls_total and the durations: a single op by its kind, a
// write by its kind, a skipped write as skipped without a duration.
func TestMetricsLabels(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	f.consume(quotaCall(time.Hour, 5, d))
	f.write(&statestore.Write{Kind: statestore.OpRefund, Timeout: callTO, Refund: statestore.Refund{Name: "q", Window: time.Hour, WindowStart: start(), Digest: d}})
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Invalidate: d})
	for _, tc := range []struct {
		name string
		op   int
		res  statestore.Result
		dur  uint64
	}{
		{"quota", emit.StateOpQuota, statestore.ResultOK, 1},
		{"refund", emit.StateOpRefund, statestore.ResultOK, 1},
		{"skipped invalidation", emit.StateOpCacheInvalidate, statestore.ResultSkipped, 0},
	} {
		if f.m.call(tc.op, tc.res) != 1 || f.m.dur[tc.op].n.Load() != tc.dur {
			t.Errorf("%s: %d calls, %d durations; want 1 and %d", tc.name, f.m.call(tc.op, tc.res), f.m.dur[tc.op].n.Load(), tc.dur)
		}
	}
	if !f.d.RoundTrips() {
		t.Fatal("RoundTrips false: State Store time would count as gateway-added")
	}
}
