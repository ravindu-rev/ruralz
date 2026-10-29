// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package revalidate

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/memory"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// waitLimit bounds every wait for an event a test caused; reaching it
// fails the test instead of hanging it. No assertion depends on timing.
const waitLimit = 10 * time.Second

// Job fixtures.
const (
	policyTimeout = 50 * time.Millisecond
	testRoute     = "catalog"
	testUpstream  = "catalog-svc"
	testURL       = "https://shop.example/v1/catalog?page=2"
)

// testVariant is the variant every fixture entry is stored under.
func testVariant() statestore.Digest { return statestore.DigestOf("accept-encoding=gzip") }

// testKey returns the partition key number n.
func testKey(n int) statestore.CacheKey {
	return statestore.CacheKey{URI: statestore.DigestOf("uri-" + strconv.Itoa(n)), Partition: statestore.DigestOf("anonymous")}
}

// seqSource returns 1, 2, 3, ... (or vals first), safely across workers.
type seqSource struct {
	n    atomic.Uint64
	mu   sync.Mutex
	vals []uint64
}

// Uint64 implements rand.Source.
func (s *seqSource) Uint64() uint64 {
	s.mu.Lock()
	if len(s.vals) > 0 {
		v := s.vals[0]
		s.vals = s.vals[1:]
		s.mu.Unlock()
		return v
	}
	s.mu.Unlock()
	return s.n.Add(1)
}

// recorded is one lease write as the store saw it.
type recorded struct {
	mode    statestore.LeaseMode
	token   uint64
	ttl     time.Duration
	timeout time.Duration
	entry   string
	err     *statestore.Error
	applied bool
}

// testStore passes writes to the memory driver, recording each lease
// write; intercept, when set, may answer a write instead (returning true).
type testStore struct {
	statestore.Store
	mu        sync.Mutex
	writes    []recorded
	intercept func(w *statestore.Write) bool
}

// Write implements statestore.Store.
func (s *testStore) Write(ctx context.Context, batch []*statestore.Write) {
	s.mu.Lock()
	icpt := s.intercept
	s.mu.Unlock()
	pass := make([]*statestore.Write, 0, len(batch))
	for _, w := range batch {
		if icpt == nil || !icpt(w) {
			pass = append(pass, w)
		}
	}
	if len(pass) > 0 {
		s.Store.Write(ctx, pass)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, w := range batch {
		s.writes = append(s.writes, recorded{
			mode: w.Lease.Mode, token: w.Lease.Token, ttl: w.Lease.TTL, timeout: w.Timeout,
			entry: string(w.Lease.Entry), err: w.Err, applied: w.Applied,
		})
	}
}

// setIntercept replaces the intercept function.
func (s *testStore) setIntercept(f func(w *statestore.Write) bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.intercept = f
}

// recordedWrites returns the lease writes so far.
func (s *testStore) recordedWrites() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.writes...)
}

// modes returns the modes of the lease writes so far.
func (s *testStore) modes() []statestore.LeaseMode {
	var out []statestore.LeaseMode
	for _, w := range s.recordedWrites() {
		out = append(out, w.mode)
	}
	return out
}

// testEntry is a stale entry over a saved GET of testURL.
type testEntry struct {
	reqHeader  http.Header
	header     http.Header
	authorized bool
	reqErr     error
	refreshErr error

	mu        sync.Mutex
	refreshed *Refreshed
}

// Request implements Entry.
func (e *testEntry) Request(ctx context.Context) (*http.Request, error) {
	if e.reqErr != nil {
		return nil, e.reqErr
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, http.NoBody)
	if err != nil {
		return nil, err
	}
	r.Header = e.reqHeader.Clone()
	if r.Header == nil {
		r.Header = http.Header{}
	}
	return r, nil
}

// Header implements Entry.
func (e *testEntry) Header() http.Header { return e.header }

// Authorized implements Entry.
func (e *testEntry) Authorized() bool { return e.authorized }

// Refresh implements Entry: the bytes name the refreshed Cache-Control.
func (e *testEntry) Refresh(r *Refreshed) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.refreshed = r
	if e.refreshErr != nil {
		return nil, e.refreshErr
	}
	return []byte("refreshed " + r.Header.Get("Cache-Control")), nil
}

// lastRefresh returns the last Refresh argument.
func (e *testEntry) lastRefresh() *Refreshed {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshed
}

// replayCall is one Replay call.
type replayCall struct {
	route, upstream, method, url string
	header                       http.Header
}

// testReplayer records Replay calls and answers them with reply.
type testReplayer struct {
	mu     sync.Mutex
	calls  []replayCall
	reply  func(ctx context.Context, req *http.Request) (*http.Response, error)
	called chan struct{}
}

// Replay implements filter.Replayer.
func (p *testReplayer) Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
	p.mu.Lock()
	p.calls = append(p.calls, replayCall{route: route, upstream: upstream, method: req.Method, url: req.URL.String(), header: req.Header.Clone()})
	reply := p.reply
	p.mu.Unlock()
	p.called <- struct{}{}
	return reply(ctx, req)
}

// setReply replaces the answer.
func (p *testReplayer) setReply(f func(ctx context.Context, req *http.Request) (*http.Response, error)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reply = f
}

// replays returns the calls so far.
func (p *testReplayer) replays() []replayCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]replayCall(nil), p.calls...)
}

// body is a response body that counts Close calls.
type body struct {
	io.Reader
	closed atomic.Int32
}

// Close implements io.Closer.
func (b *body) Close() error {
	b.closed.Add(1)
	return nil
}

// respond returns a response with status, header and a counting body.
func respond(status int, h http.Header) (*http.Response, *body) {
	b := &body{Reader: strings.NewReader("payload")}
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Header: h, Body: b}, b
}

// harness runs one Revalidator over the memory driver on a fake clock.
type harness struct {
	t      *testing.T
	clk    *clocktest.Fake
	mem    *memory.Driver
	store  *testStore
	rnd    *seqSource
	rv     *Revalidator
	rp     *testReplayer
	done   chan Outcome
	stop   context.CancelFunc
	result chan error
}

// newHarness returns an idle harness whose replayer answers 304 to every
// replay; the fake clock starts at t0.
func newHarness(t *testing.T) *harness {
	t.Helper()
	clk := clocktest.New(t0)
	mem, err := memory.New(t.Context(), statestore.Config{}, statestore.Deps{Clock: clk}, memory.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mem.Close(context.Background()) })
	h := &harness{
		t: t, clk: clk, mem: mem, store: &testStore{Store: mem}, rnd: &seqSource{},
		rp:   &testReplayer{called: make(chan struct{}, 1024)},
		done: make(chan Outcome, 8192), result: make(chan error, 1),
	}
	h.rp.reply = func(context.Context, *http.Request) (*http.Response, error) {
		r, _ := respond(http.StatusNotModified, nil)
		return r, nil
	}
	h.rv = newRevalidator(h)
	return h
}

// newRevalidator returns a Revalidator on h's clock and token source that
// reports outcomes to h.done.
func newRevalidator(h *harness) *Revalidator {
	rv := New(Options{Clock: h.clk, Rand: h.rnd})
	rv.finished = func(o Outcome) { h.done <- o }
	return rv
}

// run starts Run; the test's cleanup stops it.
func (h *harness) run() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	h.stop = cancel
	go func() { h.result <- h.rv.Run(ctx) }()
	h.t.Cleanup(h.shutdown)
}

// shutdown stops Run and waits for it.
func (h *harness) shutdown() {
	if h.stop == nil {
		return
	}
	h.stop()
	h.stop = nil
	select {
	case err := <-h.result:
		if err != nil {
			h.t.Errorf("Run = %v", err)
		}
	case <-time.After(waitLimit):
		h.t.Error("Run did not return")
	}
}

// next returns the next job outcome.
func (h *harness) next() Outcome {
	h.t.Helper()
	select {
	case o := <-h.done:
		return o
	case <-time.After(waitLimit):
		h.t.Fatal("no job finished")
		return 0
	}
}

// awaitReplay waits for the next Replay call.
func (h *harness) awaitReplay() {
	h.t.Helper()
	select {
	case <-h.rp.called:
	case <-time.After(waitLimit):
		h.t.Fatal("no replay started")
	}
}

// seed stores the entry "stale" under k's partition for testVariant, then
// moves the clock past the store's 1 s fill lease.
func (h *harness) seed(k statestore.CacheKey) {
	h.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{
		Key: k, Variant: testVariant(), Generation: 1, TTL: time.Hour, Entry: []byte("stale"), Token: 1 << 40,
	}}
	h.mem.Write(context.Background(), []*statestore.Write{w})
	if w.Err != nil || !w.Applied {
		h.t.Fatalf("seeding %v: err %v, applied %v", k, w.Err, w.Applied)
	}
	h.clk.Advance(2 * time.Second)
}

// reply304 answers every replay with a 304 carrying hdr.
func (h *harness) reply304(hdr http.Header) {
	h.rp.setReply(func(context.Context, *http.Request) (*http.Response, error) {
		r, _ := respond(http.StatusNotModified, hdr.Clone())
		return r, nil
	})
}

// lookup reads k's variant from the State Store.
func (h *harness) lookup(k statestore.CacheKey) (string, bool) {
	h.t.Helper()
	c := &statestore.Call{Kind: statestore.OpCacheGet, Timeout: time.Second, Lookup: statestore.CacheLookup{Key: k, Variant: testVariant()}}
	h.mem.Read(context.Background(), nil, []*statestore.Call{c})
	if c.Err != nil {
		h.t.Fatalf("lookup %v: %v", k, c.Err)
	}
	return string(c.Lookup.Entry), c.Lookup.Found
}

// takeLease takes k's revalidation lease for another holder.
func (h *harness) takeLease(k statestore.CacheKey) {
	h.t.Helper()
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Lease: statestore.CacheLease{
		Key: k, Mode: statestore.LeaseTake, Token: 1 << 50,
	}}
	h.mem.Write(context.Background(), []*statestore.Write{w})
	if w.Err != nil || !w.Applied {
		h.t.Fatalf("taking the lease of %v: err %v, applied %v", k, w.Err, w.Applied)
	}
}

// staleHeader is the stored header of the fixture entries.
func staleHeader() http.Header {
	return http.Header{
		"Etag":          {`"v1"`},
		"Last-Modified": {date(t0.Add(-time.Hour))},
		"Cache-Control": {"max-age=60, stale-while-revalidate=30, stale-if-error=90"},
		"Content-Type":  {"application/json"},
		"Date":          {date(t0)},
	}
}

// entry returns a fixture entry with stored header hdr.
func entry(hdr http.Header) *testEntry {
	return &testEntry{reqHeader: http.Header{"Accept": {"application/json"}}, header: hdr}
}

// job returns a Job for k over e on h's store and replayer.
func (h *harness) job(k statestore.CacheKey, e Entry) Job {
	return Job{
		Key: k, Variant: testVariant(), Store: h.store, Timeout: policyTimeout,
		Replayer: h.rp, Route: testRoute, Upstream: testUpstream, Entry: e,
	}
}

// enqueue queues a job and fails the test when it is refused.
func (h *harness) enqueue(k statestore.CacheKey, e Entry) {
	h.t.Helper()
	if !h.rv.Enqueue(h.job(k, e)) {
		h.t.Fatalf("Enqueue(%v) refused", k)
	}
}

// counter is an emit.Counter that counts.
type counter struct{ n atomic.Uint64 }

// Add implements emit.Counter.
func (c *counter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// routeMetrics returns Route handles whose Revalidations count, and the
// counters by emit.Revalidation* result.
func routeMetrics() (*emit.RouteMetrics, [emit.NumRevalidationResults]*counter) {
	m := &emit.RouteMetrics{}
	var cs [emit.NumRevalidationResults]*counter
	for i := range cs {
		cs[i] = &counter{}
		m.Revalidations[i] = cs[i]
	}
	return m, cs
}

// counts returns the values of cs.
func counts(cs [emit.NumRevalidationResults]*counter) [emit.NumRevalidationResults]uint64 {
	var out [emit.NumRevalidationResults]uint64
	for i, c := range cs {
		out[i] = c.n.Load()
	}
	return out
}

// status records the degraded reasons the Revalidator sets.
type status struct {
	mu    sync.Mutex
	calls []degradedCall
}

// degradedCall is one SetDegraded call.
type degradedCall struct {
	reason catalog.Reason
	source string
	on     bool
}

// SetDegraded implements emit.NodeStatus.
func (s *status) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, degradedCall{r, source, on})
}

// SetCleartextHops implements emit.NodeStatus.
func (*status) SetCleartextHops(catalog.Hop, int) {}

// recorded returns the calls so far.
func (s *status) recorded() []degradedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]degradedCall(nil), s.calls...)
}

// gate blocks replays until opened.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

// newGate returns a closed gate that the test's cleanup opens.
func newGate(t *testing.T) *gate {
	g := &gate{ch: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

// open releases every waiter.
func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }
