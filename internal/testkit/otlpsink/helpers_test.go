// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"testing"
	"time"

	collogs "go.opentelemetry.io/proto/otlp/collector/logs/v1"
	colmetrics "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	coltrace "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	logspb "go.opentelemetry.io/proto/otlp/logs/v1"
	metricspb "go.opentelemetry.io/proto/otlp/metrics/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
)

// testPKI is a self-signed certificate for 127.0.0.1 used as the server
// and client certificate, and the pool that trusts it. The test kit's pki
// package is not imported: WP-84's packages import no Ruralz package.
type testPKI struct {
	cert tls.Certificate
	pool *x509.CertPool
}

func newTestPKI(t testing.TB) testPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "otlpsink test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IPAddresses:           []net.IP{net.IPv4(127, 0, 0, 1)},
		DNSNames:              []string{"localhost"},
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return testPKI{cert: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool: pool}
}

func (p testPKI) server(mutual bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{p.cert}}
	if mutual {
		c.ClientAuth = tls.RequireAndVerifyClientCert
		c.ClientCAs = p.pool
	}
	return c
}

func (p testPKI) client(mutual bool) *tls.Config {
	c := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: p.pool}
	if mutual {
		c.Certificates = []tls.Certificate{p.cert}
	}
	return c
}

func start(t testing.TB, c Config) *Sink {
	t.Helper()
	s, err := Start(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// dial connects an OTLP gRPC client built on the proto module's generated
// clients; clientTLS nil dials h2c.
func dial(t testing.TB, s *Sink, clientTLS *tls.Config) *grpc.ClientConn {
	t.Helper()
	creds := insecure.NewCredentials()
	if clientTLS != nil {
		creds = credentials.NewTLS(clientTLS)
	}
	cc, err := grpc.NewClient("passthrough:///"+s.Addr(),
		grpc.WithTransportCredentials(creds),
		// Reconnect quickly after Down (09 test 37 recovery).
		grpc.WithConnectParams(grpc.ConnectParams{
			Backoff:           backoff.Config{BaseDelay: 20 * time.Millisecond, Multiplier: 1.6, MaxDelay: 200 * time.Millisecond},
			MinConnectTimeout: time.Second,
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

func ctxTimeout(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	t.Cleanup(cancel)
	return ctx
}

func str(k, v string) *commonpb.KeyValue {
	return &commonpb.KeyValue{Key: k, Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: v}}}
}

func resource(service string) *resourcepb.Resource {
	return &resourcepb.Resource{Attributes: []*commonpb.KeyValue{str("service.name", service)}}
}

func scope() *commonpb.InstrumentationScope {
	return &commonpb.InstrumentationScope{Name: "github.com/ravindu-rev/ruralz", Version: "test"}
}

func traceID(n byte) []byte {
	id := make([]byte, 16)
	id[15] = n
	id[0] = 0xab
	return id
}

func spanID(n byte) []byte {
	if n == 0 {
		return nil
	}
	id := make([]byte, 8)
	id[7] = n
	id[0] = 0xcd
	return id
}

// span builds a span of trace tr with id and parent (0: none) starting at
// base+start ms and lasting 1 ms.
func span(tr, id, parent byte, name string, kind tracepb.Span_SpanKind, start uint64, attrs ...*commonpb.KeyValue) *tracepb.Span {
	base := uint64(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC).UnixNano())
	st := base + start*uint64(time.Millisecond)
	return &tracepb.Span{
		TraceId:           traceID(tr),
		SpanId:            spanID(id),
		ParentSpanId:      spanID(parent),
		Name:              name,
		Kind:              kind,
		StartTimeUnixNano: st,
		EndTimeUnixNano:   st + uint64(time.Millisecond),
		Attributes:        attrs,
	}
}

func traceReq(service string, spans ...*tracepb.Span) *coltrace.ExportTraceServiceRequest {
	return &coltrace.ExportTraceServiceRequest{ResourceSpans: []*tracepb.ResourceSpans{{
		Resource:   resource(service),
		ScopeSpans: []*tracepb.ScopeSpans{{Scope: scope(), Spans: spans}},
	}}}
}

func logReq(service string, records ...*logspb.LogRecord) *collogs.ExportLogsServiceRequest {
	return &collogs.ExportLogsServiceRequest{ResourceLogs: []*logspb.ResourceLogs{{
		Resource:  resource(service),
		ScopeLogs: []*logspb.ScopeLogs{{Scope: scope(), LogRecords: records}},
	}}}
}

func logRecord(body, event string, attrs ...*commonpb.KeyValue) *logspb.LogRecord {
	return &logspb.LogRecord{
		TimeUnixNano: uint64(time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC).UnixNano()),
		SeverityText: "INFO",
		Body:         &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: body}},
		EventName:    event,
		Attributes:   attrs,
		TraceId:      traceID(1),
		SpanId:       spanID(1),
	}
}

func metricReq(service string, metrics ...*metricspb.Metric) *colmetrics.ExportMetricsServiceRequest {
	return &colmetrics.ExportMetricsServiceRequest{ResourceMetrics: []*metricspb.ResourceMetrics{{
		Resource:     resource(service),
		ScopeMetrics: []*metricspb.ScopeMetrics{{Scope: scope(), Metrics: metrics}},
	}}}
}

func counter(name string, v int64) *metricspb.Metric {
	return &metricspb.Metric{
		Name: name,
		Data: &metricspb.Metric_Sum{Sum: &metricspb.Sum{
			AggregationTemporality: metricspb.AggregationTemporality_AGGREGATION_TEMPORALITY_CUMULATIVE,
			IsMonotonic:            true,
			DataPoints: []*metricspb.NumberDataPoint{{
				Value: &metricspb.NumberDataPoint_AsInt{AsInt: v},
			}},
		}},
	}
}

// exportAll sends one request per signal.
func exportAll(ctx context.Context, cc *grpc.ClientConn, marker string) error {
	if _, err := coltrace.NewTraceServiceClient(cc).Export(ctx, traceReq("svc",
		span(1, 1, 0, "GET orders", tracepb.Span_SPAN_KIND_SERVER, 0, str("marker", marker)))); err != nil {
		return err
	}
	if _, err := colmetrics.NewMetricsServiceClient(cc).Export(ctx, metricReq("svc",
		counter("ruralz_http_requests_total", 1))); err != nil {
		return err
	}
	_, err := collogs.NewLogsServiceClient(cc).Export(ctx, logReq("svc", logRecord("access", "ruralz.access", str("marker", marker))))
	return err
}
