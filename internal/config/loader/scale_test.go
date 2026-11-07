// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

// scaleBundle returns a Bundle of 300 Upstreams and 300 Routes in 100
// files, with an overlay that patches every tenth Upstream.
func scaleBundle() fstest.MapFS {
	const n = 300
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

// TestScaleLoad checks that the 601 resources of scaleBundle load without
// findings, with the overlay's patch on every tenth Upstream and the
// substitution default on the others.
func TestScaleLoad(t *testing.T) {
	m := scaleBundle()
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
