// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"
)

// Ladder is PBB's rate ladder: open-loop steps at fractions of the
// expected saturation, each a run of StepDuration, the first preceded by
// a warm-up at its rate.
type Ladder struct {
	// Expected is the expected saturation rate, requests per second.
	Expected float64
	// From, To and Step are fractions of Expected: PBB steps of 5% from
	// 50% to 110% (target).
	From, To, Step float64
	// StepDuration is each step's measured length; PBB: 60 s (target).
	StepDuration time.Duration
	// Warmup precedes the first step; PBB: 60 s (target).
	Warmup time.Duration
	// Stop, when set, ends the climb after a step for which it returns
	// true, as G1 stops once p99 exceeds 1 ms (target).
	Stop func(*Result) bool
}

// BenchmarkLadder returns PBB's ladder for an expected saturation.
func BenchmarkLadder(expected float64) Ladder {
	return Ladder{Expected: expected, From: 0.50, To: 1.10, Step: 0.05, StepDuration: 60 * time.Second, Warmup: BenchmarkWarmup}
}

// Rates returns the offered rate of every step: Expected × (From + k ×
// Step) for every k keeping the fraction at or below To.
func (l Ladder) Rates() ([]float64, error) {
	switch {
	case !(l.Expected > 0) || math.IsInf(l.Expected, 0):
		return nil, fmt.Errorf("loadgen: expected saturation %v is not a positive number", l.Expected)
	case !(l.From > 0) || !(l.Step > 0) || l.To < l.From || math.IsInf(l.To, 0):
		return nil, errors.New("loadgen: ladder needs 0 < From <= To and a positive Step")
	}
	// Round k's count so 0.50 + 12 × 0.05 lands on 1.10 despite binary
	// fractions.
	n := int(math.Floor((l.To-l.From)/l.Step+1e-9)) + 1
	rates := make([]float64, n)
	for k := range rates {
		rates[k] = l.Expected * (l.From + float64(k)*l.Step)
	}
	return rates, nil
}

// RunLadder runs every step of l against t with base's other settings
// (Duration, Requests and Settle are replaced: each step lasts
// StepDuration, and only the first has a warm-up). It returns the step
// results so far and the first error.
func RunLadder(ctx context.Context, t Target, base Config, l Ladder) ([]*Result, error) {
	rates, err := l.Rates()
	if err != nil {
		return nil, err
	}
	if l.StepDuration <= 0 {
		return nil, errors.New("loadgen: ladder step duration must be positive")
	}
	var out []*Result
	for k, rate := range rates {
		c := base
		c.Rate, c.Duration, c.Requests, c.Settle, c.Warmup = rate, l.StepDuration, 0, nil, 0
		if k == 0 {
			c.Warmup = l.Warmup
		}
		res, err := Run(ctx, t, c)
		if res != nil {
			out = append(out, res)
		}
		if err != nil {
			return out, err
		}
		if l.Stop != nil && l.Stop(res) {
			break
		}
	}
	return out, nil
}

// StepSummary is what the saturation rule reads of one ladder step, so the
// harness can apply it to any tool's results.
type StepSummary struct {
	// Offered is the step's offered rate.
	Offered float64
	// RateError is the achieved rate's shortfall as a fraction of the
	// offered rate.
	RateError float64
	// P99 is the step's end-to-end p99.
	P99 time.Duration
	// ErrorRate is the fraction of offered requests that failed.
	ErrorRate float64
}

// Summary returns the measured phase's StepSummary, counting codes as
// Stats.Achieved does.
func (r *Result) Summary(counted ...string) StepSummary {
	return StepSummary{
		Offered:   r.Rate,
		RateError: r.RateError(counted...),
		P99:       r.Measured.Latency.ValueAtPercentile(99),
		ErrorRate: r.Measured.ErrorRate(counted...),
	}
}

// SaturationCriteria is PBB's definition of saturation.
type SaturationCriteria struct {
	// MaxRateError bounds the achieved rate's shortfall: 0.5% (target).
	MaxRateError float64
	// MaxP99 bounds the end-to-end p99: 10 ms (target).
	MaxP99 time.Duration
	// MaxErrorRate bounds the errors: 0.01% (target).
	MaxErrorRate float64
}

// BenchmarkSaturation returns PBB's saturation criteria.
func BenchmarkSaturation() SaturationCriteria {
	return SaturationCriteria{MaxRateError: DefaultMaxRateError, MaxP99: 10 * time.Millisecond, MaxErrorRate: 0.0001}
}

// Saturation returns the highest offered rate among the steps that meet
// c (PBB "Throughput targets"); ok is false when none does.
func Saturation(steps []StepSummary, c SaturationCriteria) (rate float64, ok bool) {
	for _, s := range steps {
		if s.RateError <= c.MaxRateError && s.P99 <= c.MaxP99 && s.ErrorRate <= c.MaxErrorRate && (!ok || s.Offered > rate) {
			rate, ok = s.Offered, true
		}
	}
	return rate, ok
}
