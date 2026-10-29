// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// noisyBundle returns the base Bundle plus n Routes, each with a missing
// Upstream, an invalid host and a duplicate of the previous Route's
// criteria, and n Policies sharing one secret with two destinations: many
// findings of several codes across resources.
func noisyBundle(t testing.TB, n int) map[string]string {
	t.Helper()
	files := readDirs(t, baseDir)
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteString("---\n")
		}
		fmt.Fprintf(&b, `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: noisy-%03d
spec:
  match:
    hosts: ["bad.*.host%d"]
    path: {prefix: /noisy/%d}
  upstreams:
    - name: missing-%d
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: twin-%03d
spec:
  match:
    path: {prefix: /twin/%d}
  upstreams:
    - name: orders
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: twin-%03d-copy
spec:
  match:
    path: {prefix: /twin/%d}
  upstreams:
    - name: orders
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: oauth-%03d
spec:
  type: auth.upstream-oauth2
  config:
    tokenUrl: https://auth%d.shop.example/token
    clientId: gateway-edge
    clientSecret: {secretRef: {provider: file, name: /etc/ruralz/oauth/client-secret}}
`, i, i, i, i, i, i, i, i, i, i)
	}
	files["noisy.yaml"] = b.String()
	return files
}

// TestWorkersDeterministic: the diagnostics and secret uses of a run are
// the same for every worker count.
func TestWorkersDeterministic(t *testing.T) {
	b, files := loadFiles(t, noisyBundle(t, 40))
	v := newValidator(t, nil)
	res1, want := stageH(t, v, b, files, 1)
	if len(want) < 160 {
		t.Fatalf("want at least 160 findings, got %d", len(want))
	}
	for _, workers := range []int{0, 2, 3, 8, 64} {
		res, got := stageH(t, v, b, files, workers)
		if diagText(t, got) != diagText(t, want) {
			t.Errorf("workers %d: diagnostics differ from one worker", workers)
		}
		if secretsText(res.Secrets) != secretsText(res1.Secrets) {
			t.Errorf("workers %d: secret uses differ from one worker", workers)
		}
	}
	sorted := slices.Clone(want)
	sorted.Sort()
	if diagText(t, sorted) != diagText(t, want) {
		t.Error("the diagnostics are not in diag.List.Sort order")
	}
}

// TestMaxDiagnostics: a run returns the first MaxDiagnostics+1 findings
// of the uncapped run, in order, so a collector of the same cap sees the
// cap reached; below the cap it returns everything.
func TestMaxDiagnostics(t *testing.T) {
	b, files := loadFiles(t, noisyBundle(t, 20))
	_, all := stageH(t, newValidator(t, nil), b, files, 4)
	for _, maxDiags := range []int{1, 7, 50, len(all) - 1, len(all), len(all) + 5} {
		v := newValidator(t, func(o *Options) { o.MaxDiagnostics = maxDiags })
		_, got := stageH(t, v, b, files, 4)
		n := min(len(all), maxDiags+1)
		if diagText(t, got) != diagText(t, all[:n]) {
			t.Errorf("MaxDiagnostics %d: got %d findings, want the first %d of %d", maxDiags, len(got), n, len(all))
		}
	}
}

// TestNilAndEmptyBundle: a nil or empty Bundle has no findings and no
// secret uses.
func TestNilAndEmptyBundle(t *testing.T) {
	v := newValidator(t, nil)
	for _, b := range []*hub.Bundle{nil, hub.NewBundle(nil)} {
		res, ds, err := v.Run(t.Context(), b, nil, 4)
		if err != nil || len(ds) > 0 || len(res.Secrets) > 0 {
			t.Errorf("Run(%v) = %v, %v, %v", b, res, ds, err)
		}
	}
}

// TestNewOptions: New refuses missing schemas or registry, negative
// limits, a nil check and schemas without the hub apiVersion, each with
// ErrOptions; zero limits take the defaults.
func TestNewOptions(t *testing.T) {
	s := pipeline(t)
	empty, err := schemaidx.NewSet()
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		o    Options
	}{
		{"no schemas", Options{Registry: s.reg}},
		{"no registry", Options{Schemas: s.idx}},
		{"negative cap", Options{Schemas: s.idx, Registry: s.reg, MaxDiagnostics: -1}},
		{"negative yield", Options{Schemas: s.idx, Registry: s.reg, YieldAfter: -time.Second}},
		{"nil check", Options{Schemas: s.idx, Registry: s.reg, Checks: hub.Checks{v1alpha1.PolicyTypeCORS: nil}}},
		{"no hub schema", Options{Schemas: empty, Registry: s.reg}},
	}
	for _, tt := range tests {
		if _, err := New(tt.o); !errors.Is(err, ErrOptions) {
			t.Errorf("%s: New error %v, want ErrOptions", tt.name, err)
		}
	}
	v, err := New(Options{Schemas: s.idx, Registry: s.reg})
	if err != nil {
		t.Fatal(err)
	}
	if v.maxDiagnostics != DefaultMaxDiagnostics || v.yieldAfter != DefaultYieldAfter {
		t.Errorf("defaults %d, %v", v.maxDiagnostics, v.yieldAfter)
	}
	if v.defaults != (schemaDefaults{adminPort: 9901, maxSteps: 16, maxResponse: 10 << 20}) {
		t.Errorf("schema defaults %+v, want admin.port 9901, maxCompositionSteps 16, maxResponseBodyBytes 10Mi", v.defaults)
	}
}

// TestNewCopiesChecks: a check added to the caller's map after New is not
// run, so one Validator's behavior never changes under it.
func TestNewCopiesChecks(t *testing.T) {
	calls := 0
	checks := hub.Checks{}
	v := newValidator(t, func(o *Options) { o.Checks = checks })
	checks[v1alpha1.PolicyTypeAuthJWT] = func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) { calls++ }
	b, files := loadDirs(t, baseDir)
	stageH(t, v, b, files, 1)
	if calls != 0 {
		t.Errorf("a check added after New ran %d times", calls)
	}
}

// TestInjectedChecks: the check of a Policy type runs once per Policy of
// that type with its decoded config, its loc maps a path under spec.config
// to the source position, its findings reach the output with the
// resource filled in, and no other type's Policy reaches it.
func TestInjectedChecks(t *testing.T) {
	var calls atomic.Int32
	check := func(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
		calls.Add(1)
		cfg, ok := r.Config.(*v1alpha1.AuthJWTConfig)
		if !ok || r.Kind != v1alpha1.KindPolicy || r.Name != "jwt" {
			t.Errorf("check called with %s and config %T", r.ID, r.Config)
			return
		}
		p := diag.Path{
			diag.Field("spec"), diag.Field("config"), diag.Field("issuers"),
			diag.Keyed("issuer", cfg.Issuers[0].Issuer), diag.Field("jwksUrl"),
		}
		add(diag.Diagnostic{Code: "RZ-CFG-037", Severity: diag.SeverityError, Location: loc(p), Path: p, Message: "fake finding"})
	}
	v := newValidator(t, func(o *Options) { o.Checks = hub.Checks{v1alpha1.PolicyTypeAuthJWT: check} })
	b, files := loadDirs(t, baseDir)
	_, ds := stageH(t, v, b, files, 3)
	if calls.Load() != 1 {
		t.Fatalf("check ran %d times, want once", calls.Load())
	}
	want := "policies.yaml:10:18 error RZ-CFG-037 Policy/jwt spec.config.issuers[issuer=https://auth.shop.example].jwksUrl: fake finding\n"
	if got := diagText(t, ds); got != want {
		t.Errorf("findings:\n%s want:\n%s", got, want)
	}
}

// TestCheckPanicReachesCaller: a panic in an injected check stops the run
// and is raised again in the calling goroutine, wrapping the panic value
// and holding the worker's stack, for every worker count.
func TestCheckPanicReachesCaller(t *testing.T) {
	boom := errors.New("boom")
	check := func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) { panic(boom) }
	v := newValidator(t, func(o *Options) { o.Checks = hub.Checks{v1alpha1.PolicyTypeQuota: check} })
	b, files := loadDirs(t, baseDir)
	for _, workers := range []int{1, 4} {
		func() {
			defer func() {
				err, ok := recover().(error)
				if !ok || !errors.Is(err, boom) {
					t.Fatalf("workers %d: recovered %v, want an error wrapping boom", workers, err)
				}
				if !strings.Contains(err.Error(), "stage H panicked") || !strings.Contains(err.Error(), "goroutine") {
					t.Errorf("workers %d: panic text lacks the stage or the stack: %.200s", workers, err)
				}
			}()
			_, _, _ = v.Run(t.Context(), b, files, workers)
			t.Fatalf("workers %d: Run returned after a panicking check", workers)
		}()
	}
}

// TestCheckGoexitReachesCaller: a runtime.Goexit in a check, which leaves
// no panic to recover, is raised in the calling goroutine as an error
// wrapping errGoexit, so no resource is dropped silently.
func TestCheckGoexitReachesCaller(t *testing.T) {
	check := func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) { runtime.Goexit() }
	v := newValidator(t, func(o *Options) { o.Checks = hub.Checks{v1alpha1.PolicyTypeQuota: check} })
	var stopped atomic.Int64
	stopped.Store(-1)
	v.stopped = func(i int) { stopped.Store(int64(i)) }
	b, files := loadDirs(t, baseDir)
	defer func() {
		err, ok := recover().(error)
		if !ok || !errors.Is(err, errGoexit) || !strings.Contains(err.Error(), "stage H stopped") {
			t.Fatalf("recovered %v, want an error wrapping errGoexit", err)
		}
		r := b.Resources()[stopped.Load()]
		if r.Name != "quota-daily" {
			t.Errorf("stop recorded at %s, want the quota Policy", r.ID)
		}
	}()
	_, _, _ = v.Run(t.Context(), b, files, 2)
	t.Fatal("Run returned after a check ran runtime.Goexit")
}

// TestContextEnds: a context that ended before or during the run makes Run
// return its error and no result.
func TestContextEnds(t *testing.T) {
	b, files := loadDirs(t, baseDir)
	v := newValidator(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if res, ds, err := v.Run(ctx, b, files, 2); !errors.Is(err, context.Canceled) || ds != nil || res.Secrets != nil {
		t.Errorf("ended before: %v, %v, %v", res, ds, err)
	}
	ctx, cancel = context.WithCancel(t.Context())
	defer cancel()
	check := func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) { cancel() }
	v = newValidator(t, func(o *Options) { o.Checks = hub.Checks{v1alpha1.PolicyTypeQuota: check} })
	if res, ds, err := v.Run(ctx, b, files, 1); !errors.Is(err, context.Canceled) || ds != nil || res.Secrets != nil {
		t.Errorf("ended during: %v, %v, %v", res, ds, err)
	}
}

// TestYieldReadsInjectedClock: workers read the injected clock and yield
// once YieldAfter of work has passed; without a clock they never read one.
func TestYieldReadsInjectedClock(t *testing.T) {
	var reads atomic.Int64
	now := func() time.Time { return time.Unix(0, reads.Add(1)*int64(time.Millisecond)) }
	v := newValidator(t, func(o *Options) { o.Now, o.YieldAfter = now, time.Microsecond })
	b, files := loadDirs(t, "testdata/shop-bundle")
	stageH(t, v, b, files, 2)
	if reads.Load() == 0 {
		t.Fatal("the run never read the injected clock")
	}
	w := &work{now: now, after: time.Microsecond}
	w.charge(yieldEvery)
	w.charge(yieldEvery)
	if w.yields != 1 {
		t.Errorf("yields after two clock reads 1 ms apart = %d, want 1", w.yields)
	}
	idle := &work{}
	idle.charge(10 * yieldEvery)
	if idle.yields != 0 {
		t.Error("a timer without a clock yielded")
	}
}

// TestResourcesWithoutTree: a resource without a positioned tree, as a
// caller may build, is checked by its typed view and located at its
// start; Environment and Cluster resources, which stage C keeps out of
// a Bundle, are skipped.
func TestResourcesWithoutTree(t *testing.T) {
	gw := &v1alpha1.Gateway{Spec: v1alpha1.GatewaySpec{Listeners: []v1alpha1.Listener{
		{Name: "a", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 8080},
		{Name: "b", Protocol: v1alpha1.ListenerProtocolHTTP, Port: 8080},
	}}}
	rt := &v1alpha1.Route{Spec: v1alpha1.RouteSpec{
		Listeners: []string{"c"},
		Match:     v1alpha1.RouteMatch{Path: &v1alpha1.PathMatch{Prefix: "/x"}},
	}}
	env := &v1alpha1.Environment{Spec: v1alpha1.EnvironmentSpec{Promotion: &v1alpha1.Promotion{From: "missing"}}}
	b := hub.NewBundle([]*hub.Resource{
		typedResource(v1alpha1.KindGateway, "edge", gw, 1),
		typedResource(v1alpha1.KindRoute, "r", rt, 2),
		typedResource(v1alpha1.KindEnvironment, "prod", env, 3),
		typedResource(v1alpha1.KindPolicy, "odd", "not a Policy", 4),
	})
	_, ds := stageH(t, newValidator(t, nil), b, nil, 2)
	want := "f.yaml:1:1 error RZ-CFG-005 Gateway/edge spec.listeners[name=b].port: listener \"b\" uses port 8080, already used by listener \"a\" (give each listener its own port) (first used in f.yaml:1:1)\n" +
		"f.yaml:2:1 error RZ-CFG-009 Route/r spec.listeners[item=c]: Gateway listener \"c\" not found (did you mean \"a\"?)\n"
	if got := diagText(t, ds); got != want {
		t.Errorf("findings:\n%s want:\n%s", got, want)
	}
}

// TestRegistryUsed: the filterClass rule reads the Registry the options
// pass, so every binary's registry decides alike.
func TestRegistryUsed(t *testing.T) {
	v := newValidator(t, func(o *Options) { o.Registry = registry.New() })
	b, files := loadDirs(t, baseDir, "testdata/fixtures/005-filter-class")
	_, ds := stageH(t, v, b, files, 1)
	if len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Path.String() != "spec.filterClass" {
		t.Errorf("findings:\n%s", diagText(t, ds))
	}
}

// TestRingHashLeftToStageF: ring-hash without hashKey is the schema's
// requiredIf rule, reported once at stage F, so stage H on a Bundle that
// skipped stage F reports nothing for it.
func TestRingHashLeftToStageF(t *testing.T) {
	ring := v1alpha1.LoadBalancingRingHash
	up := &v1alpha1.Upstream{Spec: v1alpha1.UpstreamSpec{
		Protocol:      v1alpha1.UpstreamProtocolHTTP,
		Endpoints:     []v1alpha1.Endpoint{{Address: "orders.svc:80"}},
		LoadBalancing: &v1alpha1.LoadBalancing{Algorithm: &ring},
	}}
	b := hub.NewBundle([]*hub.Resource{typedResource(v1alpha1.KindUpstream, "orders", up, 1)})
	if _, ds := stageH(t, newValidator(t, nil), b, nil, 1); len(ds) > 0 {
		t.Errorf("stage H reports ring-hash without hashKey, which stage F reports:\n%s", diagText(t, ds))
	}
}
