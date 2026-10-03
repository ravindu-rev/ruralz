// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"math"
	"testing"
)

// capturedOutput is `go test -bench . -benchmem -cpu 4 -count 2` output
// with sub-benchmarks, a custom metric, a name printed before its log and
// the trailer lines.
const capturedOutput = `goos: linux
goarch: amd64
pkg: example.com/p
cpu: Intel(R) Xeon(R) CPU @ 2.20GHz
BenchmarkMatch-4          	    2000	       512.3 ns/op	      64 B/op	       2 allocs/op
BenchmarkMatch-4          	    2000	       498.0 ns/op	      64 B/op	       3 allocs/op
BenchmarkChain/size-1024-4         	    2000	      1200 ns/op	        12.5 hits/op	    2048 B/op	      30 allocs/op
BenchmarkChain/size-1024-4         	    2000	      1180 ns/op	        12.5 hits/op	    2048 B/op	      31 allocs/op
BenchmarkLogs-4
    bench_test.go:12: warming up
BenchmarkLogs-4           	    2000	        10.0 ns/op	       0 B/op	       0 allocs/op
BenchmarkLogs-4           	    2000	        10.0 ns/op	       0 B/op	       0 allocs/op
PASS
ok  	example.com/p	0.123s
`

// TestParseBench covers 11 test plan item 1: several lines per benchmark
// (-count), the -<cpu> suffix, sub-benchmarks and custom metrics.
func TestParseBench(t *testing.T) {
	got := map[string][]sample{}
	if err := parseBench([]byte(capturedOutput), 4, got); err != nil {
		t.Fatal(err)
	}
	want := map[string][]sample{
		"BenchmarkMatch":           {{allocs: 2, bytes: 64}, {allocs: 3, bytes: 64}},
		"BenchmarkChain/size-1024": {{allocs: 30, bytes: 2048}, {allocs: 31, bytes: 2048}},
		"BenchmarkLogs":            {{allocs: 0, bytes: 0}, {allocs: 0, bytes: 0}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d benchmarks %v, want %d", len(got), got, len(want))
	}
	for name, ws := range want {
		gs := got[name]
		if len(gs) != len(ws) {
			t.Fatalf("%s: got %v, want %v", name, gs, ws)
		}
		for i := range ws {
			if gs[i] != ws[i] {
				t.Errorf("%s[%d] = %+v, want %+v", name, i, gs[i], ws[i])
			}
		}
	}
	// A second pass appends in run order.
	if err := parseBench([]byte("BenchmarkMatch-4 2000 1 ns/op 64 B/op 4 allocs/op\n"), 4, got); err != nil {
		t.Fatal(err)
	}
	if n := len(got["BenchmarkMatch"]); n != 3 || got["BenchmarkMatch"][2].allocs != 4 {
		t.Errorf("appended samples = %v", got["BenchmarkMatch"])
	}
}

// TestParseBenchCPU1 checks that no suffix is stripped when cpu is 1, where
// the testing package prints none.
func TestParseBenchCPU1(t *testing.T) {
	got := map[string][]sample{}
	if err := parseBench([]byte("BenchmarkSize-1 100 1 ns/op 8 B/op 1 allocs/op\n"), 1, got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["BenchmarkSize-1"]; !ok {
		t.Errorf("got %v, want the name kept as BenchmarkSize-1", got)
	}
}

// TestParseBenchErrors: output without -benchmem is a tool error (exit 2,
// 11 test plan item 1).
func TestParseBenchErrors(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      error
	}{
		{"missing benchmem", "BenchmarkMatch-4  2000  512 ns/op\n", errNoBenchmem},
		{"missing B/op", "BenchmarkMatch-4  2000  512 ns/op  2 allocs/op\n", errNoBenchmem},
		{"bad value", "BenchmarkMatch-4  2000  x ns/op  1 B/op  2 allocs/op\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := parseBench([]byte(tc.out), 4, map[string][]sample{})
			if err == nil {
				t.Fatal("parseBench = nil, want an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("parseBench = %v, want %v", err, tc.want)
			}
		})
	}
}

// TestMedian covers even and odd counts (11 test plan item 1).
func TestMedian(t *testing.T) {
	for _, tc := range []struct {
		in   []float64
		want float64
		ok   bool
	}{
		{nil, 0, false},
		{[]float64{7}, 7, true},
		{[]float64{3, 1, 2}, 2, true},
		{[]float64{4, 1, 3, 2}, 2.5, true},
		{[]float64{30, 30, 31, 31, 30, 31, 30, 31, 31, 31}, 31, true},
		{[]float64{30, 30, 30, 30, 30, 31, 31, 31, 31, 31}, 30.5, true},
	} {
		in := append([]float64(nil), tc.in...)
		got, ok := median(tc.in)
		if ok != tc.ok || (ok && got != tc.want) {
			t.Errorf("median(%v) = %v, %v; want %v, %v", tc.in, got, ok, tc.want, tc.ok)
		}
		for i := range in {
			if in[i] != tc.in[i] {
				t.Fatalf("median reordered its input: %v", tc.in)
			}
		}
	}
	if m := medianOrNaN(nil); !math.IsNaN(m) {
		t.Errorf("medianOrNaN(nil) = %v, want NaN", m)
	}
}
