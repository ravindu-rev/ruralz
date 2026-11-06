// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package chaos

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// Upstream row parameters: the chaos Upstream's defaults
// (docs/architecture/09-traffic-management-and-resilience.md
// "Deadlines" and "Health checking and outlier detection").
const (
	// PerTryTimeout is the default retries.perTryTimeout of a Route with
	// the 15 s default timeout and one retry: 15 s / 2.
	PerTryTimeout = 7500 * time.Millisecond
	// StallTimeout is the default healthCheck.stallTimeout, after which
	// an attempt without response headers turns its Endpoint suspect.
	StallTimeout = time.Second
	// slowUpstream is the slowed Endpoint's delay, which outlasts the
	// phase and the perTryTimeout of its last attempt in real time: the
	// Nodes' clock runs no faster than real time, and slower while the
	// pump advances late, so a delay just past PerTryTimeout in real time
	// could answer before an attempt's timeout on the Nodes' clock. The
	// mock ends a delayed request its attempt abandoned.
	slowUpstream = FaultPhase + PerTryTimeout
	// UpstreamName is the chaos Bundle's Upstream.
	UpstreamName = "chaos-backend"
)

// Phases of the Upstream row.
const (
	PhaseOneResetting = "one-resetting"
	PhaseOneSlowed    = "one-slowed"
	PhaseAllResetting = "all-resetting"
)

// TestUpstreamRow is the System overview row "Upstream failing" on the
// chaos Upstream's three Endpoints at 50% load, in three 60 s phases: one
// Endpoint resetting, one slowed past perTryTimeout, then all three
// resetting. The resetting Endpoint is ejected and GETs succeed on
// retries; the slowed one turns suspect within 1 s, new attempts skip
// it, attempts already on it end at perTryTimeout with RZ-UP-003, which
// ejects it; with all three resetting the breaker opens and requests get
// 503 RZ-UP-005, until half-open probes close it once the Endpoints
// recover. Each phase shows in ruralz_upstream_ejections_total,
// ruralz_upstream_retries_total and ruralz_upstream_breaker_state_info
// (docs/engineering/03-testing-and-quality-strategy.md "Chaos testing").
func TestUpstreamRow(t *testing.T) {
	s := saturation(t)
	// The suspect mark and the slowed mock's arrivals are read every
	// 100 ms; /metrics on the default period, whose rounds of every
	// Node's exposition would otherwise run ten times a second.
	every := 100 * time.Millisecond
	r := suite.Start(t, Spec{Name: RowUpstream})
	rate := suite.Profile.LoadShare * s
	l := r.StartLoad(LoadSpec{Duration: Baseline + 3*FaultPhase + RecoveryTime, Streams: []StreamSpec{
		{Name: "plain", Path: PathPlain, Protocol: loadgen.H2C, Rate: rate, Counted: nil},
	}})
	reset := mockup.Behavior{Reset: mockup.ResetConn}
	set := func(i int, b mockup.Behavior) func() error { return func() error { return r.Mocks[i].SetBehavior(b) } }
	r.Begin(l)
	base := r.Hold(l, PhaseBaseline, Baseline)

	aStart := r.Inject("reset", r.Mocks[0].Name(), set(0, reset))
	a := r.Hold(l, PhaseOneResetting, FaultPhase)
	r.Inject("heal", r.Mocks[0].Name(), set(0, mockup.Behavior{}))

	slowBefore := r.Mocks[1].Stats()
	arrivals := trackRequests(r.Context(), r.Mocks[1], every)
	bStart := r.Inject("slow", r.Mocks[1].Name(), set(1, mockup.Behavior{Delay: mockup.Fixed(slowUpstream)}))
	suspects := trackSuspect(r.Context(), r, r.Mocks[1].Addr(), every)
	b := r.Hold(l, PhaseOneSlowed, FaultPhase)
	suspects.stop()
	r.Inject("heal", r.Mocks[1].Name(), set(1, mockup.Behavior{}))

	cStart := time.Now()
	for i := range r.Mocks {
		r.Inject("reset", r.Mocks[i].Name(), set(i, reset))
	}
	c := r.Hold(l, PhaseAllResetting, FaultPhase)
	end := time.Now()
	for i := range r.Mocks {
		r.Inject("heal", r.Mocks[i].Name(), set(i, mockup.Behavior{}))
	}
	rec := r.Hold(l, PhaseRecovery, RecoveryTime)
	if err := l.Wait(); err != nil {
		t.Fatal(err)
	}
	st := l.Streams[0]
	r.IntervalLog(st.Spec.Name, st)
	r.Baseline(l, base)
	nodes := r.NodeNames()
	up := promtext.L("upstream", UpstreamName)
	perNode := func(name string, match promtext.Labels, from, to time.Time) (least float64) {
		least = -1
		for _, n := range nodes {
			v := Increase(r.Sampler.Trace(n, name, match), from, to)
			if least < 0 || v < least {
				least = v
			}
		}
		return max(least, 0)
	}

	// One Endpoint resetting.
	r.Add(
		Check("one resetting: failed requests", Functional, float64(PhaseStats(st, aStart, a.End).Failed), EQ, 0),
		Check("one resetting: passive ejections on the Node with the fewest", Functional, perNode("ruralz_upstream_ejections_total", promtext.L("upstream", UpstreamName, "reason", "passive"), aStart, a.End), GE, 1),
		Check("one resetting: retries on the Node with the fewest", Functional, perNode("ruralz_upstream_retries_total", up, aStart, a.End), GT, 0),
	)
	r.Breakdown("one_resetting_retry_budget_exhausted", r.SumIncrease("ruralz_upstream_retry_budget_exhausted_total", up, aStart, a.End))

	// One Endpoint slowed past perTryTimeout. A suspect Endpoint stays
	// eligible, so ruralz_upstream_healthy_endpoints never shows the mark:
	// a Node's mark is the first /debug/upstreams answer showing the
	// Endpoint suspect, at the Node time of its arrival.
	suspect := time.Duration(0)
	var lastSuspect time.Time
	bNode := r.NodeTime(bStart)
	for _, n := range nodes {
		at, ok := suspects.first(n)
		if !ok {
			t.Logf("chaos: %s never showed the slowed Endpoint suspect in %d /debug/upstreams answers (%d failed)", n, suspects.answers(n), suspects.failures(n))
			suspect = slowUpstream
			continue
		}
		suspect = max(suspect, r.NodeTime(at).Sub(bNode))
		if at.After(lastSuspect) {
			lastSuspect = at
		}
	}
	// Requests the slowed mock received after every Node marked it
	// suspect, a sample later, and before the timeouts eject it.
	var skipped int64 = -1
	if !lastSuspect.IsZero() {
		skipped = arrivals.between(lastSuspect.Add(every), bStart.Add(PerTryTimeout-every))
	}
	// The attempts on the slowed Endpoint end at perTryTimeout as attempt
	// timeouts, RZ-UP-003; a retry within the budget may still answer the
	// client, so they show in ruralz_upstream_attempts_total rather than
	// in the responses, which carry RZ-UP-003 only past the budget.
	attemptTimeouts := r.SumIncrease(catalog.UpstreamAttemptsTotal, promtext.L("upstream", UpstreamName, "error", "timeout"), bStart, b.End)
	timeouts := CodesIn(st, bStart, b.End, "RZ-UP-003")
	stB := PhaseStats(st, bStart, b.End)
	r.Add(
		Check("one slowed: Endpoint suspect after the slowdown, seconds (sampled every 100 ms)", Functional, suspect.Seconds(), LE, (StallTimeout+every).Seconds()),
		Check("one slowed: new attempts on the suspect Endpoint", Functional, float64(skipped), EQ, 0),
		Check("one slowed: attempts ending with RZ-UP-003", Functional, attemptTimeouts, GT, 0),
		Check("one slowed: failures other than RZ-UP-003", Functional, float64(stB.Failed-timeouts), EQ, 0),
		Check("one slowed: ejections on the Node with the fewest", Functional, perNode("ruralz_upstream_ejections_total", up, bStart, b.End), GE, 1),
	)
	arrivals.stop()
	r.Breakdown("one_slowed_requests_received", float64(r.Mocks[1].Stats().Requests-slowBefore.Requests))

	// All three resetting.
	opened := 0
	for _, n := range nodes {
		if _, ok := FirstAt(r.Sampler.Trace(n, "ruralz_upstream_breaker_state_info", promtext.L("upstream", UpstreamName, "state", "open")), cStart, func(v float64) bool { return v > 0 }); ok {
			opened++
		}
	}
	r.Add(
		Check("all resetting: Nodes whose breaker opened", Functional, float64(opened), EQ, float64(len(nodes))),
		Check("all resetting: requests answered 503 RZ-UP-005", Functional, float64(CodesIn(st, cStart, c.End, "RZ-UP-005")), GT, 0),
	)
	// Recovery: half-open probes close every breaker, then no request
	// fails.
	var closedAt time.Time
	closedAll := true
	for _, n := range nodes {
		p, ok := FirstAt(r.Sampler.Trace(n, "ruralz_upstream_breaker_state_info", promtext.L("upstream", UpstreamName, "state", "closed")), end, func(v float64) bool { return v > 0 })
		// Closed by the sample's answer: the Node rendered it after the
		// scrape began.
		if !ok || !p.Done.Before(rec.End) {
			closedAll = false
			continue
		}
		if p.Done.After(closedAt) {
			closedAt = p.Done
		}
	}
	r.Add(Holds("breakers closed by half-open probes within the recovery", Functional, closedAll))
	if closedAll {
		r.Breakdown("breakers_closed_after_heal_s", closedAt.Sub(end).Seconds())
		r.Add(Check("recovery errors after the breakers closed", Functional, float64(PhaseStats(st, closedAt, rec.End).Failed), EQ, 0))
	}
	r.Finish()
}

// requestTrack samples a mock Upstream's request count on a period.
type requestTrack struct {
	mu     sync.Mutex
	at     []time.Time
	count  []int64
	cancel context.CancelFunc
	done   chan struct{}
}

// trackRequests samples m's request count every period until stop.
func trackRequests(ctx context.Context, m *mockup.Server, every time.Duration) *requestTrack {
	ctx, cancel := context.WithCancel(ctx)
	rt := &requestTrack{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(rt.done)
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			n := m.Stats().Requests
			rt.mu.Lock()
			rt.at, rt.count = append(rt.at, time.Now()), append(rt.count, n)
			rt.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return rt
}

func (rt *requestTrack) stop() {
	rt.cancel()
	<-rt.done
}

// maxUpstreamsBody bounds a /debug/upstreams answer the row reads.
const maxUpstreamsBody = 1 << 20

// suspectTrack polls every Node's /debug/upstreams on a period, outside
// the leak scan's capture store as the sampler scrapes, and keeps each
// Node's first answer showing one Endpoint of the chaos Upstream suspect.
type suspectTrack struct {
	mu     sync.Mutex
	at     map[string]time.Time
	polled map[string]int
	failed map[string]int
	cancel context.CancelFunc
	done   chan struct{}
}

// trackSuspect polls r's Nodes every period for the Endpoint identity
// until stop.
func trackSuspect(ctx context.Context, r *Run, identity string, every time.Duration) *suspectTrack {
	ctx, cancel := context.WithCancel(ctx)
	st := &suspectTrack{at: map[string]time.Time{}, polled: map[string]int{}, failed: map[string]int{}, cancel: cancel, done: make(chan struct{})}
	tr := &http.Transport{TLSClientConfig: r.Top.ClientTLS()}
	client := &http.Client{Transport: tr}
	nodes := r.Top.Nodes()
	go func() {
		defer close(st.done)
		defer tr.CloseIdleConnections()
		ticker := time.NewTicker(every)
		defer ticker.Stop()
		for {
			var wg sync.WaitGroup
			for _, n := range nodes {
				wg.Go(func() {
					pctx, cancelPoll := context.WithTimeout(ctx, max(every, time.Second))
					defer cancelPoll()
					suspect, err := endpointSuspect(pctx, client, n.AdminURL()+"/debug/upstreams", n.OperatorToken(), identity)
					arrived := time.Now()
					st.mu.Lock()
					defer st.mu.Unlock()
					st.polled[n.Name()]++
					switch {
					case err != nil:
						st.failed[n.Name()]++
					case suspect:
						if _, seen := st.at[n.Name()]; !seen {
							st.at[n.Name()] = arrived
						}
					}
				})
			}
			wg.Wait()
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return st
}

// endpointSuspect GETs one /debug/upstreams answer and reports whether
// the chaos Upstream's Endpoint identity is suspect in it.
func endpointSuspect(ctx context.Context, client *http.Client, url, token, identity string) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("chaos: /debug/upstreams status %d", resp.StatusCode)
	}
	var doc adminapi.Upstreams
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxUpstreamsBody)).Decode(&doc); err != nil {
		return false, fmt.Errorf("chaos: /debug/upstreams: %w", err)
	}
	for _, u := range doc.Upstreams {
		if u.Name != UpstreamName {
			continue
		}
		for _, e := range u.Endpoints {
			if e.Identity == identity {
				return e.Suspect, nil
			}
		}
	}
	return false, fmt.Errorf("chaos: /debug/upstreams lists no Endpoint %s of %s", identity, UpstreamName)
}

func (st *suspectTrack) stop() {
	st.cancel()
	<-st.done
}

// first returns the arrival of node's first answer showing the Endpoint
// suspect.
func (st *suspectTrack) first(node string) (time.Time, bool) {
	st.mu.Lock()
	defer st.mu.Unlock()
	at, ok := st.at[node]
	return at, ok
}

// answers returns how many polls of node ended.
func (st *suspectTrack) answers(node string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.polled[node]
}

// failures returns how many polls of node failed.
func (st *suspectTrack) failures(node string) int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.failed[node]
}

// between returns the requests received between the last sample at or
// before from and the first sample at or after to; -1 without samples
// covering the interval.
func (rt *requestTrack) between(from, to time.Time) int64 {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	lo, hi := -1, -1
	for i, t := range rt.at {
		if !t.After(from) {
			lo = i
		}
		if !t.Before(to) && hi < 0 {
			hi = i
		}
	}
	if lo < 0 || hi < 0 || hi < lo {
		return -1
	}
	return rt.count[hi] - rt.count[lo]
}
