// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package otlpsink

import (
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/credentials"
)

// TestOTelSDKExporters sends each signal through the OpenTelemetry Go
// OTLP/gRPC exporters the telemetry runtime uses (09 req 16) over TLS
// with a test CA: traces, metrics and logs arrive with the resource, and
// no header beyond gRPC's own (09 test 36).
func TestOTelSDKExporters(t *testing.T) {
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_HEADERS", "OTEL_EXPORTER_OTLP_TRACES_HEADERS",
		"OTEL_EXPORTER_OTLP_METRICS_HEADERS", "OTEL_EXPORTER_OTLP_LOGS_HEADERS", "OTEL_RESOURCE_ATTRIBUTES",
	} {
		t.Setenv(k, "")
	}
	p := newTestPKI(t)
	s := start(t, Config{TLS: p.server(false)})
	ctx := ctxTimeout(t, 20*time.Second)
	creds := credentials.NewTLS(p.client(false))
	res := sdkresource.NewSchemaless(attribute.String("service.name", "ruralzd"), attribute.String("service.instance.id", "01JTEST"))

	te, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpoint(s.Addr()), otlptracegrpc.WithTLSCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithSyncer(te), sdktrace.WithResource(res))
	_, sp := tp.Tracer("github.com/ravindu-rev/ruralz").Start(ctx, "GET orders")
	sp.SetAttributes(attribute.String("ruralz.route", "orders"))
	sp.End()
	if err := tp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	me, err := otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithEndpoint(s.Addr()), otlpmetricgrpc.WithTLSCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(me, sdkmetric.WithInterval(time.Hour))), sdkmetric.WithResource(res))
	c, err := mp.Meter("github.com/ravindu-rev/ruralz").Int64Counter("ruralz_http_requests_total")
	if err != nil {
		t.Fatal(err)
	}
	c.Add(ctx, 3)
	if err := mp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	le, err := otlploggrpc.New(ctx, otlploggrpc.WithEndpoint(s.Addr()), otlploggrpc.WithTLSCredentials(creds))
	if err != nil {
		t.Fatal(err)
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithProcessor(sdklog.NewSimpleProcessor(le)), sdklog.WithResource(res))
	var rec otellog.Record
	rec.SetBody(attribute.StringValue("access"))
	rec.SetEventName("ruralz.access")
	lp.Logger("github.com/ravindu-rev/ruralz").Emit(ctx, rec)
	if err := lp.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}

	spans := s.Spans()
	if len(spans) != 1 || spans[0].Name() != "GET orders" || spans[0].AttrString("ruralz.route") != "orders" {
		t.Fatalf("spans = %v", spans)
	}
	if v, _ := ResourceAttr(spans[0].Resource, "service.instance.id"); ValueString(v) != "01JTEST" {
		t.Fatalf("span resource = %v", spans[0].Resource)
	}
	m, ok := s.LastMetric("ruralz_http_requests_total")
	if !ok || m.Proto.GetSum().GetDataPoints()[0].GetAsInt() != 3 {
		t.Fatalf("metric = %v %v", m, ok)
	}
	if v, _ := ResourceAttr(m.Resource, "service.name"); ValueString(v) != "ruralzd" {
		t.Fatalf("metric resource = %v", m.Resource)
	}
	logs := s.Logs()
	if len(logs) != 1 || logs[0].Body() != "access" || logs[0].EventName() != "ruralz.access" {
		t.Fatalf("logs = %v", logs)
	}
	for _, r := range s.Requests() {
		if extra := r.ExtraMetadata(); len(extra) != 0 {
			t.Errorf("%v export carries extra headers %v", r.Signal, extra)
		}
		if r.TLS == nil {
			t.Errorf("%v export not over TLS", r.Signal)
		}
	}
}
