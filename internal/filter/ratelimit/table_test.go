// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"context"
	"errors"
	"math/rand/v2"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Tests for the key table: its sizes, CLOCK eviction per segment, the
// eviction counts, the index under insertions and removals, and the limit
// registry.

// TestEntrySize checks the 64-byte entry behind the table's about 64 MiB
// for 1,048,576 entries (target).
func TestEntrySize(t *testing.T) {
	if s := reflect.TypeFor[entry]().Size(); s != 64 {
		t.Fatalf("entry is %d bytes, want 64", s)
	}
}

// TestTableDefaults checks the target sizes: 1,048,576 entries in 256
// shards, 131,072 of them local-only.
func TestTableDefaults(t *testing.T) {
	tbl, err := NewTable(clocktest.New(testStart()), nil, TableOptions{Seed: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(tbl.shards) != DefaultShards {
		t.Fatalf("%d shards, want %d", len(tbl.shards), DefaultShards)
	}
	main, local := 0, 0
	for i := range tbl.shards {
		main += len(tbl.shards[i].seg[segMain].entries)
		local += len(tbl.shards[i].seg[segLocalOnly].entries)
	}
	if main+local != DefaultEntries || local != DefaultLocalOnlyEntries {
		t.Fatalf("%d main and %d local-only entries, want %d in all with %d local-only", main, local, DefaultEntries, DefaultLocalOnlyEntries)
	}
	if tbl.Name() != TableName {
		t.Errorf("Name() = %q", tbl.Name())
	}
	var _ filter.Component = tbl
}

// TestTableTooSmall checks that a table whose segments cannot hold one
// request's limits beside an entry to evict is refused.
func TestTableTooSmall(t *testing.T) {
	if _, err := NewTable(nil, nil, TableOptions{Entries: 16, LocalOnlyEntries: 8, Shards: 1}); !errors.Is(err, ErrTableSize) {
		t.Fatalf("NewTable of 8 entries per segment = %v, want ErrTableSize", err)
	}
}

// TestTableRunWaitsForCancel checks the Component's Run: it returns nil
// once its context ends.
func TestTableRunWaitsForCancel(t *testing.T) {
	tbl := smallTable(t, clocktest.New(testStart()), nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- tbl.Run(ctx) }()
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
}

// TestClockEvictionCountsPerSegment checks CLOCK eviction: a full main
// segment evicts for a new key with a GCRA answer, a local-only flood
// evicts only local-only entries, and each eviction counts for the evicted
// entry's Policy and segment.
func TestClockEvictionCountsPerSegment(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(100, time.Second))
	cfg.LocalOnly = ptr(true)
	a, ma := fx.build(t, "a", cfg)
	b, mb := fx.build(t, "b", keyed(limit(100, time.Second)))
	// Policy a fills the main segment of 20 entries (localOnly keys live
	// there) and one more key evicts one of them.
	for i := range 21 {
		fx.request(t, a, vars("k"+strconv.Itoa(i), ""))
	}
	if got := ma.evictions[segMain].value(); got != 1 {
		t.Fatalf("a's main evictions = %d, want 1", got)
	}
	// Policy b spends the first-seen budget, then floods the local-only
	// segment: 25 keys past the budget evict 5 local-only entries and no
	// main one.
	for i := range FirstSeenNoCount {
		fx.request(t, b, vars("b"+strconv.Itoa(i), ""))
	}
	mainA := ma.evictions[segMain].value()
	before := mb.evictions[segMain].value() + ma.evictions[segMain].value()
	for i := range 25 {
		fx.request(t, b, vars("flood"+strconv.Itoa(i), ""))
	}
	if got := mb.evictions[segLocalOnly].value(); got != 5 {
		t.Errorf("b's local-only evictions = %d, want 5", got)
	}
	if after := mb.evictions[segMain].value() + ma.evictions[segMain].value(); after != before || ma.evictions[segMain].value() != mainA {
		t.Errorf("the local-only flood evicted %d main entries", after-before)
	}
}

// TestClockSparesReferencedEntries checks that CLOCK passes over an entry
// used since the hand last passed: the hot key keeps its bucket while
// cold keys are evicted.
func TestClockSparesReferencedEntries(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(100, time.Second))
	cfg.LocalOnly = ptr(true)
	f, _ := fx.build(t, "a", cfg)
	fx.request(t, f, vars("hot", ""))
	fx.request(t, f, vars("hot", ""))
	for i := range 60 {
		fx.request(t, f, vars("cold"+strconv.Itoa(i), ""))
		fx.request(t, f, vars("hot", ""))
	}
	if n, ok := fx.tokens(f, "hot", 0); !ok || n != 100-62 {
		t.Fatalf("hot key holds %d (%v), want its bucket kept at %d", n, ok, 100-62)
	}
}

// TestIndexMatchesModel drives one shard with random insertions, lookups
// and removals and compares it with a map after every step: every held
// key is found at its entry, every removed key is gone, so backward-shift
// removal never breaks a probe run.
func TestIndexMatchesModel(t *testing.T) {
	tbl, err := NewTable(clocktest.New(testStart()), nil, TableOptions{Entries: 600, LocalOnlyEntries: 100, Shards: 1, Seed: 3})
	if err != nil {
		t.Fatal(err)
	}
	sh := &tbl.shards[0]
	rng := rand.New(rand.NewPCG(1, 2))
	model := map[[2]uint64]bool{}
	keyOf := func(n uint64) (uint32, statestore.Digest) {
		return uint32(n%5) + 1, statestore.DigestOf(strconv.FormatUint(n, 10))
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for step := range 20_000 {
		n := rng.Uint64N(800)
		id, d := keyOf(n)
		k := [2]uint64{n, 0}
		slot, v := tbl.find(sh, id, &d)
		if (v != 0) != model[k] {
			t.Fatalf("step %d: key %d found %v, model %v", step, n, v != 0, model[k])
		}
		switch {
		case v != 0 && rng.IntN(2) == 0:
			tbl.remove(sh, slot, v)
			delete(model, k)
		case v == 0 && len(model) < 400:
			seg := segMain
			if rng.IntN(5) == 0 {
				seg = segLocalOnly
			}
			if free := len(sh.seg[seg].free) + len(sh.seg[seg].entries) - sh.seg[seg].used; free == 0 {
				continue
			}
			nv := tbl.insert(sh, seg, id, &d, 0)
			if e := sh.entry(nv); e.limit != id || e.digest != d {
				t.Fatalf("step %d: inserted entry holds another key", step)
			}
			model[k] = true
		}
	}
	for k := range model {
		id, d := keyOf(k[0])
		if _, v := tbl.find(sh, id, &d); v == 0 {
			t.Fatalf("held key %d is not found", k[0])
		}
	}
}

// TestRegistryIDs checks the limit registry: one id per Policy, requests
// and window, kept while a Filter holds it and never reused.
func TestRegistryIDs(t *testing.T) {
	tbl := smallTable(t, clocktest.New(testStart()), nil)
	var c [emit.NumBucketSegments]emit.Counter
	k1 := limitKey{policy: "a", requests: 10, window: time.Second}
	k2 := limitKey{policy: "a", requests: 10, window: time.Minute}
	id1 := tbl.register(k1, &c)
	if tbl.register(k1, &c) != id1 || tbl.register(k2, &c) == id1 {
		t.Fatal("register gave a held limit another id, or two limits one id")
	}
	tbl.release(k1)
	if tbl.limits() != 2 {
		t.Fatalf("%d limits after one of two releases, want 2", tbl.limits())
	}
	tbl.release(k1)
	tbl.release(k1)
	if tbl.limits() != 1 {
		t.Fatalf("%d limits after k1's last release, want 1", tbl.limits())
	}
	if id := tbl.register(k1, &c); id == id1 {
		t.Fatal("a forgotten limit got its old id back")
	}
}

// TestBucketsSeparatePolicies checks that two Policies
// with equal limits keep separate buckets for one key value.
func TestBucketsSeparatePolicies(t *testing.T) {
	fx := newFixture(t, nil)
	cfg := keyed(limit(1, time.Second))
	cfg.LocalOnly = ptr(true)
	a, _ := fx.build(t, "a", cfg)
	b, _ := fx.build(t, "b", cfg)
	fx.request(t, a, vars("k", ""))
	if o := fx.request(t, b, vars("k", "")); o.r.Outcome != filter.Continue {
		t.Fatalf("Policy b's first request = %+v, want its own bucket", o.r)
	}
	if o := fx.request(t, a, vars("k", "")); o.code() != CodeLocalDeny {
		t.Fatalf("Policy a's second request = %s, want RZ-RL-001", o.code())
	}
}

// TestLocalOnlyExpiryFreesEntry checks that an ended local-only entry is
// removed when read, so its slot is reused before CLOCK evicts.
func TestLocalOnlyExpiryFreesEntry(t *testing.T) {
	fx := newFixture(t, nil)
	f, m := fx.build(t, "rl", keyed(limit(5, time.Second)))
	for i := range FirstSeenNoCount {
		fx.request(t, f, vars("w"+strconv.Itoa(i), ""))
	}
	fx.request(t, f, vars("late", ""))
	fx.clk.Advance(LocalOnlyLife)
	fx.table.budgetMu.Lock()
	fx.table.budgetTokens = 0
	fx.table.budgetLast = fx.clk.Now().UnixNano()
	fx.table.budgetMu.Unlock()
	fx.request(t, f, vars("late", ""))
	sh := &fx.table.shards[0]
	sh.mu.Lock()
	used, free := sh.seg[segLocalOnly].used, len(sh.seg[segLocalOnly].free)
	sh.mu.Unlock()
	if used != 1 || free != 0 || m.evictions[segLocalOnly].value() != 0 {
		t.Fatalf("local-only segment used %d, free %d, evictions %d; want the ended entry's slot reused", used, free, m.evictions[segLocalOnly].value())
	}
}
