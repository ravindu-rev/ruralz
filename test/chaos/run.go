// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/l4lb"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Timeline defaults of every experiment
// (docs/architecture/11-scalability-and-distributed-state.md "Chaos
// experiments"): a 60 s baseline, the fault for its row's duration, else
// 60 s, then 60 s of recovery (target).
const (
	Baseline     = 60 * time.Second
	FaultPhase   = 60 * time.Second
	RecoveryTime = 60 * time.Second
)

// Phase names of the timeline.
const (
	PhaseBaseline = "baseline"
	PhaseFault    = "fault"
	PhaseRecovery = "recovery"
)

// Spec describes one run.
type Spec struct {
	// Name is the record's experiment, such as "CE-1" or "CE-5/sentinel".
	Name string
	// Measurement runs the release build in real time, for latency and
	// CPU criteria; otherwise the functional build, on the controllable
	// clock when it has one.
	Measurement bool
	// MeasureKind is the kind of a measurement run's criteria, its
	// timeline's validity rules included: Latency (the default) or CPU.
	// They gate only where the profile gates that kind, since a
	// measurement run's functional criteria belong to a functional run.
	MeasureKind Kind
	// Nodes overrides the profile's Node count, as CE-5's runs do.
	Nodes int
	// StateStore configures the main State Store; the zero value is a
	// standalone redis-server with TLS and an ACL user.
	StateStore topology.StateStoreOptions
	// CacheStore, when set, starts the State Store of a stateStore.cache
	// connection, and the Bundle takes OverlayCacheConnection.
	CacheStore *topology.StateStoreOptions
	// Bundle adjusts the Bundle parameters after the harness filled the
	// backends, topology and overlay.
	Bundle func(*BundleParams)
	// HTTPS starts a balancer over the https listener beside the http
	// one.
	HTTPS bool
	// SampleEvery is the /metrics sampling period; default
	// DefaultSampleEvery.
	SampleEvery time.Duration
	// Keep lists extra metric families the sampler keeps beside
	// DefaultKeep.
	Keep []string
	// NodeEnv is added to every Node's environment.
	NodeEnv []string
	// Zones, when set, are the failure zones the Nodes split over, as
	// evenly as possible with earlier zones taking the remainder
	// (ZoneSplit), each Node's RURALZ_ZONE, as GD-2 splits 10 Nodes 4, 3
	// and 3 over zones b, c and a; Run.Zone names a Node's.
	Zones []string
	// PumpStep overrides DefaultPumpStep.
	PumpStep time.Duration
	// Mock sets each mock Upstream's configuration before it starts
	// (Name and LogSize are the harness's); a target profile run, whose
	// mocks run on RH-1U as labhost's, takes none.
	Mock func(i int, c *mockup.Config)
	// Node adjusts the Nodes' options after the harness filled them, as
	// GD-5 keeps OverlayEnvStateStore's reference as authored and gives
	// its value in the Nodes' environment.
	Node func(r *Run, o *topology.NodeOptions)
	// Balancer adjusts an L4 balancer's settings, by the listener it
	// balances, before it starts, as GD-2 dials one zone's Nodes through
	// fault proxies.
	Balancer func(r *Run, listener string, c *l4lb.Config)
}

// DefaultKeep are the metric families every run samples: degraded
// reasons, State Store calls and dropped writes, Rate Limit and Quota
// decisions, bucket evictions and the Upstream counters.
func DefaultKeep() []string {
	return []string{
		catalog.NodeDegradedInfo,
		"ruralz_state_",
		"ruralz_ratelimit_",
		catalog.QuotaDecisionsTotal,
		"ruralz_cache_",
		"ruralz_upstream_",
		catalog.FilterFailuresTotal,
		catalog.HTTPRequestsTotal,
		catalog.HTTPRequestDurationSeconds,
		catalog.ConfigActivationsTotal,
	}
}

// Run is one run of an experiment, drill or row: the topology and its
// dependencies, the clock pump, the sampler and the record being built.
type Run struct {
	T     testing.TB
	H     *Harness
	Spec  Spec
	Nodes int
	Top   *topology.Topology
	Store *topology.StateStore
	Cache *topology.StateStore
	// Mocks are the in-process mock Upstreams; none at the target profile,
	// whose mocks run on RH-1U, at Params.Backends.
	Mocks   [3]*mockup.Server
	HTTP    *topology.Balancer
	HTTPS   *topology.Balancer
	Pump    *Pump
	Sampler *Sampler
	// APIKeys maps each Consumer to its API key, a canary.
	APIKeys map[string]string
	// Params are the Bundle parameters the Nodes serve.
	Params BundleParams
	// Bundle is the rendered chaos Bundle.
	Bundle []byte
	// Binary is the ruralzd the run drives.
	Binary string
	// TestClock reports that the Nodes follow the controllable clock.
	TestClock bool

	ctx    context.Context
	zones  map[string]string // Node name to zone, with Spec.Zones
	mu     sync.Mutex
	rec    Record
	cursor time.Time
	loads  []*Load
	logs   []IntervalLog
	// session is the load service on the load host at the target
	// profile, started with the first load.
	session *loadSession
}

// Start starts a run for t: the topology over the run's build, the State
// Stores, three mock Upstreams, the chaos Bundle rendered for the run on
// the profile's Node count, the Nodes, the balancers, the clock pump and
// the sampler, each where the harness's Placement puts it at the target
// profile and on the harness's own host otherwise. It fails t when any of
// them does not start, and skips it when the profile does not run the
// experiment.
func (h *Harness) Start(t testing.TB, s Spec) *Run {
	t.Helper()
	if !h.Profile.Runs(s.Name) {
		t.Skipf("chaos: the %s profile does not run %s", h.Profile.Name, s.Name)
	}
	ctx := t.Context()
	r := &Run{T: t, H: h, Spec: s, Nodes: h.Profile.Nodes, ctx: ctx, APIKeys: map[string]string{}}
	if s.Nodes > 0 {
		r.Nodes = s.Nodes
	}
	bin := h.Functional
	if s.Measurement {
		bin = h.Measurement
	}
	r.Binary, r.TestClock = bin.Ruralzd, bin.TestClock
	var backends []string
	if h.place != nil {
		if s.Mock != nil {
			t.Fatalf("chaos: %s configures its mock Upstreams, which run on the upstream host as labhost's at the target profile", s.Name)
		}
		// Started before the topology, the mocks stop after its teardown
		// stopped the Nodes.
		var err error
		if backends, err = h.place.StartMocks(ctx, t, len(r.Mocks)); err != nil {
			t.Fatal(err)
		}
	}
	tc := topology.Config{Binary: bin.Ruralzd, TestClock: bin.TestClock, Digest: h.Digest}
	if h.place != nil {
		tc.CPUs = h.place.NodeCPUs()
	}
	r.Top = topology.New(t, tc)
	r.Store = r.Top.StartStateStore("main", h.placeStore(s.StateStore))
	if s.CacheStore != nil {
		r.Cache = r.Top.StartStateStore("cache", h.placeStore(*s.CacheStore))
	}
	if h.place == nil {
		for i := range r.Mocks {
			// Chaos runs keep no mock request log, as benchmark runs do:
			// its size would follow the load.
			c := mockup.Config{}
			if s.Mock != nil {
				s.Mock(i, &c)
			}
			c.Name, c.LogSize = fmt.Sprintf("backend-%d", i+1), -1
			m, err := r.Top.StartMockUpstream(c)
			if err != nil {
				t.Fatal(err)
			}
			r.Mocks[i] = m
			backends = append(backends, m.Addr())
		}
	}
	r.Params = DefaultBundleParams(r.Nodes)
	copy(r.Params.Backends[:], backends)
	r.Params.Topology = TopologyName(r.Store.Deployment().Topology())
	switch {
	case s.CacheStore != nil && (s.CacheStore.RESPProxy || r.Cache.CAPEM() == nil):
		r.Params.Overlay = OverlayCacheCleartext
	case s.CacheStore != nil:
		r.Params.Overlay = OverlayCacheConnection
	case s.StateStore.RESPProxy || r.Store.CAPEM() == nil:
		r.Params.Overlay = OverlayCleartext
	}
	if s.Bundle != nil {
		s.Bundle(&r.Params)
	}
	var err error
	if r.Bundle, err = RenderBundle(ctx, h.pl, r.Params); err != nil {
		t.Fatal(err)
	}
	opts, err := r.nodeOptions()
	if err != nil {
		t.Fatal(err)
	}
	if err := r.addNodes(ctx, opts); err != nil {
		t.Fatal(err)
	}
	r.HTTP = r.startBalancer("http")
	if s.HTTPS {
		r.HTTPS = r.startBalancer("https")
	}
	if r.TestClock {
		r.Pump = StartPump(ctx, r.Top, clock.Real(), s.PumpStep)
	}
	r.Sampler = StartSampler(ctx, SamplerConfig{
		Client:  &http.Client{Transport: &http.Transport{TLSClientConfig: r.Top.ClientTLS()}},
		Targets: r.scrapeTargets,
		Every:   s.SampleEvery,
		Keep:    append(DefaultKeep(), s.Keep...),
		Node:    r.nodeNow,
	})
	r.rec = Record{Format: Format, Experiment: s.Name, Profile: h.Profile.Name, Commit: h.Commit, Nodes: r.Nodes, Breakdowns: map[string]float64{}}
	if sat, err := h.Saturation(); err == nil {
		r.rec.Breakdowns[SaturationBreakdown] = sat
	}
	t.Cleanup(r.stopBackground)
	return r
}

// nodeOptions returns the Nodes' options: the rendered Bundle, the
// State Store URLs through each Node's proxies, the https listener's
// certificate and the Consumers' API keys, every secret a canary.
func (r *Run) nodeOptions() (topology.NodeOptions, error) {
	cert, key, err := r.Top.ServerCertificate()
	if err != nil {
		return topology.NodeOptions{}, err
	}
	o := topology.NodeOptions{
		Bundle:         r.Bundle,
		StateStoreURLs: map[string]string{RefStateStoreURL: "main"},
		Secrets:        map[string][]byte{RefTLSCertificate: cert, RefTLSKey: key},
		Env:            r.Spec.NodeEnv,
	}
	if ca := r.Store.CAPEM(); ca != nil {
		o.Secrets[RefStateStoreCA] = ca
	}
	if r.Cache != nil {
		o.StateStoreURLs[RefCacheURL] = "cache"
		if ca := r.Cache.CAPEM(); ca != nil {
			o.Secrets[RefCacheCA] = ca
		}
	}
	for _, c := range Consumers() {
		k := r.Top.Canary()
		r.APIKeys[c] = k
		o.Secrets[ConsumerKeyRef(c)] = []byte(k)
	}
	if r.H.place != nil {
		o.GOMAXPROCS = TargetGOMAXPROCS
	}
	if r.Spec.Node != nil {
		r.Spec.Node(r, &o)
	}
	return o, nil
}

// placeStore returns the options of a State Store on the placement's State
// Store host at the target profile, else o.
func (h *Harness) placeStore(o topology.StateStoreOptions) topology.StateStoreOptions {
	if h.place != nil {
		o.Host = h.place.StateStoreHost()
	}
	return o
}

// EnvZone is the Node setting naming its failure zone.
const EnvZone = "RURALZ_ZONE"

// addNodes starts the run's Nodes on o: all at once, or zone by zone
// with Spec.Zones, every Node then on GOMAXPROCS max(1, CPUs / Nodes), as
// the harness gives Nodes added together, or at the target profile on
// TargetGOMAXPROCS.
func (r *Run) addNodes(ctx context.Context, o topology.NodeOptions) error {
	if len(r.Spec.Zones) == 0 {
		_, err := r.Top.AddNodes(ctx, r.Nodes, o)
		return err
	}
	if o.GOMAXPROCS == 0 {
		o.GOMAXPROCS = max(1, runtime.NumCPU()/r.Nodes)
	}
	r.zones = map[string]string{}
	for i, count := range ZoneSplit(r.Nodes, len(r.Spec.Zones)) {
		if count == 0 {
			continue
		}
		zo := o
		zo.Env = append(slices.Clone(o.Env), EnvZone+"="+r.Spec.Zones[i])
		nodes, err := r.Top.AddNodes(ctx, count, zo)
		if err != nil {
			return err
		}
		for _, n := range nodes {
			r.zones[n.Name()] = r.Spec.Zones[i]
		}
	}
	return nil
}

// Zone returns the failure zone of the Node named node, "" without
// Spec.Zones.
func (r *Run) Zone(node string) string { return r.zones[node] }

// startBalancer starts the L4 balancer over a listener, redispatching a
// failed dial to another healthy Node as an operator's balancer does,
// and waits until every Node is healthy behind it. At the target profile
// it listens on the gateway host's address, which the load host reaches.
func (r *Run) startBalancer(listener string) *topology.Balancer {
	r.T.Helper()
	c := l4lb.Config{Redispatch: true}
	if r.H.place != nil {
		c.Listen = r.H.place.BalancerListen()
	}
	if r.Spec.Balancer != nil {
		r.Spec.Balancer(r, listener, &c)
	}
	b, err := r.Top.StartBalancer(r.ctx, listener, topology.BalancerOptions{Config: c})
	if err != nil {
		r.T.Fatal(err)
	}
	if err := b.WaitHealthy(r.ctx); err != nil {
		r.T.Fatal(err)
	}
	return b
}

// UnreadyTransitions counts the transitions of every balancer's backends
// to unhealthy in [from, to): the times a Node's /readyz failed the
// balancer's probes long enough to take it out of rotation.
func (r *Run) UnreadyTransitions(from, to time.Time) int {
	n := 0
	for _, b := range []*topology.Balancer{r.HTTP, r.HTTPS} {
		if b == nil {
			continue
		}
		for _, tr := range b.Transitions() {
			if tr.To == l4lb.Unhealthy && !tr.At.Before(from) && tr.At.Before(to) {
				n++
			}
		}
	}
	return n
}

// scrapeTargets returns every Node's /metrics.
func (r *Run) scrapeTargets() []ScrapeTarget {
	var out []ScrapeTarget
	for _, n := range r.Top.Nodes() {
		if n.Process() == nil {
			continue
		}
		out = append(out, ScrapeTarget{Name: n.Name(), URL: n.AdminURL() + "/metrics", Token: n.MetricsToken()})
	}
	return out
}

// nodeNow returns the Nodes' clock reading, or the real time without a
// controllable clock.
func (r *Run) nodeNow() time.Time {
	if r.TestClock {
		return r.Top.Now()
	}
	return time.Now()
}

// stopBackground stops the sampler and the pump before the topology's
// teardown stops the Nodes.
func (r *Run) stopBackground() {
	r.Sampler.Stop()
	if r.Pump != nil {
		if err := r.Pump.Stop(); err != nil {
			r.T.Errorf("%v", err)
		}
	}
}

// Context returns the run's context, which ends with its test.
func (r *Run) Context() context.Context { return r.ctx }

// NodeTime returns the Nodes' clock reading at real instant t: the Node
// time the pump had set by then, or t itself without a controllable
// clock.
func (r *Run) NodeTime(t time.Time) time.Time {
	if r.Pump == nil {
		return t
	}
	n, _, err := r.Pump.NodeTime(t)
	if err != nil {
		return t
	}
	return n
}

// NodeSpan returns the Node times a Node may have read at real instant t
// (Pump.NodeSpan), t itself twice without a controllable clock.
func (r *Run) NodeSpan(t time.Time) (lo, hi time.Time) {
	if r.Pump == nil {
		return t, t
	}
	lo, hi, err := r.Pump.NodeSpan(t)
	if err != nil {
		return t, t
	}
	return lo, hi
}

// CauseSpan returns the Node times at which a Node timer may have fired
// to cause an event seen at real instant t (Pump.CauseSpan), t itself
// twice without a controllable clock.
func (r *Run) CauseSpan(t time.Time) (lo, hi time.Time) {
	if r.Pump == nil {
		return t, t
	}
	lo, hi, err := r.Pump.CauseSpan(t)
	if err != nil {
		return t, t
	}
	return lo, hi
}

// A walk under load moves the Nodes' clock WalkStep at a time, each step
// followed by WalkStepWait of real time, which the pump adds, so Node
// time runs at most six times as fast as real time: a deadline of a
// request in flight longer than a step, such as the Upstream's 1 s
// connectTimeout, which a successor's first dials meet, leaves it at
// least 150 ms of real time
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing").
// The harness's 1 s Drain steps would end every dial in flight at a step.
const (
	WalkStep     = 250 * time.Millisecond
	WalkStepWait = topology.DrainStepWait
)

// WalkUntil waits until done reports true or maxWalk has passed, moving
// the Nodes' controllable clock meanwhile a WalkStep at a time with
// WalkStepWait of real time after each (Pump.Walk); without a
// controllable clock it polls done for maxWalk of real time. It reports
// whether done held.
func (r *Run) WalkUntil(maxWalk time.Duration, done func() bool) bool {
	r.T.Helper()
	if r.Pump != nil {
		ok, err := r.Pump.Walk(r.ctx, WalkStep, WalkStepWait, maxWalk, done)
		if err != nil {
			r.T.Fatal(err)
		}
		return ok
	}
	deadline := time.Now().Add(maxWalk)
	for !done() {
		if !time.Now().Before(deadline) {
			return false
		}
		select {
		case <-r.ctx.Done():
			r.T.Fatal(r.ctx.Err())
		case <-time.After(topology.PollInterval):
		}
	}
	return true
}

// StartLoad starts the run's load and records its interval logs: on the
// harness's host, or at the target profile on the load host, which keeps
// every request's record until the run's load has ended (Load.Wait).
func (r *Run) StartLoad(spec LoadSpec) *Load {
	r.T.Helper()
	var l *Load
	var err error
	if r.H.place != nil {
		l, err = r.startRemoteLoad(spec)
	} else {
		httpsAddr := ""
		if r.HTTPS != nil {
			httpsAddr = r.HTTPS.Addr()
		}
		l, err = startLoad(r.ctx, spec, r.HTTP.Addr(), httpsAddr, r.Top.ClientTLS(), r.APIKeys, r.attribute)
	}
	if err != nil {
		r.T.Fatal(err)
	}
	r.mu.Lock()
	r.loads = append(r.loads, l)
	for _, ss := range spec.Streams {
		r.rec.Breakdowns["offered_rps_"+ss.Name] = ss.Rate
	}
	r.mu.Unlock()
	// A run that ended without its Wait, as a failed one, leaves the load
	// host's records there.
	r.T.Cleanup(func() {
		l.Stop()
		_ = l.wait(false)
	})
	return l
}

// attribute returns the Node the balancer attributes a client address to.
func (r *Run) attribute(https bool, local string) string {
	b := r.HTTP
	if https {
		b = r.HTTPS
	}
	if b == nil {
		return ""
	}
	name, ok := b.BackendOf(local)
	if !ok {
		return ""
	}
	return name
}

// ValidityKind returns the kind of the run's timeline validity rules and
// recovery rules: Functional, or a measurement run's MeasureKind.
func (r *Run) ValidityKind() Kind {
	switch {
	case !r.Spec.Measurement:
		return Functional
	case r.Spec.MeasureKind != 0:
		return r.Spec.MeasureKind
	default:
		return Latency
	}
}

// Begin starts the run's timeline at the load's measured phase: it waits
// for the warm-up to end, records how long it lasted and, for a settling
// load, whether it settled, a validity rule of the run's kind.
func (r *Run) Begin(l *Load) {
	r.T.Helper()
	start, err := l.WaitMeasuring(r.ctx)
	if err != nil {
		r.T.Fatal(err)
	}
	if err := l.WaitUntil(r.ctx, start); err != nil {
		r.T.Fatal(err)
	}
	r.Breakdown("warmup_s", l.Warmup().Seconds())
	if l.settle {
		r.Add(Holds("warm-up settled", r.ValidityKind(), l.Settled()))
	}
	r.mu.Lock()
	r.cursor = start
	r.mu.Unlock()
}

// Baseline adds the timeline's validity rules over the baseline phase p
// of l, of the run's validity kind (BaselineCriteria).
func (r *Run) Baseline(l *Load, p Phase) {
	r.Add(BaselineCriteria(l, p, r.ValidityKind())...)
}

// Hold runs the phase name for d of the load's schedule from the end of
// the previous phase, waiting until the load has scheduled every request
// due by its end, and records it.
func (r *Run) Hold(l *Load, name string, d time.Duration) Phase {
	r.T.Helper()
	r.mu.Lock()
	start := r.cursor
	r.mu.Unlock()
	if start.IsZero() {
		r.T.Fatal("chaos: Hold before Begin")
	}
	end := start.Add(d)
	if err := l.WaitUntil(r.ctx, end); err != nil {
		r.T.Fatal(err)
	}
	p := Phase{Name: name, Start: start, End: end}
	r.mu.Lock()
	r.rec.Phases = append(r.rec.Phases, p)
	r.cursor = end
	r.mu.Unlock()
	r.captureMetrics()
	return p
}

// Mark records a phase that ended now, for a timeline that follows
// events rather than the load's schedule.
func (r *Run) Mark(name string, start time.Time) Phase {
	p := Phase{Name: name, Start: start, End: time.Now()}
	r.mu.Lock()
	r.rec.Phases = append(r.rec.Phases, p)
	r.cursor = p.End
	r.mu.Unlock()
	r.captureMetrics()
	return p
}

// captureTimeout bounds one /metrics fetch of captureMetrics.
const captureTimeout = 5 * time.Second

// captureMetrics fetches every running Node's /metrics once, together,
// through the harness, which keeps the bodies for the leak scan: one
// exposition per Node and phase, beside the Sampler's periodic scrapes,
// which bypass the capture store
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing").
// A Node that stopped answering, as a killed one, is skipped.
func (r *Run) captureMetrics() {
	var wg sync.WaitGroup
	for _, n := range r.Top.Nodes() {
		if p := n.Process(); p == nil || p.State() != nil {
			continue
		}
		wg.Go(func() {
			ctx, cancel := context.WithTimeout(r.ctx, captureTimeout)
			defer cancel()
			_, _ = n.AdminAs(ctx, "/metrics", topology.Metrics)
		})
	}
	wg.Wait()
}

// Inject runs fn, the fault kind on target, and records it at the time
// fn started; a failing fn fails the run.
func (r *Run) Inject(kind, target string, fn func() error) time.Time {
	r.T.Helper()
	at := time.Now()
	if err := fn(); err != nil {
		r.T.Fatalf("chaos: inject %s on %s: %v", kind, target, err)
	}
	r.mu.Lock()
	r.rec.Injections = append(r.rec.Injections, Injection{Kind: kind, Target: target, Time: at})
	r.mu.Unlock()
	return at
}

// Add records criteria.
func (r *Run) Add(cs ...Criterion) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Criteria = append(r.rec.Criteria, cs...)
}

// Breakdown records an ungated value.
func (r *Run) Breakdown(name string, v float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec.Breakdowns[name] = v
}

// Attribute records failed requests per Node.
func (r *Run) Attribute(counts map[string]int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rec.Attribution == nil {
		r.rec.Attribution = map[string]int64{}
	}
	for k, v := range counts {
		r.rec.Attribution[k] += v
	}
}

// IntervalLog adds a stream's interval log beside the record.
func (r *Run) IntervalLog(stream string, l *Stream) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.logs = append(r.logs, IntervalLog{Stream: stream, Result: l.Result()})
}

// Record returns a copy of the record so far.
func (r *Run) Record() Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.rec
	out.Phases = append([]Phase(nil), r.rec.Phases...)
	out.Injections = append([]Injection(nil), r.rec.Injections...)
	out.Criteria = append([]Criterion(nil), r.rec.Criteria...)
	return out
}

// Finish ends the run: it stops the sampler and the pump, logs every
// criterion, fails the test for each failed criterion the profile gates,
// and writes the record and its interval logs into the records
// directory.
func (r *Run) Finish() {
	r.T.Helper()
	r.stopBackground()
	r.counterBreakdowns()
	r.attributeTimeline()
	rec := r.Record()
	for _, c := range rec.Criteria {
		gated := ""
		if r.H.Profile.Gates(c.kind) {
			gated = ", gated"
		}
		r.T.Logf("chaos: %s: %s = %s, bound %s: %s (%s%s)", rec.Experiment, c.Name, formatNumber(c.Value), c.Bound, c.Verdict, c.kind, gated)
	}
	for _, c := range rec.Failed(r.H.Profile) {
		r.T.Errorf("chaos: %s failed %s: %s, bound %s", rec.Experiment, c.Name, formatNumber(c.Value), c.Bound)
	}
	if r.H.Records == "" {
		r.T.Logf("chaos: %s is unset: no record written", EnvRecords)
		return
	}
	path, err := rec.Write(r.H.Records)
	if err != nil {
		r.T.Errorf("%v", err)
		return
	}
	r.mu.Lock()
	logs := append([]IntervalLog(nil), r.logs...)
	r.mu.Unlock()
	if _, err := rec.WriteIntervalLogs(r.H.Records, logs); err != nil {
		r.T.Errorf("%v", err)
	}
	r.T.Logf("chaos: record %s", path)
}

// attributeTimeline records, unless the experiment recorded its own, the
// failed requests per Node of every load over the run's timeline, from
// its first phase's start to its last phase's end, as the L4 balancer
// attributed their connections.
func (r *Run) attributeTimeline() {
	r.mu.Lock()
	own := r.rec.Attribution != nil
	phases := slices.Clone(r.rec.Phases)
	loads := slices.Clone(r.loads)
	r.mu.Unlock()
	if own || len(phases) == 0 {
		return
	}
	counts := map[string]int64{}
	for _, l := range loads {
		for n, c := range Attribution(l, phases[0].Start, phases[len(phases)-1].End) {
			counts[n] += c
		}
	}
	r.Attribute(counts)
}

// breakdownCounters are the counters every record breaks down over the
// whole run, summed over the Nodes, by the labels listed.
func breakdownCounters() map[string][]string {
	return map[string][]string{
		catalog.StateCallsTotal:         {"op", "result"},
		catalog.StateWritesDroppedTotal: {"kind"},
		catalog.RateLimitDecisionsTotal: {"result"},
		catalog.QuotaDecisionsTotal:     {"result"},
		catalog.FilterFailuresTotal:     {"mode"},
		catalog.UpstreamRetriesTotal:    nil,
		catalog.UpstreamEjectionsTotal:  {"reason"},
	}
}

// counterBreakdowns records the increase of every breakdown counter over
// the run as ungated values.
func (r *Run) counterBreakdowns() {
	far := time.Now().Add(time.Hour)
	for name, by := range breakdownCounters() {
		for k, v := range r.Sampler.Totals(name, time.Time{}, far, by...) {
			r.Breakdown(k, v)
		}
	}
	if r.Pump != nil {
		r.Breakdown("clock_pump_max_behind_ms", float64(r.Pump.MaxBehind())/float64(time.Millisecond))
	}
}

// TopologyName returns the stateStore.topology value of t.
func TopologyName(t statestore.Topology) string {
	switch t {
	case statestore.TopologyCluster:
		return "cluster"
	case statestore.TopologySentinel:
		return "sentinel"
	default:
		return "standalone"
	}
}

// Primaries returns the main State Store's primaries.
func (r *Run) Primaries() []*redisserver.Server { return r.Store.Deployment().Nodes() }
