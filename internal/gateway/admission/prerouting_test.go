// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for pre-routing responses: 5 s write deadline, at most 2,000 at
// once, beyond it http.ErrAbortHandler without writing.

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// deadlineRecorder is a ResponseWriter whose write deadline
// http.ResponseController can set.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	mu       sync.Mutex
	deadline time.Time
}

func (d *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
	return nil
}

// abortOf runs fn and returns what it panicked with.
func abortOf(fn func()) (v any) {
	defer func() { v = recover() }()
	fn()
	return nil
}

// TestPreRoutingLimit holds 2,000 pre-routing writes blocked in their
// write; the 2,001st aborts with http.ErrAbortHandler and writes nothing,
// and room returns when a write ends.
func TestPreRoutingLimit(t *testing.T) {
	p := NewPreRouting(DefaultPreRouting, clocktest.New(epoch()))
	release := make(chan struct{})
	var entered, done sync.WaitGroup
	entered.Add(DefaultPreRouting)
	for range DefaultPreRouting {
		done.Go(func() {
			p.Do(nil, func() {
				entered.Done()
				<-release
			})
		})
	}
	entered.Wait()
	if p.InUse() != DefaultPreRouting {
		t.Fatalf("InUse = %d, want %d", p.InUse(), DefaultPreRouting)
	}

	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	wrote := false
	v := abortOf(func() {
		p.Do(http.NewResponseController(rec), func() {
			wrote = true
			rec.WriteHeader(http.StatusServiceUnavailable)
		})
	})
	err, _ := v.(error)
	if !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("the 2,001st write panicked with %v, want http.ErrAbortHandler", v)
	}
	if wrote || rec.Body.Len() != 0 || !rec.deadline.IsZero() {
		t.Fatal("the aborted response was written or got a deadline")
	}
	if p.InUse() != DefaultPreRouting {
		t.Fatalf("the rejected write kept a slot: InUse = %d", p.InUse())
	}

	close(release)
	done.Wait()
	if p.InUse() != 0 {
		t.Fatalf("InUse = %d after the writes ended", p.InUse())
	}
	if v := abortOf(func() { p.Do(nil, func() {}) }); v != nil {
		t.Fatalf("a write after the flood panicked: %v", v)
	}
}

// blockingFlusher is a ResponseWriter whose flush blocks until release
// closes, as net/http's does for a client that never reads, such as an
// HTTP/2 client with a zero stream window, until the write deadline ends
// the stream.
type blockingFlusher struct {
	*httptest.ResponseRecorder
	events  chan<- bool
	release <-chan struct{}
}

// FlushError is the flush http.ResponseController calls: it reports true
// on events, then blocks.
func (b *blockingFlusher) FlushError() error {
	b.events <- true
	<-b.release
	return os.ErrDeadlineExceeded
}

// TestPreRoutingHoldsSlotWhileFlushing holds 2,000 pre-routing responses
// whose small bodies net/http buffered and whose flush blocks on clients
// that never read: each keeps its slot until the flush ends, so the
// 2,001st aborts without writing. Do released the slot when the write
// returned, before any flush, so 2,500 such clients all got a response
// written at once.
func TestPreRoutingHoldsSlotWhileFlushing(t *testing.T) {
	p := NewPreRouting(DefaultPreRouting, clocktest.New(epoch()))
	release := make(chan struct{})
	// Each response reports true when its flush blocks and false when Do
	// returned.
	events := make(chan bool, 2*DefaultPreRouting)
	var done sync.WaitGroup
	defer done.Wait()
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	for range DefaultPreRouting {
		done.Go(func() {
			w := &blockingFlusher{ResponseRecorder: httptest.NewRecorder(), events: events, release: release}
			p.Do(http.NewResponseController(w), func() {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":404}`))
			})
			events <- false
		})
	}
	for range DefaultPreRouting {
		if flushing := <-events; !flushing {
			t.Fatal("a pre-routing response left Do without flushing to its client")
		}
	}
	if p.InUse() != DefaultPreRouting {
		t.Fatalf("InUse = %d with every flush blocked, want %d", p.InUse(), DefaultPreRouting)
	}
	rec := httptest.NewRecorder()
	v := abortOf(func() { p.Do(http.NewResponseController(rec), func() { rec.WriteHeader(http.StatusNotFound) }) })
	if err, _ := v.(error); !errors.Is(err, http.ErrAbortHandler) {
		t.Fatalf("the 2,001st response panicked with %v, want http.ErrAbortHandler", v)
	}
	unblock()
	done.Wait()
	if p.InUse() != 0 {
		t.Fatalf("InUse = %d after the flushes ended", p.InUse())
	}
}

// TestPreRoutingWriteDeadline sets the write deadline to now + 5 s
// from the injected clock before writing.
func TestPreRoutingWriteDeadline(t *testing.T) {
	clk := clocktest.New(epoch())
	p := NewPreRouting(1, clk)
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	var seen time.Time
	p.Do(http.NewResponseController(rec), func() {
		seen = rec.deadline
		rec.WriteHeader(http.StatusNotFound)
	})
	if want := epoch().Add(PreRoutingWriteTimeout); !seen.Equal(want) || PreRoutingWriteTimeout != 5*time.Second {
		t.Fatalf("deadline during write = %v, want %v", seen, want)
	}
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
}

// TestPreRoutingNoDeadlineSupport writes through a ResponseWriter
// without deadline support.
func TestPreRoutingNoDeadlineSupport(t *testing.T) {
	p := NewPreRouting(1, clocktest.New(epoch()))
	rec := httptest.NewRecorder()
	p.Do(http.NewResponseController(rec), func() { rec.WriteHeader(http.StatusBadRequest) })
	if rec.Code != http.StatusBadRequest || p.InUse() != 0 {
		t.Fatalf("status %d, InUse %d", rec.Code, p.InUse())
	}
}

// TestPreRoutingReleaseOnPanic returns the slot when the write panics.
func TestPreRoutingReleaseOnPanic(t *testing.T) {
	p := NewPreRouting(1, clocktest.New(epoch()))
	if v := abortOf(func() { p.Do(nil, func() { panic("write failed") }) }); v != "write failed" {
		t.Fatalf("panic %v", v)
	}
	if p.InUse() != 0 || !p.TryAcquire() {
		t.Fatal("the slot was not returned")
	}
	if p.TryAcquire() {
		t.Fatal("limit 1 admitted two")
	}
	p.Release()
}

func TestPreRoutingZeroLimit(t *testing.T) {
	for _, limit := range []int64{0, -1} {
		p := NewPreRouting(limit, clocktest.New(epoch()))
		if p.TryAcquire() || p.InUse() != 0 {
			t.Fatalf("limit %d admitted a write", limit)
		}
	}
}

// BenchmarkPreRoutingDo measures the limiter around a pre-routing write.
func BenchmarkPreRoutingDo(b *testing.B) {
	p := NewPreRouting(DefaultPreRouting, clocktest.New(epoch()))
	write := func() {}
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			p.Do(nil, write)
		}
	})
}
