// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"errors"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Comparison rules (PBB "Regression policy and gates", "Reproducibility").
const (
	// Pairs is the number of interleaved head and baseline runs: five
	// each (target).
	Pairs = 5
	// MinSlowerPairs is how many pairs must be slower for a regression:
	// four of five.
	MinSlowerPairs = 4
	// RegressionThreshold is the median-of-runs p99 regression that
	// fails: 5% (target).
	RegressionThreshold = 0.05
	// MaxBaselineCV bounds the baseline p99 coefficient of variation: 2%
	// (target); above it the comparison is inconclusive.
	MaxBaselineCV = 0.02
)

// Median returns the median of xs (the mean of the middle two for an even
// count); 0 for none.
func Median(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

// CV returns the coefficient of variation of xs, the population standard
// deviation over the mean; 0 for fewer than two values or a zero mean.
func CV(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	if mean == 0 {
		return 0
	}
	var sq float64
	for _, x := range xs {
		sq += (x - mean) * (x - mean)
	}
	return math.Sqrt(sq/float64(len(xs))) / mean
}

// SlowerPairs counts the pairs whose head value is above the baseline's;
// head[i] and base[i] are one interleaved pair.
func SlowerPairs(head, base []float64) int {
	n := 0
	for i := range min(len(head), len(base)) {
		if head[i] > base[i] {
			n++
		}
	}
	return n
}

// ViewComparison is one view's A/B comparison.
type ViewComparison struct {
	HeadMedian, BaseMedian float64
	Slower                 int
	BaseCV                 float64
	// Regressed reports a median-of-runs p99 more than 5% above the
	// baseline's with four of five pairs slower.
	Regressed bool
	// Noisy reports a baseline coefficient of variation above 2%.
	Noisy bool
}

// CompareView compares one view's per-run p99 values in pair order.
func CompareView(head, base []float64) ViewComparison {
	v := ViewComparison{HeadMedian: Median(head), BaseMedian: Median(base), Slower: SlowerPairs(head, base), BaseCV: CV(base)}
	v.Regressed = v.BaseMedian > 0 && v.HeadMedian > v.BaseMedian*(1+RegressionThreshold) && v.Slower >= MinSlowerPairs
	v.Noisy = v.BaseCV > MaxBaselineCV
	return v
}

// ABVerdict returns the A/B verdict of the views: inconclusive when a side
// has fewer than Pairs valid runs or a baseline view is noisy, failed when
// a view regressed, else passed. A view whose values are all zero is not
// measured and never decides.
func ABVerdict(validHead, validBase int, views ...ViewComparison) string {
	if validHead < Pairs || validBase < Pairs {
		return benchrecord.ABInconclusive
	}
	for _, v := range views {
		if v.BaseMedian > 0 && v.Noisy {
			return benchrecord.ABInconclusive
		}
	}
	for _, v := range views {
		if v.Regressed {
			return benchrecord.ABFail
		}
	}
	return benchrecord.ABPass
}

// Pool merges histograms into a new one at the run settings; nil entries
// are skipped.
func Pool(hs ...*loadgen.Histogram) (*loadgen.Histogram, error) {
	out, err := loadgen.NewHistogram(loadgen.DefaultLowest, loadgen.DefaultHighest, loadgen.DefaultDigits)
	if err != nil {
		return nil, err
	}
	var errs []error
	for _, h := range hs {
		if h != nil {
			errs = append(errs, out.Merge(h))
		}
	}
	return out, errors.Join(errs...)
}

// Percentiles returns h's percentiles; zero for nil or an empty histogram.
func Percentiles(h *loadgen.Histogram) benchrecord.Percentiles {
	if h == nil || h.Count() == 0 {
		return benchrecord.Percentiles{}
	}
	return h.Percentiles()
}

// p99 returns h's p99 in nanoseconds as a float; 0 for nil or empty.
func p99(h *loadgen.Histogram) float64 {
	if h == nil || h.Count() == 0 {
		return 0
	}
	return float64(h.ValueAtPercentile(99))
}

// durationString formats d for a budget result with three significant
// digits in its natural unit.
func durationString(d time.Duration) string {
	switch {
	case d >= time.Second:
		return trimFloat(d.Seconds()) + "s"
	case d >= time.Millisecond:
		return trimFloat(float64(d)/float64(time.Millisecond)) + "ms"
	case d >= time.Microsecond:
		return trimFloat(float64(d)/float64(time.Microsecond)) + "µs"
	default:
		return trimFloat(float64(d)) + "ns"
	}
}

// trimFloat formats x with three significant digits.
func trimFloat(x float64) string {
	return formatSig(x, 3)
}

// formatSig formats x with n significant digits, without exponent.
func formatSig(x float64, n int) string {
	if x == 0 {
		return "0"
	}
	mag := int(math.Floor(math.Log10(math.Abs(x))))
	decimals := max(n-1-mag, 0)
	return strconv.FormatFloat(x, 'f', decimals, 64)
}
