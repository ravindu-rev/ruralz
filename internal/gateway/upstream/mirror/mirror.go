// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Schema defaults of a Route mirror (target), which stage G materializes;
// the snapshot compiler falls back on them for a mirror without its own.
const (
	// DefaultPercent is the share of requests a mirror copies.
	DefaultPercent = 100
	// DefaultMaxInFlight bounds a mirror's copies in flight on a Node.
	DefaultMaxInFlight = 1000
)

// Errors of New and Run.
var (
	// ErrDeps reports a Deps without a required dependency.
	ErrDeps = errors.New("mirror: missing dependency")
	// ErrRunning is returned by a second Run: a Runner runs once.
	ErrRunning = errors.New("mirror: the runner already ran")
)

// errEnded is the cause a leg's context ends with when the ending
// protocol or the Runner's shutdown ends it: no deadline expiry, so the
// leg's attempt is no Upstream failure.
var errEnded = errors.New("mirror: leg ended by the ending protocol or the Node's shutdown")

// Pinner pins the snapshot a mirror leg runs on; *retire.Holder implements
// it.
type Pinner interface {
	// PinLeg pins s once more for rec on stripe, for a leg of a request
	// that pins s, unless the Drain deadline has passed, and reports
	// whether it did.
	PinLeg(s *snapshot.Snapshot, stripe emit.Stripe, rec *snapshot.PinnedRequest) bool
	// Unpin releases a pin PinLeg took.
	Unpin(s *snapshot.Snapshot, stripe emit.Stripe, rec *snapshot.PinnedRequest)
}

// Deps are the Node-wide parts a Runner uses.
type Deps struct {
	// Snapshots pins the snapshot of each leg; required.
	Snapshots Pinner
	// Budget is the Node buffer budget, limits.maxBufferedBytes, which the
	// body copies reserve from; required.
	Budget *body.Budget
	// Rand draws the sampling of each mirror and must be safe for
	// concurrent use; nil means the math/rand/v2 runtime generator.
	Rand rand.Source
	// Clock times each leg's Route deadline; nil means clock.Real().
	Clock clock.Clock
	// Logger records a leg that panicked, at error with its stack; nil
	// discards.
	Logger *slog.Logger
}

// Runner runs the mirror legs of a Node (package documentation). It is
// safe for concurrent use.
type Runner struct {
	pins   Pinner
	budget *body.Budget
	rnd    *rand.Rand
	clk    clock.Clock
	log    *slog.Logger

	// mu guards base and ran: Offer starts legs under its read lock only
	// until Run ended them, so Run's Wait follows every Add.
	mu sync.RWMutex
	// base is the parent of every leg's context, nil once Run ended them;
	// stop ends it.
	base context.Context
	stop context.CancelCauseFunc
	ran  bool
	wg   sync.WaitGroup
}

// New returns a Runner over d.
func New(d Deps) (*Runner, error) {
	switch {
	case d.Snapshots == nil:
		return nil, errors.Join(ErrDeps, errors.New("mirror: Snapshots is required"))
	case d.Budget == nil:
		return nil, errors.Join(ErrDeps, errors.New("mirror: Budget is required"))
	}
	r := &Runner{pins: d.Snapshots, budget: d.Budget, clk: d.Clock, log: d.Logger}
	if d.Rand != nil {
		r.rnd = rand.New(d.Rand) //nolint:gosec // G404: sampling, not security.
	}
	if r.clk == nil {
		r.clk = clock.Real()
	}
	if r.log == nil {
		r.log = slog.New(slog.DiscardHandler)
	}
	r.base, r.stop = context.WithCancelCause(context.Background())
	return r, nil
}

// Run waits until ctx ends, then ends every running leg, so Offer starts
// no other, and returns once each has returned: the Node's shutdown. Offer
// starts legs from New on. A Runner runs once: a second Run returns
// ErrRunning.
func (r *Runner) Run(ctx context.Context) error {
	r.mu.Lock()
	if r.ran {
		r.mu.Unlock()
		return ErrRunning
	}
	r.ran = true
	r.mu.Unlock()
	<-ctx.Done()
	r.mu.Lock()
	r.base = nil
	r.mu.Unlock()
	r.stop(errEnded)
	r.wg.Wait()
	return nil
}

// Offer is the first attempt of one request's upstreams leg, offered to
// its Route's mirrors.
type Offer struct {
	// Snapshot is the snapshot the request pinned.
	Snapshot *snapshot.Snapshot
	// Mirrors are the Route's mirrors (snapshot.Route.Mirrors).
	Mirrors []*snapshot.Mirror
	// Request is the attempt as it will be sent, after its onUpstreamRequest
	// Policies; Offer may replace its Body with a reader that copies what
	// the attempt sends.
	Request *http.Request
	// Vars are the request's Base variables, read for each mirror
	// Upstream's ring-hash key.
	Vars *expr.Vars
	// Deadline is the request's Route deadline.
	Deadline time.Time
	// Stripe is the request's metric stripe.
	Stripe emit.Stripe
}

// Offer offers o's attempt to its Route's mirrors on the request's
// goroutine (package documentation, "Offers"): it samples each mirror,
// takes its slot, copies the attempt and starts one leg per mirror that
// pinned. It never blocks on a mirror: a buffered body is copied at once,
// a streamed one as the attempt reads it.
func (r *Runner) Offer(o *Offer) {
	if o == nil || o.Snapshot == nil || o.Snapshot.MirrorLegs == nil || o.Request == nil || o.Request.URL == nil {
		return
	}
	var picked []*snapshot.Mirror
	for _, m := range o.Mirrors {
		if !r.sampled(m.Percent) {
			continue
		}
		if !takeSlot(m) {
			count(m, emit.MirrorDropped, o.Stripe)
			continue
		}
		picked = append(picked, m)
	}
	if len(picked) == 0 {
		return
	}
	cp, ok := r.copyBody(o.Request, o.Stripe, len(picked))
	if !ok {
		for _, m := range picked {
			m.InFlight.Add(-1)
			count(m, emit.MirrorDropped, o.Stripe)
		}
		return
	}
	src := o.Request
	tmpl := template{
		method: src.Method, path: src.URL.Path, rawPath: src.URL.RawPath, rawQuery: src.URL.RawQuery,
		trailer: src.Trailer, copy: cp,
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, m := range picked {
		l := &leg{
			r: r, snap: o.Snapshot, m: m, tmpl: tmpl, deadline: o.Deadline, stripe: o.Stripe,
			key: o.Snapshot.MirrorLegs.MirrorKey(m.Upstream, o.Vars, o.Stripe), header: src.Header.Clone(),
		}
		if r.base == nil {
			l.end(emit.MirrorDropped)
			continue
		}
		ctx, cancel := context.WithCancelCause(r.base)
		l.cancel = cancel
		l.rec.Cancel = func() { cancel(errEnded) }
		if !r.pins.PinLeg(o.Snapshot, o.Stripe, &l.rec) {
			cancel(nil)
			l.end(emit.MirrorDropped)
			continue
		}
		l.pinned = true
		r.wg.Go(func() { l.run(ctx) })
	}
}

// sampled reports whether a mirror of percent copies this request: always
// at 100, never at 0, else a draw below percent of 100.
func (r *Runner) sampled(percent uint32) bool {
	switch {
	case percent >= 100:
		return true
	case percent == 0:
		return false
	case r.rnd != nil:
		return uint32(r.rnd.IntN(100)) < percent //nolint:gosec // G115: IntN(100) fits.
	default:
		return uint32(rand.IntN(100)) < percent //nolint:gosec // G404, G115: sampling, not security; IntN(100) fits.
	}
}

// takeSlot takes one of m's maxInFlight slots on the Node, or reports
// false when every slot is taken.
func takeSlot(m *snapshot.Mirror) bool {
	for {
		n := m.InFlight.Load()
		if n >= m.MaxInFlight {
			return false
		}
		if m.InFlight.CompareAndSwap(n, n+1) {
			return true
		}
	}
}

// count adds one copy with result to m's counter.
func count(m *snapshot.Mirror, result int, stripe emit.Stripe) {
	if m.Metrics != nil && m.Metrics.Results[result] != nil {
		m.Metrics.Results[result].Add(stripe, 1)
	}
}

// template is what an offer copied of the attempt, which each leg's own
// request is built from.
type template struct {
	method, path, rawPath, rawQuery string
	// trailer is the attempt's declared trailer section, whose values a
	// streamed body's end fills; nil without.
	trailer http.Header
	// copy is the body copy the legs share; nil for no body.
	copy *bodyCopy
}

// leg is one mirror leg.
type leg struct {
	r    *Runner
	snap *snapshot.Snapshot
	m    *snapshot.Mirror
	tmpl template
	// header is the leg's clone of the attempt's header fields, taken at
	// Offer, while the attempt's own map is live.
	header   http.Header
	key      snapshot.MirrorKey
	deadline time.Time
	stripe   emit.Stripe
	rec      snapshot.PinnedRequest
	cancel   context.CancelCauseFunc
	pinned   bool
}

// run sends the leg on its goroutine, within the Route deadline: once a
// streamed body's copy completed, one attempt to the mirror's Upstream,
// whose result it counts.
func (l *leg) run(ctx context.Context) {
	result := emit.MirrorFailed
	defer func() {
		if v := recover(); v != nil {
			l.r.log.ErrorContext(ctx, "mirror leg panic recovered", catalog.KeyUpstream, l.m.Upstream,
				catalog.KeyPanicType, fmt.Sprintf("%T", v), catalog.KeyStack, string(debug.Stack()))
			result = emit.MirrorFailed
		}
		l.end(result)
	}()
	if !l.deadline.IsZero() {
		t := l.r.clk.AfterFuncAt(l.deadline, func() { l.cancel(context.DeadlineExceeded) })
		defer t.Stop()
	}
	if cp := l.tmpl.copy; cp != nil && !cp.wait(ctx) {
		result = emit.MirrorDropped
		return
	}
	req := l.request(ctx)
	if err := l.snap.MirrorLegs.Mirror(ctx, l.m.Upstream, &snapshot.MirrorCopy{
		Request: req, Key: l.key, Deadline: l.deadline, Stripe: l.stripe,
	}); err == nil {
		result = emit.MirrorSent
	}
}

// request builds the leg's own request from the template: the attempt's
// method, path, query and header fields, the body copy, and the trailer
// section with the values the attempt sent.
func (l *leg) request(ctx context.Context) *http.Request {
	t := &l.tmpl
	req := &http.Request{
		Method:     t.method,
		URL:        &url.URL{Path: t.path, RawPath: t.rawPath, RawQuery: t.rawQuery},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     l.header,
		Body:       http.NoBody,
	}
	if cp := t.copy; cp != nil {
		b := cp.bytes()
		req.Body, req.ContentLength = cp.reader(b), int64(len(b))
		req.GetBody = func() (io.ReadCloser, error) { return cp.reader(b), nil }
	}
	if len(t.trailer) > 0 {
		req.Trailer, req.ContentLength = t.trailer.Clone(), -1
	}
	return req.WithContext(ctx)
}

// end counts the leg's result and gives back what it holds: its slot, its
// share of the body copy, its pin and its context.
func (l *leg) end(result int) {
	count(l.m, result, l.stripe)
	l.m.InFlight.Add(-1)
	if cp := l.tmpl.copy; cp != nil {
		cp.release()
	}
	if l.pinned {
		l.r.pins.Unpin(l.snap, l.stripe, &l.rec)
	}
	if l.cancel != nil {
		l.cancel(nil)
	}
}
