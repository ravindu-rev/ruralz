// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"slices"
	"testing"
	"testing/fstest"
)

// FuzzLoad covers stages A to E on arbitrary file contents: a base file,
// an overlay file, a .ruralzignore and an environments file. Every load
// ends without a panic, with registered codes in sorted order, the same
// result for one worker and for several, and resources exactly when it did
// not stop.
func FuzzLoad(f *testing.F) {
	f.Add(upstream("orders")+"  timeout: ${T:-1s}\n", "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 2s\n", "", environment("prod", "\n  variables:\n    T: 3s"))
	f.Add("a: &x 1\nb: *x\n", "{", "*.yaml\n!ruralz.yaml\n", "kind: [")
	f.Add(docs(upstream("a"), upstream("a")), upstream("b")+"  $patch: delete\n", "[", environment("prod", "\n  overlay: ../x"))
	f.Add("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\nspec: {}\n", "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\n  annotations:\n    ruralz.io/patch: delete\n", "overlays/\n", environment("prod", "\n  promotion:\n    from: prod"))
	f.Add("version https://git-lfs.github.com/spec/v1\n", "\xef\xbb\xbfkind: Route\n", "\\", "")
	one := newLoader(f, Options{Workers: 1, Limits: Limits{MaxDiagnostics: 20}})
	many := newLoader(f, Options{Workers: 3, Limits: Limits{MaxDiagnostics: 20}})
	f.Fuzz(func(t *testing.T, base, over, ignore, envFile string) {
		m := mapFS(minimal())
		m["base.yaml"] = &fstest.MapFile{Data: []byte(base)}
		m["overlays/prod/over.yaml"] = &fstest.MapFile{Data: []byte(over)}
		if ignore != "" {
			m[IgnoreFile] = &fstest.MapFile{Data: []byte(ignore)}
		}
		m["envs.yaml"] = &fstest.MapFile{Data: []byte(envFile)}
		run := func(l *Loader) *Result {
			r, err := l.Load(t.Context(), Request{
				Source: Source{FS: m, Path: "."}, Environments: Source{FS: m, Path: "envs.yaml"}, Environment: "prod",
			})
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			return r
		}
		r1, r3 := run(one), run(many)
		registered(t, r1.Diagnostics)
		sorted := slices.Clone(r1.Diagnostics)
		sorted.Sort()
		if !slices.Equal(texts(sorted), texts(r1.Diagnostics)) {
			t.Errorf("diagnostics are not sorted: %q", texts(r1.Diagnostics))
		}
		if !slices.Equal(texts(r1.Diagnostics), texts(r3.Diagnostics)) || !slices.Equal(dumpAll(r1.Resources), dumpAll(r3.Resources)) {
			t.Errorf("one worker and three differ:\n%q\n%q", texts(r1.Diagnostics), texts(r3.Diagnostics))
		}
		if r1.Stopped != (r1.Resources == nil) {
			t.Errorf("stopped %v with %d resources", r1.Stopped, len(r1.Resources))
		}
	})
}
