// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
)

// Histogram settings.
const (
	// DefaultLowest is the lowest discernible latency of the run
	// histograms (target).
	DefaultLowest = time.Microsecond
	// DefaultHighest is the highest trackable latency of the run
	// histograms (target); a larger value is recorded as DefaultHighest
	// and counted by Clamped.
	DefaultHighest = time.Hour
	// DefaultDigits is the precision PBB reports percentiles at: three
	// significant digits.
	DefaultDigits = 3
	// WindowDigits is the precision of the per-window histograms of
	// Result.Windows (target): two significant digits keep a window's
	// percentiles within 1% at an eighth of the memory.
	WindowDigits = 2
)

// ErrIncompatible is returned by Merge when the other histogram can hold
// values above this one's highest trackable value.
var ErrIncompatible = errors.New("loadgen: histogram ranges are incompatible")

// Histogram is a latency histogram with the bucket layout, value
// equivalence ranges and percentile rule of HdrHistogram, the histogram
// PBB's percentiles come from: at three significant digits two values
// share a bucket only when they agree to within 0.1%. Values are
// nanoseconds. Buckets are allocated on first use, so memory follows the
// range of recorded values. A Histogram is not safe for concurrent use.
type Histogram struct {
	lowest, highest int64
	digits          int

	unitMagnitude uint  // floor(log2(lowest))
	halfMagnitude uint  // log2(subBucketHalf)
	subBuckets    int64 // sub-buckets per bucket, a power of two
	subBucketHalf int64
	subBucketMask int64
	zeroBase      int // leading-zero count base of the bucket index
	countsLen     int

	pages   [][]uint64 // countsLen/subBucketHalf pages, nil until used
	total   uint64
	min     int64 // smallest non-zero value recorded; math.MaxInt64 when none
	max     int64 // largest value recorded
	clamped uint64
}

// NewHistogram returns an empty histogram tracking lowest to highest at
// digits significant digits (0 to 5); lowest is at least 1 ns and highest
// at least twice lowest.
func NewHistogram(lowest, highest time.Duration, digits int) (*Histogram, error) {
	switch {
	case lowest < 1:
		return nil, fmt.Errorf("loadgen: histogram lowest value %v below 1ns", lowest)
	case digits < 0 || digits > 5:
		return nil, fmt.Errorf("loadgen: histogram precision %d outside 0..5 digits", digits)
	case highest/2 < lowest:
		return nil, fmt.Errorf("loadgen: histogram highest value %v below twice the lowest %v", highest, lowest)
	}
	h := &Histogram{lowest: int64(lowest), highest: int64(highest), digits: digits, min: math.MaxInt64}
	// The sub-bucket count is the power of two at or above the largest
	// value with single-unit resolution, 2 × 10^digits.
	largest := 2 * pow10(digits)
	countMagnitude := uint(bits.Len64(largest - 1))
	h.halfMagnitude = countMagnitude - 1
	h.unitMagnitude = uint(bits.Len64(uint64(h.lowest)) - 1) //nolint:gosec // G115: lowest is at least 1
	if countMagnitude+h.unitMagnitude > 62 {
		return nil, fmt.Errorf("loadgen: histogram lowest value %v too large for %d digits", lowest, digits)
	}
	h.subBuckets = 1 << countMagnitude
	h.subBucketHalf = h.subBuckets / 2
	h.subBucketMask = (h.subBuckets - 1) << h.unitMagnitude
	h.zeroBase = 64 - int(h.unitMagnitude) - int(countMagnitude)
	buckets := h.bucketsNeeded(h.highest)
	h.countsLen = (buckets + 1) * int(h.subBucketHalf)
	h.pages = make([][]uint64, buckets+1)
	return h, nil
}

// mustHistogram returns a histogram with constant, valid settings.
func mustHistogram(digits int) *Histogram {
	h, err := NewHistogram(DefaultLowest, DefaultHighest, digits)
	if err != nil {
		panic(err)
	}
	return h
}

func pow10(n int) uint64 {
	v := uint64(1)
	for range n {
		v *= 10
	}
	return v
}

// bucketsNeeded returns the buckets covering values up to v.
func (h *Histogram) bucketsNeeded(v int64) int {
	untrackable := h.subBuckets << h.unitMagnitude
	n := 1
	for untrackable <= v {
		if untrackable > math.MaxInt64/2 {
			return n + 1
		}
		untrackable <<= 1
		n++
	}
	return n
}

func (h *Histogram) bucketIndex(v int64) int {
	return h.zeroBase - bits.LeadingZeros64(uint64(v|h.subBucketMask)) //nolint:gosec // G115: recorded values are never negative
}

func (h *Histogram) subBucketIndex(v int64, bucket int) int64 {
	return v >> (uint(bucket) + h.unitMagnitude)
}

// countsIndex returns the counts slot of v.
func (h *Histogram) countsIndex(v int64) int {
	b := h.bucketIndex(v)
	sb := h.subBucketIndex(v, b)
	return (b+1)<<h.halfMagnitude + int(sb-h.subBucketHalf)
}

// valueFromIndex returns the lowest value of counts slot i.
func (h *Histogram) valueFromIndex(i int) int64 {
	b := i>>h.halfMagnitude - 1
	sb := int64(i&int(h.subBucketHalf-1)) + h.subBucketHalf
	if b < 0 {
		sb -= h.subBucketHalf
		b = 0
	}
	return sb << (uint(b) + h.unitMagnitude)
}

// rangeSize returns the size of the range of values equivalent to v.
func (h *Histogram) rangeSize(v int64) int64 {
	b := h.bucketIndex(v)
	if h.subBucketIndex(v, b) >= h.subBuckets {
		b++
	}
	return 1 << (h.unitMagnitude + uint(b))
}

func (h *Histogram) lowestEquivalent(v int64) int64 {
	b := h.bucketIndex(v)
	return h.subBucketIndex(v, b) << (uint(b) + h.unitMagnitude)
}

func (h *Histogram) highestEquivalent(v int64) int64 {
	return h.lowestEquivalent(v) + h.rangeSize(v) - 1
}

func (h *Histogram) medianEquivalent(v int64) int64 {
	return h.lowestEquivalent(v) + h.rangeSize(v)>>1
}

// Lowest returns the lowest discernible value.
func (h *Histogram) Lowest() time.Duration { return time.Duration(h.lowest) }

// Highest returns the highest trackable value.
func (h *Histogram) Highest() time.Duration { return time.Duration(h.highest) }

// Digits returns the significant digits of the histogram.
func (h *Histogram) Digits() int { return h.digits }

// Record records one value.
func (h *Histogram) Record(d time.Duration) { h.RecordN(d, 1) }

// RecordN records n occurrences of d. A negative value is recorded as 0;
// a value above Highest is recorded as Highest and counted by Clamped.
func (h *Histogram) RecordN(d time.Duration, n uint64) {
	if n == 0 {
		return
	}
	v := max(int64(d), 0)
	if v > h.highest {
		v = h.highest
		h.clamped += n
	}
	h.recordAt(v, n)
}

// recordAt counts n values v, 0 <= v <= highest.
func (h *Histogram) recordAt(v int64, n uint64) {
	i := h.countsIndex(v)
	p := h.pages[i>>h.halfMagnitude]
	if p == nil {
		p = make([]uint64, h.subBucketHalf)
		h.pages[i>>h.halfMagnitude] = p
	}
	p[i&int(h.subBucketHalf-1)] += n
	h.total += n
	if v != 0 && v < h.min {
		h.min = v
	}
	if v > h.max {
		h.max = v
	}
}

// RecordCorrected records d and back-fills the samples a tool that
// measures raw service times missed while a request stalled: d − k × the
// expected interval for every k leaving at least one interval
// (HdrHistogram's recordValueWithExpectedInterval, PBB "Open-loop load and
// coordinated omission"). The generator itself never needs it, since it
// measures from the intended send time; the benchmark harness uses it for
// tools that report service times. A non-positive interval records d
// alone.
func (h *Histogram) RecordCorrected(d, interval time.Duration) {
	h.Record(d)
	v, iv := int64(d), int64(interval)
	if iv <= 0 || v-iv < iv {
		return
	}
	// The missing values are v − k×iv for k = 1..kMax, each at least iv.
	kMax := (v - iv) / iv
	k := int64(1)
	if v-iv > h.highest {
		// Values above the highest trackable value are clamped.
		kc := min((v-h.highest-1)/iv, kMax)
		h.recordAt(h.highest, uint64(kc)) //nolint:gosec // G115: kc is a positive count
		h.clamped += uint64(kc)           //nolint:gosec // G115: kc is a positive count
		k = kc + 1
	}
	// Walk the equivalence ranges from the top, counting every missing
	// value of a range at once, so a long stall costs one step per range.
	for k <= kMax {
		x := v - k*iv
		kEnd := min((v-h.lowestEquivalent(x))/iv, kMax)
		h.recordAt(x, uint64(kEnd-k+1)) //nolint:gosec // G115: kEnd >= k, a positive count
		k = kEnd + 1
	}
}

// Count returns the number of recorded values.
func (h *Histogram) Count() uint64 { return h.total }

// Clamped returns the number of values recorded as Highest because they
// exceeded it.
func (h *Histogram) Clamped() uint64 { return h.clamped }

// Max returns the largest recorded value, rounded up to the top of its
// equivalence range as HdrHistogram reports it; 0 when empty.
func (h *Histogram) Max() time.Duration {
	if h.total == 0 || h.max == 0 {
		return 0
	}
	return time.Duration(h.highestEquivalent(h.max))
}

// Min returns the smallest recorded value, rounded down to the bottom of
// its equivalence range; 0 when empty.
func (h *Histogram) Min() time.Duration {
	if h.total == 0 || h.countAt(0) > 0 || h.min == math.MaxInt64 {
		return 0
	}
	return time.Duration(h.lowestEquivalent(h.min))
}

// Mean returns the mean of the recorded values, each taken at the middle
// of its equivalence range; 0 when empty.
func (h *Histogram) Mean() time.Duration {
	if h.total == 0 {
		return 0
	}
	var sum float64
	h.each(func(i int, c uint64) {
		sum += float64(h.medianEquivalent(h.valueFromIndex(i))) * float64(c)
	})
	return time.Duration(sum / float64(h.total))
}

// ValueAtPercentile returns the value at percentile p (0 to 100) by
// HdrHistogram's rule: the top of the equivalence range holding the
// ceil(p/100 × count)-th smallest value (p is lowered by one ulp first,
// so rounding never reaches the next value), or the bottom of the
// smallest value's range for p = 0; 0 when empty.
func (h *Histogram) ValueAtPercentile(p float64) time.Duration {
	if h.total == 0 || math.IsNaN(p) {
		return 0
	}
	req := math.Min(math.Max(math.Nextafter(p, math.Inf(-1)), 0), 100)
	want := max(uint64(math.Ceil(req/100*float64(h.total))), 1)
	var run uint64
	var at int64 = -1
	h.each(func(i int, c uint64) {
		if at >= 0 {
			return
		}
		run += c
		if run >= want {
			at = h.valueFromIndex(i)
		}
	})
	if at < 0 {
		return 0
	}
	if p <= 0 {
		return time.Duration(h.lowestEquivalent(at))
	}
	return time.Duration(h.highestEquivalent(at))
}

// Percentiles returns the p50, p90, p99, p99.9 and maximum a result
// record stores (PBB "Warm-up, duration and percentiles").
func (h *Histogram) Percentiles() benchrecord.Percentiles {
	return benchrecord.Percentiles{
		P50:  h.ValueAtPercentile(50),
		P90:  h.ValueAtPercentile(90),
		P99:  h.ValueAtPercentile(99),
		P999: h.ValueAtPercentile(99.9),
		Max:  h.Max(),
	}
}

// Merge adds o's values to h ("pooled" percentiles across runs). Both may
// differ in lowest value and precision; o's values are then re-recorded
// at the bottom of their ranges, as HdrHistogram does. It returns
// ErrIncompatible when o tracks values above h's highest.
func (h *Histogram) Merge(o *Histogram) error {
	if o == nil || o.total == 0 {
		return nil
	}
	if o.highest > h.highest {
		return fmt.Errorf("%w: highest %v above %v", ErrIncompatible, o.Highest(), h.Highest())
	}
	if o.unitMagnitude == h.unitMagnitude && o.halfMagnitude == h.halfMagnitude {
		for pi, src := range o.pages {
			if src == nil {
				continue
			}
			dst := h.pages[pi]
			if dst == nil {
				dst = make([]uint64, h.subBucketHalf)
				h.pages[pi] = dst
			}
			for j, c := range src {
				dst[j] += c
			}
		}
		h.total += o.total
		if o.min < h.min {
			h.min = o.min
		}
		h.max = max(h.max, o.max)
	} else {
		o.each(func(i int, c uint64) { h.recordAt(o.valueFromIndex(i), c) })
	}
	h.clamped += o.clamped
	return nil
}

// Clone returns a deep copy of h.
func (h *Histogram) Clone() *Histogram {
	c := *h
	c.pages = make([][]uint64, len(h.pages))
	for i, p := range h.pages {
		if p != nil {
			c.pages[i] = append([]uint64(nil), p...)
		}
	}
	return &c
}

// Reset empties h, keeping its settings.
func (h *Histogram) Reset() {
	clear(h.pages)
	h.total, h.clamped, h.max, h.min = 0, 0, 0, math.MaxInt64
}

// countAt returns the count of slot i.
func (h *Histogram) countAt(i int) uint64 {
	p := h.pages[i>>h.halfMagnitude]
	if p == nil {
		return 0
	}
	return p[i&int(h.subBucketHalf-1)]
}

// each calls f for every non-empty slot in value order.
func (h *Histogram) each(f func(i int, c uint64)) {
	half := int(h.subBucketHalf)
	for pi, p := range h.pages {
		if p == nil {
			continue
		}
		for j, c := range p {
			if c != 0 {
				f(pi*half+j, c)
			}
		}
	}
}

// Bucket is one non-empty equivalence range of a Histogram.
type Bucket struct {
	// Low and High bound the range (both inclusive).
	Low, High time.Duration
	// Count is the number of values recorded in it.
	Count uint64
}

// Buckets returns the non-empty equivalence ranges in value order.
func (h *Histogram) Buckets() []Bucket {
	var out []Bucket
	h.each(func(i int, c uint64) {
		v := h.valueFromIndex(i)
		out = append(out, Bucket{Low: time.Duration(h.lowestEquivalent(v)), High: time.Duration(h.highestEquivalent(v)), Count: c})
	})
	return out
}
