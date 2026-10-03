// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// Tests for spec 08 req 45 details beyond the conformance suite
// (variant order, partition TTLs, lease edge cases, generation reuse) and
// req 53 (Response Cache store admission on the memory reading).

func TestAdmitStoreBytes(t *testing.T) {
	// Spec 08 reqs 53 and 58, spec 05 req 84, with N_c = 1 for the memory
	// driver (R-72). maxmemory 1,000,000,000: c = min(2% of maxmemory per
	// 10 s / N_c, 4 MiB/s) = 2,000,000 bytes per second, so 20,000,000
	// bytes between readings every 10 s, or 2,000,000 every 1 s above 50%.
	const mb = 1_000_000 // 0.1% of maxmemory
	f := newFixture(t, Options{MaxCacheBytes: 1000 * mb}, nil)
	defer f.d.bytes.Store(0)
	k := statestore.CacheKey{}
	steps := []struct {
		name    string
		advance time.Duration
		used    int64 // cache bytes before the step
		n       int
		want    bool
	}{
		{"first reading, within budget", 0, 0, 15_000_000, true},
		{"over the remaining budget", 0, 0, 6_000_000, false},
		{"rest of the budget", 0, 0, 5_000_000, true},
		{"budget spent", 0, 0, 1, false},
		{"no reading yet: still spent", 9 * time.Second, 0, 1, false},
		{"new reading renews the budget", time.Second, 0, 20_000_000, true},
		{"grew 15% of maxmemory: skip", 10 * time.Second, 150 * mb, 1, false},
		{"grew 5%: still skipping until under 2%", 10 * time.Second, 200 * mb, 1, false},
		{"grew 1%: admits again", 10 * time.Second, 210 * mb, 1, true},
		{"grew 9%: no skip", 10 * time.Second, 300 * mb, 1, true},
		{"grew 9% again", 10 * time.Second, 390 * mb, 1, true},
		{"grew 9% once more", 10 * time.Second, 480 * mb, 1, true},
		{"above 50%: 1 s readings, 2,000,000 bytes", 10 * time.Second, 510 * mb, 2_000_001, false},
		{"above 50%: the 2,000,000 bytes", 0, 510 * mb, 2_000_000, true},
		{"above 50%: next reading after 1 s", time.Second, 515 * mb, 1_999_999, true},
		{"above 70%: skip", time.Second, 710 * mb, 1, false},
		{"back under 70%", time.Second, 600 * mb, 0, true},
	}
	for _, s := range steps {
		f.clk.Advance(s.advance)
		f.d.bytes.Store(s.used)
		if got := f.d.AdmitStoreBytes(k, s.n); got != s.want {
			t.Fatalf("%s: AdmitStoreBytes(%d) = %v, want %v", s.name, s.n, got, s.want)
		}
	}
	// The default 64 MiB store admits 64 MiB × 2% / 10 s / N_c (N_c = 1) =
	// 134,217 bytes per second in whole bytes: 1,342,170 bytes per 10 s
	// reading.
	def := newFixture(t, Options{}, nil)
	if !def.d.AdmitStoreBytes(k, 1_342_170) || def.d.AdmitStoreBytes(k, 1) {
		t.Fatal("default store budget is not 1,342,170 bytes per 10 s reading")
	}
	// The 4 MiB per second cap applies from 2,097,152,000 bytes of
	// maxmemory (2% / 10 s of it is 4 MiB): 1,000,000,000 is under it
	// (above), 4 GiB is over it and admits 40 MiB per 10 s reading.
	big := newFixture(t, Options{MaxCacheBytes: 4 << 30}, nil)
	if !big.d.AdmitStoreBytes(k, 40<<20) || big.d.AdmitStoreBytes(k, 1) {
		t.Fatal("per-second cap of 4 MiB not applied to 10 s of budget")
	}
}

func TestVariantOrder(t *testing.T) {
	// Spec 08 req 45: at most 8 variants in store order; re-storing a
	// variant makes it the newest.
	f := newFixture(t, Options{}, nil)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	v := func(i int) statestore.Digest { return statestore.DigestOf(strconv.Itoa(i)) }
	var token uint64
	put := func(i int) {
		t.Helper()
		f.clk.Advance(1001 * time.Millisecond)
		token++
		if w := f.store(ck, "n", v(i), []byte(strconv.Itoa(i)), token); !w.Applied {
			t.Fatalf("store %d not applied", i)
		}
	}
	for i := range 8 {
		put(i)
	}
	put(0) // variant 0 becomes the newest
	put(8) // evicts variant 1, the oldest
	for i, want := range []bool{true, false, true, true, true, true, true, true, true} {
		c := f.lookup(ck, v(i))
		if c.Lookup.Found != want {
			t.Fatalf("variant %d found %v, want %v", i, c.Lookup.Found, want)
		}
		if want && string(c.Lookup.Entry) != strconv.Itoa(i) {
			t.Fatalf("variant %d entry %v", i, c.Lookup.Entry)
		}
	}
}

func TestPartitionTTL(t *testing.T) {
	// Spec 08 reqs 45 and 50: the partition expires at the larger of its
	// remaining TTL and the entry TTL, capped at 25 h; never without a TTL.
	f := newFixture(t, Options{}, nil)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	ttl := func() time.Duration {
		s := f.d.shardOf(int(keys.PartitionSlot(ck)))
		s.mu.Lock()
		defer s.mu.Unlock()
		return time.Duration(s.part[ck].exp-instantOf(f.clk.Now()).ms) * time.Millisecond
	}
	put := func(v string, d time.Duration) {
		t.Helper()
		f.clk.Advance(2 * time.Second)
		w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{
			Key: ck, Names: "n", Variant: statestore.DigestOf(v), TTL: d, Entry: []byte(v), Token: 1,
		}}
		f.write(w)
		if !w.Applied {
			t.Fatalf("store %s not applied", v)
		}
	}
	put("a", 10*time.Hour)
	if got := ttl(); got != 10*time.Hour {
		t.Fatalf("TTL %v, want 10h", got)
	}
	put("b", time.Hour)
	if got := ttl(); got != 10*time.Hour-2*time.Second {
		t.Fatalf("shorter entry shortened the partition: %v", got)
	}
	put("c", 20*time.Hour)
	if got := ttl(); got != 20*time.Hour {
		t.Fatalf("TTL %v, want 20h", got)
	}
	put("d", 100*time.Hour)
	if got := ttl(); got != 25*time.Hour {
		t.Fatalf("TTL %v, want the 25h cap", got)
	}
	put("e", 0)
	if got := ttl(); got != 25*time.Hour-2*time.Second {
		t.Fatalf("zero entry TTL: %v", got)
	}
	// A fresh partition with a zero TTL still expires (1 ms).
	other := statestore.CacheKey{URI: statestore.DigestOf("other")}
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{Key: other, Variant: other.URI, Entry: []byte("x"), Token: 1}}
	f.write(w)
	f.clk.Advance(2 * time.Millisecond)
	if f.lookup(other, other.URI).Lookup.Found {
		t.Fatal("zero-TTL partition never expired")
	}
}

func TestLeaseEdges(t *testing.T) {
	// Refresh needs the lease, the partition and the variant; end-stale
	// with the token succeeds even when the variant is gone; a refresh
	// extends the partition TTL.
	f := newFixture(t, Options{}, nil)
	ck := statestore.CacheKey{URI: statestore.DigestOf("u")}
	v, gone := statestore.DigestOf("v"), statestore.DigestOf("gone")
	lease := func(mode statestore.LeaseMode, variant statestore.Digest, token uint64, ttl time.Duration) bool {
		t.Helper()
		w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{
			Key: ck, Variant: variant, Mode: mode, Token: token, Entry: []byte("fresh"), TTL: ttl,
		}}
		f.write(w)
		if w.Err != nil {
			t.Fatalf("lease op: %v", w.Err)
		}
		return w.Applied
	}
	if !lease(statestore.LeaseTake, v, 1, 0) {
		t.Fatal("take on an empty partition")
	}
	if lease(statestore.LeaseRefresh, v, 1, time.Hour) {
		t.Fatal("refresh without a partition applied")
	}
	if !lease(statestore.LeaseEndStale, v, 1, 0) {
		t.Fatal("end-stale with the token but no variant not applied")
	}
	f.clk.Advance(6 * time.Second)
	f.store(ck, "n", v, []byte("old"), 2)
	f.clk.Advance(2 * time.Second)
	lease(statestore.LeaseTake, v, 3, 0)
	if lease(statestore.LeaseRefresh, gone, 3, time.Hour) {
		t.Fatal("refresh of a missing variant applied")
	}
	if !lease(statestore.LeaseRefresh, v, 3, 20*time.Hour) {
		t.Fatal("refresh not applied")
	}
	s := f.d.shardOf(int(keys.PartitionSlot(ck)))
	s.mu.Lock()
	exp := s.part[ck].exp - instantOf(f.clk.Now()).ms
	s.mu.Unlock()
	if exp != (20 * time.Hour).Milliseconds() {
		t.Fatalf("refresh set TTL %d ms, want 20h", exp)
	}
	if c := f.lookup(ck, v); string(c.Lookup.Entry) != "fresh" {
		t.Fatalf("entry %q after refresh", c.Lookup.Entry)
	}
}

func TestGenerationNeverReused(t *testing.T) {
	// Spec 08 req 45: when the stored generation is not below the server
	// time (a clock that stepped back), the bump is stored + 1.
	f := newFixture(t, Options{}, nil)
	uri := statestore.DigestOf("u")
	future := instantOf(f.clk.Now()).unixUs + 1_000_000
	s := f.d.shardOf(int(keys.DigestSlot(uri)))
	s.mu.Lock()
	s.gen[uri] = countEntry{n: future, exp: instantOf(f.clk.Now()).ms + 1000}
	s.mu.Unlock()
	f.write(&statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: time.Second, Invalidate: uri})
	if g := f.lookup(statestore.CacheKey{URI: uri}, uri).Lookup.Generation; g != future+1 {
		t.Fatalf("generation %d, want %d", g, future+1)
	}
}
