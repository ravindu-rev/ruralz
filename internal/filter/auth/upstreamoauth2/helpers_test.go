// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// waitLimit bounds every wait for an event a test caused; reaching it
// fails the test instead of hanging it. No assertion depends on timing.
const waitLimit = 10 * time.Second

// Test identities: the client, its secret's reference and the secret.
const (
	testClientID = "gateway edge"
	testSecret   = "s3cr:et+/&=% value" //nolint:gosec // G101: a test fixture, not a credential
	testPolicy   = "orders-token"
)

// testStart is the fake clock's start time.
func testStart() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

// testRef is the clientSecret reference of every test config.
func testRef() v1alpha1.SecretRef {
	return v1alpha1.SecretRef{Provider: v1alpha1.SecretProviderFile, Name: "/etc/ruralz/oauth/client.json", Key: "secret"}
}

// fakeSecrets is a secret.Store over a map; set replaces a value, as a
// rotation does. A nil getHook leaves Get plain.
type fakeSecrets struct {
	mu      sync.Mutex
	vals    map[secret.Ref]secret.Value
	getHook func()
}

var _ secret.Store = (*fakeSecrets)(nil)

func newSecrets(value string) *fakeSecrets {
	s := &fakeSecrets{vals: map[secret.Ref]secret.Value{}}
	s.set(secret.RefOf(testRef()), value)
	return s
}

func (s *fakeSecrets) set(r secret.Ref, v string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vals[r] = secret.NewValue([]byte(v))
}

// Get implements secret.Store.
func (s *fakeSecrets) Get(r secret.Ref) (secret.Value, bool) {
	s.mu.Lock()
	hook := s.getHook
	v, ok := s.vals[r]
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return v, ok
}

// Watch implements secret.Store; nothing watches in these tests.
func (*fakeSecrets) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// recGauge records the last value set and how often it was set.
type recGauge struct{ v, sets atomic.Int64 }

func (g *recGauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }
func (g *recGauge) Set(v int64)                { g.v.Store(v); g.sets.Add(1) }

// recCounter records its total.
type recCounter struct{ n atomic.Uint64 }

func (c *recCounter) Add(_ emit.Stripe, n uint64) { c.n.Add(n) }

// syncBuffer is a log destination safe for concurrent writes.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return bytes.Clone(b.b.Bytes())
}

// tokenRequest is one request the token server received.
type tokenRequest struct {
	method, path, rawQuery string
	header                 http.Header
	form                   url.Values
	// basic reports Basic credentials; rawUser and rawPass are them as
	// sent, user and pass form-decoded (RFC 6749 section 2.3.1).
	basic            bool
	rawUser, rawPass string
	user, pass       string
}

// responder answers the n-th request (from 1) of a token server.
type responder func(n int, w http.ResponseWriter, r *http.Request)

// tokenServer is an httptest TLS token endpoint whose answers a test
// scripts; it records every request.
type tokenServer struct {
	srv     *httptest.Server
	respond atomic.Pointer[responder]
	arrived chan int
	mu      sync.Mutex
	reqs    []tokenRequest
}

func newTokenServer(t *testing.T, r responder) *tokenServer {
	t.Helper()
	ts := &tokenServer{arrived: make(chan int, 1024)}
	ts.setResponder(r)
	ts.srv = httptest.NewTLSServer(http.HandlerFunc(ts.serve))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *tokenServer) setResponder(r responder) { ts.respond.Store(&r) }

func (ts *tokenServer) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	form, _ := url.ParseQuery(string(body))
	rec := tokenRequest{method: r.Method, path: r.URL.Path, rawQuery: r.URL.RawQuery, header: r.Header.Clone(), form: form}
	if u, p, ok := r.BasicAuth(); ok {
		rec.basic, rec.rawUser, rec.rawPass = true, u, p
		rec.user, _ = url.QueryUnescape(u)
		rec.pass, _ = url.QueryUnescape(p)
	}
	ts.mu.Lock()
	ts.reqs = append(ts.reqs, rec)
	n := len(ts.reqs)
	ts.mu.Unlock()
	ts.arrived <- n
	(*ts.respond.Load())(n, w, r)
}

// url returns the token endpoint URL.
func (ts *tokenServer) url() string { return ts.srv.URL + "/oauth2/token" }

// count returns the number of requests received.
func (ts *tokenServer) count() int {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	return len(ts.reqs)
}

// request returns the n-th request (from 1).
func (ts *tokenServer) request(t *testing.T, n int) tokenRequest {
	t.Helper()
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if n < 1 || n > len(ts.reqs) {
		t.Fatalf("token server got %d requests, want request %d", len(ts.reqs), n)
	}
	return ts.reqs[n-1]
}

// waitArrival waits until request n arrived.
func (ts *tokenServer) waitArrival(t *testing.T, n int) {
	t.Helper()
	deadline := time.NewTimer(waitLimit)
	defer deadline.Stop()
	for ts.count() < n {
		select {
		case <-ts.arrived:
		case <-deadline.C:
			t.Fatalf("token request %d never arrived", n)
		}
	}
}

// roots trusts the server's certificate under the Node's client policy.
func (ts *tokenServer) roots() *tls.Config {
	pool := x509.NewCertPool()
	pool.AddCert(ts.srv.Certificate())
	return tlsconf.Client(pool)
}

// writeJSON answers status with a JSON body.
func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// tokenBody returns a token response for tok expiring in expiresIn
// seconds.
func tokenBody(tok string, expiresIn int) string {
	b, _ := json.Marshal(map[string]any{"access_token": tok, "token_type": "Bearer", "expires_in": expiresIn})
	return string(b)
}

// issue answers every request n with token "tok-<n>" expiring in
// expiresIn seconds.
func issue(expiresIn int) responder {
	return func(n int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, tokenBody("tok-"+strconv.Itoa(n), expiresIn))
	}
}

// fail answers every request with status and an RFC 6749 error.
func fail(status int, code string) responder {
	return func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, status, `{"error":"`+code+`"}`)
	}
}

// gate is a release signal that test cleanup also releases, so a handler
// waiting on it never outlives the test.
type gate struct {
	ch   chan struct{}
	once sync.Once
}

func newGate(t *testing.T) *gate {
	g := &gate{ch: make(chan struct{})}
	t.Cleanup(g.open)
	return g
}

func (g *gate) open() { g.once.Do(func() { close(g.ch) }) }

// held answers with next once g opens; a request whose client went away
// first gets nothing.
func held(g *gate, next responder) responder {
	return func(n int, w http.ResponseWriter, r *http.Request) {
		select {
		case <-g.ch:
			next(n, w, r)
		case <-r.Context().Done():
		}
	}
}

// harness is a Registry on a fake clock, trusting a token server on
// loopback, with Run on a goroutine the test owns.
type harness struct {
	t        *testing.T
	clk      *clocktest.Fake
	ts       *tokenServer
	reg      *Registry
	secrets  *fakeSecrets
	logs     *syncBuffer
	fetched  atomic.Int64
	fetchSig chan struct{}

	runMu     sync.Mutex
	runCancel context.CancelFunc
	runDone   chan error
}

// newHarness returns a harness whose Registry is not running yet.
func newHarness(t *testing.T, r responder) *harness {
	t.Helper()
	h := &harness{
		t:        t,
		clk:      clocktest.New(testStart()),
		ts:       newTokenServer(t, r),
		secrets:  newSecrets(testSecret),
		logs:     &syncBuffer{},
		fetchSig: make(chan struct{}, 1),
	}
	guard, err := egress.ParseAllow("127.0.0.1/32")
	if err != nil {
		t.Fatal(err)
	}
	h.reg = NewRegistry(Options{
		Guard:  guard,
		TLS:    h.ts.roots(),
		Clock:  h.clk,
		Logger: slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	h.reg.fetchHook = func() {
		h.fetched.Add(1)
		select {
		case h.fetchSig <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { _ = h.stop() })
	return h
}

// running returns a harness whose Registry runs.
func running(t *testing.T, r responder) *harness {
	t.Helper()
	h := newHarness(t, r)
	h.start()
	return h
}

// start runs the Registry on a goroutine that stop cancels and waits for,
// and returns once Run is active, so a miss right after it never meets a
// Registry that is not running yet.
func (h *harness) start() {
	h.t.Helper()
	h.runMu.Lock()
	defer h.runMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.reg.Run(ctx) }()
	h.runCancel, h.runDone = cancel, done
	waitFor(h.t, "Run to start", func() bool { return h.reg.state.Load() == stateRunning })
}

// waitFor polls cond until it holds, failing the test at waitLimit.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitLimit)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %v for %s", waitLimit, what)
		}
		time.Sleep(time.Millisecond)
	}
}

// advanceArmed advances the fake clock by d once Run armed its timer for
// the scheduled fetch. Run computes the timer's duration from the clock
// before arming it, so an advance between the two would arm it late by the
// advance; a real clock moves too little between them to matter.
func (h *harness) advanceArmed(d time.Duration) {
	h.t.Helper()
	waitFor(h.t, "Run to arm its timer", func() bool { return h.clk.Pending() > 0 })
	h.clk.Advance(d)
}

// stop cancels Run and returns its result once it returned; nil when Run
// is not running.
func (h *harness) stop() error {
	h.runMu.Lock()
	defer h.runMu.Unlock()
	if h.runCancel == nil {
		return nil
	}
	h.runCancel()
	err := <-h.runDone
	h.runCancel, h.runDone = nil, nil
	return err
}

// waitFetches waits until n fetches finished.
func (h *harness) waitFetches(n int64) {
	h.t.Helper()
	deadline := time.NewTimer(waitLimit)
	defer deadline.Stop()
	for h.fetched.Load() < n {
		select {
		case <-h.fetchSig:
		case <-deadline.C:
			h.t.Fatalf("%d token fetches finished, want %d", h.fetched.Load(), n)
		}
	}
}

// config returns a valid config for the token server, changed by mods.
func (h *harness) config(mods ...func(*v1alpha1.AuthUpstreamOAuth2Config)) *v1alpha1.AuthUpstreamOAuth2Config {
	cfg := &v1alpha1.AuthUpstreamOAuth2Config{
		TokenURL:     h.ts.url(),
		ClientID:     testClientID,
		ClientSecret: v1alpha1.SecretValue{SecretRef: testRef()},
		Scopes:       []string{"orders.read", "orders.write"},
	}
	for _, m := range mods {
		m(cfg)
	}
	return cfg
}

// built is a Filter with the handles it records into.
type built struct {
	*Filter
	age      *recGauge
	failures *recCounter
}

// build builds the Policy name with cfg on the harness's Registry; the
// test's cleanup closes the Filter.
func (h *harness) build(name string, cfg *v1alpha1.AuthUpstreamOAuth2Config) built {
	h.t.Helper()
	b := built{age: &recGauge{}, failures: &recCounter{}}
	f, err := Factory{Registry: h.reg}.Build(h.t.Context(), filter.BuildEnv{
		Policy:  &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{Type: Type}},
		Config:  cfg,
		Scope:   phase.ScopeUpstream,
		Phases:  phase.Of(phase.OnUpstreamRequest),
		Secrets: h.secrets,
		Metrics: &emit.PolicyMetrics{UpstreamTokenAge: b.age, UpstreamRefreshes: b.failures},
	})
	if err != nil {
		h.t.Fatalf("Build: %v", err)
	}
	b.Filter = f.(*Filter)
	h.t.Cleanup(func() { _ = b.Close() })
	return b
}

// leg returns an onUpstreamRequest leg view whose outgoing request
// carries the client's own Authorization.
func leg() *filtertest.Exchange {
	return &filtertest.Exchange{
		Msg:  &filtertest.Message{Hdr: http.Header{"Authorization": {"Bearer client-jwt"}, "Accept": {"application/json"}}},
		LegV: &filter.Leg{Upstream: "orders", Attempt: 1},
	}
}

// handle runs onUpstreamRequest on a fresh leg view and returns the result
// and the outgoing Authorization values.
func handle(ctx context.Context, f filter.Filter) (filter.Result, []string) {
	x := leg()
	r := f.Handle(ctx, phase.OnUpstreamRequest, x)
	return r, x.Msg.Header().Values("Authorization")
}

// wantBearer fails unless r continued with Authorization "Bearer tok".
func wantBearer(t *testing.T, r filter.Result, auth []string, tok string) {
	t.Helper()
	if r.Outcome != filter.Continue || r.Err != nil {
		t.Fatalf("Handle = outcome %d, err %v; want Continue", r.Outcome, r.Err)
	}
	if len(auth) != 1 || auth[0] != "Bearer "+tok {
		t.Fatalf("Authorization = %q, want [%q]", auth, "Bearer "+tok)
	}
}

// wantUndecided fails unless r is CannotDecide with RZ-AUTH-020 and an
// error matching target.
func wantUndecided(t *testing.T, r filter.Result, target error) {
	t.Helper()
	if r.Outcome != filter.CannotDecide || r.Code != CodeUpstreamAuth {
		t.Fatalf("Handle = outcome %d code %q, want CannotDecide %s", r.Outcome, r.Code, CodeUpstreamAuth)
	}
	if target != nil && !errors.Is(r.Err, target) {
		t.Fatalf("Handle error = %v, want %v", r.Err, target)
	}
	if st := errcode.Status(r.Code); st != http.StatusUnauthorized {
		t.Fatalf("registered status of %s = %d, want 401", r.Code, st)
	}
}

// entryState returns the entry's refresh time and latest failure.
func entryState(e *entry) (refreshAt time.Time, lastErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshAt, e.lastErr
}
