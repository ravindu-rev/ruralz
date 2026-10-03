// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// mustNew builds a Balancer or fails the test.
func mustNew(t *testing.T, cfg Config) *Balancer {
	t.Helper()
	if cfg.Rand == nil {
		cfg.Rand = pcg(1)
	}
	if cfg.Now.IsZero() {
		cfg.Now = t0
	}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// identityOf returns the identity Select picked in list.
func identityOf(list []Endpoint, c Choice) string { return list[c.Index].Identity }

func TestNewErrors(t *testing.T) {
	if _, err := New(Config{Algorithm: Algorithm(7)}); err == nil {
		t.Fatal("invalid algorithm accepted")
	}
	if _, err := New(Config{Endpoints: []Endpoint{{"b", 1}, {"a", 1}}}); !errors.Is(err, ErrNotNormalized) {
		t.Fatalf("unsorted Endpoints: %v", err)
	}
	b := mustNew(t, Config{Endpoints: eps(2, ones)})
	if err := b.SetEndpoints([]Endpoint{{"x", 1}, {"x", 2}}, nil); !errors.Is(err, ErrNotNormalized) {
		t.Fatalf("duplicate Endpoints: %v", err)
	}
	if b.Len() != 2 || b.Algorithm() != LeastRequest {
		t.Fatalf("a refused SetEndpoints changed the set: %d", b.Len())
	}
}

// TestRemovedExcludedUntilRebuild covers 05 req 16: an Endpoint removed from
// the set counts as excluded at once, the structure rebuilds only after
// RebuildInterval, and the remap follows the current list's indices.
func TestRemovedExcludedUntilRebuild(t *testing.T) {
	for _, alg := range allAlgorithms {
		t.Run(alg.String(), func(t *testing.T) {
			full := eps(6, ones)
			b := mustNew(t, Config{Algorithm: alg, VirtualNodes: 64, Endpoints: full})
			cut := slices.Delete(slices.Clone(full), 2, 3) // ep-00002 leaves
			if err := b.SetEndpoints(cut, nil); err != nil {
				t.Fatal(err)
			}
			if !b.Info().Stale {
				t.Fatal("set change not marked stale")
			}
			src := pcg(4)
			for k := range uint64(3000) {
				c, err := b.Load().Select(k*0x9e3779b97f4a7c15, src, nil)
				if err != nil || c.Index < 0 || c.Index >= len(cut) || c.Grade != Eligible {
					t.Fatalf("Select = %+v, %v", c, err)
				}
			}
			// The remap is by identity: a View that marks the current index of
			// ep-00003 Down keeps it out.
			grades := make([]Grade, len(cut))
			grades[2] = Down // ep-00003 now sits at index 2
			for k := range uint64(2000) {
				c, _ := b.Load().Select(k*0x9e3779b97f4a7c15, src, &view{grades: grades})
				if identityOf(cut, c) == "ep-00003" {
					t.Fatal("remap graded the wrong Endpoint")
				}
			}
			if b.RebuildDue(t0.Add(RebuildInterval - time.Nanosecond)) {
				t.Fatal("rebuild due before the interval")
			}
			if at, ok := b.NextRebuild(); !ok || !at.Equal(t0.Add(RebuildInterval)) {
				t.Fatalf("NextRebuild = %v, %v", at, ok)
			}
			done, err := b.Rebuild(context.Background(), nil, t0.Add(RebuildInterval))
			if !done || err != nil {
				t.Fatalf("Rebuild = %v, %v", done, err)
			}
			info := b.Info()
			if info.Stale || info.Endpoints != 5 || !info.Built.Equal(t0.Add(RebuildInterval)) {
				t.Fatalf("after rebuild: %+v", info)
			}
			if _, ok := b.NextRebuild(); ok {
				t.Fatal("current structure reports a pending rebuild")
			}
			if done, _ := b.Rebuild(context.Background(), nil, t0.Add(time.Hour)); done {
				t.Fatal("rebuilt a current structure")
			}
		})
	}
}

// TestStaleRingMatchesRebuild covers ring stability under churn (WP-22
// "Done when"): until the rebuild, the stale ring walking past a removed
// Endpoint answers every key exactly as the rebuilt ring does.
func TestStaleRingMatchesRebuild(t *testing.T) {
	full := eps(10, ones)
	b := mustNew(t, Config{Algorithm: RingHash, VirtualNodes: 128, Endpoints: full})
	cut := slices.Delete(slices.Clone(full), 3, 4)
	if err := b.SetEndpoints(cut, nil); err != nil {
		t.Fatal(err)
	}
	rebuilt := BuildRing(cut, 128)
	for _, key := range keys(20000) {
		c, err := b.Load().Select(key, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := identityOf(cut, c), ringOwner(rebuilt, key); got != want {
			t.Fatalf("key %#x: stale ring %s, rebuilt ring %s", key, got, want)
		}
	}
	// And keys of the remaining Endpoints never moved.
	before := BuildRing(full, 128)
	for _, key := range keys(20000) {
		if owner := ringOwner(before, key); owner != "ep-00003" {
			c, _ := b.Load().Select(key, nil, nil)
			if identityOf(cut, c) != owner {
				t.Fatalf("key %#x moved from %s", key, owner)
			}
		}
	}
}

// TestAddedJoinAtRebuild covers 05 req 16: new Endpoints wait for the
// rebuild, except that when the structure has nothing as good the current
// set decides, so a healthy newcomer beats panic mode.
func TestAddedJoinAtRebuild(t *testing.T) {
	old := eps(3, ones)
	grown := eps(4, ones) // adds ep-00003
	b := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: old})
	if err := b.SetEndpoints(grown, nil); err != nil {
		t.Fatal(err)
	}
	src := pcg(8)
	for range 1000 {
		c, _ := b.Load().Select(0, src, nil)
		if c.Index == 3 {
			t.Fatal("new Endpoint picked before the rebuild")
		}
	}
	// Every old Endpoint Down: the newcomer is the only Eligible one.
	v := &view{grades: []Grade{Down, Down, Down, Eligible}}
	for range 100 {
		if c, _ := b.Load().Select(0, src, v); c.Index != 3 || c.Grade != Eligible {
			t.Fatalf("Select = %+v, want the healthy newcomer", c)
		}
	}
	if _, err := b.Rebuild(context.Background(), nil, t0.Add(RebuildInterval)); err != nil {
		t.Fatal(err)
	}
	got := counts(4, 4*64, func() int { c, _ := b.Load().Select(0, src, nil); return c.Index })
	if got[3] != 64 {
		t.Fatalf("after the rebuild counts %v", got)
	}
}

// TestUrgentRebuilds covers the rebuilds that do not wait for the interval:
// the first structure over a set that was empty from the start, and a plan
// that shrinks the ring or starts a fallback (05 req 17 "lower v at once").
// A full replacement of the set waits like any other change (05 req 16),
// the current set's table serving meanwhile.
func TestUrgentRebuilds(t *testing.T) {
	ctx := context.Background()
	b := mustNew(t, Config{Algorithm: RingHash, VirtualNodes: 256})
	if b.Len() != 0 {
		t.Fatal("empty Balancer has Endpoints")
	}
	later := t0.Add(time.Second)
	if err := b.SetEndpoints(eps(4, ones), nil); err != nil {
		t.Fatal(err)
	}
	if at, ok := b.NextRebuild(); !ok || !at.Equal(t0) {
		t.Fatalf("NextRebuild = %v, %v; want due since the last build", at, ok)
	}
	// Until then the current table serves (weighted random).
	if c, err := b.Load().Select(HashKey("k"), pcg(1), nil); err != nil || c.Index < 0 {
		t.Fatalf("Select before the first build = %+v, %v", c, err)
	}
	if done, err := b.Rebuild(ctx, nil, later); !done || err != nil {
		t.Fatalf("first build = %v, %v", done, err)
	}
	// Full replacement: nothing of the structure remains, yet the rebuild
	// waits for the interval while the current table serves.
	replaced := []Endpoint{{"new-a", 1}, {"new-b", 1}}
	if err := b.SetEndpoints(replaced, nil); err != nil {
		t.Fatal(err)
	}
	if b.RebuildDue(later.Add(RebuildInterval - time.Nanosecond)) {
		t.Fatal("full replacement rebuilt before the interval")
	}
	if at, ok := b.NextRebuild(); !ok || !at.Equal(later.Add(RebuildInterval)) {
		t.Fatalf("NextRebuild after a full replacement = %v, %v", at, ok)
	}
	for range 100 {
		if c, err := b.Load().Select(HashKey("k"), pcg(2), nil); err != nil || c.Index < 0 || c.Index > 1 {
			t.Fatalf("Select after a full replacement = %+v, %v", c, err)
		}
	}
	// A set that empties and refills later is no first build either.
	if err := b.SetEndpoints(nil, nil); err != nil {
		t.Fatal(err)
	}
	later = later.Add(RebuildInterval)
	if done, err := b.Rebuild(ctx, nil, later); !done || err != nil {
		t.Fatalf("rebuild over the empty set = %v, %v", done, err)
	}
	if err := b.SetEndpoints(replaced, nil); err != nil {
		t.Fatal(err)
	}
	if b.RebuildDue(later) {
		t.Fatal("refilled set rebuilt before the interval")
	}
	later = later.Add(RebuildInterval)
	if _, err := b.Rebuild(ctx, nil, later); err != nil {
		t.Fatal(err)
	}
	// Growing the ring waits; shrinking it does not.
	b.SetPlan(512, false)
	if b.RebuildDue(later) || !b.RebuildDue(later.Add(RebuildInterval)) {
		t.Fatal("growing the ring must wait for the interval")
	}
	b.SetPlan(64, false)
	if !b.RebuildDue(later) {
		t.Fatal("shrinking the ring must be due at once")
	}
	if _, err := b.Rebuild(ctx, nil, later); err != nil {
		t.Fatal(err)
	}
	if info := b.Info(); info.VirtualNodes != 64 || info.Bytes != 16*128 || info.Fallback {
		t.Fatalf("after shrink %+v", info)
	}
	b.SetPlan(64, true)
	if !b.RebuildDue(later) {
		t.Fatal("a fallback must be due at once")
	}
	if _, err := b.Rebuild(ctx, nil, later); err != nil {
		t.Fatal(err)
	}
	info := b.Info()
	if !info.Fallback || info.Effective != Random || info.Algorithm != RingHash || info.VirtualNodes != 0 {
		t.Fatalf("after fallback %+v", info)
	}
	// Leaving the fallback waits for the interval.
	b.SetPlan(64, false)
	if b.RebuildDue(later) || !b.RebuildDue(later.Add(RebuildInterval)) {
		t.Fatal("leaving a fallback must wait for the interval")
	}
	// Other algorithms ignore the plan.
	rr := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: eps(2, ones)})
	rr.SetPlan(64, true)
	if rr.Info().Stale {
		t.Fatal("round-robin reacted to a ring plan")
	}
}

// TestHealthNeverRebuilds covers 05 req 16: selections with any grades
// leave the structure current.
func TestHealthNeverRebuilds(t *testing.T) {
	b := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: eps(3, ones)})
	for _, grades := range [][]Grade{{Down, Down, Down}, {Avoided, Eligible, Down}} {
		for range 50 {
			if _, err := b.Load().Select(0, nil, &view{grades: grades}); err != nil {
				t.Fatal(err)
			}
		}
	}
	if b.Info().Stale || b.RebuildDue(t0.Add(time.Hour)) {
		t.Fatal("health changes made the structure stale")
	}
	// Setting the same set again changes nothing either.
	if err := b.SetEndpoints(eps(3, ones), nil); err != nil {
		t.Fatal(err)
	}
	if b.Info().Stale {
		t.Fatal("an unchanged set made the structure stale")
	}
}

// TestWeightChanges covers weight changes of a kept identity: the old
// weights serve until the rebuild; a weight dropping to 0 excludes the
// Endpoint at once.
func TestWeightChanges(t *testing.T) {
	b := mustNew(t, Config{Algorithm: Random, Endpoints: eps(3, ones)})
	changed := eps(3, func(i int) uint32 { return []uint32{1, 0, 5}[i] })
	if err := b.SetEndpoints(changed, nil); err != nil {
		t.Fatal(err)
	}
	src := pcg(6)
	got := counts(3, 30000, func() int { c, _ := b.Load().Select(0, src, nil); return c.Index })
	if got[1] != 0 || got[0] < 13000 || got[2] < 13000 {
		t.Fatalf("before the rebuild counts %v, want Endpoint 1 excluded and 0, 2 even", got)
	}
	if _, err := b.Rebuild(context.Background(), nil, t0.Add(RebuildInterval)); err != nil {
		t.Fatal(err)
	}
	got = counts(3, 30000, func() int { c, _ := b.Load().Select(0, src, nil); return c.Index })
	if got[1] != 0 || got[2] < 4*got[0] {
		t.Fatalf("after the rebuild counts %v, want 1:0:5", got)
	}
	// All weights 0: every Endpoint weighs 1, Endpoint 1 joining at the
	// next rebuild like any newcomer.
	if err := b.SetEndpoints(eps(3, func(int) uint32 { return 0 }), nil); err != nil {
		t.Fatal(err)
	}
	if got := counts(3, 3000, func() int { c, _ := b.Load().Select(0, src, nil); return c.Index }); got[1] != 0 {
		t.Fatalf("newly weighted Endpoint picked before the rebuild: %v", got)
	}
	if _, err := b.Rebuild(context.Background(), nil, t0.Add(2*RebuildInterval)); err != nil {
		t.Fatal(err)
	}
	got = counts(3, 3000, func() int { c, _ := b.Load().Select(0, src, nil); return c.Index })
	for i, n := range got {
		if n == 0 {
			t.Fatalf("Endpoint %d never picked with all weights 0: %v", i, got)
		}
	}
}

// TestSelectRandom covers 05 req 14's hashKey runtime error: a weighted
// random Endpoint of the current set, graded like Select.
func TestSelectRandom(t *testing.T) {
	list := eps(4, u32) // weights 0, 1, 2, 3
	b := mustNew(t, Config{Algorithm: RingHash, VirtualNodes: 64, Endpoints: list})
	src := pcg(12)
	got := counts(4, 60000, func() int {
		c, err := b.Load().SelectRandom(src, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Index
	})
	if got[0] != 0 || got[3] < got[2] || got[2] < got[1] {
		t.Fatalf("keyless picks %v, want weighted 0:1:2:3", got)
	}
	v := &view{grades: []Grade{Eligible, Down, Down, Avoided}}
	if c, _ := b.Load().SelectRandom(src, v); c.Index != 3 || c.Grade != Avoided {
		t.Fatalf("SelectRandom = %+v, want Endpoint 3 Avoided", c)
	}
}

// TestRebuildGate covers 05 req 16's "at most 8 builds at a time per Node":
// a full gate holds Rebuild until ctx ends, and concurrent Rebuild calls
// build once.
func TestRebuildGate(t *testing.T) {
	gate := NewBuildGate(0)
	for range MaxConcurrentBuilds {
		if !gate.TryAcquire() {
			t.Fatal("gate has fewer than 8 slots")
		}
	}
	if gate.TryAcquire() || gate.InUse() != MaxConcurrentBuilds {
		t.Fatal("gate has more than 8 slots")
	}
	b := mustNew(t, Config{Algorithm: RoundRobin, Endpoints: eps(3, ones)})
	if err := b.SetEndpoints(eps(4, ones), nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	done, err := b.Rebuild(ctx, gate, t0.Add(RebuildInterval))
	if done || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Rebuild on a full gate = %v, %v", done, err)
	}
	for range MaxConcurrentBuilds {
		gate.Release()
	}
	gate.Release() // unpaired; frees nothing on an idle gate (see Release)
	if gate.InUse() != 0 {
		t.Fatalf("gate in use %d", gate.InUse())
	}

	var wg sync.WaitGroup
	results := make(chan bool, 16)
	for range 16 {
		wg.Go(func() {
			done, err := b.Rebuild(context.Background(), gate, t0.Add(RebuildInterval))
			if err != nil {
				t.Error(err)
			}
			results <- done
		})
	}
	wg.Wait()
	close(results)
	var built int
	for done := range results {
		if done {
			built++
		}
	}
	if built != 1 || gate.InUse() != 0 {
		t.Fatalf("%d concurrent builds, gate in use %d", built, gate.InUse())
	}
}

// TestGateAcquire covers BuildGate waiting and cancellation.
func TestGateAcquire(t *testing.T) {
	gate := NewBuildGate(1)
	if err := gate.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.Acquire(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire on a canceled context = %v", err)
	}
	released := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Go(func() { released <- gate.Acquire(context.Background()) })
	gate.Release()
	wg.Wait()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
}

// TestRemap covers the identity merge behind 05 req 16's stale structures:
// removed and newly zero-weight Endpoints map to -1, insertions anywhere in
// the order count as added, and an unchanged list needs no map.
func TestRemap(t *testing.T) {
	p := BuildRandom([]Endpoint{{"b", 1}, {"d", 2}, {"f", 0}, {"h", 3}})
	tests := []struct {
		name        string
		cur         []Endpoint
		wantMap     []int32
		wantPresent int
		wantAdded   int
	}{
		{"same", []Endpoint{{"b", 1}, {"d", 2}, {"f", 0}, {"h", 3}}, nil, 3, 0},
		{"inserted before, between and after", []Endpoint{{"a", 1}, {"b", 1}, {"c", 1}, {"d", 2}, {"f", 0}, {"h", 3}, {"i", 1}}, []int32{1, 3, -1, 5}, 3, 3},
		{"removed", []Endpoint{{"b", 1}, {"h", 3}}, []int32{0, -1, -1, 1}, 2, 0},
		{"weight to zero and from zero", []Endpoint{{"b", 0}, {"d", 2}, {"f", 4}, {"h", 3}}, []int32{-1, 1, -1, 3}, 2, 1},
		{"weight change", []Endpoint{{"b", 1}, {"d", 5}, {"f", 0}, {"h", 3}}, []int32{0, 1, -1, 3}, 3, 0},
		{"empty", nil, []int32{-1, -1, -1, -1}, 0, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			curW, _, _ := effectiveWeights(tt.cur)
			m, present, added := remap(p, tt.cur, curW)
			if !slices.Equal(m, tt.wantMap) || (m == nil) != (tt.wantMap == nil) || present != tt.wantPresent || added != tt.wantAdded {
				t.Fatalf("remap = %v, %d, %d; want %v, %d, %d", m, present, added, tt.wantMap, tt.wantPresent, tt.wantAdded)
			}
		})
	}
	if p.Weight(1) != 2 || p.Weight(2) != 0 || p.Identity(3) != "h" {
		t.Fatalf("accessors: %d %d %q", p.Weight(1), p.Weight(2), p.Identity(3))
	}
}
