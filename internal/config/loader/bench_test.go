// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// scaleBundle returns a Bundle of n Upstreams and n Routes in 100 files,
// with an overlay that patches every tenth Upstream.
func scaleBundle(n int) fstest.MapFS {
	m := mapFS(minimal())
	var patches []string
	for f := range 100 {
		var ds []string
		for i := f; i < n; i += 100 {
			name := "u" + strconv.Itoa(i)
			ds = append(ds, upstream(name)+"  timeout: ${TIMEOUT:-3s}\n", strings.Replace(route("r"+strconv.Itoa(i)), "name: orders", "name: "+name, 1))
			if i%10 == 0 {
				patches = append(patches, "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: "+name+"\nspec:\n  timeout: 9s\n")
			}
		}
		m["r/"+strconv.Itoa(f)+".yaml"] = &fstest.MapFile{Data: []byte(docs(ds...))}
	}
	m["overlays/prod/p.yaml"] = &fstest.MapFile{Data: []byte(docs(patches...))}
	return m
}

// BenchmarkLoad loads 10,000 resources through stages A to E for one
// Environment, the loader's share of the 2 s budget for validating 10,000
// resources (target).
func BenchmarkLoad(b *testing.B) {
	l := newLoader(b, Options{})
	m := scaleBundle(5000)
	envs := fstest.MapFS{"e.yaml": {Data: []byte(environment("prod", " {}"))}}
	b.ReportAllocs()
	for b.Loop() {
		r, err := l.Load(b.Context(), Request{Source: Source{FS: m, Path: "."}, Environments: Source{FS: envs, Path: "e.yaml"}, Environment: "prod"})
		if err != nil || r.HasErrors() || len(r.Resources) != 10_001 {
			b.Fatalf("Load: %v, %d resources, %q", err, len(r.Resources), texts(r.Diagnostics))
		}
	}
}

// TestScaleLoad checks the scale fixture loads without findings, so the
// benchmark measures a clean Bundle.
func TestScaleLoad(t *testing.T) {
	m := scaleBundle(300)
	r := loadEnv(t, newLoader(t, Options{}), m, environment("prod", " {}"), "prod")
	wantClean(t, "scale", r.Diagnostics)
	if len(r.Resources) != 601 {
		t.Errorf("%d resources", len(r.Resources))
	}
	if got := at(t, find(t, r.Resources, "Upstream/u10").Root, "spec.timeout").Text; got != "9s" {
		t.Errorf("patched timeout %q", got)
	}
	if got := at(t, find(t, r.Resources, "Upstream/u11").Root, "spec.timeout").Text; got != "3s" {
		t.Errorf("substituted timeout %q", got)
	}
}
