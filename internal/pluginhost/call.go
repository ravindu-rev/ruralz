// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tetratelabs/wazero/sys"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// callKey is the context key of the guest call a Host Function serves: a
// guest call runs under a child of the request's context that the call
// timer cancels, and its callKey value is the call.
type callKey struct{}

// errProcExit is the trap of WASI proc_exit.
var errProcExit = errors.New("pluginhost: the guest called proc_exit")

// call is the host state of one guest call: a Phase call or the
// rz_configure of a new instance. It lives on the goroutine of that call.
type call struct {
	e *Engine
	// b is the Policy binding: its Capabilities, keys, state namespace,
	// metric names and log budget.
	b *binding
	// x is the request; nil in rz_configure.
	x  filter.Exchange
	ph phase.Phase
	// configuring marks rz_configure.
	configuring bool
	// req is the Policy's per-request state; nil in rz_configure.
	req *perRequest
	// reqCtx is the request's context, which State Store calls take their
	// own deadline from, outside the call timer.
	reqCtx context.Context

	hostCalls int
	// sticky is the first sticky error (ERR_PHASE, ERR_BUDGET,
	// ERR_DENIED), 0 when none: the call then fails with RZ-PLG-006.
	sticky    int32
	stickyWhy string

	// sent is the accepted response_send.
	sent       bool
	sendStatus int
	sendBody   []byte

	// stateErr is the State Store failure behind the call's last
	// ERR_UNAVAILABLE: a failed or timed-out call, or the open breaker's
	// skip (RZ-STS); parkedRefused marks a parked-bound refusal.
	stateErr      *statestore.Error
	parkedRefused bool

	// logBytes is what the call logged so far.
	logBytes int

	timer *callTimer
	// inst is the instance the call runs on.
	inst *instance
	// chunkOff is where the call's next chunk_read starts.
	chunkOff int
}

// callTimer is the wall-clock limit of one call: a host timer of
// limits.timeout that cancels the call's context, stopped while the guest
// is parked in its State Store call and resumed with the time left.
type callTimer struct {
	clk       clock.Clock
	mu        sync.Mutex
	t         clock.Timer
	remaining time.Duration
	started   time.Time
	cancel    context.CancelFunc
	fired     atomic.Bool
}

// startTimer arms a timer of d that cancels the call.
func startTimer(clk clock.Clock, d time.Duration, cancel context.CancelFunc) *callTimer {
	t := &callTimer{clk: clk, remaining: d, cancel: cancel}
	t.mu.Lock()
	t.started = clk.Now()
	t.t = clk.AfterFunc(d, t.expire)
	t.mu.Unlock()
	return t
}

// expire cancels the call at its deadline.
func (t *callTimer) expire() {
	t.fired.Store(true)
	t.cancel()
}

// pause stops the timer, keeping the time left; false when it already
// fired.
func (t *callTimer) pause() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.t == nil {
		return !t.fired.Load()
	}
	if !t.t.Stop() {
		t.t = nil
		return false
	}
	t.t = nil
	t.remaining -= t.clk.Since(t.started)
	if t.remaining <= 0 {
		t.expire()
		return false
	}
	return true
}

// resume re-arms a paused timer with the time left.
func (t *callTimer) resume() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.t != nil || t.fired.Load() {
		return
	}
	t.started = t.clk.Now()
	t.t = t.clk.AfterFunc(t.remaining, t.expire)
}

// stop ends the timer.
func (t *callTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.t != nil {
		t.t.Stop()
		t.t = nil
	}
}

// outcome classifies a guest call that returned err or the result r.
type outcome struct {
	// fault is a trap, timeout or memory limit: the instance is discarded
	// and the fault breaker counts it.
	fault bool
	// discard closes the instance without counting a fault: a call the
	// request canceled, whose module wazero closed.
	discard bool
	// code is the RZ code of a call that cannot decide; "" for CONTINUE,
	// RESPOND and END_STREAM.
	code   string
	result int32
	err    error
}

// classify maps a finished guest call to its outcome
// (docs/architecture/05-wasm-plugin-system.md "Failure semantics and the
// RZ-PLG registry"): a timer expiry is RZ-PLG-002; a trap at the memory
// limit RZ-PLG-003; any other trap, proc_exit or recovered host panic
// RZ-PLG-001; then a sticky error RZ-PLG-006 whatever the guest returned;
// RESPOND without an accepted response_send, END_STREAM outside onChunk or
// an unknown result RZ-PLG-007; and a negative result cannot decide, under
// the State Store failure behind an ERR_UNAVAILABLE (RZ-STS), the parked
// bound's RZ-PLG-010, else RZ-PLG-011.
func (c *call) classify(r int32, err error, atLimit bool) outcome {
	if err != nil {
		var exit *sys.ExitError
		switch {
		case c.timer != nil && c.timer.fired.Load():
			return outcome{fault: true, code: CodeDeadline, err: err}
		case errors.As(err, &exit) && (exit.ExitCode() == sys.ExitCodeContextCanceled || exit.ExitCode() == sys.ExitCodeDeadlineExceeded):
			// The request ended the call: no Plugin fault.
			return outcome{discard: true, code: CodeDeadline, err: err}
		case atLimit && !errors.Is(err, errOutOfBounds) && !errors.Is(err, errProcExit):
			return outcome{fault: true, code: CodeMemory, err: err}
		default:
			return outcome{fault: true, code: CodeTrap, err: err}
		}
	}
	if c.sticky != 0 {
		return outcome{code: CodeMisuse, err: errors.New(c.stickyWhy)}
	}
	switch {
	case r == resultContinue:
		return outcome{result: r}
	case r == resultRespond:
		if !c.sent {
			return outcome{code: CodeInvalidResult, err: errors.New("RESPOND without an accepted response_send")}
		}
		return outcome{result: r}
	case r == resultEndStream:
		if c.ph != phase.OnChunk {
			return outcome{code: CodeInvalidResult, err: errors.New("END_STREAM outside onChunk")}
		}
		return outcome{result: r}
	case r < 0:
		switch {
		case c.stateErr != nil:
			return outcome{code: c.stateErr.Code(), result: r, err: c.stateErr}
		case c.parkedRefused:
			return outcome{code: CodeParkedBound, result: r, err: errors.New("the parked bound refused the State Store call")}
		default:
			return outcome{code: CodeUndecided, result: r, err: errors.New("the guest could not decide")}
		}
	default:
		return outcome{code: CodeInvalidResult, err: errors.New("an unknown Phase result")}
	}
}

// response returns the generated response of an accepted response_send.
func (c *call) response() *filter.Response {
	body := c.sendBody
	if body == nil {
		body = []byte{}
	}
	return &filter.Response{Status: c.sendStatus, Header: http.Header{}, Body: body}
}
