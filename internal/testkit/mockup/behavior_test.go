// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"context"
	"math"
	"math/rand/v2"
	"net/http"
	"testing"
	"time"
)

// Delay distributions (programmable delay distribution; PBB S5x "500 µs ±
// 100 µs normal"; the same 5% over 10,000 samples bar as 11 test plan
// item 7 sets for the fault proxy).

func TestDistributionStatistics(t *testing.T) {
	const n = 10_000
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: seeded delay samples, not security
	for _, tc := range []struct {
		name         string
		d            Distribution
		mean, stddev float64 // expected, in ns
		lo, hi       time.Duration
	}{
		{"fixed", Fixed(3 * time.Millisecond), 3e6, 0, 3 * time.Millisecond, 3 * time.Millisecond},
		{"uniform", Uniform{Min: time.Millisecond, Max: 3 * time.Millisecond}, 2e6, 2e6 / math.Sqrt(12), time.Millisecond, 3 * time.Millisecond},
		{"normal S5x", Normal{Mean: 500 * time.Microsecond, StdDev: 100 * time.Microsecond}, 500e3, 100e3, 0, time.Second},
		{"exponential", Exponential{Mean: time.Millisecond}, 1e6, 1e6, 0, time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sum, sq float64
			for range n {
				v := tc.d.Sample(r)
				if v < tc.lo || v > tc.hi {
					t.Fatalf("sample %v outside [%v, %v]", v, tc.lo, tc.hi)
				}
				sum += float64(v)
				sq += float64(v) * float64(v)
			}
			mean := sum / n
			std := math.Sqrt(sq/n - mean*mean)
			if math.Abs(mean-tc.mean) > 0.05*tc.mean {
				t.Errorf("mean %.0f, want %.0f within 5%%", mean, tc.mean)
			}
			if tc.stddev == 0 {
				if std > 1 {
					t.Errorf("std dev %.0f, want 0", std)
				}
			} else if math.Abs(std-tc.stddev) > 0.05*tc.stddev {
				t.Errorf("std dev %.0f, want %.0f within 5%%", std, tc.stddev)
			}
		})
	}
}

func TestDistributionEdges(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: seeded delay samples, not security
	for range 1000 {
		if v := (Normal{Mean: 0, StdDev: time.Millisecond}).Sample(r); v < 0 {
			t.Fatalf("normal sample %v below 0", v)
		}
	}
	for _, tc := range []struct {
		d    Distribution
		want time.Duration
	}{
		{Fixed(-5), 0},
		{Uniform{Min: 7, Max: 7}, 7},
		{Uniform{Min: -7, Max: -9}, 0},
		{Normal{}, 0},
		{Exponential{}, 0},
		{Normal{Mean: time.Duration(math.MaxInt64), StdDev: time.Duration(math.MaxInt64)}, -1},
		{Exponential{Mean: time.Duration(math.MaxInt64)}, -1},
	} {
		got := tc.d.Sample(r)
		if tc.want >= 0 && got != tc.want {
			t.Errorf("%v.Sample = %v, want %v", tc.d, got, tc.want)
		}
		if got < 0 {
			t.Errorf("%v.Sample = %v below 0", tc.d, got)
		}
	}
	if v := (Uniform{Min: 0, Max: time.Duration(math.MaxInt64)}).Sample(r); v < 0 {
		t.Fatalf("wide uniform sample %v", v)
	}
}

func TestDelayOnLiveServer(t *testing.T) { // the drawn delay is applied and logged
	s := start(t, Config{Seed: 42, Behavior: Behavior{Delay: Uniform{Min: 20 * time.Millisecond, Max: 40 * time.Millisecond}}})
	c := client(t, h1, testPKI{})
	for range 5 {
		r, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
		ok(t, r, err, 200)
		last, _ := s.LastRequest()
		if last.Delay < 20*time.Millisecond || last.Delay > 40*time.Millisecond || r.elapsed < last.Delay {
			t.Fatalf("delay %v, elapsed %v", last.Delay, r.elapsed)
		}
	}
	// A delayed request whose client gives up ends early.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := s.SetBehavior(Behavior{Delay: Fixed(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.URL()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp, err := c.Do(req); err == nil {
		_ = resp.Body.Close()
		t.Fatal("request succeeded")
	}
	waitFor(t, func() bool { return s.Stats().Active == 0 })
}

func TestStrings(t *testing.T) {
	for _, tc := range []struct{ got, want string }{
		{Fixed(time.Second).String(), "fixed(1s)"},
		{Uniform{Min: time.Millisecond, Max: 2 * time.Millisecond}.String(), "uniform(1ms..2ms)"},
		{Normal{Mean: 500 * time.Microsecond, StdDev: 100 * time.Microsecond}.String(), "normal(500µs±100µs)"},
		{Exponential{Mean: time.Millisecond}.String(), "exponential(1ms)"},
		{NoReset.String(), "none"},
		{ResetConn.String(), "conn"},
		{ResetStream.String(), "stream"},
		{ResetKind(7).String(), "ResetKind(7)"},
	} {
		if tc.got != tc.want {
			t.Errorf("got %q, want %q", tc.got, tc.want)
		}
	}
}
