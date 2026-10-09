// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"strconv"
	"testing"
)

// benchSelect measures one Endpoint selection (05 test plan item 38,
// BenchmarkPick{RoundRobin,LeastRequest,RingHash,Random}) over 16 Endpoints
// with one Down and one Avoided, the runtime random source and a key per
// request.
func benchSelect(b *testing.B, alg Algorithm) {
	bal, err := New(Config{Algorithm: alg, VirtualNodes: MaxVirtualNodes, Endpoints: eps(16, ones), Now: t0})
	if err != nil {
		b.Fatal(err)
	}
	grades := make([]Grade, 16)
	grades[3], grades[9] = Down, Avoided
	v := &staticView{grades: grades, loads: []int64{3, 1, 4, 1, 5, 9, 2, 6, 5, 3, 5, 8, 9, 7, 9, 3}}
	ks := make([]uint64, 1024)
	for i := range ks {
		ks[i] = HashKey("user-" + strconv.Itoa(i))
	}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			if _, err := bal.Load().Select(ks[i&1023], nil, v); err != nil {
				b.Fatal(err)
			}
			i++
		}
	})
}

func BenchmarkPickRoundRobin(b *testing.B)   { benchSelect(b, RoundRobin) }
func BenchmarkPickLeastRequest(b *testing.B) { benchSelect(b, LeastRequest) }
func BenchmarkPickRingHash(b *testing.B)     { benchSelect(b, RingHash) }
func BenchmarkPickRandom(b *testing.B)       { benchSelect(b, Random) }

// BenchmarkSplitPick measures the onRoute weighted leg pick (05 req 10:
// 1 µs p50, 4 µs p99, 0 allocations).
func BenchmarkSplitPick(b *testing.B) {
	s := NewSplit([]uint32{95, 5})
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := s.Pick(nil); !ok {
			b.Fatal("no pick")
		}
	}
}

// BenchmarkHashKey measures hashing a hashKey value onto the ring.
func BenchmarkHashKey(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		HashKey("tenant-42/user-1234567")
	}
}

// BenchmarkBuildRing measures a full 65,536-node ring build, the largest
// rebuild the scheduler runs off the request path (05 req 16).
func BenchmarkBuildRing(b *testing.B) {
	list := eps(64, ones)
	b.ReportAllocs()
	for b.Loop() {
		BuildRing(list, MaxVirtualNodes)
	}
}

// BenchmarkBuildRoundRobin measures a full 65,536-slot schedule build.
func BenchmarkBuildRoundRobin(b *testing.B) {
	list := eps(1024, func(i int) uint32 { return u32(1 + i%5) })
	b.ReportAllocs()
	for b.Loop() {
		BuildRoundRobin(list, 1)
	}
}

// benchPanic measures one selection in panic mode over a large Upstream:
// 1,000 Endpoints, v = 64 and every Endpoint Down (05 reqs 11, 12, 14; PBB
// Upstream-layer budget, target below 20 µs). TestDegradedWalk gates the
// grades a degraded pick makes at O(E).
func benchPanic(b *testing.B, alg Algorithm) {
	benchDegraded(b, alg, ones, func(int) Grade { return Down })
}

// benchPanicSkewed is the slowest degraded pick 05 req 14 allows (one full
// pass): Endpoint 377 weighs 1 against 1,000 and alone holds the lowest
// grade present (Down among DownAvoided), so after the O(E) grading the
// walk resumes over up to one pass of slots to reach its single slot.
// It is reported, not gated, and can exceed the 20 µs target.
func benchPanicSkewed(b *testing.B, alg Algorithm) {
	const light = 377
	benchDegraded(b, alg,
		func(i int) uint32 { return []uint32{1000, 1}[b2i(i == light)] },
		func(i int) Grade { return []Grade{DownAvoided, Down}[b2i(i == light)] })
}

// benchDegraded measures one selection over 1,000 Endpoints of weights w
// and grades g at v = 64.
func benchDegraded(b *testing.B, alg Algorithm, w func(int) uint32, g func(int) Grade) {
	const n = 1000
	bal, err := New(Config{Algorithm: alg, VirtualNodes: MinVirtualNodes, Endpoints: eps(n, w), Now: t0})
	if err != nil {
		b.Fatal(err)
	}
	grades := make([]Grade, n)
	for i := range grades {
		grades[i] = g(i)
	}
	v := &staticView{grades: grades, loads: make([]int64, n)}
	ks := make([]uint64, 1024)
	for i := range ks {
		ks[i] = HashKey("user-" + strconv.Itoa(i))
	}
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		if _, err := bal.Load().Select(ks[i&1023], nil, v); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

func BenchmarkPickRoundRobinPanic(b *testing.B)   { benchPanic(b, RoundRobin) }
func BenchmarkPickLeastRequestPanic(b *testing.B) { benchPanic(b, LeastRequest) }
func BenchmarkPickRingHashPanic(b *testing.B)     { benchPanic(b, RingHash) }
func BenchmarkPickRandomPanic(b *testing.B)       { benchPanic(b, Random) }

func BenchmarkPickRoundRobinPanicSkewed(b *testing.B) { benchPanicSkewed(b, RoundRobin) }
func BenchmarkPickRingHashPanicSkewed(b *testing.B)   { benchPanicSkewed(b, RingHash) }
