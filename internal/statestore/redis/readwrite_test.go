// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestReadOtherKindIsAnErrorReply checks Read's contract: a call of
// another kind fails with RZ-STS-002 while the lookups of the batch are
// answered, and the batch counts as a failed pipeline.
func TestReadOtherKindIsAnErrorReply(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	l := lookupCall(statestore.CacheKey{URI: d, Partition: d}, d)
	g := gcraCall(d, oneLimit())
	f.read(l, g)
	if l.Err != nil || !l.Done || g.Err != statestore.ErrFailed || g.Done {
		t.Fatalf("lookup Done %v Err %v; gcra Done %v Err %v", l.Done, l.Err, g.Done, g.Err)
	}
	if f.m.call(emit.StateOpPipeline, statestore.ResultError) != 1 {
		t.Fatal("the batch is not counted as a failed pipeline")
	}
	only := gcraCall(d, oneLimit())
	f.read(only)
	if only.Err != statestore.ErrFailed || f.m.call(emit.StateOpPipeline, statestore.ResultError) != 2 {
		t.Fatalf("a batch without lookups: Err %v", only.Err)
	}
}

// TestLookupReadsTheStoredVariant checks the lookup's decoding of a
// stored variant: the generation token, the Vary names, the entry, which
// a MAC key seals and opens, and its resident time, the shard's TIME
// minus the store time less the write's receipt delay.
func TestLookupReadsTheStoredVariant(t *testing.T) {
	mac := []byte("0123456789abcdef0123456789abcdef")
	f := newFixture(t, fixtureOptions{mac: mac})
	f.ready()
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: statestore.DigestOf("p")}
	v := statestore.DigestOf("v")
	tok := statestore.Token{0xab, 0xcd}
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d, Bump: tok})
	first := lookupCall(k, v)
	f.read(first)
	if !first.Lookup.HasToken || first.Lookup.Token != tok || first.Lookup.Partition {
		t.Fatalf("lookup after the bump: %+v", first.Lookup)
	}
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
		Key: k, Names: "accept", Variant: v, Generation: tok, TTL: time.Hour, Entry: []byte("body"), Token: 7,
		Received: f.clk.Now().Add(-1200 * time.Millisecond),
	}}
	f.write(w)
	if w.Err != nil || !w.Applied {
		t.Fatalf("store: Err %v Applied %v", w.Err, w.Applied)
	}
	f.clk.Advance(300 * time.Millisecond)
	l := lookupCall(k, v)
	l.Lookup.Entry = make([]byte, 0, 64)
	f.read(l)
	got := l.Lookup
	if !got.Found || string(got.Entry) != "body" || !got.Partition || got.Names != "accept" || got.Token != tok || got.Resident != 1500*time.Millisecond {
		t.Fatalf("lookup %+v, want 1.5 s resident", got)
	}
	if f.srv.count("EVAL") != 0 {
		t.Fatal("the driver sent EVAL")
	}
}

// TestLookupRefusesAGarbledToken checks that a generation key holding
// anything but a token, which no Node writes, is an unparsable reply
// (RZ-STS-002), never a miss that a create would race.
func TestLookupRefusesAGarbledToken(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: d}
	f.srv.mu.Lock()
	f.srv.set(string(keys.AppendCacheGeneration(nil, d)), "1790380800000123", 0)
	f.srv.mu.Unlock()
	l := lookupCall(k, d)
	f.read(l)
	if l.Err != statestore.ErrFailed || l.Done {
		t.Fatalf("garbled token: Done %v Err %v, want RZ-STS-002", l.Done, l.Err)
	}
}

// TestStoreGenerationSteps checks the generation step of a store and of
// the lease writes against the library: a store whose lookup found no key
// creates it (SET NX) and loses to a key that appeared; a store, refresh
// or end-stale under another token than the key's writes nothing; the
// bump sets the token the Node passes.
func TestStoreGenerationSteps(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: statestore.DigestOf("p")}
	v := statestore.DigestOf("v")
	store := func(tok statestore.Token, create bool, lease uint64) *statestore.Write {
		w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
			Key: k, Names: "n", Variant: v, TTL: time.Hour, Entry: []byte("e"), Token: lease, Generation: tok, Create: create,
		}}
		f.write(w)
		if w.Err != nil {
			t.Fatalf("store: %v", w.Err)
		}
		return w
	}
	one, two := statestore.Token{1}, statestore.Token{2}
	if !store(one, true, 1).Applied {
		t.Fatal("create on a missing key not applied")
	}
	f.clk.Advance(1100 * time.Millisecond)
	if store(two, true, 2).Applied {
		t.Fatal("create on an existing key applied")
	}
	if store(two, false, 3).Applied {
		t.Fatal("store under another token applied")
	}
	if !store(one, false, 4).Applied {
		t.Fatal("store under the key's token not applied")
	}
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d, Bump: two})
	f.clk.Advance(1100 * time.Millisecond)
	take := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: k, Mode: statestore.LeaseTake, Token: 9}}
	refresh := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: k, Variant: v, Mode: statestore.LeaseRefresh, Token: 9, Generation: one, Entry: []byte("x"), TTL: time.Hour}}
	end := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: k, Variant: v, Mode: statestore.LeaseEndStale, Token: 9, Generation: one}}
	f.write(take, refresh, end)
	if !take.Applied || refresh.Applied || end.Applied || refresh.Err != nil || end.Err != nil {
		t.Fatalf("after the bump: take %+v refresh %+v end %+v; want the take only", *take, *refresh, *end)
	}
	l := lookupCall(k, v)
	f.read(l)
	if !l.Lookup.Found || string(l.Lookup.Entry) != "e" || l.Lookup.Token != two {
		t.Fatalf("lookup %+v: the old entry's lease writes changed it", l.Lookup)
	}
}

// TestWriteRefusesUnknownKind checks that a write of another kind or an
// unknown lease mode fails with RZ-STS-002 without a call, while the rest
// of its batch is written.
func TestWriteRefusesUnknownKind(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	d := statestore.DigestOf("k")
	bad := &statestore.Write{Kind: statestore.OpGCRA, Timeout: callTO}
	mode := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Lease: statestore.CacheLease{Key: statestore.CacheKey{URI: d}, Mode: 9}}
	good := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d}
	f.write(bad, mode, good)
	if bad.Err != statestore.ErrFailed || mode.Err != statestore.ErrFailed || good.Err != nil || !good.Applied {
		t.Fatalf("bad %v, mode %v, good %v %v", bad.Err, mode.Err, good.Err, good.Applied)
	}
	if f.srv.count("EVALSHA") != 1 {
		t.Fatalf("%d EVALSHA, want only the invalidation", f.srv.count("EVALSHA"))
	}
}

// TestWriteTimeoutIsTheSmallest checks that a batch's round trip waits for
// the smallest timeout of its writes, each of which then fails with
// RZ-STS-001.
func TestWriteTimeoutIsTheSmallest(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{}), times: 2})
	defer close(hold.hold)
	d := statestore.DigestOf("k")
	long := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Hour, Invalidate: d}
	short := &statestore.Write{Kind: statestore.OpRefund, Timeout: 20 * time.Millisecond, Refund: statestore.Refund{Name: "q", Window: time.Hour, WindowStart: start(), Digest: d}}
	f.advanceUntil(background(func() { f.write(long, short) }), 5*time.Millisecond)
	if long.Err != statestore.ErrTimeout || short.Err != statestore.ErrTimeout {
		t.Fatalf("writes: %v %v, want RZ-STS-001", long.Err, short.Err)
	}
	if f.clk.Now().Sub(start()) >= time.Hour {
		t.Fatal("the batch waited for its longest timeout")
	}
}
