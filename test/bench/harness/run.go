// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// ErrRefused reports a Node that did not start on, or does not serve, the
// Revision: for a baseline, the comparison is inconclusive (PBB
// "Reproducibility").
var ErrRefused = errors.New("harness: the Node refused the Revision")

// Validity bounds of a run (PBB "Open-loop load and coordinated
// omission", "Measurement points"), all targets.
const (
	MaxRateError     = 0.005
	MaxGeneratorCPU  = 0.70
	MaxMockCPU       = 0.70
	MaxMockExcessP99 = 200 * time.Microsecond
)

// Benchmark run lengths (PBB "Warm-up, duration and percentiles"):
// 60 s of warm-up extended by the settle rule, 180 s measured (targets).
const (
	Warmup   = loadgen.BenchmarkWarmup
	Measured = 180 * time.Second
	// ProfileSeconds is the CPU profile taken after the last head run.
	ProfileSeconds = 30
	// foldSlack extends the sink windows past the run's planned end.
	foldSlack = 30 * time.Second
)

// RunSpec is one run of one side.
type RunSpec struct {
	Scenario Scenario
	Build    Build
	Revision Revision
	// Baseline is the record's baseline commit; empty without one.
	Baseline string
	// Name is the record's file name, without extension.
	Name string
	// Warmup, Duration and NoSettle set the phases: zero values take the
	// benchmark lengths (a ladder's later steps measure without a warm-up,
	// a negative Warmup).
	Warmup, Duration time.Duration
	NoSettle         bool
	// Profile takes a CPU profile right after the measured window at the
	// same offered rate, so no measured window is profiled.
	Profile bool
	// IdleSettle, when positive, waits that long after the load and
	// records the idle RSS.
	IdleSettle time.Duration
	// NoInternal runs without the internal view (telemetry off).
	NoInternal bool
	// NodeEnv is added to the Node's environment.
	NodeEnv []string
	// Sample is the Node sampling interval; default one tenth of the
	// measured length, at least 1 s and at most 10 s.
	Sample time.Duration
	// Node adds to the Node's settings beyond NodeEnv: Last-Known-Good
	// starts and scope properties.
	Node NodeSpec
	// Criteria are a driver's pass criteria, judged on the outcome before
	// its record is written (every field but Path set) and added to the
	// record's budget results.
	Criteria func(*RunOutcome) []benchrecord.BudgetResult
}

// RunOutcome is what one run measured.
type RunOutcome struct {
	Record   *benchrecord.Record
	Path     string
	Measures Measures
	Valid    bool
	// Metrics are the Node histogram deltas over the window, by MetricKey.
	Metrics map[string]promtext.Histogram
	// MeasureStart and MeasureEnd bound the measured phase.
	MeasureStart, MeasureEnd time.Time
	// ErrorRate is the share of offered requests that failed.
	ErrorRate float64
	// Load is the load tool's result: offered and achieved counts, the
	// statuses, the RZ codes of problem documents and the outcomes.
	Load *lab.LoadResult
	// Samples are the Node samples of the run, warm-up included.
	Samples []NodeSample
}

// Stage is a prepared run: the lab services and the Node serving the
// Revision, which a ladder, a soak or a driver keeps across its steps.
type Stage struct {
	lab  *Lab
	node *Node
	pki  *RunPKI
	info func() (*ValkeyInfo, error)
	done []func()
	// started is when the Node started.
	started time.Time
	// ns is the Node's settings: StartNode keeps its binary, State Store
	// URL and environment.
	ns NodeSpec
}

func (st *Stage) close() {
	for i := len(st.done) - 1; i >= 0; i-- {
		st.done[i]()
	}
	st.done = nil
}

// Close stops the stage's Node and lab services, the State Store last.
func (st *Stage) Close() { st.close() }

// StartNode stops the stage's Node and starts another on the same lab
// services, with the first Node's binary, State Store URL and environment
// and ns's source, data directory and readiness settings: R1's cold starts
// from rendered YAML, from no Revision and from Last-Known-Good.
func (st *Stage) StartNode(ctx context.Context, ns NodeSpec) (*Node, error) {
	if err := st.node.Stop(context.WithoutCancel(ctx)); err != nil {
		return nil, err
	}
	next := st.ns
	next.Rendered, next.KeepData, next.NoRevision = ns.Rendered, ns.KeepData, ns.NoRevision
	if len(ns.Env) > 0 {
		next.Env = append(append([]string{}, st.ns.Env...), ns.Env...)
	}
	n, err := StartNode(ctx, st.lab.p, next)
	if err != nil {
		return nil, err
	}
	st.node = n
	return n, nil
}

// Lab returns the stage's lab services.
func (st *Stage) Lab() *Lab { return st.lab }

// Node returns the Node serving the stage's Revision.
func (st *Stage) Node() *Node { return st.node }

// PKI returns the stage's run PKI.
func (st *Stage) PKI() *RunPKI { return st.pki }

// Defer adds f to what Close runs, before what the stage added earlier.
func (st *Stage) Defer(f func()) { st.done = append(st.done, f) }

// NodeSample is one sample of the Node during a run: its CPU, its RSS, a
// scrape of its metrics and, with a State Store, Valkey's INFO counters.
type NodeSample struct {
	At      time.Time
	Usage   proc.Usage
	RSS     int64
	Metrics promtext.Set
	Valkey  *ValkeyInfo
	// Err is why the sample is incomplete; its other fields are then not
	// to be read.
	Err error
}

// sampler samples the Node every interval until stopped.
type sampler struct {
	mu      sync.Mutex
	samples []NodeSample
	stop    chan struct{}
	done    sync.WaitGroup
}

func startSampler(ctx context.Context, n *Node, every time.Duration, info func() (*ValkeyInfo, error)) *sampler {
	s := &sampler{stop: make(chan struct{})}
	take := func() {
		smp := NodeSample{At: time.Now()}
		smp.Usage, smp.Err = n.Usage()
		if smp.Err == nil {
			smp.RSS, smp.Err = n.RSS()
		}
		if smp.Err == nil {
			smp.Metrics, smp.Err = n.Scrape(ctx)
		}
		if smp.Err == nil && info != nil {
			smp.Valkey, smp.Err = info()
		}
		s.mu.Lock()
		s.samples = append(s.samples, smp)
		s.mu.Unlock()
	}
	take()
	s.done.Go(func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-s.stop:
				take()
				return
			case <-ctx.Done():
				return
			case <-t.C:
				take()
			}
		}
	})
	return s
}

func (s *sampler) finish() []NodeSample {
	close(s.stop)
	s.done.Wait()
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.samples)
}

// window picks the samples bracketing [start, end): the first at or after
// start and the last at or before end, or the nearest outside when fewer
// than two fall inside.
func window(samples []NodeSample, start, end time.Time) (a, b *NodeSample, err error) {
	var ok []NodeSample
	for _, s := range samples {
		if s.Err == nil {
			ok = append(ok, s)
		}
	}
	if len(ok) < 2 {
		return nil, nil, errors.New("fewer than two Node samples")
	}
	ai, bi := -1, -1
	for i, s := range ok {
		if ai < 0 && !s.At.Before(start) {
			ai = i
		}
		if !s.At.After(end) {
			bi = i
		}
	}
	if ai < 0 || bi < 0 || bi <= ai {
		// Too short for two samples inside: bracket it from outside.
		ai, bi = 0, len(ok)-1
		for i, s := range ok {
			if !s.At.After(start) {
				ai = i
			}
		}
		for i := len(ok) - 1; i >= 0; i-- {
			if !ok[i].At.Before(end) {
				bi = i
			}
		}
		if bi <= ai {
			return nil, nil, errors.New("no two Node samples bracket the measured window")
		}
	}
	return &ok[ai], &ok[bi], nil
}

// windowMeasures computes the resource measures between two samples.
func windowMeasures(a, b *NodeSample, metricBudgets []Budget) (Measures, int64, map[string]promtext.Histogram) {
	var m Measures
	requests := promtext.Delta(a.Metrics, b.Metrics, catalog.HTTPRequestsTotal, nil)
	cpu := (b.Usage.UserCPU + b.Usage.SysCPU) - (a.Usage.UserCPU + a.Usage.SysCPU)
	if requests > 0 {
		m.CPUPerRequest = time.Duration(float64(cpu) / requests)
	}
	gc := promtext.Delta(a.Metrics, b.Metrics, catalog.RuntimeGCCPUSecondsTotal, nil)
	if cpu > 0 {
		m.GCShare = math.Min(math.Max(gc/cpu.Seconds(), 0), 1)
	}
	m.RSSLoad = b.RSS
	hists := map[string]promtext.Histogram{}
	for _, bg := range metricBudgets {
		if bg.Kind != MetricP99 {
			continue
		}
		after, err1 := b.Metrics.Histogram(bg.Metric, promtext.Labels(bg.Labels))
		if err1 != nil {
			continue
		}
		before, err2 := a.Metrics.Histogram(bg.Metric, promtext.Labels(bg.Labels))
		if err2 != nil {
			before = promtext.Histogram{}
		}
		hists[MetricKey(bg.Metric, bg.Labels)] = histogramDelta(before, after)
	}
	m.Metrics = hists
	return m, int64(requests), hists
}

// foldWindows returns the sink windows covering a run: equal windows of at
// least 1 s from start over span, at most otlpsink's 64.
func foldWindows(start time.Time, span time.Duration) []struct{ start, end time.Time } {
	w := max(time.Second, (span+63)/64)
	w = w.Truncate(time.Second)
	if w < span/64 {
		w += time.Second
	}
	var out []struct{ start, end time.Time }
	for t := start; t.Before(start.Add(span)) && len(out) < 64; t = t.Add(w) {
		out = append(out, struct{ start, end time.Time }{t, t.Add(w)})
	}
	return out
}

// Run runs one side once and writes its record: a fresh PKI, lab and
// Node, then the warm-up and the measured phase.
func (s *Session) Run(ctx context.Context, spec RunSpec) (*RunOutcome, error) {
	if spec.Warmup == 0 {
		spec.Warmup = Warmup
	}
	st, err := s.prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer st.close()
	return s.measure(ctx, st, spec)
}

// Prepare makes a run's PKI, starts its lab services and the Node, and
// checks the Node serves the Revision, for a driver that acts on the stage
// between or during measured phases (Measure); the caller closes it.
func (s *Session) Prepare(ctx context.Context, spec RunSpec) (*Stage, error) {
	return s.prepare(ctx, spec)
}

// Measure offers spec's workload on a prepared stage, the warm-up and the
// measured phase, and writes the run's record; a negative spec.Warmup
// measures at once.
func (s *Session) Measure(ctx context.Context, st *Stage, spec RunSpec) (*RunOutcome, error) {
	if spec.Warmup == 0 {
		spec.Warmup = Warmup
	}
	return s.measure(ctx, st, spec)
}

// prepare makes the run's PKI, starts its lab services and the Node, and
// checks the Node serves the Revision.
func (s *Session) prepare(ctx context.Context, spec RunSpec) (st *Stage, err error) {
	p := s.Profile
	sc := spec.Scenario
	dir := filepath.Join(s.Work, "runs", sc.ID, spec.Name)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, err
	}
	st = &Stage{}
	defer func() {
		if err != nil {
			st.close()
		}
	}()
	if st.pki, err = NewRunPKI(filepath.Join(dir, "pki"), p); err != nil {
		return st, err
	}
	if err = st.pki.Distribute(ctx, p); err != nil {
		return st, err
	}
	if st.lab, err = NewLab(ctx, p, s.Labhost, st.pki, dir); err != nil {
		return st, err
	}
	st.done = append(st.done, func() { _ = st.lab.Close() })
	if err = st.lab.StartUpstream(ctx, sc.Workload); err != nil {
		return st, err
	}
	var storeURL string
	if sc.StateStore {
		if storeURL, err = st.lab.StartStateStore(ctx, sc); err != nil {
			return st, err
		}
	}
	st.started = time.Now().UTC().Truncate(time.Millisecond)
	ns := spec.Node
	ns.Binary, ns.StateStoreURL = spec.Build.Ruralzd, storeURL
	if ns.Rendered == "" {
		ns.Rendered = spec.Revision.Rendered
	}
	ns.Env = append(append([]string{}, spec.NodeEnv...), spec.Node.Env...)
	if st.node, err = StartNode(ctx, p, ns); err != nil {
		return st, fmt.Errorf("%w: %s %s: the Node did not start on Revision %s: %w", ErrRefused, sc.ID, spec.Name, spec.Revision.Rev, err)
	}
	st.ns = ns
	st.done = append(st.done, func() { _ = st.node.Stop(context.WithoutCancel(ctx)) })
	if !ns.NoRevision {
		rev, rerr := st.node.Revision(ctx)
		if rerr != nil {
			return st, fmt.Errorf("%w: %s %s: the Node serves %q, want %s: %w", ErrRefused, sc.ID, spec.Name, rev, spec.Revision.Rev, rerr)
		}
		if rev != spec.Revision.Rev {
			return st, fmt.Errorf("%w: %s %s: the Node serves %q, want %s", ErrRefused, sc.ID, spec.Name, rev, spec.Revision.Rev)
		}
	}
	if sc.StateStore {
		ca, rerr := os.ReadFile(st.pki.Path(FileCA))
		if rerr != nil {
			return st, rerr
		}
		ic, rerr := dialInfo(ctx, p.Services.StateStore, ca, StateStoreUser, st.lab.StateStorePassword)
		if rerr != nil {
			return st, rerr
		}
		st.done = append(st.done, func() { _ = ic.Close() })
		st.info = func() (*ValkeyInfo, error) {
			v, err := ic.info()
			v.At = time.Now()
			return &v, err
		}
	}
	return st, nil
}

// measure offers the workload on a prepared stage and writes the record.
func (s *Session) measure(ctx context.Context, st *Stage, spec RunSpec) (*RunOutcome, error) {
	sc := spec.Scenario
	w := sc.Workload
	warm, dur := max(spec.Warmup, 0), spec.Duration
	if dur == 0 {
		dur = Measured
	}
	l, node := st.lab, st.node
	maxWarm := warm
	if !spec.NoSettle && warm > 0 {
		maxWarm = max(warm, loadgen.BenchmarkSettle().Max)
	}
	windows := foldWindows(time.Now(), maxWarm+dur+foldSlack)
	if !spec.NoInternal {
		for i, fw := range windows {
			name := spec.Name + "-w" + strconv.Itoa(i)
			if _, err := l.Sink.Do(lab.Request{Op: lab.OpWindow, Name: name, Start: fw.start, End: fw.end}); err != nil {
				return nil, err
			}
		}
		defer func() {
			for i := range windows {
				_, _ = l.Sink.Do(lab.Request{Op: lab.OpRemove, Name: spec.Name + "-w" + strconv.Itoa(i)})
			}
		}()
	}
	if err := l.Reset(); err != nil {
		return nil, err
	}
	started := time.Now().UTC().Truncate(time.Millisecond)
	every := spec.Sample
	if every == 0 {
		every = min(max(dur/10, time.Second), 10*time.Second)
	}
	smp := startSampler(ctx, node, every, st.info)
	res, loadErr := s.load(ctx, l, w, warm, dur, !spec.NoSettle && warm > 0)
	samples := smp.finish()
	if loadErr != nil {
		return nil, loadErr
	}
	out := &RunOutcome{Samples: samples, Load: res}
	var reasons []string
	mockResp, err := l.Mock.Do(lab.Request{Op: lab.OpStats})
	if err != nil {
		return nil, err
	}
	ext, err := lab.DecodeHistogram(res.Histogram)
	if err != nil {
		return nil, err
	}
	var internal *loadgen.Histogram
	var drops int64
	if !spec.NoInternal {
		if err := waitExported(ctx, l.Sink); err != nil {
			return nil, err
		}
		internal, err = foldedWindow(l.Sink, spec.Name, windows, res.MeasureStart, res.MeasureEnd)
		if err != nil {
			return nil, err
		}
		if internal.Count() == 0 {
			reasons = append(reasons, "no access record fell in the measured window")
		}
		sinkResp, err := l.Sink.Do(lab.Request{Op: lab.OpStats})
		if err != nil {
			return nil, err
		}
		if sinkResp.Sink.Rejected > 0 {
			reasons = append(reasons, fmt.Sprintf("the OTLP sink rejected %d exports", sinkResp.Sink.Rejected))
		}
	}
	if len(samples) > 0 {
		first, last := samples[0], samples[len(samples)-1]
		if first.Err == nil && last.Err == nil {
			drops = int64(promtext.Delta(first.Metrics, last.Metrics, catalog.TelemetryLogsDroppedTotal, nil))
		}
	}
	a, b, werr := window(samples, res.MeasureStart, res.MeasureEnd)
	var m Measures
	var requests int64
	if werr != nil {
		reasons = append(reasons, "resources: "+werr.Error())
	} else {
		m, requests, out.Metrics = windowMeasures(a, b, sc.Budgets)
		m.RSSLoad = maxRSS(samples, res.MeasureStart, res.MeasureEnd, b.RSS)
	}
	m.External, m.Internal = ext, internal
	var profileFiles []string
	if spec.Profile {
		name, err := s.profile(ctx, l, node, w, sc.ID, spec.Name)
		if err != nil {
			return nil, err
		}
		profileFiles = append(profileFiles, name)
	}
	var idleRSS int64
	if spec.IdleSettle > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(spec.IdleSettle):
		}
		if idleRSS, err = node.RSS(); err != nil {
			return nil, err
		}
	}
	if node.Exited() {
		reasons = append(reasons, "the Node exited during the run: "+node.Stderr())
	}
	genShare, mockShare := res.Process.Share(), mockResp.Process.Share()
	excess := time.Duration(0)
	if mockResp.Mock != nil {
		excess = mockResp.Mock.ExcessP99
	}
	if !res.Valid {
		reasons = append(reasons, res.Invalid)
	}
	if res.RateError > MaxRateError {
		reasons = append(reasons, fmt.Sprintf("rate error %.4f above %.3f", res.RateError, MaxRateError))
	}
	if genShare >= MaxGeneratorCPU {
		reasons = append(reasons, fmt.Sprintf("generator CPU %.2f of its cpuset, at or above %.2f", genShare, MaxGeneratorCPU))
	}
	if mockShare >= MaxMockCPU {
		reasons = append(reasons, fmt.Sprintf("mock CPU %.2f of its cpuset, at or above %.2f", mockShare, MaxMockCPU))
	}
	if excess > MaxMockExcessP99 {
		reasons = append(reasons, fmt.Sprintf("mock p99 beyond its delay %s, above %s", excess, MaxMockExcessP99))
	}
	if drops > 0 {
		reasons = append(reasons, fmt.Sprintf("%d telemetry records dropped", drops))
	}
	reasons = dedupe(reasons)
	extLog, err := decodeLog(res.IntervalLog)
	if err != nil {
		return nil, err
	}
	extName, err := s.Rec.WriteFile(sc.ID, spec.Name+".external.hlog", extLog)
	if err != nil {
		return nil, err
	}
	files := []string{extName}
	if internal != nil {
		intLog, err := intervalLog(internal, res.MeasureStart, res.MeasureEnd, "ruralz bench internal view (access log gateway_duration)")
		if err != nil {
			return nil, err
		}
		intName, err := s.Rec.WriteFile(sc.ID, spec.Name+".internal.hlog", intLog)
		if err != nil {
			return nil, err
		}
		files = append(files, intName)
	}
	rec := s.baseRecord(sc, spec, started, benchrecord.KindMacro)
	rec.Load = benchrecord.Load{
		Tool: string(w.Tool), Version: s.ToolVersions[w.Tool], CommandLine: s.commandLine(w),
		OfferedRate: w.Rate, AchievedRate: res.AchievedRate, Connections: w.Connections,
		KeyCardinality: res.KeyCardinality, KeyDistribution: res.KeyDistribution, InjectedDelay: sc.Proxy.String(),
	}
	rec.Latency = benchrecord.Latency{External: Percentiles(ext), Internal: Percentiles(internal), HistogramFiles: files}
	rec.Resources = benchrecord.Resources{
		CPUPerRequest: m.CPUPerRequest, GCCPUShare: m.GCShare, RSSLoadBytes: m.RSSLoad, RSSIdleBytes: idleRSS,
		CPUProfileFiles: profileFiles,
	}
	if b != nil {
		rec.Resources.LiveHeapBytes = int64(b.Metrics.Sum(catalog.RuntimeHeapBytes, nil))
		if requests > 0 {
			rec.Resources.AllocsPerOp = promtext.Delta(a.Metrics, b.Metrics, catalog.RuntimeAllocationsTotal, nil) / float64(requests)
		}
		if a.Valkey != nil && b.Valkey != nil {
			secs := b.Valkey.At.Sub(a.Valkey.At).Seconds()
			if secs > 0 {
				rec.Resources.StateStoreCPUShare = math.Min(math.Max((b.Valkey.MainThreadCPU-a.Valkey.MainThreadCPU)/secs, 0), 1)
				rec.Resources.EvalshaMicrosPerSecond = math.Max((b.Valkey.EvalshaMicros-a.Valkey.EvalshaMicros)/secs, 0)
			}
		}
	}
	rec.Validity = benchrecord.Validity{
		RateError: res.RateError, GeneratorCPU: math.Min(genShare, 1), MockCPU: math.Min(mockShare, 1),
		MockExcessP99: excess, Drops: drops, Verdict: benchrecord.VerdictValid, Reasons: reasons,
	}
	if len(reasons) > 0 {
		rec.Validity.Verdict = benchrecord.VerdictInvalid
	}
	m.Saturation = 0
	rec.Budgets = EvaluateAll(sc.Budgets, &m)
	out.Record, out.Measures, out.Valid, out.ErrorRate = rec, m, len(reasons) == 0, res.ErrorRate
	out.MeasureStart, out.MeasureEnd = res.MeasureStart, res.MeasureEnd
	if spec.Criteria != nil {
		rec.Budgets = append(rec.Budgets, spec.Criteria(out)...)
	}
	path, err := s.Rec.Write(spec.Name, rec)
	if err != nil {
		return nil, err
	}
	out.Path = path
	return out, nil
}

// load runs the workload's tool on the load host.
func (s *Session) load(ctx context.Context, l *Lab, w Workload, warm, dur time.Duration, settle bool) (*lab.LoadResult, error) {
	spec, err := l.LoadSpec(w, s.Profile.Hosts[RoleGateway].Address, warm, dur, settle)
	if err != nil {
		return nil, err
	}
	switch w.Tool {
	case ToolOha, ToolVegeta:
		path, ok := s.Tools[w.Tool]
		if !ok {
			return nil, fmt.Errorf("harness: %s is not installed (%s, %s)", w.Tool, EnvOha, EnvVegeta)
		}
		spec.Tool, spec.ToolPath = string(w.Tool), path
	case ToolLoadgen:
	default:
		return nil, fmt.Errorf("harness: unknown tool %q", w.Tool)
	}
	return l.RunLoad(ctx, spec)
}

// profile offers the same rate for the profile's length plus a margin and
// takes the Node's CPU profile meanwhile.
func (s *Session) profile(ctx context.Context, l *Lab, n *Node, w Workload, scenario, name string) (string, error) {
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	var loadErr error
	wg.Go(func() {
		_, loadErr = s.load(pctx, l, w, 0, (ProfileSeconds+5)*time.Second, false)
	})
	time.Sleep(2 * time.Second) // let the load start before the profile window opens
	b, err := n.CPUProfile(ctx, ProfileSeconds)
	wg.Wait()
	if err != nil {
		return "", err
	}
	if loadErr != nil {
		return "", loadErr
	}
	return s.Rec.WriteFile(scenario, name+".cpu.pprof", b)
}

// foldedWindow merges the sink windows lying inside [start, end).
func foldedWindow(sink *Proc, prefix string, windows []struct{ start, end time.Time }, start, end time.Time) (*loadgen.Histogram, error) {
	h, err := Pool()
	if err != nil {
		return nil, err
	}
	for i, fw := range windows {
		if fw.start.Before(start) || fw.end.After(end) {
			continue
		}
		resp, err := sink.Do(lab.Request{Op: lab.OpResult, Name: prefix + "-w" + strconv.Itoa(i)})
		if err != nil {
			return nil, err
		}
		wh, err := lab.DecodeHistogram(resp.Window.Histogram)
		if err != nil {
			return nil, err
		}
		if err := h.Merge(wh); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// Export settling: the Node exports access records in batches, so after
// the load the harness waits until the sink's count of access records
// stops moving for exportQuiet, within exportWait.
const (
	exportQuiet = 2 * time.Second
	exportWait  = 30 * time.Second
	exportPoll  = 250 * time.Millisecond
)

// waitExported waits until the sink received every access record the
// Node exported: its record count unchanged for exportQuiet. A sink still
// receiving after exportWait leaves the late records out of the internal
// view, as the record count shows.
func waitExported(ctx context.Context, sink *Proc) error {
	deadline := time.Now().Add(exportWait)
	last, since := int64(-1), time.Now()
	for time.Now().Before(deadline) {
		resp, err := sink.Do(lab.Request{Op: lab.OpStats})
		if err != nil {
			return err
		}
		if n := resp.Sink.Records; n != last {
			last, since = n, time.Now()
		} else if time.Since(since) >= exportQuiet {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(exportPoll):
		}
	}
	return nil
}

// maxRSS returns the highest RSS sampled in [start, end], or def.
func maxRSS(samples []NodeSample, start, end time.Time, def int64) int64 {
	v := def
	for _, s := range samples {
		if s.Err == nil && !s.At.Before(start) && !s.At.After(end) {
			v = max(v, s.RSS)
		}
	}
	return v
}

// commandLine records how the workload ran.
func (s *Session) commandLine(w Workload) string {
	target := "http://" + s.Profile.Hosts[RoleGateway].Address + ":" + strconv.Itoa(w.Port) + w.Path
	switch w.Tool {
	case ToolOha:
		return "oha " + strings.Join(lab.OhaArgs(lab.LoadSpec{Rate: w.Rate, Connections: w.Connections, Method: w.Method, Protocol: w.Protocol, URL: target}, Measured, ""), " ")
	case ToolVegeta:
		return "vegeta " + strings.Join(lab.VegetaArgs(lab.LoadSpec{Rate: w.Rate, Connections: w.Connections, Protocol: w.Protocol}, Measured, ""), " ") + " (" + w.Method + " " + target + ")"
	default:
		return fmt.Sprintf("labhost loadgen rate=%s connections=%d streams=%d protocol=%s tokens=%d keyHeader=%s %s %s",
			strconv.FormatFloat(w.Rate, 'f', -1, 64), w.Connections, w.Streams, w.Protocol, w.Tokens, w.KeyHeader, w.Method, target)
	}
}

// BaseRecord returns a record of kind for sc with the Identity and
// Environment of spec's run started at started, for a driver that writes
// records of its own (Recorder.Write).
func (s *Session) BaseRecord(sc Scenario, spec RunSpec, started time.Time, kind string) *benchrecord.Record {
	return s.baseRecord(sc, spec, started, kind)
}

// baseRecord fills Identity and Environment.
func (s *Session) baseRecord(sc Scenario, spec RunSpec, started time.Time, kind string) *benchrecord.Record {
	f := s.Facts
	cpus := s.Profile.Hosts[RoleGateway].CPUs
	if cpus == "" {
		cpus = f.Cpuset
	}
	procs := s.Profile.GOMAXPROCS
	if procs == 0 {
		if list, ok := proc.ParseCPUList(f.Cpuset); ok {
			procs = len(list)
		} else {
			procs = 1
		}
	}
	nic := make([]string, 0, len(f.NIC))
	for k, v := range f.NIC {
		nic = append(nic, k+" "+v)
	}
	slices.Sort(nic)
	gomemlimit := ""
	for _, e := range spec.NodeEnv {
		if v, ok := strings.CutPrefix(e, "GOMEMLIMIT="); ok {
			gomemlimit = v
		}
	}
	kernel := f.Kernel
	if kernel == "" {
		kernel = "unknown"
	}
	return &benchrecord.Record{
		Format: benchrecord.Format,
		Identity: benchrecord.Identity{
			Scenario: sc.ID, Kind: kind, Commit: spec.Build.Commit, Version: spec.Build.Version, Baseline: spec.Baseline,
			RevisionDigest: spec.Revision.Digest, Toolchain: spec.Build.Toolchain, StartedAt: started,
		},
		Environment: benchrecord.Environment{
			HardwareProfile: s.Profile.HardwareProfile(), Microarchitecture: f.Microarchitecture, Kernel: kernel,
			Cpuset: cpus, NIC: strings.Join(nic, "; "), GOMAXPROCS: procs, GOGC: "100", GOMEMLIMIT: gomemlimit,
			Sysctls: f.Sysctls,
		},
		Budgets: []benchrecord.BudgetResult{},
	}
}

func dedupe(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x != "" && !slices.Contains(out, x) {
			out = append(out, x)
		}
	}
	return out
}
