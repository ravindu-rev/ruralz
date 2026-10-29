// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/gateway/readiness"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Test tokens: RFC 6750 b64token, at least 22 bytes; canaries, not
// credentials.
//
//nolint:gosec // G101: test canaries, not credentials.
const (
	operatorToken = "op-canary-4fQ9x2LmZ7rT1vB8nK3sW6yH"
	metricsToken  = "metrics-canary-8Hs2Lq9Zp4Xw7Rt1Vn"
)

// testRequestID is the new trace ID the fixtures' tracer draws: the
// requestId of every problem document of a request without a valid
// traceparent.
const testRequestID = "4bf92f3577b34da6a3ce929d0e0e4736"

// testTraceID is testRequestID as bytes.
func testTraceID() [16]byte {
	return [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36}
}

// loopbackPeer is the RemoteAddr of in-process requests: tokens are
// accepted over cleartext from a loopback peer only.
const loopbackPeer = "127.0.0.1:50000"

// idTracer is an emit.Tracer whose Decide draws a fixed trace ID and
// records how it was asked; no other method is called.
type idTracer struct {
	emit.Tracer
	id [16]byte

	mu      sync.Mutex
	headers []http.Header
	ratios  []float64
}

func (f *idTracer) Decide(h http.Header, ratio float64, d *emit.Decision) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.headers = append(f.headers, h)
	f.ratios = append(f.ratios, ratio)
	*d = emit.Decision{TraceID: f.id}
}

// asked returns the headers and ratios Decide was called with.
func (f *idTracer) asked() ([]http.Header, []float64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.headers), slices.Clone(f.ratios)
}

// requestID is the RequestID function of the fixtures: the decided one,
// over a tracer that draws testRequestID.
func requestID(r *http.Request) string {
	return RequestID(&idTracer{id: testTraceID()})(r)
}

// logBuffer collects log output safely across goroutines.
type logBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// hopStatus is an emit.NodeStatus recording the admin hop.
type hopStatus struct {
	mu   sync.Mutex
	hops []int
}

func (*hopStatus) SetDegraded(catalog.Reason, string, bool) {}

func (h *hopStatus) SetCleartextHops(hop catalog.Hop, n int) {
	if hop != catalog.HopAdmin {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.hops = append(h.hops, n)
}

func (h *hopStatus) values() []int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]int(nil), h.hops...)
}

// named is a handler that answers its name, standing in for an injected
// handler.
func named(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, name)
	})
}

// testSnapshots is the injected /debug/snapshots view.
func testSnapshots() adminapi.Snapshots {
	lkg := "sha256:68f782530463d5c1f0e2b9a7c4d8e6f1a3b5c7d9e0f2a4b6c8d0e2f4a6b8c0d2"
	return adminapi.Snapshots{
		Snapshots: []adminapi.SnapshotInfo{{
			State: adminapi.StateActive, Revision: "rev-68f782530463", Digest: lkg, Pins: 3,
			ActivatedAt: "2026-09-26T12:00:00Z",
		}},
		LastKnownGood: &lkg,
	}
}

// fixture is a Server with every dependency injected.
type fixture struct {
	srv    *Server
	ready  *readiness.Tracker
	hub    *tap.Hub
	status *hopStatus
	logs   *logBuffer
}

// tokenFile writes a token file and returns its path.
func tokenFile(t *testing.T, name, token string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// newAuth returns an Authenticator for s that logs to logs.
func newAuth(t *testing.T, s adminauth.Settings, logs io.Writer) *adminauth.Authenticator {
	t.Helper()
	o := adminauth.Options{RequestID: requestID}
	if logs != nil {
		o.Logger = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	a, err := adminauth.New(s, o)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// operatorSettings has both tokens.
func operatorSettings(t *testing.T) adminauth.Settings {
	t.Helper()
	return adminauth.Settings{
		TokenFile:        tokenFile(t, "operator", operatorToken),
		MetricsTokenFile: tokenFile(t, "metrics", metricsToken),
	}
}

// newFixture builds a cleartext Server with every handler injected; edit
// changes the Config before New.
func newFixture(t *testing.T, edit func(*Config)) *fixture {
	t.Helper()
	f := &fixture{ready: readiness.New(), hub: tap.New(tap.Config{RequestID: requestID}), status: &hopStatus{}, logs: &logBuffer{}}
	cfg := Config{
		Auth:       newAuth(t, operatorSettings(t), f.logs),
		Readiness:  f.ready,
		Tap:        f.hub,
		Metrics:    named("metrics"),
		ConfigDump: named("dump"),
		Upstreams:  named("upstreams"),
		Snapshots:  testSnapshots,
		RequestID:  requestID,
		Status:     f.status,
	}
	if edit != nil {
		edit(&cfg)
	}
	srv, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	f.srv = srv
	return f
}

// do sends one in-process request from a loopback peer, with token as a
// bearer credential when not empty.
func (f *fixture) do(t *testing.T, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	return doCtx(context.Background(), t, f.srv, method, target, token)
}

// doCtx is do with a request context.
func doCtx(ctx context.Context, t *testing.T, s *Server, method, target, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, method, target, nil)
	req.RemoteAddr = loopbackPeer
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// checkProblem checks a problem document answer.
func checkProblem(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status %d, want %d (%s)", w.Code, status, w.Body)
	}
	if got := w.Header().Get("Content-Type"); got != problem.ContentType {
		t.Errorf("Content-Type %q, want %q", got, problem.ContentType)
	}
	d, err := problem.Decode(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if d.Code != code || d.Status != status {
		t.Errorf("problem %+v, want code %s status %d", d, code, status)
	}
	if d.RequestID != testRequestID {
		t.Errorf("requestId %q, want %q", d.RequestID, testRequestID)
	}
}

// serve runs s on a loopback listener until the test ends and returns
// the base URL.
func serve(t *testing.T, s *Server, scheme string) string {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- s.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Shutdown at once: the test is over.
		_ = s.Shutdown(ctx)
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("Serve = %v, want http.ErrServerClosed", err)
		}
	})
	return scheme + "://" + ln.Addr().String()
}

// clientGet sends GET with the bearer token (if any) through c.
func clientGet(t *testing.T, c *http.Client, url, token string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
