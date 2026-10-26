// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// testNodeID is a ULID-shaped node.id.
const testNodeID = "01JABCDEFGHJKMNPQRSTVWXYZ0"

// syncBuffer is a stdout that tests read while the worker writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write implements io.Writer.
func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// lines returns the JSON lines written so far.
func (s *syncBuffer) lines(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	text := s.b.String()
	s.mu.Unlock()
	var out []map[string]any
	for l := range strings.SplitSeq(strings.TrimSpace(text), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line %q: %v", l, err)
		}
		out = append(out, m)
	}
	return out
}

// newRuntime returns a Runtime writing to a buffer, on a fake clock
// unless mutate sets another, shut down at cleanup.
func newRuntime(t *testing.T, mutate func(*Options)) (*Runtime, *syncBuffer) {
	t.Helper()
	out := &syncBuffer{}
	o := Options{
		Service: ServiceGateway, NodeID: testNodeID, Version: "0.0.0-test", Stdout: out, Level: slog.LevelInfo,
		Clock: clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	if mutate != nil {
		mutate(&o)
	}
	r, err := New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := r.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
	})
	return r, out
}

// flushLogs stops the stdout worker, so every queued line is written.
func flushLogs(t *testing.T, r *Runtime) {
	t.Helper()
	r.stopWorker()
	select {
	case <-r.workerDone:
	case <-time.After(10 * time.Second):
		t.Fatal("stdout worker did not stop")
	}
}

// scrapeText returns the body and content type of a /metrics scrape with
// header accept.
func scrapeText(t *testing.T, r *Runtime, accept string) (string, string) {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil)
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	rec := httptest.NewRecorder()
	r.MetricsHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String(), rec.Header().Get("Content-Type")
}

// scrape returns the parsed text exposition of /metrics.
func scrape(t *testing.T, r *Runtime) promtext.Set {
	t.Helper()
	body, _ := scrapeText(t, r, "")
	set, err := promtext.ParseBytes([]byte(body))
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	return set
}

// scrapeAfter scrapes once the fake clock has moved past the collection
// reuse window, so the scrape sees every change since the last one.
func scrapeAfter(t *testing.T, r *Runtime) promtext.Set {
	t.Helper()
	clk, ok := r.clk.(*clocktest.Fake)
	if !ok {
		t.Fatal("scrapeAfter needs the fake clock")
	}
	clk.Advance(2 * aggregate.ReuseWindow)
	return scrape(t, r)
}

// value returns the sample of name with labels, failing the test when
// absent.
func value(t *testing.T, set promtext.Set, name string, labels ...string) float64 {
	t.Helper()
	v, err := set.Value(name, promtext.L(labels...))
	if err != nil {
		t.Fatalf("%s%v: %v", name, labels, err)
	}
	return v
}

// fakeStore is a secret.Store over fixed values; rotations never happen.
type fakeStore map[secret.Ref]secret.Value

// Get implements secret.Store.
func (s fakeStore) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := s[r]
	return v, ok
}

// Watch implements secret.Store.
func (fakeStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// startSink starts an in-process collector, closed at cleanup after the
// Runtime's own cleanup (which needs it for the final export): its context
// is not the test's, which ends before any cleanup runs.
func startSink(t *testing.T, c otlpsink.Config) *otlpsink.Sink {
	t.Helper()
	s, err := otlpsink.Start(context.Background(), c)
	if err != nil {
		t.Fatalf("otlpsink: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// eventually polls cond until it holds or ctx ends.
func eventually(ctx context.Context, cond func() bool) bool {
	for !cond() {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(10 * time.Millisecond):
		}
	}
	return true
}

// waitCtx bounds a wait on an export.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	t.Cleanup(cancel)
	return ctx
}
