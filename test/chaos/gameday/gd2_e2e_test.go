// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/l4lb"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// GD-2's runs: the functional runs cut zone a by SIGSTOP and by black-hole;
// the measurement run repeats the SIGSTOP on the release build for the
// CPU criteria (docs/engineering/03-testing-and-quality-strategy.md
// "Hermetic rule scope").
const (
	gd2Stop      = "sigstop"
	gd2Blackhole = "blackhole"
	gd2CPU       = "cpu"
)

// gd2Lost is the zone GD-2 loses: it holds the State Store primary; zone b
// holds its replica and zone c its sentinel.
const gd2Lost = "a"

// gd2Zones are GD-2's zones in the order chaos.Spec.Zones splits the Nodes
// over them, the earlier ones taking the remainder: the lost zone last, so
// it holds the fewest Nodes, three of ten, and its loss leaves the
// survivors at about 76% of their GOMAXPROCS from 53%, under the 80% bound
// (docs/engineering/03-testing-and-quality-strategy.md "Drills and failure
// rows").
func gd2Zones() []string { return []string{"b", "c", gd2Lost} }

// GD-2's measurement windows: the calibration load's warm-up and measured
// span, and the wait after the zone's loss before the survivors' CPU is
// measured, past the load generator's request timeout, after which every
// request lane once on the lost zone's connections dials a survivor.
const (
	gd2CalWarmup = 10 * time.Second
	gd2CalSpan   = 20 * time.Second
	gd2Moved     = loadgen.DefaultTimeout + 2*time.Second
)

// TestGD2ZoneLoss is GD-2, zone loss, and the failure catalog row FC-3 on
// its runs (docs/operations/04-high-availability-and-disaster-recovery.md
// "Game days" and "Failure catalog"): the Nodes split 3, 4 and 3 over
// zones a, b and c on a sentinel State Store whose primary sits in zone
// a, its replica in b and its sentinel in c, at 50% load; after the
// baseline zone a is lost. The sigstop run SIGSTOPs zone a's Nodes and
// primary; the blackhole run black-holes every path the harness proxies
// into zone a: the balancer's connections and readiness probes to its
// Nodes and their State Store connections, through fault proxies, and
// SIGSTOPs the primary, which the sentinel names to the Nodes and which
// its replica and sentinel reach directly. The surviving Nodes decide by
// failureMode for 15 s or less while the sentinel fails over, and, for
// FC-3, the failover promotes zone b's replica, the survivors answer by
// GCRA from it, and the balancer's health checks take zone a's Nodes out.
// The cpu run, on the release build, first calibrates the load so each
// Node runs at 53% of its GOMAXPROCS, the three-zone scale-out threshold,
// then measures the survivors' CPU share once the lost zone's load has
// moved to them: under 80% (target), gating at the target profile.
func TestGD2ZoneLoss(t *testing.T) {
	t.Run(gd2Stop, func(t *testing.T) { gd2(t, gd2Stop) })
	t.Run(gd2Blackhole, func(t *testing.T) { gd2(t, gd2Blackhole) })
	t.Run(gd2CPU, func(t *testing.T) { gd2(t, gd2CPU) })
}

// gd2 runs one of GD-2's runs.
func gd2(t *testing.T, variant string) {
	s := saturation(t)
	nodes := suite.Profile.Nodes
	rate := suite.Profile.LoadShare * s
	plan := chaos.PlanLimits(rate, nodes)
	measure := variant == gd2CPU
	spec := chaos.Spec{
		Name: GD2 + "/" + variant, Zones: gd2Zones(), Bundle: plan.Apply,
		StateStore:  topology.StateStoreOptions{Kind: redisserver.ProcessSentinel},
		SampleEvery: 100 * time.Millisecond,
	}
	if measure {
		spec.Measurement, spec.MeasureKind, spec.SampleEvery = true, chaos.CPU, 0
	}
	cut := &gd2Cut{}
	if variant == gd2Blackhole {
		spec.Balancer = cut.balancer
	}
	r := suite.Start(t, spec)
	ctx := r.Context()
	var lost, survivors []string
	for _, n := range r.NodeNames() {
		if r.Zone(n) == gd2Lost {
			lost = append(lost, n)
		} else {
			survivors = append(survivors, n)
		}
	}
	if measure {
		share := gd2Calibrate(ctx, t, r, plan.Streams())
		calibrated := RateForShare(rate, share, ZoneScaleOut)
		r.Breakdown("calibration_cpu_share", share)
		r.Breakdown("calibrated_rps", calibrated)
		plan = chaos.PlanLimits(calibrated, nodes)
	}
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + chaos.FaultPhase + chaos.RecoveryTime, Streams: plan.Streams()})
	r.Begin(l)
	pre := gd2Usage(t, r)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	if measure {
		shares := gd2Shares(t, r, "baseline", pre, gd2Usage(t, r), r.NodeNames())
		r.Breakdown("baseline_cpu_share_mean", gd2Mean(shares))
		r.Breakdown("baseline_cpu_share_max", slices.Max(shares))
	}
	dep := r.Store.Deployment()
	primary, replica := dep.Primary(), dep.Replica()
	start := r.Inject(variant, "zone-"+gd2Lost, func() error {
		if variant == gd2Blackhole {
			if err := cut.set(r, lost, faultproxy.Blackhole{}); err != nil { //nolint:contextcheck // A proxy changes mode on its own connections, at once.
				return err
			}
		} else if err := gd2Signal(ctx, r, lost, syscall.SIGSTOP); err != nil {
			return err
		}
		return primary.Signal(syscall.SIGSTOP)
	})
	r.Inject("await-failover", primary.Name(), func() error { return dep.AwaitFailover(ctx) })
	failedOver := time.Now()
	promoted := gd2Master(ctx, t, dep) == replica.Addr()
	var moved []float64
	if measure {
		r.Hold(l, chaos.PhaseFault, gd2Moved)
		at := gd2Usage(t, r)
		r.Hold(l, "fault-measured", chaos.FaultPhase-gd2Moved)
		moved = gd2Shares(t, r, "moved", at, gd2Usage(t, r), survivors)
	} else {
		r.Hold(l, chaos.PhaseFault, chaos.FaultPhase)
	}
	end := r.Inject("heal", "zone-"+gd2Lost, func() error {
		if err := primary.Signal(syscall.SIGCONT); err != nil {
			return err
		}
		if variant == gd2Blackhole {
			return cut.set(r, lost, faultproxy.Pass{}) //nolint:contextcheck // A proxy changes mode on its own connections, at once.
		}
		return gd2Signal(ctx, r, lost, syscall.SIGCONT)
	})
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	if measure {
		r.Add(AlsoFor(FC3, chaos.Check("surviving Nodes' highest CPU share once the zone's load moved to them", chaos.CPU, slices.Max(moved), chaos.LT, SurvivorCPU))...)
		r.RecoveryCriteria(l, base, rec, end)
		r.Finish()
		return
	}
	// failureMode: each survivor's first to last fail-open or fail-closed
	// decision after the loss, as CE-5 measures it.
	worst := time.Duration(0)
	for _, n := range survivors {
		var rises []chaos.Point
		for _, fam := range []string{catalog.RateLimitDecisionsTotal, catalog.QuotaDecisionsTotal} {
			for _, res := range []string{"fail_open", "fail_closed"} {
				rises = append(rises, chaos.Rises(chaos.Between(r.Sampler.Trace(n, fam, promtext.L("result", res)), start, rec.End))...)
			}
		}
		worst = max(worst, ModeSpan(rises))
	}
	node := nodeOf(r)
	answered := FirstGCRA(l.StreamNamed("closed").Requests(), node, failedOver)
	served := 0
	for _, n := range survivors {
		if _, ok := answered[n]; ok {
			served++
		}
	}
	outOfRotation := 0
	for _, n := range lost {
		for _, tr := range r.HTTP.Transitions() {
			if tr.Backend == n && tr.To == l4lb.Unhealthy && !tr.At.Before(start) && tr.At.Before(end) {
				outOfRotation++
				break
			}
		}
	}
	r.Breakdown("failover_after_loss_s", failedOver.Sub(start).Seconds())
	r.Add(
		chaos.Check("longest State Store failureMode of a surviving Node, seconds", chaos.Functional, worst.Seconds(), chaos.LE, FailureModeBound.Seconds()),
		chaos.Holds(FC3+": the State Store failed over to zone b's replica", chaos.Functional, promoted),
		chaos.Check(FC3+": surviving Nodes answering by GCRA after the failover", chaos.Functional, float64(served), chaos.EQ, float64(len(survivors))),
		chaos.Check(FC3+": lost-zone Nodes the balancer's health checks took out", chaos.Functional, float64(outOfRotation), chaos.EQ, float64(len(lost))),
	)
	r.Attribute(chaos.Attribution(l, start, rec.End))
	r.RecoveryCriteria(l, base, rec, end)
	r.Finish()
}

// gd2Signal sends sig to the named Nodes through the harness, which holds
// a stopped Node out of the clock's advances and catches it up on SIGCONT.
func gd2Signal(ctx context.Context, r *chaos.Run, names []string, sig os.Signal) error {
	for _, n := range r.Top.Nodes() {
		if slices.Contains(names, n.Name()) {
			if err := n.Signal(ctx, sig); err != nil {
				return fmt.Errorf("gameday: %s: %w", n.Name(), err)
			}
		}
	}
	return nil
}

// gd2Master returns the address the sentinel names as the primary.
func gd2Master(ctx context.Context, t *testing.T, dep *redisserver.Deployment) string {
	t.Helper()
	v, err := dep.Sentinels()[0].Do(ctx, "SENTINEL", "get-master-addr-by-name", dep.MasterSet())
	if err != nil || len(v.Elems) != 2 {
		t.Fatalf("gameday: the sentinel's primary: %v %v", v, err)
	}
	return net.JoinHostPort(v.Elems[0].Str, v.Elems[1].Str)
}

// gd2Cut routes the balancer's connections and readiness probes to the
// Nodes through fault proxies of their own, so the blackhole run cuts
// every path the balancer has into the lost zone; the other Nodes' paths
// stay direct.
type gd2Cut struct {
	// via maps a Node's listener and admin addresses to its proxies;
	// filled before the balancer starts and only read after.
	via     map[string]*faultproxy.Proxy
	proxies map[string][]*faultproxy.Proxy // by Node
}

// balancer is the run's Spec.Balancer: it starts the lost zone's proxies
// and dials through them.
func (c *gd2Cut) balancer(r *chaos.Run, listener string, cfg *l4lb.Config) {
	r.T.Helper()
	c.via, c.proxies = map[string]*faultproxy.Proxy{}, map[string][]*faultproxy.Proxy{}
	for _, n := range r.Top.Nodes() {
		if r.Zone(n.Name()) != gd2Lost {
			continue
		}
		u, err := url.Parse(n.AdminURL())
		if err != nil {
			r.T.Fatal(err)
		}
		for _, addr := range []string{n.Addr(listener), u.Host} {
			p, err := faultproxy.Start(r.Context(), faultproxy.Config{Upstream: addr})
			if err != nil {
				r.T.Fatal(err)
			}
			c.via[addr] = p
			c.proxies[n.Name()] = append(c.proxies[n.Name()], p)
		}
	}
	cfg.Dial, cfg.ProbeDial = c.dial, c.dial
}

// dial dials addr, through its proxy when it has one.
func (c *gd2Cut) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if p, ok := c.via[addr]; ok {
		addr = p.Addr()
	}
	return (&net.Dialer{}).DialContext(ctx, network, addr)
}

// set puts every proxy into the named Nodes, the balancer's and their
// State Store connections', in mode m.
func (c *gd2Cut) set(r *chaos.Run, names []string, m faultproxy.Mode) error {
	for _, n := range r.Top.Nodes() {
		if !slices.Contains(names, n.Name()) {
			continue
		}
		ps, _ := n.StateStoreProxy("main")
		for _, p := range append(slices.Clone(c.proxies[n.Name()]), ps...) {
			if err := p.SetMode(m); err != nil {
				return fmt.Errorf("gameday: %s: %w", n.Name(), err)
			}
		}
	}
	return nil
}

// gd2Usage reads every Node process's CPU usage.
func gd2Usage(t *testing.T, r *chaos.Run) map[string]proc.Usage {
	t.Helper()
	out := map[string]proc.Usage{}
	for _, n := range r.Top.Nodes() {
		p := n.Process()
		if p == nil {
			continue
		}
		u, err := p.Sample()
		if err != nil {
			t.Logf("gameday: %s: CPU sample: %v", n.Name(), err)
			continue
		}
		out[n.Name()] = u
	}
	return out
}

// gd2Shares returns the CPU share of their GOMAXPROCS the named Nodes used
// between two readings, and records each as a breakdown under label.
func gd2Shares(t *testing.T, r *chaos.Run, label string, from, to map[string]proc.Usage, names []string) []float64 {
	t.Helper()
	var out []float64
	for _, n := range r.Top.Nodes() {
		a, okA := from[n.Name()]
		b, okB := to[n.Name()]
		if !slices.Contains(names, n.Name()) || !okA || !okB {
			continue
		}
		share := proc.CPUShare(a, b) / float64(gd2Procs(r, n))
		r.Breakdown("cpu_share_"+label+"_"+n.Name(), share)
		out = append(out, share)
	}
	if len(out) == 0 {
		t.Fatal("gameday: no CPU reading of any Node")
	}
	return out
}

// gd2Procs returns a Node process's GOMAXPROCS, from its environment, else
// the harness's rule for Nodes split over zones, max(1, CPUs / Nodes).
func gd2Procs(r *chaos.Run, n *topology.Node) int {
	if p := n.Process(); p != nil {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID()) + "/environ")
		if err == nil {
			if v, ok := GOMAXPROCSOf(b); ok {
				return v
			}
		}
	}
	return max(1, runtime.NumCPU()/r.Nodes)
}

// gd2Calibrate offers streams for a short warm-up and a measured span and
// returns the Nodes' mean CPU share of their GOMAXPROCS over that span.
func gd2Calibrate(ctx context.Context, t *testing.T, r *chaos.Run, streams []chaos.StreamSpec) float64 {
	t.Helper()
	cal := r.StartLoad(chaos.LoadSpec{Warmup: gd2CalWarmup, Duration: gd2CalSpan, Streams: streams})
	from, err := cal.WaitMeasuring(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := cal.WaitUntil(ctx, from); err != nil {
		t.Fatal(err)
	}
	a := gd2Usage(t, r)
	if err := cal.WaitUntil(ctx, from.Add(gd2CalSpan)); err != nil {
		t.Fatal(err)
	}
	b := gd2Usage(t, r)
	cal.Stop()
	if err := cal.Wait(); err != nil {
		t.Fatal(err)
	}
	return gd2Mean(gd2Shares(t, r, "calibration", a, b, r.NodeNames()))
}

// gd2Mean returns the mean of xs, 0 for none.
func gd2Mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	total := 0.0
	for _, x := range xs {
		total += x
	}
	return total / float64(len(xs))
}
