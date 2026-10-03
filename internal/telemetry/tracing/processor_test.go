// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// procFixture is a Processor with counters and a recording exporter.
type procFixture struct {
	p    *Processor
	c    testCounters
	exp  *recordExporter
	fake *clocktest.Fake
	mu   sync.Mutex
	errs []error
}

func newProcFixture(t *testing.T, o ProcessorOptions) *procFixture {
	t.Helper()
	f := &procFixture{exp: newRecordExporter(), fake: clocktest.New(time.Unix(1_700_000_000, 0))}
	o.Clock = f.fake
	o.Spans, o.DroppedQueueFull, o.DroppedExportError = &f.c.spans, &f.c.full, &f.c.export
	o.OnExport = func(_ int, err error) {
		f.mu.Lock()
		f.errs = append(f.errs, err)
		f.mu.Unlock()
	}
	f.p = NewProcessor(o)
	f.p.SetExporter(f.exp)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = f.p.Shutdown(ctx)
	})
	return f
}

// ended returns n distinct ended spans named name.
func ended(n int, name string) []sdktrace.ReadOnlySpan {
	out := make([]sdktrace.ReadOnlySpan, n)
	for i := range out {
		out[i] = tracetest.SpanStub{Name: name}.Snapshot()
	}
	return out
}

func (f *procFixture) end(spans []sdktrace.ReadOnlySpan) {
	for _, s := range spans {
		f.p.OnEnd(s)
	}
}

// tick advances the fake clock by d once the worker's batch timer is
// armed (the worker re-arms it after each tick).
func (f *procFixture) tick(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for f.fake.Pending() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("batch timer never re-armed")
		}
		time.Sleep(time.Millisecond)
	}
	f.fake.Advance(d)
}

func (f *procFixture) flush(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.p.ForceFlush(ctx); err != nil {
		t.Fatalf("ForceFlush: %v", err)
	}
}

// TestProcessorBatchSize covers req 20: a full batch is exported at once
// without waiting for the interval.
func TestProcessorBatchSize(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 4, QueueSize: 64})
	f.end(ended(9, "s"))
	if n := f.exp.waitExported(t); n != 4 {
		t.Fatalf("first batch %d, want 4", n)
	}
	if n := f.exp.waitExported(t); n != 4 {
		t.Fatalf("second batch %d, want 4", n)
	}
	select {
	case n := <-f.exp.exported:
		t.Fatalf("partial batch of %d exported before the interval", n)
	case <-time.After(50 * time.Millisecond):
	}
	f.tick(t, DefaultBatchInterval)
	if n := f.exp.waitExported(t); n != 1 {
		t.Fatalf("interval batch %d, want 1", n)
	}
	if got := f.c.spans.Load(); got != 9 {
		t.Fatalf("spans_total = %d", got)
	}
}

// TestProcessorInterval covers req 20: queued spans leave every interval.
func TestProcessorInterval(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 512, BatchInterval: time.Second})
	f.end(ended(3, "a"))
	f.tick(t, time.Second)
	if n := f.exp.waitExported(t); n != 3 {
		t.Fatalf("batch %d, want 3", n)
	}
	f.end(ended(2, "b"))
	f.tick(t, time.Second)
	if n := f.exp.waitExported(t); n != 2 {
		t.Fatalf("batch %d, want 2", n)
	}
	_, names := f.exp.got()
	if !slices.Equal(names, []string{"a", "a", "a", "b", "b"}) {
		t.Fatalf("export order %v", names)
	}
}

// TestProcessorQueueFull covers req 20: a full queue drops with
// queue_full and never blocks OnEnd.
func TestProcessorQueueFull(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{QueueSize: 8, BatchSize: 512})
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.end(ended(20, "s"))
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnEnd blocked on a full queue")
	}
	if got := f.c.full.Load(); got != 12 {
		t.Fatalf("queue_full = %d, want 12", got)
	}
	if got := f.c.spans.Load(); got != 20 {
		t.Fatalf("spans_total = %d, want 20", got)
	}
	f.flush(t)
	if b, _ := f.exp.got(); !slices.Equal(b, []int{8}) {
		t.Fatalf("batches %v", b)
	}
}

// TestProcessorExportError covers reqs 20 and 23: a failed batch counts
// export_error per span and reaches OnExport.
func TestProcessorExportError(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 3})
	f.exp.fail.Store(true)
	f.end(ended(3, "s"))
	f.exp.waitExported(t)
	f.flush(t)
	if got := f.c.export.Load(); got != 3 {
		t.Fatalf("export_error = %d, want 3", got)
	}
	f.exp.fail.Store(false)
	f.end(ended(2, "s"))
	f.flush(t)
	if got := f.c.export.Load(); got != 3 {
		t.Fatalf("export_error after recovery = %d", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) != 2 || !errors.Is(f.errs[0], errExport) || f.errs[1] != nil {
		t.Fatalf("OnExport results %v", f.errs)
	}
}

// TestProcessorNoExporter: spans ended while no exporter is set (the
// endpoint was removed) are dropped as export_error, and OnExport is not
// told (no export failed).
func TestProcessorNoExporter(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{})
	if f.p.SetExporter(nil) != f.exp || f.p.Exporter() != nil {
		t.Fatal("SetExporter(nil)")
	}
	f.end(ended(5, "s"))
	f.flush(t)
	if got := f.c.export.Load(); got != 5 {
		t.Fatalf("export_error = %d, want 5", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) != 0 {
		t.Fatalf("OnExport called %v", f.errs)
	}
}

// TestProcessorExportTimeout: one export is bounded by ExportTimeout.
func TestProcessorExportTimeout(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 1, ExportTimeout: 20 * time.Millisecond})
	f.exp.block = make(chan struct{})
	f.end(ended(1, "s"))
	f.flush(t)
	if got := f.c.export.Load(); got != 1 {
		t.Fatalf("export_error = %d, want 1", got)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) != 1 || !errors.Is(f.errs[0], context.DeadlineExceeded) {
		t.Fatalf("OnExport %v", f.errs)
	}
}

// TestProcessorShutdown covers req 25: Shutdown exports what is queued,
// shuts the exporter down, refuses later spans with queue_full, and a
// second call is a no-op.
func TestProcessorShutdown(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 4})
	if f.p.Exporter() != f.exp {
		t.Fatal("Exporter")
	}
	f.end(ended(6, "s"))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := f.p.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if b, _ := f.exp.got(); !slices.Equal(b, []int{4, 2}) {
		t.Fatalf("batches %v", b)
	}
	if f.exp.shut.Load() != 1 || f.p.Exporter() != nil {
		t.Fatal("exporter not shut down")
	}
	f.end(ended(1, "late"))
	if got := f.c.full.Load(); got != 1 {
		t.Fatalf("late span queue_full = %d", got)
	}
	if err := f.p.Shutdown(ctx); err != nil {
		t.Fatalf("second Shutdown: %v", err)
	}
	if err := f.p.ForceFlush(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("ForceFlush after Shutdown = %v", err)
	}
}

// TestProcessorShutdownDeadline covers req 25: when the flush cannot
// finish within ctx, Shutdown returns at the deadline and the spans left
// are dropped and counted.
func TestProcessorShutdownDeadline(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 2, QueueSize: 16})
	f.exp.block = make(chan struct{}) // a stalled collector
	f.end(ended(7, "s"))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := f.p.Shutdown(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Shutdown = %v, want deadline exceeded", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("Shutdown did not return at its deadline")
	}
	select {
	case <-f.p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not stop")
	}
	if got := f.c.export.Load(); got != 7 {
		t.Fatalf("export_error = %d, want 7 (every span lost)", got)
	}
}

// TestProcessorForceFlushDeadline: a flush that runs out of time keeps
// the queued spans for later batches.
func TestProcessorForceFlushDeadline(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 1, QueueSize: 16})
	f.exp.block = make(chan struct{})
	f.end(ended(3, "s"))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := f.p.ForceFlush(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("ForceFlush = %v", err)
	}
	close(f.exp.block)
	f.flush(t)
	b, _ := f.exp.got()
	if got := f.c.export.Load(); uint64(len(b))+got != 3 {
		t.Fatalf("exported %v, dropped %d: spans lost", b, got)
	}
}

// TestProcessorConcurrentOnEnd runs OnEnd from many goroutines against a
// small queue (with -race): every span is either exported or counted.
func TestProcessorConcurrentOnEnd(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 16, QueueSize: 64})
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { f.end(ended(500, "s")) })
	}
	wg.Wait()
	f.flush(t)
	b, _ := f.exp.got()
	exported := 0
	for _, n := range b {
		exported += n
	}
	if total := uint64(exported) + f.c.full.Load(); total != 4000 || f.c.spans.Load() != 4000 {
		t.Fatalf("exported %d + queue_full %d != 4000 (spans_total %d)", exported, f.c.full.Load(), f.c.spans.Load())
	}
}

// TestProcessorShutdownDoneContext covers spec 09 req 25: a context that
// is already done still stops the worker before Shutdown returns, drops
// and counts every queued span, and shuts the exporter down.
func TestProcessorShutdownDoneContext(t *testing.T) {
	f := newProcFixture(t, ProcessorOptions{BatchSize: 512, QueueSize: 16})
	f.end(ended(5, "s"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := f.p.Shutdown(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown = %v, want context canceled", err)
	}
	select {
	case <-f.p.done:
	default:
		t.Fatal("worker still running after Shutdown returned")
	}
	b, _ := f.exp.got()
	exported := 0
	for _, n := range b {
		exported += n
	}
	if got := uint64(exported) + f.c.export.Load(); got != 5 {
		t.Fatalf("exported %d + export_error %d != 5", exported, f.c.export.Load())
	}
	if f.exp.shut.Load() != 1 || len(f.p.queue) != 0 {
		t.Fatalf("exporter shut %d times, %d spans left", f.exp.shut.Load(), len(f.p.queue))
	}
}

// TestProcessorShutdownRacingOnEnd covers reqs 20 and 25: OnEnd calls
// racing Shutdown leave no span uncounted, so spans_total equals exported
// plus queue_full plus export_error once Shutdown returns and the callers
// are done.
func TestProcessorShutdownRacingOnEnd(t *testing.T) {
	iterations := 400
	if testing.Short() {
		iterations = 50
	}
	for iter := range iterations {
		var c testCounters
		exp := newRecordExporter()
		p := NewProcessor(ProcessorOptions{
			Spans: &c.spans, DroppedQueueFull: &c.full, DroppedExportError: &c.export,
			BatchSize: 4, QueueSize: 64,
		})
		p.SetExporter(exp)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for range 4 {
			wg.Go(func() {
				<-start
				for _, s := range ended(50, "s") {
					p.OnEnd(s)
				}
			})
		}
		close(start)
		time.Sleep(time.Duration(iter%5) * 10 * time.Microsecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := p.Shutdown(ctx)
		cancel()
		wg.Wait()
		if err != nil {
			t.Fatalf("iteration %d: Shutdown = %v", iter, err)
		}
		b, _ := exp.got()
		exported := 0
		for _, n := range b {
			exported += n
		}
		if total := uint64(exported) + c.full.Load() + c.export.Load(); total != c.spans.Load() || c.spans.Load() != 200 {
			t.Fatalf("iteration %d: spans_total %d, exported %d + queue_full %d + export_error %d = %d, %d left queued",
				iter, c.spans.Load(), exported, c.full.Load(), c.export.Load(), total, len(p.queue))
		}
	}
}

// stripeCounter records the stripes it is called on.
type stripeCounter struct{ hits [256]atomic.Uint64 }

func (c *stripeCounter) Add(s emit.Stripe, n uint64) { c.hits[s].Add(n) }

// used returns the stripes that were counted on.
func (c *stripeCounter) used() []int {
	var out []int
	for i := range c.hits {
		if c.hits[i].Load() > 0 {
			out = append(out, i)
		}
	}
	return out
}

// TestStripes: the per-span and per-decision counters spread over the
// eight stripes by a random ID instead of all hitting stripe 0.
func TestStripes(t *testing.T) {
	var spans, root stripeCounter
	fake := clocktest.New(time.Unix(1_700_000_000, 0))
	tr := newTracer(t, Options{Clock: fake, RootPerSecond: 1, Counters: Counters{Spans: &spans, UnsampledRoot: &root}})
	tr.SetExporter(discardExporter{})
	for range 200 {
		var d emit.Decision
		tr.Decide(nil, 1, &d)
		_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
		s.End(http.StatusOK, "", "")
		if !d.Sampled {
			continue
		}
		// The one admitted decision: force more spans through.
		for range 199 {
			d.Sampled = true
			tr.src.spanID(&d.ServerSpanID)
			_, s := tr.StartServer(context.Background(), &d, http.MethodGet, emit.ServerAttrs{})
			s.End(http.StatusOK, "", "")
		}
	}
	for name, c := range map[string]*stripeCounter{"spans_total": &spans, "rate_cap_root": &root} {
		used := c.used()
		if len(used) < 4 || used[len(used)-1] > 7 {
			t.Errorf("%s stripes %v, want several of 0 to 7", name, used)
		}
	}
}
