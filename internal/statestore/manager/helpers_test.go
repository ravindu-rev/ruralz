// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package manager

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// storeHandle mirrors snapshot.StoreHandle, which this package cannot
// import: Handle must keep satisfying it.
type storeHandle interface {
	Store() statestore.Store
	Enqueuer() statestore.Enqueuer
	Release()
}

var _ storeHandle = (*Handle)(nil)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// events is an ordered log shared by the fake drivers of one test.
type events struct {
	mu  sync.Mutex
	log []string
}

func (e *events) add(s string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log = append(e.log, s)
}

func (e *events) list() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.log)
}

// driver is a fake statestore.Driver. Status returns status; Write
// records the refunds' names, waiting on blockWrite when set; Close waits
// for its context when blockClose is set and returns closeErr.
type driver struct {
	cfg    statestore.Config
	name   string
	events *events

	mu     sync.Mutex
	status statestore.Status
	writes []string

	blockWrite   chan struct{}
	writeEntered chan struct{}
	blockClose   bool
	closeErr     error

	statusCalls atomic.Int64
	closes      atomic.Int64
	closed      atomic.Bool
	lateWrites  atomic.Int64 // writes after Close
}

var _ statestore.Driver = (*driver)(nil)

func (d *driver) Consume(context.Context, *statestore.RequestBudget, []*statestore.Call) int {
	return 1
}
func (d *driver) Read(context.Context, *statestore.RequestBudget, []*statestore.Call) {}
func (d *driver) AdmitStoreBytes(statestore.CacheKey, int) bool                       { return true }

func (d *driver) Supports(statestore.Capability) bool { return true }

func (d *driver) RoundTrips() bool { return true }

func (d *driver) Write(ctx context.Context, batch []*statestore.Write) {
	if d.closed.Load() {
		d.lateWrites.Add(int64(len(batch)))
	}
	if d.writeEntered != nil {
		d.writeEntered <- struct{}{}
	}
	if d.blockWrite != nil {
		select {
		case <-d.blockWrite:
		case <-ctx.Done():
			for _, w := range batch {
				w.Err = statestore.ErrNotAttempted
			}
			return
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, w := range batch {
		w.Applied = true
		d.writes = append(d.writes, w.Refund.Name)
		if d.events != nil {
			d.events.add("write " + w.Refund.Name + " to " + d.name)
		}
	}
}

func (d *driver) Status() statestore.Status {
	d.statusCalls.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.status
}

func (d *driver) setStatus(s statestore.Status) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.status = s
}

func (d *driver) written() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.writes)
}

func (d *driver) Close(ctx context.Context) error {
	d.closes.Add(1)
	d.closed.Store(true)
	if d.events != nil {
		d.events.add("close " + d.name)
	}
	if d.blockClose {
		<-ctx.Done()
		return ctx.Err()
	}
	return d.closeErr
}

// opener is a fake statestore.Opener recording every driver it opens.
type opener struct {
	err    error
	setup  func(d *driver)
	events *events

	mu     sync.Mutex
	opened []*driver
}

func (o *opener) open(_ context.Context, cfg statestore.Config, _ statestore.Deps) (statestore.Driver, error) {
	if o.err != nil {
		return nil, o.err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	d := &driver{cfg: cfg, name: cfg.URL, events: o.events}
	if d.name == "" {
		d.name = "memory"
	}
	if o.setup != nil {
		o.setup(d)
	}
	o.opened = append(o.opened, d)
	return d, nil
}

func (o *opener) drivers() []*driver {
	o.mu.Lock()
	defer o.mu.Unlock()
	return slices.Clone(o.opened)
}

// openers returns Openers serving both drivers from o.
func (o *opener) openers() Openers { return Openers{Memory: o.open, Redis: o.open} }

// nodeStatus is a fake emit.NodeStatus.
type nodeStatus struct {
	mu      sync.Mutex
	sources [catalog.NumReasons]map[string]bool
	hops    [catalog.NumHops]int
}

var _ emit.NodeStatus = (*nodeStatus)(nil)

func (s *nodeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sources[r] == nil {
		s.sources[r] = map[string]bool{}
	}
	if on {
		s.sources[r][source] = true
	} else {
		delete(s.sources[r], source)
	}
}

func (s *nodeStatus) SetCleartextHops(h catalog.Hop, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hops[h] = n
}

// on reports whether any source holds r.
func (s *nodeStatus) on(r catalog.Reason) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sources[r]) > 0
}

func (s *nodeStatus) hop() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hops[catalog.HopStateStore]
}

// secrets is a fake secret.Store.
type secrets map[secret.Ref]string

func (s secrets) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s[r]
	if !ok {
		return secret.Value{}, false
	}
	return secret.NewValue([]byte(v)), true
}

func (s secrets) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// logRecorder keeps every log message.
type logRecorder struct {
	mu   sync.Mutex
	msgs []string
}

func (l *logRecorder) Enabled(context.Context, slog.Level) bool { return true }
func (l *logRecorder) Handle(_ context.Context, r slog.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.msgs = append(l.msgs, r.Message)
	return nil
}
func (l *logRecorder) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logRecorder) WithGroup(string) slog.Handler      { return l }

// count returns how many records carry msg.
func (l *logRecorder) count(msg string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, m := range l.msgs {
		if m == msg {
			n++
		}
	}
	return n
}

// Warning messages under test.
const (
	warnFallback = "no State Store is configured: the memory driver multiplies every Rate Limit and Quota by the Node count"
	warnShared   = "the Response Cache shares the State Store that holds Rate Limit and Quota keys"
)

// fixture is a Manager under test with its fakes.
type fixture struct {
	m      *Manager
	clk    *clocktest.Fake
	status *nodeStatus
	logs   *logRecorder
}

// newFixture returns a Manager over openers; cleanup closes it.
func newFixture(t *testing.T, ops Openers) *fixture {
	t.Helper()
	f := &fixture{clk: clocktest.New(epoch()), status: &nodeStatus{}, logs: &logRecorder{}}
	f.m = New(t.Context(), Config{
		Deps:    statestore.Deps{Clock: f.clk, Status: f.status, Logger: slog.New(f.logs)},
		Openers: ops,
	})
	// t.Context is canceled before cleanups run: Close skips the flush.
	t.Cleanup(func() { _ = f.m.Close(t.Context()) })
	return f
}

// acquire is Acquire failing the test on an error.
func (f *fixture) acquire(t *testing.T, spec Spec) Handles {
	t.Helper()
	hs, err := f.m.Acquire(t.Context(), spec)
	if err != nil {
		t.Fatalf("Acquire = %v", err)
	}
	return hs
}

// entries returns the number of open, shareable drivers.
func (f *fixture) entries() int {
	f.m.mu.Lock()
	defer f.m.mu.Unlock()
	return len(f.m.entries)
}

// redisSpec returns a standalone redis Spec for url.
func redisSpec(url string) Spec {
	return Spec{Main: statestore.Config{
		Driver: statestore.DriverRedis, Topology: statestore.TopologyStandalone, URL: url,
		DefaultTimeout: statestore.DefaultTimeout, Source: statestore.SourceGateway,
	}}
}

// memorySpec returns a memory Spec from src.
func memorySpec(src statestore.Source) Spec {
	return Spec{Main: statestore.Config{Driver: statestore.DriverMemory, DefaultTimeout: statestore.DefaultTimeout, Source: src}}
}

// cacheConfig returns a cache connection to url.
func cacheConfig(url string) *statestore.Config {
	return &statestore.Config{
		Driver: statestore.DriverRedis, Topology: statestore.TopologyStandalone, URL: url,
		DefaultTimeout: statestore.DefaultTimeout, Source: statestore.SourceGateway,
	}
}

// driverOf returns the fake driver behind h.
func driverOf(t *testing.T, h *Handle) *driver {
	t.Helper()
	d, ok := h.Store().(*driver)
	if !ok {
		t.Fatalf("Store() is %T, not the fake driver", h.Store())
	}
	return d
}

// refund returns a refund write named id.
func refund(id string) *statestore.Write {
	return &statestore.Write{Kind: statestore.OpRefund, Timeout: time.Second, Refund: statestore.Refund{Name: id, Window: time.Minute}}
}

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
