// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/composition"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

var (
	// ErrDeps reports a Deps without a required dependency.
	ErrDeps = errors.New("replay: missing dependency")
	// ErrNoRequest reports a Replay without a request or its URL.
	ErrNoRequest = errors.New("replay: no request")
	// ErrGenerated reports a response the Node generated instead of an
	// Upstream; the error carries the response's RZ code when it has one.
	ErrGenerated = errors.New("replay: the Node generated the response instead of an Upstream")
	// errEnded is the cause with which the ending protocol cancels a
	// replay.
	errEnded = errors.New("replay: ended by the ending protocol")
)

// Deps are the Node-wide parts a Replayer shares with the request handler.
type Deps struct {
	// Snapshots publishes the snapshot every replay pins; required.
	Snapshots *retire.Holder
	// Units is the Node in-flight unit ceiling; required.
	Units *admission.Units
	// Exchanges pools the per-request state the replay's legs see;
	// required.
	Exchanges *exchange.Pool
	// Executor runs the upstream-leg Phases; required.
	Executor *executor.Executor
	// Tracer makes the replay's sampling decision, a new unsampled root;
	// required.
	Tracer emit.Tracer
	// Clock times the replay's deadline; nil means clock.Real().
	Clock clock.Clock
}

// Replayer implements filter.Replayer over the Node's published snapshots
// (package documentation). It is safe for concurrent use.
type Replayer struct {
	snaps  *retire.Holder
	units  *admission.Units
	pool   *exchange.Pool
	exec   *executor.Executor
	tracer emit.Tracer
	clk    clock.Clock
}

var _ filter.Replayer = (*Replayer)(nil)

// New returns the Replayer over d.
func New(d Deps) (*Replayer, error) {
	switch {
	case d.Snapshots == nil:
		return nil, errors.Join(ErrDeps, errors.New("replay: Snapshots is required"))
	case d.Units == nil:
		return nil, errors.Join(ErrDeps, errors.New("replay: Units is required"))
	case d.Exchanges == nil:
		return nil, errors.Join(ErrDeps, errors.New("replay: Exchanges is required"))
	case d.Executor == nil:
		return nil, errors.Join(ErrDeps, errors.New("replay: Executor is required"))
	case d.Tracer == nil:
		return nil, errors.Join(ErrDeps, errors.New("replay: Tracer is required"))
	}
	p := &Replayer{snaps: d.Snapshots, units: d.Units, pool: d.Exchanges, exec: d.Executor, tracer: d.Tracer, clk: d.Clock}
	if p.clk == nil {
		p.clk = clock.Real()
	}
	return p, nil
}

// call is one replay's state, from the in-flight unit to the body's Close.
// Only the ending protocol reaches it from another goroutine, through rec.
type call struct {
	p        *Replayer
	start    time.Time
	stripe   emit.Stripe
	cancel   context.CancelCauseFunc
	rec      snapshot.PinnedRequest
	snap     *snapshot.Snapshot
	x        *exchange.Request
	run      *executor.Run
	timer    clock.Timer
	decision emit.Decision
}

// Replay sends req, the saved request, to upstream on route in the
// published snapshot (filter.Replayer): one leg to the named Upstream, or
// for a composition entry, whose upstream is empty, the Route's whole
// composition. The response holds the pin and the in-flight unit until
// the caller closes its body; only StatusCode, Status, Header, Trailer
// (filled once the body was read to its end), ContentLength, Body and
// Request are set. req is not modified.
func (p *Replayer) Replay(ctx context.Context, route, upstream string, req *http.Request) (*http.Response, error) {
	if req == nil || req.URL == nil {
		return nil, ErrNoRequest
	}
	if !p.units.TryAcquire(1) {
		return nil, fmt.Errorf("replay: the in-flight ceiling is full: %w", filter.ErrNotAvailable)
	}
	c := &call{p: p, start: p.clk.Now(), stripe: p.snaps.ConnStripe()}
	ctx, c.cancel = context.WithCancelCause(ctx)
	cancel := c.cancel
	c.rec.Cancel = func() { cancel(errEnded) }
	c.snap = p.snaps.Pin(c.stripe, &c.rec)
	if c.snap == nil {
		cancel(nil)
		p.units.Release(1)
		return nil, fmt.Errorf("replay: no published snapshot: %w", filter.ErrNotAvailable)
	}
	resp, err := c.send(ctx, route, upstream, req)
	if err != nil {
		c.finish()
		return nil, err
	}
	return resp, nil
}

// send resolves the target in the pinned snapshot and runs it.
func (c *call) send(ctx context.Context, route, upstreamName string, req *http.Request) (*http.Response, error) {
	if c.ended() {
		return nil, fmt.Errorf("replay: the snapshot is ending: %w", filter.ErrNotAvailable)
	}
	rt := routeNamed(c.snap, route)
	if rt == nil {
		return nil, fmt.Errorf("replay: route %q: %w", route, filter.ErrNotAvailable)
	}
	_, composed := rt.Forward.(*composition.Composition)
	switch {
	case upstreamName == "" && !composed:
		return nil, fmt.Errorf("replay: route %q no longer runs a composition: %w", route, filter.ErrNotAvailable)
	case upstreamName != "" && composed:
		return nil, fmt.Errorf("replay: route %q now runs a composition: %w", route, filter.ErrNotAvailable)
	case upstreamName != "" && c.snap.Legs == nil:
		return nil, fmt.Errorf("replay: upstream %q: %w", upstreamName, filter.ErrNotAvailable)
	}
	path, err := routematch.NormalizePath(routematch.RequestPath(req.URL))
	if err != nil {
		return nil, fmt.Errorf("replay: %w", err)
	}
	scheme := strings.ToLower(req.URL.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	// A new root, whatever traceparent the saved request carries, and
	// never sampled.
	c.p.tracer.Decide(nil, 0, &c.decision)
	c.decision.Sampled, c.decision.Remote = false, false

	hreq := req.Clone(ctx)
	x := c.p.pool.Get()
	c.x = x
	x.Begin(hreq, &exchange.Start{
		TraceID: c.decision.TraceID, Time: c.start, Scheme: scheme, Path: path,
		Stripe: c.stripe, Limits: c.snap.Gateway.Limits,
	})
	x.SetRoute(rt, nil)
	if err := x.PrepareBody(ctx); err != nil {
		return nil, fmt.Errorf("replay: request body: %w", err)
	}
	c.run = c.p.exec.BeginLegs(x, c.snap.LegChains)
	o := x.Outbound()
	o.Trace, o.Deadline, o.Hooks, o.Replay = &c.decision, c.start.Add(rt.Timeout), c.run, true
	if rt.Timeout > 0 {
		cancel := c.cancel
		c.timer = c.p.clk.AfterFunc(rt.Timeout, func() { cancel(context.DeadlineExceeded) })
	}

	var u *snapshot.UpstreamResponse
	if composed {
		u, err = rt.Forward.Forward(ctx, o)
	} else {
		u, err = c.snap.Legs.RunLeg(ctx, upstreamName, o)
	}
	switch {
	case err != nil:
		return nil, fmt.Errorf("replay: %w", err)
	case u == nil:
		return nil, fmt.Errorf("replay: no response: %w", filter.ErrNotAvailable)
	case u.Generated != nil:
		if u.Body != nil {
			_ = u.Body.Close()
		}
		return nil, generated(u.Generated)
	}
	h := u.Header
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{
		Status: strconv.Itoa(u.Status) + " " + http.StatusText(u.Status), StatusCode: u.Status,
		Header: h, Trailer: u.Trailer, ContentLength: contentLength(h),
		Body: &respBody{c: c, rc: u.Body}, Request: req,
	}, nil
}

// ended reports whether the ending protocol ended the replay.
func (c *call) ended() bool {
	c.rec.Mu.Lock()
	defer c.rec.Mu.Unlock()
	return c.rec.Ended != snapshot.EndNone
}

// finish releases everything the replay holds, in the request handler's
// order: the deadline timer and the context, the executor run and the
// exchange, the pin, then the in-flight unit. Every leg has ended by now:
// on an error the Forwarder ended them, after a response the body's Close
// did.
func (c *call) finish() {
	if c.timer != nil {
		c.timer.Stop()
	}
	c.cancel(nil)
	if c.run != nil {
		c.run.Release()
		c.run = nil
	}
	if c.x != nil {
		c.p.pool.Put(c.x)
		c.x = nil
	}
	c.p.snaps.Unpin(c.snap, c.stripe, &c.rec)
	c.p.units.Release(1)
}

// routeNamed returns the snapshot's Route named name, nil when it holds
// none.
func routeNamed(s *snapshot.Snapshot, name string) *snapshot.Route {
	for _, rt := range s.Routes {
		if rt != nil && rt.Name == name {
			return rt
		}
	}
	return nil
}

// generated returns the error of a response the Node generated: its code
// over ErrGenerated, or its status when it has no code.
func generated(g *filter.Response) error {
	if g.Code != "" {
		return fmt.Errorf("replay: %w", errcode.Wrap(g.Code, ErrGenerated))
	}
	return fmt.Errorf("replay: status %d: %w", g.Status, ErrGenerated)
}

// contentLength returns the declared Content-Length, -1 when absent or
// invalid.
func contentLength(h http.Header) int64 {
	vs := h["Content-Length"]
	if len(vs) != 1 {
		return -1
	}
	n, err := strconv.ParseInt(strings.TrimSpace(vs[0]), 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// respBody is the replay response's body: the leg's, and the release of the
// replay at its first Close.
type respBody struct {
	c      *call
	rc     io.ReadCloser
	closed atomic.Bool
}

// Read reads the leg's body; an empty one reads io.EOF.
func (b *respBody) Read(p []byte) (int, error) {
	if b.closed.Load() {
		return 0, http.ErrBodyReadAfterClose
	}
	if b.rc == nil {
		return 0, io.EOF
	}
	return b.rc.Read(p)
}

// Close closes the leg's body, which ends its leg, then releases the
// replay; only the first Close acts.
func (b *respBody) Close() error {
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	var err error
	if b.rc != nil {
		err = b.rc.Close()
	}
	b.c.finish()
	return err
}
