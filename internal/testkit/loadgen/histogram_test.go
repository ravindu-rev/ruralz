// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"errors"
	"math"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
)

func newHist(t *testing.T, lowest, highest time.Duration, digits int) *Histogram {
	t.Helper()
	h, err := NewHistogram(lowest, highest, digits)
	if err != nil {
		t.Fatalf("NewHistogram = %v", err)
	}
	return h
}

// The layout matches HdrHistogram's for 1 µs to 1 h at three digits:
// 2,048 sub-buckets, a 512 ns unit, 23 buckets.
func TestHistogramLayoutMatchesHdrHistogram(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, DefaultDigits)
	if h.subBuckets != 2048 || h.subBucketHalf != 1024 || h.unitMagnitude != 9 || h.halfMagnitude != 10 {
		t.Fatalf("sub-buckets %d, half %d, unit 2^%d, half 2^%d; want 2048, 1024, 2^9, 2^10",
			h.subBuckets, h.subBucketHalf, h.unitMagnitude, h.halfMagnitude)
	}
	if h.countsLen != 24*1024 {
		t.Fatalf("counts length %d, want %d (23 buckets)", h.countsLen, 24*1024)
	}
	// Equivalence ranges computed by HdrHistogram's rules.
	for _, c := range []struct {
		v, lo, hi int64
		index     int
	}{
		{v: 1_000_000, lo: 999_936, hi: 1_000_447, index: 1953},
		{v: 5_000_000, lo: 4_997_120, hi: 5_001_215, index: 4<<10 + (1220 - 1024)},
		{v: 10_000_000, lo: 9_994_240, hi: 10_002_431, index: 5<<10 + (1220 - 1024)},
		{v: 0, lo: 0, hi: 511, index: 0},
	} {
		if got := h.lowestEquivalent(c.v); got != c.lo {
			t.Errorf("lowestEquivalent(%d) = %d, want %d", c.v, got, c.lo)
		}
		if got := h.highestEquivalent(c.v); got != c.hi {
			t.Errorf("highestEquivalent(%d) = %d, want %d", c.v, got, c.hi)
		}
		if got := h.countsIndex(c.v); got != c.index {
			t.Errorf("countsIndex(%d) = %d, want %d", c.v, got, c.index)
		}
		if got := h.valueFromIndex(h.countsIndex(c.v)); got != c.lo {
			t.Errorf("valueFromIndex(countsIndex(%d)) = %d, want %d", c.v, got, c.lo)
		}
	}
}

// Percentiles follow HdrHistogram's rule: the top of the range of the
// ceil(p/100 × count)-th value; p0 and Min are the bottom of the
// smallest value's range.
func TestHistogramPercentileRule(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, DefaultDigits)
	for i := 1; i <= 10; i++ {
		h.Record(ms(i))
	}
	for _, c := range []struct {
		p    float64
		want time.Duration
	}{
		{0, 999_936}, {10, 1_000_447}, {50, 5_001_215}, {90, 9_003_007}, {100, 10_002_431},
	} {
		if got := h.ValueAtPercentile(c.p); got != c.want {
			t.Errorf("ValueAtPercentile(%v) = %d, want %d", c.p, got, c.want)
		}
	}
	if h.Min() != 999_936 || h.Max() != 10_002_431 || h.Count() != 10 {
		t.Errorf("Min, Max, Count = %d, %d, %d; want 999936, 10002431, 10", h.Min(), h.Max(), h.Count())
	}
	want := benchrecord.Percentiles{P50: 5_001_215, P90: 9_003_007, P99: 10_002_431, P999: 10_002_431, Max: 10_002_431}
	if got := h.Percentiles(); got != want {
		t.Errorf("Percentiles = %+v, want %+v", got, want)
	}
	if got := newHist(t, DefaultLowest, DefaultHighest, 3).ValueAtPercentile(50); got != 0 {
		t.Errorf("empty ValueAtPercentile = %v, want 0", got)
	}
}

// Three significant digits keep every percentile within 0.1% of the exact
// order statistic (PBB "Warm-up, duration and percentiles").
func TestHistogramPrecisionThreeDigits(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, DefaultDigits)
	const n = 100_000
	for i := 1; i <= n; i++ {
		h.Record(time.Duration(i) * time.Microsecond)
	}
	for _, p := range []float64{1, 25, 50, 90, 99, 99.9, 99.99, 100} {
		exact := time.Duration(math.Ceil(p/100*n)) * time.Microsecond
		got := h.ValueAtPercentile(p)
		if rel := math.Abs(float64(got-exact)) / float64(exact); rel > 0.001 {
			t.Errorf("p%v = %v, exact %v: relative error %.5f above 0.1%%", p, got, exact, rel)
		}
	}
	if mean := h.Mean(); math.Abs(float64(mean)-50_000.5e3)/50_000.5e3 > 0.001 {
		t.Errorf("Mean = %v, want 50.0005ms within 0.1%%", mean)
	}
}

func TestHistogramClampsAboveHighest(t *testing.T) {
	h := newHist(t, time.Microsecond, time.Second, 3)
	h.Record(time.Minute)
	h.Record(-time.Second) // a negative latency is recorded as 0
	if h.Clamped() != 1 || h.Count() != 2 {
		t.Fatalf("Clamped, Count = %d, %d; want 1, 2", h.Clamped(), h.Count())
	}
	if got := h.Max(); got < time.Second || got > time.Second+time.Second/1000 {
		t.Errorf("Max = %v, want the range at 1s", got)
	}
	if h.Min() != 0 {
		t.Errorf("Min = %v, want 0 (a zero was recorded)", h.Min())
	}
}

// naiveCorrected is HdrHistogram's back-fill loop, one value at a time.
func naiveCorrected(h *Histogram, d, iv time.Duration) {
	h.Record(d)
	if iv <= 0 {
		return
	}
	for m := d - iv; m >= iv; m -= iv {
		h.Record(m)
	}
}

// RecordCorrected back-fills the samples a stalled tool missed
// (HdrHistogram's recordValueWithExpectedInterval), counting a long
// stall per range instead of per value, with the same result.
func TestHistogramRecordCorrectedBackfills(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, 3)
	h.RecordCorrected(100*time.Millisecond, 10*time.Millisecond)
	if h.Count() != 10 {
		t.Fatalf("Count = %d, want 10: 100ms and 90ms down to 10ms", h.Count())
	}
	if got := h.ValueAtPercentile(0); got > 10*time.Millisecond {
		t.Errorf("smallest back-filled value %v, want 10ms", got)
	}
	for _, c := range []struct {
		name    string
		d, iv   time.Duration
		highest time.Duration
	}{
		{"short", 35 * time.Millisecond, 10 * time.Millisecond, time.Hour},
		{"exact multiple", time.Second, time.Millisecond, time.Hour},
		{"long stall", 7 * time.Second, 37 * time.Microsecond, time.Hour},
		{"clamped head", 2 * time.Hour, time.Second, time.Hour},
		{"no interval", time.Second, 0, time.Hour},
		{"interval above value", time.Millisecond, time.Second, time.Hour},
	} {
		t.Run(c.name, func(t *testing.T) {
			fast := newHist(t, DefaultLowest, c.highest, 3)
			slow := newHist(t, DefaultLowest, c.highest, 3)
			fast.RecordCorrected(c.d, c.iv)
			naiveCorrected(slow, c.d, c.iv)
			if fast.Count() != slow.Count() || fast.Clamped() != slow.Clamped() {
				t.Fatalf("Count, Clamped = %d, %d; want %d, %d", fast.Count(), fast.Clamped(), slow.Count(), slow.Clamped())
			}
			fb, sb := fast.Buckets(), slow.Buckets()
			if len(fb) != len(sb) {
				t.Fatalf("%d buckets, want %d", len(fb), len(sb))
			}
			for i := range fb {
				if fb[i] != sb[i] {
					t.Fatalf("bucket %d = %+v, want %+v", i, fb[i], sb[i])
				}
			}
		})
	}
}

func TestHistogramMerge(t *testing.T) {
	a := newHist(t, DefaultLowest, DefaultHighest, 3)
	b := newHist(t, DefaultLowest, DefaultHighest, 3)
	for i := 1; i <= 50; i++ {
		a.Record(ms(i))
		b.Record(ms(50 + i))
	}
	all := newHist(t, DefaultLowest, DefaultHighest, 3)
	for i := 1; i <= 100; i++ {
		all.Record(ms(i))
	}
	if err := a.Merge(b); err != nil {
		t.Fatal(err)
	}
	if a.Percentiles() != all.Percentiles() || a.Count() != 100 || a.Min() != all.Min() {
		t.Errorf("merged %+v (count %d), want %+v", a.Percentiles(), a.Count(), all.Percentiles())
	}

	// A coarser histogram merges by re-recording its ranges.
	coarse := newHist(t, DefaultLowest, DefaultHighest, WindowDigits)
	coarse.Record(ms(5))
	fine := newHist(t, DefaultLowest, DefaultHighest, 3)
	if err := fine.Merge(coarse); err != nil {
		t.Fatal(err)
	}
	if fine.Count() != 1 || math.Abs(float64(fine.Max()-ms(5)))/float64(ms(5)) > 0.01 {
		t.Errorf("re-recorded merge: count %d max %v, want 1 near 5ms", fine.Count(), fine.Max())
	}

	// A histogram tracking higher values cannot merge into a narrower one.
	narrow := newHist(t, DefaultLowest, time.Second, 3)
	if err := narrow.Merge(a); !errors.Is(err, ErrIncompatible) {
		t.Errorf("Merge of a wider histogram = %v, want ErrIncompatible", err)
	}
}

// Buckets are allocated on first use, so memory follows the recorded
// range.
func TestHistogramAllocatesOnFirstUse(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, 3)
	used := func() int {
		n := 0
		for _, p := range h.pages {
			if p != nil {
				n++
			}
		}
		return n
	}
	if used() != 0 {
		t.Fatalf("%d pages before recording, want 0", used())
	}
	h.Record(ms(1))
	h.Record(ms(1) + time.Microsecond)
	if used() != 1 {
		t.Fatalf("%d pages after two close values, want 1", used())
	}
	c := h.Clone()
	h.Reset()
	if used() != 0 || h.Count() != 0 || c.Count() != 2 {
		t.Fatalf("after Reset: %d pages, count %d, clone count %d; want 0, 0, 2", used(), h.Count(), c.Count())
	}
}

func TestNewHistogramRejects(t *testing.T) {
	for _, c := range []struct {
		name            string
		lowest, highest time.Duration
		digits          int
	}{
		{"zero lowest", 0, time.Second, 3},
		{"negative digits", time.Microsecond, time.Second, -1},
		{"six digits", time.Microsecond, time.Second, 6},
		{"highest below twice lowest", time.Second, time.Second, 3},
		{"unit too coarse", time.Duration(1) << 55, time.Duration(1) << 62, 5},
	} {
		if _, err := NewHistogram(c.lowest, c.highest, c.digits); err == nil {
			t.Errorf("%s: NewHistogram succeeded, want an error", c.name)
		}
	}
	for d := 0; d <= 5; d++ {
		if _, err := NewHistogram(time.Nanosecond, time.Hour, d); err != nil {
			t.Errorf("NewHistogram(1ns, 1h, %d) = %v", d, err)
		}
	}
}
