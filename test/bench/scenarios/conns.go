// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package scenarios

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// Connection memory rows (PBB "Memory budget"), targets.
const (
	// IdleCleartextBytes bounds an idle cleartext connection: 24 KiB.
	IdleCleartextBytes = 24 << 10
	// IdleTLSBytes bounds an idle TLS connection: 96 KiB.
	IdleTLSBytes = 96 << 10
	// ClientConnections is the Node's client connection ceiling, which C1
	// and C2 hold: 20,000.
	ClientConnections = 20000
)

// C2 bounds (PBB "Scenarios"), targets.
const (
	// C2ExistingP99 bounds the existing traffic's external p99 through
	// C2's burst: 5 ms.
	C2ExistingP99 = 5 * time.Millisecond
	// C2HandshakeP99 bounds the handshake p99 of
	// ruralz_listener_tls_handshake_duration_seconds: 1 s.
	C2HandshakeP99 = time.Second
)

// GOMEMLIMIT run settings (PBB "Memory budget"), targets: a 2.5 GiB
// container with GOMEMLIMIT at 90% of it.
const (
	MemContainerBytes = 2560 << 20
	MemLimitShare     = 0.90
)

// connsPoll is how often a driver reads the conns service's counters.
const connsPoll = 250 * time.Millisecond

// connsWait bounds the wait for every connection of a conns service to be
// set up or fail: its count at its rate, plus the setup timeout.
func connsWait(c lab.ConnsConfig) time.Duration {
	d := 30 * time.Second
	if c.Rate > 0 {
		d += time.Duration(float64(c.Count) / c.Rate * float64(time.Second))
	}
	return d
}

// waitConns polls p until every connection was set up or failed, within
// limit; it returns the last counters.
func waitConns(ctx context.Context, p *harness.Proc, limit time.Duration) (*lab.ConnsStats, error) {
	deadline := time.Now().Add(limit)
	for {
		resp, err := p.Do(lab.Request{Op: lab.OpStats})
		if err != nil {
			return nil, err
		}
		st := resp.Conns
		if st == nil {
			return nil, errors.New("scenarios: the conns service answered no counters")
		}
		if st.Done || time.Now().After(deadline) {
			return st, nil
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(connsPoll):
		}
	}
}

// connsStats reads p's counters once.
func connsStats(p *harness.Proc) (*lab.ConnsStats, error) {
	resp, err := p.Do(lab.Request{Op: lab.OpStats})
	if err != nil {
		return nil, err
	}
	if resp.Conns == nil {
		return nil, errors.New("scenarios: the conns service answered no counters")
	}
	return resp.Conns, nil
}

// held judges a conns service holding every one of want connections: all
// set up, none failed and none closed by the Node.
func held(id, value string, st *lab.ConnsStats, want int) benchrecord.BudgetResult {
	if st == nil {
		return judge(id, value, false, "no counters")
	}
	ok := st.Opened == int64(want) && st.Open == int64(want) && st.Failed == 0 && st.ClosedByPeer == 0
	result := fmt.Sprintf("%d of %d set up, %d held, %d failed, %d closed by the Node", st.Opened, want, st.Open, st.Failed, st.ClosedByPeer)
	if len(st.Errors) > 0 {
		result += "; first failure: " + st.Errors[0]
	}
	return judge(id, value, ok, result)
}

// openConnections returns the Node's open client connections on the
// listener named listener, from ruralz_listener_open_connections.
func openConnections(s harness.NodeSample, listener string) float64 {
	return s.Metrics.Sum(metricOpenConns, promtext.L("listener", listener))
}

// listenerHeld judges the Node's view: its open connections on the
// listener reach want while the client holds them.
func listenerHeld(id, value string, s harness.NodeSample, listener string, want int) benchrecord.BudgetResult {
	if s.Err != nil {
		return judge(id, value, false, "no Node sample: "+s.Err.Error())
	}
	n := openConnections(s, listener)
	return judge(id, value, n >= float64(want), fmt.Sprintf("%s open on listener %s", fmtCount(n), listener))
}

// perConnection judges a connection memory row: the RSS the connections
// added, over their count, against limit bytes.
func perConnection(id, value string, before, after int64, n int, limit int64) benchrecord.BudgetResult {
	if n < 1 || before <= 0 || after <= 0 {
		return judge(id, value, false, "no RSS reading")
	}
	per := (after - before) / int64(n)
	return judge(id, value, per <= limit, fmt.Sprintf("%s per connection (RSS %s to %s over %d connections)", kib(per), mib(before), mib(after), n))
}

// kib and mib format byte counts.
func kib(b int64) string { return strconv.FormatFloat(float64(b)/(1<<10), 'f', 1, 64) + " KiB" }
func mib(b int64) string { return strconv.FormatFloat(float64(b)/(1<<20), 'f', 1, 64) + " MiB" }

// sample takes one Node sample now.
func sample(ctx context.Context, n *harness.Node) harness.NodeSample {
	s := harness.NodeSample{At: time.Now()}
	s.Usage, s.Err = n.Usage()
	if s.Err == nil {
		s.RSS, s.Err = n.RSS()
	}
	if s.Err == nil {
		s.Metrics, s.Err = n.Scrape(ctx)
	}
	return s
}

// sampleAfter takes a Node sample once a fresh collection is due after
// prev: the Node shares one metrics collection among the scrapes of its
// reuse window (internal/telemetry/aggregate ReuseWindow), so a scrape
// within it of prev would read prev's values again.
func sampleAfter(ctx context.Context, n *harness.Node, prev harness.NodeSample) (harness.NodeSample, error) {
	if err := sleepCtx(ctx, time.Until(prev.At.Add(aggregate.ReuseWindow+100*time.Millisecond))); err != nil {
		return harness.NodeSample{}, err
	}
	return sample(ctx, n), nil
}

// gatewayHost returns the address the load host dials for the Node.
func gatewayHost(s *harness.Session) string { return s.Profile.Hosts[harness.RoleGateway].Address }

// Listener names and ports of the scenario Bundles.
const (
	listenerHTTP  = "http"
	listenerHTTPS = "https"
	portHTTP      = 8080
	portHTTPS     = 8443
)

// c1Run is one of C1's runs: cleartext on 8080 or TLS on 8443.
type c1Run struct {
	id, listener, kind string
	port               int
	limit              int64
	row                string
}

// c1Runs returns C1's cleartext and TLS runs.
func c1Runs() []c1Run {
	return []c1Run{
		{id: "C1-cleartext", listener: listenerHTTP, kind: lab.ConnsIdleHTTP1, port: portHTTP, limit: IdleCleartextBytes, row: "<= 24 KiB per idle cleartext connection (target)"},
		{id: "C1-tls", listener: listenerHTTPS, kind: lab.ConnsIdleH2, port: portHTTPS, limit: IdleTLSBytes, row: "<= 96 KiB per idle TLS connection (target)"},
	}
}

// RunC1 runs C1 (PBB "Scenarios"): idle client connections, a cleartext
// run and a TLS run on S3's Bundle. Each connection completes one exchange
// (a GET of /bench/1k over HTTP/1.1, or the TLS handshake and the HTTP/2
// preface and SETTINGS) and then idles; the run holds them for its hold
// time and judges that the Node holds every one (C1 holds the 20,000
// client connections, PBB "Constants the suite verifies") and, on RH-1,
// the RSS each added against the connection memory rows.
func RunC1(ctx context.Context, e Env, s Scale) (*Result, error) {
	if err := e.needFiles("C1", s.C1.Connections); err != nil {
		return nil, err
	}
	res := &Result{}
	s3, err := catalog("S3")
	if err != nil {
		return nil, err
	}
	for _, run := range c1Runs() {
		sc := scenario(run.id, s.C1.Dir, s3.Workload, s3.Saturation)
		if err := e.connsRun(ctx, sc, run, s, res); err != nil {
			return nil, err
		}
	}
	return res.finish(), nil
}

// connsRun runs one of C1's runs and writes its record.
func (e Env) connsRun(ctx context.Context, sc harness.Scenario, run c1Run, s Scale, res *Result) error {
	if err := e.check(ctx, sc); err != nil {
		return err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return err
	}
	spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: "run-1"}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return err
	}
	defer st.Close()
	node := st.Node()
	before := sample(ctx, node)
	started := time.Now().UTC().Truncate(time.Millisecond)
	cc := lab.ConnsConfig{
		Kind: run.kind, Target: net.JoinHostPort(gatewayHost(e.S), strconv.Itoa(run.port)),
		Count: s.C1.Connections, Rate: s.C1.OpenRate, Path: benchPath, Host: harness.NameGateway,
	}
	if run.kind == lab.ConnsIdleH2 {
		cc.CAFile, cc.ServerName = harness.FileCA, harness.NameGateway
	}
	p, err := st.Lab().StartConns(ctx, "conns-"+run.id, cc)
	if err != nil {
		return err
	}
	if _, err := waitConns(ctx, p, connsWait(cc)); err != nil {
		return err
	}
	if err := sleepCtx(ctx, s.C1.Hold); err != nil {
		return err
	}
	cs, err := connsStats(p)
	if err != nil {
		return err
	}
	after, err := sampleAfter(ctx, node, before)
	if err != nil {
		return err
	}
	budgets := []benchrecord.BudgetResult{
		held(run.id+"-held", "every idle connection held by the Node (target)", cs, s.C1.Connections),
		listenerHeld(run.id+"-listener", "the listener counts every idle connection open (target)", after, run.listener, s.C1.Connections),
		s.hardware(perConnection(run.id+"-memory", run.row, before.RSS, after.RSS, s.C1.Connections, run.limit)),
	}
	var reasons []string
	if node.Exited() {
		reasons = append(reasons, "the Node exited during the run: "+node.Stderr())
	}
	rec := e.S.BaseRecord(sc, spec, started, benchrecord.KindMacro)
	rec.Load = benchrecord.Load{
		Tool: "conns", Version: e.S.ToolVersions[harness.ToolLoadgen],
		CommandLine: "labhost conns " + strings.Join(lab.ConnsArgs(cc), " "),
		OfferedRate: s.C1.OpenRate, AchievedRate: achievedOpenRate(cs), Connections: s.C1.Connections,
		KeyDistribution: "none", InjectedDelay: "none",
	}
	rec.Resources = benchrecord.Resources{RSSLoadBytes: after.RSS, LiveHeapBytes: int64(after.Metrics.Sum(metricHeap, nil))}
	path, err := e.writeRecord(rec, "run-1", budgets, reasons)
	if err != nil {
		return err
	}
	res.add(sc.ID, path, budgets)
	if len(reasons) > 0 {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(reasons, "; "))
	}
	return nil
}

// achievedOpenRate returns the connections set up per second over their
// spread.
func achievedOpenRate(st *lab.ConnsStats) float64 {
	if st == nil || st.Spread <= 0 {
		return 0
	}
	return float64(st.Opened) / st.Spread.Seconds()
}

// writeRecord validates a driver's own record and writes it as name: the
// run's budgets, and a valid verdict unless reasons say otherwise.
func (e Env) writeRecord(rec *benchrecord.Record, name string, budgets []benchrecord.BudgetResult, reasons []string) (string, error) {
	rec.Budgets = append(rec.Budgets, budgets...)
	rec.Latency.HistogramFiles = []string{}
	rec.Validity = benchrecord.Validity{Verdict: benchrecord.VerdictValid, Reasons: reasons}
	if len(reasons) > 0 {
		rec.Validity.Verdict = benchrecord.VerdictInvalid
	}
	return e.S.Rec.Write(name, rec)
}

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// benchPath is the scenario Bundles' bench-1k path.
const benchPath = "/bench/1k"

// RunC2 runs C2 (PBB "Scenarios"): S3 at half saturation, then a burst of
// new TLS connections within its window during the measured phase. It
// judges, on RH-1, the existing traffic's external p99 within 5 ms and the
// handshake p99 of ruralz_listener_tls_handshake_duration_seconds within
// 1 s, and everywhere that every burst connection completes its handshake
// and stays held (C2 holds every connection, PBB "Constants the suite
// verifies").
func RunC2(ctx context.Context, e Env, s Scale) (*Result, error) {
	res := &Result{}
	s3, err := catalog("S3")
	if err != nil {
		return nil, err
	}
	if err := e.needFiles("C2", s3.Workload.Connections+s.C2.Burst); err != nil {
		return nil, err
	}
	warm, dur, _ := e.lengths(s)
	if warm == 0 {
		warm = harness.Warmup
	}
	if dur == 0 {
		dur = harness.Measured
	}
	w := s3.Workload
	w.Rate = s.C2.Rate
	sc := scenario("C2", s.C2.Dir, w, s3.Saturation)
	if err := e.check(ctx, sc); err != nil {
		return nil, err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return nil, err
	}
	spec := harness.RunSpec{Scenario: sc, Build: e.Head, Revision: rev, Name: "run-1", Warmup: warm, Duration: dur, NoSettle: true, Sample: sampleEvery}
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	cc := lab.ConnsConfig{
		Kind: lab.ConnsIdleH2, Target: net.JoinHostPort(gatewayHost(e.S), strconv.Itoa(portHTTPS)),
		Count: s.C2.Burst, Rate: float64(s.C2.Burst) / s.C2.Within.Seconds(),
		CAFile: harness.FileCA, ServerName: harness.NameGateway,
	}
	// The burst starts a third into the measured phase, so the existing
	// traffic's measured window holds the time before, during and after
	// it.
	burstAt := warm + dur/3
	bctx, cancel := context.WithCancel(ctx)
	defer cancel()
	type started struct {
		p   *harness.Proc
		err error
	}
	burst := make(chan started, 1)
	go func() {
		if err := sleepCtx(bctx, burstAt); err != nil {
			burst <- started{err: err}
			return
		}
		p, err := st.Lab().StartConns(bctx, "conns-C2", cc)
		burst <- started{p: p, err: err}
	}()
	var cs *lab.ConnsStats
	var burstErr error
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		b := <-burst
		burstErr = b.err
		if b.p != nil {
			cs, burstErr = waitConns(ctx, b.p, connsWait(cc))
		}
		win := inWindow(o.Samples, o.MeasureStart, o.MeasureEnd)
		return []benchrecord.BudgetResult{
			held("C2-held", fmt.Sprintf("%d new TLS connections within %s, every one held (target)", s.C2.Burst, s.C2.Within), cs, s.C2.Burst),
			s.hardware(externalP99("C2-existing-p99", "existing traffic's external p99 <= 5ms (target)", o, C2ExistingP99)),
			s.hardware(quantileBound("C2-handshake-p99", "TLS handshake p99 <= 1s (target)", win, metricHandshake, promtext.L("listener", listenerHTTPS), 0.99, C2HandshakeP99)),
		}
	}
	out, err := e.S.Measure(ctx, st, spec)
	if err != nil {
		return nil, err
	}
	if burstErr != nil {
		return nil, fmt.Errorf("scenarios: C2's burst: %w", burstErr)
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return res.finish(), nil
}

// externalP99 judges a run's external p99 against limit.
func externalP99(id, value string, o *harness.RunOutcome, limit time.Duration) benchrecord.BudgetResult {
	h := o.Measures.External
	if h == nil || h.Count() == 0 {
		return judge(id, value, false, "no external view")
	}
	v := h.ValueAtPercentile(99)
	return judge(id, value, v <= limit, fmt.Sprintf("%s over %d requests", v, h.Count()))
}

// RunGOMEMLIMIT runs the informational GOMEMLIMIT run (PBB "Memory
// budget"): C1 over TLS plus S2 in a 2.5 GiB container with GOMEMLIMIT at
// 90% of it, so the limit binds, reporting GC CPU. S3's Bundle serves both
// on one Node; the idle TLS connections leave the 256 of S2's load within
// the Node's 20,000-connection ceiling. On the loopback profile, which has
// no scopes, only GOMEMLIMIT applies.
func RunGOMEMLIMIT(ctx context.Context, e Env, s Scale) (*Result, error) {
	if err := e.needFiles(ItemGOMEMLIMIT, s.Mem.Connections); err != nil {
		return nil, err
	}
	res := &Result{}
	s2, err := catalog("S2")
	if err != nil {
		return nil, err
	}
	warm, dur, noSettle := e.lengths(s)
	w := s2.Workload
	w.Rate = s.Mem.Rate
	sc := scenario(ItemGOMEMLIMIT, s.Mem.Dir, w, s2.Saturation)
	limit := int64(float64(s.Mem.Container) * MemLimitShare)
	idle := min(s.Mem.Connections, ClientConnections-w.Connections)
	spec := harness.RunSpec{
		Warmup: warm, Duration: dur, NoSettle: noSettle, Sample: sampleEvery,
		NodeEnv: []string{"GOMEMLIMIT=" + strconv.FormatInt(limit>>20, 10) + "MiB"},
		Node:    harness.NodeSpec{ScopeProperties: []string{"MemoryMax=" + strconv.FormatInt(s.Mem.Container>>20, 10) + "M"}},
	}
	if err := e.check(ctx, sc); err != nil {
		return nil, err
	}
	rev, err := harness.Render(ctx, e.Head.Ruralz, e.S.RepoRoot, sc, e.S.Profile)
	if err != nil {
		return nil, err
	}
	spec.Scenario, spec.Build, spec.Revision, spec.Name = sc, e.Head, rev, "run-1"
	st, err := e.S.Prepare(ctx, spec)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	cc := lab.ConnsConfig{
		Kind: lab.ConnsIdleH2, Target: net.JoinHostPort(gatewayHost(e.S), strconv.Itoa(portHTTPS)),
		Count: idle, Rate: s.C1.OpenRate, CAFile: harness.FileCA, ServerName: harness.NameGateway,
	}
	p, err := st.Lab().StartConns(ctx, "conns-gomemlimit", cc)
	if err != nil {
		return nil, err
	}
	if _, err := waitConns(ctx, p, connsWait(cc)); err != nil {
		return nil, err
	}
	spec.Criteria = func(o *harness.RunOutcome) []benchrecord.BudgetResult {
		cs, err := connsStats(p)
		if err != nil {
			cs = nil
		}
		return []benchrecord.BudgetResult{
			held("S2-gomemlimit-held", "the idle TLS connections held through S2's load (target)", cs, idle),
			{ID: "S2-gomemlimit-gc", Value: "informational: GC CPU with GOMEMLIMIT at 90% of a 2.5 GiB container",
				Result: "GC CPU share " + strconv.FormatFloat(o.Measures.GCShare*100, 'f', 2, 64) + "%", Verdict: benchrecord.BudgetSkipped},
		}
	}
	out, err := e.S.Measure(ctx, st, spec)
	if err != nil {
		return nil, err
	}
	res.add(sc.ID, out.Path, criteriaOf(out.Record))
	if !out.Valid {
		res.Invalid = append(res.Invalid, sc.ID+": "+strings.Join(out.Record.Validity.Reasons, "; "))
	}
	return res.finish(), nil
}
