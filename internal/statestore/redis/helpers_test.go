// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"crypto/tls"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/breaker"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// start is the fake clock's start: mid-window for every window the tests
// use, and after the 2026 GCRA epoch.
func start() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 123_456_789, time.UTC) }

// lowRand always draws the low end: breakers open for OpenMin, reconnects
// wait no jitter.
func lowRand(lo, _ time.Duration) time.Duration { return lo }

// counter is an allocation-free emit.Counter.
type counter struct{ n atomic.Uint64 }

func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// histogram counts observations and sums them.
type histogram struct{ n, sum atomic.Uint64 }

func (h *histogram) Record(_ emit.Stripe, v uint64) { h.n.Add(1); h.sum.Add(v) }
func (h *histogram) RecordExemplar(s emit.Stripe, v uint64, _ emit.Exemplar) {
	h.Record(s, v)
}

// metrics records every State Store handle.
type metrics struct {
	calls [emit.NumStateOps][emit.NumStateResults]counter
	ops   [emit.NumStateOps]counter
	dur   [emit.NumStateOps][emit.NumStateZones]histogram
}

func (m *metrics) handles() *emit.StateMetrics {
	h := &emit.StateMetrics{}
	for op := range emit.NumStateOps {
		for r := range emit.NumStateResults {
			h.Calls[op][r] = &m.calls[op][r]
		}
		h.Ops[op] = &m.ops[op]
		for z := range emit.NumStateZones {
			h.CallDuration[op][z] = &m.dur[op][z]
		}
	}
	return h
}

func (m *metrics) call(op int, r statestore.Result) uint64 { return m.calls[op][r].n.Load() }

// durs returns the duration observations of op in every zone.
func (m *metrics) durs(op int) uint64 {
	var n uint64
	for z := range emit.NumStateZones {
		n += m.dur[op][z].n.Load()
	}
	return n
}

// zoned returns the duration observations of op labeled zone.
func (m *metrics) zoned(op, zone int) uint64 { return m.dur[op][zone].n.Load() }

// logRecorder is a slog.Handler keeping every record.
type logRecorder struct {
	mu   sync.Mutex
	recs []slog.Record
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r.Clone())
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

func (l *logRecorder) records() []slog.Record {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]slog.Record(nil), l.recs...)
}

// count returns how many records carry msg.
func (l *logRecorder) count(msg string) int {
	n := 0
	for _, r := range l.records() {
		if r.Message == msg {
			n++
		}
	}
	return n
}

// text renders every record with its attributes.
func (l *logRecorder) text() string {
	var b strings.Builder
	for _, r := range l.records() {
		b.WriteString(r.Level.String() + " " + r.Message)
		r.Attrs(func(a slog.Attr) bool {
			b.WriteString(" " + a.Key + "=" + a.Value.String())
			return true
		})
		b.WriteByte('\n')
	}
	return b.String()
}

// skewClock is a Node clock offset from its base, for the clock skew
// cases.
type skewClock struct {
	base clock.Clock
	off  atomic.Int64
}

func (s *skewClock) Now() time.Time                  { return s.base.Now().Add(time.Duration(s.off.Load())) }
func (s *skewClock) Since(t time.Time) time.Duration { return s.Now().Sub(t) }
func (s *skewClock) NewTimer(d time.Duration) clock.Timer {
	return s.base.NewTimer(d)
}

func (s *skewClock) AfterFunc(d time.Duration, f func()) clock.Timer {
	return s.base.AfterFunc(d, f)
}

// NewTimerAt and AfterFuncAt take t on this clock, the base's plus the
// offset.
func (s *skewClock) NewTimerAt(t time.Time) clock.Timer {
	return s.base.NewTimerAt(t.Add(-time.Duration(s.off.Load())))
}

func (s *skewClock) AfterFuncAt(t time.Time, f func()) clock.Timer {
	return s.base.AfterFuncAt(t.Add(-time.Duration(s.off.Load())), f)
}

func (s *skewClock) Sleep(ctx context.Context, d time.Duration) error {
	return s.base.Sleep(ctx, d)
}

// fixture is a driver on a fake clock in front of a fake server, with
// recorded metrics and logs.
type fixture struct {
	t   *testing.T
	clk *clocktest.Fake
	srv *fakeServer
	// nodes are a cluster's primaries, srv the first; cl their slots. Under
	// sentinel they are the servers, srv the first primary, which sent
	// names.
	nodes []*fakeServer
	cl    *fakeCluster
	sent  *fakeSentinel
	d     *Driver
	m     *metrics
	logs  *logRecorder
	gate  *statestore.InFlight
	// dropped counts the connections the State client dropped
	// (Options.Dropped).
	dropped atomic.Int64
}

// fixtureOptions adjust a fixture.
type fixtureOptions struct {
	url      string // default: the fake server's
	role     statestore.Role
	topology statestore.Topology
	limits   statestore.Limits
	mac      []byte
	opts     Options
	timeout  time.Duration // Config.DefaultTimeout
	tls      *statestore.TLS
	fake     fakeOptions
	noServer bool
	// setup runs on the fake server before the driver opens.
	setup func(s *fakeServer)
	// urlFor returns the driver's URL from the fake server's.
	urlFor func(s *fakeServer) string
	// cluster starts that many fake primaries splitting the slots evenly,
	// with topology cluster and every primary a seed.
	cluster int
	// sentinel starts that many fake servers and a fake sentinel naming
	// the first as master set main's primary, with topology sentinel;
	// sentinelTLS serves the sentinel over TLS, sentinelUser and
	// sentinelPassword are the credentials it requires, and sentinelURL
	// returns the driver's URL from the sentinel's address (default
	// redis://<addr>?master_set=main).
	sentinel                       int
	sentinelTLS                    *tls.Config
	sentinelUser, sentinelPassword string
	sentinelURL                    func(addr string) string
	// hosts are the loopback addresses of the cluster primaries or
	// sentinel servers, in order (default 127.0.0.1), so zone maps can
	// tell them apart.
	hosts []string
	// zone is the Node's RURALZ_ZONE (Deps.Zone), zones the connection's
	// zone map (Config.Zones).
	zone  string
	zones statestore.Zones
}

// hostAt returns the loopback address of the i-th server of o.
func (o fixtureOptions) hostAt(i int) string {
	if i < len(o.hosts) {
		return o.hosts[i]
	}
	return ""
}

// newFixture starts a fake server and a driver against it, and waits
// for the State client.
func newFixture(t *testing.T, o fixtureOptions) *fixture {
	t.Helper()
	f := &fixture{t: t, clk: clocktest.New(start()), m: &metrics{}, logs: &logRecorder{}}
	url := o.url
	if o.cluster > 0 {
		f.cl = &fakeCluster{}
		seeds := make([]string, o.cluster)
		for i := range o.cluster {
			fo := o.fake
			fo.cluster = f.cl
			fo.host = o.hostAt(i)
			n := newFakeServer(t, f.clk, fo)
			f.nodes = append(f.nodes, n)
			seeds[i] = n.addr
			first, last := i*16384/o.cluster, (i+1)*16384/o.cluster-1
			f.cl.ranges = append(f.cl.ranges, fakeRange{first: first, last: last, srv: n})
		}
		f.srv = f.nodes[0]
		o.topology = statestore.TopologyCluster
		url = "redis://" + seeds[0]
		if len(seeds) > 1 {
			url += "?addr=" + strings.Join(seeds[1:], "&addr=")
		}
		o.noServer = true
	}
	if o.sentinel > 0 {
		for i := range o.sentinel {
			fo := o.fake
			fo.host = o.hostAt(i)
			f.nodes = append(f.nodes, newFakeServer(t, f.clk, fo))
		}
		f.srv = f.nodes[0]
		f.sent = newFakeSentinel(t, o.sentinelTLS, o.sentinelUser, o.sentinelPassword, map[string]string{"main": f.srv.addr})
		o.topology = statestore.TopologySentinel
		url = "redis://" + f.sent.addr + "?master_set=main"
		if o.sentinelURL != nil {
			url = o.sentinelURL(f.sent.addr)
		}
		o.noServer = true
	}
	if !o.noServer {
		f.srv = newFakeServer(t, f.clk, o.fake)
		switch {
		case o.urlFor != nil:
			url = o.urlFor(f.srv)
		case url == "":
			url = f.srv.url()
		}
		if o.setup != nil {
			o.setup(f.srv)
		}
	}
	lim := fillLimits(o.limits)
	f.gate = statestore.NewInFlight(lim.InFlight, nil)
	if o.opts.Rand == nil {
		o.opts.Rand = lowRand
	}
	if o.opts.Dropped == nil {
		o.opts.Dropped = func() { f.dropped.Add(1) }
	}
	d, err := New(t.Context(), statestore.Config{
		Role: o.role, Driver: statestore.DriverRedis, Topology: o.topology, URL: url, DefaultTimeout: o.timeout, TLS: o.tls,
		Zones: o.zones,
	}, statestore.Deps{
		Clock: f.clk, Metrics: f.m.handles(), Logger: slog.New(f.logs), Limits: lim, MACKey: o.mac, InFlight: f.gate,
		Zone: o.zone,
	}, o.opts)
	if err != nil {
		t.Fatal(err)
	}
	f.d = d
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := d.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return f
}

// ready waits until the driver published its State client.
func (f *fixture) ready() {
	f.t.Helper()
	select {
	case <-f.d.ready:
	case <-time.After(10 * time.Second):
		f.t.Fatal("the State client was not created")
	}
}

// awaitIdle waits until the maintenance goroutine finished its pass and
// armed its next tick, the one timer a driver without a call or probe in
// flight holds on the fake clock, so the next advance fires that tick
// and never overtakes a pass still running.
func (f *fixture) awaitIdle() {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(f.t.Context(), 10*time.Second)
	defer cancel()
	if err := f.clk.BlockUntil(ctx, 1); err != nil {
		f.t.Fatalf("the maintenance goroutine never armed its next tick: %v", err)
	}
}

// awaitDropped waits until the State client dropped every connection it
// opened to the standalone server, one per HELLO the server answered,
// after the test or the server closed them all while no maintenance pass
// was sending. rueidis notices a closed connection on its own goroutine:
// until then a call can still go out on it and fail with a connection
// error instead of dialing anew.
func (f *fixture) awaitDropped() {
	f.t.Helper()
	waitFor(f.t, "the client to drop its closed connections", func() bool {
		return f.dropped.Load() == int64(f.srv.count("HELLO"))
	})
}

// waitFor polls cond on real time until it holds.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// budget returns a fresh per-request budget.
func budget() *statestore.RequestBudget {
	rb := statestore.NewRequestBudget(time.Hour, 0)
	return &rb
}

// callTO is the unit tests' Policy timeout: long on the fake clock, which
// moves only when a test advances it.
const callTO = 50 * time.Millisecond

// gcraCall is a GCRA call of Policy rl.
func gcraCall(d statestore.Digest, limits ...statestore.GCRALimit) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpGCRA, Timeout: callTO, GCRA: statestore.GCRA{
		Policy: "rl", Digest: d, Limits: limits, Out: make([]statestore.GCRAOutcome, len(limits)),
	}}
}

// quotaCall is a Quota call of Consumer quota q.
func quotaCall(window time.Duration, limit int64, d statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpQuota, Timeout: callTO, Quota: statestore.Quota{
		Name: "q", Window: window, Limit: limit, Digest: d,
	}}
}

func lookupCall(k statestore.CacheKey, v statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpCacheGet, Timeout: callTO, Lookup: statestore.CacheLookup{Key: k, Variant: v}}
}

// oneLimit is a GCRA limit of 10 per second with a burst of 10.
func oneLimit() statestore.GCRALimit {
	return statestore.GCRALimit{Requests: 10, Window: time.Second, Burst: 10}
}

// consume runs calls as one request and returns how many Consume took.
func (f *fixture) consume(calls ...*statestore.Call) int {
	return f.d.Consume(context.Background(), budget(), calls)
}

func (f *fixture) read(calls ...*statestore.Call) {
	f.d.Read(context.Background(), budget(), calls)
}

func (f *fixture) write(ws ...*statestore.Write) {
	f.d.Write(context.Background(), ws)
}

// openBreaker fails 20 GCRA calls on the standalone shard, which opens its
// breaker.
func (f *fixture) openBreaker() {
	f.t.Helper()
	rule := f.srv.inject(&fault{cmd: "EVALSHA", err: "ERR injected", times: 20})
	for i := range 20 {
		c := gcraCall(statestore.DigestOf("open"+string(rune('a'+i))), oneLimit())
		f.consume(c)
		if c.Err != statestore.ErrFailed {
			f.t.Fatalf("call %d: Err %v, want RZ-STS-002", i, c.Err)
		}
	}
	_ = rule
	if st := f.d.single.brk.State(); st != breaker.Open {
		f.t.Fatalf("breaker %v after 20 failures, want open", st)
	}
}
