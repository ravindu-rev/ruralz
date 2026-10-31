// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/routematch"
)

// headerPartial is the field a response carries when an optional step was
// left out.
const headerPartial = "Ruralz-Partial"

// exec runs step i: its target, its leg, and, for a step that gates its
// body, the body read whole, decoded for later steps and shaped for the
// merge. The outcome is in r.res[i]; a fault in the step is recovered as
// its failure (RZ-RT-015).
func (r *run) exec(ctx context.Context, i int) {
	st, res := &r.c.steps[i], &r.res[i]
	res.executed = true
	defer func() {
		if v := recover(); v != nil {
			r.logPanic(ctx, st, v)
			r.fail(ctx, i, errcode.Wrap(CodeStepFailed, errPanic), nil, failOwn)
		}
	}()
	path, query, kind, err := r.target(ctx, st)
	if err != nil {
		r.fail(ctx, i, err, nil, kind)
		return
	}
	method := st.method
	if method == "" {
		method = r.incoming
	}
	res.method = method
	res.out = *r.o
	if method != r.incoming {
		// Only a step whose method is the incoming one sends the client
		// body.
		res.out.Body = nil
	}
	t := upstream.Target{Step: st.name, Method: method, Path: path, RawQuery: query, MaxBodyBytes: st.maxBody}
	clk := r.c.e.clk
	res.sent = clk.Now()
	resp, err := st.leg.Leg(ctx, &res.out, t)
	res.received = clk.Now()
	res.ran, res.upstream = true, st.upstream
	if err != nil {
		r.legFailed(ctx, i, err)
		return
	}
	r.responded(ctx, i, resp)
}

// target returns step st's path and raw query: its literal path, or its
// pathExpression result split at the first "?", the path checked with the
// hardening of the client path and the query for raw control bytes;
// without a "?", the client's raw query. A failure says whether a
// sibling's cancellation can have caused it.
func (r *run) target(ctx context.Context, st *step) (path, query string, kind failKind, err error) {
	if st.pathExpr == nil {
		if st.hasQuery {
			return st.path, st.query, failOwn, nil
		}
		return st.path, r.query, failOwn, nil
	}
	s, err := r.evalPath(ctx, st)
	if err != nil {
		return "", "", failIO, errcode.Wrap(CodeStepFailed, err)
	}
	path, query, hasQuery := strings.Cut(s, "?")
	if err := routematch.CheckForwardPath(path); err != nil {
		return "", "", failOwn, errcode.Errorf(CodeStepFailed, "computed path refused: %w", err)
	}
	if !hasQuery {
		return path, r.query, failOwn, nil
	}
	if err := routematch.CheckForwardQuery(query); err != nil {
		return "", "", failOwn, errcode.Errorf(CodeStepFailed, "computed query refused: %w", err)
	}
	return path, query, failOwn, nil
}

// evalPath evaluates a step's pathExpression; a fault in the evaluation is
// a runtime error.
func (r *run) evalPath(ctx context.Context, st *step) (s string, err error) {
	defer func() {
		if v := recover(); v != nil {
			r.logPanic(ctx, st, v)
			s, err = "", errPanic
		}
	}()
	return st.pathExpr.EvalString(ctx, &r.vars)
}

// legFailed records a leg that ended without a response: its code, or
// RZ-RT-015 for an error without one.
func (r *run) legFailed(ctx context.Context, i int, err error) {
	res := &r.res[i]
	if le := (*upstream.LegError)(nil); errors.As(err, &le) {
		res.upstream, res.endpoint, res.attempts = le.Upstream, le.Endpoint, le.Attempts
	}
	if _, ok := errcode.CodeOf(err); !ok {
		err = errcode.Wrap(CodeStepFailed, err)
	}
	r.fail(ctx, i, err, nil, failIO)
}

// responded handles a step's response: a non-2xx result fails a step
// that merges at once and is deferred for one that does not; a declared
// length over maxBodyBytes fails it before any byte is read; a step that
// gates its body reads it, decodes it for later steps and shapes it for
// the merge, while any other keeps it unread for a possible passthrough.
func (r *run) responded(ctx context.Context, i int, resp *snapshot.UpstreamResponse) {
	st, res := &r.c.steps[i], &r.res[i]
	res.upstream, res.endpoint, res.attempts = resp.Upstream, resp.Endpoint, resp.Attempts
	if res.upstream == "" {
		res.upstream = st.upstream
	}
	if g := resp.Generated; g != nil {
		res.gen, res.status, res.header = g, g.Status, g.Header
		closeBody(resp.Body)
	} else {
		res.status, res.header, res.trailer, res.stream = resp.Status, resp.Header, resp.Trailer, resp.Body
	}
	if res.status < 200 || res.status > 299 {
		if st.merges {
			r.drop(res)
			r.fail(ctx, i, r.statusError(i), r.statusResponse(i), failOwn)
			return
		}
		res.deferred = true
	}
	declared := r.declared(res)
	if declared > st.maxBody {
		r.drop(res)
		r.fail(ctx, i, errcode.Errorf(CodeStepFailed, "declared body of %d bytes over maxBodyBytes (%d)", declared, st.maxBody), nil, failOwn)
		return
	}
	if !st.gate() {
		return
	}
	if err := r.gateBody(ctx, i, declared); err != nil {
		r.bodyFailed(ctx, i, err)
		return
	}
	if st.readLater {
		if err := r.decodeForSteps(i); err != nil {
			r.fail(ctx, i, err, nil, failOwn)
			return
		}
	}
	if st.merges {
		if err := r.contribute(i); err != nil {
			r.fail(ctx, i, err, nil, failOwn)
			return
		}
		// The merge holds decoded values, not the body's bytes.
		if res.buf != nil {
			res.buf.Release()
		}
		res.buf, res.data = nil, nil
	}
}

// declared returns the length a response declares for its body: a
// generated response's literal body, else a valid Content-Length; 0 for a
// response that cannot have content (to a HEAD, or 1xx, 204 or 304), and
// -1 when unknown.
func (r *run) declared(res *result) int64 {
	switch {
	case res.method == http.MethodHead, res.status < 200, res.status == http.StatusNoContent, res.status == http.StatusNotModified:
		return 0
	case res.gen != nil:
		return int64(len(res.gen.Body))
	}
	lines := res.header["Content-Length"]
	if len(lines) != 1 {
		return -1
	}
	n, err := strconv.ParseInt(lines[0], 10, 64)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

// gateBody reads step i's body whole within its maxBodyBytes, reserved
// from the buffer budget (body.ReadGate), and closes the leg's body, which
// ends its attempt.
func (r *run) gateBody(ctx context.Context, i int, declared int64) error {
	res := &r.res[i]
	if res.gen != nil {
		if declared > 0 {
			res.data = res.gen.Body
		}
		return nil
	}
	stream := res.stream
	res.stream = nil
	if declared == 0 {
		closeBody(stream)
		return nil
	}
	buf, err := body.ReadGate(ctx, stream, declared, r.c.steps[i].maxBody, r.c.e.budget, r.o.Stripe)
	closeBody(stream)
	if err != nil {
		return err
	}
	res.buf, res.data = buf, buf.Bytes()
	return nil
}

// bodyFailed records a failed body read: over maxBodyBytes is RZ-RT-015,
// a spent budget RZ-RT-004, and a read that failed an Upstream error,
// RZ-UP-003 when a deadline ended it, else RZ-UP-004 (a body cut before
// its end is no response).
func (r *run) bodyFailed(ctx context.Context, i int, err error) {
	switch {
	case errors.Is(err, body.ErrTooLarge):
		r.fail(ctx, i, errcode.Errorf(CodeStepFailed, "body over maxBodyBytes (%d): %w", r.c.steps[i].maxBody, err), nil, failOwn)
	case errors.Is(err, body.ErrBudget):
		r.fail(ctx, i, errcode.Wrap(CodeBudget, err), nil, failOwn)
	default:
		code := resilience.CodeReset
		if resilience.Classify(ctx, err) == resilience.KindTimeout {
			code = resilience.CodeTimeout
		}
		r.fail(ctx, i, errcode.Errorf(code, "read the step body: %w", err), nil, failIO)
	}
}

// decodeForSteps decodes step i's body for later steps
// (steps.<name>.body), within filter.DecodedLimitFactor times its
// maxBodyBytes and charged to the buffer budget: JSON null for a body that
// is empty, not JSON by its one Content-Type field line, or not valid
// JSON, as request.body reads (docs/architecture/03-data-plane.md
// "Transform Policies").
func (r *run) decodeForSteps(i int) error {
	st, res := &r.c.steps[i], &r.res[i]
	res.celBody = nullBody{}
	dec := r.c.e.dec
	if len(res.data) == 0 || !jsonBody(res.header) || dec == nil {
		return nil
	}
	v, built, err := dec.DecodeJSON(res.data, body.DecodedCap(st.maxBody))
	switch {
	case errors.Is(err, expr.ErrTooLarge):
		return errcode.Errorf(CodeStepFailed, "decoded body over %d times maxBodyBytes: %w", filter.DecodedLimitFactor, err)
	case err != nil, v == nil:
		return nil //nolint:nilerr // a body that is not valid JSON reads as JSON null, not as a failed step.
	}
	if err := res.acct.ReserveDecoded(built, st.maxBody); err != nil {
		return chargeError(err)
	}
	res.celBody = v
	return nil
}

// contribute decodes step i's body for the merge under the Transform
// Policies rule and shapes it: the value at target, an array of objects
// with collection, else an object, with select and rename applied.
func (r *run) contribute(i int) error {
	st, res := &r.c.steps[i], &r.res[i]
	switch {
	case len(res.data) == 0:
		return errcode.Errorf(CodeStepFailed, "merged body is empty")
	case !jsonBody(res.header):
		return errcode.Errorf(CodeStepFailed, "merged body is not JSON by its Content-Type")
	}
	tree, cost, err := jsonval.Decode(res.data, jsonval.Options{MaxCost: body.DecodedCap(st.maxBody)})
	switch {
	case errors.Is(err, jsonval.ErrTooLarge):
		return errcode.Errorf(CodeStepFailed, "decoded body over %d times maxBodyBytes: %w", filter.DecodedLimitFactor, err)
	case err != nil:
		return errcode.Errorf(CodeStepFailed, "merged body is not valid JSON: %w", err)
	}
	if err := res.acct.ReserveDecoded(cost, st.maxBody); err != nil {
		return chargeError(err)
	}
	v, err := st.shape(tree)
	if err != nil {
		return errcode.Wrap(CodeStepFailed, err)
	}
	res.contrib = v
	return nil
}

// chargeError maps a refused charge of a decoded value: past its cap
// RZ-RT-015, a spent budget RZ-RT-004.
func chargeError(err error) error {
	if errors.Is(err, body.ErrTooLarge) {
		return errcode.Wrap(CodeStepFailed, err)
	}
	return errcode.Wrap(CodeBudget, err)
}

// statusError is the failure of step i's non-2xx result: RZ-UP-011 for an
// Upstream's, the code of a leg Filter's response otherwise.
func (r *run) statusError(i int) error {
	res := &r.res[i]
	if g := res.gen; g != nil && g.Code != "" {
		return errcode.Errorf(g.Code, "step response %d", g.Status)
	}
	if res.gen != nil {
		return fmt.Errorf("composition: step response %d", res.status)
	}
	return errcode.Errorf(CodeStepStatus, "step response %d", res.status)
}

// statusResponse is the leg Filter's response a non-2xx step result fails
// the request with, its own status and code; nil for an Upstream's.
func (r *run) statusResponse(i int) *filter.Response { return r.res[i].gen }

// drop closes the unread body of a step the request no longer needs.
func (r *run) drop(res *result) {
	closeBody(res.stream)
	res.stream = nil
}

// passthrough returns final step i's own response: a leg Filter's, the
// gated bytes, or the leg's body streamed and cut past maxBodyBytes with
// RZ-UP-009. It carries ruralz-partial: true when an optional step was
// left out.
func (r *run) passthrough(i int) *snapshot.UpstreamResponse {
	st, res := &r.c.steps[i], &r.res[i]
	upstreamName, endpoint, attempts := r.last()
	if g := res.gen; g != nil {
		if r.partial {
			g = withPartial(g)
		}
		return &snapshot.UpstreamResponse{Status: g.Status, Generated: g, Partial: r.partial, Upstream: upstreamName, Endpoint: endpoint, Attempts: attempts}
	}
	h := res.header
	if h == nil {
		h = make(http.Header, 1)
	}
	if r.partial {
		h[headerPartial] = []string{"true"}
	}
	out := &snapshot.UpstreamResponse{
		Status: res.status, Header: h, Trailer: res.trailer, Partial: r.partial,
		Upstream: upstreamName, Endpoint: endpoint, Attempts: attempts,
	}
	switch {
	case res.buf != nil:
		out.Body = &heldBody{buf: res.buf, data: res.data}
		res.buf, res.data = nil, nil
	case res.stream != nil:
		out.Body = newLimitedBody(res.stream, st.maxBody)
		res.stream = nil
	default:
		out.Body = &heldBody{}
	}
	return out
}

// withPartial returns a copy of g that carries ruralz-partial: true.
func withPartial(g *filter.Response) *filter.Response {
	c := *g
	c.Header = make(http.Header, len(g.Header)+1)
	for k, vs := range g.Header {
		c.Header[k] = vs
	}
	c.Header[headerPartial] = []string{"true"}
	return &c
}

// jsonBody reports whether h has exactly one Content-Type field line and
// it names JSON (jsonval.IsJSONMediaType); a repeated one is ambiguous.
func jsonBody(h http.Header) bool {
	lines := h["Content-Type"]
	return len(lines) == 1 && jsonval.IsJSONMediaType(lines[0])
}

// nullBody is JSON null, what a later step reads as the body of a step
// whose body is not JSON.
type nullBody struct{}

// IsNull reports true.
func (nullBody) IsNull() bool { return true }

// AppendBody appends null.
func (nullBody) AppendBody(dst []byte) ([]byte, error) { return append(dst, "null"...), nil }

// Native returns nil, JSON null.
func (nullBody) Native() (any, error) { return nil, nil }
