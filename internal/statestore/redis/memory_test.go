// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestParseInfo checks the INFO memory fields the driver reads, through a
// RESP3 verbatim prefix and without the policy field.
func TestParseInfo(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		want     reading
	}{
		{
			"full", "# Memory\r\nused_memory:1024\r\nused_memory_human:1K\r\nmaxmemory:4096\r\nmaxmemory_policy:noeviction\r\n",
			reading{used: 1024, max: 4096, policy: "noeviction", ok: true},
		},
		{"verbatim prefix", "txt:# Memory\r\nused_memory:1\r\nmaxmemory:0\r\n", reading{used: 1, ok: true}},
		{"no maxmemory", "# Memory\r\nused_memory:1\r\n", reading{used: 1}},
		{"bad number", "used_memory:x\r\nmaxmemory:10\r\n", reading{max: 10}},
		{"negative", "used_memory:-1\r\nmaxmemory:10\r\n", reading{used: -1, max: 10}},
	} {
		if got := parseInfo(tc.in); got != tc.want {
			t.Errorf("parseInfo(%s) = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

// memSnapshot is a shard's memory state.
type memSnapshot struct {
	read      bool
	used, max int64
	blocked   bool
	budget    int64
}

func (f *fixture) mem() memSnapshot {
	m := &f.d.single.mem
	m.mu.Lock()
	defer m.mu.Unlock()
	return memSnapshot{read: m.read, used: m.used, max: m.max, blocked: m.blocked, budget: m.budget}
}

// readingOf advances the fake clock until the shard holds a reading of
// used bytes.
func (f *fixture) readingOf(used int64, step time.Duration) {
	f.t.Helper()
	f.advanceWhile("the reading", func() bool { m := f.mem(); return m.read && m.used == used }, step)
}

// cacheKey is a partition key for the admission tests.
func cacheKey() statestore.CacheKey {
	return statestore.CacheKey{URI: statestore.DigestOf("u"), Partition: statestore.DigestOf("p")}
}

// TestNoStoreBeforeFirstReading checks that a shard admits no store bytes
// until its first INFO memory reading arrives.
func TestNoStoreBeforeFirstReading(t *testing.T) {
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "INFO", err: "ERR injected"})
	}})
	f.ready()
	waitFor(t, "a reading attempt", func() bool { return f.srv.count("INFO") >= 1 })
	if f.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("a store was admitted before the first reading")
	}
	if st := f.d.Status(); st.EvictionPolicy != statestore.Unknown {
		t.Fatalf("eviction policy %v before any reading, want unknown", st.EvictionPolicy)
	}
}

// TestCacheConnectionAdmitsEveryStore checks that the memory rules 2 and
// 3, which keep cache stores from filling the shards of limit keys, are
// off on a dedicated stateStore.cache connection: it admits stores before
// any reading and above 70% of maxmemory, filling its deployment.
func TestCacheConnectionAdmitsEveryStore(t *testing.T) {
	f := newFixture(t, fixtureOptions{role: statestore.RoleCache, fake: fakeOptions{info: memInfo(9_000, 10_000, volatileTTL)}, setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "INFO", err: "ERR injected", times: 1})
	}})
	f.ready()
	if !f.d.AdmitStoreBytes(cacheKey(), 1<<20) {
		t.Fatal("the cache connection refused a store before its first reading")
	}
	f.readingOf(9_000, time.Second)
	if !f.d.AdmitStoreBytes(cacheKey(), 1<<20) {
		t.Fatal("the cache connection refused a store above 70% of maxmemory")
	}
	main := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(9_000, 10_000, noEviction)}})
	main.ready()
	main.readingOf(9_000, 0)
	if main.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("the main connection admitted a store above 70% of maxmemory")
	}
}

// TestFailedReadingRetriedAfterOneSecond checks that a failed reading,
// the first included, is retried after the 1 s fast interval, not at
// every maintenance pass.
func TestFailedReadingRetriedAfterOneSecond(t *testing.T) {
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "INFO", err: "ERR injected"})
	}})
	f.ready()
	waitFor(t, "the first reading attempt", func() bool { return f.srv.count("INFO") == 1 && !f.d.single.mem.due(f.clk.Now()) })
	if got := f.nextReading(); got < time.Second || got > time.Second+250*time.Millisecond {
		t.Fatalf("failed reading retried after %v, want 1 s", got)
	}
}

// TestStoreBudgetPerReading checks rule 3: between readings a shard
// admits min(2% of maxmemory per 10 s / N_c, 4 MiB per second) bytes, N_c
// being 1,000 without a published Node count, and the next reading
// refills it.
func TestStoreBudgetPerReading(t *testing.T) {
	const maxMem = 10_000_000_000 // 2% per 10 s / 1,000 = 20,000 bytes per second
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(1_000_000_000, maxMem, noEviction)}})
	f.ready()
	f.readingOf(1_000_000_000, 0)
	if !f.d.AdmitStoreBytes(cacheKey(), 200_000) {
		t.Fatal("the 10 s budget of 200,000 bytes was refused")
	}
	if f.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("a store beyond the budget was admitted")
	}
	f.srv.setInfo(memInfo(1_000_000_001, maxMem, noEviction))
	f.readingOf(1_000_000_001, time.Second)
	if !f.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("the next reading did not refill the budget")
	}
}

// TestStoreRateCapped checks the 4 MiB per second cap of rule 3.
func TestStoreRateCapped(t *testing.T) {
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(10_000_000, 1<<50, noEviction)}})
	f.ready()
	f.readingOf(10_000_000, 0)
	if m := f.mem(); m.budget != 10*(4<<20) {
		t.Fatalf("budget %d, want 10 s at 4 MiB per second", m.budget)
	}
}

// TestStoresSkipAbove70Percent checks rule 2: no store above 70% of
// maxmemory, and readings every 1 s above 50%.
func TestStoresSkipAbove70Percent(t *testing.T) {
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(7_100, 10_000, noEviction)}})
	f.ready()
	f.readingOf(7_100, 0)
	if f.d.AdmitStoreBytes(cacheKey(), 0) {
		t.Fatal("a store was admitted above 70% of maxmemory")
	}
	if got := f.nextReading(); got < time.Second || got > time.Second+250*time.Millisecond {
		t.Fatalf("next reading after %v above 50%%, want 1 s", got)
	}
}

// nextReading advances the fake clock in maintenance ticks until the next
// INFO arrives and returns the fake time it took.
func (f *fixture) nextReading() time.Duration {
	f.t.Helper()
	from, infos := f.clk.Now(), f.srv.count("INFO")
	f.advanceWhile("the next reading", func() bool { return f.srv.count("INFO") > infos }, 250*time.Millisecond)
	return f.clk.Now().Sub(from)
}

// TestPollCadence checks that readings come every 10 s at or below 50%
// of maxmemory.
func TestPollCadence(t *testing.T) {
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(100, 10_000, noEviction)}})
	f.ready()
	f.readingOf(100, 0)
	if got := f.nextReading(); got < 10*time.Second || got > 10*time.Second+250*time.Millisecond {
		t.Fatalf("next reading after %v at 1%%, want 10 s", got)
	}
}

// TestStoresBlockWhileGrowing checks rule 2's growth guard: a shard that
// grew by more than 10% of maxmemory between readings admits no store
// until its growth falls under 2%.
func TestStoresBlockWhileGrowing(t *testing.T) {
	const maxMem = 10_000_000_000
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(1_000_000_000, maxMem, noEviction)}})
	f.ready()
	f.readingOf(1_000_000_000, 0)
	f.srv.setInfo(memInfo(2_500_000_000, maxMem, noEviction))
	f.readingOf(2_500_000_000, time.Second)
	if m := f.mem(); !m.blocked || f.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("a store was admitted after 15% growth")
	}
	f.srv.setInfo(memInfo(2_900_000_000, maxMem, noEviction))
	f.readingOf(2_900_000_000, time.Second)
	if !f.mem().blocked {
		t.Fatal("4% growth cleared the block")
	}
	f.srv.setInfo(memInfo(2_950_000_000, maxMem, noEviction))
	f.readingOf(2_950_000_000, time.Second)
	if f.mem().blocked || !f.d.AdmitStoreBytes(cacheKey(), 1) {
		t.Fatal("growth under 2% did not clear the block")
	}
}

// TestMaxmemoryZeroAdmitsNothing checks that a shard without maxmemory
// admits no store, which the driver warns about once.
func TestMaxmemoryZeroAdmitsNothing(t *testing.T) {
	f := newFixture(t, fixtureOptions{fake: fakeOptions{info: memInfo(100, 0, noEviction)}})
	f.ready()
	f.readingOf(100, 0)
	if f.d.AdmitStoreBytes(cacheKey(), 0) {
		t.Fatal("a store was admitted with maxmemory 0")
	}
	f.srv.setInfo(memInfo(101, 0, noEviction))
	f.readingOf(101, time.Second)
	if got := f.logs.count("state store reports maxmemory 0; Response Cache stores skip"); got != 1 {
		t.Fatalf("%d maxmemory warnings, want 1", got)
	}
}
