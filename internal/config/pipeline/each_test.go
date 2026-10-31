// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
)

// environmentDoc returns an Environment document with a spec body.
func environmentDoc(name, spec string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: " + name + "\nspec:" + spec + "\n"
}

// runEach runs p.RunEach on m with the environments file envs.
func runEach(t testing.TB, p *Pipeline, m fstest.MapFS, envs string) *EachResult {
	t.Helper()
	control := fstest.MapFS{"envs.yaml": &fstest.MapFile{Data: []byte(envs)}}
	res, err := p.RunEach(t.Context(), EachRequest{Source: loader.Source{FS: m, Path: "."}, Environments: loader.Source{FS: control, Path: "envs.yaml"}})
	if err != nil {
		t.Fatalf("RunEach: %v", err)
	}
	return res
}

// TestRunEachSharesStagesAToC covers a run per Environment: the findings
// of the environments file and of stages A to C print once, naming no
// Environment; each Environment runs stages D to M, in file order, and
// its findings name it; an overlay applies only to the Environment that
// selects it.
func TestRunEachSharesStagesAToC(t *testing.T) {
	m := bundle(map[string]string{
		"ruralz.yaml":             gatewayDoc,
		"u.yaml":                  docs(upstreamDoc("orders"), upstreamDoc("orders")),
		"h.yaml":                  routeDoc("a", "orders", "") + "---\n" + upstreamDoc("hosted") + "  timeout: ${TIMEOUT}\n",
		"overlays/prod/r.yaml":    "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: a\nspec:\n  timeout: 4s\n",
		"overlays/staging/r.yaml": "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: a\nspec:\n  unknown: 1\n",
	})
	envs := docs(
		environmentDoc("staging", "\n  variables:\n    TIMEOUT: 2s"),
		environmentDoc("prod", "\n  variables:\n    OTHER: x"),
	)
	res := runEach(t, newPipeline(t, Options{}), m, envs)
	wantTexts(t, "shared", res.Shared,
		"u.yaml:13:9 error RZ-CFG-008 Upstream/orders metadata.name: Upstream/orders is declared twice: a resource lives in exactly one place (first declared in u.yaml:4:9)")
	if res.Stopped || !slices.Equal(res.Environments, []string{"staging", "prod"}) || len(res.Runs) != 2 {
		t.Fatalf("Stopped %v, Environments %q, %d runs", res.Stopped, res.Environments, len(res.Runs))
	}
	staging, prod := res.Runs[0], res.Runs[1]
	if staging.Environment != "staging" || staging.Overlay != "staging" || prod.Environment != "prod" || prod.Overlay != "prod" {
		t.Errorf("runs for %s (overlay %s) and %s (overlay %s)", staging.Environment, staging.Overlay, prod.Environment, prod.Overlay)
	}
	wantTexts(t, "staging", staging.Diagnostics,
		`overlays/staging/r.yaml:6:3 error RZ-CFG-006 Route/a spec.unknown: unknown field "unknown" [environment=staging]`)
	wantTexts(t, "prod", prod.Diagnostics,
		`h.yaml:19:12 error RZ-CFG-010 Upstream/hosted spec.timeout: variable "TIMEOUT" is not defined and has no default [environment=prod]`)
	if !res.HasErrors() {
		t.Error("HasErrors() = false")
	}
}

// TestRunEachValidRuns covers clean runs: each Environment reaches its own
// digest with its own variables, and HasErrors is false.
func TestRunEachValidRuns(t *testing.T) {
	m := bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"u.yaml":      upstreamDoc("orders") + "  timeout: ${TIMEOUT}\n",
		"r.yaml":      routeDoc("a", "orders", ""),
	})
	res := runEach(t, newPipeline(t, Options{}), m, docs(
		environmentDoc("zeta", "\n  variables:\n    TIMEOUT: 2s"),
		environmentDoc("alpha", "\n  variables:\n    TIMEOUT: 3s"),
	))
	if res.HasErrors() || len(res.Runs) != 2 || !slices.Equal(res.Environments, []string{"zeta", "alpha"}) {
		t.Fatalf("HasErrors %v, Environments %q, %d runs", res.HasErrors(), res.Environments, len(res.Runs))
	}
	for _, r := range res.Runs {
		wantClean(t, r.Environment, r.Diagnostics)
		if r.Validated == nil {
			t.Fatalf("%s: no Validated", r.Environment)
		}
	}
	if res.Runs[0].Validated.Revision.Digest == res.Runs[1].Validated.Revision.Digest {
		t.Error("two Environments with different variables share a digest")
	}
}

// TestRunEachStoppedBundle covers a Bundle whose stages A to C stop the
// load: no Environment renders, and the stop is shared.
func TestRunEachStoppedBundle(t *testing.T) {
	res := runEach(t, newPipeline(t, Options{}), bundle(map[string]string{"u.yaml": upstreamDoc("orders")}), environmentDoc("prod", " {}"))
	if !res.Stopped || len(res.Runs) != 0 || !slices.Equal(codes(res.Shared), []string{"RZ-CFG-016"}) {
		t.Errorf("Stopped %v, %d runs, shared %q", res.Stopped, len(res.Runs), texts(res.Shared))
	}
}

// TestRunEachWithoutEnvironment covers an environments file that declares
// no Environment, only a Cluster: no run and no finding, which the CLI
// reports as a usage error; and RunEach without an environments file.
func TestRunEachWithoutEnvironment(t *testing.T) {
	p := newPipeline(t, Options{})
	res := runEach(t, p, bundle(validBundle()), "apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: eu\nspec:\n  environment: prod\n")
	if len(res.Environments) != 0 || len(res.Runs) != 0 || len(res.Shared) != 0 || res.HasErrors() {
		t.Errorf("Environments %q, %d runs, shared %q", res.Environments, len(res.Runs), texts(res.Shared))
	}
	if _, err := p.RunEach(t.Context(), EachRequest{Source: loader.Source{FS: bundle(validBundle()), Path: "."}}); !errors.Is(err, loader.ErrOptions) {
		t.Errorf("RunEach without an environments file: %v, want loader.ErrOptions", err)
	}
	if _, err := p.RunEach(t.Context(), EachRequest{
		Source:       loader.Source{FS: bundle(validBundle()), Path: "missing"},
		Environments: loader.Source{FS: fstest.MapFS{"e.yaml": {Data: []byte(environmentDoc("prod", " {}"))}}, Path: "e.yaml"},
	}); !errors.Is(err, loader.ErrSource) {
		t.Errorf("RunEach of a missing source: %v, want loader.ErrSource", err)
	}
}

// TestRunEachRenderAndOnline covers EachRequest.Render and Online: each
// Environment's run keeps its rendered resources and runs the online
// stage, whose RZ-CFG-028 names the Environment.
func TestRunEachRenderAndOnline(t *testing.T) {
	p := newPipeline(t, Options{})
	control := loader.Source{FS: fstest.MapFS{"envs.yaml": {Data: []byte(docs(environmentDoc("staging", " {}"), environmentDoc("prod", " {}")))}}, Path: "envs.yaml"}
	res, err := p.RunEach(t.Context(), EachRequest{
		Source: loader.Source{Path: filepath.Join(examplesDir, "shop-bundle")}, Environments: control, Render: true, Online: true,
	})
	if err != nil {
		t.Fatalf("RunEach: %v", err)
	}
	if len(res.Runs) != 2 {
		t.Fatalf("%d runs, want 2", len(res.Runs))
	}
	for _, r := range res.Runs {
		if r.Validated == nil || r.Rendered == nil {
			t.Errorf("%s: Validated %v, %d rendered resources", r.Environment, r.Validated != nil, len(r.Rendered))
		}
		if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != CodeArtifact || r.Diagnostics[0].Environment != r.Environment {
			t.Errorf("%s: diagnostics %q, want the online stage's RZ-CFG-028", r.Environment, texts(r.Diagnostics))
		}
	}
}
