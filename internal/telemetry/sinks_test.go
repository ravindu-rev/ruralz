// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// blockedStdout is a stdout nobody reads: every Write blocks until open.
type blockedStdout struct {
	open    chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newBlockedStdout() *blockedStdout {
	return &blockedStdout{open: make(chan struct{}), entered: make(chan struct{})}
}

func (b *blockedStdout) Write(p []byte) (int, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.open
	return len(p), nil
}

// submitAccess queues one access record for route.
func submitAccess(r *Runtime, route string) {
	rec := r.AccessLog().Acquire()
	rec.Start, rec.Method, rec.Path, rec.Status, rec.Route = r.clk.Now(), "GET", "/"+route, 200, route
	r.AccessLog().Submit(rec)
}

// dropped returns ruralz_telemetry_logs_dropped_total for stream, sink and
// reason.
func dropped(t *testing.T, r *Runtime, stream, sink, reason string) float64 {
	t.Helper()
	return value(t, scrapeAfter(t, r), catalog.TelemetryLogsDroppedTotal, "stream", stream, "sink", sink, "reason", reason)
}

// TestBlockedStdoutKeepsOTLPLogs checks that log records reach stdout and
// the OTLP log queue independently: with stdout blocked, both streams drop
// from stdout with sink stdout and reason queue_full, while every record
// still reaches the collector and the OTLP sink loses none.
func TestBlockedStdoutKeepsOTLPLogs(t *testing.T) {
	sink := startSink(t, otlpsink.Config{})
	stdout := newBlockedStdout()
	r, _ := newRuntime(t, func(o *Options) { o.Stdout = stdout })
	// Cleanups run last first: stdout opens before the runtime shuts down.
	t.Cleanup(func() { close(stdout.open) })
	applyEndpoint(t, r, cleartext(sink), nil)
	r.Logger("probe").Info("first")
	<-stdout.entered // the stdout worker is now blocked in its write

	// Past the process queue's 4,096 records and 1 MiB, within the access
	// queue's 8,192 records and 4 MiB.
	const n = 6000
	for i := range n {
		r.Logger("probe").Info("blocked")
		submitAccess(r, "orders")
		if i%512 == 0 {
			// Keep the OTLP queue short, as its worker would under load.
			if err := r.pipe.lp.ForceFlush(waitCtx(t)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := r.pipe.lp.ForceFlush(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	var access, process int
	for _, l := range sink.Logs() {
		switch {
		case l.EventName() == "ruralz.access":
			access++
		case l.Body() == "blocked":
			process++
		}
	}
	if access != n || process != n {
		t.Fatalf("collector got %d access and %d process records, want %d each while stdout was blocked", access, process, n)
	}
	if v := dropped(t, r, "process", "stdout", "queue_full"); v < n-4096 {
		t.Errorf("process stdout queue_full = %v, want at least %d", v, n-4096)
	}
	if v := dropped(t, r, "access", "stdout", "queue_full"); v != 0 {
		// 6,000 access records fit the 8,192-record queue; the byte
		// budget holds them too.
		t.Errorf("access stdout queue_full = %v, want 0 below the access queue's bounds", v)
	}
	for _, stream := range []string{"access", "process"} {
		for _, reason := range []string{"queue_full", "export_error"} {
			if v := dropped(t, r, stream, "otlp", reason); v != 0 {
				t.Errorf("%s otlp %s = %v, want no OTLP loss", stream, reason, v)
			}
		}
	}
	for range 3000 {
		submitAccess(r, "more")
	}
	if v := dropped(t, r, "access", "stdout", "queue_full"); v == 0 {
		t.Error("access stdout queue_full = 0 past the 8,192-record queue")
	}
}

// TestOTLPLossCountsOnlyTheOTLPSink checks that a record the collector
// refuses counts once under sink otlp and reason export_error for its
// stream, and stdout, which wrote it, counts nothing.
func TestOTLPLossCountsOnlyTheOTLPSink(t *testing.T) {
	sink := startSink(t, otlpsink.Config{Mode: otlpsink.Reject{Code: codes.InvalidArgument}})
	r, _ := newRuntime(t, nil)
	applyEndpoint(t, r, cleartext(sink), nil)
	for range 3 {
		submitAccess(r, "orders")
	}
	r.Logger("probe").Info("refused")
	if err := r.pipe.lp.ForceFlush(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	flushLogs(t, r)
	if v := dropped(t, r, "access", "otlp", "export_error"); v != 3 {
		t.Errorf("access otlp export_error = %v, want 3", v)
	}
	if v := dropped(t, r, "process", "otlp", "export_error"); v < 1 {
		t.Errorf("process otlp export_error = %v, want at least 1", v)
	}
	for _, stream := range []string{"access", "process"} {
		for _, reason := range []string{"queue_full", "export_error"} {
			if v := dropped(t, r, stream, "stdout", reason); v != 0 {
				t.Errorf("%s stdout %s = %v, want 0: stdout wrote every line", stream, reason, v)
			}
		}
	}
	// Let the final exports through.
	if err := sink.SetMode(otlpsink.Accept{}); err != nil {
		t.Fatal(err)
	}
}

// TestOTLPQueueFullCountsOnlyTheOTLPSink checks the OTLP log queue's
// record bound: with the collector stalled, the first batch of 512 in
// flight and 8,192 records queued, every later access record drops with
// sink otlp and reason queue_full, counted once.
func TestOTLPQueueFullCountsOnlyTheOTLPSink(t *testing.T) {
	sink := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	applyEndpoint(t, r, cleartext(sink), nil)
	// The cleartext hop's WARN record leaves before the collector stalls,
	// so the first batch holds access records only. The export bound runs
	// on the fake clock, which stays put: that batch stays stalled.
	if err := r.pipe.lp.ForceFlush(waitCtx(t)); err != nil {
		t.Fatal(err)
	}
	if err := sink.SetMode(otlpsink.Stall{}); err != nil {
		t.Fatal(err)
	}
	const extra = 100
	for range logQueueRecords + logBatch + extra {
		submitAccess(r, "orders")
	}
	if got := dropped(t, r, "access", "otlp", "queue_full"); got != extra {
		t.Errorf("access otlp queue_full = %v, want %d", got, extra)
	}
	if err := sink.SetMode(otlpsink.Accept{}); err != nil {
		t.Fatal(err)
	}
}

// TestScrapeWaitFollowsTheClock checks that a scrape waiting for a slot
// gives up after 10 s of the runtime's clock, a Node timer the
// controllable clock drives, not of real time.
func TestScrapeWaitFollowsTheClock(t *testing.T) {
	r, _ := newRuntime(t, nil)
	clk := r.clk.(*clocktest.Fake)
	h := r.metrics
	for range scrapeSlots {
		h.slots <- struct{}{}
	}
	t.Cleanup(func() {
		for range scrapeSlots {
			<-h.slots
		}
	})
	armed := clk.Pending()
	done := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/metrics", nil))
		done <- rec.Code
	}()
	waitTimers(t, clk, armed+1)
	clk.Advance(scrapeWait - time.Nanosecond)
	select {
	case code := <-done:
		t.Fatalf("the waiting scrape answered %d before 10 s of the clock", code)
	case <-time.After(50 * time.Millisecond):
	}
	clk.Advance(time.Nanosecond)
	select {
	case code := <-done:
		if code != http.StatusServiceUnavailable {
			t.Errorf("the fifth scrape = %d after its wait, want 503", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the fifth scrape did not end at 10 s of the clock")
	}
}

// waitTimers waits until at least n timers are armed on clk.
func waitTimers(t *testing.T, clk *clocktest.Fake, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for clk.Pending() < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d timers armed, want %d", clk.Pending(), n)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestMetricPushFollowsTheClock checks that metrics are pushed every
// interval of the runtime's clock, a Node timer the controllable clock
// drives, and only then.
func TestMetricPushFollowsTheClock(t *testing.T) {
	sink := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	clk := r.clk.(*clocktest.Fake)
	applyEndpoint(t, r, cleartext(sink), nil)
	pushes := func() int64 { return sink.Stats().MetricRequests }
	clk.Advance(metricInterval - time.Nanosecond)
	select {
	case <-time.After(100 * time.Millisecond):
	case <-pollUntil(func() bool { return pushes() > 0 }):
		t.Fatal("a metric push before one interval of the clock")
	}
	clk.Advance(time.Nanosecond)
	if !eventually(waitCtx(t), func() bool { return pushes() == 1 }) {
		t.Fatalf("%d pushes after one interval, want 1", pushes())
	}
	clk.Advance(metricInterval)
	if !eventually(waitCtx(t), func() bool { return pushes() == 2 }) {
		t.Fatalf("%d pushes after two intervals, want 2", pushes())
	}
}

// pollUntil returns a channel closed once cond holds, polling for at most
// 10 s.
func pollUntil(cond func() bool) <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if eventually(ctx, cond) {
			close(ch)
		}
	}()
	return ch
}

// TestExportRetryFollowsTheClock checks the export bounds on the runtime's
// clock: a retryable failure retries 1 s, then 2 s and 4 s later, within
// the 10 s retry window; a non-retryable one fails at once; the export as
// a whole ends at the 10 s bound.
func TestExportRetryFollowsTheClock(t *testing.T) {
	clk := clocktest.New(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	x := exportRetry{clk: clk, b: defaultExportBounds()}
	unavailable := statusError(codes.Unavailable)
	var mu sync.Mutex
	var at []time.Duration
	start := clk.Now()
	done := make(chan error, 1)
	go func() {
		done <- x.run(context.Background(), func(context.Context) error {
			mu.Lock()
			at = append(at, clk.Since(start))
			mu.Unlock()
			return unavailable
		})
	}()
	attempts := func() int {
		mu.Lock()
		defer mu.Unlock()
		return len(at)
	}
	if !eventually(waitCtx(t), func() bool { return attempts() == 1 }) {
		t.Fatal("no first attempt")
	}
	for i, step := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		waitTimers(t, clk, 2) // the export bound and the backoff
		clk.Advance(step)
		if !eventually(waitCtx(t), func() bool { return attempts() == i+2 }) {
			t.Fatalf("%d attempts after a %v backoff, want %d", attempts(), step, i+2)
		}
	}
	// The next backoff, 5 s, would start past the 10 s window.
	if err := <-done; err == nil {
		t.Fatal("the export succeeded")
	}
	mu.Lock()
	if want := []time.Duration{0, time.Second, 3 * time.Second, 7 * time.Second}; len(at) != len(want) {
		t.Errorf("attempts at %v, want %v", at, want)
	} else {
		for i := range want {
			if at[i] != want[i] {
				t.Errorf("attempts at %v, want %v", at, want)
				break
			}
		}
	}
	mu.Unlock()

	calls := 0
	if err := x.run(context.Background(), func(context.Context) error { calls++; return statusError(codes.InvalidArgument) }); err == nil || calls != 1 {
		t.Errorf("a non-retryable failure: %d calls, err %v; want 1 call and the error", calls, err)
	}

	stalled := make(chan error, 1)
	go func() {
		stalled <- x.run(context.Background(), func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	waitTimers(t, clk, 1)
	clk.Advance(exportTimeout - time.Nanosecond)
	select {
	case err := <-stalled:
		t.Fatalf("a stalled export ended before its bound on the clock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	clk.Advance(time.Nanosecond)
	select {
	case err := <-stalled:
		if err == nil {
			t.Error("a stalled export succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a stalled export outlived its 10 s bound on the clock")
	}
}

// statusError is a gRPC status error with code c.
func statusError(c codes.Code) error { return status.Error(c, "test") }

// TestRetireBoundFollowsTheClock checks that a replaced pipeline whose
// collector stalls is cut off 5 s later on the runtime's clock, so its
// connection closes then, never on real time.
func TestRetireBoundFollowsTheClock(t *testing.T) {
	old := startSink(t, otlpsink.Config{Mode: otlpsink.Stall{}})
	next := startSink(t, otlpsink.Config{})
	r, _ := newRuntime(t, nil)
	clk := r.clk.(*clocktest.Fake)
	applyEndpoint(t, r, cleartext(old), nil)
	// The retirement's final metric push dials the old collector, which
	// holds it.
	applyEndpoint(t, r, cleartext(next), nil)
	if !eventually(waitCtx(t), func() bool { return old.Stats().Stalled > 0 }) {
		t.Fatalf("the retirement's final export never reached the old collector: %+v", old.Stats())
	}
	clk.Advance(retireTimeout - time.Millisecond)
	select {
	case <-time.After(100 * time.Millisecond):
	case <-pollUntil(func() bool { return old.Stats().Stalled == 0 }):
		t.Fatal("the retirement ended before 5 s of the clock")
	}
	clk.Advance(time.Millisecond)
	if !eventually(waitCtx(t), func() bool { return old.Stats().OpenConnections == 0 }) {
		t.Fatalf("the old connection stayed open past the 5 s bound on the clock: %+v", old.Stats())
	}
}
