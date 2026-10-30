// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// Causes a call's context is canceled with when a deadline passes.
var (
	errHeaderTimeout = errors.New("response header deadline expired")
	errCallTimeout   = errors.New("call deadline expired")
)

// errTooLarge marks a body over its bound.
var errTooLarge = errors.New("body over its bound")

// inflight is one admin call: the cancel of its context, its header and
// call deadlines, the deadline that expired, and its operation for
// messages. It holds no context: the caller's context is passed to each
// method that classifies a failure.
type inflight struct {
	c      *Client
	op     string // "GET <url>"
	cancel context.CancelCauseFunc

	mu          sync.Mutex
	headerTimer clock.Timer
	callTimer   clock.Timer
	headersDone bool
	finished    bool
	// expired is the first deadline that canceled the call, "" for none.
	expired DeadlineKind
}

// roundTrip sends one GET to u and returns the response with its call,
// which the caller finishes once it has read the body or the stream ends.
// The token file is read again and a fresh traceparent drawn for the
// request. The header deadline starts once the transport holds the
// connection; a call that does not stream also runs under the client's
// call deadline from the start.
func (c *Client) roundTrip(ctx context.Context, u *url.URL, stream bool) (*http.Response, *inflight, error) {
	op := http.MethodGet + " " + u.String()
	if ctx.Err() != nil {
		return nil, nil, canceled(ctx, op)
	}
	token, err := c.readToken()
	if err != nil {
		return nil, nil, err
	}
	tp, err := c.traceparent()
	if err != nil {
		return nil, nil, command.NoResultf("%s: draw a trace ID: %w", op, err)
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	fl := &inflight{c: c, op: op, cancel: cancel}
	if !stream && c.timeout > 0 {
		fl.callTimer = c.clock.AfterFunc(c.timeout, func() { fl.expire(DeadlineCall, errCallTimeout) })
	}
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { fl.armHeaderTimer() }}
	req, err := http.NewRequestWithContext(httptrace.WithClientTrace(callCtx, trace), http.MethodGet, u.String(), nil)
	if err != nil {
		fl.finish()
		return nil, nil, command.NoResultf("%s: %w", op, err)
	}
	req.Header.Set("User-Agent", "ruralz/"+c.version)
	req.Header.Set("Traceparent", tp)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.transport.RoundTrip(req)
	fl.headersArrived()
	if err != nil {
		err = fl.failure(ctx, err)
		fl.finish()
		return nil, nil, err
	}
	c.checkSkew(resp.Header)
	return resp, fl, nil
}

// armHeaderTimer starts the header deadline once the connection is
// established; the transport's GotConn hook calls it on the call's
// goroutine before the request is written.
func (fl *inflight) armHeaderTimer() {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.headersDone || fl.finished || fl.headerTimer != nil {
		return
	}
	fl.headerTimer = fl.c.clock.AfterFunc(HeaderTimeout, func() { fl.expire(DeadlineHeaders, errHeaderTimeout) })
}

// headersArrived stops the header deadline once the round trip returned.
func (fl *inflight) headersArrived() {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	fl.headersDone = true
	if fl.headerTimer != nil {
		fl.headerTimer.Stop()
	}
}

// expire records the first deadline that passed and cancels the call.
func (fl *inflight) expire(k DeadlineKind, cause error) {
	fl.mu.Lock()
	if fl.expired == "" {
		fl.expired = k
	}
	fl.mu.Unlock()
	fl.cancel(cause)
}

// finish ends the call: its deadlines stop, its context is canceled, so a
// stream's read returns, and the transport closes what the call left, a
// dial it stopped waiting for included. It is idempotent.
func (fl *inflight) finish() {
	fl.mu.Lock()
	if fl.finished {
		fl.mu.Unlock()
		return
	}
	fl.finished = true
	timers := [...]clock.Timer{fl.headerTimer, fl.callTimer}
	fl.mu.Unlock()
	for _, t := range timers {
		if t != nil {
			t.Stop()
		}
	}
	fl.cancel(nil)
	fl.c.transport.CloseIdleConnections()
}

// failure classifies the failure err of a call whose caller's context is
// ctx: the end of ctx first, by Ctrl-C or the caller; then a missed
// connection deadline, which the dialer reports; then the header or call
// deadline that canceled the call; anything else exits 2 as it is.
func (fl *inflight) failure(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return canceled(ctx, fl.op)
	}
	if de, ok := errors.AsType[*DeadlineError](err); ok {
		return command.NoResult(de)
	}
	fl.mu.Lock()
	expired := fl.expired
	fl.mu.Unlock()
	switch expired {
	case DeadlineHeaders:
		return command.NoResult(&DeadlineError{Kind: DeadlineHeaders, Op: fl.op, Limit: HeaderTimeout})
	case DeadlineCall:
		return command.NoResult(&DeadlineError{Kind: DeadlineCall, Op: fl.op, Limit: fl.c.timeout, Flag: fl.c.names.Timeout})
	default:
		return command.NoResultf("%s: %w", fl.op, err)
	}
}

// canceled is the error of a call whose caller's context ended: it wraps
// the context's cause, so after a signal command.Code maps it to the
// signal's exit code and Execute prints nothing more.
func canceled(ctx context.Context, op string) error {
	return command.NoResultf("%s: %w", op, context.Cause(ctx))
}

// accept reports whether a response carries the endpoint's own body.
type accept func(*http.Response) bool

// ok200 accepts only 200, the answer of /config/dump and /tap.
func ok200(resp *http.Response) bool { return resp.StatusCode == http.StatusOK }

// check classifies resp: nil when ok accepts it; else a 3xx is a
// RedirectError, a problem document a ProblemError and anything else a
// StatusError, each exiting 2.
func (fl *inflight) check(ctx context.Context, resp *http.Response, ok accept) error {
	switch {
	case ok(resp):
		return nil
	case resp.StatusCode >= 300 && resp.StatusCode <= 399:
		return command.NoResult(&RedirectError{Op: fl.op, Status: resp.StatusCode, Location: resp.Header.Get("Location")})
	case isProblem(resp.Header):
		return fl.problem(ctx, resp)
	default:
		return command.NoResult(&StatusError{Op: fl.op, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type")})
	}
}

// isProblem reports whether h declares an RFC 9457 problem document.
func isProblem(h http.Header) bool {
	mt, _, err := mime.ParseMediaType(h.Get("Content-Type"))
	return err == nil && mt == problem.ContentType
}

// problem reads the problem document of resp, at most MaxProblemBytes.
func (fl *inflight) problem(ctx context.Context, resp *http.Response) error {
	raw, err := readBounded(resp.Body, resp.ContentLength, fl.c.limits.problem)
	switch {
	case errors.Is(err, errTooLarge):
		return command.NoResult(&StatusError{
			Op: fl.op, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"),
			Reason: "the problem document exceeds " + size(fl.c.limits.problem),
		})
	case err != nil:
		return fl.failure(ctx, fmt.Errorf("read the problem document: %w", err))
	}
	doc, err := decodeProblem(raw)
	if err != nil {
		return command.NoResult(&StatusError{
			Op: fl.op, Status: resp.StatusCode, ContentType: resp.Header.Get("Content-Type"), Reason: err.Error(),
		})
	}
	return command.NoResult(&ProblemError{Status: resp.StatusCode, Doc: doc, raw: raw})
}

// decodeProblem decodes a problem document, which must be a JSON object.
func decodeProblem(raw []byte) (problem.Document, error) {
	if t := bytes.TrimLeft(raw, " \t\r\n"); len(t) == 0 || t[0] != '{' {
		return problem.Document{}, errors.New("problem document: not a JSON object")
	}
	return problem.Decode(raw)
}

// body reads the endpoint body of resp, at most limit bytes, and
// classifies a failure; path names the endpoint in messages.
func (fl *inflight) body(ctx context.Context, resp *http.Response, path string, limit int64) ([]byte, error) {
	b, err := readBounded(resp.Body, resp.ContentLength, limit)
	switch {
	case errors.Is(err, errTooLarge):
		return nil, command.NoResultf("%s: the %s body exceeds %s", fl.op, path, size(limit))
	case err != nil:
		return nil, fl.failure(ctx, fmt.Errorf("read the body: %w", err))
	}
	return b, nil
}

// readBounded reads r to its end, refusing more than limit bytes; a
// Content-Length (contentLength, -1 when unknown) over limit is refused
// before anything is read, and a known one sizes the buffer.
func readBounded(r io.Reader, contentLength, limit int64) ([]byte, error) {
	if contentLength > limit {
		return nil, errTooLarge
	}
	capacity := int64(512)
	if contentLength >= 0 {
		// One spare byte reads the end of the body without growing.
		capacity = contentLength + 1
	}
	b := make([]byte, 0, capacity)
	lr := io.LimitReader(r, limit+1)
	for {
		if len(b) == cap(b) {
			b = append(b, 0)[:len(b)]
		}
		n, err := lr.Read(b[len(b):cap(b)])
		b = b[:len(b)+n]
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if int64(len(b)) > limit {
		return nil, errTooLarge
	}
	return b, nil
}
