// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for spec 04 req 20 (pre-routing responses: 5 s write deadline, at
// most 2,000 at once, beyond it http.ErrAbortHandler without writing);
// test plan item 9.

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

// TestReq20PreRoutingLimit holds 2,000 pre-routing writes blocked in their
// write; the 2,001st aborts with http.ErrAbortHandler and writes nothing,
// and room returns when a write ends.
func TestReq20PreRoutingLimit(t *testing.T) {
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

// TestReq20PreRoutingWriteDeadline sets the write deadline to now + 5 s
// from the injected clock before writing.
func TestReq20PreRoutingWriteDeadline(t *testing.T) {
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

// TestReq20PreRoutingNoDeadlineSupport writes through a ResponseWriter
// without deadline support.
func TestReq20PreRoutingNoDeadlineSupport(t *testing.T) {
	p := NewPreRouting(1, clocktest.New(epoch()))
	rec := httptest.NewRecorder()
	p.Do(http.NewResponseController(rec), func() { rec.WriteHeader(http.StatusBadRequest) })
	if rec.Code != http.StatusBadRequest || p.InUse() != 0 {
		t.Fatalf("status %d, InUse %d", rec.Code, p.InUse())
	}
}

// TestReq20PreRoutingReleaseOnPanic returns the slot when the write panics.
func TestReq20PreRoutingReleaseOnPanic(t *testing.T) {
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

func TestReq20PreRoutingZeroLimit(t *testing.T) {
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
