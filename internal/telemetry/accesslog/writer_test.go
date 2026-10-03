// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// drainLines takes every queued line through the Stream methods, as the
// logsink worker does, reporting lost of them as not written.
func drainLines(w *Writer, lost int) []string {
	var buf []byte
	for {
		b, ok := w.Next(buf)
		if !ok {
			break
		}
		buf = b
	}
	w.Flushed(lost)
	s := strings.TrimSuffix(string(buf), "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// emit.AccessLog contract: Acquire returns a reset record; Submit copies
// it and returns it to the pool; a reused record carries nothing over.
func TestAcquireSubmit_Req67(t *testing.T) {
	w := New(Options{NodeID: testNode})
	r := w.Acquire()
	fullRecord(r)
	w.Submit(r)
	for range 10 {
		r2 := w.Acquire()
		if r2.Route != "" || r2.Status != 0 || r2.FailureModes != nil || r2.Sampled {
			t.Fatalf("Acquire returned a dirty record: %+v", r2)
		}
		r2.Route = "second"
		r2.Status = 200
		w.Submit(r2)
	}
	lines := drainLines(w, 0)
	if len(lines) != 11 {
		t.Fatalf("got %d lines", len(lines))
	}
	want := &emit.AccessRecord{}
	fullRecord(want)
	if lines[0] != encode(want) {
		t.Fatalf("line 0 = %s\nwant     %s", lines[0], encode(want))
	}
	for _, l := range lines[1:] {
		m := object(t, l)
		if m["route"] != "second" || m["consumer"] != nil || m["failure_modes"] != nil {
			t.Fatalf("data carried over between records: %s", l)
		}
	}
	if st := w.Stats(); st != (Stats{Produced: 11}) {
		t.Fatalf("stats = %+v", st)
	}
	if w.bytes.Load() != 0 {
		t.Fatalf("byte budget not released: %d", w.bytes.Load())
	}
}

// 09 req 67: a queued record owns copies of its strings; it holds no
// reference to the caller's memory, and the caller's record is reset.
func TestRecordCopiesStrings_Req67(t *testing.T) {
	w := New(Options{})
	r := w.Acquire()
	buf := []byte("/orders/1")
	r.Path = string(buf)
	r.FailureModes = append(r.FailureModes, emit.FailureModeEntry{Policy: "p", Phase: phase.OnRoute, Mode: "open"})
	w.Submit(r)
	if r.Path != "" || len(r.FailureModes) != 0 {
		t.Fatalf("submitted record not reset: %+v", r)
	}
	e := <-w.q
	if got := string(e.str(e.spans[fieldPath])); got != "/orders/1" {
		t.Fatalf("path = %q", got)
	}
	if cap(e.arena) != classSize(0) || e.class != 0 {
		t.Fatalf("arena class %d cap %d", e.class, cap(e.arena))
	}
	w.release(e)
}

// Arena classes: an entry takes the smallest class holding its strings.
func TestArenaClasses_Req67(t *testing.T) {
	tests := []struct {
		n, class int
	}{{0, 0}, {256, 0}, {257, 1}, {512, 1}, {513, 2}, {1024, 2}, {2049, 4}, {4096, 4}, {9000, 4}}
	for _, tt := range tests {
		if got := classFor(tt.n); got != tt.class {
			t.Errorf("classFor(%d) = %d, want %d", tt.n, got, tt.class)
		}
	}
	r := &emit.AccessRecord{Path: strings.Repeat("p", 5000), Host: strings.Repeat("h", 5000)}
	if got := need(r); got != MaxPathBytes+MaxHostBytes {
		t.Errorf("need = %d", got)
	}
	r.Route = strings.Repeat("r", 5000)
	if got := need(r); got != MaxRecordBytes {
		t.Errorf("need = %d, want the record cap", got)
	}
}

// 09 req 67, 65: the queue is bounded in records; a full queue drops with
// queue_full and never blocks.
func TestQueueRecordBound_Req67(t *testing.T) {
	w := New(Options{QueueRecords: 3})
	for range 5 {
		r := w.Acquire()
		r.Route = "r"
		w.Submit(r)
	}
	if st := w.Stats(); st != (Stats{Produced: 5, QueueFull: 2}) {
		t.Fatalf("stats = %+v", st)
	}
	if w.Pending() != 3 {
		t.Fatalf("pending = %d", w.Pending())
	}
	if n := len(drainLines(w, 0)); n != 3 {
		t.Fatalf("lines = %d", n)
	}
}

// 09 req 67: each record reserves its actual size (copied bytes plus the
// fixed overhead) from the byte budget until it is written; an exhausted
// budget drops with queue_full.
func TestQueueByteBound_Req67(t *testing.T) {
	w := New(Options{QueueBytes: 3 * (entryOverhead + 600)})
	for range 4 {
		r := w.Acquire()
		r.Path = strings.Repeat("p", 600) + "?q=not-copied"
		w.Submit(r)
	}
	if st := w.Stats(); st.QueueFull != 1 || w.Pending() != 3 {
		t.Fatalf("stats = %+v pending %d", st, w.Pending())
	}
	if got := w.bytes.Load(); got != 3*(entryOverhead+600) {
		t.Fatalf("reserved %d", got)
	}
	drainLines(w, 0)
	if got := w.bytes.Load(); got != 0 {
		t.Fatalf("reservation not released: %d", got)
	}

	// A value cut by the escaped-size budget reserves what was copied.
	w2 := New(Options{})
	r := w2.Acquire()
	r.Route = strings.Repeat("\x01", MaxRecordBytes) // 6 escaped bytes each
	w2.Submit(r)
	if got, want := w2.bytes.Load(), int64(entryOverhead+MaxRecordBytes/6); got != want {
		t.Fatalf("reserved %d, want %d", got, want)
	}
	drainLines(w2, 0)
}

// 09 req 67 at the default options: typical records reach the 8,192-record
// bound before the 4 MiB budget; records at the 4 KiB cap hit the budget.
func TestQueueDepthAtDefaults_Req67(t *testing.T) {
	w := New(Options{})
	for range DefaultQueueRecords + 1 {
		r := w.Acquire()
		fullRecord(r)
		w.Submit(r)
	}
	if st := w.Stats(); st.QueueFull != 1 || w.Pending() != DefaultQueueRecords {
		t.Fatalf("stats = %+v pending %d, want the record bound reached", st, w.Pending())
	}
	if got := w.bytes.Load(); got > DefaultQueueBytes {
		t.Fatalf("reserved %d over the budget", got)
	}
	w.Close()

	big := New(Options{})
	const perRecord = entryOverhead + MaxRecordBytes
	for range DefaultQueueBytes/perRecord + 1 {
		r := big.Acquire()
		r.Route = strings.Repeat("r", 2*MaxRecordBytes)
		big.Submit(r)
	}
	if st := big.Stats(); st.QueueFull != 1 || big.Pending() != DefaultQueueBytes/perRecord {
		t.Fatalf("stats = %+v pending %d, want the byte bound reached", st, big.Pending())
	}
	big.Close()
}

// 09 req 67: a caller may assign its own FailureModes slice; the pooled
// record never keeps it, so a later Acquire cannot write into it.
func TestSubmitKeepsNoCallerSlice_Req67(t *testing.T) {
	w := New(Options{})
	mine := make([]emit.FailureModeEntry, 0, MaxFailureModes)
	r := w.Acquire()
	mine = append(mine, emit.FailureModeEntry{Policy: "p", Phase: phase.OnRoute, Mode: "open"})
	r.FailureModes = mine
	w.Submit(r)
	for range 10 {
		r2 := w.Acquire()
		if r2.FailureModes != nil {
			t.Fatalf("Acquire returned a record holding a caller slice: %v", r2.FailureModes)
		}
		r2.FailureModes = append(r2.FailureModes, emit.FailureModeEntry{Policy: "other"})
		w.Submit(r2)
	}
	if got := mine[0].Policy; got != "p" {
		t.Fatalf("caller slice overwritten: %q", got)
	}
	if lines := drainLines(w, 0); len(lines) != 11 || !strings.Contains(lines[0], `"policy":"p"`) {
		t.Fatalf("lines = %q", lines)
	}
}

// 09 section 9 item 20, req 65: Close counts the lines a worker blocked in
// its write still holds as export_error, once; when the worker later
// settles them they are not counted again and reach the bridge flagged.
func TestCloseCountsUnflushedLines_Req65(t *testing.T) {
	w := New(Options{})
	var bridged, flagged int
	w.SetBridge(bridgeFunc(func(r *Exported) error {
		bridged++
		if r.Dropped {
			flagged++
		}
		return errors.New("bridge failure is not counted for a dropped record")
	}))
	for range 3 {
		w.Submit(w.Acquire())
	}
	var buf []byte
	for range 2 {
		buf, _ = w.Next(buf) // two lines in the worker's batch
	}
	w.Close() // the worker is abandoned in its write
	if st := w.Stats(); st != (Stats{Produced: 3, QueueFull: 1, ExportError: 2}) {
		t.Fatalf("stats after Close = %+v", st)
	}
	w.Flushed(0) // the write returns after all
	if st := w.Stats(); st != (Stats{Produced: 3, QueueFull: 1, ExportError: 2}) {
		t.Fatalf("stats after Flushed = %+v, want no double count", st)
	}
	if bridged != 2 || flagged != 2 {
		t.Fatalf("bridged %d, flagged %d", bridged, flagged)
	}
	if w.bytes.Load() != 0 {
		t.Fatalf("reservation not released: %d", w.bytes.Load())
	}
}

// 09 section 9 item 20, req 65: a Submit racing Close never leaves a
// record queued and uncounted.
func TestSubmitRacingClose_Req65(t *testing.T) {
	for range 50 {
		w := New(Options{})
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 50 {
					w.Submit(w.Acquire())
				}
			})
		}
		wg.Go(w.Close)
		wg.Wait()
		w.Close()
		st := w.Stats()
		if w.Pending() != 0 || st.QueueFull != st.Produced || w.bytes.Load() != 0 {
			t.Fatalf("pending %d, stats %+v, reserved %d", w.Pending(), st, w.bytes.Load())
		}
	}
}

// Close drops queued and later records with queue_full.
func TestClose(t *testing.T) {
	w := New(Options{})
	w.Submit(w.Acquire())
	w.Submit(w.Acquire())
	w.Close()
	w.Close()
	w.Submit(w.Acquire())
	if st := w.Stats(); st != (Stats{Produced: 3, QueueFull: 3}) {
		t.Fatalf("stats = %+v", st)
	}
	if w.Pending() != 0 || w.bytes.Load() != 0 {
		t.Fatalf("pending %d, reserved %d", w.Pending(), w.bytes.Load())
	}
}

// Wake signals the worker after a record is queued.
func TestWake(t *testing.T) {
	w := New(Options{})
	select {
	case <-w.Wake():
		t.Fatal("woken without a record")
	default:
	}
	w.Submit(w.Acquire())
	w.Submit(w.Acquire())
	select {
	case <-w.Wake():
	default:
		t.Fatal("not woken after Submit")
	}
}

type bridgeFunc func(*Exported) error

func (f bridgeFunc) Export(r *Exported) error { return f(r) }

// 09 req 65, 66: lines lost by stdout count export_error once, reach the
// bridge flagged Dropped, and a bridge failure alone counts export_error.
func TestFlushedAndBridge_Req66(t *testing.T) {
	fake := clocktest.New(time.Date(2026, 9, 26, 8, 0, 0, 0, time.UTC))
	w := New(Options{NodeID: testNode, Clock: fake})
	var got []Exported
	var mu sync.Mutex
	w.SetBridge(bridgeFunc(func(r *Exported) error {
		mu.Lock()
		defer mu.Unlock()
		c := *r
		c.Line = append([]byte(nil), r.Line...)
		got = append(got, c)
		if strings.Contains(string(r.Line), `"route":"fail"`) {
			return errors.New("export failed")
		}
		return nil
	}))
	for _, route := range []string{"ok", "fail", "lost", "lostfail"} {
		r := w.Acquire()
		fullRecord(r)
		r.Route = route
		if route == "lostfail" {
			r.Route = "fail"
		}
		w.Submit(r)
	}
	lines := drainLines(w, 2)
	if len(lines) != 4 || len(got) != 4 {
		t.Fatalf("lines %d, bridged %d", len(lines), len(got))
	}
	if st := w.Stats(); st.ExportError != 3 {
		t.Fatalf("stats = %+v, want 2 stdout losses and 1 bridge failure", st)
	}
	want := &emit.AccessRecord{}
	fullRecord(want)
	for i, ex := range got {
		if ex.Dropped != (i >= 2) {
			t.Errorf("record %d Dropped = %v", i, ex.Dropped)
		}
		if !ex.Start.Equal(want.Start) || !ex.Observed.Equal(fake.Now()) || ex.TraceID != want.TraceID || ex.SpanID != want.SpanID || !ex.Sampled {
			t.Errorf("record %d = %+v", i, ex)
		}
		if string(ex.Line) != lines[i] {
			t.Errorf("bridge line %d differs from stdout:\n%s\n%s", i, ex.Line, lines[i])
		}
	}
	w.SetBridge(nil)
	w.Submit(w.Acquire())
	drainLines(w, 0)
	if len(got) != 4 {
		t.Fatal("bridge called after SetBridge(nil)")
	}
}

// WP-11 done-when: no allocation per record at steady state, on the
// request goroutine (Acquire, fill, Submit) and on the worker (encode,
// Flushed).
func TestSteadyStateZeroAllocs_Req67(t *testing.T) {
	skipUnderRace(t)
	w := New(Options{NodeID: testNode})
	var buf []byte
	modes := make([]emit.FailureModeEntry, 0, MaxFailureModes) // the handler's own state
	one := func() {
		r := w.Acquire()
		r.FailureModes = modes[:0]
		fullRecord(r)
		w.Submit(r)
		buf, _ = w.Next(buf[:0])
		w.Flushed(0)
	}
	for range 100 {
		one()
	}
	if allocs := testing.AllocsPerRun(1000, one); allocs != 0 {
		t.Fatalf("%v allocations per record, want 0", allocs)
	}
	if w.Stats().QueueFull != 0 {
		t.Fatalf("stats = %+v", w.Stats())
	}
}
