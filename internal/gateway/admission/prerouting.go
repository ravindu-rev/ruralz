// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admission

import (
	"net/http"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// PreRouting bounds the pre-routing responses written at once (spec 04 req
// 20): RZ-RT-001, RZ-RT-002, RZ-RT-005, RZ-RT-006 and RZ-RT-017 run no
// Filter Chain, get a 5 s write deadline, and at most DefaultPreRouting of
// them are written concurrently; beyond it the handler aborts with
// http.ErrAbortHandler without writing, so a flood of rejections costs no
// more than its connections. PreRouting is safe for concurrent use.
type PreRouting struct {
	n     atomic.Int64
	limit int64
	clk   clock.Clock
}

// NewPreRouting returns a limiter admitting limit concurrent writes
// (DefaultPreRouting in ruralzd) that sets write deadlines from clk.
func NewPreRouting(limit int64, clk clock.Clock) *PreRouting {
	return &PreRouting{limit: max(0, limit), clk: clk}
}

// TryAcquire takes one write slot with one atomic add, or returns false at
// the limit.
func (p *PreRouting) TryAcquire() bool {
	if p.n.Add(1) <= p.limit {
		return true
	}
	p.n.Add(-1)
	return false
}

// Release returns a slot taken by a successful TryAcquire.
func (p *PreRouting) Release() { p.n.Add(-1) }

// InUse returns the pre-routing writes in progress.
func (p *PreRouting) InUse() int64 { return max(0, p.n.Load()) }

// Do writes one pre-routing response: it takes a slot, sets the write
// deadline to now plus PreRoutingWriteTimeout through rc (a writer that
// does not support deadlines is written without one), calls write and
// releases the slot, even when write panics. At the limit it calls
// panic(http.ErrAbortHandler) without calling write: net/http then aborts
// the response silently (the HTTP/1.1 connection closes, the HTTP/2 stream
// resets) and logs nothing.
func (p *PreRouting) Do(rc *http.ResponseController, write func()) {
	if !p.TryAcquire() {
		panic(http.ErrAbortHandler)
	}
	defer p.Release()
	if rc != nil {
		// ErrNotSupported only: the response is still written.
		_ = rc.SetWriteDeadline(p.clk.Now().Add(PreRoutingWriteTimeout))
	}
	write()
}
