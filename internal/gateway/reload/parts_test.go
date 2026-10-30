// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the Parts an activation switches: the client listeners, the
// admin port and the telemetry settings.

// The admin socket is the admin port's handover socket group.
var (
	_ handover.Sockets = (*AdminSocket)(nil)
	_ handover.Pinner  = (*AdminSocket)(nil)
	_ Listener         = (*AdminSocket)(nil)
	_ Stopper          = (*AdminSocket)(nil)
	_ Part             = (*AdminSocket)(nil)
)

// testServer is an admin server answering "admin" to every request.
type testServer struct {
	mu    sync.Mutex
	srvs  []*http.Server
	fault error // returned by every Serve instead of serving
}

func (s *testServer) Serve(ln net.Listener) error {
	s.mu.Lock()
	fault := s.fault
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "admin") }),
		ReadHeaderTimeout: bound,
	}
	s.srvs = append(s.srvs, srv)
	s.mu.Unlock()
	if fault != nil {
		return fault
	}
	return srv.Serve(ln)
}

func (s *testServer) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, srv := range s.srvs {
		_ = srv.Close()
	}
}

// get sends one request to addr and returns the answer, "" when it fails.
func get(t *testing.T, addr string) string {
	t.Helper()
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// freePort returns a loopback port no socket holds right now.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// adminCompiled is a compiled snapshot naming the admin port.
func adminCompiled(port int) Compiled {
	return &fakeCompiled{snap: &snapshot.Snapshot{Gateway: snapshot.Gateway{AdminPort: port}}, ev: &events{}}
}

func newAdminSocket(t *testing.T, steer bool, fail func(error)) (*AdminSocket, *testServer) {
	t.Helper()
	srv := &testServer{}
	a, err := NewAdminSocket(AdminSocketConfig{Server: srv, Host: "127.0.0.1", Steer: steer, Fail: fail})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		srv.close()
		_ = a.Close()
	})
	return a, srv
}

func TestAdminSocketHolderServesAtOnce(t *testing.T) {
	a, _ := newAdminSocket(t, true, nil)
	port := freePort(t)
	if err := a.Bind(t.Context(), port); err != nil {
		t.Fatal(err)
	}
	if got := get(t, a.Addr().String()); got != "admin" {
		t.Fatalf("answer %q, want the admin server serving at once", got)
	}
	if a.Port() != port {
		t.Fatalf("Port = %d, want %d", a.Port(), port)
	}
	// Listen of a socket already served does nothing.
	if err := a.Listen(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestAdminSocketMovesThePort(t *testing.T) {
	a, _ := newAdminSocket(t, true, nil)
	oldPort, newPort := freePort(t), freePort(t)
	if err := a.Bind(t.Context(), oldPort); err != nil {
		t.Fatal(err)
	}
	oldAddr := a.Addr().String()
	// The same port changes nothing.
	sw, err := a.Prepare(t.Context(), adminCompiled(oldPort))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sw.(nopSwitch); !ok {
		t.Fatalf("Prepare(same port) = %T, want no change", sw)
	}
	sw, err = a.Prepare(t.Context(), adminCompiled(newPort))
	if err != nil {
		t.Fatal(err)
	}
	// Before the swap the old port still serves.
	if got := get(t, oldAddr); got != "admin" {
		t.Fatalf("old port answers %q before the swap", got)
	}
	if err := sw.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	newAddr := net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort))
	if got := get(t, newAddr); got != "admin" {
		t.Fatalf("new port answers %q", got)
	}
	if got := get(t, oldAddr); got != "" {
		t.Fatalf("old port still answers %q after the move", got)
	}
	if a.Port() != newPort {
		t.Fatalf("Port = %d", a.Port())
	}
}

func TestAdminSocketBusyPortRejectsTheRevision(t *testing.T) {
	a, _ := newAdminSocket(t, true, nil)
	if err := a.Bind(t.Context(), freePort(t)); err != nil {
		t.Fatal(err)
	}
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	_, err = a.Prepare(t.Context(), adminCompiled(busy.Addr().(*net.TCPAddr).Port))
	if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Fatalf("Prepare(busy port) = %v, want RZ-CFG-039", err)
	}
	if err := a.Bind(t.Context(), busy.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("Bind of a busy port succeeded")
	}
}

func TestAdminSocketAbortClosesTheNewSocket(t *testing.T) {
	a, _ := newAdminSocket(t, true, nil)
	if err := a.Bind(t.Context(), freePort(t)); err != nil {
		t.Fatal(err)
	}
	newPort := freePort(t)
	sw, err := a.Prepare(t.Context(), adminCompiled(newPort))
	if err != nil {
		t.Fatal(err)
	}
	sw.Abort()
	// The port is free again: a plain listener takes it.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(newPort)))
	if err != nil {
		t.Fatalf("the aborted port is still held: %v", err)
	}
	_ = ln.Close()
}

func TestAdminSocketServeFailureIsFatal(t *testing.T) {
	failed := make(chan error, 1)
	a, srv := newAdminSocket(t, true, func(err error) { failed <- err })
	srv.mu.Lock()
	srv.fault = errors.New("accept: too many open files")
	srv.mu.Unlock()
	if err := a.Bind(t.Context(), freePort(t)); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		if err.Error() != "accept: too many open files" {
			t.Fatalf("Fail(%v)", err)
		}
	case <-time.After(bound):
		t.Fatal("a failing Serve did not reach Fail")
	}
}

func TestAdminSocketStopAccepting(t *testing.T) {
	failed := make(chan error, 1)
	a, _ := newAdminSocket(t, true, func(err error) { failed <- err })
	if err := a.Bind(t.Context(), freePort(t)); err != nil {
		t.Fatal(err)
	}
	addr := a.Addr().String()
	a.StopAccepting()
	if got := get(t, addr); got != "" {
		t.Fatalf("answer %q after StopAccepting", got)
	}
	// Later activations bind nothing.
	if sw, err := a.Prepare(t.Context(), adminCompiled(freePort(t))); err != nil {
		t.Fatal(err)
	} else if _, ok := sw.(nopSwitch); !ok {
		t.Fatalf("Prepare after StopAccepting = %T", sw)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	// Closing the socket is no Serve failure.
	select {
	case err := <-failed:
		t.Fatalf("Fail(%v) after StopAccepting", err)
	default:
	}
}

func TestAdminSocketBeforeBind(t *testing.T) {
	a, _ := newAdminSocket(t, false, nil)
	if a.Addr() != nil || a.Port() != 0 {
		t.Fatal("an unbound socket has an address")
	}
	for name, err := range map[string]error{
		"Pin": a.Pin(), "SteerToSuccessor": a.SteerToSuccessor(), "WaitLingers": a.WaitLingers(t.Context()), "Listen": a.Listen(t.Context()),
	} {
		if err != nil {
			t.Errorf("%s = %v before Bind", name, err)
		}
	}
	if _, err := NewAdminSocket(AdminSocketConfig{}); !errors.Is(err, ErrConfig) {
		t.Fatalf("NewAdminSocket without a server = %v", err)
	}
}

func TestListenerPart(t *testing.T) {
	set, err := listener.New(listener.Options{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "client") }),
		Conns:   admission.NewConnLimiter(admission.DefaultConnections), Host: "127.0.0.1", Steer: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = set.Close() })
	p := ListenerPart(set)
	next := &fakeCompiled{ev: &events{}, snap: &snapshot.Snapshot{
		Listeners: []*snapshot.Listener{{Name: "web", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 0}},
		Gateway:   snapshot.Gateway{AdminPort: 9901},
	}}
	sw, err := p.Prepare(t.Context(), next)
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	addr := set.Addr("web")
	if addr == nil {
		t.Fatal("the listener is not serving")
	}
	if got := get(t, addr.String()); got != "client" {
		t.Fatalf("answer %q", got)
	}
	// An aborted Plan binds nothing new.
	next2 := &fakeCompiled{ev: &events{}, snap: &snapshot.Snapshot{
		Listeners: []*snapshot.Listener{{Name: "web", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 0}, {Name: "other", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 0}},
		Gateway:   snapshot.Gateway{AdminPort: 9901},
	}}
	sw, err = p.Prepare(t.Context(), next2)
	if err != nil {
		t.Fatal(err)
	}
	sw.Abort()
	if set.Addr("other") != nil {
		t.Fatal("an aborted listener serves")
	}
	p.(Stopper).StopAccepting()
	if set.Bound() {
		t.Fatal("the Set still accepts after StopAccepting")
	}
}

// fakeTelemetry records what the telemetry Part switches.
type fakeTelemetry struct {
	mu       sync.Mutex
	revision string
	headers  []string
	applied  []telemetry.Settings
	err      error
}

func (f *fakeTelemetry) Apply(_ context.Context, s telemetry.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, s)
	return f.err
}

func (f *fakeTelemetry) SetActiveRevision(d string) {
	f.mu.Lock()
	f.revision = d
	f.mu.Unlock()
}

func (f *fakeTelemetry) SetCredentialHeaders(n []string) {
	f.mu.Lock()
	f.headers = slices.Clone(n)
	f.mu.Unlock()
}

// telemetryCompiled is a snapshot of a Revision with telemetry spec t and
// two auth.api-key Policies.
func telemetryCompiled(t *v1alpha1.Telemetry) Compiled {
	header := "x-tenant-key"
	gw := &v1alpha1.Gateway{Spec: v1alpha1.GatewaySpec{Telemetry: t}}
	rs := []*hub.Resource{
		{ID: hub.ID{Kind: v1alpha1.KindGateway, Name: "edge"}, Object: gw},
		{
			ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "keys"},
			Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "keys"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
			Config: &v1alpha1.AuthAPIKeyConfig{Header: &header},
		},
		{
			ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "legacy"},
			Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "legacy"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeAuthAPIKey}},
		},
		{
			ID:     hub.ID{Kind: v1alpha1.KindPolicy, Name: "cors"},
			Object: &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: "cors"}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeCORS}},
		},
	}
	v := &hub.Validated{Revision: rev("T"), Bundle: hub.NewBundle(rs)}
	return &fakeCompiled{ev: &events{}, snap: &snapshot.Snapshot{Revision: v.Revision, Validated: v}}
}

func TestTelemetryPart(t *testing.T) {
	ft := &fakeTelemetry{}
	p := TelemetryPart(ft)
	ratio := 0.25
	sw, err := p.Prepare(t.Context(), telemetryCompiled(&v1alpha1.Telemetry{TraceSampling: &ratio}))
	if err != nil {
		t.Fatal(err)
	}
	if len(ft.applied) != 0 || ft.revision != "" {
		t.Fatal("Prepare switched the runtime before the swap")
	}
	if err := sw.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if ft.revision != revision.Sum([]byte("T")).Short() {
		t.Errorf("active revision %q", ft.revision)
	}
	if want := []string{"x-tenant-key", "x-api-key"}; !slices.Equal(ft.headers, want) {
		t.Errorf("credential headers %v, want %v", ft.headers, want)
	}
	if len(ft.applied) != 1 || ft.applied[0].Sampling() != 0.25 {
		t.Fatalf("applied %+v, want the Revision's sampling", ft.applied)
	}
	sw.Abort() // after Commit, the runtime owns the settings
}

func TestTelemetryPartFailures(t *testing.T) {
	ft := &fakeTelemetry{}
	p := TelemetryPart(ft)
	// An endpoint the schema would refuse is a coded Prepare error.
	_, err := p.Prepare(t.Context(), telemetryCompiled(&v1alpha1.Telemetry{OTLP: &v1alpha1.OTLP{Endpoint: "ftp://collector"}}))
	if _, ok := errcode.CodeOf(err); !ok {
		t.Fatalf("Prepare = %v, want a coded error", err)
	}
	// Abort before Commit closes the unadopted settings.
	sw, err := p.Prepare(t.Context(), telemetryCompiled(nil))
	if err != nil {
		t.Fatal(err)
	}
	sw.Abort()
	ft.err = errors.New("exporter failed")
	sw, err = p.Prepare(t.Context(), telemetryCompiled(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := sw.Commit(t.Context()); err == nil {
		t.Fatal("a failed Apply was not reported")
	}
	// A snapshot without a validated Revision compiles the defaults.
	sw, err = p.Prepare(t.Context(), &fakeCompiled{ev: &events{}, snap: &snapshot.Snapshot{}})
	if err != nil {
		t.Fatal(err)
	}
	sw.Abort()
}
