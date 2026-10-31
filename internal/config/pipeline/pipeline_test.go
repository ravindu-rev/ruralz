// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestNewRefusesIncompleteWiring covers New's ErrOptions: the CEL compiler
// and the Policy checks are injected by every binary and required, a nil
// check is refused, and no worker count, yield time or limit is negative.
func TestNewRefusesIncompleteWiring(t *testing.T) {
	b, err := shared()
	if err != nil {
		t.Fatal(err)
	}
	full := func() Options {
		return Options{Schemas: b.schemas, Views: b.views, CEL: b.cel, Checks: hub.Checks{}}
	}
	tests := []struct {
		name string
		edit func(*Options)
	}{
		{"no CEL compiler", func(o *Options) { o.CEL = nil }},
		{"no checks", func(o *Options) { o.Checks = nil }},
		{"a nil check", func(o *Options) { o.Checks = hub.Checks{v1alpha1.PolicyTypeHeaders: nil} }},
		{"negative workers", func(o *Options) { o.Workers = -1 }},
		{"negative yield time", func(o *Options) { o.YieldAfter = -time.Microsecond }},
		{"negative limit", func(o *Options) { o.Limits.MaxFiles = -1 }},
		{"negative diagnostics cap", func(o *Options) { o.Limits.MaxDiagnostics = -1 }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o := full()
			tc.edit(&o)
			if p, err := New(o); !errors.Is(err, ErrOptions) {
				t.Errorf("New() = %v, %v; want ErrOptions", p, err)
			}
		})
	}
	if _, err := New(full()); err != nil {
		t.Errorf("New(complete options) = %v", err)
	}
}

// TestNewDefaults covers the defaults New fills in: half the processors
// as workers, as a Node's activation runs, the loader's limits with the
// 10,000-diagnostic cap, the embedded schemas and the Policy type registry
// when the options leave them nil, and the shared components the CLI
// reads.
func TestNewDefaults(t *testing.T) {
	b, err := shared()
	if err != nil {
		t.Fatal(err)
	}
	p, err := New(Options{CEL: b.cel, Checks: hub.Checks{}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if want := max(1, runtime.GOMAXPROCS(0)/2); p.Workers() != want {
		t.Errorf("Workers() = %d, want %d", p.Workers(), want)
	}
	if l := p.Limits(); l.MaxDiagnostics != loader.DefaultMaxDiagnostics || l.MaxSourceBytes != loader.DefaultMaxSourceBytes ||
		l.MaxResources != loader.DefaultMaxResources {
		t.Errorf("Limits() = %+v, want the loader's defaults", l)
	}
	if p.Schemas() == nil || p.HubIndex() == nil || p.Registry() == nil || p.Resolver() == nil || p.Decoder() == nil {
		t.Error("an accessor returned nil")
	}
	if _, ok := p.Registry().Lookup(v1alpha1.PolicyTypeRateLimit); !ok {
		t.Error("the default registry does not know ratelimit")
	}
	if p.HubIndex().APIVersion() != v1alpha1.APIVersion {
		t.Errorf("HubIndex().APIVersion() = %q", p.HubIndex().APIVersion())
	}
	if p.yieldAfter != loader.DefaultYieldAfter {
		t.Errorf("yieldAfter = %v, want %v", p.yieldAfter, loader.DefaultYieldAfter)
	}
	q := newPipeline(t, Options{Workers: 3, Limits: loader.Limits{MaxDiagnostics: 7}})
	if q.Workers() != 3 || q.Limits().MaxDiagnostics != 7 {
		t.Errorf("Workers %d, MaxDiagnostics %d; want the options' 3 and 7", q.Workers(), q.Limits().MaxDiagnostics)
	}
}

// TestChecksRunAtStageH covers the injected Policy checks: the facade
// hands Options.Checks to stage H, which runs the check of a Policy's type
// on it and reports its findings in the run.
func TestChecksRunAtStageH(t *testing.T) {
	check := func(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
		p := diag.Path{diag.Field("spec"), diag.Field("config")}
		add(diag.Diagnostic{Code: "RZ-CFG-037", Severity: diag.SeverityError, Location: loc(p), Path: p, Message: "checked " + r.Name})
	}
	p := newPipeline(t, Options{Checks: hub.Checks{v1alpha1.PolicyTypeHeaders: check}})
	res := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"p.yaml":      policyDoc("h", "headers", "  config:\n    response:\n      set: [{name: x-a, value: b}]\n"),
	}), Request{})
	wantTexts(t, "run", res.Diagnostics, "p.yaml:8:5 error RZ-CFG-037 Policy/h spec.config: checked h")
	if res.Validated != nil {
		t.Error("a run with a check's error has a Revision")
	}
}
