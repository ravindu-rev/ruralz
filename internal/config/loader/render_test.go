// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// overlayBundle returns a Bundle with an Upstream and two overlays.
func overlayBundle() fstest.MapFS {
	m := mapFS(minimal())
	m["upstreams.yaml"] = &fstest.MapFile{Data: []byte(upstream("orders") + "  retries:\n    attempts: 2\n    perTryTimeout: 1s\n  timeout: 3s\n")}
	m["overlays/prod/scale.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  retries:\n    attempts: 3\n")}
	m["overlays/staging/fast.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 1s\n")}
	return m
}

// envs is an environments file with prod, staging and qa.
var envs = docs(environment("prod", " {}"), environment("staging", " {}"), environment("qa", " {}"))

// TestOverlaySelected covers stage D: the Environment selects
// overlays/<name>/ by default; the overlay merges into the base, scalars
// replaced, other fields kept; only that overlay applies.
func TestOverlaySelected(t *testing.T) {
	l := newLoader(t, Options{})
	r := loadEnv(t, l, overlayBundle(), envs, "prod")
	wantClean(t, "prod", r.Diagnostics)
	if r.Overlay != "prod" || r.Environment != "prod" {
		t.Errorf("Overlay %q, Environment %q", r.Overlay, r.Environment)
	}
	u := find(t, r.Resources, "Upstream/orders")
	if got := dump(at(t, u.Root, "spec.retries")) + dump(at(t, u.Root, "spec.timeout")); got != `{"attempts":3,"perTryTimeout":"1s"}"3s"` {
		t.Errorf("merged prod Upstream: %s", got)
	}
	r = loadEnv(t, l, overlayBundle(), envs, "staging")
	u = find(t, r.Resources, "Upstream/orders")
	if got := dump(at(t, u.Root, "spec.retries")) + dump(at(t, u.Root, "spec.timeout")); got != `{"attempts":2,"perTryTimeout":"1s"}"1s"` {
		t.Errorf("merged staging Upstream: %s", got)
	}
}

// TestOverlayExplicit covers spec.overlay naming the directory: it wins
// over the Environment's name; a directory that does not exist is
// RZ-CFG-009 at spec.overlay with the nearest overlay as the hint, and no
// overlay applies.
func TestOverlayExplicit(t *testing.T) {
	l := newLoader(t, Options{})
	file := docs(environment("production", "\n  overlay: prod"), environment("eu", "\n  overlay: prd"))
	r := loadEnv(t, l, overlayBundle(), file, "production")
	wantClean(t, "production", r.Diagnostics)
	if r.Overlay != "prod" {
		t.Errorf("Overlay = %q, want prod", r.Overlay)
	}
	r = loadEnv(t, l, overlayBundle(), file, "eu")
	wantTexts(t, "missing", r.Diagnostics,
		`envs.yaml:13:12 error RZ-CFG-009 Environment/eu spec.overlay: overlay directory "overlays/prd/" does not exist (did you mean "prod"?)`)
	if r.Overlay != "" || at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.retries.attempts").Text != "2" {
		t.Errorf("an overlay applied: %q", r.Overlay)
	}
	r = loadEnv(t, l, overlayBundle(), environment("x", "\n  overlay: ../prod"), "x")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeMissing}) || r.Overlay != "" {
		t.Errorf("an overlay outside overlays/: %q, overlay %q", texts(r.Diagnostics), r.Overlay)
	}
}

// TestOverlayDefaultedMissing covers an Environment without spec.overlay
// whose directory does not exist: an empty overlay, no diagnostic.
func TestOverlayDefaultedMissing(t *testing.T) {
	r := loadEnv(t, newLoader(t, Options{}), overlayBundle(), envs, "qa")
	wantClean(t, "qa", r.Diagnostics)
	if r.Overlay != "" || r.Environment != "qa" {
		t.Errorf("result = %+v", r)
	}
}

// TestNoOverlayWithoutEnvironment covers a render without an Environment:
// no overlay applies and no overlay's findings are reported.
func TestNoOverlayWithoutEnvironment(t *testing.T) {
	m := overlayBundle()
	m["overlays/prod/broken.yaml"] = &fstest.MapFile{Data: []byte("{ not yaml")}
	symlink(m, "overlays/staging/link.yaml", "../prod/scale.yaml")
	r := load(t, newLoader(t, Options{}), m)
	wantClean(t, "no Environment", r.Diagnostics)
	if r.Overlay != "" || at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.retries.attempts").Text != "2" {
		t.Errorf("an overlay applied: %q", r.Overlay)
	}
}

// TestOverlayFindingsWhenSelected covers an overlay's own findings: stage
// A to C findings of overlays/<name>/ are reported only by renders that
// select it.
func TestOverlayFindingsWhenSelected(t *testing.T) {
	m := overlayBundle()
	symlink(m, "overlays/staging/link.yaml", "../prod/scale.yaml")
	m["overlays/staging/x.yaml"] = &fstest.MapFile{Data: []byte("kind: Upstream\nmetadata:\n  name: x\n")}
	l := newLoader(t, Options{})
	wantClean(t, "prod", loadEnv(t, l, m, envs, "prod").Diagnostics)
	wantTexts(t, "staging", loadEnv(t, l, m, envs, "staging").Diagnostics,
		"overlays/staging/link.yaml error RZ-CFG-001: symbolic link: a Bundle holds only regular files and directories (Ruralz Control reads the Bundle from Git, where a link has no file content; commit the file itself)",
		"overlays/staging/x.yaml:1:1 error RZ-CFG-007 Upstream/x apiVersion: apiVersion is required",
	)
}

// TestOverlayDirectives covers stage D through the loader: a delete
// directive, an addition (after the base, in kind and name order), a
// duplicate identity in one overlay (RZ-CFG-008, applied nowhere) and an
// apiVersion that differs from the base (RZ-CFG-030).
func TestOverlayDirectives(t *testing.T) {
	m := overlayBundle()
	m["more.yaml"] = &fstest.MapFile{Data: []byte(docs(upstream("legacy"), upstream("twice")))}
	m["overlays/prod/scale.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: legacy\n  annotations:\n    ruralz.io/patch: delete\n",
		upstream("zeta"), upstream("alpha"), route("new"),
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: twice\nspec:\n  timeout: 1s\n",
	))}
	m["overlays/prod/z.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: twice\nspec:\n  timeout: 2s\n",
	))}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeDuplicate}) {
		t.Errorf("diagnostics %q, want one RZ-CFG-008", texts(r.Diagnostics))
	}
	want := []string{"Gateway/edge", "Upstream/twice", "Upstream/orders", "Route/new", "Upstream/alpha", "Upstream/zeta"}
	if got := ids(r.Resources); !slices.Equal(got, want) {
		t.Errorf("resources = %q\nwant %q", got, want)
	}
	if _, ok := find(t, r.Resources, "Upstream/twice").Root.Get("spec"); !ok {
		t.Fatal("no spec")
	}
	if _, ok := at(t, find(t, r.Resources, "Upstream/twice").Root, "spec").Get("timeout"); ok {
		t.Error("a duplicated overlay identity was applied")
	}
}

// TestOverlayAPIVersionMismatch covers RZ-CFG-030 through the loader: two
// served apiVersions, and an overlay that patches a base resource with
// the other one.
func TestOverlayAPIVersionMismatch(t *testing.T) {
	l := twoVersions(t)
	m := overlayBundle()
	m["overlays/prod/scale.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1beta9\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 9s\n")}
	r := loadEnv(t, l, m, envs, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-030"}) {
		t.Errorf("diagnostics %q, want RZ-CFG-030", texts(r.Diagnostics))
	}
}

// TestOverlayEnvelope covers stage C on overlay documents: a document
// without an apiVersion or of a kind outside the Bundle never reaches the
// merge.
func TestOverlayEnvelope(t *testing.T) {
	m := overlayBundle()
	m["overlays/prod/scale.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"kind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 9s\n",
		"apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: eu\nspec:\n  environment: prod\n",
	))}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeUnserved, CodeControlKind}) {
		t.Errorf("diagnostics %q", texts(r.Diagnostics))
	}
	if got := at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.timeout").Text; got != "3s" {
		t.Errorf("timeout = %s, want the base's", got)
	}
}

// TestOverlayGateway covers RZ-CFG-016 after the merge: an overlay may
// patch the Gateway, but neither add another nor remove it.
func TestOverlayGateway(t *testing.T) {
	l := newLoader(t, Options{})
	m := overlayBundle()
	m["overlays/prod/gw.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  admin:\n    port: 9902\n")}
	r := loadEnv(t, l, m, envs, "prod")
	wantClean(t, "patched Gateway", r.Diagnostics)
	if got := at(t, find(t, r.Resources, "Gateway/edge").Root, "spec.admin.port").Text; got != "9902" {
		t.Errorf("admin.port = %s", got)
	}
	m["overlays/prod/gw.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: second\nspec: {}\n")}
	wantTexts(t, "added Gateway", loadEnv(t, l, m, envs, "prod").Diagnostics,
		"overlays/prod/gw.yaml:2:7 error RZ-CFG-016 Gateway/second kind: an overlay cannot add a Gateway: ruralz.yaml declares the Bundle's only Gateway")
	m["overlays/prod/gw.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\n  annotations:\n    ruralz.io/patch: delete\n")}
	r = loadEnv(t, l, m, envs, "prod")
	wantTexts(t, "removed Gateway", r.Diagnostics,
		"overlays/prod/gw.yaml:6:22 error RZ-CFG-016 Gateway/edge: an overlay cannot remove the Gateway: a Bundle has exactly one")
	if slices.Contains(ids(r.Resources), "Gateway/edge") {
		t.Error("the Gateway is still there")
	}
}

// TestOverlayAdditionsWithheld covers cascades: when a base file loses
// documents to a parse error, an overlay addition may be the patch of a
// lost resource, so it is withheld; so is a patch of a base document stage
// C dropped. The base's own finding is the only one.
func TestOverlayAdditionsWithheld(t *testing.T) {
	l := newLoader(t, Options{})
	m := overlayBundle()
	m["broken.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: x\n")}
	m["overlays/prod/x.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: x\nspec:\n  timeout: 1s\n")}
	r := loadEnv(t, l, m, envs, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-001"}) {
		t.Errorf("diagnostics %q, want the parse error alone", texts(r.Diagnostics))
	}
	if slices.Contains(ids(r.Resources), "Upstream/x") {
		t.Error("the overlay addition was not withheld")
	}

	m = overlayBundle()
	m["old.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v0\nkind: Upstream\nmetadata:\n  name: x\nspec: {}\n")}
	m["overlays/prod/x.yaml"] = &fstest.MapFile{Data: []byte(docs(
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: x\nspec:\n  timeout: 1s\n",
		upstream("brand-new"),
	))}
	r = loadEnv(t, l, m, envs, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeUnserved}) {
		t.Errorf("diagnostics %q, want the unserved apiVersion alone", texts(r.Diagnostics))
	}
	if got := ids(r.Resources); slices.Contains(got, "Upstream/x") || !slices.Contains(got, "Upstream/brand-new") {
		t.Errorf("resources = %q: the patch of the dropped resource must be withheld, the new one kept", got)
	}
}

// TestSubstitution covers stage E's variable sources: an Environment's
// variables and never the caller's; without an Environment, the caller's
// (the process environment); hints suggest only an Environment's names;
// a whole-value expression is re-typed.
func TestSubstitution(t *testing.T) {
	l := newLoader(t, Options{})
	m := mapFS(minimal())
	m["u.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n  endpoints:\n    - address: ${HOST}:80\n  retries:\n    attempts: ${N}\n")}
	file := docs(environment("prod", "\n  variables:\n    HOST: prod.internal\n    N: \"4\"\n    HOTS: x"), environment("qa", " {}"))
	r, err := l.Load(t.Context(), Request{
		Source: Source{FS: m, Path: "."}, Environments: Source{FS: fstest.MapFS{"e.yaml": {Data: []byte(file)}}, Path: "e.yaml"},
		Environment: "prod", Variables: vars("HOST", "process.internal", "N", "9"),
	})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, "prod", r.Diagnostics)
	u := find(t, r.Resources, "Upstream/orders")
	if a, n := at(t, u.Root, "spec.endpoints.0.address"), at(t, u.Root, "spec.retries.attempts"); a.Text != "prod.internal:80" || n.Kind != tree.KindInt || n.Text != "4" {
		t.Errorf("substituted %s and %s", dump(a), dump(n))
	}

	r, err = l.Load(t.Context(), Request{Source: Source{FS: m, Path: "."}, Variables: vars("HOST", "process.internal", "N", "9")})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, "process", r.Diagnostics)
	if a := at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.endpoints.0.address"); a.Text != "process.internal:80" {
		t.Errorf("without an Environment: %s", dump(a))
	}

	m["u.yaml"] = &fstest.MapFile{Data: []byte(strings.Replace(string(m["u.yaml"].Data), "${HOST}", "${HOST1}", 1))}
	r, err = l.Load(t.Context(), Request{
		Source: Source{FS: m, Path: "."}, Environments: Source{FS: fstest.MapFS{"e.yaml": {Data: []byte(file)}}, Path: "e.yaml"},
		Environment: "prod", Variables: vars("HOST1", "process.internal"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "RZ-CFG-010" || !strings.Contains(r.Diagnostics[0].Hint, `"HOST"`) {
		t.Errorf("undefined in prod: %q, want RZ-CFG-010 with the Environment's HOST as the hint", texts(r.Diagnostics))
	}
	r, _ = l.Load(t.Context(), Request{Source: Source{FS: m, Path: "."}, Variables: vars("HOST", "x", "N", "1")})
	if len(r.Diagnostics) != 1 || r.Diagnostics[0].Code != "RZ-CFG-010" || r.Diagnostics[0].Hint != "" {
		t.Errorf("undefined in the process environment: %q, want no hint", texts(r.Diagnostics))
	}
}

// TestSubstitutionAfterOverlay covers the stage order: the overlay merges
// before substitution, so a value the overlay replaces is never
// substituted, and an expression the overlay writes is. Keys compare as
// written when they merge.
func TestSubstitutionAfterOverlay(t *testing.T) {
	m := overlayBundle()
	m["u.yaml"] = &fstest.MapFile{Data: []byte(upstream("cart") + "  timeout: ${UNDEFINED}\n")}
	m["overlays/prod/cart.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: cart\nspec:\n  timeout: 2s\n  endpoints:\n    - address: ${HOST:-cart2.internal}:80\n")}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prod")
	wantClean(t, "overlay then substitution", r.Diagnostics)
	u := find(t, r.Resources, "Upstream/cart")
	if got := dump(at(t, u.Root, "spec.timeout")) + dump(at(t, u.Root, "spec.endpoints")); got != `"2s"[{"address":"cart.internal:80"},{"address":"cart2.internal:80"}]` {
		t.Errorf("cart = %s", got)
	}
}

// TestSubstitutionOutputLimit covers the substitution output limit, which
// is the source limit: once spent, its one RZ-CFG-001 is stage E's only
// finding and the load stops.
func TestSubstitutionOutputLimit(t *testing.T) {
	l := newLoader(t, Options{Limits: Limits{MaxSourceBytes: 4096}})
	m := mapFS(minimal())
	var b strings.Builder
	b.WriteString("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n  endpoints:\n")
	for i := range 20 {
		b.WriteString("    - address: ${BIG}" + strings.Repeat("x", i) + "\n")
	}
	b.WriteString("  timeout: ${UNDEFINED}\n")
	m["u.yaml"] = &fstest.MapFile{Data: []byte(b.String())}
	r, err := l.Load(t.Context(), Request{Source: Source{FS: m, Path: "."}, Variables: vars("BIG", strings.Repeat("v", 500))})
	if err != nil {
		t.Fatal(err)
	}
	wantTexts(t, "output limit", r.Diagnostics,
		"- error RZ-CFG-001: substitution writes more than 4096 bytes of text in one render (each use of a variable repeats its value: keep large values out of variables used often)")
	if !r.Stopped || r.Resources != nil {
		t.Errorf("result: stopped %v, %d resources", r.Stopped, len(r.Resources))
	}
}

// TestRenderTwice covers one Bundle rendered for several Environments,
// concurrently: each render works on its own copy, so results never mix
// and the Bundle's trees never change.
func TestRenderTwice(t *testing.T) {
	l := newLoader(t, Options{})
	b, err := l.Parse(t.Context(), Source{FS: overlayBundle(), Path: "."})
	if err != nil {
		t.Fatal(err)
	}
	e := readEnvs(t, l, envs)
	before := dumpAll(b.base)
	var wg sync.WaitGroup
	got := map[string][]string{}
	var mu sync.Mutex
	for range 4 {
		for _, name := range []string{"prod", "staging", "qa", ""} {
			wg.Go(func() {
				env, _ := e.Select(name)
				r, err := b.Render(t.Context(), env, nil)
				if err != nil {
					t.Error(err)
					return
				}
				d := dumpAll(r.Resources)
				mu.Lock()
				defer mu.Unlock()
				if prev, ok := got[name]; ok && !slices.Equal(prev, d) {
					t.Errorf("%s: renders differ", name)
				}
				got[name] = d
			})
		}
	}
	wg.Wait()
	if after := dumpAll(b.base); !slices.Equal(before, after) {
		t.Errorf("Render changed the Bundle:\n%q\n%q", before, after)
	}
	if slices.Equal(got["prod"], got["staging"]) || !slices.Equal(got["qa"], got[""]) {
		t.Errorf("renders: %q", got)
	}
}

// TestOneFileBundleHasNoOverlay covers an Environment with a one-file
// Bundle: its variables apply, and its overlay does not, without
// RZ-CFG-009.
func TestOneFileBundleHasNoOverlay(t *testing.T) {
	l := newLoader(t, Options{})
	m := fstest.MapFS{
		"r.yaml": {Data: []byte(docs(gateway, "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: o\nspec:\n  protocol: http\n  endpoints:\n    - address: ${HOST}\n"))},
		"e.yaml": {Data: []byte(environment("prod", "\n  overlay: prod\n  variables:\n    HOST: h"))},
	}
	r, err := l.Load(t.Context(), Request{Source: Source{FS: m, Path: "r.yaml"}, Environments: Source{FS: m, Path: "e.yaml"}, Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	wantClean(t, "one-file", r.Diagnostics)
	if r.Overlay != "" || at(t, find(t, r.Resources, "Upstream/o").Root, "spec.endpoints.0.address").Text != "h" {
		t.Errorf("result: overlay %q, %q", r.Overlay, dumpAll(r.Resources))
	}
}

// TestLoadRequest covers the request rules: an Environment needs an
// environments file (reading Environments from Ruralz Control is Planned
// (M2)), and an environments file needs an Environment name.
func TestLoadRequest(t *testing.T) {
	l := newLoader(t, Options{})
	m := mapFS(minimal())
	for name, req := range map[string]Request{
		"Environment without file": {Source: Source{FS: m, Path: "."}, Environment: "prod"},
		"file without Environment": {Source: Source{FS: m, Path: "."}, Environments: Source{FS: m, Path: RootFile}},
	} {
		if _, err := l.Load(t.Context(), req); !errors.Is(err, ErrOptions) {
			t.Errorf("%s: error %v, want ErrOptions", name, err)
		}
	}
}

// TestLoadUnknownEnvironment covers --env naming no Environment of the
// file: RZ-CFG-009; the load stops after reporting the Bundle's own
// findings too.
func TestLoadUnknownEnvironment(t *testing.T) {
	m := overlayBundle()
	m["broken.yaml"] = &fstest.MapFile{Data: []byte("{")}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prd")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeLimit, CodeMissing}) {
		t.Errorf("diagnostics %q", texts(r.Diagnostics))
	}
	if !r.Stopped || r.Resources != nil || r.Environment != "prd" {
		t.Errorf("result: %+v", r)
	}
}

// TestLoadReportsEnvironmentsFile covers Load's diagnostics: the
// environments file's findings are part of the run, beside the Bundle's.
func TestLoadReportsEnvironmentsFile(t *testing.T) {
	file := docs(environment("prod", " {}"), environment("a", "\n  promotion:\n    from: a"))
	r := loadEnv(t, newLoader(t, Options{}), overlayBundle(), file, "prod")
	if got := codes(r.Diagnostics); !slices.Equal(got, []string{CodeCycle}) {
		t.Errorf("diagnostics %q", texts(r.Diagnostics))
	}
	if r.Stopped || r.Overlay != "prod" {
		t.Errorf("result: %+v", r)
	}
}

// TestLoaderWritesNoDefault covers the boundary with stage G: the loader
// writes no schema or registry default, so an absent field stays absent
// for stage F, and stage G applies defaults only to absent fields.
func TestLoaderWritesNoDefault(t *testing.T) {
	m := overlayBundle()
	m["p.yaml"] = &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata:\n  name: p\nspec:\n  type: cors\n  config: {}\n")}
	r := loadEnv(t, newLoader(t, Options{}), m, envs, "prod")
	wantClean(t, "defaults", r.Diagnostics)
	if got := dump(at(t, find(t, r.Resources, "Upstream/orders").Root, "spec.endpoints")); got != `[{"address":"orders.internal:80"}]` {
		t.Errorf("endpoints = %s, want no weight default", got)
	}
	if got := dump(at(t, find(t, r.Resources, "Policy/p").Root, "spec")); got != `{"type":"cors","config":{}}` {
		t.Errorf("Policy spec = %s, want no overridable, slot or failureMode", got)
	}
}
