// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

func TestNewRequiresStdout(t *testing.T) {
	if _, err := New(Options{}); !errors.Is(err, ErrNoStdout) {
		t.Fatalf("New without Stdout: %v", err)
	}
	s, _ := newSink(t, Options{Level: slog.LevelWarn})
	if s.Level() != slog.LevelWarn {
		t.Fatalf("Level = %v", s.Level())
	}
}

// 09 req 61, 65: the queue is bounded in records; a full queue drops
// without blocking and counts queue_full; every record counts as produced.
func TestQueueRecordBound_Req61(t *testing.T) {
	s, out := newSink(t, Options{QueueRecords: 4})
	log := s.Logger("c")
	ctx := t.Context()
	for i := range 10 {
		log.InfoContext(ctx, "m", slog.Int("i", i))
	}
	if got := s.Stats(); got != (Stats{Produced: 10, QueueFull: 6}) {
		t.Fatalf("stats = %+v", got)
	}
	drain(s)
	if n := len(out.lines(t)); n != 4 {
		t.Fatalf("wrote %d lines, want 4", n)
	}
	if s.bytes.Load() != 0 {
		t.Fatalf("byte budget not released: %d", s.bytes.Load())
	}
}

// 09 req 61: the queue is bounded in bytes; a record that does not fit
// the remaining budget drops with queue_full.
func TestQueueByteBound_Req61(t *testing.T) {
	s, out := newSink(t, Options{QueueBytes: 3 * 1024})
	log := s.Logger("c")
	ctx := t.Context()
	big := strings.Repeat("x", 1000)
	for range 5 {
		log.InfoContext(ctx, "m", slog.String("v", big))
	}
	st := s.Stats()
	if st.Produced != 5 || st.QueueFull == 0 || st.QueueFull == 5 {
		t.Fatalf("stats = %+v, want some dropped by bytes", st)
	}
	drain(s)
	if n := uint64(len(out.lines(t))); n != st.Produced-st.QueueFull {
		t.Fatalf("wrote %d lines, stats %+v", n, st)
	}
}

// 09 req 62: the worker writes one batch when the queue drains and a
// batch whenever it reaches the batch size.
func TestBatching_Req62(t *testing.T) {
	s, out := newSink(t, Options{BatchBytes: 1024})
	log := s.Logger("c")
	ctx := t.Context()
	log.InfoContext(ctx, "one")
	log.InfoContext(ctx, "two")
	drain(s)
	if out.Writes() != 1 {
		t.Fatalf("writes = %d, want one batch", out.Writes())
	}
	for range 40 {
		log.InfoContext(ctx, "m", slog.String("pad", strings.Repeat("p", 100)))
	}
	drain(s)
	if w := out.Writes(); w < 4 {
		t.Fatalf("writes = %d, want batches of about 1 KiB", w)
	}
	if n := len(out.lines(t)); n != 42 {
		t.Fatalf("lines = %d", n)
	}
}

// 09 req 62, 65: a failed stdout write counts export_error once for each
// line it lost; the lines written before the failure are not counted.
func TestWriteErrorCountsLostLines_Req62(t *testing.T) {
	fw := &failWriter{}
	s, _ := newSink(t, Options{Stdout: fw})
	log := s.Logger("c")
	ctx := t.Context()
	for range 3 {
		log.InfoContext(ctx, "m")
	}
	// Let the first line and a half through.
	var probe syncBuffer
	ps, _ := newSink(t, Options{Stdout: &probe})
	ps.Logger("c").InfoContext(ctx, "m")
	drain(ps)
	lineLen := len(probe.String())
	fw.n = lineLen + lineLen/2
	drain(s)
	if st := s.Stats(); st.ExportError != 2 || st.QueueFull != 0 {
		t.Fatalf("stats = %+v, want 2 export errors", st)
	}
}

// ADR-0010 "Blocked stdout test", 09 req 61 and 70, WP-11 done-when: a
// stdout nobody reads never blocks the logging goroutine; records beyond
// the queue drop with queue_full; after the pipe unblocks the worker
// writes what it held and Run returns on cancel.
func TestBlockedStdout_Req61(t *testing.T) {
	bw := newBlockingWriter()
	s, _ := newSink(t, Options{Stdout: bw, QueueRecords: 64})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()

	log := s.Logger("c")
	log.InfoContext(ctx, "first")
	<-bw.entered // the worker is now blocked in Write

	start := time.Now()
	const n = 10_000
	for i := range n {
		log.InfoContext(ctx, "m", slog.Int("i", i))
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("logging %d records took %v with stdout blocked", n, d)
	}
	st := s.Stats()
	if st.Produced != n+1 || st.QueueFull < n-64 {
		t.Fatalf("stats = %+v, want at least %d queue_full drops", st, n-64)
	}

	close(bw.release)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not return after stdout unblocked")
	}
	lines := bw.out.lines(t)
	if uint64(len(lines)) != st.Produced-st.QueueFull {
		t.Fatalf("wrote %d lines, want %d", len(lines), st.Produced-st.QueueFull)
	}
	for _, l := range lines {
		object(t, l)
	}
	s.Close()
	log.InfoContext(t.Context(), "after close")
	if got := s.Stats().QueueFull; got != st.QueueFull+1 {
		t.Fatalf("record after Close not counted: %d", got)
	}
}

// Run drains what is queued when canceled; a second Run is refused;
// Close counts records still queued.
func TestRunLifecycle(t *testing.T) {
	s, out := newSink(t, Options{})
	log := s.Logger("c")
	for range 3 {
		log.InfoContext(t.Context(), "queued")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(out.lines(t)); n != 3 {
		t.Fatalf("final drain wrote %d lines", n)
	}

	s.running.Store(true)
	if err := s.Run(t.Context()); !errors.Is(err, ErrRunning) {
		t.Fatalf("second Run: %v", err)
	}
	s.running.Store(false)

	log.InfoContext(t.Context(), "left behind")
	log.InfoContext(t.Context(), "left behind")
	s.Close()
	s.Close()
	if st := s.Stats(); st.QueueFull != 2 || st.Produced != 5 {
		t.Fatalf("stats = %+v", st)
	}
	if s.bytes.Load() != 0 {
		t.Fatalf("byte budget not released: %d", s.bytes.Load())
	}
}

// Run serves records as they arrive.
func TestRunWritesAsRecordsArrive(t *testing.T) {
	s, out := newSink(t, Options{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	s.Logger("c").InfoContext(ctx, "live")
	deadline := time.Now().Add(5 * time.Second)
	for out.Writes() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if n := len(out.lines(t)); n != 1 {
		t.Fatalf("lines = %d", n)
	}
}

// fakeStream is an access Stream of fixed lines.
type fakeStream struct {
	mu      sync.Mutex
	lines   []string
	wake    chan struct{}
	handed  int
	flushed []int
}

func newFakeStream() *fakeStream { return &fakeStream{wake: make(chan struct{}, 1)} }

func (f *fakeStream) push(l string) {
	f.mu.Lock()
	f.lines = append(f.lines, l)
	f.mu.Unlock()
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

func (f *fakeStream) Wake() <-chan struct{} { return f.wake }

func (f *fakeStream) Pending() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lines)
}

func (f *fakeStream) Next(dst []byte) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lines) == 0 {
		return dst, false
	}
	l := f.lines[0]
	f.lines = f.lines[1:]
	f.handed++
	return append(append(dst, l...), '\n'), true
}

func (f *fakeStream) Flushed(lost int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushed = append(f.flushed, lost)
}

// ADR-0010 row Logs: one worker owns stdout for both streams; lines never
// interleave and a lost write is attributed to the stream that lost it.
func TestAccessStream_Req62(t *testing.T) {
	st := newFakeStream()
	s, out := newSink(t, Options{Access: st})
	log := s.Logger("c")
	ctx := t.Context()
	log.InfoContext(ctx, "p1")
	st.push(`{"msg":"access","n":1}`)
	st.push(`{"msg":"access","n":2}`)
	log.InfoContext(ctx, "p2")
	drain(s)
	lines := out.lines(t)
	if len(lines) != 4 {
		t.Fatalf("lines = %q", lines)
	}
	for _, l := range lines {
		object(t, l)
	}
	if !slices.Equal(st.flushed, []int{0}) {
		t.Fatalf("Flushed calls = %v", st.flushed)
	}

	// A failing write loses the batch: two process and one access line.
	fw := &failWriter{}
	st2 := newFakeStream()
	s2, _ := newSink(t, Options{Stdout: fw, Access: st2})
	s2.Logger("c").InfoContext(ctx, "p")
	st2.push(`{"msg":"access"}`)
	s2.Logger("c").InfoContext(ctx, "p")
	drain(s2)
	if got := s2.Stats().ExportError; got != 2 {
		t.Fatalf("process export errors = %d", got)
	}
	if !slices.Equal(st2.flushed, []int{1}) {
		t.Fatalf("Flushed = %v, want [1]", st2.flushed)
	}

	// The worker wakes for access lines alone and drains them on cancel.
	st3 := newFakeStream()
	s3, out3 := newSink(t, Options{Access: st3})
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s3.Run(runCtx) }()
	st3.push(`{"n":1}`)
	deadline := time.Now().Add(5 * time.Second)
	for out3.Writes() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	st3.push(`{"n":2}`)
	s3.final()
	if n := len(out3.lines(t)); n != 2 {
		t.Fatalf("lines = %d", n)
	}
}

// recordingBridge collects exported records.
type recordingBridge struct {
	mu  sync.Mutex
	got []Exported
	err error
}

func (b *recordingBridge) Export(r *Exported) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c := *r
	c.Record = r.Record.Clone()
	b.got = append(b.got, c)
	return b.err
}

func recordAttrs(r slog.Record) map[string]string {
	m := map[string]string{}
	var walk func(prefix string, a slog.Attr)
	walk = func(prefix string, a slog.Attr) {
		if a.Value.Kind() == slog.KindGroup {
			for _, c := range a.Value.Group() {
				walk(prefix+a.Key+".", c)
			}
			return
		}
		m[prefix+a.Key] = a.Value.String()
	}
	r.Attrs(func(a slog.Attr) bool {
		walk("", a)
		return true
	})
	return m
}

// 09 req 61, 62: after the stdout write each record goes once to the OTLP
// bridge, with framing members as attributes, groups applied, trace IDs
// separate and node_id left to the resource.
func TestBridge_Req61(t *testing.T) {
	s, _ := newSink(t, Options{NodeID: "node", TraceContext: traceFromContext})
	b := &recordingBridge{}
	s.SetBridge(b)
	s.SetRevision("rev-0123456789ab")
	ctx := withTrace(t.Context(), testIDs())
	h := s.Handler("gateway").WithAttrs([]slog.Attr{slog.String("policy", "p")}).WithGroup("g")
	slog.New(h).WarnContext(ctx, "m", slog.String(catalog.KeyCode, "RZ-RT-005"), slog.Int("n", 1),
		slog.String("authorization", "Bearer x"))
	slog.New(s.Handler("")).InfoContext(t.Context(), "top", slog.String(catalog.KeyCode, "RZ-RT-001"),
		slog.Group("", slog.String(catalog.KeyError, "e"), slog.Int("k", 2)))
	// Nested inline groups: slot keys at any depth are framing only, and
	// the other members are flattened, as on stdout.
	slog.New(s.Handler("")).InfoContext(t.Context(), "nested",
		slog.Group("", slog.Group("", slog.String(catalog.KeyCode, "RZ-RT-002"), slog.Int("k", 3)),
			slog.Group(catalog.KeyError, slog.String("kind", "x"))))
	drain(s)
	if len(b.got) != 3 {
		t.Fatalf("bridge got %d records", len(b.got))
	}
	first := b.got[0]
	if !first.HasTrace || first.TraceID != testIDs().trace || first.SpanID != testIDs().span || first.Dropped {
		t.Errorf("trace = %+v", first)
	}
	want := map[string]string{
		"component": "gateway", "revision": "rev-0123456789ab", "policy": "p",
		"g.code": "RZ-RT-005", "g.n": "1", "g.authorization": "[REDACTED]",
	}
	if got := recordAttrs(first.Record); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("attrs = %v\nwant    %v", got, want)
	}
	if first.Record.Message != "m" || first.Record.Level != slog.LevelWarn {
		t.Errorf("record = %v", first.Record)
	}
	want2 := map[string]string{"revision": "rev-0123456789ab", "code": "RZ-RT-001", "error": "e", "k": "2"}
	if got := recordAttrs(b.got[1].Record); fmt.Sprint(got) != fmt.Sprint(want2) {
		t.Errorf("attrs = %v\nwant    %v", got, want2)
	}
	want3 := map[string]string{"revision": "rev-0123456789ab", "code": "RZ-RT-002", "error.kind": "x", "k": "3"}
	if got := recordAttrs(b.got[2].Record); fmt.Sprint(got) != fmt.Sprint(want3) {
		t.Errorf("attrs = %v\nwant    %v", got, want3)
	}

	s.SetBridge(nil)
	s.Logger("c").InfoContext(t.Context(), "no bridge")
	drain(s)
	if len(b.got) != 3 {
		t.Fatal("bridge called after SetBridge(nil)")
	}
}

// 09 req 65: a record lost by stdout still reaches the bridge, flagged, and
// is counted once even when the bridge fails too; a bridge failure alone
// counts export_error.
func TestDropCountedOnce_Req65(t *testing.T) {
	b := &recordingBridge{err: errors.New("export failed")}
	s, _ := newSink(t, Options{Stdout: &failWriter{}})
	s.SetBridge(b)
	s.Logger("c").InfoContext(t.Context(), "m")
	drain(s)
	if st := s.Stats(); st.ExportError != 1 {
		t.Fatalf("stats = %+v, want exactly one export error", st)
	}
	if len(b.got) != 1 || !b.got[0].Dropped {
		t.Fatalf("bridge got %+v", b.got)
	}

	s2, _ := newSink(t, Options{})
	s2.SetBridge(b)
	s2.Logger("c").InfoContext(t.Context(), "m")
	drain(s2)
	if st := s2.Stats(); st.ExportError != 1 {
		t.Fatalf("stats = %+v, want the bridge failure counted", st)
	}
}

// 04 req 79: http.Server.ErrorLog is bridged through the sink with a rate
// limit.
func TestErrorLogBridge_Req79(t *testing.T) {
	s, out := newSink(t, Options{Clock: clocktest.New(time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC))})
	std := slog.NewLogLogger(s.ErrorLog("listener"), slog.LevelWarn)
	for range 50 {
		std.Print("http: TLS handshake error from 192.0.2.1:1234: EOF")
	}
	drain(s)
	lines := out.lines(t)
	if len(lines) != ErrorLogRate {
		t.Fatalf("wrote %d lines, want %d", len(lines), ErrorLogRate)
	}
	m := object(t, lines[0])
	if m["level"] != "WARN" || m["component"] != "listener" || m["msg"] != "http: TLS handshake error from 192.0.2.1:1234: EOF" {
		t.Fatalf("line = %s", lines[0])
	}
}

// 09 section 9 item 20, req 65: when the owner gives up on a worker
// blocked in its write, Close counts the queued records as queue_full and
// the lines of the blocked batch as export_error, once: the batch written
// later is neither counted again nor counted by the bridge.
func TestCloseCountsAbandonedBatch_Req65(t *testing.T) {
	bw := newBlockingWriter()
	s, _ := newSink(t, Options{Stdout: bw})
	b := &recordingBridge{err: errors.New("bridge failure")}
	s.SetBridge(b)
	log := s.Logger("c")
	ctx := t.Context()
	for range 3 {
		log.InfoContext(ctx, "in the batch")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- s.Run(runCtx) }()
	<-bw.entered // the worker holds the three lines in a blocked write
	log.InfoContext(ctx, "queued")
	log.InfoContext(ctx, "queued")
	s.Close()
	want := Stats{Produced: 5, QueueFull: 2, ExportError: 3}
	if st := s.Stats(); st != want {
		t.Fatalf("stats after Close = %+v, want %+v", st, want)
	}
	close(bw.release)
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if st := s.Stats(); st != want {
		t.Fatalf("stats after the write returned = %+v, want %+v", st, want)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.got) != 3 || !b.got[0].Dropped || !b.got[2].Dropped {
		t.Fatalf("bridge got %d records, want 3 flagged Dropped", len(b.got))
	}
	if s.bytes.Load() != 0 {
		t.Fatalf("byte budget not released: %d", s.bytes.Load())
	}
}

// 09 section 9 item 20, req 65: a Handle racing Close never leaves a
// record queued and uncounted.
func TestHandleRacingClose_Req65(t *testing.T) {
	for range 50 {
		s, _ := newSink(t, Options{})
		log := s.Logger("c")
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 50 {
					log.InfoContext(t.Context(), "m")
				}
			})
		}
		wg.Go(s.Close)
		wg.Wait()
		s.Close()
		st := s.Stats()
		if len(s.q) != 0 || st.QueueFull != st.Produced || s.bytes.Load() != 0 {
			t.Fatalf("queued %d, stats %+v, reserved %d", len(s.q), st, s.bytes.Load())
		}
	}
}
