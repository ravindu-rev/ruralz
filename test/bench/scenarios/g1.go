// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// G1 bounds and settings (PBB "Scenarios"), targets.
const (
	// G1ScriptCPU is the script CPU share G1 climbs to: 70%.
	G1ScriptCPU = 0.70
	// G1P99 is the call p99 that ends G1's climb: 1 ms.
	G1P99 = time.Millisecond
	// G1Requests and G1Window are the limit each call checks, S2's
	// ratelimit-bench, which denies none at G1's key rates.
	G1Requests = 20000
	G1Window   = time.Second
	// G1Timeout is each call's stateStoreTimeout.
	G1Timeout = time.Second
)

// g1Step is one measured step of G1's climb.
type g1Step struct {
	rate, achieved, rateError, errorRate float64
	p99                                  time.Duration
	// cpu is the script CPU share over the step, from Valkey's
	// main-thread CPU per 1 s window; evalsha is cmdstat_evalsha µs per
	// second.
	cpu, evalsha float64
	// clients is the most connected clients seen, the harness's INFO
	// connection excluded.
	clients int64
	path    string
}

// RunG1 runs G1 (PBB "Scenarios"): open-loop GCRA EVALSHA calls through
// the State client against one Valkey shard, keys cycling S2's 100,000
// values, in steps from From climbing by Step until the calls' p99 exceeds
// 1 ms or Valkey's script CPU reaches 70%; then the connection variant,
// whose emulated Nodes each open their own connections. The result is the
// per-shard calls per second at 70% script CPU, interpolated between steps
// (judged on RH-1), with cmdstat_evalsha µs per second as an ungated
// breakdown; everywhere, each emulated Node holds at most 2 plus 8
// connections to the shard (PBB "Constants the suite verifies").
func RunG1(ctx context.Context, e Env, s Scale) (*Result, error) {
	// The connection variant's generator opens up to 2 plus 8 connections
	// per emulated Node.
	if err := e.needFiles("G1", s.G1.Nodes*ShardConnections); err != nil {
		return nil, err
	}
	res := &Result{}
	s5, err := catalog("S5")
	if err != nil {
		return nil, err
	}
	// G1 emulates S5's Nodes' calls, so its records name S5's Revision.
	sc := scenario("G1", "s5", s5.Workload, s5.Saturation)
	sc.StateStore = true
	if err := e.check(ctx, sc); err != nil {
		return nil, err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return nil, err
	}
	g, err := newG1Lab(ctx, e.S, sc)
	if err != nil {
		return nil, err
	}
	defer g.close()
	dur := s.G1.StepDuration
	var steps []g1Step
	for i := range s.G1.Steps {
		rate := s.G1.From + float64(i)*s.G1.Step
		st, err := g.step(ctx, e, sc, rev, "step-"+strconv.Itoa(i+1), rate, dur, 1)
		if err != nil {
			return nil, err
		}
		steps = append(steps, st)
		res.Records = append(res.Records, sc.ID+"/"+st.path)
		if st.p99 > G1P99 || st.cpu >= G1ScriptCPU {
			break
		}
	}
	variant, err := g.step(ctx, e, sc, rev, "nodes-"+strconv.Itoa(s.G1.Nodes), s.G1.From, dur, s.G1.Nodes)
	if err != nil {
		return nil, err
	}
	res.Records = append(res.Records, sc.ID+"/"+variant.path)
	budgets := []benchrecord.BudgetResult{
		s.hardware(g1Rate(steps)),
		shardConnections("G1-connections", "at most 2 plus 8 connections per emulated Node to the shard (target)", steps[0].clients, 1),
		shardConnections("G1-node-connections", fmt.Sprintf("at most 2 plus 8 connections to the shard for each of %d emulated Nodes (target)", s.G1.Nodes), variant.clients, s.G1.Nodes),
		judge("G1-node-calls", "the emulated Nodes' calls all answered within Valkey's 20,000 clients (target)", variant.errorRate == 0,
			fmt.Sprintf("error rate %.4f", variant.errorRate)),
	}
	path, err := g.summary(e, sc, rev, steps, budgets)
	if err != nil {
		return nil, err
	}
	res.add(sc.ID, path, budgets)
	return res.finish(), nil
}

// g1Rate judges G1's figure: the calls per second at 70% script CPU,
// interpolated between the steps below and at or above it.
func g1Rate(steps []g1Step) benchrecord.BudgetResult {
	const id, value = "G1-calls-at-70", "per-shard GCRA calls per second at 70% script CPU, interpolated, before p99 exceeds 1ms (target)"
	rates := make([]float64, len(steps))
	cpus := make([]float64, len(steps))
	for i, st := range steps {
		rates[i], cpus[i] = st.achieved, st.cpu
	}
	r, ok := interpolate(rates, cpus, G1ScriptCPU)
	last := steps[len(steps)-1]
	switch {
	case ok:
		return judge(id, value, true, strconv.FormatFloat(r, 'f', 0, 64)+" calls/s at 70% script CPU")
	case len(steps) > 0 && cpus[0] >= G1ScriptCPU:
		return judge(id, value, false, fmt.Sprintf("the first step, %s calls/s, already reached %.1f%% script CPU", strconv.FormatFloat(rates[0], 'f', 0, 64), cpus[0]*100))
	default:
		return judge(id, value, false, fmt.Sprintf("70%% script CPU not reached: the last step, %s calls/s, ran at %.1f%% with p99 %s",
			strconv.FormatFloat(last.achieved, 'f', 0, 64), last.cpu*100, last.p99))
	}
}

// shardConnections judges a shard's connected clients against 2 plus 8
// per emulated Node.
func shardConnections(id, value string, clients int64, nodes int) benchrecord.BudgetResult {
	limit := int64(ShardConnections * nodes)
	return judge(id, value, clients >= 1 && clients <= limit, fmt.Sprintf("%d connections for %d emulated Nodes, at most %d", clients, nodes, limit))
}

// g1Lab is G1's lab: the run PKI and Valkey, with no Node.
type g1Lab struct {
	l    *harness.Lab
	url  string
	ca   []byte
	info *harness.InfoClient
}

// newG1Lab makes the run PKI and starts Valkey.
func newG1Lab(ctx context.Context, s *harness.Session, sc harness.Scenario) (g *g1Lab, err error) {
	dir := filepath.Join(s.Work, "runs", sc.ID)
	rp, err := harness.NewRunPKI(filepath.Join(dir, "pki"), s.Profile)
	if err != nil {
		return nil, err
	}
	if err := rp.Distribute(ctx, s.Profile); err != nil {
		return nil, err
	}
	l, err := harness.NewLab(ctx, s.Profile, s.Labhost, rp, dir)
	if err != nil {
		return nil, err
	}
	g = &g1Lab{l: l}
	defer func() {
		if err != nil {
			g.close()
		}
	}()
	if g.url, err = l.StartStateStore(ctx, sc); err != nil {
		return g, err
	}
	if g.ca, err = l.CA(); err != nil {
		return g, err
	}
	g.info, err = harness.DialInfo(ctx, s.Profile.Services.StateStore, g.ca, harness.StateStoreUser, l.StateStorePassword)
	return g, err
}

func (g *g1Lab) close() {
	if g.info != nil {
		_ = g.info.Close()
	}
	_ = g.l.Close()
}

// step runs one step at rate for dur with nodes emulated Nodes, sampling
// Valkey's INFO every second, and writes its macro record as name.
func (g *g1Lab) step(ctx context.Context, e Env, sc harness.Scenario, rev harness.Revision, name string, rate float64, dur time.Duration, nodes int) (g1Step, error) {
	spec := lab.LoadSpec{
		Rate: rate, Duration: dur, CA: string(g.ca),
		Keys:        &lab.KeySpec{Kind: lab.KeysCycle, Prefix: "key-", N: harness.BenchKeyCardinality},
		MaxInFlight: StateInFlight,
		GCRA:        &lab.GCRASpec{URL: g.url, Nodes: nodes, Requests: G1Requests, Window: G1Window, Timeout: G1Timeout},
	}
	started := time.Now().UTC().Truncate(time.Millisecond)
	var mu sync.Mutex
	var infos []harness.ValkeyInfo
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Go(func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			if v, err := g.info.Info(); err == nil {
				mu.Lock()
				infos = append(infos, v)
				mu.Unlock()
			}
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-t.C:
			}
		}
	})
	lr, err := g.l.RunLoad(ctx, spec)
	close(stop)
	wg.Wait()
	if err != nil {
		return g1Step{}, err
	}
	h, err := lab.DecodeHistogram(lr.Histogram)
	if err != nil {
		return g1Step{}, err
	}
	st := g1Step{rate: rate, achieved: lr.AchievedRate, rateError: lr.RateError, errorRate: lr.ErrorRate, p99: h.ValueAtPercentile(99)}
	st.cpu, st.evalsha, st.clients = valkeyWindow(infos, lr.MeasureStart, lr.MeasureEnd)
	var reasons []string
	if !lr.Valid {
		reasons = append(reasons, lr.Invalid)
	}
	rec := e.S.BaseRecord(sc, harness.RunSpec{Build: e.Head, Revision: rev}, started, benchrecord.KindMacro)
	rec.Load = benchrecord.Load{
		Tool: string(harness.ToolLoadgen), Version: e.S.ToolVersions[harness.ToolLoadgen],
		CommandLine: fmt.Sprintf("labhost loadgen gcra rate=%s nodes=%d keys=cycle(%d) limit=%d/%s", strconv.FormatFloat(rate, 'f', -1, 64), nodes, harness.BenchKeyCardinality, G1Requests, G1Window),
		OfferedRate: rate, AchievedRate: lr.AchievedRate, Connections: max(int(st.clients), 1),
		KeyCardinality: lr.KeyCardinality, KeyDistribution: lr.KeyDistribution, InjectedDelay: "none",
	}
	rec.Latency.External = harness.Percentiles(h)
	rec.Resources = benchrecord.Resources{StateStoreCPUShare: math.Min(st.cpu, 1), EvalshaMicrosPerSecond: st.evalsha}
	rec.Validity.RateError = lr.RateError
	rec.Validity.GeneratorCPU = math.Min(lr.Process.Share(), 1)
	path, err := e.writeRecord(rec, name, nil, dedupe(reasons))
	if err != nil {
		return g1Step{}, err
	}
	st.path = path
	return st, nil
}

// valkeyWindow returns the script CPU share and cmdstat_evalsha µs per
// second over the INFO samples inside [start, end] (their first and last),
// and the most connected clients among them less the INFO connection.
func valkeyWindow(infos []harness.ValkeyInfo, start, end time.Time) (cpu, evalsha float64, clients int64) {
	var in []harness.ValkeyInfo
	for _, v := range infos {
		if !v.At.Before(start) && !v.At.After(end) {
			in = append(in, v)
		}
		clients = max(clients, v.ConnectedClients-1)
	}
	if len(in) < 2 {
		in = infos
	}
	if len(in) < 2 {
		return 0, 0, clients
	}
	a, b := in[0], in[len(in)-1]
	secs := b.At.Sub(a.At).Seconds()
	if secs <= 0 {
		return 0, 0, clients
	}
	return math.Max((b.MainThreadCPU-a.MainThreadCPU)/secs, 0), math.Max((b.EvalshaMicros-a.EvalshaMicros)/secs, 0), clients
}

// summary writes G1's summary record: the steps as a ladder, the
// variant's record and the budgets.
func (g *g1Lab) summary(e Env, sc harness.Scenario, rev harness.Revision, steps []g1Step, budgets []benchrecord.BudgetResult) (string, error) {
	rec := e.S.BaseRecord(sc, harness.RunSpec{Build: e.Head, Revision: rev}, time.Now().UTC().Truncate(time.Millisecond), benchrecord.KindSummary)
	sum := &benchrecord.Summary{Of: benchrecord.OfLadder, Verdict: benchrecord.ABNone, Ladder: benchrecord.Ladder{Steps: []benchrecord.LadderStep{}}}
	for _, st := range steps {
		sum.Runs = append(sum.Runs, st.path)
		sum.HeadRuns++
		sum.Ladder.Steps = append(sum.Ladder.Steps, benchrecord.LadderStep{
			OfferedRate: st.rate, AchievedRate: st.achieved, RateError: st.rateError, ErrorRate: math.Min(st.errorRate, 1), P99: st.p99,
		})
	}
	rec.Summary = sum
	rec.Load = benchrecord.Load{Tool: string(harness.ToolLoadgen), Version: e.S.ToolVersions[harness.ToolLoadgen], KeyDistribution: "cycle", InjectedDelay: "none"}
	return e.writeRecord(rec, "summary-ladder", budgets, nil)
}

// dedupe drops empty and repeated lines.
func dedupe(xs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, x := range xs {
		if x != "" && !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
