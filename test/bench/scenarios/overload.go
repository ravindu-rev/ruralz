// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Codes the overload criteria count.
const (
	codeFull       = "RZ-RT-005"
	codeUpstream   = "RZ-UP-006"
	codeBufferFull = "RZ-RT-004"
	codeNoRoute    = "RZ-RT-001"
)

// S3Saturation is S3's expected saturation on 4 vCPU, 28,000 rps (target):
// O1b offers 150% of it and PB-11 judges O1b's goodput against it.
const S3Saturation = 28000

// PB11Share is PB-11's goodput floor: 90% of saturation (target).
const PB11Share = 0.90

// noRejectionView is why O1a's rejection criterion has no result: a
// RZ-RT-005 response writes no access record and the Node times no
// rejection, so no internal view of it exists.
const noRejectionView = "no internal view: RZ-RT-005 writes no access record and the Node records no gateway duration for it"

// o1Workload returns O1's load: S3's requests, rotating tokens and keys,
// over HTTP/2 on 8443 on Connections of Streams at rate, RZ-RT-005
// counting as achieved and the generator holding every stream in flight;
// the mock holds each admitted request for delay.
func o1Workload(s O1Scale, rate float64, delay time.Duration) (harness.Workload, error) {
	s3, err := catalog("S3")
	if err != nil {
		return harness.Workload{}, err
	}
	w := s3.Workload
	w.Connections, w.Streams, w.Rate, w.MockDelay = s.Connections, s.Streams, rate, delay
	w.Count = []string{codeFull}
	w.MaxInFlight = s.Connections * s.Streams
	return w, nil
}

// heldUpstream returns the upstream connections O1a's Node and mock hold
// at once: one per admitted request the mock holds, its rate times its
// delay, up to the in-flight ceiling (20,000 at the full and functional
// scales, a handful in the smoke run's undelayed 100 rps).
func heldUpstream(s O1Scale) int {
	return min(InFlightUnits, int(math.Ceil(s.ARate*s.ADelay.Seconds())))
}

// RunO1 runs O1 (PBB "Scenarios"): O1a offers 150% of what the in-flight
// ceiling admits with every admitted request held at the mock, O1b 150% of
// S3's saturation; both must account for every offered request as admitted
// or RZ-RT-005, see no RZ-UP-006 and plateau at 20,000 active requests,
// and O1b meets PB-11.
func RunO1(ctx context.Context, e Env, s Scale) (*Result, error) {
	// The Node holds a client connection per generator connection and an
	// upstream connection per admitted request held at the mock: O1a's
	// rate times its delay, up to the in-flight ceiling.
	if err := e.needFiles("O1", s.O1.Connections+heldUpstream(s.O1)); err != nil {
		return nil, err
	}
	res := &Result{}
	warm, dur, noSettle := e.lengths(s)
	wa, err := o1Workload(s.O1, s.O1.ARate, s.O1.ADelay)
	if err != nil {
		return nil, err
	}
	sa := scenario("O1a", s.O1.Dir, wa, S3Saturation)
	if _, err := e.run(ctx, sa, harness.RunSpec{
		Warmup: warm, Duration: dur, NoSettle: noSettle, Sample: sampleEvery,
		Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			win := inWindow(o.Samples, o.MeasureStart, o.MeasureEnd)
			return []benchrecord.BudgetResult{
				accounting("O1a-accounting", "admitted plus RZ-RT-005 within 0.5% of offered (target)", o.Load),
				noCode("O1a-no-rz-up-006", "zero RZ-UP-006 (target)", o.Load, codeUpstream),
				plateau("O1a-plateau", "active requests plateau at 20,000 (target)", win, metricActive, nil, InFlightUnits),
				s.hardware(skipped("O1a-rejection-p99", "RZ-RT-005 at an internal p99 of 1ms or less (target)", noRejectionView)),
			}
		},
	}, res); err != nil {
		return nil, err
	}
	if s.O1.BRate > 0 {
		wb, err := o1Workload(s.O1, s.O1.BRate, 0)
		if err != nil {
			return nil, err
		}
		sb := scenario("O1b", s.O1.Dir, wb, S3Saturation)
		if _, err := e.run(ctx, sb, harness.RunSpec{
			Warmup: warm, Duration: dur, NoSettle: noSettle, Sample: sampleEvery,
			Criteria: func(o *harness.RunOutcome) []benchrecord.BudgetResult {
				win := inWindow(o.Samples, o.MeasureStart, o.MeasureEnd)
				return []benchrecord.BudgetResult{
					accounting("O1b-accounting", "admitted plus RZ-RT-005 within 0.5% of offered (target)", o.Load),
					noCode("O1b-no-rz-up-006", "zero RZ-UP-006 (target)", o.Load, codeUpstream),
					plateau("O1b-plateau", "active requests plateau at 20,000 (target)", win, metricActive, nil, InFlightUnits),
					s.hardware(withSLO(goodput(o.Load, o.MeasureEnd.Sub(o.MeasureStart)), "SLO-GW-1")),
				}
			},
		}, res); err != nil {
			return nil, err
		}
	}
	return res.finish(), nil
}

// goodput judges PB-11: O1b's admitted responses per second at 90% or more
// of S3's saturation, every other response RZ-RT-005 (target).
func goodput(r *lab.LoadResult, measured time.Duration) benchrecord.BudgetResult {
	const id, value = "PB-11", ">= 90% of saturation goodput at 150% offered load, excess only as RZ-RT-005 (target)"
	if r == nil || measured <= 0 {
		return judge(id, value, false, "no load result")
	}
	var ok2xx, other int64
	for status, n := range r.Statuses {
		switch {
		case status >= 200 && status <= 299:
			ok2xx += n
		default:
			other += n
		}
	}
	other -= r.Codes[codeFull]
	for o, n := range r.Outcomes {
		if o != loadgen.Success.String() && o != loadgen.ServerError.String() {
			other += n
		}
	}
	rate := float64(ok2xx) / measured.Seconds()
	floor := PB11Share * S3Saturation
	return judge(id, value, rate >= floor && other == 0,
		fmt.Sprintf("goodput %s rps of %s; %d responses neither admitted nor RZ-RT-005", strconv.FormatFloat(rate, 'f', 0, 64), strconv.FormatFloat(floor, 'f', 0, 64), other))
}

// o2Workload returns O2's load: S5's requests on Connections HTTP/1.1
// connections at Rate, the generator holding a request on each, cycling
// Keys values when set.
func o2Workload(s O2Scale) (harness.Workload, error) {
	s5, err := catalog("S5")
	if err != nil {
		return harness.Workload{}, err
	}
	w := s5.Workload
	w.Connections, w.Rate, w.MaxInFlight = s.Connections, s.Rate, s.Connections
	if s.Keys > 0 && w.Keys != nil {
		k := *w.Keys
		k.N = s.Keys
		w.Keys = &k
	}
	return w, nil
}

// skippedWhere is the access-record attribute of a request whose
// ratelimit Policy applied its failureMode: a skipped State Store call.
const skippedWhere = "failure_modes"

// O2InternalSlack is how far O2's skipped requests' internal p99 may lie
// above the same night's S5 run's: 1 ms (target).
const O2InternalSlack = time.Millisecond

// RunO2 runs O2 (PBB "Scenarios"): S5 with a State Store whose every round
// trip the fault proxy delays, so calls in flight plateau at the Node's
// 8,192 and the excess is skipped and counted; goroutines stay within
// O2's bound, the Node holds at most 2 plus 8 connections to the shard,
// and on RH-1 the skipped requests' internal p99 stays within 1 ms of the
// night's S5 run.
func RunO2(ctx context.Context, e Env, s Scale) (*Result, error) {
	if err := e.needFiles("O2", s.O2.Connections); err != nil {
		return nil, err
	}
	res := &Result{}
	warm, dur, noSettle := e.lengths(s)
	w, err := o2Workload(s.O2)
	if err != nil {
		return nil, err
	}
	sc := scenario("O2", s.O2.Dir, w, 40000)
	sc.StateStore = true
	// The proxy delays the replies of the Node's established connections:
	// it passes connection setup unchanged, a TLS handshake's round trips
	// included, and starts delaying once the Node is ready.
	sc.Proxy = &harness.Delay{Mean: s.O2.Delay / 2, PassFirst: true}
	if err := e.check(ctx, sc); err != nil {
		return nil, err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return nil, err
	}
	var s5p99 time.Duration
	if !s.Functional {
		if rec, err := readRecord(e.S, "S5", "summary-comparison.json"); err == nil {
			s5p99 = rec.Summary.Head.Internal.P99
		}
	}
	for attempt := 0; ; attempt++ {
		name := "run-1"
		if attempt > 0 {
			name = "run-1-rerun"
		}
		spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: name, Warmup: warm, Duration: dur, NoSettle: noSettle, Sample: sampleEvery}
		st, err := e.S.Prepare(ctx, spec)
		if err != nil {
			return nil, err
		}
		if err := warmStateStore(ctx, e.S, st, w.Keys); err != nil {
			st.Close()
			return nil, err
		}
		if err := st.Lab().ProxyMode(lab.ProxyDelay); err != nil {
			st.Close()
			return nil, err
		}
		span := warm + dur + 60*time.Second
		if !noSettle {
			span = loadgen.BenchmarkSettle().Max + dur + 60*time.Second
		}
		wins, err := addWindows(st.Lab().Sink, "o2-skipped-"+name, time.Now(), span, skippedWhere)
		if err != nil {
			st.Close()
			return nil, err
		}
		var shardConns int64
		spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
			win := inWindow(o.Samples, o.MeasureStart, o.MeasureEnd)
			skippedCalls := counterDelta(win, metricStateCalls, promtext.L("op", "gcra", "result", "skipped"))
			failed := counterDelta(win, metricStateCalls, promtext.L("op", "gcra", "result", "timeout")) +
				counterDelta(win, metricStateCalls, promtext.L("op", "gcra", "result", "error"))
			out := []benchrecord.BudgetResult{
				plateau("O2-plateau", "State Store calls in flight plateau at 8,192 (target)", win, metricStateInflight, nil, StateInFlight),
				judge("O2-skipped", "the excess skipped and counted in ruralz_state_calls_total, without timeouts or errors (target)",
					skippedCalls > 0 && failed == 0, fmt.Sprintf("%s skipped, %s timed out or failed", fmtCount(skippedCalls), fmtCount(failed))),
				goroutineBound("O2-goroutines", "goroutines below 1 per client connection and request in flight, 2 per upstream connection, 8,192 and 1,000 more (target)", win),
				judge("O2-shard-connections", "at most 2 plus 8 connections to the shard (target)", shardConns >= 1 && shardConns <= ShardConnections,
					strconv.FormatInt(shardConns, 10)+" connections"),
			}
			out = append(out, s.hardware(skippedP99(st, wins, o.MeasureStart, o.MeasureEnd, s5p99)))
			return out
		}
		// The Node's connections to the shard: Valkey's clients during the
		// load, less the harness's own INFO connection.
		stop := make(chan struct{})
		done := make(chan int64, 1)
		go func() { done <- sampleShardConns(ctx, e.S, st, stop) }()
		out, err := e.S.Measure(ctx, st, measureHook(spec, func() { close(stop); shardConns = <-done }))
		st.Close()
		if err != nil {
			return nil, err
		}
		res.add(sc.ID, out.Path, criteriaOf(out.Record))
		if out.Valid || attempt > 0 || !e.rerun() {
			if !out.Valid {
				res.Invalid = append(res.Invalid, "O2: invalid run")
			}
			break
		}
		res.Budgets = nil
		res.Records = nil
	}
	return res.finish(), nil
}

// measureHook returns spec whose Criteria first runs before.
func measureHook(spec harness.RunSpec, before func()) harness.RunSpec {
	crit := spec.Criteria
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		before()
		return crit(o)
	}
	return spec
}

// sampleShardConns samples Valkey's connected clients every second until
// stop closes and returns the highest count, less the INFO connection.
func sampleShardConns(ctx context.Context, s *harness.Session, st *harness.Stage, stop <-chan struct{}) int64 {
	ca, err := st.Lab().CA()
	if err != nil {
		return 0
	}
	ic, err := harness.DialInfo(ctx, s.Profile.Services.StateStore, ca, harness.StateStoreUser, st.Lab().StateStorePassword)
	if err != nil {
		return 0
	}
	defer func() { _ = ic.Close() }()
	var hi int64
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		if v, err := ic.Info(); err == nil {
			hi = max(hi, v.ConnectedClients-1)
		}
		select {
		case <-stop:
			return hi
		case <-ctx.Done():
			return hi
		case <-t.C:
		}
	}
}

// foldWindow is one sink window the driver declared.
type foldWindow struct {
	name       string
	start, end time.Time
}

// addWindows declares sink windows of at least 1 s from start over span,
// at most 32, folding the records whose attribute where is not empty.
func addWindows(sink *harness.Proc, prefix string, start time.Time, span time.Duration, where string) ([]foldWindow, error) {
	step := max(time.Second, (span+31)/32).Truncate(time.Second)
	if step*32 < span {
		step += time.Second
	}
	var out []foldWindow
	for t := start; t.Before(start.Add(span)) && len(out) < 32; t = t.Add(step) {
		w := foldWindow{name: prefix + "-w" + strconv.Itoa(len(out)), start: t, end: t.Add(step)}
		if _, err := sink.Do(lab.Request{Op: lab.OpWindow, Name: w.name, Start: w.start, End: w.end, Where: where}); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, nil
}

// foldedP99 merges the windows inside [start, end) and returns their p99.
func foldedP99(sink *harness.Proc, wins []foldWindow, start, end time.Time) (time.Duration, int64, error) {
	h, err := harness.Pool()
	if err != nil {
		return 0, 0, err
	}
	for _, w := range wins {
		if w.start.Before(start) || w.end.After(end) {
			continue
		}
		resp, err := sink.Do(lab.Request{Op: lab.OpResult, Name: w.name, End: w.end})
		if err != nil {
			return 0, 0, err
		}
		wh, err := lab.DecodeHistogram(resp.Window.Histogram)
		if err != nil {
			return 0, 0, err
		}
		if err := h.Merge(wh); err != nil {
			return 0, 0, err
		}
	}
	return h.ValueAtPercentile(99), int64(h.Count()), nil
}

// skippedP99 judges O2's skipped requests: their internal p99 at most 1 ms
// above the night's S5 run's (target).
func skippedP99(st *harness.Stage, wins []foldWindow, start, end time.Time, s5 time.Duration) benchrecord.BudgetResult {
	const id, value = "O2-skipped-p99", "skipped requests' internal p99 at most 1ms above the night's S5 run's (target)"
	if s5 <= 0 {
		return judge(id, value, false, "no S5 comparison ran tonight")
	}
	p99, n, err := foldedP99(st.Lab().Sink, wins, start, end)
	if err != nil {
		return judge(id, value, false, err.Error())
	}
	if n == 0 {
		return judge(id, value, false, "no access record of a skipped request in the measured window")
	}
	return judge(id, value, p99 <= s5+O2InternalSlack, fmt.Sprintf("%s over %d skipped requests against S5's %s", p99, n, s5))
}

// gaugeMaxOf returns a gauge's highest value over the samples, 0 without
// one.
func gaugeMaxOf(samples []harness.NodeSample, name string, match promtext.Labels) float64 {
	v, n := gaugeMax(samples, name, match)
	if n == 0 || math.IsInf(v, 0) {
		return 0
	}
	return v
}

// warmStateStore makes the Node's State client connect before the fault
// proxy starts delaying: the client dials its shard's connections on
// demand, and a TLS handshake through a proxy holding every chunk for
// O2's delay would outlast the dial timeout. It sends one request for
// each of the run's first FirstSeenBurst keys, which the first-seen
// budget gives full buckets, then waits until Valkey counts the Node's
// connections.
func warmStateStore(ctx context.Context, s *harness.Session, st *harness.Stage, keys *lab.KeySpec) error {
	client := &http.Client{Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	url := "http://" + net.JoinHostPort(gatewayHost(s), strconv.Itoa(portHTTP)) + benchPath
	src, err := keys.Source()
	if err != nil {
		return err
	}
	for i := range uint64(min(src.Cardinality(), FirstSeenBurst)) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("X-Bench-Key", src.Key(i))
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("scenarios: warming the State Store: %w", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	ca, err := st.Lab().CA()
	if err != nil {
		return err
	}
	ic, err := harness.DialInfo(ctx, s.Profile.Services.StateStore, ca, harness.StateStoreUser, st.Lab().StateStorePassword)
	if err != nil {
		return err
	}
	defer func() { _ = ic.Close() }()
	deadline := time.Now().Add(30 * time.Second)
	for {
		v, err := ic.Info()
		if err == nil && v.ConnectedClients-1 >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("scenarios: the Node did not connect to the State Store: %v", err)
		}
		if err := sleepCtx(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
}
