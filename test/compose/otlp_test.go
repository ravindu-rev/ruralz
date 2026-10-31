// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compose

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// The Collector's file exporter writes OTLP JSON (the OTLP/HTTP JSON
// encoding: hex trace and span IDs, 64-bit integers as strings, enums as
// numbers), one export request per line. The suite decodes the members it
// reads with the standard library: the OpenTelemetry modules stay confined
// to internal/telemetry and internal/testkit/otlpsink.

// otlpValue is an AnyValue.
type otlpValue struct {
	StringValue *string         `json:"stringValue,omitempty"`
	IntValue    json.RawMessage `json:"intValue,omitempty"`
	BoolValue   *bool           `json:"boolValue,omitempty"`
	DoubleValue *float64        `json:"doubleValue,omitempty"`
}

// String returns the value as text.
func (v otlpValue) String() string {
	switch {
	case v.StringValue != nil:
		return *v.StringValue
	case len(v.IntValue) > 0:
		return strings.Trim(string(v.IntValue), `"`)
	case v.BoolValue != nil:
		return strconv.FormatBool(*v.BoolValue)
	case v.DoubleValue != nil:
		return strconv.FormatFloat(*v.DoubleValue, 'g', -1, 64)
	}
	return ""
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes"`
}

// attrMap returns attributes as text by key.
func attrMap(kvs []otlpKeyValue) map[string]string {
	out := make(map[string]string, len(kvs))
	for _, kv := range kvs {
		out[kv.Key] = kv.Value.String()
	}
	return out
}

// flexUint is a 64-bit integer written as a string or a number.
type flexUint uint64

func (f *flexUint) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return fmt.Errorf("integer %s: %w", b, err)
	}
	*f = flexUint(n)
	return nil
}

// spanKind is a span kind written as a number or an enum name.
type spanKind int

// Span kinds (OTLP SpanKind).
const (
	kindInternal spanKind = 1
	kindServer   spanKind = 2
	kindClient   spanKind = 3
)

func (k *spanKind) UnmarshalJSON(b []byte) error {
	if n, err := strconv.Atoi(string(b)); err == nil {
		*k = spanKind(n)
		return nil
	}
	var name string
	if err := json.Unmarshal(b, &name); err != nil {
		return err
	}
	for i, want := range []string{"SPAN_KIND_UNSPECIFIED", "SPAN_KIND_INTERNAL", "SPAN_KIND_SERVER", "SPAN_KIND_CLIENT", "SPAN_KIND_PRODUCER", "SPAN_KIND_CONSUMER"} {
		if name == want {
			*k = spanKind(i)
			return nil
		}
	}
	return fmt.Errorf("unknown span kind %q", name)
}

// span is one exported span, flattened with its resource.
type span struct {
	TraceID, SpanID, ParentID, Name string
	Kind                            spanKind
	Start, End                      uint64
	Attrs                           map[string]string
	// Service is the resource's service.name.
	Service string
	// Seq is the span's position in the files, which breaks start-time
	// ties.
	Seq int
}

// tracesLine is one line of the traces file.
type tracesLine struct {
	ResourceSpans []struct {
		Resource   otlpResource `json:"resource"`
		ScopeSpans []struct {
			Spans []struct {
				TraceID      string         `json:"traceId"`
				SpanID       string         `json:"spanId"`
				ParentSpanID string         `json:"parentSpanId"`
				Name         string         `json:"name"`
				Kind         spanKind       `json:"kind"`
				Start        flexUint       `json:"startTimeUnixNano"`
				End          flexUint       `json:"endTimeUnixNano"`
				Attributes   []otlpKeyValue `json:"attributes"`
			} `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// metricsLine is one line of the metrics file.
type metricsLine struct {
	ResourceMetrics []struct {
		Resource     otlpResource `json:"resource"`
		ScopeMetrics []struct {
			Metrics []struct {
				Name string `json:"name"`
			} `json:"metrics"`
		} `json:"scopeMetrics"`
	} `json:"resourceMetrics"`
}

// logsLine is one line of the logs file.
type logsLine struct {
	ResourceLogs []struct {
		Resource  otlpResource `json:"resource"`
		ScopeLogs []struct {
			LogRecords []struct {
				EventName  string         `json:"eventName"`
				TraceID    string         `json:"traceId"`
				Body       otlpValue      `json:"body"`
				Attributes []otlpKeyValue `json:"attributes"`
			} `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
}

// logRecord is one exported log record, flattened with its resource.
type logRecord struct {
	EventName, TraceID, Body string
	Attrs                    map[string]string
	Service                  string
}

// metricPoint is one exported metric's name and resource.
type metricPoint struct {
	Name, Service string
}

// jsonLines calls decode for every complete line of data. A last line
// without its newline is still being written and is skipped; any other
// line that does not decode is an error.
func jsonLines(data []byte, decode func([]byte) error) error {
	lines := bytes.Split(data, []byte("\n"))
	for i, l := range lines {
		if len(bytes.TrimSpace(l)) == 0 {
			continue
		}
		if err := decode(l); err != nil {
			if i == len(lines)-1 {
				return nil
			}
			return fmt.Errorf("line %d: %w", i+1, err)
		}
	}
	return nil
}

// decodeSpans decodes the traces file.
func decodeSpans(data []byte) ([]span, error) {
	var out []span
	err := jsonLines(data, func(l []byte) error {
		var line tracesLine
		if err := json.Unmarshal(l, &line); err != nil {
			return err
		}
		for _, rs := range line.ResourceSpans {
			service := attrMap(rs.Resource.Attributes)["service.name"]
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					out = append(out, span{
						TraceID: strings.ToLower(s.TraceID), SpanID: strings.ToLower(s.SpanID), ParentID: strings.ToLower(s.ParentSpanID),
						Name: s.Name, Kind: s.Kind, Start: uint64(s.Start), End: uint64(s.End),
						Attrs: attrMap(s.Attributes), Service: service, Seq: len(out),
					})
				}
			}
		}
		return nil
	})
	return out, err
}

// decodeMetrics decodes the metrics file.
func decodeMetrics(data []byte) ([]metricPoint, error) {
	var out []metricPoint
	err := jsonLines(data, func(l []byte) error {
		var line metricsLine
		if err := json.Unmarshal(l, &line); err != nil {
			return err
		}
		for _, rm := range line.ResourceMetrics {
			service := attrMap(rm.Resource.Attributes)["service.name"]
			for _, sm := range rm.ScopeMetrics {
				for _, m := range sm.Metrics {
					out = append(out, metricPoint{Name: m.Name, Service: service})
				}
			}
		}
		return nil
	})
	return out, err
}

// decodeLogs decodes the logs file.
func decodeLogs(data []byte) ([]logRecord, error) {
	var out []logRecord
	err := jsonLines(data, func(l []byte) error {
		var line logsLine
		if err := json.Unmarshal(l, &line); err != nil {
			return err
		}
		for _, rl := range line.ResourceLogs {
			service := attrMap(rl.Resource.Attributes)["service.name"]
			for _, sl := range rl.ScopeLogs {
				for _, r := range sl.LogRecords {
					out = append(out, logRecord{
						EventName: r.EventName, TraceID: strings.ToLower(r.TraceID), Body: r.Body.String(),
						Attrs: attrMap(r.Attributes), Service: service,
					})
				}
			}
		}
		return nil
	})
	return out, err
}

// traceSpans returns the spans of one trace.
func traceSpans(all []span, traceID string) []span {
	var out []span
	for _, s := range all {
		if s.TraceID == traceID {
			out = append(out, s)
		}
	}
	return out
}

func TestDecodeSpans(t *testing.T) {
	data := []byte(`{"resourceSpans":[{"resource":{"attributes":[{"key":"service.name","value":{"stringValue":"ruralzd"}}]},"scopeSpans":[{"scope":{"name":"ruralz"},"spans":[` +
		`{"traceId":"0AF7651916CD43DD8448EB211C80319C","spanId":"b7ad6b7169203331","parentSpanId":"00f067aa0ba902b7","name":"GET orders-summary","kind":2,"startTimeUnixNano":"1700000000000000001","endTimeUnixNano":"1700000000000000009","attributes":[{"key":"http.response.status_code","value":{"intValue":"200"}}]},` +
		`{"traceId":"0af7651916cd43dd8448eb211c80319c","spanId":"c7ad6b7169203331","parentSpanId":"b7ad6b7169203331","name":"ruralz.filter.jwt-default","kind":"SPAN_KIND_INTERNAL","startTimeUnixNano":1700000000000000002,"endTimeUnixNano":"1700000000000000003","attributes":[{"key":"ruralz.phase","value":{"stringValue":"onRequestHeaders"}},{"key":"sampled","value":{"boolValue":true}},{"key":"ratio","value":{"doubleValue":0.5}}]}` +
		`]}]}]}` + "\n" + `{"resourceSpans":[{"resource":{},"scopeSpans":[{"spans":[{"traceId":"`) // a line still being written
	spans, err := decodeSpans(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(spans) != 2 {
		t.Fatalf("decodeSpans = %d spans, want 2 (the partial last line skipped)", len(spans))
	}
	server, filter := spans[0], spans[1]
	if server.TraceID != "0af7651916cd43dd8448eb211c80319c" || server.Kind != kindServer || server.Start != 1700000000000000001 ||
		server.Service != "ruralzd" || server.Attrs["http.response.status_code"] != "200" {
		t.Errorf("server span = %+v", server)
	}
	if filter.ParentID != server.SpanID || filter.Kind != kindInternal || filter.Start != 1700000000000000002 ||
		filter.Attrs["ruralz.phase"] != "onRequestHeaders" || filter.Attrs["sampled"] != "true" || filter.Attrs["ratio"] != "0.5" || filter.Seq != 1 {
		t.Errorf("filter span = %+v", filter)
	}
	if got := traceSpans(spans, server.TraceID); len(got) != 2 {
		t.Errorf("traceSpans = %d spans, want 2", len(got))
	}
	if _, err := decodeSpans([]byte("{\n{}\n")); err == nil {
		t.Error("decodeSpans of a broken complete line succeeded")
	}
}

func TestDecodeMetricsAndLogs(t *testing.T) {
	res := `{"attributes":[{"key":"service.name","value":{"stringValue":"ruralzd"}}]}`
	metrics := []byte(`{"resourceMetrics":[{"resource":` + res + `,"scopeMetrics":[{"metrics":[{"name":"ruralz_http_requests_total","sum":{}},{"name":"ruralz_http_active_requests","gauge":{}}]}]}]}` + "\n")
	ms, err := decodeMetrics(metrics)
	if err != nil || len(ms) != 2 || ms[0] != (metricPoint{Name: "ruralz_http_requests_total", Service: "ruralzd"}) {
		t.Errorf("decodeMetrics = %+v, %v", ms, err)
	}
	logs := []byte(`{"resourceLogs":[{"resource":` + res + `,"scopeLogs":[{"logRecords":[{"timeUnixNano":"1","eventName":"ruralz.access","traceId":"AB","body":{"stringValue":"access"},"attributes":[{"key":"status","value":{"intValue":"401"}}]}]}]}]}` + "\n")
	ls, err := decodeLogs(logs)
	if err != nil || len(ls) != 1 {
		t.Fatalf("decodeLogs = %+v, %v", ls, err)
	}
	if l := ls[0]; l.EventName != "ruralz.access" || l.TraceID != "ab" || l.Body != "access" || l.Attrs["status"] != "401" || l.Service != "ruralzd" {
		t.Errorf("log record = %+v", l)
	}
}
