// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"context"
	"crypto/rand"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// source fills IDs from crypto/rand (lock-free per thread: the runtime's
// vDSO getrandom state), or from fill when a test sets it.
type source struct {
	fill func(b []byte)
}

// read fills b. The test path reads into a heap copy so b never escapes on
// the production path.
func (s *source) read(b []byte) {
	if s.fill != nil {
		tmp := make([]byte, len(b))
		s.fill(tmp)
		copy(b, tmp)
		return
	}
	_, _ = rand.Read(b)
}

// ids fills a new trace ID and span ID, neither all zeros, in one read.
func (s *source) ids(traceID *[16]byte, spanID *[8]byte) {
	var b [24]byte
	for {
		s.read(b[:])
		copy(traceID[:], b[:16])
		copy(spanID[:], b[16:])
		if *traceID != ([16]byte{}) && *spanID != ([8]byte{}) {
			return
		}
	}
}

// spanID fills a new span ID, never all zeros.
func (s *source) spanID(spanID *[8]byte) {
	for {
		s.read(spanID[:])
		if *spanID != ([8]byte{}) {
			return
		}
	}
}

// serverParent is the span StartServer puts in the context it starts the
// server span from: the client's span when its traceparent was valid,
// else no span (an invalid span context makes the server span a root).
// It carries the request's Decision to the IDGenerator; the server span's
// children see the server span, not this one, as the span of their
// context, so they never reuse the Decision's IDs.
type serverParent struct {
	noop.Span
	d *emit.Decision
}

// SpanContext returns the client's span context, or an invalid one.
func (p *serverParent) SpanContext() trace.SpanContext {
	if !p.d.Remote {
		return trace.SpanContext{}
	}
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    p.d.TraceID,
		SpanID:     p.d.ParentSpanID,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
}

// IDGenerator implements sdktrace.IDGenerator (spec 09 req 30): the
// server span of a request gets the trace ID and server span ID its
// Decision holds, so the exported server span, the access log and the
// requestId agree; every other span gets a random span ID from
// crypto/rand.
type IDGenerator struct {
	src *source
}

// NewIDs returns the Decision's IDs for a root server span, or random IDs.
func (g *IDGenerator) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if d := serverDecision(ctx); d != nil {
		return d.TraceID, d.ServerSpanID
	}
	var tid [16]byte
	var sid [8]byte
	g.src.ids(&tid, &sid)
	return tid, sid
}

// NewSpanID returns the Decision's server span ID for a server span with
// a remote parent in traceID, or a random span ID.
func (g *IDGenerator) NewSpanID(ctx context.Context, traceID trace.TraceID) trace.SpanID {
	if d := serverDecision(ctx); d != nil && d.TraceID == traceID {
		return d.ServerSpanID
	}
	var sid [8]byte
	g.src.spanID(&sid)
	return sid
}

// serverDecision returns the Decision of the server span being started
// from ctx, nil for any other span.
func serverDecision(ctx context.Context) *emit.Decision {
	if p, ok := trace.SpanFromContext(ctx).(*serverParent); ok {
		return p.d
	}
	return nil
}
