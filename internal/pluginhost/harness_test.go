// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pluginhost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	ht "github.com/ravindu-rev/ruralz/internal/pluginhost/pluginhosttest"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// epoch is the fake clock's start.
func epoch() time.Time { return time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC) }

// memSource serves artifacts from memory by digest.
type memSource struct {
	mu      sync.Mutex
	bins    map[revision.Digest][]byte
	fetches int
	err     error
}

func (s *memSource) Fetch(_ context.Context, _ string, d revision.Digest) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	if s.err != nil {
		return nil, s.err
	}
	b, ok := s.bins[d]
	if !ok {
		return nil, errcode.Wrap(CodeArtifact, errors.New("not in the registry"))
	}
	return b, nil
}

// add stores bin and returns its image reference.
func (s *memSource) add(bin []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.bins == nil {
		s.bins = map[revision.Digest][]byte{}
	}
	sum := sha256.Sum256(bin)
	ref := "sha256:" + hex.EncodeToString(sum[:])
	d, err := revision.Parse(ref)
	if err != nil {
		panic(err)
	}
	s.bins[d] = bin
	return "registry.example/acme/plugin:1.0@" + ref
}

// statusRecorder records degraded reasons by source.
type statusRecorder struct {
	mu  sync.Mutex
	set map[catalog.Reason]map[string]bool
}

func (r *statusRecorder) SetDegraded(reason catalog.Reason, source string, on bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.set == nil {
		r.set = map[catalog.Reason]map[string]bool{}
	}
	if r.set[reason] == nil {
		r.set[reason] = map[string]bool{}
	}
	if on {
		r.set[reason][source] = true
	} else {
		delete(r.set[reason], source)
	}
}

func (*statusRecorder) SetCleartextHops(catalog.Hop, int) {}

// raised reports whether any source holds reason.
func (r *statusRecorder) raised(reason catalog.Reason) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.set[reason]) > 0
}

// recordedTelemetry records one Policy's Plugin telemetry.
type recordedTelemetry struct {
	mu        sync.Mutex
	calls     int
	failures  map[string]int
	idle      int
	busy      int
	events    map[string]int64
	truncated int
	dropped   int
}

func (t *recordedTelemetry) Call(phase.Phase, time.Duration, time.Duration) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
}
func (*recordedTelemetry) Wait(time.Duration) {}
func (t *recordedTelemetry) Failure(code string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.failures == nil {
		t.failures = map[string]int{}
	}
	t.failures[code]++
}

func (t *recordedTelemetry) Instances(idle, busy int) {
	t.mu.Lock()
	t.idle, t.busy = idle, busy
	t.mu.Unlock()
}

func (t *recordedTelemetry) GuestEvent(_ int, name string, n int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.events == nil {
		t.events = map[string]int64{}
	}
	t.events[name] += n
}
func (t *recordedTelemetry) LogTruncated() { t.mu.Lock(); t.truncated++; t.mu.Unlock() }
func (t *recordedTelemetry) LogDropped()   { t.mu.Lock(); t.dropped++; t.mu.Unlock() }

// failureCount returns the failures counted with code.
func (t *recordedTelemetry) failureCount(code string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.failures[code]
}

// zeroReader is a deterministic host random source.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte(i)
	}
	return len(p), nil
}

// logBuffer collects log records as text.
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

// harness runs an Engine on a fake clock.
type harness struct {
	t      *testing.T
	clk    *clocktest.Fake
	src    *memSource
	status *statusRecorder
	logs   *logBuffer
	e      *Engine
	tel    map[string]*recordedTelemetry
	telMu  sync.Mutex
}

// newHarness returns a running Engine with four processors.
func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clk: clocktest.New(epoch()), src: &memSource{}, status: &statusRecorder{}, logs: &logBuffer{}, tel: map[string]*recordedTelemetry{}}
	h.e = NewEngine(Options{
		Clock: h.clk, Source: h.src, Status: h.status, Rand: zeroReader{}, Procs: 4,
		Logger:   slog.New(slog.NewTextHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		MemLimit: func() int64 { return 16 << 30 },
		Measure:  func() (int64, bool) { return 0, false },
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = h.e.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return h
}

// telemetry returns the recorder of a Policy.
func (h *harness) telemetry(policy, _ string) Telemetry {
	h.telMu.Lock()
	defer h.telMu.Unlock()
	if h.tel[policy] == nil {
		h.tel[policy] = &recordedTelemetry{}
	}
	return h.tel[policy]
}

// rec returns a Policy's recorded telemetry.
func (h *harness) rec(policy string) *recordedTelemetry {
	h.telMu.Lock()
	defer h.telMu.Unlock()
	return h.tel[policy]
}

// pluginOpts configure one test Plugin.
type pluginOpts struct {
	name     string
	bin      []byte
	phases   []v1alpha1.Phase
	caps     []v1alpha1.Capability
	memBytes int64
	timeout  time.Duration
	schema   map[string]any
	// policy names the plugin Policy; config is its config.
	policy string
	config map[string]any
	class  v1alpha1.FilterClass
	// capBytes is the Gateway's limits.maxPluginMemoryBytes, 0 for none.
	capBytes int64
}

// bundleOf returns a Bundle with a Gateway, the Plugins and their Policies.
func (h *harness) bundleOf(ps ...pluginOpts) *hub.Bundle {
	var rs []*hub.Resource
	gw := &v1alpha1.Gateway{Metadata: v1alpha1.ObjectMeta{Name: "edge"}}
	for _, p := range ps {
		if p.capBytes > 0 {
			c := v1alpha1.ByteSize(p.capBytes)
			gw.Spec.Limits = &v1alpha1.Limits{MaxPluginMemoryBytes: &c}
		}
	}
	rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindGateway, Name: "edge"}, Object: gw})
	for _, p := range ps {
		spec := v1alpha1.PluginSpec{Image: h.src.add(p.bin), ABI: v1alpha1.PluginABIV1, Phases: p.phases, Capabilities: p.caps, ConfigSchema: p.schema}
		if p.memBytes > 0 || p.timeout > 0 {
			spec.Limits = &v1alpha1.PluginLimits{}
			if p.memBytes > 0 {
				m := v1alpha1.ByteSize(p.memBytes)
				spec.Limits.MemoryBytes = &m
			}
			if p.timeout > 0 {
				d := v1alpha1.Duration(p.timeout)
				spec.Limits.Timeout = &d
			}
		}
		rs = append(rs, &hub.Resource{
			ID:     hub.ID{Kind: v1alpha1.KindPlugin, Name: p.name},
			Object: &v1alpha1.Plugin{Metadata: v1alpha1.ObjectMeta{Name: p.name}, Spec: spec},
		})
		pol := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: p.policy}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypePlugin, Plugin: p.name}}
		if p.class != "" {
			c := p.class
			pol.Spec.FilterClass = &c
		}
		cfg := v1alpha1.PluginConfig(p.config)
		if cfg == nil {
			cfg = v1alpha1.PluginConfig{}
		}
		raw, _ := json.Marshal(cfg)
		pol.Spec.Config = raw
		rs = append(rs, &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: p.policy}, Object: pol, Config: &cfg})
	}
	return hub.NewBundle(rs)
}

// buildOpts configure one Build.
type buildOpts struct {
	scope    phase.Scope
	store    statestore.Store
	enqueue  statestore.Enqueuer
	secrets  secret.Store
	previous filter.Filter
	env      string
	limits   filter.Limits
}

// build builds the Filter of Policy name in b.
func (h *harness) build(b *hub.Bundle, name string, o buildOpts) (*Filter, error) {
	h.t.Helper()
	r, ok := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: name})
	if !ok {
		h.t.Fatalf("no Policy %q", name)
	}
	pol := r.Object.(*v1alpha1.Policy)
	scope := o.scope
	if scope == phase.ScopeNone {
		scope = phase.ScopeRoute
	}
	env := filter.BuildEnv{
		Policy: pol, Config: r.Config, Scope: scope, Bundle: b, Secrets: o.secrets,
		StateStore: o.store, Enqueuer: o.enqueue, StateStoreTimeout: 50 * time.Millisecond,
		Limits: o.limits, Clock: h.clk, Logger: h.e.log, Previous: o.previous,
	}
	f, err := Factory{Engine: h.e, Environment: o.env, Telemetry: h.telemetry}.Build(context.Background(), env)
	if err != nil {
		return nil, err
	}
	return f.(*Filter), nil
}

// mustBuild builds or fails the test.
func (h *harness) mustBuild(b *hub.Bundle, name string, o buildOpts) *Filter {
	h.t.Helper()
	f, err := h.build(b, name, o)
	if err != nil {
		h.t.Fatalf("Build(%q) = %v", name, err)
	}
	h.t.Cleanup(func() { _ = f.Close() })
	return f
}

// one builds a Filter for a single Plugin.
func (h *harness) one(p pluginOpts, o buildOpts) *Filter {
	h.t.Helper()
	if p.name == "" {
		p.name = "p"
	}
	if p.policy == "" {
		p.policy = "pol"
	}
	return h.mustBuild(h.bundleOf(p), p.policy, o)
}

// exchange returns a request Exchange with headers.
func exchange(hdr http.Header) *filtertest.Exchange {
	if hdr == nil {
		hdr = http.Header{}
	}
	rb := statestore.NewRequestBudget(time.Second, 0)
	return &filtertest.Exchange{
		ID: "0123456789abcdef0123456789abcdef", MethodV: "GET", SchemeV: "https", HostV: "api.example", PathV: "/orders",
		Hdr: hdr, RouteV: &expr.Route{Name: "orders"}, Msg: &filtertest.Message{Hdr: hdr},
		StateDeadline: &rb,
	}
}

// codeOf returns a Result's code, "" for Continue and Respond.
func codeOf(r filter.Result) string { return r.Code }

// wantCode fails unless err carries code.
func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("got no error, want %s", code)
	}
	if got, _ := errcode.CodeOf(err); got != code {
		t.Fatalf("error %v has code %q, want %s", err, got, code)
	}
}

// simple returns a guest exporting rz_on_request_headers with body.
func simple(imports []string, locals []ht.Locals, body func(g *ht.Guest) [][]byte) []byte {
	g := ht.NewGuest(imports...)
	g.Phase("rz_on_request_headers", locals, body(g)...)
	return g.Encode()
}

// headersPlugin is a Plugin of the guest bin in onRequestHeaders with caps.
func headersPlugin(bin []byte, caps ...v1alpha1.Capability) pluginOpts {
	return pluginOpts{bin: bin, phases: []v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders}, caps: caps}
}
