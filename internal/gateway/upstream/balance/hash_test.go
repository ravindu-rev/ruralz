// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"hash/fnv"
	"math/bits"
	"strconv"
	"testing"
)

// stdHash is the reference of 05 req 14: FNV-1a-64 from the standard
// library (OQ-traffic-management-and-resilience-15 (a)), then the
// splitmix64 finalizer.
func stdHash(s string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(s))
	return mix64(h.Sum64())
}

// TestHashKeyMatchesStandardLibrary pins the inlined FNV-1a to hash/fnv and
// the key hash to the virtual node hash (05 req 14: "the key hashes with the
// same function").
func TestHashKeyMatchesStandardLibrary(t *testing.T) {
	for _, s := range []string{"", "a", "user-42", "10.0.0.1:8080#17", "é∑ unicode", string(make([]byte, 300))} {
		if got, want := HashKey(s), stdHash(s); got != want {
			t.Errorf("HashKey(%q) = %#x, want %#x", s, got, want)
		}
	}
	for _, id := range []string{"10.0.0.1:8080", "svc.example:443", ""} {
		prefix := vnodePrefix(id)
		for _, j := range []int{0, 1, 9, 10, 99, 1023, 65535} {
			want := stdHash(id + "#" + strconv.Itoa(j))
			if got := vnodePosition(prefix, j); got != want {
				t.Errorf("vnodePosition(%q, %d) = %#x, want %#x", id, j, got, want)
			}
		}
	}
}

// TestMix64 pins the splitmix64 finalizer to the reference generator's
// first output for seed 0.
func TestMix64(t *testing.T) {
	if got := mix64(0x9e3779b97f4a7c15); got != 0xe220a8397b1dcdaf {
		t.Fatalf("mix64 = %#x, want 0xe220a8397b1dcdaf", got)
	}
}

// seqSource replays values.
type seqSource struct {
	vals []uint64
	i    int
}

func (s *seqSource) Uint64() uint64 {
	v := s.vals[s.i%len(s.vals)]
	s.i++
	return v
}

// TestUint64n covers the unbiased bounded draw behind 05 req 15's uniform
// 64-bit random.
func TestUint64n(t *testing.T) {
	src := pcg(1)
	for _, n := range []uint64{1, 2, 3, 7, 8, 1000, 1 << 40, 1<<63 + 5, ^uint64(0)} {
		for range 1000 {
			if v := uint64n(src, n); v >= n {
				t.Fatalf("uint64n(%d) = %d", n, v)
			}
		}
	}
	// A draw that lands in the biased zone is rejected and redrawn: for n = 3
	// the threshold is 2^64 mod 3 = 1, so a first value whose low product is
	// 0 is refused.
	s := &seqSource{vals: []uint64{0, ^uint64(0)}}
	if v := uint64n(s, 3); v != 2 || s.i != 2 {
		t.Fatalf("uint64n with rejection = %d after %d draws, want 2 after 2", v, s.i)
	}
	hi, _ := bits.Mul64(^uint64(0), 3)
	if hi != 2 {
		t.Fatalf("reference product %d", hi)
	}
	if _, ok := source(nil).(runtimeSource); !ok {
		t.Fatal("a nil source is not the runtime generator")
	}
	if src := pcg(1); source(src) != src {
		t.Fatal("source replaced a given generator")
	}
}
