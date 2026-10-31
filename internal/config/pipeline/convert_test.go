// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"errors"
	"slices"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestConversionThroughFacade covers what ruralz bundle render
// --api-version runs through the facade: the Bundle's files parsed under
// the run's limits (Parse), converted with the shared registry
// (Converters), and both trees built for every Environment of the
// environments file in file order (ReadEnvironments, BuildFunc), which
// yield equal Revisions; a tree that does not build yields no digest and
// its diagnostics.
func TestConversionThroughFacade(t *testing.T) {
	p := newPipeline(t, Options{})
	files := validBundle()
	files["overlays/prod/r.yaml"] = "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: a\nspec:\n  timeout: 4s\n"
	files["upstreams.yaml"] = docs(upstreamDoc("orders")+"  timeout: ${TIMEOUT:-2s}\n", upstreamDoc("secure"))
	src := loader.Source{FS: bundle(files), Path: "."}
	control := loader.Source{FS: fstest.MapFS{"envs.yaml": {Data: []byte(docs(
		environmentDoc("staging", "\n  variables:\n    TIMEOUT: 3s"),
		environmentDoc("prod", " {}"),
	))}}, Path: "envs.yaml"}

	envs, err := p.ReadEnvironments(t.Context(), control)
	if err != nil {
		t.Fatalf("ReadEnvironments: %v", err)
	}
	if got := envs.FileOrder(); !slices.Equal(got, []string{"staging", "prod"}) {
		t.Fatalf("FileOrder = %q", got)
	}
	b, err := p.Parse(t.Context(), src)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	var sources []render.SourceFile
	for _, f := range b.Files() {
		rs, ds, err := b.ParseFile(t.Context(), f.Path)
		if err != nil || len(ds) > 0 {
			t.Fatalf("ParseFile(%s) = %q, %v", f.Path, texts(ds), err)
		}
		sf := render.SourceFile{Path: f.Path}
		for _, r := range rs {
			sf.Documents = append(sf.Documents, r.Root)
		}
		sources = append(sources, sf)
	}
	out, ds, err := render.Convert(t.Context(), sources, v1alpha1.APIVersion, p.Converters())
	if err != nil || len(ds) > 0 {
		t.Fatalf("Convert = %q, %v", texts(ds), err)
	}
	converted := fstest.MapFS{}
	for _, f := range out {
		converted[f.Path] = &fstest.MapFile{Data: f.Content}
	}
	cs, err := render.VerifyConversion(t.Context(), envs.FileOrder(), p.BuildFunc(src, control), p.BuildFunc(loader.Source{FS: converted, Path: "."}, control))
	if err != nil {
		t.Fatalf("VerifyConversion: %v", err)
	}
	if !render.AllEqual(cs) || cs[0].Source.Digest == cs[1].Source.Digest {
		t.Errorf("comparisons %+v, want equal Revisions per Environment, different between Environments", cs)
	}

	broken := p.BuildFunc(loader.Source{FS: bundle(map[string]string{"ruralz.yaml": gatewayDoc, "r.yaml": routeDoc("a", "x", "")}), Path: "."}, control)
	digest, ds, err := broken(t.Context(), "prod")
	if err != nil || !digest.IsZero() || !ds.HasErrors() {
		t.Errorf("BuildFunc of a failing Bundle = %s, %q, %v; want no digest and its diagnostics", digest, texts(ds), err)
	}
	if _, _, err := p.BuildFunc(loader.Source{FS: converted, Path: "missing"}, control)(t.Context(), "prod"); !errors.Is(err, loader.ErrSource) {
		t.Errorf("BuildFunc of a missing source: %v, want loader.ErrSource", err)
	}
}
