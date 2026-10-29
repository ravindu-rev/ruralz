// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
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
// stored variant: the generation, the Vary names, the variant's
// generation and its entry, which a MAC key seals and opens.
func TestLookupReadsTheStoredVariant(t *testing.T) {
	mac := []byte("0123456789abcdef0123456789abcdef")
	f := newFixture(t, fixtureOptions{mac: mac})
	f.ready()
	d := statestore.DigestOf("k")
	k := statestore.CacheKey{URI: d, Partition: statestore.DigestOf("p")}
	v := statestore.DigestOf("v")
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO, Invalidate: d})
	first := lookupCall(k, v)
	f.read(first)
	gen := first.Lookup.Generation
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: callTO, Cache: statestore.CacheStore{
		Key: k, Names: "accept", Variant: v, Generation: gen, TTL: time.Hour, Entry: []byte("body"), Token: 7,
	}}
	f.write(w)
	if w.Err != nil || !w.Applied {
		t.Fatalf("store: Err %v Applied %v", w.Err, w.Applied)
	}
	l := lookupCall(k, v)
	l.Lookup.Entry = make([]byte, 0, 64)
	f.read(l)
	got := l.Lookup
	if !got.Found || string(got.Entry) != "body" || got.Names != "accept" || got.Generation != gen || got.EntryGen != gen {
		t.Fatalf("lookup %+v", got)
	}
	if f.srv.count("EVAL") != 0 {
		t.Fatal("the driver sent EVAL")
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
