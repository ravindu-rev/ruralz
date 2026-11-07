// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Error types a Filter names for the span's error.type
// (filter.Result.Err).
const (
	errTypeCEL          = "cel_error"
	errTypeResultType   = "result_type"
	errTypeInvalidValue = "invalid_value"
	errTypeNullBody     = "null_body"
	errTypePath         = "path_error"
	errTypeOutputCap    = "output_cap"
	errTypeInternal     = "internal"
)

// Causes of cannot decide. Their text never holds response data.
var (
	errNoMessage = errors.New("no response to sign into")
	errNoKey     = errors.New("no signing key")
	errNullBody  = errors.New("the body is not JSON")
	errOutputCap = errors.New("the body would pass its output cap")
)

// maxScratch is the largest buffer a pooled scratch keeps between calls,
// so a Filter never pins a large body.
const maxScratch = 64 << 10

// Filter is a compiled auth.jwt-sign Policy. It signs in onResponse only
// and is safe for concurrent use; its key follows rotations of
// config.signingKey through its holder.
type Filter struct {
	signer signer
	// paths is config.paths, in order; claims and target are config.claims
	// and config.target. Exactly one mode is set.
	paths  []dotpath.Path
	claims expr.Program
	target dotpath.Path
	// limit caps the rewritten body: limits.maxResponseBodyBytes.
	limit int64
	keys  *holder

	closed atomic.Bool
	pool   sync.Pool // *scratch
}

var (
	_ filter.Filter = (*Filter)(nil)
	_ filter.Closer = (*Filter)(nil)
)

// Handle signs into the client response in onResponse and continues in
// every other Phase. A response whose body may not be rewritten (package
// documentation, "Body") and a generated one pass unchanged; a failure of
// the package documentation ("Failures") cannot decide with the class
// default code and leaves the response as it was, and a spent buffer
// budget or an oversized body returns the SPI sentinel.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != phase.OnResponse {
		return filter.Next()
	}
	m := x.Message()
	if m == nil || m.Header() == nil {
		return filter.Undecided("", errNoMessage)
	}
	if m.Generated() || !transform.BodyRewritable(x, m) {
		return filter.Next()
	}
	k := f.keys.current()
	if k == nil {
		return filter.Undecided("", &signError{kind: errTypeInternal, err: errNoKey})
	}
	s := f.get()
	defer f.put(s)
	r := run{f: f, ctx: ctx, x: x, m: m, s: s, k: k, now: x.Now()}
	var err error
	if f.claims != nil {
		err = r.claimsMode()
	} else {
		err = r.pathsMode()
	}
	if err != nil {
		return filter.Undecided("", err)
	}
	return filter.Next()
}

// Close stops the key's watch once no Filter shares its holder.
func (f *Filter) Close() error {
	if f.closed.CompareAndSwap(false, true) && f.keys != nil {
		f.keys.release()
	}
	return nil
}

// scratch is the pooled working state of one call.
type scratch struct {
	ed dotpath.Editor
	sc jsonval.Scanner
	// buf holds the documents of a call: the current document and the
	// next one.
	buf [2][]byte
	// val holds the claims of config.claims and tok its token.
	val, tok []byte
	rw       rewriter
}

// get takes a scratch from the pool.
func (f *Filter) get() *scratch {
	s, _ := f.pool.Get().(*scratch)
	if s == nil {
		s = &scratch{}
	}
	return s
}

// put drops every reference s holds to response data, and every buffer
// over maxScratch, and returns it to the pool.
func (f *Filter) put(s *scratch) {
	for i := range s.buf {
		s.buf[i] = trim(s.buf[i])
	}
	s.val, s.tok = trim(s.val), trim(s.tok)
	s.rw = rewriter{}
	f.pool.Put(s)
}

// trim returns b emptied, or nil when its capacity is over maxScratch.
func trim(b []byte) []byte {
	if cap(b) > maxScratch {
		return nil
	}
	return b[:0]
}

// run is one call's state.
type run struct {
	f   *Filter
	ctx context.Context
	x   filter.Exchange
	m   filter.Message
	s   *scratch
	k   *key
	now time.Time
}

// limit returns the Editor limit of the output cap, 0 for none.
func (r *run) limit() int {
	if r.f.limit <= 0 {
		return 0
	}
	return int(min(r.f.limit, math.MaxInt))
}

// body returns the response body as a normalized JSON document; empty
// reports an empty body. A body that is not JSON, by its media type or its
// bytes, is null_body.
func (r *run) body() (doc []byte, empty bool, err error) {
	b, err := r.m.Body(r.ctx)
	if err != nil {
		if errors.Is(err, filter.ErrBudget) || errors.Is(err, filter.ErrTooLarge) {
			return nil, false, err
		}
		return nil, false, &signError{kind: errTypeInternal, err: fmt.Errorf("body: %w", err)}
	}
	if len(b) == 0 {
		return nil, true, nil
	}
	if !transform.JSONContent(r.m.Header()) {
		return nil, false, &signError{kind: errTypeNullBody, err: errNullBody}
	}
	r.s.ed.Limit = r.limit()
	out, err := r.s.ed.Normalize(r.s.buf[0][:0], b)
	r.s.buf[0] = out
	if err != nil {
		return nil, false, r.editFail("body", err)
	}
	return out, false, nil
}

// pathsMode replaces each object config.paths matches with its token, the
// paths in order, each over the document the one before it wrote. An
// empty body and a body no path matches pass unchanged.
func (r *run) pathsMode() error {
	doc, empty, err := r.body()
	if err != nil || empty {
		return err
	}
	cur := 0
	r.s.rw = rewriter{run: r}
	changed := false
	for i, p := range r.f.paths {
		next := 1 - cur
		out, n, err := r.s.ed.RewriteObjects(r.s.buf[next][:0], doc, p, &r.s.rw)
		r.s.buf[next] = out
		if err != nil {
			return r.editFail(fmt.Sprintf("config.paths[%d]", i), err)
		}
		if n > 0 {
			doc, cur, changed = out, next, true
		}
	}
	if !changed {
		return nil
	}
	return r.commit(doc, false)
}

// claimsMode builds one token from config.claims and writes it at
// config.target, creating missing objects; an empty body starts from an
// empty object.
func (r *run) claimsMode() error {
	doc, empty, err := r.body()
	if err != nil {
		return err
	}
	val, err := r.f.claims.EvalValue(r.x.Vars())
	if err != nil {
		return r.evalError(err)
	}
	nj, ok := val.(expr.NestedJSON)
	if !ok {
		return &signError{at: "config.claims", kind: errTypeResultType, err: errNotClaims}
	}
	claims, err := nj.AppendJSON(r.s.val[:0])
	r.s.val = claims
	if err != nil {
		return &signError{at: "config.claims", kind: errTypeResultType, err: err}
	}
	if len(claims) == 0 || claims[0] != '{' {
		return &signError{at: "config.claims", kind: errTypeResultType, err: errNotClaims}
	}
	tok, err := r.token(r.s.tok[:0], claims)
	r.s.tok = tok
	if err != nil {
		return err
	}
	if empty {
		r.s.buf[0] = append(r.s.buf[0][:0], '{', '}')
		doc = r.s.buf[0]
	}
	r.s.ed.Limit = r.limit()
	out, n, err := r.s.ed.Set(r.s.buf[1][:0], doc, r.f.target, tok)
	r.s.buf[1] = out
	if err != nil {
		return r.editFail("config.target", err)
	}
	if n == 0 {
		return nil
	}
	return r.commit(out, empty)
}

// token appends to dst the JSON string of the token of claims, a claims
// object: its exp must be a NumericDate at most MaxExpiry ahead of the
// request's start.
func (r *run) token(dst, claims []byte) ([]byte, error) {
	if err := r.ctx.Err(); err != nil {
		return dst, err
	}
	if err := checkExp(&r.s.sc, claims, r.now); err != nil {
		return dst, &signError{at: "claims", kind: errTypeInvalidValue, err: err}
	}
	start := len(dst)
	dst = append(dst, '"')
	dst, err := r.f.signer.appendToken(dst, r.k, claims)
	if err != nil {
		return dst[:start], &signError{at: "signature", kind: errTypeInternal, err: err}
	}
	return append(dst, '"'), nil
}

// evalError classifies a runtime error of config.claims: result_type for
// a result of the wrong type, the SPI sentinel when response.body could
// not be decoded for lack of buffer budget or past its decoded limit, else
// cel_error.
func (r *run) evalError(err error) error {
	kind := errTypeCEL
	var ee *expr.EvalError
	if errors.As(err, &ee) {
		switch ee.Kind {
		case expr.KindResultType:
			kind = errTypeResultType
		case expr.KindBody:
			if _, derr := r.m.Decoded(r.ctx); errors.Is(derr, filter.ErrBudget) || errors.Is(derr, filter.ErrTooLarge) {
				return derr
			}
		default:
		}
	}
	return &signError{at: "config.claims", kind: kind, err: err}
}

// editFail classifies an Editor error at the field at: path_error,
// output_cap, a Rewriter's own error as it is, and null_body for a body
// that is not JSON.
func (r *run) editFail(at string, err error) error {
	var se *signError
	switch {
	case errors.As(err, &se), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return err
	case errors.Is(err, dotpath.ErrPath):
		return &signError{at: at, kind: errTypePath, err: err}
	case errors.Is(err, dotpath.ErrOutputCap):
		return &signError{at: at, kind: errTypeOutputCap, err: errOutputCap}
	default:
		return &signError{at: at, kind: errTypeNullBody, err: errNullBody}
	}
}

// commit gives the response its rewritten body, the call's own copy, and
// applies the validators rule; created marks a body written where there
// was none, which takes Content-Type application/json.
func (r *run) commit(doc []byte, created bool) error {
	body := bytes.Clone(doc)
	if err := r.m.SetBody(r.ctx, body); err != nil {
		if errors.Is(err, filter.ErrBudget) || errors.Is(err, filter.ErrTooLarge) {
			return err
		}
		return &signError{at: "body", kind: errTypeInternal, err: err}
	}
	transform.MarkRewritten(r.m.Header(), created)
	return nil
}

// rewriter replaces one claims object config.paths matches with its
// token (dotpath.Rewriter).
type rewriter struct{ run *run }

// Rewrite appends the JSON string of obj's token.
func (w *rewriter) Rewrite(dst, obj []byte) ([]byte, error) {
	return w.run.token(dst, obj)
}

// signError is a failure of one field or step: a CEL runtime error, a
// claims refusal, a path error, a body that is not JSON, an output over
// its cap or a signature that could not be computed. It names the span's
// error.type; its text names the field and the failure, never response
// data.
type signError struct {
	at   string
	kind string
	err  error
}

// Error returns the Policy type, the field and the cause's text.
func (e *signError) Error() string {
	if e.at == "" {
		return string(Type) + ": " + e.err.Error()
	}
	return string(Type) + ": " + e.at + ": " + e.err.Error()
}

// Unwrap returns the cause.
func (e *signError) Unwrap() error { return e.err }

// ErrorType returns the span's error.type.
func (e *signError) ErrorType() string { return e.kind }
