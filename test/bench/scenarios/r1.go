// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	telcatalog "github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/gen"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// R1Size is one row of the size-class table (PBB "Startup and reload time
// by configuration size"), targets; byte budgets are decimal.
type R1Size struct {
	Routes int
	// Cold is the cold start to /readyz 200 from rendered YAML, LKG from
	// Last-Known-Good, Reload the Hot Reload of an idle Node.
	Cold, LKG, Reload time.Duration
	// Heap is the snapshot live heap, Peak the peak configuration memory
	// of a Hot Reload.
	Heap, Peak int64
}

// R1Table returns the size-class table.
func R1Table() []R1Size {
	return []R1Size{
		{Routes: 100, Cold: 300 * time.Millisecond, LKG: 200 * time.Millisecond, Reload: 50 * time.Millisecond, Heap: 50_000, Peak: 200_000},
		{Routes: 1000, Cold: 600 * time.Millisecond, LKG: 300 * time.Millisecond, Reload: 125 * time.Millisecond, Heap: 500_000, Peak: 2_000_000},
		{Routes: 5000, Cold: 1600 * time.Millisecond, LKG: 700 * time.Millisecond, Reload: 500 * time.Millisecond, Heap: 2_500_000, Peak: 10_000_000},
		{Routes: 10000, Cold: 3 * time.Second, LKG: 1300 * time.Millisecond, Reload: time.Second, Heap: 5_000_000, Peak: 20_000_000},
		{Routes: 17000, Cold: 5 * time.Second, LKG: 2200 * time.Millisecond, Reload: 1700 * time.Millisecond, Heap: 8_500_000, Peak: 34_000_000},
	}
}

// R1StageRoutes is the size whose activation stages the stage table
// budgets, and whose loaded reload and gateway-added p99 are budgeted.
const R1StageRoutes = 5000

// R1Stage is one row of the Hot Reload stage table for 5,000 Routes on an
// idle Node (PBB "Hot Reload stages"), targets.
type R1Stage struct {
	Stage string
	Limit time.Duration
}

// R1Stages returns the stage table: the stage label values of
// ruralz_config_activation_duration_seconds with their budgets.
func R1Stages() []R1Stage {
	return []R1Stage{
		{Stage: "verify", Limit: 25 * time.Millisecond},
		{Stage: "compile", Limit: 400 * time.Millisecond},
		{Stage: "plugin_compile", Limit: 0},
		{Stage: "swap", Limit: 25 * time.Millisecond},
		{Stage: "total", Limit: 500 * time.Millisecond},
	}
}

// R1 loaded and memory budgets, targets unless noted.
const (
	// R1LoadedReload bounds a Hot Reload of 5,000 Routes under S1 at half
	// saturation: 1 s.
	R1LoadedReload = time.Second
	// R1LoadedP99 bounds the gateway-added p99 meanwhile: 1.5 ms
	// (hypothesis).
	R1LoadedP99 = 1500 * time.Microsecond
	// PB6Base and PB6PerRoute are PB-6's allowance: 100 MB plus 2 KB per
	// Route.
	PB6Base     = 100_000_000
	PB6PerRoute = 2_000
	// R1SlopeFrom and R1Slope bound the RSS slope fitted over R1 from
	// 1,000 Routes up: 2 KB per Route.
	R1SlopeFrom = 1000
	R1Slope     = 2_000
)

// activationMetric is the activation duration histogram of every stage.
const activationMetric = telcatalog.ConfigActivationDurationSeconds

// r1Wait bounds the wait for one activation to show at /readyz.
const r1Wait = 2 * time.Minute

// r1Variant is one generated and rendered Revision of a size.
type r1Variant struct {
	rev  harness.Revision
	file string
}

// r1Idle is what a size's idle measurements found.
type r1Idle struct {
	cold, lkg     time.Duration
	lkgRev        string
	heap          int64
	reloads       int
	reloadMax     time.Duration
	stages        map[string]time.Duration
	peakMax       int64
	rssIdle       int64
	lastActivated string
}

// labOf returns the generator's lab of the session's profile: the
// loopback profile's fixed addresses, or the bench.example names of RH-1
// and RH-3.
func labOf(p *harness.Profile) gen.Lab {
	if p.Name == harness.ProfileLoopback {
		return gen.LoopbackLab()
	}
	return gen.NamedLab()
}

// RunR1 runs R1 (PBB "Startup and reload time by configuration size"): for
// each size of the ladder, the two Revisions test/bench/gen generates, a
// cold start from rendered YAML, the first activation into a Node with no
// Revision for the snapshot live heap, idle Hot Reloads alternating the two
// Revisions with their stage durations and peak configuration memory, the
// idle settle for PB-6's RSS, a cold start from Last-Known-Good, and a run
// under S1's workload at half saturation with Hot Reloads during it. One
// record per size holds the loaded run's measures and every size-class
// criterion; a summary fits the RSS slope over the sizes from 1,000
// Routes. Timings and memory are judged on RH-1 only; everywhere, every
// activation must take effect, the Last-Known-Good start must serve the
// last activated Revision and no request may fail across the reloads.
func RunR1(ctx context.Context, e Env, s Scale) (*Result, error) {
	res := &Result{}
	s1, err := catalog("S1")
	if err != nil {
		return nil, err
	}
	var paths []string
	var routes []float64
	var rss []float64
	for _, r := range s.R1.Sizes {
		path, idle, err := e.r1Size(ctx, s, s1, r, res)
		if err != nil {
			return nil, err
		}
		paths = append(paths, path)
		if r >= R1SlopeFrom {
			routes, rss = append(routes, float64(r)), append(rss, float64(idle.rssIdle))
		}
	}
	budgets := []benchrecord.BudgetResult{s.hardware(r1Slope(routes, rss))}
	rec := e.S.BaseRecord(harness.Scenario{ID: "R1"}, harness.RunSpec{Build: e.Head}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	rec.Summary = &benchrecord.Summary{Of: benchrecord.OfLadder, Runs: paths, HeadRuns: len(paths), Verdict: benchrecord.ABNone, Ladder: benchrecord.Ladder{Steps: []benchrecord.LadderStep{}}}
	rec.Load = benchrecord.Load{Tool: string(s1.Workload.Tool), KeyDistribution: "none", InjectedDelay: "none"}
	path, err := e.writeRecord(rec, "summary-ladder", budgets, nil)
	if err != nil {
		return nil, err
	}
	res.add("R1", path, budgets)
	return res.finish(), nil
}

// r1Slope judges the RSS slope, least squares over the sizes from 1,000
// Routes.
func r1Slope(routes, rss []float64) benchrecord.BudgetResult {
	const id, value = "R1-rss-slope", "RSS slope from 1,000 to 17,000 Routes <= 2 KB per Route (target)"
	if len(routes) < 2 {
		return skipped(id, value, "fewer than two sizes of 1,000 Routes or more")
	}
	slope := leastSquares(routes, rss)
	return judge(id, value, slope <= R1Slope, strconv.FormatFloat(slope, 'f', 0, 64)+" bytes per Route")
}

// leastSquares returns the slope of the least-squares line through (x, y).
func leastSquares(x, y []float64) float64 {
	var mx, my float64
	for i := range x {
		mx += x[i]
		my += y[i]
	}
	n := float64(len(x))
	mx, my = mx/n, my/n
	var sxy, sxx float64
	for i := range x {
		sxy += (x[i] - mx) * (y[i] - my)
		sxx += (x[i] - mx) * (x[i] - mx)
	}
	if sxx == 0 {
		return math.NaN()
	}
	return sxy / sxx
}

// r1Size runs one size and returns its record's path.
func (e Env) r1Size(ctx context.Context, s Scale, s1 harness.Scenario, routes int, res *Result) (string, r1Idle, error) {
	var idle r1Idle
	dir := filepath.Join(e.S.Work, "r1", strconv.Itoa(routes))
	l := labOf(e.S.Profile)
	var vs [2]r1Variant
	for v := range 2 {
		src := filepath.Join(dir, "bundle-"+strconv.Itoa(v))
		if err := gen.Write(src, gen.Spec{Routes: routes, Variant: v, Lab: l}); err != nil {
			return "", idle, err
		}
		file := filepath.Join(dir, "rendered-"+strconv.Itoa(v)+".yaml")
		rev, err := renderBundle(ctx, e.Head.Ruralz, src, file)
		if err != nil {
			return "", idle, err
		}
		vs[v] = r1Variant{rev: rev, file: file}
	}
	w := s1.Workload
	w.Rate = s.R1.LoadedRate
	if s.Functional {
		w.Tool = harness.ToolLoadgen
	}
	name := "size-" + strconv.Itoa(routes)
	sc := harness.Scenario{ID: "R1", Bundle: filepath.Join(dir, "bundle-0"), Workload: w, Saturation: s1.Saturation}
	if err := e.check(ctx, sc); err != nil {
		return "", idle, err
	}
	spec := harness.RunSpec{
		Scenario: sc, Build: e.Head, Revision: vs[0].rev, Name: name,
		Warmup: noWarmup(s.R1.LoadedWarmup), Duration: s.R1.LoadedDuration, NoSettle: true, Sample: sampleEvery,
	}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return "", idle, err
	}
	defer st.Close()
	node := st.Node()
	idle.cold = node.Ready().Sub(node.Started())
	host, _, err := net.SplitHostPort(e.S.Profile.Services.Mock)
	if err != nil {
		return "", idle, err
	}
	if _, err := st.Lab().StartMock(ctx, "mock-2", net.JoinHostPort(host, strconv.Itoa(l.MockPorts[1])), w); err != nil {
		return "", idle, err
	}
	cfg := filepath.Join(dir, "config")
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		return "", idle, err
	}
	if node, err = st.StartNode(ctx, harness.NodeSpec{Rendered: cfg, NoRevision: true}); err != nil {
		return "", idle, err
	}
	heap0, err := node.HeapAlloc(ctx)
	if err != nil {
		return "", idle, err
	}
	if _, err := activate(ctx, node, cfg, vs[0]); err != nil {
		return "", idle, err
	}
	heap1, err := node.HeapAlloc(ctx)
	if err != nil {
		return "", idle, err
	}
	idle.heap, idle.lastActivated = heap1-heap0, vs[0].rev.Rev
	idle.stages = map[string]time.Duration{}
	for i := range s.R1.Reloads {
		v := vs[(i+1)%2]
		rssBefore, err := node.RSS()
		if err != nil {
			return "", idle, err
		}
		if err := node.ResetPeak(); err != nil {
			return "", idle, err
		}
		stages, err := activate(ctx, node, cfg, v)
		if err != nil {
			return "", idle, err
		}
		peak, err := node.Peak()
		if err != nil {
			return "", idle, err
		}
		idle.reloads++
		idle.lastActivated = v.rev.Rev
		idle.reloadMax = max(idle.reloadMax, stages["total"])
		for k, d := range stages {
			idle.stages[k] = max(idle.stages[k], d)
		}
		idle.peakMax = max(idle.peakMax, peak-rssBefore)
	}
	if err := sleepCtx(ctx, s.R1.IdleSettle); err != nil {
		return "", idle, err
	}
	if idle.rssIdle, err = node.RSS(); err != nil {
		return "", idle, err
	}
	empty := filepath.Join(dir, "empty")
	if err := os.MkdirAll(empty, 0o750); err != nil {
		return "", idle, err
	}
	if node, err = st.StartNode(ctx, harness.NodeSpec{Rendered: empty, KeepData: true}); err != nil {
		return "", idle, err
	}
	idle.lkg = node.Ready().Sub(node.Started())
	if idle.lkgRev, err = node.Revision(ctx); err != nil {
		return "", idle, err
	}
	if err := place(cfg, vs[0].file); err != nil {
		return "", idle, err
	}
	if node, err = st.StartNode(ctx, harness.NodeSpec{Rendered: cfg}); err != nil {
		return "", idle, err
	}
	loaded := e.loadedReloads(ctx, node, cfg, vs, s.R1.LoadedWarmup, s.R1.LoadedDuration, s.R1.LoadedEvery)
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		lr := <-loaded
		return r1Criteria(s, routes, idle, lr, o)
	}
	out, err := e.S.Measure(ctx, st, spec)
	if err != nil {
		return "", idle, err
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+" "+name+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return out.Path, idle, nil
}

// noWarmup returns d, or -1 for none: a zero RunSpec.Warmup takes the
// benchmark's 60 s.
func noWarmup(d time.Duration) time.Duration {
	if d <= 0 {
		return -1
	}
	return d
}

// r1Loaded is what the Hot Reloads under load found.
type r1Loaded struct {
	reloads   int
	reloadMax time.Duration
	err       error
}

// loadedReloads activates the two Revisions in turn every period during
// the measured phase that starts after warm and lasts dur, and sends what
// it found once done.
func (e Env) loadedReloads(ctx context.Context, n *harness.Node, cfg string, vs [2]r1Variant, warm, dur, period time.Duration) <-chan r1Loaded {
	out := make(chan r1Loaded, 1)
	go func() {
		var r r1Loaded
		defer func() { out <- r }()
		start := time.Now()
		if err := sleepCtx(ctx, warm+period); err != nil {
			r.err = err
			return
		}
		for i := 0; time.Since(start)+period <= warm+dur; i++ {
			stages, err := activate(ctx, n, cfg, vs[(i+1)%2])
			if err != nil {
				r.err = err
				return
			}
			r.reloads++
			r.reloadMax = max(r.reloadMax, stages["total"])
			if err := sleepCtx(ctx, period); err != nil {
				r.err = err
				return
			}
		}
	}()
	return out
}

// r1Criteria returns a size's criteria.
func r1Criteria(s Scale, routes int, idle r1Idle, lr r1Loaded, o *harness.RunOutcome) []benchrecord.BudgetResult {
	var row *R1Size
	for _, r := range R1Table() {
		if r.Routes == routes {
			row = &r
		}
	}
	out := []benchrecord.BudgetResult{
		judge("R1-activations", "every Hot Reload activates its Revision", lr.err == nil,
			fmt.Sprintf("%d idle and %d loaded Hot Reloads activated%s", idle.reloads, lr.reloads, errSuffix(lr.err))),
		judge("R1-lkg-revision", "the Last-Known-Good start serves the last activated Revision", idle.lkgRev == idle.lastActivated,
			fmt.Sprintf("serves %s, last activated %s", idle.lkgRev, idle.lastActivated)),
		judge("R1-loaded-failures", "no request fails across the Hot Reloads", o.Load != nil && failedRequests(o.Load) == 0,
			fmt.Sprintf("%d of %d requests failed", failedRequests(o.Load), offered(o.Load))),
	}
	if row == nil {
		return out
	}
	sz := strconv.Itoa(routes) + " Routes"
	reloadID, reloadSLO := "R1-reload", ""
	if routes == R1StageRoutes {
		reloadID, reloadSLO = "PB-7", "SLO-GW-6"
	}
	out = append(out,
		s.hardware(durationBound("R1-cold-start", "cold start to /readyz 200 from rendered YAML, "+sz+", <= "+row.Cold.String()+" (target)", idle.cold, row.Cold)),
		s.hardware(durationBound("R1-lkg-start", "cold start from Last-Known-Good, "+sz+", <= "+row.LKG.String()+" (target)", idle.lkg, row.LKG)),
		s.hardware(withSLO(durationBound(reloadID, "Hot Reload of an idle Node, "+sz+", <= "+row.Reload.String()+" (target)", idle.reloadMax, row.Reload), reloadSLO)),
		s.hardware(bytesBound("R1-snapshot-heap", "snapshot live heap, "+sz+", <= "+decimalBytes(row.Heap)+" (target)", idle.heap, row.Heap)),
		s.hardware(bytesBound("R1-peak-config", "peak configuration memory of a Hot Reload, "+sz+", <= "+decimalBytes(row.Peak)+" (target)", idle.peakMax, row.Peak)),
		s.hardware(bytesBound("PB-6", "base RSS <= 100 MB + 2 KB x "+strconv.Itoa(routes)+" Routes after the idle settle (target)", idle.rssIdle, PB6Base+PB6PerRoute*int64(routes))),
	)
	if routes == R1StageRoutes {
		for _, st := range R1Stages() {
			if st.Stage == "total" {
				continue
			}
			out = append(out, s.hardware(durationBound("R1-stage-"+strings.ReplaceAll(st.Stage, "_", "-"),
				"Hot Reload stage "+st.Stage+", 5,000 Routes, <= "+st.Limit.String()+" (target)", idle.stages[st.Stage], st.Limit)))
		}
		out = append(out,
			s.hardware(durationBound("R1-loaded-reload", "Hot Reload of 5,000 Routes under S1 at half saturation <= 1s (target)", lr.reloadMax, R1LoadedReload)),
			s.hardware(internalP99("R1-loaded-p99", "gateway-added p99 under S1 at half saturation across Hot Reloads <= 1.5ms (hypothesis)", o, R1LoadedP99)),
		)
	}
	return out
}

// errSuffix formats an optional error after a result.
func errSuffix(err error) string {
	if err == nil {
		return ""
	}
	return "; " + err.Error()
}

// durationBound and bytesBound judge a measure against its limit.
func durationBound(id, value string, v, limit time.Duration) benchrecord.BudgetResult {
	return judge(id, value, v <= limit, v.String())
}

func bytesBound(id, value string, v, limit int64) benchrecord.BudgetResult {
	return judge(id, value, v <= limit, decimalBytes(v))
}

// decimalBytes formats bytes in decimal units, as the size-class table
// states them.
func decimalBytes(b int64) string {
	switch a := math.Abs(float64(b)); {
	case a >= 1e6:
		return strconv.FormatFloat(float64(b)/1e6, 'f', 2, 64) + " MB"
	case a >= 1e3:
		return strconv.FormatFloat(float64(b)/1e3, 'f', 1, 64) + " KB"
	default:
		return strconv.FormatInt(b, 10) + " B"
	}
}

// internalP99 judges a run's internal p99 against limit.
func internalP99(id, value string, o *harness.RunOutcome, limit time.Duration) benchrecord.BudgetResult {
	h := o.Measures.Internal
	if h == nil || h.Count() == 0 {
		return judge(id, value, false, "no internal view")
	}
	v := h.ValueAtPercentile(99)
	return judge(id, value, v <= limit, fmt.Sprintf("%s over %d requests", v, h.Count()))
}

// place writes file into dir as its ruralz.yaml by an atomic rename.
func place(dir, file string) error {
	b, err := os.ReadFile(file) //nolint:gosec // G304: a rendered Bundle of the run directory
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ".ruralz.yaml.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil { //nolint:gosec // G703: a copy of a rendered Bundle of the run directory
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "ruralz.yaml"))
}

// activate replaces the Node's source with v, sends SIGHUP so it loads at
// once, waits until /readyz serves v and returns the activation's stage
// durations, read from the activation duration histogram's sums.
func activate(ctx context.Context, n *harness.Node, dir string, v r1Variant) (map[string]time.Duration, error) {
	before, err := n.Scrape(ctx)
	if err != nil {
		return nil, err
	}
	if err := place(dir, v.file); err != nil {
		return nil, err
	}
	if err := n.Signal(syscall.SIGHUP); err != nil {
		return nil, err
	}
	if _, err := n.WaitRevision(ctx, v.rev.Rev, r1Wait); err != nil {
		return nil, err
	}
	// The activation records its stages as it finishes; wait for its
	// total to count.
	total := promtext.L("stage", "total")
	deadline := time.Now().Add(10 * time.Second)
	for {
		after, err := n.Scrape(ctx)
		if err != nil {
			return nil, err
		}
		if promtext.Delta(before, after, activationMetric+"_count", total) >= 1 || time.Now().After(deadline) {
			out := map[string]time.Duration{}
			for _, st := range R1Stages() {
				d := promtext.Delta(before, after, activationMetric+"_sum", promtext.L("stage", st.Stage))
				out[st.Stage] = time.Duration(d * float64(time.Second))
			}
			return out, nil
		}
		if err := sleepCtx(ctx, 20*time.Millisecond); err != nil {
			return nil, err
		}
	}
}

// renderBundle renders the Bundle at src into file with the head's ruralz
// and builds its Revision offline.
func renderBundle(ctx context.Context, ruralz, src, file string) (harness.Revision, error) {
	run := func(args ...string) ([]byte, error) {
		cmd := exec.CommandContext(ctx, ruralz, args...) //nolint:gosec // the head's ruralz
		var out, errb bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &errb
		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("scenarios: ruralz %s: %w: %s", strings.Join(args, " "), err, errb.String())
		}
		return out.Bytes(), nil
	}
	if _, err := run("bundle", "render", src, "--output-file", file); err != nil {
		return harness.Revision{}, err
	}
	out, err := run("bundle", "build", src, "--offline", "--output", "json")
	if err != nil {
		return harness.Revision{}, err
	}
	var doc struct {
		Digest   string `json:"digest"`
		Revision string `json:"revision"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return harness.Revision{}, fmt.Errorf("scenarios: ruralz bundle build output %q: %w", out, err)
	}
	if doc.Digest == "" {
		return harness.Revision{}, fmt.Errorf("scenarios: ruralz bundle build output %q: no digest", out)
	}
	return harness.Revision{Rendered: file, Digest: doc.Digest, Rev: doc.Revision}, nil
}

// failedRequests counts a load's offered requests of the measured phase
// that got no 2xx response.
func failedRequests(r *lab.LoadResult) int64 {
	if r == nil {
		return 0
	}
	var ok int64
	for status, n := range r.Statuses {
		if status >= 200 && status <= 299 {
			ok += n
		}
	}
	return r.Offered - ok
}

// offered returns a load's offered requests of the measured phase.
func offered(r *lab.LoadResult) int64 {
	if r == nil {
		return 0
	}
	return r.Offered
}
