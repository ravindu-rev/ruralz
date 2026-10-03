// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package memory

import (
	"context"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// counter is an allocation-free emit.Counter that keeps per-stripe sums.
type counter struct{ n [8]atomic.Uint64 }

func (c *counter) Add(s emit.Stripe, n uint64) { c.n[s%8].Add(n) }

func (c *counter) total() uint64 {
	var t uint64
	for i := range c.n {
		t += c.n[i].Load()
	}
	return t
}

// histogram counts observations.
type histogram struct{ n atomic.Uint64 }

func (h *histogram) Record(emit.Stripe, uint64)                        { h.n.Add(1) }
func (h *histogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) { h.n.Add(1) }

// metrics records every State Store handle.
type metrics struct {
	calls [emit.NumStateOps][emit.NumStateResults]counter
	ops   [emit.NumStateOps]counter
	dur   [emit.NumStateOps]histogram
}

func newMetrics() *metrics { return &metrics{} }

func (m *metrics) handles() *emit.StateMetrics {
	h := &emit.StateMetrics{}
	for op := range emit.NumStateOps {
		for r := range emit.NumStateResults {
			h.Calls[op][r] = &m.calls[op][r]
		}
		h.Ops[op] = &m.ops[op]
		h.CallDuration[op] = &m.dur[op]
	}
	return h
}

// durations returns every duration observation (memory records none).
func (m *metrics) durations() uint64 {
	var t uint64
	for i := range m.dur {
		t += m.dur[i].n.Load()
	}
	return t
}

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

// attr returns the value of key in r.
func attr(r slog.Record, key string) string {
	var v string
	r.Attrs(func(a slog.Attr) bool {
		if a.Key == key {
			v = a.Value.String()
			return false
		}
		return true
	})
	return v
}

func newLogger(h slog.Handler) *slog.Logger { return slog.New(h) }

// closeDriver closes d, failing tb on an error.
func closeDriver(tb testing.TB, d *Driver) {
	tb.Helper()
	if err := d.Close(context.Background()); err != nil {
		tb.Errorf("Close: %v", err)
	}
}

// newRand returns a deterministic generator for property tests.
func newRand(a, b uint64) *rand.Rand {
	return rand.New(rand.NewPCG(a, b)) //nolint:gosec // reproducible test inputs, not secrets
}

// fixture is a driver on a fake clock with recorded metrics and logs.
type fixture struct {
	d    *Driver
	clk  *clocktest.Fake
	m    *metrics
	logs *logRecorder
}

func newFixture(t *testing.T, opts Options, mac []byte) *fixture {
	t.Helper()
	f := &fixture{clk: clocktest.New(start()), m: newMetrics(), logs: &logRecorder{}}
	d, err := New(context.Background(), statestore.Config{Driver: statestore.DriverMemory}, statestore.Deps{
		Clock: f.clk, Metrics: f.m.handles(), Logger: slog.New(f.logs), MACKey: mac,
	}, opts)
	if err != nil {
		t.Fatal(err)
	}
	f.d = d
	t.Cleanup(func() {
		if err := d.Close(context.Background()); err != nil {
			t.Errorf("Close: %v", err)
		}
		checkAccounting(t, d)
	})
	return f
}

// budget returns a fresh per-request budget on stripe s.
func budget(s emit.Stripe) *statestore.RequestBudget {
	rb := statestore.NewRequestBudget(time.Second, s)
	return &rb
}

func gcraCall(policy string, d statestore.Digest, limits ...statestore.GCRALimit) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpGCRA, Timeout: 50 * time.Millisecond, GCRA: statestore.GCRA{
		Policy: policy, Digest: d, Limits: limits, Out: make([]statestore.GCRAOutcome, len(limits)),
	}}
}

func quotaCall(name string, window time.Duration, limit int64, d statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpQuota, Timeout: 50 * time.Millisecond, Quota: statestore.Quota{
		Name: name, Window: window, Limit: limit, Digest: d,
	}}
}

func lookupCall(k statestore.CacheKey, v statestore.Digest) *statestore.Call {
	return &statestore.Call{Kind: statestore.OpCacheGet, Timeout: 50 * time.Millisecond, Lookup: statestore.CacheLookup{Key: k, Variant: v}}
}

func (f *fixture) consume(calls ...*statestore.Call) {
	f.d.Consume(context.Background(), budget(0), calls)
}

func (f *fixture) write(ws ...*statestore.Write) { f.d.Write(context.Background(), ws) }

func (f *fixture) store(k statestore.CacheKey, names string, v statestore.Digest, entry []byte, token uint64) *statestore.Write {
	w := &statestore.Write{Kind: statestore.OpCacheSet, Timeout: time.Second, Cache: statestore.CacheStore{
		Key: k, Names: names, Variant: v, TTL: time.Hour, Entry: entry, Token: token,
	}}
	f.write(w)
	return w
}

func (f *fixture) lookup(k statestore.CacheKey, v statestore.Digest) *statestore.Call {
	c := lookupCall(k, v)
	f.d.Read(context.Background(), budget(0), []*statestore.Call{c})
	return c
}
