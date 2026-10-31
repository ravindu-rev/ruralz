// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/exchange"
	"github.com/ravindu-rev/ruralz/internal/gateway/executor"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/retire"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// epoch is the fake clock's start for tests that never touch a socket.
func epoch() time.Time { return time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC) }

// kit is a Handler over real Node parts (retire.Holder, admission,
// exchange pool, executor) with recording telemetry, on a fake clock.
type kit struct {
	t      *testing.T
	clk    *clocktest.Fake
	holder *retire.Holder
	units  *admission.Units
	pre    *admission.PreRouting
	budget *body.Budget
	tracer *handlertest.Tracer
	alog   *handlertest.AccessLog
	node   *handlertest.Node
	plan   *handlertest.Plan
	conn   *handlertest.Conn
	tap    *tap.Hub
	logs   *logCapture
	h      *Handler
}

// kitOptions changes a kit before its Handler is built.
type kitOptions struct {
	units      int64
	preRouting int64
	start      time.Time
	noConn     bool
	// wall, when set, is the handler's wall clock, apart from its clock.
	wall *clocktest.Fake
}

// newKit returns a kit whose connection comes from 192.0.2.10:40000 on the
// handlertest listener.
func newKit(t *testing.T, mods ...func(*kitOptions)) *kit {
	t.Helper()
	o := kitOptions{units: admission.DefaultUnits, preRouting: admission.DefaultPreRouting, start: epoch()}
	for _, m := range mods {
		m(&o)
	}
	k := &kit{
		t: t, clk: clocktest.New(o.start), tracer: &handlertest.Tracer{}, alog: &handlertest.AccessLog{},
		node: handlertest.NewNode(), plan: handlertest.NewPlan(), logs: &logCapture{},
	}
	k.holder = retire.New(retire.Config{Clock: k.clk, Stripes: 1})
	k.units = admission.NewUnits(o.units)
	k.pre = admission.NewPreRouting(o.preRouting, k.clk)
	k.budget = body.NewBudget(64<<20, &handlertest.Gauge{})
	k.conn = handlertest.NewConn(handlertest.ListenerName, "192.0.2.10:40000")
	k.tap = tap.New(tap.Config{Dropped: &handlertest.Counter{}, Clock: k.clk})
	d := Deps{
		Snapshots: k.holder, Units: k.units, PreRouting: k.pre,
		Exchanges: exchange.NewPool(exchange.Deps{Budget: k.budget}),
		Executor:  executor.New(executor.Deps{Clock: k.clk, Tracer: k.tracer}),
		Tracer:    k.tracer, AccessLog: k.alog, Node: k.node.M, Tap: k.tap, Clock: k.clk,
		Logger: slog.New(k.logs),
	}
	if o.wall != nil {
		d.Wall = o.wall
	}
	if !o.noConn {
		c := k.conn
		d.Conn = func(context.Context) Conn { return c }
	} else {
		d.Conn = func(context.Context) Conn { return nil }
	}
	h, err := New(d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	k.h = h
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = k.holder.Close(ctx)
	})
	return k
}

// publish builds and publishes the snapshot spec describes, with the
// kit's metric plan unless spec has one.
func (k *kit) publish(spec handlertest.Spec) *snapshot.Snapshot {
	k.t.Helper()
	if spec.Metrics == nil {
		spec.Metrics = k.plan
	}
	s := handlertest.Snapshot(k.t, spec)
	for _, l := range s.Listeners {
		if l.Metrics == nil {
			l.Metrics = k.plan.Listener(l.Name)
		}
	}
	for _, rt := range s.Routes {
		if rt.Metrics == nil {
			rt.Metrics = k.plan.Route(rt.Name)
		}
	}
	if _, err := k.holder.Publish(s); err != nil {
		k.t.Fatalf("Publish: %v", err)
	}
	return s
}

// result is one handled request: the recorder, and whether the handler
// aborted with http.ErrAbortHandler.
type result struct {
	*httptest.ResponseRecorder
	aborted bool
}

// do serves r, recovering the handler's http.ErrAbortHandler abort.
func (k *kit) do(r *http.Request) (res result) {
	k.t.Helper()
	res.ResponseRecorder = httptest.NewRecorder()
	defer func() {
		if v := recover(); v != nil {
			if err, ok := v.(error); ok && errors.Is(err, http.ErrAbortHandler) {
				res.aborted = true
				return
			}
			panic(v)
		}
	}()
	k.h.ServeHTTP(res.ResponseRecorder, r)
	return res
}

// get serves GET target.
func (k *kit) get(target string) result {
	k.t.Helper()
	return k.do(httptest.NewRequestWithContext(k.t.Context(), http.MethodGet, target, nil))
}

// record returns the only access record, failing otherwise.
func (k *kit) record() emit.AccessRecord {
	k.t.Helper()
	recs := k.alog.Records()
	if len(recs) != 1 {
		k.t.Fatalf("access records = %d, want 1: %+v", len(recs), recs)
	}
	return recs[0]
}

// pins returns the requests pinned to s.
func pins(s *snapshot.Snapshot) int64 { return s.Pins.Count() }

// decodeProblem decodes a problem document response.
func decodeProblem(t *testing.T, res result) problem.Document {
	t.Helper()
	if ct := res.Header().Get("Content-Type"); ct != problem.ContentType {
		t.Fatalf("Content-Type = %q, want %s", ct, problem.ContentType)
	}
	d, err := problem.Decode(res.Body.Bytes())
	if err != nil {
		t.Fatalf("decode problem %q: %v", res.Body.String(), err)
	}
	return d
}

// wantProblem checks a problem document response's status and code and
// that its requestId is the request's trace ID.
func wantProblem(t *testing.T, res result, status int, code string) problem.Document {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status = %d, want %d (body %q)", res.Code, status, res.Body.String())
	}
	d := decodeProblem(t, res)
	if d.Code != code || d.Status != status || len(d.RequestID) != 32 {
		t.Fatalf("problem = %+v, want status %d code %s and a 32-hex requestId", d, status, code)
	}
	return d
}

// route returns a handlertest Route named name forwarding to f.
func route(name string, f snapshot.Forwarder, ps ...*snapshot.Policy) handlertest.Route {
	return handlertest.Route{Name: name, Forward: f, Policies: ps}
}

// ok is a Forwarder answering 200 "hello" from Upstream orders.
func ok() snapshot.Forwarder {
	return handlertest.Respond("orders", http.StatusOK, http.Header{"Content-Length": {"5"}, "Content-Type": {"text/plain"}}, "hello")
}

// logCapture is a slog.Handler keeping every record's level, message and
// attributes.
type logCapture struct {
	mu   sync.Mutex
	recs []string
}

func (*logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (l *logCapture) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, b.String())
	return nil
}
func (l *logCapture) WithAttrs([]slog.Attr) slog.Handler { return l }
func (l *logCapture) WithGroup(string) slog.Handler      { return l }

func (l *logCapture) has(part string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range l.recs {
		if strings.Contains(r, part) {
			return true
		}
	}
	return false
}

// filterFunc builds a Filter from a function.
func filterFunc(f func(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result) filter.Filter {
	return handlertest.FilterFunc(f)
}

// header returns a Filter that sets name to value on the message of every
// Phase it runs in.
func header(name, value string) filter.Filter {
	return filterFunc(func(_ context.Context, _ phase.Phase, x filter.Exchange) filter.Result {
		x.Message().Header().Set(name, value)
		return filter.Next()
	})
}

// recordingWriter is a ResponseWriter that records the connection
// deadlines http.ResponseController sets and its flushes.
type recordingWriter struct {
	*httptest.ResponseRecorder
	mu      sync.Mutex
	reads   []time.Time
	writes  []time.Time
	flushes int
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{ResponseRecorder: httptest.NewRecorder()}
}

func (w *recordingWriter) SetReadDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.reads = append(w.reads, t)
	return nil
}

func (w *recordingWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writes = append(w.writes, t)
	return nil
}

func (w *recordingWriter) FlushError() error {
	w.mu.Lock()
	w.flushes++
	w.mu.Unlock()
	w.Flush()
	return nil
}

// errReader returns data then fails with err.
type errReader struct {
	data []byte
	err  error
}

func (r *errReader) Read(p []byte) (int, error) {
	if len(r.data) > 0 {
		n := copy(p, r.data)
		r.data = r.data[n:]
		return n, nil
	}
	return 0, r.err
}

func (*errReader) Close() error { return nil }

// trackedBody is a response body that records its Close.
type trackedBody struct {
	io.Reader
	mu     sync.Mutex
	closed bool
}

func (b *trackedBody) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.closed = true
	return nil
}

// newTracked returns a tracked body of s.
func newTracked(s string) *trackedBody { return &trackedBody{Reader: bytes.NewReader([]byte(s))} }

// beyondDeadlines is a fake clock start for tests over real sockets: far
// enough ahead that the connection deadlines the handler derives from it
// never pass during the test, whatever the wall time.
func beyondDeadlines() time.Time { return time.Date(2999, 1, 1, 0, 0, 0, 0, time.UTC) }

// errUpstream is a failed leg's error with code.
func errUpstream(code string) error { return errcode.Wrap(code, errors.New("leg failed")) }
