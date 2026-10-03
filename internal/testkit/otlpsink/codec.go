// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"context"
	"errors"
	"sync/atomic"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/encoding"
	"google.golang.org/grpc/mem"
	"google.golang.org/grpc/stats"
)

// capture carries one export request from the codec to its handler. The
// handler passes it to gRPC's decode function; rawCodec.Unmarshal decodes
// the request into msg and copies the wire bytes into raw. The bytes
// travel with the message, so an export that is decoded but never handled
// leaves nothing behind in the codec: gRPC decodes the message and then
// reads on to the end of the request, and the decode function fails when
// that read does (the client cancels or its deadline passes, an
// RST_STREAM, or Stop during Down or Close).
type capture struct {
	msg any
	raw []byte
}

// rawCodec is the server codec. It delegates to gRPC's registered "proto"
// codec and, when decoding into a *capture, keeps a copy of the request's
// wire bytes (after gRPC decompression), so the handler records exactly
// what the client sent for canary scans (11 req 39). It holds no
// per-request state.
type rawCodec struct {
	base encoding.CodecV2
}

func newRawCodec() (rawCodec, error) {
	base := encoding.GetCodecV2("proto")
	if base == nil {
		return rawCodec{}, errors.New("otlpsink: gRPC proto codec is not registered")
	}
	return rawCodec{base: base}, nil
}

// Marshal encodes responses with the proto codec.
func (c rawCodec) Marshal(v any) (mem.BufferSlice, error) { return c.base.Marshal(v) }

// Unmarshal decodes data into v. For a *capture it decodes into the
// capture's message and keeps a copy of data, which gRPC frees after the
// call.
func (c rawCodec) Unmarshal(data mem.BufferSlice, v any) error {
	cp, ok := v.(*capture)
	if !ok {
		return c.base.Unmarshal(data, v)
	}
	if err := c.base.Unmarshal(data, cp.msg); err != nil {
		return err
	}
	cp.raw = data.Materialize()
	return nil
}

// Name is the content subtype the codec serves.
func (c rawCodec) Name() string { return c.base.Name() }

// exportMethod is the method name of every OTLP collector service.
const exportMethod = "Export"

// collectorService is one OTLP collector service: its generated
// descriptor (service name and proto file), the signal it carries and its
// Export request and response types.
type collectorService struct {
	desc    *grpc.ServiceDesc
	signal  Signal
	newReq  func() any
	newResp func() any
}

func collectorServices() []collectorService {
	return []collectorService{
		{
			desc:    &coltrace.TraceService_ServiceDesc,
			signal:  Traces,
			newReq:  func() any { return new(coltrace.ExportTraceServiceRequest) },
			newResp: func() any { return new(coltrace.ExportTraceServiceResponse) },
		},
		{
			desc:    &colmetrics.MetricsService_ServiceDesc,
			signal:  Metrics,
			newReq:  func() any { return new(colmetrics.ExportMetricsServiceRequest) },
			newResp: func() any { return new(colmetrics.ExportMetricsServiceResponse) },
		},
		{
			desc:    &collogs.LogsService_ServiceDesc,
			signal:  Logs,
			newReq:  func() any { return new(collogs.ExportLogsServiceRequest) },
			newResp: func() any { return new(collogs.ExportLogsServiceResponse) },
		},
	}
}

// register registers the three collector services on srv with
// hand-written Export handlers: they decode through a capture, which the
// generated handlers (decoding into the bare message) cannot.
func (s *Sink) register(srv *grpc.Server) {
	for _, svc := range collectorServices() {
		srv.RegisterService(&grpc.ServiceDesc{
			ServiceName: svc.desc.ServiceName,
			HandlerType: (*any)(nil),
			Methods:     []grpc.MethodDesc{{MethodName: exportMethod, Handler: s.exportHandler(svc)}},
			Metadata:    svc.desc.Metadata,
		}, s)
	}
}

// exportHandler is the unary handler of one service's Export method. The
// sink installs no interceptor, so the interceptor argument is always nil.
func (s *Sink) exportHandler(svc collectorService) grpc.MethodHandler {
	return func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		c := &capture{msg: svc.newReq()}
		if err := dec(c); err != nil {
			return nil, err
		}
		if err := s.handle(ctx, svc.signal, c.msg, c.raw); err != nil {
			return nil, err
		}
		return svc.newResp(), nil
	}
}

// connKey is the context key under which connStats.TagConn stores a
// connection's sequence number.
type connKey struct{}

// connStats is the servers' stats.Handler. It numbers connections from 1
// (across Down, which starts a new server) and counts them: opened in
// total and open now. gRPC reports a connection once its transport is up,
// after the TLS handshake, and reports its end before Stop returns.
type connStats struct {
	seq          atomic.Uint64
	opened, open atomic.Int64
}

// TagConn numbers the connection.
func (h *connStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return context.WithValue(ctx, connKey{}, h.seq.Add(1))
}

// HandleConn counts connection begins and ends.
func (h *connStats) HandleConn(_ context.Context, st stats.ConnStats) {
	switch st.(type) {
	case *stats.ConnBegin:
		h.opened.Add(1)
		h.open.Add(1)
	case *stats.ConnEnd:
		h.open.Add(-1)
	}
}

// TagRPC leaves the RPC context unchanged.
func (*connStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context { return ctx }

// HandleRPC ignores RPC events.
func (*connStats) HandleRPC(context.Context, stats.RPCStats) {}

// connSeq returns the sequence number connStats gave the export's
// connection, 0 when unknown.
func connSeq(ctx context.Context) uint64 {
	n, _ := ctx.Value(connKey{}).(uint64)
	return n
}
