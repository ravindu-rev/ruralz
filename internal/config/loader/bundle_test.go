// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"slices"
	"testing"
	"testing/fstest"
)

// TestEnvelopeAPIVersion covers stage C's apiVersion rule: missing, not a
// string or not served is RZ-CFG-007, and the document leaves the union.
func TestEnvelopeAPIVersion(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte("kind: Upstream\nmetadata:\n  name: a\nspec: {}\n")}
	m["b.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: 1\nkind: Upstream\nmetadata:\n  name: b\nspec: {}\n")}
	m["c.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha2\nkind: Upstream\nmetadata:\n  name: c\nspec: {}\n")}
	r := load(t, newLoader(t, Options{}), m)
	wantTexts(t, "apiVersion", r.Diagnostics,
		"a.yaml:1:1 error RZ-CFG-007 Upstream/a apiVersion: apiVersion is required",
		"b.yaml:1:13 error RZ-CFG-007 Upstream/b apiVersion: apiVersion must be a string",
		`c.yaml:1:13 error RZ-CFG-007 Upstream/c apiVersion: apiVersion "ruralz/v1alpha2" is not served by this release (did you mean ruralz/v1alpha1? served apiVersions: ruralz/v1alpha1)`,
	)
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge"}) {
		t.Errorf("resources = %q", got)
	}
}

// TestEnvelopeKind covers stage C's kind rule: missing, not a string or
// not served by the apiVersion is RZ-CFG-007, with the nearest kind as the
// hint.
func TestEnvelopeKind(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nmetadata:\n  name: a\n")}
	m["b.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: [Route]\nmetadata:\n  name: b\n")}
	m["c.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Rout\nmetadata:\n  name: c\n")}
	m["d.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: ${KIND}\nmetadata:\n  name: d\n")}
	wantTexts(t, "kind", load(t, newLoader(t, Options{}), m).Diagnostics,
		"a.yaml:1:1 error RZ-CFG-007 kind: kind is required",
		"b.yaml:2:7 error RZ-CFG-007 kind: kind must be a string",
		`c.yaml:2:7 error RZ-CFG-007 Rout/c kind: kind "Rout" is not served by apiVersion "ruralz/v1alpha1" (did you mean "Route"?)`,
		`d.yaml:2:7 error RZ-CFG-007 ${KIND}/d kind: kind "${KIND}" is not served by apiVersion "ruralz/v1alpha1"`,
	)
}

// TestEnvelopeName covers stage C's identity rule: a document needs a
// metadata object with a string name (RZ-CFG-005).
func TestEnvelopeName(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nspec: {}\n")}
	m["b.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: x\n")}
	m["c.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  labels: {}\n")}
	m["d.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: 7\n")}
	wantTexts(t, "name", load(t, newLoader(t, Options{}), m).Diagnostics,
		"a.yaml:1:1 error RZ-CFG-005 metadata: metadata is required: it holds the name that identifies the resource",
		"b.yaml:3:11 error RZ-CFG-005 metadata: metadata must be an object",
		"c.yaml:4:3 error RZ-CFG-005 metadata.name: metadata.name is required: it identifies the resource",
		"d.yaml:4:9 error RZ-CFG-005 metadata.name: metadata.name must be a string",
	)
}

// TestControlKindsInBundle covers RZ-CFG-017: an Environment or Cluster in
// a Bundle, in a base file or an overlay, leaves it.
func TestControlKindsInBundle(t *testing.T) {
	m := mapFS(minimal())
	m["control.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: prod\nspec: {}\n",
		"apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: eu\nspec:\n  environment: prod\n",
	))}
	m["overlays/prod/env.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: qa\nspec: {}\n")}
	r := loadEnv(t, newLoader(t, Options{}), m, "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: prod\nspec: {}\n", "prod")
	wantTexts(t, "control kinds", r.Diagnostics,
		"control.yaml:2:7 error RZ-CFG-017 Environment/prod kind: an Environment lives outside the Bundle; the CLI reads it from an --environments file",
		"control.yaml:8:7 error RZ-CFG-017 Cluster/eu kind: a Cluster lives outside the Bundle; only Ruralz Control reads it",
		"overlays/prod/env.yaml:2:7 error RZ-CFG-017 Environment/qa kind: an Environment lives outside the Bundle; the CLI reads it from an --environments file",
	)
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge"}) {
		t.Errorf("resources = %q", got)
	}
}

// TestDuplicateIdentity covers RZ-CFG-008 in base files: the second
// declaration in path order is reported, naming the first, which stays in
// the union; equal names of different kinds are distinct identities.
func TestDuplicateIdentity(t *testing.T) {
	m := mapFS(minimal())
	m["a.yaml"] = &fstest.MapFile{Data: []byte(docs(upstream("orders"), route("orders")))}
	m["b/x.yaml"] = &fstest.MapFile{Data: []byte(upstream("orders"))}
	r := load(t, newLoader(t, Options{}), m)
	wantTexts(t, "duplicate", r.Diagnostics,
		"b/x.yaml:4:9 error RZ-CFG-008 Upstream/orders metadata.name: Upstream/orders is declared twice: a resource lives in exactly one place (first declared in a.yaml:4:9)")
	if got := ids(r.Resources); !slices.Equal(got, []string{"Gateway/edge", "Upstream/orders", "Route/orders"}) {
		t.Errorf("resources = %q", got)
	}
	if f := r.Files.File(r.Resources[1].Start.File); f.Path != "a.yaml" {
		t.Errorf("the kept Upstream comes from %q, want the first declaration", f.Path)
	}
}

// TestGatewayPlacement covers RZ-CFG-016 in the base union: a Gateway
// outside ruralz.yaml, a second Gateway in ruralz.yaml, and no Gateway.
func TestGatewayPlacement(t *testing.T) {
	l := newLoader(t, Options{})
	m := mapFS(minimal())
	m["more/gw.yaml"] = &fstest.MapFile{Data: []byte(gateway)}
	r := load(t, l, m)
	wantTexts(t, "same Gateway outside", r.Diagnostics,
		"more/gw.yaml:4:9 error RZ-CFG-008 Gateway/edge metadata.name: Gateway/edge is declared twice: a resource lives in exactly one place (first declared in ruralz.yaml:4:9)")
	if got := r.Files.File(r.Resources[0].Start.File).Path; got != RootFile {
		t.Errorf("the union keeps the Gateway of %q, want ruralz.yaml's", got)
	}
	m["more/gw.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: other\nspec: {}\n")}
	wantTexts(t, "another Gateway outside", load(t, l, m).Diagnostics,
		"more/gw.yaml:2:7 error RZ-CFG-016 Gateway/other kind: a Gateway is declared only in ruralz.yaml, the Bundle root")

	m = mapFS(map[string]string{RootFile: upstream("orders"), "gw.yaml": "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: other\nspec: {}\n"})
	wantTexts(t, "only outside", load(t, l, m).Diagnostics,
		"gw.yaml:2:7 error RZ-CFG-016 Gateway/other kind: a Gateway is declared only in ruralz.yaml, the Bundle root")

	m = mapFS(map[string]string{RootFile: docs(gateway, "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: second\nspec: {}\n")})
	wantTexts(t, "two in the root", load(t, l, m).Diagnostics,
		"ruralz.yaml:12:7 error RZ-CFG-016 Gateway/second kind: a Bundle has exactly one Gateway, and ruralz.yaml already declares Gateway/edge (first declared in ruralz.yaml:4:9)")

	m = mapFS(map[string]string{RootFile: upstream("orders")})
	wantTexts(t, "none", load(t, l, m).Diagnostics,
		"ruralz.yaml error RZ-CFG-016: the Bundle has no Gateway: ruralz.yaml declares exactly one")

	m = mapFS(map[string]string{RootFile: ""})
	wantTexts(t, "empty root", load(t, l, m).Diagnostics,
		"ruralz.yaml error RZ-CFG-016: the Bundle has no Gateway: ruralz.yaml declares exactly one")
}

// TestNoGatewayCascade covers the root file losing documents: when
// ruralz.yaml fails to parse, or its Gateway fails stage C, "no Gateway"
// would only repeat that finding and is not reported.
func TestNoGatewayCascade(t *testing.T) {
	l := newLoader(t, Options{})
	for name, root := range map[string]string{
		"syntax error":      "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge\n",
		"unserved version":  "apiVersion: ruralz/v9\nkind: Gateway\nmetadata:\n  name: edge\nspec: {}\n",
		"anchor in a file":  "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: &m\n  name: edge\nspec: {}\n",
		"without a name":    "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {}\nspec: {}\n",
		"LFS pointer":       "version https://git-lfs.github.com/spec/v1\n",
		"duplicate key":     "apiVersion: ruralz/v1alpha1\nkind: Gateway\nkind: Gateway\nmetadata:\n  name: edge\nspec: {}\n",
		"without a kind":    "apiVersion: ruralz/v1alpha1\nmetadata:\n  name: edge\nspec: {}\n",
		"with a bad header": "%YAML 1.1\n---\napiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec: {}\n",
	} {
		r := load(t, l, mapFS(map[string]string{RootFile: root}))
		if slices.Contains(codes(r.Diagnostics), CodeGateway) || !r.HasErrors() {
			t.Errorf("%s: diagnostics %q, want the root file's own finding without RZ-CFG-016", name, texts(r.Diagnostics))
		}
	}
}

// TestCheckBaseWithoutOverlay covers stage D's base check: $patch in a base
// document is RZ-CFG-006 whether or not an overlay is selected.
func TestCheckBaseWithoutOverlay(t *testing.T) {
	m := mapFS(minimal())
	m["u.yaml"] = &fstest.MapFile{Data: []byte(upstream("orders") + "  $patch: replace\n")}
	r := load(t, newLoader(t, Options{}), m)
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-006"}) || r.Diagnostics[0].File != "u.yaml" {
		t.Errorf("diagnostics = %q, want RZ-CFG-006 in u.yaml", texts(r.Diagnostics))
	}
}

// TestParseFile covers ParseFile: one file's documents as authored, in
// fresh trees, with the file's own findings; an unknown path is ErrNoFile.
func TestParseFile(t *testing.T) {
	m := mapFS(minimal())
	m["overlays/prod/u.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: ${T}\n",
		"kind: Upstream\nmetadata:\n  name: x\n",
	))}
	b := newBundle(t, m)
	rs, ds, err := b.ParseFile(t.Context(), "overlays/prod/u.yaml")
	if err != nil {
		t.Fatal(err)
	}
	wantTexts(t, "ParseFile", ds, "overlays/prod/u.yaml:7:1 error RZ-CFG-007 Upstream/x apiVersion: apiVersion is required")
	if len(rs) != 1 || at(t, rs[0].Root, "spec.timeout").Text != "${T}" {
		t.Fatalf("documents = %q", dumpAll(rs))
	}
	rs[0].Root.Members = nil
	again, _, _ := b.ParseFile(t.Context(), "overlays/prod/u.yaml")
	if len(again[0].Root.Members) == 0 {
		t.Error("a second ParseFile returned the tree the caller changed")
	}
	if _, _, err := b.ParseFile(t.Context(), "nope.yaml"); err == nil {
		t.Error("ParseFile of an unknown path succeeded")
	}
}
