// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestAccessLogWhen checks Gateway.spec.telemetry.accessLog.when over the
// onLog variables: a false result writes no record, a true one writes it,
// and a runtime error writes it and counts
// ruralz_telemetry_access_log_cel_errors_total.
func TestAccessLogWhen(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		forward snapshot.Forwarder
		records int
		errors  int64
	}{
		{"false", `response.status >= 500`, ok(), 0, 0},
		{"true", `response.status >= 500`, handlertest.Respond("orders", http.StatusBadGateway, http.Header{}, ""), 1, 0},
		{"upstream and duration", `upstream.name == "orders" && duration >= duration("0s")`, ok(), 1, 0},
		{"runtime error", `request.headers["x-missing"] == "a"`, ok(), 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			when := celtest.Program(t, expr.Site{Place: expr.PlaceAccessLogWhen}, tt.src)
			k.publish(handlertest.Spec{AccessLogWhen: when, Routes: []handlertest.Route{route("r", tt.forward)}})
			k.get("/")
			if n := len(k.alog.Records()); n != tt.records {
				t.Errorf("records = %d, want %d", n, tt.records)
			}
			if n := k.node.CELErrors.Value(); n != tt.errors {
				t.Errorf("access log CEL errors = %d, want %d", n, tt.errors)
			}
		})
	}
}

// TestAccessLogWhenOnPreRouting checks that a pre-routing refusal is
// selected by the same accessLog.when.
func TestAccessLogWhenOnPreRouting(t *testing.T) {
	k := newKit(t)
	when := celtest.Program(t, expr.Site{Place: expr.PlaceAccessLogWhen}, `response.status == 404`)
	k.publish(handlertest.Spec{AccessLogWhen: when})
	k.get("/")
	if rec := k.record(); rec.Route != unmatchedRoute || rec.Status != http.StatusNotFound {
		t.Errorf("record = %s %d, want _unmatched 404", rec.Route, rec.Status)
	}
}

// TestAccessRecordFields checks the record fields the handler fills: the
// server span context and sampling, the TLS version, the request body
// bytes, the gateway-added and Upstream durations, and an exemplar on the
// Route duration of a sampled request.
func TestAccessRecordFields(t *testing.T) {
	k := newKit(t)
	fwd := handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		rd, err := o.Body.Open()
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		_, _ = b.ReadFrom(rd)
		_ = rd.Close()
		o.Excluder.Enter(k.clk.Now())
		k.clk.Advance(30 * time.Millisecond)
		o.Excluder.Leave(k.clk.Now())
		k.clk.Advance(5 * time.Millisecond)
		return &snapshot.UpstreamResponse{Status: http.StatusOK, Header: http.Header{"Content-Length": {"2"}}, Body: newTracked("ok"), Upstream: "orders"}, nil
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd)}})
	r := httptest.NewRequest(http.MethodPost, "https://api.example/r", strings.NewReader("hello"))
	r.TLS = &tls.ConnectionState{Version: tls.VersionTLS13}
	trace := "0af7651916cd43dd8448eb211c80319c"
	r.Header.Set("Traceparent", "00-"+trace+"-b7ad6b7169203331-01")
	k.do(r)
	rec := k.record()
	if hex.EncodeToString(rec.TraceID[:]) != trace || !rec.Sampled || rec.SpanID == ([8]byte{}) {
		t.Errorf("record trace = %x sampled %t span %x, want the remote trace, sampled, with the server span", rec.TraceID, rec.Sampled, rec.SpanID)
	}
	if rec.TLSVersion != "1.3" || rec.RequestBytes != 5 || rec.ResponseBytes != 2 {
		t.Errorf("record = tls %q request %d response %d bytes, want 1.3, 5 and 2", rec.TLSVersion, rec.RequestBytes, rec.ResponseBytes)
	}
	if rec.UpstreamDuration != 30*time.Millisecond || rec.GatewayDuration != 5*time.Millisecond || rec.Duration != 35*time.Millisecond {
		t.Errorf("durations = upstream %v gateway %v total %v, want 30ms, 5ms and 35ms", rec.UpstreamDuration, rec.GatewayDuration, rec.Duration)
	}
	if ex := k.plan.RouteHandles("r").Duration.Exemplars(); len(ex) != 1 || hex.EncodeToString(ex[0].TraceID[:]) != trace {
		t.Errorf("exemplars = %+v, want one with the request's trace", ex)
	}
	if v := k.plan.ListenerHandles(handlertest.ListenerName).RequestBody.Values(); !slices.Equal(v, []uint64{5}) {
		t.Errorf("request body sizes = %v, want [5]", v)
	}
}

// TestServerSpan checks the server span: its attributes at start, the
// matched Route, the Revision, and the status and code at its end.
func TestServerSpan(t *testing.T) {
	k := newKit(t)
	snap := k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return nil, errUpstream("RZ-UP-005")
	}))}})
	r := httptest.NewRequest(http.MethodGet, "http://api.example/v1?q=1", nil)
	r.Header.Set("User-Agent", "probe/1")
	k.do(r)
	spans := k.tracer.Spans("server")
	if len(spans) != 1 {
		t.Fatalf("server spans = %d, want 1", len(spans))
	}
	s := spans[0]
	if s.Name != http.MethodGet || s.Server.Listener != handlertest.ListenerName || s.Server.Path != "/v1" || s.Server.UserAgent != "probe/1" || s.Server.Protocol != protoHTTP1 || s.Server.Scheme != "http" || s.Server.Port != 8080 {
		t.Errorf("server span = %s %+v", s.Name, s.Server)
	}
	if s.Attr(catalog.AttrRoute) != "r" || s.Attr(catalog.AttrRevision) != snap.Revision.Digest.Short() {
		t.Errorf("span attributes = %v, want route r and the Revision", s.Attrs)
	}
	if ended, status, code := s.Done(); !ended || status != http.StatusServiceUnavailable || code != "RZ-UP-005" {
		t.Errorf("span end = %t %d %s, want 503 RZ-UP-005", ended, status, code)
	}
}

// reachForwarder is a Forwarder that marks Outbound.Reached when reach is
// set, then fails with code.
func reachForwarder(reach bool, code string) snapshot.Forwarder {
	return handlertest.ForwardFunc(func(_ context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		if reach {
			o.Reached.Store(true)
		}
		return nil, errUpstream(code)
	})
}

// TestUpstreamReached checks Final.UpstreamReached: true once an attempt
// reached an Upstream, false when none did, also on a pooled state that
// served a reaching request before.
func TestUpstreamReached(t *testing.T) {
	k := newKit(t)
	var mu sync.Mutex
	var finals []filter.Final
	logger := &finisher{f: func(x filter.Exchange) {
		mu.Lock()
		defer mu.Unlock()
		finals = append(finals, x.Final())
	}}
	var reach atomic.Bool
	fwd := handlertest.ForwardFunc(func(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
		return reachForwarder(reach.Load(), "RZ-UP-004").Forward(ctx, o)
	})
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", fwd,
		handlertest.Policy("quota", phase.ClassAdmission, v1alpha1.PolicyTypeQuota, logger, phase.OnRequestHeaders, phase.OnLog))}})
	reach.Store(true)
	k.get("/")
	reach.Store(false)
	k.get("/")
	mu.Lock()
	defer mu.Unlock()
	if len(finals) != 2 || !finals[0].UpstreamReached || finals[1].UpstreamReached {
		t.Fatalf("finals = %+v, want reached then not", finals)
	}
	if finals[0].Refunds() || !finals[1].Refunds() {
		t.Errorf("refunds = %t, %t; want a reached failure charged and an unreached one refunded", finals[0].Refunds(), finals[1].Refunds())
	}
}

// tapWriter is a goroutine-safe ResponseWriter for a /tap subscription.
type tapWriter struct {
	mu      sync.Mutex
	hdr     http.Header
	buf     bytes.Buffer
	started chan struct{}
	lines   chan struct{}
	once    sync.Once
}

func (w *tapWriter) Header() http.Header { return w.hdr }
func (w *tapWriter) WriteHeader(int)     { w.once.Do(func() { close(w.started) }) }
func (w *tapWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.buf.Write(p)
	w.mu.Unlock()
	select {
	case w.lines <- struct{}{}:
	default:
	}
	return len(p), nil
}
func (*tapWriter) SetWriteDeadline(time.Time) error { return nil }
func (*tapWriter) FlushError() error                { return nil }

func (w *tapWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

// subscribe opens a /tap subscription on the kit's hub until the test
// ends.
func subscribe(t *testing.T, k *kit) *tapWriter {
	t.Helper()
	w := &tapWriter{hdr: http.Header{}, started: make(chan struct{}), lines: make(chan struct{}, 16)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		k.tap.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/tap", nil).WithContext(ctx))
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	<-w.started
	return w
}

// TestTapRedactsCredentials checks the /tap event of a finished exchange:
// published while a subscriber listens, with the Route, status and
// Upstream, and the values of the fixed credential headers and of every
// auth.api-key config.header of the Revision redacted.
func TestTapRedactsCredentials(t *testing.T) {
	k := newKit(t)
	// No sensitive word in the name: only the Revision's set redacts it.
	header := "X-Shop-Credential"
	keys := &hub.Resource{
		ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "keys"},
		Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "keys"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
		Config: &v1alpha1.AuthAPIKeyConfig{Header: &header},
	}
	k.publish(handlertest.Spec{Validated: &hub.Validated{Bundle: hub.NewBundle([]*hub.Resource{keys})}, Routes: []handlertest.Route{route("r", ok())}})
	w := subscribe(t, k)
	r := httptest.NewRequest(http.MethodGet, "http://api.example/r", nil)
	r.Header.Set(header, "shop-secret")
	r.Header.Set("Authorization", "Bearer token-secret")
	r.Header.Set("X-Plain", "visible")
	k.do(r)
	<-w.lines
	line := w.String()
	for _, want := range []string{`"route":"r"`, `"status":200`, `"upstream":"orders"`, `"x-plain":["visible"]`, `"x-shop-credential":["` + redact.Marker + `"]`, `"authorization":["` + redact.Marker + `"]`} {
		if !strings.Contains(line, want) {
			t.Errorf("tap line %s lacks %s", line, want)
		}
	}
	if strings.Contains(line, "secret") {
		t.Errorf("tap line %s shows a credential", line)
	}
}

// TestCredentialSet checks the /tap credential set of a Revision: the
// fixed credential headers, and each auth.api-key config.header, the
// default x-api-key when unset.
func TestCredentialSet(t *testing.T) {
	custom := "X-Shop-Credential"
	policy := func(name string, cfg *v1alpha1.AuthAPIKeyConfig) *hub.Resource {
		return &hub.Resource{
			ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: name},
			Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
			Config: cfg,
		}
	}
	set := credentialSet(&hub.Validated{Bundle: hub.NewBundle([]*hub.Resource{policy("a", &v1alpha1.AuthAPIKeyConfig{Header: &custom}), policy("b", &v1alpha1.AuthAPIKeyConfig{})})})
	for name, want := range map[string]bool{"x-shop-credential": true, "X-Api-Key": true, "Authorization": true, "Cookie": true, "X-Plain": false} {
		if got := set.Credential(name); got != want {
			t.Errorf("Credential(%q) = %t, want %t", name, got, want)
		}
	}
	if d := credentialSet(nil); !d.Credential("Authorization") || d.Credential("X-Shop-Credential") {
		t.Error("credentialSet(nil) is not the default set")
	}
}

// TestNoTapWithoutSubscribers checks that no event is built while nobody
// subscribes, and that the hub sees the subscriber count go back to zero.
func TestNoTapWithoutSubscribers(t *testing.T) {
	k := newKit(t)
	k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", ok())}})
	k.get("/")
	if k.tap.Active() || k.tap.Subscribers() != 0 {
		t.Errorf("hub active %t with %d subscribers, want none", k.tap.Active(), k.tap.Subscribers())
	}
}
