// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package gameday

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
	"github.com/ravindu-rev/ruralz/test/chaos"
)

// FC-19's run name, names and procedure values.
const (
	fc19Name = "FC-19"
	// fc19DNSAddr is the harness DNS server's address in the drill's
	// network namespace: a resolv.conf nameserver takes no port, so 53.
	fc19DNSAddr = "127.0.0.1:53"
	// fc19ResolvConf names that server alone, one attempt of 1 s per
	// question, so a failing server fails a lookup at once.
	fc19ResolvConf = "nameserver 127.0.0.1\noptions timeout:1 attempts:1\n"
	// fc19StateHost is the State Store host the drill moves every Node's
	// URL to, which the server resolves to loopback.
	fc19StateHost = "state.chaos.test"
	// fc19EmptySet is the code of a request to an Upstream whose Endpoint
	// set is empty.
	fc19EmptySet = "RZ-UP-008"
	// fc19DiscoveryShare is each discovery Route's share of the load; the
	// limit Routes of chaos.PlanLimits take the rest.
	fc19DiscoveryShare = 0.15
	// fc19LongStreak is a failure streak past which every backoff ceiling
	// is the 60 s cap: the bound of refreshes whose streak is unknown.
	fc19LongStreak = 6
	// fc19PollEvery spaces the /debug/upstreams readings of real-time
	// waits; a walk reads once per step. fc19PollTimeout bounds one.
	fc19PollEvery   = 100 * time.Millisecond
	fc19PollTimeout = time.Second
	// fc19Slack is added to every walk's bound for the lookups and the
	// readings themselves.
	fc19Slack = 10 * time.Second
	// fc19EmptyHold keeps the sets empty that long, so requests meet them.
	fc19EmptyHold = 10 * time.Second
	// fc19FaultBudget bounds the load from the baseline's end to the
	// recovery: both sub-phases' walks, the hold and the wait for the
	// sets to come back.
	fc19FaultBudget = 6 * time.Minute
	// fc19MaxObservations bounds the readings kept per Node and Upstream,
	// fc19MaxBody one /debug/upstreams answer.
	fc19MaxObservations = 1 << 14
	fc19MaxBody         = 1 << 20
)

// fc19SRVName is the SRV name of discovered-srv's named port.
func fc19SRVName() string { return "_" + chaos.DiscoverySRVPort + "._tcp." + chaos.DiscoverySRVName }

// fc19Targets are the targets of discovered-srv's SRV records, one per
// mock Upstream, each resolving to loopback.
func fc19Targets() []string {
	return []string{"b1.discovery.chaos.test", "b2.discovery.chaos.test", "b3.discovery.chaos.test"}
}

// fc19Discovered are the chaos Bundle's dns discovery Upstreams.
func fc19Discovered() []string {
	return []string{chaos.UpstreamDiscoveredA, chaos.UpstreamDiscoveredSRV}
}

// TestFC19DNSFailure is the failure catalog row FC-19, DNS failure, as a
// drill of its own (docs/engineering/03-testing-and-quality-strategy.md
// "Drills and failure rows"; docs/operations/04-high-availability-and-disaster-recovery.md
// "Failure catalog"). The harness DNS server answers for the dns
// discovery Upstreams discovered-a (A records) and discovered-srv (SRV
// records) and for the State Store host, and ruralzd runs in a network
// and mount namespace whose /etc/resolv.conf names it, at 50% load. The
// server fails (SERVFAIL), with every Node's State Store connections
// reset so they must dial again, then answers NXDOMAIN for discovered-a's
// name and the State Store host and empty answers for discovered-srv's,
// then answers again. Each Upstream keeps its last set through failed
// refreshes and raises discovery_stale; NXDOMAIN and the empty answer
// empty a set only once 3 refreshes repeat them (target), requests then
// getting 503 RZ-UP-008; the failed State Store dials open the breakers
// and apply failureMode; once the server answers, the sets come back and
// the breakers close. Linux only, with the privilege to create the
// namespaces, as root or in a user namespace; skipped with the reason
// otherwise.
func TestFC19DNSFailure(t *testing.T) {
	if proc.NetNSChild(t) {
		fc19(t)
		return
	}
	if !suite.Profile.Runs(fc19Name) {
		t.Skipf("chaos: the %s profile does not run %s", suite.Profile.Name, fc19Name)
	}
	if _, err := os.Stat(proc.ResolvConfPath); err != nil {
		t.Skipf("gameday: FC-19 bind-mounts its resolver configuration over %s, which this host lacks: %v", proc.ResolvConfPath, err)
	}
	saturation(t)
	conf := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(conf, []byte(fc19ResolvConf), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := proc.RunInNetNS(t.Context(), proc.NetNSOptions{
		TestName:   t.Name(),
		ResolvConf: conf,
		User:       os.Geteuid() != 0,
		Env:        childEnv(),
		Output:     testWriter{t},
	})
	switch {
	case errors.Is(err, proc.ErrChildSkipped):
		t.Skip(err)
	case errors.Is(err, proc.ErrNamespace), errors.Is(err, errors.ErrUnsupported):
		t.Skipf("gameday: FC-19 runs ruralzd in a network and mount namespace whose %s names the harness DNS server, which this process cannot create: %v", proc.ResolvConfPath, err)
	case err != nil:
		t.Fatal(err)
	}
}

// fc19 runs FC-19 inside its namespaces.
func fc19(t *testing.T) {
	s := saturation(t)
	ctx := t.Context()
	dns, err := StartDNSServer(ctx, fc19DNSAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dns.Close() })
	rate := suite.Profile.LoadShare * s
	plan := chaos.PlanLimits(rate*(1-2*fc19DiscoveryShare), suite.Profile.Nodes)
	r := suite.Start(t, chaos.Spec{
		Name: fc19Name,
		// A State Store behind the counting RESP proxy runs without TLS, so
		// its URL may name a host its certificate would not hold.
		StateStore:  topology.StateStoreOptions{RESPProxy: true},
		SampleEvery: 250 * time.Millisecond,
		// The discovery Upstreams resolve when the Nodes compile their
		// first Revision, so the server holds their records before the
		// Nodes start; the mocks' ports are known once the harness filled
		// the backends.
		Bundle: func(b *chaos.BundleParams) {
			plan.Apply(b)
			b.Overlay = chaos.OverlayDiscovery
			ports, err := fc19Ports(b.Backends)
			if err != nil {
				t.Fatal(err)
			}
			b.DiscoveryPort = int(ports[0])
			if err := fc19Records(dns, ports); err != nil {
				t.Fatal(err)
			}
		},
	})
	nodes := r.NodeNames()
	track := newFC19Track(r)
	t.Cleanup(track.close)

	// Every Node resolved both discovery Upstreams at its compile: these
	// are the sets a failing server must leave in place.
	if !waitUntil(ctx, topology.DefaultReadyTimeout, func() bool {
		track.pollIfDue(ctx)
		return track.all(nodes, func(o DNSObservation) bool { return len(o.Endpoints) > 0 && !o.Stale })
	}) {
		t.Fatalf("gameday: not every Node resolved its discovery Upstreams: %v", track.latest(nodes))
	}
	want := track.sets(nodes)
	for _, n := range nodes {
		if a := want[fc19Key(n, chaos.UpstreamDiscoveredA)]; len(a) != 1 || a[0] != r.Params.Backends[0] {
			t.Fatalf("gameday: %s's %s = %v, want the one Endpoint %s", n, chaos.UpstreamDiscoveredA, a, r.Params.Backends[0])
		}
		if srv := want[fc19Key(n, chaos.UpstreamDiscoveredSRV)]; len(srv) != len(fc19Targets()) {
			t.Fatalf("gameday: %s's %s = %v, want one Endpoint per SRV record", n, chaos.UpstreamDiscoveredSRV, srv)
		}
	}

	// Every Node's State Store URL names the State Store host from now on:
	// a rotated file secret opens a new client, which dials by the name.
	accepted := fc19Accepted(r)
	if err := fc19RotateURLs(r); err != nil {
		t.Fatal(err)
	}
	if !waitUntil(ctx, topology.DefaultReadyTimeout, func() bool {
		now := fc19Accepted(r)
		for n, a := range accepted {
			if now[n] <= a {
				return false
			}
		}
		// Each Node's new client resolved the name when it dialed.
		return dns.Queries(fc19StateHost) >= int64(len(nodes))
	}) {
		t.Fatalf("gameday: not every Node opened a State Store client by the name %s (%d questions for it)", fc19StateHost, dns.Queries(fc19StateHost))
	}

	streams := append(plan.Streams(),
		chaos.StreamSpec{Name: "discovered-a", Path: chaos.PathDiscoveredA, Rate: rate * fc19DiscoveryShare, Counted: []string{fc19EmptySet}},
		chaos.StreamSpec{Name: "discovered-srv", Path: chaos.PathDiscoveredSRV, Rate: rate * fc19DiscoveryShare, Counted: []string{fc19EmptySet}},
	)
	l := r.StartLoad(chaos.LoadSpec{Duration: chaos.Baseline + fc19FaultBudget + chaos.RecoveryTime, Streams: streams})
	r.Begin(l)
	base := r.Hold(l, chaos.PhaseBaseline, chaos.Baseline)
	track.poll(ctx)

	// The server fails: every discovery refresh fails, and every State
	// Store dial with it once the reset drops the connections. Walk the
	// Nodes' clock until every Upstream has failed as many refreshes as
	// would empty a set on NXDOMAIN: from the 30 s schedule of a good
	// answer, then the backoff ceilings.
	failAt := r.Inject("servfail", "dns", func() error { return dns.SetModes(DNSFail, nil) })
	resetAt := r.Inject("reset", "state-store", func() error {
		if err := r.SetStoreMode("main", faultproxy.Reset{}); err != nil {
			return err
		}
		return r.SetStoreMode("main", faultproxy.Pass{})
	})
	failWalk := DNSRefreshesWithin(discovery.RefreshInterval+discovery.RefreshJitter, 0, discovery.NotFoundLimit) + fc19Slack
	if !r.WalkUntil(failWalk, func() bool {
		track.poll(ctx)
		return track.all(nodes, func(o DNSObservation) bool { return o.Stale && o.Failures >= discovery.NotFoundLimit })
	}) {
		t.Logf("gameday: not every discovery Upstream failed %d refreshes within %v: %v", discovery.NotFoundLimit, failWalk, track.latest(nodes))
	}
	fails := r.Mark("dns-fails", failAt)

	// NXDOMAIN for discovered-a's name and the State Store host, empty
	// answers for discovered-srv's: walk until every set emptied, at most
	// three refreshes on the capped backoff.
	nfAt := r.Inject("nxdomain-and-empty", "dns", func() error {
		return dns.SetModes(DNSAnswer, map[string]DNSMode{
			chaos.DiscoveryAName: DNSNXDomain,
			fc19SRVName():        DNSEmpty,
			fc19StateHost:        DNSNXDomain,
		})
	})
	nfWalk := DNSRefreshesWithin(discovery.BackoffMax, fc19LongStreak, discovery.NotFoundLimit) + fc19Slack
	if !r.WalkUntil(nfWalk, func() bool {
		track.poll(ctx)
		return track.all(nodes, func(o DNSObservation) bool { return len(o.Endpoints) == 0 })
	}) {
		t.Logf("gameday: not every discovery set emptied within %v: %v", nfWalk, track.latest(nodes))
	}
	r.Mark("dns-not-found", nfAt)
	held := r.Hold(l, "sets-empty", fc19EmptyHold)

	// The server answers again: an emptied set is looked up again on the
	// failure backoff, at most 60 s of Node time later.
	end := r.Inject("answer", "dns", func() error { return dns.SetModes(DNSAnswer, nil) })
	waitNodeTime(ctx, r, discovery.BackoffMax+fc19Slack, func() bool {
		track.pollIfDue(ctx)
		return track.back(nodes, want)
	})
	track.poll(ctx)
	r.Mark("sets-back", end)
	rec := r.Hold(l, chaos.PhaseRecovery, chaos.RecoveryTime)
	l.Stop()
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	logStreams(r, l)
	r.Baseline(l, base)
	fc19Criteria(t, r, l, track, want, fc19Times{fail: failAt, reset: resetAt, failEnd: fails.End, notFound: nfAt, heldEnd: held.End, end: end, rec: rec})
	r.RecoveryCriteria(l, base, rec, end)
	for name, q := range map[string]int64{
		"dns_questions_discovered_a":   dns.Queries(chaos.DiscoveryAName),
		"dns_questions_discovered_srv": dns.Queries(fc19SRVName()),
		"dns_questions_state_store":    dns.Queries(fc19StateHost),
		"dns_questions_other":          dns.OtherQueries(),
	} {
		r.Breakdown(name, float64(q))
	}
	r.Breakdown("debug_upstreams_failed_readings", float64(track.failures()))
	r.Breakdown("state_call_errors_while_unresolved", r.CallsIncrease(resetAt, end, "error"))
	r.Finish()
}

// fc19Times are the instants FC-19's criteria read: the server failing,
// the State Store connections reset, the end of the failing sub-phase,
// the switch to NXDOMAIN and empty answers, the end of the sets' hold,
// the server answering again and the recovery phase.
type fc19Times struct {
	fail, reset, failEnd, notFound, heldEnd, end time.Time
	rec                                          chaos.Phase
}

// fc19Criteria adds FC-19's Behavior and Detection criteria.
func fc19Criteria(t *testing.T, r *chaos.Run, l *chaos.Load, track *fc19Track, want map[string][]string, at fc19Times) {
	t.Helper()
	nodes := r.NodeNames()
	k := chaos.Functional
	stale := catalog.ReasonDiscoveryStale.String()
	byUpstream := map[string]*chaos.Stream{
		chaos.UpstreamDiscoveredA:   l.StreamNamed("discovered-a"),
		chaos.UpstreamDiscoveredSRV: l.StreamNamed("discovered-srv"),
	}
	node := nodeOf(r)

	// The server fails: every set kept through the failed refreshes, the
	// Node and the Upstream gauges raising discovery_stale, the discovery
	// Routes served.
	kept, gauges := 0, 0
	var failing int64
	for _, n := range nodes {
		allKept, allRaised := true, true
		for _, u := range fc19Discovered() {
			ok, streak, seen := DNSKept(track.series(n, u), want[fc19Key(n, u)], at.fail, at.failEnd)
			if !ok || !seen || streak < discovery.NotFoundLimit {
				t.Logf("gameday: %s's %s while the server failed: set kept %v, failed refreshes in a row %d, readings %v", n, u, ok, streak, seen)
				allKept = false
			}
			trace := r.Sampler.Trace(n, catalog.UpstreamDegradedInfo, promtext.L("upstream", u, "reason", stale))
			if p, ok := chaos.FirstAt(trace, at.fail, func(v float64) bool { return v > 0 }); !ok || !p.Real.Before(at.failEnd) {
				allRaised = false
			}
		}
		if allKept {
			kept++
		}
		if allRaised {
			gauges++
		}
	}
	for _, st := range byUpstream {
		failing += chaos.PhaseStats(st, at.fail, at.failEnd).Failed + chaos.CodesIn(st, at.fail, at.failEnd, fc19EmptySet)
	}
	r.Add(
		chaos.Check("FC-19: Nodes whose discovery Upstreams kept their set through 3 failed refreshes of a failing server", k, float64(kept), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: Nodes raising discovery_stale while the server failed", k, float64(len(r.Sampler.ReasonRaised(nodes, stale, at.fail, at.failEnd))), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: Nodes raising ruralz_upstream_degraded_info discovery_stale on both discovery Upstreams", k, float64(gauges), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: discovery-Route requests failing while the server failed", k, float64(failing), chaos.EQ, 0),
	)

	// NXDOMAIN and empty answers: a set emptied only once 3 refreshes
	// repeated the answer, never earlier, and 503 RZ-UP-008 from then on.
	least := map[string]int{}
	emptied := 0
	var early, empty int64
	for _, n := range nodes {
		all := true
		for _, u := range fc19Discovered() {
			repeated, lastKept, emptiedAt, ok := DNSEmptied(track.series(n, u), at.notFound)
			t.Logf("gameday: %s's %s: %d failed refreshes kept the set, last kept %v and emptied %v after the switch (%v)",
				n, u, repeated, lastKept.Sub(at.notFound), emptiedAt.Sub(at.notFound), ok)
			if !ok {
				all = false
			}
			if cur, seen := least[u]; !seen || repeated < cur {
				least[u] = repeated
			}
			for _, q := range byUpstream[u].Requests() {
				if q.Code == fc19EmptySet && node(q) == n && q.Done.Before(lastKept) {
					early++
				}
			}
		}
		if all {
			emptied++
		}
	}
	for _, st := range byUpstream {
		empty += chaos.CodesIn(st, at.notFound, at.heldEnd, fc19EmptySet)
	}
	r.Add(
		chaos.Check("FC-19: fewest NXDOMAIN refreshes discovered-a's set was kept through before it emptied, over the Nodes", k, float64(least[chaos.UpstreamDiscoveredA]), chaos.GE, float64(discovery.NotFoundLimit-1)),
		chaos.Check("FC-19: fewest empty answers discovered-srv's set was kept through before it emptied, over the Nodes", k, float64(least[chaos.UpstreamDiscoveredSRV]), chaos.GE, float64(discovery.NotFoundLimit-1)),
		chaos.Check("FC-19: Nodes whose discovery sets emptied on the repeated NXDOMAIN and empty answers", k, float64(emptied), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: RZ-UP-008 answers before a reading still showing the Node's set", k, float64(early), chaos.EQ, 0),
		chaos.Check("FC-19: discovery-Route requests answered 503 RZ-UP-008 once the sets emptied", k, float64(empty), chaos.GT, 0),
	)

	// The State Store host does not resolve: the failed dials open every
	// breaker, and failureMode decides.
	closed := l.StreamNamed("closed")
	r.Add(
		chaos.Check("FC-19: Nodes whose State Store breaker opened on failed dials", k, float64(len(r.Sampler.ReasonRaised(nodes, chaos.BreakerReason, at.reset, at.end))), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: fail-open decisions of /limited's ratelimit while the State Store host did not resolve", k,
			r.SumIncrease(catalog.RateLimitDecisionsTotal, promtext.L("policy", chaos.PolicyLimited, "result", "fail_open"), at.reset, at.end), chaos.GT, 0),
		chaos.Check("FC-19: closed-Policy RZ-STS answers while the State Store host did not resolve", k, float64(chaos.CodesIn(closed, at.reset, at.end, chaos.STSCodes()...)), chaos.GT, 0),
		chaos.Check("FC-19: closed-Policy admissions after the first RZ-STS while the State Store host did not resolve", k, float64(chaos.AdmittedAfter(closed, at.reset, at.end, chaos.STSCodes()...)), chaos.EQ, 0),
	)

	// The server answers again: every set back, fresh, before the
	// recovery, and no request meets an empty set in it.
	back := 0
	for _, n := range nodes {
		all := true
		for _, u := range fc19Discovered() {
			if b, ok := DNSBack(track.series(n, u), want[fc19Key(n, u)], at.end); !ok || !b.Before(at.rec.Start) {
				all = false
			}
		}
		if all {
			back++
		}
	}
	var late int64
	for _, st := range byUpstream {
		late += chaos.CodesIn(st, at.rec.Start, at.rec.End, fc19EmptySet)
	}
	r.Add(
		chaos.Check("FC-19: Nodes whose discovery sets came back once the server answered", k, float64(back), chaos.EQ, float64(len(nodes))),
		chaos.Check("FC-19: RZ-UP-008 answers in the recovery", k, float64(late), chaos.EQ, 0),
	)
}

// fc19Ports returns the ports of the mock Upstreams' addresses.
func fc19Ports(backends [3]string) ([3]uint16, error) {
	var out [3]uint16
	for i, b := range backends {
		ap, err := netip.ParseAddrPort(b)
		if err != nil {
			return out, fmt.Errorf("gameday: mock Upstream %d address %q: %w", i+1, b, err)
		}
		out[i] = ap.Port()
	}
	return out, nil
}

// fc19Records makes the server answer for the chaos Bundle's discovery
// Upstreams and the State Store host: discovered-a's name and the State
// Store host resolve to loopback, and discovered-srv's SRV name lists one
// target per mock Upstream on its port, each target resolving to
// loopback.
func fc19Records(dns *DNSServer, ports [3]uint16) error {
	lo := netip.MustParseAddr("127.0.0.1")
	recs := make([]DNSSRV, 0, len(ports))
	for i, target := range fc19Targets() {
		if err := dns.SetHost(target, lo); err != nil {
			return err
		}
		recs = append(recs, DNSSRV{Priority: 10, Weight: 1, Port: ports[i], Target: target})
	}
	if err := dns.SetSRV(fc19SRVName(), recs...); err != nil {
		return err
	}
	if err := dns.SetHost(chaos.DiscoveryAName, lo); err != nil {
		return err
	}
	return dns.SetHost(fc19StateHost, lo)
}

// fc19Accepted returns the connections each Node's State Store proxies
// accepted so far.
func fc19Accepted(r *chaos.Run) map[string]int64 {
	out := map[string]int64{}
	for _, n := range r.Top.Nodes() {
		ps, _ := n.StateStoreProxy("main")
		for _, p := range ps {
			out[n.Name()] += p.Stats().Accepted
		}
	}
	return out
}

// fc19RotateURLs rewrites every Node's State Store URL file, by an atomic
// rename, to the same URL naming fc19StateHost in place of the loopback
// address: the Node, which polls its file secrets, opens a new client to
// it. No error carries the URL, whose password is a canary.
func fc19RotateURLs(r *chaos.Run) error {
	for _, n := range r.Top.Nodes() {
		cur, ok := n.StateStoreURL("main")
		if !ok {
			return fmt.Errorf("gameday: %s has no State Store URL", n.Name())
		}
		path, ok := n.SecretPath(chaos.RefStateStoreURL)
		if !ok {
			return fmt.Errorf("gameday: %s has no State Store URL file", n.Name())
		}
		u, err := url.Parse(cur)
		if err != nil {
			return fmt.Errorf("gameday: %s's State Store URL does not parse", n.Name())
		}
		_, port, err := net.SplitHostPort(u.Host)
		if err != nil {
			return fmt.Errorf("gameday: %s's State Store URL names no port", n.Name())
		}
		u.Host = net.JoinHostPort(fc19StateHost, port)
		tmp := path + ".rotate"
		if err := os.WriteFile(tmp, []byte(u.String()), 0o600); err != nil {
			return fmt.Errorf("gameday: %s: %w", n.Name(), err)
		}
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("gameday: %s: %w", n.Name(), err)
		}
	}
	return nil
}

// fc19Key keys one Node's discovery Upstream.
func fc19Key(node, upstream string) string { return node + "/" + upstream }

// fc19Track reads every Node's /debug/upstreams, outside the leak scan's
// capture store as the Sampler scrapes /metrics, and keeps the readings
// of the discovery Upstreams. Its methods are safe for concurrent use.
type fc19Track struct {
	tr     *http.Transport
	client *http.Client
	nodes  []*topology.Node

	mu     sync.Mutex
	last   time.Time
	obs    map[string][]DNSObservation // by fc19Key
	failed int
}

func newFC19Track(r *chaos.Run) *fc19Track {
	tr := &http.Transport{TLSClientConfig: r.Top.ClientTLS()}
	return &fc19Track{tr: tr, client: &http.Client{Transport: tr}, nodes: r.Top.Nodes(), obs: map[string][]DNSObservation{}}
}

// poll reads every Node once, together. A reading's time is when it was
// sent, so it shows the Node's state at that time or later.
func (k *fc19Track) poll(ctx context.Context) {
	k.mu.Lock()
	k.last = time.Now()
	k.mu.Unlock()
	var wg sync.WaitGroup
	for _, n := range k.nodes {
		wg.Go(func() {
			sent := time.Now()
			pctx, cancel := context.WithTimeout(ctx, fc19PollTimeout)
			defer cancel()
			doc, err := fc19Read(pctx, k.client, n)
			k.mu.Lock()
			defer k.mu.Unlock()
			if err != nil {
				k.failed++
				return
			}
			for _, u := range doc.Upstreams {
				if !slices.Contains(fc19Discovered(), u.Name) {
					continue
				}
				ids := make([]string, 0, len(u.Endpoints))
				for _, e := range u.Endpoints {
					ids = append(ids, e.Identity)
				}
				slices.Sort(ids)
				key := fc19Key(n.Name(), u.Name)
				if len(k.obs[key]) < fc19MaxObservations {
					k.obs[key] = append(k.obs[key], DNSObservation{At: sent, Failures: u.Source.Failures, Stale: u.Source.Stale, Endpoints: ids})
				}
			}
		})
	}
	wg.Wait()
}

// pollIfDue polls unless the last poll is younger than fc19PollEvery.
func (k *fc19Track) pollIfDue(ctx context.Context) {
	k.mu.Lock()
	due := time.Since(k.last) >= fc19PollEvery
	k.mu.Unlock()
	if due {
		k.poll(ctx)
	}
}

// fc19Read GETs one Node's /debug/upstreams with its operator token.
func fc19Read(ctx context.Context, c *http.Client, n *topology.Node) (adminapi.Upstreams, error) {
	var doc adminapi.Upstreams
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.AdminURL()+"/debug/upstreams", nil)
	if err != nil {
		return doc, err
	}
	req.Header.Set("Authorization", "Bearer "+n.OperatorToken())
	resp, err := c.Do(req)
	if err != nil {
		return doc, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return doc, fmt.Errorf("gameday: %s /debug/upstreams status %d", n.Name(), resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, fc19MaxBody)).Decode(&doc); err != nil {
		return doc, fmt.Errorf("gameday: %s /debug/upstreams: %w", n.Name(), err)
	}
	return doc, nil
}

// series returns the readings of one Node's discovery Upstream, oldest
// first.
func (k *fc19Track) series(node, upstream string) []DNSObservation {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.obs[fc19Key(node, upstream)])
}

// latestLocked returns the newest reading of one Node's discovery
// Upstream.
func (k *fc19Track) latestLocked(node, upstream string) (DNSObservation, bool) {
	s := k.obs[fc19Key(node, upstream)]
	if len(s) == 0 {
		return DNSObservation{}, false
	}
	return s[len(s)-1], true
}

// all reports whether the newest reading of every discovery Upstream of
// every Node of nodes satisfies ok.
func (k *fc19Track) all(nodes []string, ok func(DNSObservation) bool) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, n := range nodes {
		for _, u := range fc19Discovered() {
			o, seen := k.latestLocked(n, u)
			if !seen || !ok(o) {
				return false
			}
		}
	}
	return true
}

// back reports whether the newest reading of every discovery Upstream of
// every Node of nodes shows its set want again, fresh.
func (k *fc19Track) back(nodes []string, want map[string][]string) bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, n := range nodes {
		for _, u := range fc19Discovered() {
			o, seen := k.latestLocked(n, u)
			if !seen || o.Stale || !slices.Equal(o.Endpoints, want[fc19Key(n, u)]) {
				return false
			}
		}
	}
	return true
}

// sets returns the Endpoint sets of the newest readings.
func (k *fc19Track) sets(nodes []string) map[string][]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string][]string{}
	for _, n := range nodes {
		for _, u := range fc19Discovered() {
			if o, ok := k.latestLocked(n, u); ok {
				out[fc19Key(n, u)] = o.Endpoints
			}
		}
	}
	return out
}

// latest returns the newest readings, the failure evidence of a walk that
// ended at its bound.
func (k *fc19Track) latest(nodes []string) map[string]string {
	k.mu.Lock()
	defer k.mu.Unlock()
	out := map[string]string{}
	for _, n := range nodes {
		for _, u := range fc19Discovered() {
			if o, ok := k.latestLocked(n, u); ok {
				out[fc19Key(n, u)] = "failures " + strconv.Itoa(o.Failures) + ", stale " + strconv.FormatBool(o.Stale) + ", endpoints " + strconv.Itoa(len(o.Endpoints))
			}
		}
	}
	return out
}

// failures returns how many readings failed.
func (k *fc19Track) failures() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.failed
}

// close drops the readings' idle connections.
func (k *fc19Track) close() { k.tr.CloseIdleConnections() }
