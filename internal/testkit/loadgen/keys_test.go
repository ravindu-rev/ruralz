// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"math"
	"strconv"
	"testing"
)

func keys(k KeySource, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = k.Key(uint64(i))
	}
	return out
}

func TestKeySources(t *testing.T) {
	cyc, err := Cycle("k", 3)
	if err != nil {
		t.Fatal(err)
	}
	rot, err := Rotate("r", 2)
	if err != nil {
		t.Fatal(err)
	}
	given := []string{"a", "b"}
	list, err := List(given...)
	if err != nil {
		t.Fatal(err)
	}
	given[0] = "changed" // the list keeps its own copy
	for _, c := range []struct {
		name  string
		k     KeySource
		want  []string
		card  int
		distr string
	}{
		{"cycle", cyc, []string{"k0", "k1", "k2", "k0", "k1"}, 3, "uniform"},
		{"rotate", rot, []string{"r0", "r0", "r1", "r1", "r2"}, 0, "rotating"},
		{"single", Single("hot"), []string{"hot", "hot", "hot", "hot", "hot"}, 1, "single"},
		{"list", list, []string{"a", "b", "a", "b", "a"}, 2, "list"},
	} {
		got := keys(c.k, len(c.want))
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: keys %v, want %v", c.name, got, c.want)
				break
			}
		}
		if c.k.Cardinality() != c.card || c.k.Distribution() != c.distr {
			t.Errorf("%s: cardinality %d distribution %q, want %d %q", c.name, c.k.Cardinality(), c.k.Distribution(), c.card, c.distr)
		}
	}
	if _, err := Cycle("k", 0); err == nil {
		t.Error("Cycle of 0 keys succeeded")
	}
	if _, err := Rotate("r", 0); err == nil {
		t.Error("Rotate with 0 requests per key succeeded")
	}
	if _, err := List(); err == nil {
		t.Error("List of no keys succeeded")
	}
}

// Zipf keys are a pure function of the seed and sequence number, and
// their frequencies follow 1/(k+1)^s.
func TestZipfKeys(t *testing.T) {
	z, err := Zipf("z", 100, 1, 42)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := Zipf("z", 100, 1, 42)
	other, _ := Zipf("z", 100, 1, 7)
	const n = 200_000
	counts := make([]int, 100)
	differ := 0
	for i := range uint64(n) {
		k := z.Key(i)
		if k != again.Key(i) {
			t.Fatalf("Key(%d) differs between equal sources", i)
		}
		if k != other.Key(i) {
			differ++
		}
		r, err := strconv.Atoi(k[1:])
		if err != nil || r < 0 || r >= 100 {
			t.Fatalf("key %q outside z0..z99", k)
		}
		counts[r]++
	}
	if differ == 0 {
		t.Error("a different seed drew the same keys")
	}
	h100 := 0.0
	for k := 1; k <= 100; k++ {
		h100 += 1 / float64(k)
	}
	if want := 1 / h100; math.Abs(z.Share(0)-want) > 1e-12 {
		t.Errorf("Share(0) = %v, want %v", z.Share(0), want)
	}
	for _, r := range []int{0, 1, 9, 99} {
		got := float64(counts[r]) / n
		if want := z.Share(r); math.Abs(got-want) > 0.005 {
			t.Errorf("rank %d drawn %.4f of the time, want %.4f", r, got, want)
		}
	}
	sum := 0.0
	for r := range 100 {
		sum += z.Share(r)
	}
	if math.Abs(sum-1) > 1e-9 || z.Share(-1) != 0 || z.Share(100) != 0 {
		t.Errorf("shares sum to %v; out-of-range shares %v, %v", sum, z.Share(-1), z.Share(100))
	}
	if z.Cardinality() != 100 || z.Distribution() != "zipf(s=1)" {
		t.Errorf("cardinality %d distribution %q", z.Cardinality(), z.Distribution())
	}
	flat, _ := Zipf("u", 4, 0, 1)
	if flat.Share(0) != 0.25 || flat.Share(3) != 0.25 {
		t.Errorf("s = 0 shares %v, %v; want uniform", flat.Share(0), flat.Share(3))
	}
	for _, bad := range []struct {
		n int
		s float64
	}{{0, 1}, {10, -1}, {10, math.NaN()}, {10, math.Inf(1)}} {
		if _, err := Zipf("z", bad.n, bad.s, 1); err == nil {
			t.Errorf("Zipf(n=%d, s=%v) succeeded", bad.n, bad.s)
		}
	}
}

// Mix interleaves parts in proportion to their weights, by sequence
// number alone.
func TestMixInterleavesSmoothly(t *testing.T) {
	part := func(name string) Target {
		return TargetFunc(func(context.Context, Request) Response { return Response{Key: name} })
	}
	m, err := Mix(Part{Weight: 6, Target: part("read")}, Part{Weight: 2, Target: part("write")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for seq := range uint64(8) {
		got = append(got, m.Do(t.Context(), Request{Seq: seq}).Key)
	}
	want := []string{"read", "read", "write", "read", "read", "read", "write", "read"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("mix order %v, want %v", got, want)
		}
	}
	for _, bad := range [][]Part{
		nil,
		{{Weight: 0, Target: part("a")}},
		{{Weight: 1}},
		{{Weight: maxMixWeight, Target: part("a")}, {Weight: 1, Target: part("b")}},
	} {
		if _, err := Mix(bad...); err == nil {
			t.Errorf("Mix(%v) succeeded", bad)
		}
	}
}
