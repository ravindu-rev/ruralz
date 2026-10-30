// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"cmp"
	"context"
	"errors"
	"io"
	"math"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// The schedule offers exactly Rate × Duration requests, request k at
// start + k/Rate.
func TestOpenLoopScheduleIsExact(t *testing.T) {
	fk := clocktest.New(epoch())
	var s seen
	tgt := TargetFunc(func(_ context.Context, r Request) Response {
		s.add(r)
		return Response{Status: 200}
	})
	g := start(t, tgt, Config{Rate: 10, Duration: time.Second, Clock: fk})
	res := drive(t, g, fk, every(ms(100)))
	reqs := s.list()
	slices.SortFunc(reqs, func(a, b Request) int { return cmp.Compare(a.Seq, b.Seq) })
	if len(reqs) != 10 {
		t.Fatalf("%d requests, want 10", len(reqs))
	}
	for k, r := range reqs {
		if r.Seq != uint64(k) || !r.Intended.Equal(epoch().Add(ms(100*k))) {
			t.Errorf("request %d = seq %d at %v, want seq %d at %v", k, r.Seq, r.Intended, k, epoch().Add(ms(100*k)))
		}
	}
	m := res.Measured
	if m.Offered != 10 || m.Count(Success) != 10 || m.Sent() != 10 || m.Latency.Max() != 0 {
		t.Errorf("measured offered %d success %d sent %d max latency %v; want 10, 10, 10, 0",
			m.Offered, m.Count(Success), m.Sent(), m.Latency.Max())
	}
	if !res.MeasureStart.Equal(epoch()) || res.MeasuredDuration() != time.Second || res.AchievedRate() != 10 || res.RateError() != 0 {
		t.Errorf("measured from %v for %v at %v/s, rate error %v", res.MeasureStart, res.MeasuredDuration(), res.AchievedRate(), res.RateError())
	}
	if err := res.Valid(Validity{}); err != nil {
		t.Errorf("Valid = %v", err)
	}
	if !res.Settled {
		t.Error("a run without Settle reports an unsettled warm-up")
	}
}

// A stalled request delays the ones scheduled behind it, and their
// latency counts from the intended send time, not from when a worker got
// to them (coordinated omission).
func TestLatencyFromIntendedSendTime(t *testing.T) {
	fk := clocktest.New(epoch())
	release := make(chan struct{})
	var mu sync.Mutex
	lat := map[uint64]time.Duration{}
	tgt := TargetFunc(func(ctx context.Context, r Request) Response {
		if r.Seq == 0 {
			select {
			case <-release:
			case <-ctx.Done():
			}
		}
		return Response{Status: 200}
	})
	g := start(t, tgt, Config{Rate: 10, Duration: time.Second, MaxInFlight: 1, Clock: fk, OnResult: func(s Sample) {
		mu.Lock()
		lat[s.Seq] = s.Latency()
		mu.Unlock()
	}})
	waitFor(t, "request 0 in flight", func() bool { return g.Progress().InFlight == 1 })
	fk.Advance(time.Second)
	waitFor(t, "requests 1 to 9 backlogged", func() bool { return g.Progress().Backlog == 9 })
	close(release)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	for k := range uint64(10) {
		want := time.Second - ms(100*int(k))
		if k == 0 {
			want = time.Second
		}
		if lat[k] != want {
			t.Errorf("latency of request %d = %v, want %v", k, lat[k], want)
		}
	}
	m := res.Measured
	if p50 := m.Latency.ValueAtPercentile(50); p50 < ms(500) || p50 > ms(501) {
		t.Errorf("latency p50 = %v, want 500ms", p50)
	}
	if top := m.Latency.Max(); top < time.Second || top > ms(1001) {
		t.Errorf("latency max = %v, want 1s", top)
	}
	// Service time, what a closed-loop tool records, hides the stall.
	if p50 := m.Service.ValueAtPercentile(50); p50 >= time.Microsecond {
		t.Errorf("service p50 = %v, want the range at 0", p50)
	}
	if m.Count(Missed) != 0 || m.Count(Success) != 10 {
		t.Errorf("missed %d success %d, want 0 and 10", m.Count(Missed), m.Count(Success))
	}
}

// A request finding every worker busy and the backlog full is Missed:
// never sent, seen by OnResult, and short of the achieved rate.
func TestMissedWhenNoWorkerOrBacklogRoom(t *testing.T) {
	for _, c := range []struct {
		name               string
		backlog            int
		wantMissed, wantOK int64
	}{
		{"no backlog", -1, 9, 1},
		{"backlog of three", 3, 6, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			fk := clocktest.New(epoch())
			release := make(chan struct{})
			var mu sync.Mutex
			var samples []Sample
			tgt := TargetFunc(func(ctx context.Context, r Request) Response {
				if r.Seq == 0 {
					select {
					case <-release:
					case <-ctx.Done():
					}
				}
				return Response{Status: 204}
			})
			g := start(t, tgt, Config{
				Rate: 10, Duration: time.Second, MaxInFlight: 1, MaxBacklog: c.backlog, Clock: fk,
				OnResult: func(s Sample) {
					mu.Lock()
					samples = append(samples, s)
					mu.Unlock()
				},
			})
			waitFor(t, "request 0 in flight", func() bool { return g.Progress().InFlight == 1 })
			fk.Advance(time.Second)
			waitFor(t, "the misses", func() bool { return g.Progress().Missed == c.wantMissed })
			close(release)
			res, err := g.Wait()
			if err != nil {
				t.Fatal(err)
			}
			m := res.Measured
			if m.Count(Missed) != c.wantMissed || m.Count(Success) != c.wantOK || m.Sent() != c.wantOK {
				t.Errorf("missed %d success %d sent %d, want %d, %d, %d", m.Count(Missed), m.Count(Success), m.Sent(), c.wantMissed, c.wantOK, c.wantOK)
			}
			if got, want := res.RateError(), float64(c.wantMissed)/10; math.Abs(got-want) > 1e-9 {
				t.Errorf("RateError = %v, want %v", got, want)
			}
			if err := res.Valid(Validity{}); err == nil || !strings.Contains(err.Error(), "achieved rate") {
				t.Errorf("Valid = %v, want an achieved rate error", err)
			}
			mu.Lock()
			defer mu.Unlock()
			missed := 0
			for _, s := range samples {
				if s.Outcome == Missed {
					missed++
					if !s.Sent.IsZero() || s.Service() != 0 {
						t.Errorf("Missed request %d has a send time", s.Seq)
					}
				}
			}
			if len(samples) != 10 || int64(missed) != c.wantMissed {
				t.Errorf("OnResult saw %d requests, %d missed; want 10, %d", len(samples), missed, c.wantMissed)
			}
			if m.Latency.Count() != uint64(c.wantOK) { //nolint:gosec // G115: a small positive test count
				t.Errorf("latency samples %d, want %d: a Missed request has no latency", m.Latency.Count(), c.wantOK)
			}
		})
	}
}

func TestWarmupIsExcludedFromMeasured(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), Config{Rate: 10, Warmup: time.Second, Duration: time.Second, Clock: fk})
	res := drive(t, g, fk, every(ms(100)))
	if res.Warmup.Offered != 10 || res.Measured.Offered != 10 {
		t.Errorf("warm-up %d, measured %d requests; want 10 and 10", res.Warmup.Offered, res.Measured.Offered)
	}
	if !res.MeasureStart.Equal(epoch().Add(time.Second)) || !res.MeasureEnd.Equal(epoch().Add(2*time.Second)) {
		t.Errorf("measured %v to %v, want 1s to 2s after the start", res.MeasureStart.Sub(epoch()), res.MeasureEnd.Sub(epoch()))
	}
	if len(res.Windows) != 2 || res.Windows[0].Offered != 10 || res.Windows[1].Offered != 10 ||
		!res.Windows[1].Start.Equal(epoch().Add(time.Second)) {
		t.Errorf("windows %+v, want two of 10 requests", res.Windows)
	}
	if !g.Progress().Measuring {
		t.Error("Progress does not report the measured phase")
	}
}

func TestRequestsEndTheMeasuredPhase(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), Config{Rate: 10, Warmup: ms(500), Requests: 5, Clock: fk})
	res := drive(t, g, fk, every(ms(100)))
	if res.Warmup.Offered != 5 || res.Measured.Offered != 5 {
		t.Errorf("warm-up %d, measured %d requests; want 5 and 5", res.Warmup.Offered, res.Measured.Offered)
	}
	if res.MeasuredDuration() != ms(500) || res.AchievedRate() != 10 {
		t.Errorf("measured %v at %v/s, want 500ms at 10/s", res.MeasuredDuration(), res.AchievedRate())
	}
}

// Without Duration or Requests the run lasts until Stop; requests already
// due still go out, and the measured phase ends at the stop instant.
func TestStopEndsAnOpenEndedRun(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), Config{Rate: 10, Clock: fk})
	for range 3 {
		waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
		fk.Advance(ms(100))
	}
	waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
	g.Stop()
	g.Stop() // idempotent
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Measured.Offered != 4 || res.MeasuredDuration() != ms(300) || !res.Settled {
		t.Errorf("offered %d over %v (settled %v), want 4 over 300ms", res.Measured.Offered, res.MeasuredDuration(), res.Settled)
	}
}

// The run's context ends the schedule at once and cancels requests in
// flight; Wait returns the context's error with the partial result.
func TestContextEndCancelsInFlight(t *testing.T) {
	fk := clocktest.New(epoch())
	ctx, cancel := context.WithCancel(t.Context())
	tgt := TargetFunc(func(ctx context.Context, _ Request) Response {
		<-ctx.Done()
		return Response{Err: ctx.Err()}
	})
	g, err := Start(ctx, tgt, Config{Rate: 10, Duration: time.Second, Clock: fk})
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, "request 0 in flight", func() bool { return g.Progress().InFlight == 1 })
	cancel()
	res, err := g.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait error = %v, want context.Canceled", err)
	}
	if res.Measured.Count(Canceled) != 1 || res.Measured.Latency.Count() != 0 {
		t.Errorf("canceled %d, latency samples %d; want 1 and 0", res.Measured.Count(Canceled), res.Measured.Latency.Count())
	}
	if err := res.Valid(Validity{}); err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Errorf("Valid = %v, want a canceled-requests error", err)
	}
}

// The request timeout runs on the injected clock; a target that only
// sees its context canceled still reports Timeout.
func TestTimeoutRunsOnTheInjectedClock(t *testing.T) {
	fk := clocktest.New(epoch())
	tgt := TargetFunc(func(ctx context.Context, _ Request) Response {
		<-ctx.Done()
		return Response{Err: ctx.Err()} // context.Canceled, as net/http reports
	})
	g := start(t, tgt, Config{Rate: 1, Requests: 1, Timeout: 2 * time.Second, Clock: fk})
	waitFor(t, "the request's timeout to be armed", func() bool { return fk.Pending() == 1 })
	fk.Advance(2*time.Second - 1)
	if g.Progress().Done != 0 {
		t.Fatal("the request ended before its timeout")
	}
	fk.Advance(1)
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Measured.Count(Timeout) != 1 || len(res.Failures) != 1 {
		t.Fatalf("timeouts %d failures %d, want 1 and 1", res.Measured.Count(Timeout), len(res.Failures))
	}
	if f := res.Failures[0]; f.Latency() != 2*time.Second || f.Service() != 2*time.Second || f.Outcome != Timeout {
		t.Errorf("failure %+v: latency %v service %v, want Timeout after 2s", f, f.Latency(), f.Service())
	}
}

// Windows slice the run by intended send time; RZ codes of problem
// documents are counted, and O1's RZ-RT-005 can count as achieved.
func TestWindowsAndCodes(t *testing.T) {
	fk := clocktest.New(epoch())
	tgt := TargetFunc(func(_ context.Context, r Request) Response {
		if r.Seq%10 == 0 {
			return Response{Status: 503, Code: "RZ-RT-005", Bytes: 100}
		}
		return Response{Status: 200, Bytes: 10, Conn: ConnInfo{Local: "127.0.0.1:1", Reused: r.Seq%10 != 1}}
	})
	g := start(t, tgt, Config{Rate: 10, Duration: 3 * time.Second, Clock: fk})
	res := drive(t, g, fk, every(ms(100)))
	if len(res.Windows) != 3 {
		t.Fatalf("%d windows, want 3", len(res.Windows))
	}
	for i, w := range res.Windows {
		if !w.Start.Equal(epoch().Add(time.Duration(i)*time.Second)) || w.Offered != 10 || w.Count(ServerError) != 1 ||
			w.Codes["RZ-RT-005"] != 1 || w.Statuses[200] != 9 || w.Statuses[503] != 1 || w.NewConns != 1 || w.Bytes != 190 {
			t.Errorf("window %d = %+v", i, w.Stats)
		}
		if w.Service != nil || w.Latency.Digits() != WindowDigits {
			t.Errorf("window %d keeps service times or full precision", i)
		}
	}
	m := res.Measured
	if m.Codes["RZ-RT-005"] != 3 || !slices.Equal(m.CodeNames(), []string{"RZ-RT-005"}) {
		t.Errorf("codes %v, want RZ-RT-005 three times", m.Codes)
	}
	if got := res.RateError(); math.Abs(got-0.1) > 1e-9 {
		t.Errorf("RateError() = %v, want 0.1", got)
	}
	if got := res.RateError("RZ-RT-005", "RZ-RT-005"); got != 0 {
		t.Errorf(`RateError("RZ-RT-005") = %v, want 0`, got)
	}
	if err := res.Valid(Validity{Count: []string{"RZ-RT-005"}}); err != nil {
		t.Errorf("Valid counting RZ-RT-005 = %v", err)
	}
	if m.ErrorRate() != 0.1 || m.Errors("RZ-RT-005") != 0 {
		t.Errorf("ErrorRate %v Errors(RZ-RT-005) %d, want 0.1 and 0", m.ErrorRate(), m.Errors("RZ-RT-005"))
	}
}

func TestFailuresAreBounded(t *testing.T) {
	fk := clocktest.New(epoch())
	tgt := TargetFunc(func(context.Context, Request) Response { return Response{Status: 500} })
	g := start(t, tgt, Config{Rate: 10, Duration: time.Second, MaxFailures: 3, Clock: fk})
	res := drive(t, g, fk, every(ms(100)))
	if len(res.Failures) != 3 || res.FailuresDropped != 7 {
		t.Errorf("failures %d dropped %d, want 3 and 7", len(res.Failures), res.FailuresDropped)
	}
	g2 := start(t, tgt, Config{Rate: 10, Duration: time.Second, MaxFailures: -1, Clock: fk})
	res = drive(t, g2, fk, every(ms(100)))
	if len(res.Failures) != 0 || res.FailuresDropped != 10 {
		t.Errorf("with none kept: failures %d dropped %d, want 0 and 10", len(res.Failures), res.FailuresDropped)
	}
}

// settleStep jumps a whole window at once in jumpy windows, so their
// requests go out late (p99 900 ms), and steps 100 ms in smooth ones
// (p99 0).
func settleStep(fk *clocktest.Fake, jumpy func(sec int) bool) func(int) time.Duration {
	return func(int) time.Duration {
		if sec := fk.Since(epoch()) / time.Second; fk.Since(epoch())%time.Second == 0 && jumpy(int(sec)) {
			return time.Second
		}
		return ms(100)
	}
}

func settleConfig(fk *clocktest.Fake) Config {
	return Config{
		Rate: 10, Warmup: 2 * time.Second, Duration: time.Second, Window: time.Second, Clock: fk,
		Settle: &Settle{Step: 2 * time.Second, Max: 6 * time.Second, Window: time.Second, Windows: 2, MaxSpread: 0.05},
	}
}

// With Settle the warm-up extends in steps until the p99 of consecutive
// windows settles (PBB "Warm-up, duration and percentiles").
func TestSettleExtendsTheWarmup(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), settleConfig(fk))
	// Window 0 is jumpy; every later one is smooth, so the checkpoint at
	// 2 s compares 900 ms with 0 and the one at 4 s settles.
	res := drive(t, g, fk, settleStep(fk, func(sec int) bool { return sec == 0 }))
	if !res.Settled || !res.MeasureStart.Equal(epoch().Add(4*time.Second)) {
		t.Fatalf("settled %v at %v, want settled at 4s", res.Settled, res.MeasureStart.Sub(epoch()))
	}
	if res.Warmup.Offered != 40 || res.Measured.Offered != 10 || res.MeasuredDuration() != time.Second {
		t.Errorf("warm-up %d measured %d over %v, want 40, 10 over 1s", res.Warmup.Offered, res.Measured.Offered, res.MeasuredDuration())
	}
	if p99 := res.Windows[0].Latency.ValueAtPercentile(99); p99 < ms(895) || p99 > ms(905) {
		t.Errorf("window 0 p99 = %v, want 900ms", p99)
	}
	if err := res.Valid(Validity{}); err != nil {
		t.Errorf("Valid = %v", err)
	}
}

// A warm-up still unsettled at Settle.Max ends there and the run is
// invalid.
func TestSettleGivesUpAtMax(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), settleConfig(fk))
	res := drive(t, g, fk, settleStep(fk, func(sec int) bool { return sec%2 == 0 }))
	if res.Settled || !res.MeasureStart.Equal(epoch().Add(6*time.Second)) || res.Measured.Offered != 10 {
		t.Fatalf("settled %v at %v with %d measured, want unsettled at 6s with 10", res.Settled, res.MeasureStart.Sub(epoch()), res.Measured.Offered)
	}
	if err := res.Valid(Validity{}); err == nil || !strings.Contains(err.Error(), "settle") {
		t.Errorf("Valid = %v, want an unsettled warm-up error", err)
	}
}

// Progress reports the measured phase's start once the warm-up's end is
// decided: at Start without Settle, and only after the settling
// checkpoint with it, where Settled tells a settled warm-up from one that
// gave up at Settle.Max.
func TestProgressReportsTheMeasureStart(t *testing.T) {
	fk := clocktest.New(epoch())
	plain := start(t, ok(), Config{Rate: 10, Warmup: time.Second, Duration: time.Second, Clock: fk})
	if p := plain.Progress(); !p.MeasureStart.Equal(epoch().Add(time.Second)) || !p.Settled {
		t.Errorf("without Settle: MeasureStart %v, settled %v; want 1s after the start, settled", p.MeasureStart, p.Settled)
	}
	drive(t, plain, fk, every(ms(100)))
	for _, c := range []struct {
		name    string
		jumpy   func(sec int) bool
		want    time.Duration
		settled bool
	}{
		{"settles at the second checkpoint", func(sec int) bool { return sec == 0 }, 4 * time.Second, true},
		{"gives up at the maximum", func(sec int) bool { return sec%2 == 0 }, 6 * time.Second, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			fk := clocktest.New(epoch())
			g := start(t, ok(), settleConfig(fk))
			if p := g.Progress(); !p.MeasureStart.IsZero() {
				t.Fatalf("MeasureStart before any checkpoint = %v, want zero", p.MeasureStart)
			}
			res := drive(t, g, fk, settleStep(fk, c.jumpy))
			p := g.Progress()
			if !p.MeasureStart.Equal(epoch().Add(c.want)) || p.Settled != c.settled || !p.MeasureStart.Equal(res.MeasureStart) {
				t.Errorf("MeasureStart %v, settled %v; want %v after the start, settled %v, as the result's %v",
					p.MeasureStart.Sub(epoch()), p.Settled, c.want, c.settled, res.MeasureStart.Sub(epoch()))
			}
		})
	}
}

// A Stop during a settle warm-up leaves it unsettled and the measured
// phase empty.
func TestStopDuringSettle(t *testing.T) {
	fk := clocktest.New(epoch())
	g := start(t, ok(), settleConfig(fk))
	for range 5 {
		waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
		fk.Advance(ms(100))
	}
	waitFor(t, "the generator to go idle", func() bool { return idle(g, fk) })
	g.Stop()
	res, err := g.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if res.Settled || res.Measured.Offered != 0 || res.Warmup.Offered != 6 || res.MeasuredDuration() != 0 {
		t.Errorf("settled %v measured %d warm-up %d over %v; want unsettled, 0, 6, 0", res.Settled, res.Measured.Offered, res.Warmup.Offered, res.MeasuredDuration())
	}
}

func TestSettledSpreadRule(t *testing.T) {
	for _, c := range []struct {
		p99s []time.Duration
		want bool
	}{
		{[]time.Duration{ms(100), ms(105), ms(103)}, true},
		{[]time.Duration{ms(100), ms(106)}, false},
		{[]time.Duration{0, 0, 0}, true},
		{[]time.Duration{0, ms(1)}, false},
		{nil, false},
	} {
		if got := Settled(c.p99s, 0.05); got != c.want {
			t.Errorf("Settled(%v, 5%%) = %v, want %v", c.p99s, got, c.want)
		}
	}
	if s := BenchmarkSettle(); s.Step != 30*time.Second || s.Max != 180*time.Second || s.Window != 10*time.Second || s.Windows != 3 || s.MaxSpread != 0.05 {
		t.Errorf("BenchmarkSettle = %+v, want PBB's 30s steps to 180s over three 10s windows within 5%%", s)
	}
	if BenchmarkWarmup != 60*time.Second {
		t.Errorf("BenchmarkWarmup = %v, want 60s", BenchmarkWarmup)
	}
}

func measured(offered, success int64, codes map[string]int64) *Result {
	r := &Result{MeasureStart: epoch(), MeasureEnd: epoch().Add(time.Second), Settled: true}
	r.Measured.Offered = offered
	r.Measured.Outcomes[Success] = success
	r.Measured.Outcomes[ServerError] = offered - success
	r.Measured.Codes = codes
	return r
}

// PBB: a run is valid only if the achieved rate stays within 0.5% of the
// offered rate (target), counting RZ-RT-005 in O1.
func TestValidAppliesThePBBRateRule(t *testing.T) {
	if err := measured(10000, 9951, nil).Valid(Validity{}); err != nil {
		t.Errorf("0.49%% short: Valid = %v, want nil", err)
	}
	if err := measured(10000, 9949, nil).Valid(Validity{}); err == nil {
		t.Error("0.51% short: Valid = nil, want an error")
	}
	o1 := measured(100, 50, map[string]int64{"RZ-RT-005": 50})
	if err := o1.Valid(Validity{Count: []string{"RZ-RT-005"}}); err != nil {
		t.Errorf("O1 counting RZ-RT-005: Valid = %v", err)
	}
	if err := o1.Valid(Validity{}); err == nil {
		t.Error("O1 without counting RZ-RT-005: Valid = nil, want an error")
	}
	if err := measured(100, 99, nil).Valid(Validity{MaxRateError: 0.02}); err != nil {
		t.Errorf("1%% short under a 2%% bound: Valid = %v", err)
	}
	unsettled := measured(10, 10, nil)
	unsettled.Settled = false
	empty := measured(0, 0, nil)
	canceled := measured(10, 9, nil)
	canceled.Measured.Outcomes[ServerError], canceled.Measured.Outcomes[Canceled] = 0, 1
	for name, r := range map[string]*Result{"unsettled": unsettled, "empty": empty, "canceled": canceled} {
		if err := r.Valid(Validity{}); err == nil {
			t.Errorf("%s: Valid = nil, want an error", name)
		}
	}
	if empty.RateError() != 1 || empty.AchievedRate() != 0 || empty.Measured.ErrorRate() != 0 {
		t.Errorf("empty run: rate error %v achieved %v error rate %v", empty.RateError(), empty.AchievedRate(), empty.Measured.ErrorRate())
	}
}

func TestStartRejects(t *testing.T) {
	good := func() Config { return Config{Rate: 10, Duration: time.Second} }
	for _, c := range []struct {
		name string
		edit func(*Config)
	}{
		{"zero rate", func(c *Config) { c.Rate = 0 }},
		{"negative rate", func(c *Config) { c.Rate = -1 }},
		{"NaN rate", func(c *Config) { c.Rate = math.NaN() }},
		{"infinite rate", func(c *Config) { c.Rate = math.Inf(1) }},
		{"duration and requests", func(c *Config) { c.Requests = 5 }},
		{"negative duration", func(c *Config) { c.Duration = -1 }},
		{"negative warm-up", func(c *Config) { c.Warmup = -1 }},
		{"negative timeout", func(c *Config) { c.Timeout = -1 }},
		{"negative window", func(c *Config) { c.Window = -1 }},
		{"negative in-flight bound", func(c *Config) { c.MaxInFlight = -1 }},
		{"settle without a step", func(c *Config) {
			c.Warmup = time.Minute
			c.Settle = &Settle{Max: time.Minute, Window: time.Second, Windows: 1}
		}},
		{"settle window not a multiple", func(c *Config) {
			c.Warmup, c.Window = time.Minute, 3*time.Second
			c.Settle = &Settle{Step: time.Second, Max: time.Minute, Window: 10 * time.Second, Windows: 1}
		}},
		{"warm-up shorter than the settle windows", func(c *Config) {
			c.Warmup, c.Settle = 10*time.Second, &Settle{Step: time.Second, Max: time.Minute, Window: 10 * time.Second, Windows: 3}
		}},
		{"settle maximum below the warm-up", func(c *Config) {
			c.Warmup, c.Settle = time.Minute, &Settle{Step: time.Second, Max: time.Second, Window: time.Second, Windows: 1}
		}},
		{"negative spread", func(c *Config) {
			c.Warmup, c.Settle = time.Minute, &Settle{Step: time.Second, Max: time.Minute, Window: time.Second, Windows: 1, MaxSpread: -1}
		}},
	} {
		cfg := good()
		c.edit(&cfg)
		if _, err := Start(t.Context(), ok(), cfg); err == nil {
			t.Errorf("%s: Start succeeded, want an error", c.name)
		}
	}
	if _, err := Start(t.Context(), nil, good()); err == nil {
		t.Error("nil target: Start succeeded, want an error")
	}
	if _, err := Run(t.Context(), ok(), Config{}); err == nil {
		t.Error("Run with a zero Config succeeded, want an error")
	}
}

func TestNormalizeOutcome(t *testing.T) {
	for _, c := range []struct {
		name               string
		r                  Response
		timedOut, runEnded bool
		want               Outcome
	}{
		{"error classified", Response{Err: io.EOF}, false, false, Closed},
		{"status classified", Response{Status: 503}, false, false, ServerError},
		{"client status", Response{Status: 429}, false, false, ClientError},
		{"plain success", Response{}, false, false, Success},
		{"timeout overrides a failure", Response{Err: context.Canceled}, true, false, Timeout},
		{"run end overrides a failure", Response{Err: context.Canceled}, false, true, Canceled},
		{"a response stands after a timeout", Response{Outcome: ServerError, Status: 500}, true, false, ServerError},
		{"explicit outcome kept", Response{Outcome: Reset, Err: io.EOF}, false, false, Reset},
		{"missed stays missed", Response{Outcome: Missed}, true, true, Missed},
	} {
		if got := normalize(c.r, c.timedOut, c.runEnded); got != c.want {
			t.Errorf("%s: normalize = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestOutcomeNames(t *testing.T) {
	names := map[string]bool{}
	for o := Outcome(0); o < NumOutcomes; o++ {
		n := o.String()
		if names[n] || strings.HasPrefix(n, "Outcome(") {
			t.Errorf("outcome %d name %q is duplicate or unnamed", o, n)
		}
		names[n] = true
	}
	if got := NumOutcomes.String(); got != "Outcome(11)" {
		t.Errorf("NumOutcomes.String() = %q", got)
	}
	for _, c := range []struct {
		status int
		want   Outcome
	}{{200, Success}, {302, Success}, {404, ClientError}, {599, ServerError}} {
		if got := OutcomeForStatus(c.status); got != c.want {
			t.Errorf("OutcomeForStatus(%d) = %v, want %v", c.status, got, c.want)
		}
	}
	if !ServerError.Response() || Reset.Response() || new(Stats).Count(NumOutcomes) != 0 {
		t.Error("Response() or Count misclassifies")
	}
}
