// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/identity"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// t0 is the fake time of every harness.
var t0 = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

// policyName is the name of the Policy under test.
const policyName = "p"

// toolchain is the configuration pipeline and the CEL compiler every
// binary builds: the pipeline takes checks.Table(). Compiling the
// embedded schema views takes a while, so the tests of the package share
// one.
type toolchain struct {
	pipe *pipeline.Pipeline
	cel  *cel.Compiler
}

var sharedToolchain = sync.OnceValues(func() (*toolchain, error) {
	c, err := cel.New()
	if err != nil {
		return nil, err
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: c, Workers: 1})
	if err != nil {
		return nil, err
	}
	return &toolchain{pipe: p, cel: c}, nil
})

// harness validates one Policy config through the configuration pipeline
// and builds it with the Factory table, as a Node's activation does: the
// pipeline's stages A to M, then the snapshot compiler's Build of each
// attachment some Route's effective chain runs. Its Components run on a
// fake clock until the test ends.
type harness struct {
	*toolchain
	reg     *registry.Registry
	filters *filter.Registry
	clk     *clocktest.Fake
	ca      *pki.CA
	crl     []byte
}

// newHarness returns a harness whose Components run until tb ends.
func newHarness(tb testing.TB) *harness {
	tb.Helper()
	tc, err := sharedToolchain()
	if err != nil {
		tb.Fatal(err)
	}
	clk := clocktest.New(t0)
	filters, err := New(Options{Clock: clk, Entropy: zeros{}, Rand: rand.NewPCG(1, 2)})
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(tb.Context())
	var wg sync.WaitGroup
	for _, c := range filters.Components() {
		wg.Go(func() {
			if err := c.Run(ctx); err != nil {
				tb.Errorf("component %s: %v", c.Name(), err)
			}
		})
	}
	tb.Cleanup(func() { cancel(); wg.Wait() })
	ca, err := pki.NewCA(pki.CAOptions{NotBefore: t0.Add(-24 * time.Hour), NotAfter: t0.Add(365 * 24 * time.Hour)})
	if err != nil {
		tb.Fatal(err)
	}
	crl, err := ca.CRL(nil, t0.Add(-time.Hour), t0.Add(24*time.Hour))
	if err != nil {
		tb.Fatal(err)
	}
	return &harness{toolchain: tc, reg: registry.New(), filters: filters, clk: clk, ca: ca, crl: crl}
}

// zeros is an io.Reader of zero bytes: a fixed throttle key.
type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// outcome is what validation and the Factory say about one config.
type outcome struct {
	// findings are the error diagnostics validation reports about the
	// Policy under test, from any stage.
	findings diag.List
	// all are every diagnostic of the run.
	all diag.List
	// validated reports a Bundle without errors: it has a Revision, and a
	// Node builds every attachment its effective chains run.
	validated bool
	// builds are the Build results, one per attachment of a validated
	// Bundle, or one for the config decoded without validation.
	builds []build
}

// build is one Build of the Policy under test.
type build struct {
	scope phase.Scope
	err   error
}

// attachScope returns the scope the harness attaches typ at: Route when
// the type may be attached there, else Upstream.
func (h *harness) attachScope(typ v1alpha1.PolicyType) phase.Scope {
	e, ok := h.reg.Lookup(typ)
	if !ok || e.Scopes.Has(phase.ScopeRoute) {
		return phase.ScopeRoute
	}
	return phase.ScopeUpstream
}

// bundle returns a Bundle with one Gateway, Upstream, Consumer and Route
// and the Policy under test of type typ whose config is the JSON text
// config, attached at scope (Route or Upstream). The Consumer defines the
// requests quota "daily" for quota Policies.
func bundle(typ v1alpha1.PolicyType, config string, scope phase.Scope) fstest.MapFS {
	routePolicies, upstreamPolicies := "  policies:\n    - name: "+policyName+"\n", ""
	if scope == phase.ScopeUpstream {
		routePolicies, upstreamPolicies = "", "  policies:\n    - name: "+policyName+"\n"
	}
	root := `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: up
spec:
  protocol: http
  endpoints:
    - address: "127.0.0.1:9000"
` + upstreamPolicies + `---
apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: acme
spec:
  credentials:
    apiKeys:
      - name: k1
        hash: "sha256:` + strings.Repeat("ab", 32) + `"
  quotas:
    - name: daily
      unit: requests
      limit: 1000
      window: 24h
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: r
spec:
  match:
    path:
      prefix: /
  upstreams:
    - name: up
` + routePolicies
	pol := `{"apiVersion":"ruralz/v1alpha1","kind":"Policy","metadata":{"name":"` + policyName + `"},` +
		`"spec":{"type":"` + string(typ) + `","config":` + config + `}}`
	return fstest.MapFS{
		"ruralz.yaml": {Data: []byte(root)},
		"policy.json": {Data: []byte(pol)},
	}
}

// run validates config as the Policy of type typ, then builds it: every
// attachment of a validated Bundle, as the snapshot compiler would, or,
// when validation fails, the config decoded alone at the harness's scope,
// so a test sees whether Build refuses what validation refused.
func (h *harness) run(t *testing.T, typ v1alpha1.PolicyType, config string) outcome {
	t.Helper()
	scope := h.attachScope(typ)
	res, err := h.pipe.Run(t.Context(), pipeline.Request{Source: loader.Source{FS: bundle(typ, config, scope), Path: "."}})
	if err != nil {
		t.Fatalf("pipeline: %v", err)
	}
	out := outcome{all: res.Diagnostics, validated: res.Validated != nil}
	for _, d := range res.Diagnostics {
		if d.Severity == diag.SeverityError && d.Resource != nil &&
			d.Resource.Kind == string(v1alpha1.KindPolicy) && d.Resource.Name == policyName {
			out.findings = append(out.findings, d)
		}
	}
	if out.validated {
		out.builds = h.buildValidated(t, res.Validated)
		return out
	}
	if b, ok := h.buildDecoded(t, typ, config, scope); ok {
		out.builds = []build{b}
	}
	return out
}

// buildValidated builds the Policy under test at each of its attachments
// in v's effective chains, closing every Filter it gets.
func (h *harness) buildValidated(t *testing.T, v *hub.Validated) []build {
	t.Helper()
	r, ok := v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: policyName})
	if !ok {
		t.Fatal("the validated Bundle lost the Policy")
	}
	pol, ok := hub.Object[v1alpha1.Policy](r)
	if !ok {
		t.Fatalf("Policy object is %T", r.Object)
	}
	consumers := identity.CompileBundle(v.Bundle).Map()
	secrets := h.secrets(v.Secrets)
	type attachment struct {
		scope  phase.Scope
		phases phase.Set
	}
	var as []attachment
	seen := map[phase.Scope]bool{}
	add := func(e hub.Entry) {
		if e.Policy == policyName && !seen[e.Scope] {
			seen[e.Scope] = true
			as = append(as, attachment{e.Scope, e.Phases})
		}
	}
	for _, c := range v.Chains {
		for p := range phase.Count {
			for _, e := range c.Client[p] {
				add(e)
			}
		}
		for _, l := range c.Legs {
			for p := range phase.Count {
				for _, e := range l.Phases[p] {
					add(e)
				}
			}
		}
	}
	var out []build
	for _, a := range as {
		env := h.env(r, pol, r.Config, a.scope, a.phases, v.Bundle, consumers, secrets)
		out = append(out, build{scope: a.scope, err: h.build(t, env)})
	}
	return out
}

// buildDecoded decodes config as typ's typed config, as stage G would,
// and builds it at scope without a validated Bundle; false when the text
// does not decode, which no Node ever builds.
func (h *harness) buildDecoded(t *testing.T, typ v1alpha1.PolicyType, config string, scope phase.Scope) (build, bool) {
	t.Helper()
	cfg, err := h.reg.NewConfig(typ)
	if err != nil || cfg == nil {
		return build{}, false
	}
	if err := json.Unmarshal([]byte(config), cfg); err != nil {
		return build{}, false
	}
	pol := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: policyName}, Spec: v1alpha1.PolicySpec{Type: typ}}
	h.reg.Materialize(pol)
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policyName}, Object: pol, Config: cfg}
	phases, err := h.reg.Phases(registry.Attachment{Policy: pol, Config: cfg, Scope: scope}, h.cel)
	if err != nil {
		e, _ := h.reg.Lookup(typ)
		phases = e.Phases
		if scope == phase.ScopeUpstream {
			phases = e.UpstreamPhases
		}
	}
	env := h.env(r, pol, cfg, scope, phases, nil, nil, mapStore{})
	return build{scope: scope, err: h.build(t, env)}, true
}

// env returns the BuildEnv of the Policy resource r at one attachment.
func (h *harness) env(r *hub.Resource, pol *v1alpha1.Policy, cfg any, scope phase.Scope, phases phase.Set,
	b *hub.Bundle, consumers map[string]*expr.Consumer, secrets secret.Store,
) filter.BuildEnv {
	cs := make([]*expr.Consumer, 0, len(consumers))
	for _, c := range consumers {
		cs = append(cs, c)
	}
	return filter.BuildEnv{
		Policy: pol, Config: cfg, Scope: scope, Phases: phases,
		Bundle: b, Consumers: consumers,
		CEL: h.cel.NewBuilder(nil, celtest.Caps(nil, cs...)),
		Site: func(place expr.PlaceID) expr.Site {
			return h.pipe.PolicySite(r, place, nil, scope, phases)
		},
		Secrets:    secrets,
		StateStore: &filtertest.Store{}, Enqueuer: enqueuer{},
		StateStoreTimeout: 50 * time.Millisecond,
		Limits:            filter.Limits{MaxRequestBodyBytes: 10 << 20, MaxResponseBodyBytes: 10 << 20, MaxBufferedBytes: 512 << 20},
		Clock:             h.clk,
		Logger:            slog.New(slog.DiscardHandler),
		Shared:            &memo{},
	}
}

// build runs the Factory of env's Policy type and closes the Filter it
// returns.
func (h *harness) build(t *testing.T, env filter.BuildEnv) error {
	t.Helper()
	f, ok := h.filters.Factory(env.Policy.Spec.Type)
	if !ok {
		t.Fatalf("no Factory for %s", env.Policy.Spec.Type)
	}
	flt, err := f.Build(t.Context(), env)
	if err != nil {
		if flt != nil {
			t.Errorf("Build returned a Filter beside error %v", err)
		}
		return err
	}
	if flt == nil {
		t.Fatal("Build returned neither a Filter nor an error")
	}
	if c, ok := flt.(filter.Closer); ok {
		if err := c.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}
	return nil
}

// secrets resolves every secretRef use with a value of the kind its use
// checks: the harness CA for a CA bundle, its CRL for a CRL, an opaque
// value otherwise, as a Node's resolver would for a Bundle whose secrets
// exist.
func (h *harness) secrets(uses []hub.SecretUse) mapStore {
	m := mapStore{}
	for _, u := range uses {
		r := secret.RefOf(u.Ref)
		if _, ok := m[r]; ok {
			continue
		}
		var v []byte
		switch u.Kind {
		case secret.KindPEMCertPool, secret.KindPEMCertificate:
			v = h.ca.CertPEM
		case secret.KindPEMPrivateKey:
			v = h.ca.KeyPEM
		case secret.KindPEMCRL:
			v = h.crl
		case secret.KindAPIKey:
			v = []byte(strings.Repeat("k", 32))
		case secret.KindStateStoreURL:
			v = []byte("redis://127.0.0.1:6379")
		default:
			v = []byte("opaque-secret-value")
		}
		m[r] = secret.NewValue(v)
	}
	return m
}

// offline reports whether code is one an offline stage raises
// (docs/architecture/02-configuration-model.md, Figure 3): a Build refusal
// with such a code must be a validation finding. RZ-CFG-024, -026, -027,
// -028, -033, -039, -040, -042, -044 and -045 follow the digest.
func offline(code string) bool {
	switch code {
	case "RZ-CFG-024", "RZ-CFG-026", "RZ-CFG-027", "RZ-CFG-028", "RZ-CFG-033",
		"RZ-CFG-039", "RZ-CFG-040", "RZ-CFG-042", "RZ-CFG-044", "RZ-CFG-045":
		return false
	}
	return strings.HasPrefix(code, "RZ-CFG-")
}

// codeOf returns err's RZ code, or "" without one.
func codeOf(err error) string {
	c, _ := errcode.CodeOf(err)
	return c
}

// mapStore is a resolved secret table.
type mapStore map[secret.Ref]secret.Value

var _ secret.Store = mapStore{}

// Get implements secret.Store.
func (m mapStore) Get(r secret.Ref) (secret.Value, bool) {
	v, ok := m[r]
	return v, ok
}

// Watch implements secret.Store; values never rotate.
func (mapStore) Watch(secret.Ref, func(secret.Value) error) func() { return func() {} }

// enqueuer accepts every post-commit write.
type enqueuer struct{}

// Enqueue implements statestore.Enqueuer.
func (enqueuer) Enqueue(statestore.WriteClass, *statestore.Write) bool { return true }

// memo is a snapshot compile's filter.Shared.
type memo struct {
	mu sync.Mutex
	m  map[any]any
}

// Get implements filter.Shared: build runs once per key.
func (s *memo) Get(key any, build func() (any, error)) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.m[key]; ok {
		return v, nil
	}
	v, err := build()
	if err != nil {
		return nil, err
	}
	if s.m == nil {
		s.m = map[any]any{}
	}
	s.m[key] = v
	return v, nil
}

// describe renders an outcome for a failure message.
func (o outcome) describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "validated %v", o.validated)
	for _, d := range o.all {
		fmt.Fprintf(&b, "\n  %s", d.AppendText(nil))
	}
	for _, bl := range o.builds {
		fmt.Fprintf(&b, "\n  Build at %s: %v", bl.scope, bl.err)
	}
	return b.String()
}

// errNoBuild marks an outcome without a Build.
var errNoBuild = errors.New("no Build ran")

// firstBuild returns the first Build's error, or errNoBuild.
func (o outcome) firstBuild() error {
	if len(o.builds) == 0 {
		return errNoBuild
	}
	return o.builds[0].err
}
