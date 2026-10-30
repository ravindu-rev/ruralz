// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// consumerDoc is the Consumer acme with one API key hash.
const consumerDoc = "apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  credentials:\n    apiKeys:\n" +
	"      - name: k1\n        hash: \"sha256:abababababababababababababababababababababababababababababababab\"\n"

// shopBundle is a Revision with a Gateway-scope Policy, two Routes
// sharing a Route-scope Policy, a ratelimit Policy on one Route, an
// Upstream-scope Policy, a Consumer, and a Policy and an Upstream no Route
// reaches.
func shopBundle() []string {
	return []string{
		gateway(attach("gw-cors")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("up-hdr")),
		upstreamDoc("idle", "127.0.0.1:9002", attach("idle-hdr")),
		routeDoc("a", "/a", "orders", attach("hdr", "rl")),
		routeDoc("b", "/b", "orders", attach("hdr")),
		policyDoc("gw-cors", "cors", corsConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("up-hdr", "headers", headersConfig, ""),
		policyDoc("idle-hdr", "headers", headersConfig, ""),
		policyDoc("unused", "headers", headersConfig, ""),
		consumerDoc,
	}
}

// TestNewRefusesMissingDeps covers New: each required dependency left
// out, and a negative worker count, is ErrDeps naming it; zero Workers
// takes the pipeline's, and nil Clock, Logger and LookupEnv take the real
// clock, a discarding logger and the process environment.
func TestNewRefusesMissingDeps(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		edit func(*Deps)
	}{
		{"Pipeline", func(d *Deps) { d.Pipeline = nil }},
		{"CEL", func(d *Deps) { d.CEL = nil }},
		{"Filters", func(d *Deps) { d.Filters = nil }},
		{"Secrets", func(d *Deps) { d.Secrets = nil }},
		{"StateStores", func(d *Deps) { d.StateStores = nil }},
		{"Upstreams", func(d *Deps) { d.Upstreams = nil }},
		{"Composition", func(d *Deps) { d.Composition = nil }},
		{"Meter", func(d *Deps) { d.Meter = nil }},
		{"Workers", func(d *Deps) { d.Workers = -1 }},
	} {
		d := h.deps
		tc.edit(&d)
		c, err := New(d)
		if c != nil || !errors.Is(err, ErrDeps) || !strings.Contains(err.Error(), tc.name) {
			t.Errorf("New with a bad %s = %v, %v; want ErrDeps naming it", tc.name, c, err)
		}
	}
	d := h.deps
	d.Workers, d.Clock, d.Logger, d.LookupEnv, d.Status, d.Replayer, d.Rand = 0, nil, nil, nil, nil, nil, nil
	c, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	if c.workers != h.tc.pipe.Workers() || c.clk == nil || c.log == nil || c.lookupEnv == nil {
		t.Errorf("defaults: workers %d, want the pipeline's %d; clock %v, logger %v, LookupEnv set %v",
			c.workers, h.tc.pipe.Workers(), c.clk, c.log, c.lookupEnv != nil)
	}
}

// TestCompileRefusesInvalidInput covers ErrInput: an Input without a
// Revision, without a Bundle or whose Revision holds no Gateway is a
// programming error, refused before anything opens.
func TestCompileRefusesInvalidInput(t *testing.T) {
	h := newHarness(t)
	for _, tc := range []struct {
		name string
		in   Input
	}{
		{"no Revision", Input{}},
		{"no Bundle", Input{Validated: &hub.Validated{}}},
		{"no Gateway", Input{Validated: &hub.Validated{Bundle: hub.NewBundle(nil)}}},
	} {
		c, ds, err := h.comp.Compile(t.Context(), tc.in)
		if c != nil || ds != nil || !errors.Is(err, ErrInput) {
			t.Errorf("%s: Compile = %v, %v, %v; want ErrInput", tc.name, c, ds, err)
		}
	}
	if n := h.stores.calls(); n != 0 {
		t.Errorf("Acquire ran %d times for invalid input", n)
	}
}

// TestCompileResult covers the snapshot a compile returns: the Revision it
// came from, the Consumers, the CEL program set, the admission plan, the
// Upstream layer as Legs and first Resource followed by each built Filter
// holding resources once, the State Store handles, one shared by both
// roles without stateStore.cache, and the Compiled's secret table,
// Upstream set and connection total; Binding and Pins stay the
// activation's.
func TestCompileResult(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeCORS, "gw-cors", behavior{plain: true})
	res := h.validate(shopBundle()...)
	c := h.mustCompile(res, nil)
	s := c.Snapshot
	if s.Validated != res.Validated || s.Revision.Digest != res.Validated.Revision.Digest {
		t.Errorf("snapshot Revision %s, Validated %p; want %s, %p", s.Revision.Digest, s.Validated, res.Validated.Revision.Digest, res.Validated)
	}
	if s.Consumers["acme"] == nil || len(s.Consumers) != 1 {
		t.Errorf("Consumers = %v, want acme", s.Consumers)
	}
	if s.Programs == nil || s.Metrics == nil {
		t.Errorf("Programs %v, Metrics %v; want both", s.Programs, s.Metrics)
	}
	if s.Legs != snapshot.LegRunner(c.Upstreams) || c.Upstreams == nil {
		t.Errorf("Legs = %v, want the Upstream set %v", s.Legs, c.Upstreams)
	}
	if s.StateStore == nil || s.CacheStore != s.StateStore {
		t.Errorf("StateStore %v, CacheStore %v; want one shared handle", s.StateStore, s.CacheStore)
	}
	if s.Binding != nil || s.Pins != nil {
		t.Errorf("Binding %v, Pins %v; want the activation's", s.Binding, s.Pins)
	}
	if c.Secrets == nil || c.UpstreamConnections != 1024 || len(c.Warnings) != 0 {
		t.Errorf("Secrets %v, UpstreamConnections %d, Warnings %v; want a table, 1024, none", c.Secrets, c.UpstreamConnections, c.Warnings)
	}
	// Resources: the Upstream set, then hdr, rl and up-hdr, each once
	// though hdr serves two Routes; gw-cors holds none.
	var names []string
	for i, r := range s.Resources {
		if i == 0 {
			if r != c.Upstreams {
				t.Errorf("Resources[0] = %v, want the Upstream set", r)
			}
			continue
		}
		f := fakeOf(r.(filter.Filter))
		if f == nil {
			t.Fatalf("Resources[%d] = %T, want a built Filter", i, r)
		}
		names = append(names, f.policy)
	}
	if want := []string{"hdr", "rl", "up-hdr"}; !slices.Equal(names, want) {
		t.Errorf("Resources Filters = %v, want %v", names, want)
	}
	if got := h.runtimes(); !slices.Equal(got, []string{"orders"}) {
		t.Errorf("Upstream runtimes = %v, want [orders]", got)
	}
}

// TestActivate covers Activate: the secret resolver switches to the
// snapshot's table and the Upstream runtimes go live, once; a later
// Activate, and a Discard after it, change nothing, as the retirer then
// owns the snapshot's resources.
func TestActivate(t *testing.T) {
	h := newHarness(t)
	c := h.mustCompile(h.validate(shopBundle()...), nil)
	if h.res.Current() == c.Secrets {
		t.Fatal("the resolver holds the snapshot's table before Activate")
	}
	for _, st := range h.ups.Status() {
		if st.Live {
			t.Errorf("runtime %s is live before Activate", st.Name)
		}
	}
	c.Activate()
	c.Activate()
	if h.res.Current() != c.Secrets {
		t.Error("Activate did not switch the resolver to the snapshot's table")
	}
	c.Discard()
	for _, st := range h.ups.Status() {
		if !st.Live {
			t.Errorf("runtime %s is not live after Activate", st.Name)
		}
	}
	for _, p := range c.Snapshot.Routes[0].Chain.Policies {
		if f := fakeOf(p.Filter); f != nil && f.closes.Load() != 0 {
			t.Errorf("Discard after Activate closed the Filter of %s", p.Name)
		}
	}
	if h.released() {
		t.Error("Discard after Activate released the State Store handles")
	}
}

// TestDiscard covers Discard: every Filter the compile built is closed
// once, one Filter serving two attachments and a Filter that cannot be
// compared included; a Filter whose Close panics is logged and the rest
// still close; the Upstream set closes, dropping its runtimes, and the
// State Store handles are released. A second Discard, and an Activate
// after it, do nothing.
func TestDiscard(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeHeaders, "hdr", behavior{share: true})
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{closePanics: true})
	h.behave(v1alpha1.PolicyTypeHeaders, "up-only", behavior{unhashable: true})
	res := h.validate(
		gateway(attach("gw-cors")),
		upstreamDoc("orders", "127.0.0.1:9001", attach("hdr", "up-only")),
		routeDoc("a", "/a", "orders", attach("hdr", "rl")),
		policyDoc("gw-cors", "cors", corsConfig, ""),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("up-only", "headers", headersConfig, ""),
	)
	c, ds, err := h.compile(res, nil)
	if err != nil || len(ds) > 0 {
		t.Fatalf("Compile = %v\n%s", err, text(ds))
	}
	if n := len(h.buildsOf("hdr")); n != 2 {
		t.Fatalf("hdr built %d times, want once per attachment, 2", n)
	}
	var slice sliceFilter
	for _, ps := range c.Snapshot.LegChains["orders"].Phases {
		for _, p := range ps {
			if s, ok := p.Filter.(sliceFilter); ok {
				slice = s
			}
		}
	}
	if slice.closes == nil {
		t.Fatal("no Filter that cannot be compared in the leg chain")
	}
	c.Discard()
	c.Discard()
	c.Activate()
	assertAllClosed(t, h)
	if n := len(h.built()); n != 3 {
		t.Errorf("%d fake Filters built, want gw-cors, hdr once and rl", n)
	}
	if n := slice.closes.Load(); n != 1 {
		t.Errorf("Filter that cannot be compared closed %d times, want 1", n)
	}
	if !slices.Contains(h.logs.messages(), "a Filter of a discarded snapshot panicked while closing") {
		t.Errorf("logs = %q, want the Close panic", h.logs.messages())
	}
	if got := h.runtimes(); len(got) != 0 {
		t.Errorf("Upstream runtimes after Discard = %v, want none", got)
	}
	if !h.released() {
		t.Error("Discard kept the State Store handles")
	}
	if h.res.Current() == c.Secrets {
		t.Error("Activate after Discard switched the resolver")
	}
}

// TestRejectedCompileReleases covers the undo of a rejected compile: when
// a Factory refuses one Policy, the Filters built for the others, the
// Upstream set and the State Store handles are all released, and the
// findings come back alone.
func TestRejectedCompileReleases(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{err: errcode.Errorf("RZ-CFG-005", "rl is refused")})
	got := h.rejects(h.validate(shopBundle()...))
	if len(got) != 1 || !strings.Contains(got[0], "error RZ-CFG-005 Policy/rl spec.config: rl is refused") {
		t.Errorf("findings = %q, want the refusal of rl", got)
	}
	if n := len(h.built()); n != 3 {
		t.Fatalf("%d Filters built, want gw-cors, hdr and up-hdr", n)
	}
	assertAllClosed(t, h)
	if got := h.runtimes(); len(got) != 0 {
		t.Errorf("Upstream runtimes after a rejected compile = %v, want none", got)
	}
	if !h.released() {
		t.Error("a rejected compile kept the State Store handles")
	}
}

// TestCanceledCompileReleases covers the compile deadline: a compile
// whose context ends while a Factory builds returns the context's error
// and no findings, starts no further Build, and releases what it opened,
// the Filters already built included.
func TestCanceledCompileReleases(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Workers = 1 })
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{block: true})
	res := h.validate(shopBundle()...)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	type outcome struct {
		c   *Compiled
		n   int
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		c, ds, err := h.comp.Compile(ctx, Input{Validated: res.Validated, Files: res.Files})
		done <- outcome{c, len(ds), err}
	}()
	for len(h.buildsOf("rl")) == 0 {
		yield()
	}
	cancel()
	o := <-done
	if o.c != nil || o.n != 0 || !errors.Is(o.err, context.Canceled) {
		t.Fatalf("Compile = %v, %d findings, %v; want context.Canceled", o.c, o.n, o.err)
	}
	if n := len(h.buildsOf("up-hdr")); n != 0 {
		t.Errorf("up-hdr built %d times after the context ended", n)
	}
	if n := len(h.built()); n != 2 {
		t.Errorf("%d Filters built, want gw-cors and hdr", n)
	}
	assertAllClosed(t, h)
	if !h.released() {
		t.Error("a canceled compile kept the State Store handles")
	}
}

// TestBuildPanicReleases covers a Factory that panics: the panic reaches
// the caller of Compile on its goroutine, wrapping the panic value, after
// the compile released the Filters already built and the State Store
// handles.
func TestBuildPanicReleases(t *testing.T) {
	h := newHarness(t, func(d *Deps) { d.Workers = 1 })
	boom := errors.New("boom")
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{panic: boom})
	res := h.validate(shopBundle()...)
	v := func() (v any) {
		defer func() { v = recover() }()
		_, _, _ = h.compile(res, nil)
		return nil
	}()
	err, ok := v.(error)
	if !ok || !errors.Is(err, boom) {
		t.Fatalf("Compile panicked with %v, want an error wrapping %v", v, boom)
	}
	var up *unitPanic
	if !errors.As(err, &up) || !strings.Contains(up.Error(), "boom") {
		t.Errorf("panic value %T, want a *unitPanic naming the value", v)
	}
	if n := len(h.built()); n != 2 {
		t.Errorf("%d Filters built, want gw-cors and hdr", n)
	}
	assertAllClosed(t, h)
	if !h.released() {
		t.Error("a compile that panicked kept the State Store handles")
	}
}

// assertAllClosed fails the test unless every fake Filter the Factories
// built was closed exactly once.
func assertAllClosed(t *testing.T, h *harness) {
	t.Helper()
	for _, f := range h.built() {
		if n := f.closes.Load(); n != 1 {
			t.Errorf("Filter of %s at %s closed %d times, want 1", f.policy, f.scope, n)
		}
	}
}

// TestCarryOver covers BuildEnv.Previous and the carried Filters: a
// Policy whose name, type, scope, Phase set and canonical config are those
// of a Filter of the previous snapshot gets that Filter, which a Factory
// may return again; a changed config, a moved scope and a new Policy get
// none. The new compile builds its CEL programs over the previous set, and
// its Discard closes the Filters it built but never one the previous
// snapshot holds.
func TestCarryOver(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeHeaders, "hdr", behavior{reuse: true})
	prev := h.mustCompile(h.validate(
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("a", "/a", "orders", attach("hdr", "rl", "moved")),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
		policyDoc("moved", "headers", `{"request":{"set":[{"name":"x-moved","value":"1"}]}}`, ""),
	), nil)
	old := map[string]filter.Filter{}
	for _, p := range prev.Snapshot.Routes[0].Chain.Policies {
		old[p.Name] = p.Filter
	}
	next := h.mustCompile(h.validate(
		gateway(attach("moved")),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("a", "/a", "orders", attach("hdr", "rl", "fresh")),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", `{"limits":[{"requests":200,"window":"1s"}]}`, ""),
		policyDoc("moved", "headers", `{"request":{"set":[{"name":"x-moved","value":"1"}]}}`, ""),
		policyDoc("fresh", "headers", headersConfig, ""),
	), prev.Snapshot)
	last := func(name string) filter.BuildEnv {
		envs := h.buildsOf(name)
		return envs[len(envs)-1]
	}
	if got := last("hdr").Previous; got != old["hdr"] || got == nil {
		t.Errorf("hdr Previous = %v, want the previous Filter %v", got, old["hdr"])
	}
	for _, name := range []string{"rl", "moved", "fresh"} {
		if got := last(name).Previous; got != nil {
			t.Errorf("%s Previous = %v, want none", name, got)
		}
	}
	var shared filter.Filter
	for _, p := range next.Snapshot.Routes[0].Chain.Policies {
		if p.Name == "hdr" {
			shared = p.Filter
		}
	}
	if shared != old["hdr"] {
		t.Errorf("hdr Filter = %v, want the carried %v", shared, old["hdr"])
	}
	if _, prevSet := h.cel.lastCaps(t); prevSet != prev.Snapshot.Programs {
		t.Errorf("Builder previous programs = %v, want the previous snapshot's %v", prevSet, prev.Snapshot.Programs)
	}
	next.Discard()
	if n := fakeOf(old["hdr"]).closes.Load(); n != 0 {
		t.Errorf("Discard closed the carried Filter %d times", n)
	}
	for _, p := range next.Snapshot.Routes[0].Chain.Policies {
		if p.Name != "hdr" && fakeOf(p.Filter).closes.Load() != 1 {
			t.Errorf("Discard did not close the new Filter of %s", p.Name)
		}
	}
	for name, f := range old {
		if name != "hdr" && fakeOf(f).closes.Load() != 0 {
			t.Errorf("Discard closed the previous snapshot's Filter of %s", name)
		}
	}
}

// TestCarryOverRejectedKeepsShared covers a rejected compile over a
// previous snapshot: its undo never closes a Filter the previous snapshot
// holds.
func TestCarryOverRejectedKeepsShared(t *testing.T) {
	h := newHarness(t)
	h.behave(v1alpha1.PolicyTypeHeaders, "hdr", behavior{reuse: true})
	docs := []string{
		gateway(""),
		upstreamDoc("orders", "127.0.0.1:9001", ""),
		routeDoc("a", "/a", "orders", attach("hdr", "rl")),
		policyDoc("hdr", "headers", headersConfig, ""),
		policyDoc("rl", "ratelimit", rateLimitConfig, ""),
	}
	prev := h.mustCompile(h.validate(docs...), nil)
	carried := fakeOf(prev.Snapshot.Routes[0].Chain.Policies[1].Filter)
	if carried == nil || carried.policy != "hdr" {
		t.Fatalf("previous chain = %+v", prev.Snapshot.Routes[0].Chain.Policies)
	}
	h.behave(v1alpha1.PolicyTypeRateLimit, "rl", behavior{err: errcode.Errorf("RZ-CFG-005", "refused")})
	if c, ds, err := h.compile(h.validate(docs...), prev.Snapshot); c != nil || err != nil || len(ds) != 1 {
		t.Fatalf("Compile = %v, %v, %s; want one finding", c, err, text(ds))
	}
	if n := carried.closes.Load(); n != 0 {
		t.Errorf("the rejected compile closed the carried Filter %d times", n)
	}
}
