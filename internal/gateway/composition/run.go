// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// errNoExchange reports a Forward without a client exchange.
var errNoExchange = errors.New("composition: forward without an Outbound exchange")

// run is one request's composition state, pooled by the Engine. Its
// results are written by the step that owns them: in an aggregate each
// step's goroutine writes its own, and the request goroutine reads them
// once every goroutine has ended.
type run struct {
	c *Composition
	o *snapshot.Outbound
	// vars is the steps' activation: the client's Base variables after the
	// request Phases, with steps in a sequential composition. Evaluation
	// only reads it, so aggregate steps share it.
	vars  expr.Vars
	steps expr.Steps
	res   []result
	// incoming is the client request's method and query its raw query
	// after the request Phases.
	incoming, query string
	// partial is set once an optional step was left out.
	partial bool
	wg      sync.WaitGroup
}

// result is one step's outcome.
type result struct {
	// out is the step's copy of the request's Outbound: the client body
	// for a step that sends it, none for another.
	out snapshot.Outbound
	// executed is set once the step ran (its when held); ran once its leg
	// ran, with the leg's Upstream, last Endpoint and attempts.
	executed, ran      bool
	upstream, endpoint string
	attempts           int
	method             string
	// sent and received bound the leg, for the response's current age.
	sent, received time.Time
	// The response: status and header, with the trailer and the unread
	// body of an Upstream response, or the leg Filter's response.
	status  int
	header  http.Header
	trailer http.Header
	stream  io.ReadCloser
	gen     *filter.Response
	// buf is the gated body and data its bytes (or a generated response's
	// literal body).
	buf  *body.Buffer
	data []byte
	// celBody is the body later steps read as steps.<name>.body; contrib
	// is the value the step merges.
	celBody expr.Value
	contrib any
	// acct charges the step's decoded values.
	acct body.Account
	// deferred is set for a non-2xx result of a step that merges nothing:
	// it fails the step only once the step is non-final.
	deferred bool
	// The failure: err carries its code; failGen is the leg Filter's
	// response a step failed with; consequence is set for a failure a
	// sibling's cancellation caused, which reports nothing.
	failed      bool
	err         error
	failGen     *filter.Response
	failAt      time.Time
	consequence bool
}

// failKind says whether a sibling's cancellation can cause a failure.
type failKind uint8

const (
	// failOwn is a failure of the step's own: a response, a refused path
	// or body, the budget, a fault.
	failOwn failKind = iota
	// failIO is a failure of an evaluation, leg or body read, which a
	// sibling's cancellation of the shared context also ends.
	failIO
)

// Forward runs the composition for one request (snapshot.Forwarder): the
// merged response, the final step's own, or the request's failure, an
// *Error carrying its code or the generated response a step failed with.
// Every goroutine it starts has ended when it returns, and only the
// returned body holds resources: the final step's leg body or the merged
// bytes, until the caller closes it.
func (c *Composition) Forward(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
	if o == nil || o.X == nil {
		return nil, &Error{Err: errcode.Wrap(CodeStepFailed, errNoExchange)}
	}
	r := c.e.getRun(c, o)
	defer c.e.putRun(r)
	switch c.mode {
	case v1alpha1.CompositionModeAggregate:
		return r.aggregate(ctx)
	case v1alpha1.CompositionModeSequential:
		return r.sequential(ctx)
	default:
		return r.conditional(ctx)
	}
}

// getRun returns a pooled run set up for c and o.
func (e *Engine) getRun(c *Composition, o *snapshot.Outbound) *run {
	r, _ := e.runs.Get().(*run)
	if r == nil {
		r = new(run)
	}
	r.c, r.o = c, o
	if n := len(c.steps); cap(r.res) < n {
		r.res = make([]result, n)
	} else {
		r.res = r.res[:n]
	}
	for i := range r.res {
		r.res[i].acct.Bind(e.budget, body.Gate, o.Stripe)
	}
	x := o.X
	r.incoming = x.Method()
	if v := x.Vars(); v != nil {
		r.vars = *v
	}
	r.vars.Steps = nil
	if c.mode == v1alpha1.CompositionModeSequential {
		r.vars.Steps = &r.steps
	}
	switch {
	case x.Message() != nil:
		r.query = x.Message().RawQuery()
	case r.vars.Request != nil:
		r.query = r.vars.Request.RawQuery()
	}
	return r
}

// putRun releases everything r still holds, the bodies and reservations
// of steps the response does not carry, and returns r to the pool.
func (e *Engine) putRun(r *run) {
	for i := range r.res {
		res := &r.res[i]
		closeBody(res.stream)
		if res.buf != nil {
			res.buf.Release()
		}
		res.acct.ReleaseAll()
		*res = result{}
	}
	r.steps.Reset()
	r.c, r.o = nil, nil
	r.vars = expr.Vars{}
	r.incoming, r.query, r.partial = "", "", false
	e.runs.Put(r)
}

// aggregate runs every step at once, one goroutine each, and merges their
// bodies. It takes one in-flight unit per step before the first starts; a
// failed step that is not optional cancels its siblings.
func (r *run) aggregate(ctx context.Context) (*snapshot.UpstreamResponse, error) {
	n := len(r.c.steps)
	if u := r.c.e.units; u != nil {
		if err := u.Acquire(int64(n)); err != nil {
			return nil, r.composite(err)
		}
	}
	sctx, cancel := context.WithCancelCause(ctx)
	r.wg.Add(n)
	for i := range n {
		go r.parallel(sctx, cancel, i)
	}
	r.wg.Wait()
	cancel(nil)
	if i := r.decide(); i >= 0 {
		return r.failure(i)
	}
	for i := range r.res {
		r.partial = r.partial || r.res[i].failed
	}
	return r.merged()
}

// parallel runs aggregate step i on its own goroutine; it returns the
// step's in-flight unit when the step ends.
func (r *run) parallel(ctx context.Context, cancel context.CancelCauseFunc, i int) {
	defer r.wg.Done()
	if u := r.c.e.units; u != nil {
		defer u.Release(1)
	}
	r.exec(ctx, i)
	if res := &r.res[i]; res.failed && !r.c.steps[i].optional && !res.consequence {
		cancel(errSibling)
	}
}

// decide returns the aggregate step whose failure fails the request: among
// the failed steps that are not optional, and whose failure no sibling's
// cancellation caused, the first to fail in time, ties going to list
// order; -1 when none failed.
func (r *run) decide() int {
	best := -1
	for i := range r.res {
		res := &r.res[i]
		if !res.failed || r.c.steps[i].optional || res.consequence {
			continue
		}
		if best < 0 || res.failAt.Before(r.res[best].failAt) {
			best = i
		}
	}
	return best
}

// sequential runs the steps in list order, skipping a step whose when is
// false. A step that got a response joins steps whatever its status; when
// a later step runs, the earlier one is non-final (settle). The response
// is the last executed step's own, unless that step merges: then every
// executed step that merges merges.
func (r *run) sequential(ctx context.Context) (*snapshot.UpstreamResponse, error) {
	last := -1
	for i := range r.c.steps {
		st := &r.c.steps[i]
		ok, err := r.when(ctx, st)
		if err != nil {
			if st.optional {
				continue
			}
			r.fail(ctx, i, errcode.Wrap(CodeStepFailed, err), nil, failIO)
			return r.failure(i)
		}
		if !ok {
			continue
		}
		if last >= 0 {
			if r.settle(ctx, last) {
				return r.failure(last)
			}
		}
		last = i
		r.exec(ctx, i)
		res := &r.res[i]
		if res.failed {
			if !st.optional {
				return r.failure(i)
			}
			r.partial = true
			continue
		}
		r.steps.Add(st.name, expr.Step{Status: res.status, Header: res.header, Body: res.celBody})
	}
	if last < 0 {
		return nil, r.composite(errcode.Errorf(CodeNoStep, "no step ran"))
	}
	if r.c.steps[last].merges {
		return r.merged()
	}
	if r.res[last].failed {
		// A final step that streams answers its own failure, optional or
		// not: nothing else is left to return.
		return r.failure(last)
	}
	return r.passthrough(last), nil
}

// settle makes executed step i non-final, because a later step runs: a
// body nothing reads is closed, and a deferred non-2xx result fails the
// step now. It reports whether that failure fails the request.
func (r *run) settle(ctx context.Context, i int) bool {
	res := &r.res[i]
	closeBody(res.stream)
	res.stream = nil
	if res.buf != nil && !r.c.steps[i].merges {
		// Gated only for later steps, which have decoded it: no passthrough
		// needs the bytes any more.
		res.buf.Release()
		res.buf, res.data = nil, nil
	}
	if res.failed || !res.deferred {
		return false
	}
	r.fail(ctx, i, r.statusError(i), r.statusResponse(i), failOwn)
	if r.c.steps[i].optional {
		r.partial = true
		return false
	}
	return true
}

// conditional runs the first step whose when is true and answers with its
// response, merged alone when the step merges.
func (r *run) conditional(ctx context.Context) (*snapshot.UpstreamResponse, error) {
	for i := range r.c.steps {
		st := &r.c.steps[i]
		ok, err := r.when(ctx, st)
		if err != nil {
			if st.optional {
				continue
			}
			r.fail(ctx, i, errcode.Wrap(CodeStepFailed, err), nil, failIO)
			return r.failure(i)
		}
		if !ok {
			continue
		}
		r.exec(ctx, i)
		res := &r.res[i]
		if st.merges {
			if res.failed {
				if !st.optional {
					return r.failure(i)
				}
				r.partial = true
			}
			return r.merged()
		}
		if res.failed {
			return r.failure(i)
		}
		return r.passthrough(i), nil
	}
	return nil, r.composite(errcode.Errorf(CodeNoStep, "no step's when is true"))
}

// when evaluates a step's when; true without one. A fault in the
// evaluation is a runtime error.
func (r *run) when(ctx context.Context, st *step) (ok bool, err error) {
	if st.when == nil {
		return true, nil
	}
	defer func() {
		if v := recover(); v != nil {
			r.logPanic(ctx, st, v)
			ok, err = false, errPanic
		}
	}()
	return st.when.EvalBool(ctx, &r.vars)
}

// fail records step i's failure at the clock's reading now. A failIO
// failure after a sibling canceled the shared context is its consequence.
func (r *run) fail(ctx context.Context, i int, err error, gen *filter.Response, k failKind) {
	res := &r.res[i]
	res.failed, res.err, res.failGen = true, err, gen
	res.failAt = r.c.e.clk.Now()
	res.consequence = k == failIO && errors.Is(context.Cause(ctx), errSibling)
}

// failure returns the request's failure decided by step i: the generated
// response it failed with, or an *Error with its code.
func (r *run) failure(i int) (*snapshot.UpstreamResponse, error) {
	res := &r.res[i]
	upstream, endpoint, attempts := r.last()
	if g := res.failGen; g != nil {
		return &snapshot.UpstreamResponse{Status: g.Status, Generated: g, Upstream: upstream, Endpoint: endpoint, Attempts: attempts}, nil
	}
	return nil, &Error{Step: r.c.steps[i].name, Upstream: upstream, Endpoint: endpoint, Attempts: attempts, Err: res.err}
}

// composite returns the composition's own failure, carrying err's code.
func (r *run) composite(err error) error {
	upstream, endpoint, attempts := r.last()
	return &Error{Upstream: upstream, Endpoint: endpoint, Attempts: attempts, Err: err}
}

// last returns the Upstream, last Endpoint and attempts of the last step
// in list order whose leg ran.
func (r *run) last() (upstream, endpoint string, attempts int) {
	for i := len(r.res) - 1; i >= 0; i-- {
		if res := &r.res[i]; res.ran {
			return res.upstream, res.endpoint, res.attempts
		}
	}
	return "", "", 0
}

// logPanic records a recovered step fault: the Route, the step's
// Upstream, the panic value's type (never its text, which may hold
// request data) and the stack.
func (r *run) logPanic(ctx context.Context, st *step, v any) {
	r.c.e.log.LogAttrs(ctx, slog.LevelError, "composition step panic recovered",
		slog.String(catalog.KeyResourceKind, string(v1alpha1.KindRoute)),
		slog.String(catalog.KeyResourceName, r.c.route),
		slog.String(catalog.KeyUpstream, st.upstream),
		slog.String(catalog.KeyPanicType, fmt.Sprintf("%T", v)),
		slog.String(catalog.KeyStack, string(debug.Stack())))
}

// closeBody closes a body the request does not need.
func closeBody(rc io.ReadCloser) {
	if rc != nil {
		_ = rc.Close()
	}
}
