// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The example Bundle tests (docs/engineering/02-repository-layout-and-
// conventions.md "Generator rules"): examples/ holds what the documents
// show, byte for byte, and every Bundle there renders through the
// configuration pipeline facade, with checks.Table() and the CEL
// compiler, without --env and for each Environment of examples/control/
// with no diagnostic, warnings included.

// The Bundles of examples/, each a directory beside control/.
const (
	// shopBundle is the example Bundle: "The shop-bundle from Bundle
	// layout and merge" (configuration model "Complete annotated example
	// Bundle").
	shopBundle = "shop-bundle"
	// shopRestBundle is the part of it an M1 Node serves (configuration
	// model "REST example Bundle").
	shopRestBundle = "shop-rest-bundle"
	// quickstartBundle is the Bundle of the scripted quickstart
	// (Deployment topologies "Quickstart").
	quickstartBundle = "quickstart"
)

// exampleSource is one document section that shows files of examples/: a
// name its captions give relative to a Bundle root is a file of dir.
type exampleSource struct {
	doc, heading, dir string
}

// exampleSources returns every section that shows files of examples/.
func exampleSources() []exampleSource {
	return []exampleSource{
		{configModelDoc, "## Complete annotated example Bundle", shopBundle},
		{configModelDoc, "## REST example Bundle", shopRestBundle},
		{topologiesDoc, "#### Quickstart", quickstartBundle},
	}
}

// exampleRel returns the examples/ path of a file a caption names: a
// control/ file sits beside the Bundles, a name under examples/ is given
// from the repository root, and any other name is relative to dir.
func exampleRel(dir, name string) string {
	switch {
	case strings.HasPrefix(name, controlDir+"/"):
		return name
	case strings.HasPrefix(name, examplesDir+"/"):
		return strings.TrimPrefix(name, examplesDir+"/")
	default:
		return dir + "/" + name
	}
}

// TestExamplesMatchDocument checks that examples/ holds what the documents
// show, byte for byte (exampleSources): each block of a section whose
// caption names files is those files joined by "---" lines, every section
// shows at least one file, no file is shown twice, and every configuration
// file and .ruralzignore under examples/ is shown.
func TestExamplesMatchDocument(t *testing.T) {
	s := mustSuite(t)
	shown := map[string]string{}
	for _, src := range exampleSources() {
		blocks := 0
		for _, b := range fencedBlocks(docSection(t, s, src.doc, src.heading)) {
			names := captionFiles(b.caption)
			if len(names) == 0 {
				continue
			}
			blocks++
			parts := make([]string, 0, len(names))
			for _, name := range names {
				rel := exampleRel(src.dir, name)
				if prev, dup := shown[rel]; dup {
					t.Errorf("examples/%s is shown in %q and %q", rel, prev, src.heading)
				}
				shown[rel] = src.heading
				data, err := os.ReadFile(s.path(examplesDir + "/" + rel))
				if err != nil {
					t.Errorf("%q shows examples/%s: %v", src.heading, rel, err)
				}
				parts = append(parts, string(data))
			}
			if got := strings.Join(parts, "---\n"); got != b.body {
				t.Errorf("examples/ %s differ from the block after %q: %s", strings.Join(names, ", "), b.caption, firstDiff(got, b.body))
			}
		}
		if blocks == 0 {
			t.Errorf("%q in %s shows no file", src.heading, src.doc)
		}
	}
	err := fs.WalkDir(os.DirFS(s.path(examplesDir)), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && isExampleFile(p) && shown[p] == "" {
			t.Errorf("examples/%s is in no document section", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// authoringView compiles the authoring view of ruralz/v1alpha1 once, with
// the index loaded from the same bytes, as schemaview.Compile requires.
var authoringView = sync.OnceValues(func() (*schemaview.View, error) {
	data := schema.AuthoringV1alpha1()
	idx, err := schemaidx.Load(v1alpha1.APIVersion, data)
	if err != nil {
		return nil, err
	}
	return schemaview.Compile(data, idx)
})

// resourceOf wraps a parsed document as a resource, as stage C does.
func resourceOf(doc profile.Document) *tree.Resource {
	root := doc.Root
	r := &tree.Resource{Root: root, Start: doc.Start}
	if k, ok := root.Get("kind"); ok && k.Kind == tree.KindString {
		r.ID.Kind = v1alpha1.Kind(k.Text)
	}
	if a, ok := root.Get("apiVersion"); ok && a.Kind == tree.KindString {
		r.APIVersion = a.Text
	}
	if meta, ok := root.Get("metadata"); ok {
		if n, ok := meta.Get("name"); ok && n.Kind == tree.KindString {
			r.ID.Name = n.Text
		}
	}
	return r
}

// checkDocuments parses the file rel of the examples tree dir with the
// production front ends (the restricted YAML profile, strict JSON) and
// validates each document whose apiVersion starts with ruralz/ against the
// authoring view, since raw documents precede substitution. It returns one
// problem per finding, the parse's included when required is set, and the
// documents it validated; a file that need not be a Ruralz file at all
// (outside every Bundle and control/, such as a Compose file with anchors)
// that does not parse is left out.
func checkDocuments(t testing.TB, dir, rel string, required bool) (problems []string, validated int) {
	t.Helper()
	view, err := authoringView()
	if err != nil {
		t.Fatalf("compile the authoring view: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel))) //nolint:gosec // G304: a file of the examples tree under test.
	if err != nil {
		t.Fatal(err)
	}
	format, _ := profile.FormatOf(rel)
	files := &tree.FileTable{}
	docs, ds, err := profile.Parse(context.Background(), data, files.Add(tree.File{Path: rel, Role: tree.RoleBase}), rel, format, profile.Options{})
	if err != nil {
		t.Fatalf("profile.Parse(%s) error = %v", rel, err)
	}
	if len(ds) > 0 && !required {
		return nil, 0
	}
	for _, d := range ds {
		problems = append(problems, strings.TrimSpace(texts(diag.List{d})))
	}
	for _, doc := range docs {
		if v, ok := doc.Root.Get("apiVersion"); !ok || v.Kind != tree.KindString || !strings.HasPrefix(v.Text, "ruralz/") {
			continue
		}
		validated++
		for _, d := range view.Validate(resourceOf(doc), files) {
			problems = append(problems, strings.TrimSpace(texts(diag.List{d})))
		}
	}
	return problems, validated
}

// bundleRoots returns the directories of the examples tree dir that hold a
// ruralz.yaml, hidden ones excepted, slash-separated and sorted.
func bundleRoots(t testing.TB, dir string) []string {
	t.Helper()
	var roots []string
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() {
			return err
		}
		if p != "." && strings.HasPrefix(d.Name(), ".") {
			return fs.SkipDir
		}
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(p), bundleRoot)); err == nil {
			roots = append(roots, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return roots
}

// checkExampleTree checks an examples tree dir as the example Bundle tests
// do and returns one problem per finding, warnings included, and the count
// of documents and renders it checked:
//   - every document of a Bundle's base files (those discovery reads,
//     overlay patches aside) and of control/ validates against the
//     authoring view, and so does every ruralz/ document of another file
//     that parses (checkDocuments);
//   - control/environments.yaml has no finding;
//   - every Bundle renders through the CLI's pipeline without --env and
//     for each Environment with no diagnostic, each problem
//     "<Bundle root>: <diagnostic>";
//   - every overlays/<name>/ directory is selected by an Environment, so
//     some render checks it.
func checkExampleTree(t testing.TB, s *suite, dir string) (problems []string, documents, renders int) {
	t.Helper()
	roots := bundleRoots(t, dir)
	inBundle := func(rel string) bool {
		return slices.ContainsFunc(roots, func(r string) bool { return r == "." || strings.HasPrefix(rel, r+"/") })
	}
	envFile := filepath.Join(dir, controlDir, environmentsName)
	var envs []string
	selected := map[string]bool{}
	if _, err := os.Stat(envFile); err == nil {
		read, err := s.cli.ReadEnvironments(context.Background(), environmentsSource(envFile))
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range read.Diagnostics() {
			problems = append(problems, controlDir+": "+strings.TrimSpace(texts(diag.List{d})))
		}
		envs = read.FileOrder()
		for _, name := range envs {
			if e, _ := read.Select(name); e != nil {
				selected[e.Overlay] = true
			}
		}
	}
	// Every configuration file outside the Bundles: control/ and the rest.
	err := fs.WalkDir(os.DirFS(dir), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if p != "." && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}
		if _, ok := profile.FormatOf(d.Name()); !ok || strings.HasPrefix(d.Name(), ".") || inBundle(p) {
			return nil
		}
		ps, n := checkDocuments(t, dir, p, strings.HasPrefix(p, controlDir+"/"))
		problems = append(problems, ps...)
		documents += n
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, root := range roots {
		bundle := filepath.Join(dir, filepath.FromSlash(root))
		b, err := s.cli.Parse(context.Background(), loaderSource(bundle))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range b.Files() {
			if f.Overlay != "" {
				continue
			}
			ps, n := checkDocuments(t, bundle, f.Path, true)
			for _, p := range ps {
				problems = append(problems, root+": "+p)
			}
			documents += n
		}
		for _, name := range b.Overlays() {
			if !selected[name] {
				problems = append(problems, fmt.Sprintf("%s: overlays/%s/ is selected by no Environment of %s/, so no render checks it", root, name, controlDir))
			}
		}
		for _, r := range append([]string{noEnv}, envs...) {
			renders++
			o, err := cliPath{s}.validate(context.Background(), renderRun(bundle, envFile, r))
			if err != nil {
				t.Fatalf("%s [environment=%s]: %v", root, r, err)
			}
			for _, line := range strings.Split(strings.TrimSuffix(string(o.text), "\n"), "\n") {
				if line != "" {
					suffix := ""
					if r != noEnv {
						suffix = " [environment=" + r + "]"
					}
					problems = append(problems, root+": "+line+suffix)
				}
			}
		}
	}
	return problems, documents, renders
}

// TestExampleTree checks the examples/ tree (checkExampleTree): no
// problem, and some documents and renders checked.
func TestExampleTree(t *testing.T) {
	s := mustSuite(t)
	problems, documents, renders := checkExampleTree(t, s, s.path(examplesDir))
	if documents == 0 || renders == 0 {
		t.Errorf("examples/ holds %d documents and %d renders, want some of each", documents, renders)
	}
	for _, p := range problems {
		t.Errorf("examples/: %s", p)
	}
}

// writeFiles writes files (slash paths relative to root).
func writeFiles(t testing.TB, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestCheckExampleTree checks what checkExampleTree reports on an examples
// tree: nothing for a valid tree whose drafts, hidden files and overlay
// patches it must not validate as whole documents; and for each broken
// rule one problem: a schema finding in a document, a parse finding in
// control/, a schema finding in control/, a finding in a render, named
// by its Environment, a warning, and an overlay no Environment selects. A
// file outside every Bundle and control/ that does not parse is left out.
func TestCheckExampleTree(t *testing.T) {
	s := mustSuite(t)
	base := map[string]string{
		"b/ruralz.yaml": "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  listeners:\n" +
			"    - name: http\n      protocol: http\n      port: ${PORT:-8080}\n",
		"b/upstreams.yaml": "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n" +
			"  endpoints:\n    - address: \"orders.shop.svc:8080\"\n",
		"b/routes.yaml": "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: orders\nspec:\n  match:\n    path: {prefix: /orders}\n" +
			"  upstreams:\n    - name: orders\n",
		"b/overlays/env1/patch.yaml": "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  timeout: 2s\n",
		"b/.ruralzignore":            "drafts/\n",
		"b/drafts/x.yaml":            "kind: [broken\n",
		"b/.hidden.yaml":             "kind: [broken\n",
		"control/environments.yaml":  "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: env1\nspec:\n  variables:\n    PORT: \"8081\"\n",
		"control/clusters.yaml":      "apiVersion: ruralz/v1alpha1\nkind: Cluster\nmetadata:\n  name: c1\nspec:\n  environment: env1\n",
		"other/compose.yaml":         "x: &anchor 1\n",
	}
	for _, tc := range []struct {
		name    string
		changes map[string]string
		want    []string
	}{
		{"valid", nil, nil},
		{
			"document schema",
			map[string]string{"b/routes.yaml": base["b/routes.yaml"] + "  timout: 1s\n"},
			[]string{
				`b: routes.yaml:10:3 error RZ-CFG-006 Route/orders spec.timout: unknown field "timout" (did you mean "timeout"?)`,
				`b: routes.yaml:10:3 error RZ-CFG-006 Route/orders spec.timout: unknown field "timout" (did you mean "timeout"?)`,
				`b: routes.yaml:10:3 error RZ-CFG-006 Route/orders spec.timout: unknown field "timout" (did you mean "timeout"?) [environment=env1]`,
			},
		},
		{
			"control parse",
			map[string]string{"control/clusters.yaml": "a: &x 1\n"},
			[]string{"control/clusters.yaml:1:4 error RZ-CFG-003: anchors are not allowed (&x)"},
		},
		{
			"control schema",
			map[string]string{"control/clusters.yaml": base["control/clusters.yaml"] + "  regoin: eu\n"},
			[]string{`control/clusters.yaml:7:3 error RZ-CFG-006 Cluster/c1 spec.regoin: unknown field "regoin" (did you mean "region"?)`},
		},
		{
			"render",
			map[string]string{"b/routes.yaml": strings.Replace(base["b/routes.yaml"], "- name: orders", "- name: ordrs", 1)},
			[]string{
				`b: routes.yaml:9:13 error RZ-CFG-009 Route/orders spec.upstreams[name=ordrs].name: Upstream "ordrs" not found (did you mean "orders"?)`,
				`b: routes.yaml:9:13 error RZ-CFG-009 Route/orders spec.upstreams[name=ordrs].name: Upstream "ordrs" not found (did you mean "orders"?) [environment=env1]`,
			},
		},
		{
			"warning",
			map[string]string{"b/routes.yaml": strings.Replace(base["b/routes.yaml"], "prefix: /orders", `prefix: "/${API_TOKEN:-x}"`, 1)},
			[]string{
				`b: routes.yaml:7:20 warning RZ-CFG-013 Route/orders spec.match.path.prefix: variable "API_TOKEN" looks like a secret (use a secretRef with provider: env instead of substitution)`,
				`b: routes.yaml:7:20 warning RZ-CFG-013 Route/orders spec.match.path.prefix: variable "API_TOKEN" looks like a secret (use a secretRef with provider: env instead of substitution) [environment=env1]`,
			},
		},
		{
			"overlay no Environment selects",
			map[string]string{"b/overlays/qa/patch.yaml": base["b/overlays/env1/patch.yaml"]},
			[]string{"b: overlays/qa/ is selected by no Environment of control/, so no render checks it"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := maps.Clone(base)
			maps.Copy(files, tc.changes)
			dir := t.TempDir()
			writeFiles(t, dir, files)
			problems, documents, renders := checkExampleTree(t, s, dir)
			if renders != 2 || documents == 0 {
				t.Errorf("checked %d documents and %d renders, want some and 2", documents, renders)
			}
			if !slices.Equal(problems, tc.want) {
				t.Errorf("problems =\n%s\nwant\n%s", strings.Join(problems, "\n"), strings.Join(tc.want, "\n"))
			}
		})
	}
}

// exampleRenders renders the Bundle x of examples/ without --env and for
// each Environment of examples/control/ through the CLI's pipeline, by
// render name, failing t on any diagnostic.
func exampleRenders(t testing.TB, s *suite, x string) map[string]*pipeline.Result {
	t.Helper()
	envFile := s.path(examplesDir + "/" + controlDir + "/" + environmentsName)
	out := map[string]*pipeline.Result{}
	for _, r := range s.renders(t, envFile) {
		req := requestOf(renderRun(s.path(examplesDir+"/"+x), envFile, r))
		res, err := s.cli.Run(context.Background(), req)
		if err != nil || len(res.Diagnostics) > 0 || res.Validated == nil {
			t.Fatalf("examples/%s, render %s: %v\n%s", x, r, err, texts(res.Diagnostics))
		}
		out[r] = res
	}
	return out
}

// nodeAt returns the text of the node at p in the resource kind/name of
// res, or "<absent>" when there is none.
func nodeAt(t testing.TB, res *pipeline.Result, kind v1alpha1.Kind, name string, p ...diag.PathElem) string {
	t.Helper()
	r, ok := res.Validated.Bundle.Get(hub.ID{Kind: kind, Name: name})
	if !ok {
		t.Fatalf("no %s/%s", kind, name)
	}
	n, ok := r.Tree.At(p)
	if !ok {
		return "<absent>"
	}
	return n.Text
}

// distinctRevisions fails t when two renders hold one Revision: each
// Environment renders its own (configuration model "Environment").
func distinctRevisions(t *testing.T, renders map[string]*pipeline.Result) {
	t.Helper()
	digests := map[string]string{}
	for _, r := range sortedKeys(renders) {
		d := renders[r].Validated.Revision.Digest.String()
		if other, dup := digests[d]; dup {
			t.Errorf("renders %q and %q hold one Revision %s", other, r, d)
		}
		digests[d] = r
	}
}

// attached reports whether some Route's effective chain runs Policy name.
func attached(res *pipeline.Result, name string) bool {
	for _, c := range res.Validated.Chains {
		for _, es := range c.Client {
			if slices.ContainsFunc(es, func(e hub.Entry) bool { return e.Policy == name }) {
				return true
			}
		}
		for _, l := range c.Legs {
			for _, es := range l.Phases {
				if slices.ContainsFunc(es, func(e hub.Entry) bool { return e.Policy == name }) {
					return true
				}
			}
		}
	}
	return false
}

// TestShopBundleRenders checks what each render of the example Bundle
// holds, as its comments state (configuration model "Complete annotated
// example Bundle"): the OTLP endpoint is the Environment's variable, and
// without --env the default that "keeps local runs working";
// overlays/prod/scale.yaml replaces the retry attempts and the breaker's
// consecutiveFailures and keeps the other retry and breaker fields, and
// merges the secondary candidate in place ("position kept");
// overlays/staging/models.yaml "removes the fallback candidate in
// staging"; semantic-cache-support stays attached nowhere; and each
// Environment renders its own Revision.
func TestShopBundleRenders(t *testing.T) {
	s := mustSuite(t)
	renders := exampleRenders(t, s, shopBundle)
	if got := sortedKeys(renders); !slices.Equal(got, []string{noEnv, "prod", "staging"}) {
		t.Fatalf("renders = %q, want without --env, prod and staging", got)
	}
	f, k := diag.Field, diag.Keyed
	for _, tc := range []struct {
		env, endpoint, attempts, failures, secondaryWhen string
		candidates                                       []string
	}{
		{noEnv, "https://otel-collector:4317", "2", "5", "ai.estimatedInputTokens < 100000", []string{"primary", "secondary"}},
		{"staging", "https://otel-collector.observability:4317", "2", "5", "<absent>", []string{"primary"}},
		{"prod", "https://otel-collector.observability:4317", "3", "10", "ai.estimatedInputTokens < 50000", []string{"primary", "secondary"}},
	} {
		res := renders[tc.env]
		checkOrdersRender(t, res, tc.env, tc.endpoint, tc.attempts, tc.failures)
		if got := nodeAt(t, res, v1alpha1.KindAIModel, "support-chat", f("spec"), f("candidates"), k("name", "secondary"), f("when")); got != tc.secondaryWhen {
			t.Errorf("render %s: secondary candidate when = %q, want %q", tc.env, got, tc.secondaryWhen)
		}
		model, _ := res.Validated.Bundle.Get(hub.ID{Kind: v1alpha1.KindAIModel, Name: "support-chat"})
		var names []string
		if list, ok := model.Tree.At(diag.Path{f("spec"), f("candidates")}); ok {
			for _, c := range list.Items {
				if n, ok := c.Get("name"); ok {
					names = append(names, n.Text)
				}
			}
		}
		if !slices.Equal(names, tc.candidates) {
			t.Errorf("render %s: AIModel support-chat candidates = %q, want %q", tc.env, names, tc.candidates)
		}
		if _, ok := res.Validated.Bundle.Policy("semantic-cache-support"); !ok {
			t.Errorf("render %s: no Policy semantic-cache-support", tc.env)
		}
		if attached(res, "semantic-cache-support") {
			t.Errorf("render %s: semantic-cache-support is attached, want nowhere", tc.env)
		}
	}
	distinctRevisions(t, renders)
}

// checkOrdersRender checks the Gateway edge OTLP endpoint and the Upstream
// orders retry and breaker fields of one render of the shop-bundle or the
// shop-rest-bundle, which share both: the endpoint and the fields the prod
// overlay replaces take the values given, and every other field shown
// keeps its base value.
func checkOrdersRender(t *testing.T, res *pipeline.Result, env, endpoint, attempts, failures string) {
	t.Helper()
	f := diag.Field
	for _, c := range []struct{ got, want, what string }{
		{nodeAt(t, res, v1alpha1.KindGateway, "edge", f("spec"), f("telemetry"), f("otlp"), f("endpoint")), endpoint, "Gateway edge OTLP endpoint"},
		{nodeAt(t, res, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("attempts")), attempts, "Upstream orders retries.attempts"},
		{nodeAt(t, res, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("perTryTimeout")), "1s", "Upstream orders retries.perTryTimeout"},
		{
			nodeAt(t, res, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("retryOn")),
			`error != null ? error.kind in ["connect", "reset"] : response.status == 503`, "Upstream orders retries.retryOn",
		},
		{nodeAt(t, res, v1alpha1.KindUpstream, "orders", f("spec"), f("circuitBreaker"), f("consecutiveFailures")), failures, "Upstream orders circuitBreaker.consecutiveFailures"},
		{nodeAt(t, res, v1alpha1.KindUpstream, "orders", f("spec"), f("circuitBreaker"), f("openDuration")), "30s", "Upstream orders circuitBreaker.openDuration"},
	} {
		if c.got != c.want {
			t.Errorf("render %s: %s = %q, want %q", env, c.what, c.got, c.want)
		}
	}
}

// TestShopRestBundleRenders checks each render of the REST example Bundle
// (configuration model "REST example Bundle"): the OTLP endpoint is the
// Environment's variable or the default without --env, prod applies the
// orders patch and keeps the other retry and breaker fields, staging has
// no overlay, and each Environment renders its own Revision.
func TestShopRestBundleRenders(t *testing.T) {
	s := mustSuite(t)
	renders := exampleRenders(t, s, shopRestBundle)
	if got := sortedKeys(renders); !slices.Equal(got, []string{noEnv, "prod", "staging"}) {
		t.Fatalf("renders = %q, want without --env, prod and staging", got)
	}
	for _, tc := range []struct{ env, endpoint, attempts, failures string }{
		{noEnv, "https://otel-collector:4317", "2", "5"},
		{"staging", "https://otel-collector.observability:4317", "2", "5"},
		{"prod", "https://otel-collector.observability:4317", "3", "10"},
	} {
		checkOrdersRender(t, renders[tc.env], tc.env, tc.endpoint, tc.attempts, tc.failures)
	}
	distinctRevisions(t, renders)
}

// chainRows returns the Phase, Leg, Policy and From of the rows render
// --effective prints for a Route (pipeline.Pipeline.Effective).
func chainRows(t testing.TB, s *suite, res *pipeline.Result, route string) [][4]string {
	t.Helper()
	rows, err := s.cli.Effective(res, route)
	if err != nil {
		t.Fatalf("effective chain of %s: %v", route, err)
	}
	var out [][4]string
	for _, r := range rows {
		out = append(out, [4]string{r.Phase, r.Leg, r.Policy, r.From})
	}
	return out
}

// TestShopBundleWorkedExample checks the configuration model's "Worked
// example" against examples/shop-bundle: the effective chain of
// orders-summary is the table's Phase, Leg, Policy and From columns, row
// for row, over the legs orders and inventory; on support-chat
// apikey-partner replaces jwt-default in slot auth; cart-grpc excludes
// cors-default.
func TestShopBundleWorkedExample(t *testing.T) {
	s := mustSuite(t)
	res := exampleRenders(t, s, shopBundle)[noEnv]
	if got, want := chainRows(t, s, res, "orders-summary"), chainTable(t, s, configModelDoc, "### Worked example"); !slices.Equal(got, want) {
		t.Errorf("orders-summary rows =\n%q\nwant the document's\n%q", got, want)
	}
	var legs []string
	for _, leg := range res.Validated.Chains["orders-summary"].Legs {
		legs = append(legs, leg.Upstream)
	}
	if !slices.Equal(legs, []string{"orders", "inventory"}) {
		t.Errorf("orders-summary legs = %q, want [orders inventory]", legs)
	}
	for _, tc := range []struct {
		route string
		want  []hub.Removed
	}{
		{"support-chat", []hub.Removed{{Policy: "jwt-default", Slot: "auth", By: "apikey-partner", Reason: hub.Replaced}}},
		{"cart-grpc", []hub.Removed{{Policy: "cors-default", Slot: "cors", Reason: hub.Excluded}}},
	} {
		c := res.Validated.Chains[tc.route]
		if c == nil {
			t.Errorf("no chain for %s", tc.route)
			continue
		}
		if !slices.Equal(c.Removed, tc.want) {
			t.Errorf("%s removed = %+v, want %+v", tc.route, c.Removed, tc.want)
		}
	}
}

// TestShopRestBundleChains checks the effective chains the REST example
// Bundle section states: orders-summary runs the Worked example chain
// without geo-block-default, and partner-orders runs the section's table,
// row for row.
func TestShopRestBundleChains(t *testing.T) {
	s := mustSuite(t)
	res := exampleRenders(t, s, shopRestBundle)[noEnv]
	var want [][4]string
	for _, r := range chainTable(t, s, configModelDoc, "### Worked example") {
		if r[2] != "geo-block-default" {
			want = append(want, r)
		}
	}
	if got := chainRows(t, s, res, "orders-summary"); !slices.Equal(got, want) {
		t.Errorf("orders-summary rows =\n%q\nwant the Worked example without geo-block-default\n%q", got, want)
	}
	if got, want := chainRows(t, s, res, "partner-orders"), chainTable(t, s, configModelDoc, "## REST example Bundle"); !slices.Equal(got, want) {
		t.Errorf("partner-orders rows =\n%q\nwant the document's\n%q", got, want)
	}
}

// examplesWith returns a copy of examples/ in a temporary directory with
// each change applied: in file, old replaced once by repl, or the file
// removed when old and repl are both empty.
func examplesWith(t *testing.T, s *suite, changes ...[3]string) string {
	t.Helper()
	files := readTree(t, s.path(examplesDir))
	for _, c := range changes {
		data, ok := files[c[0]]
		switch {
		case !ok:
			t.Fatalf("examples/%s does not exist", c[0])
		case c[1] == "" && c[2] == "":
			delete(files, c[0])
		case !bytes.Contains(data, []byte(c[1])):
			t.Fatalf("examples/%s does not hold %q", c[0], c[1])
		default:
			files[c[0]] = bytes.Replace(data, []byte(c[1]), []byte(c[2]), 1)
		}
	}
	dir := t.TempDir()
	for name, data := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestShopBundleIgnoreFile checks the shop-bundle's .ruralzignore
// (configuration model "Complete annotated example Bundle"): discovery
// leaves out the draft Route and the sample response, which both exist,
// and each pattern is what keeps its file out: without the pattern,
// discovery reads the file and the render fails there.
func TestShopBundleIgnoreFile(t *testing.T) {
	s := mustSuite(t)
	discovered := func(dir string) []string {
		t.Helper()
		b, err := s.cli.Parse(context.Background(), loaderSource(dir))
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, f := range b.Files() {
			out = append(out, f.Path)
		}
		return out
	}
	base := discovered(s.path(examplesDir + "/" + shopBundle))
	for _, tc := range []struct{ pattern, file string }{
		{"*.draft.yaml\n", "routes/orders-history.draft.yaml"},
		{"samples/\n", "samples/orders-summary.json"},
	} {
		if _, err := os.Stat(s.path(examplesDir + "/" + shopBundle + "/" + tc.file)); err != nil {
			t.Errorf("examples/%s/%s: %v", shopBundle, tc.file, err)
		}
		if slices.Contains(base, tc.file) {
			t.Errorf("discovery reads %s", tc.file)
		}
		dir := filepath.Join(examplesWith(t, s, [3]string{shopBundle + "/.ruralzignore", tc.pattern, ""}), shopBundle)
		if !slices.Contains(discovered(dir), tc.file) {
			t.Errorf("without %q, discovery does not read %s", tc.pattern, tc.file)
		}
		res, err := s.cli.Run(context.Background(), requestOf(run{bundle: dir}))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.ContainsFunc(res.Diagnostics, func(d diag.Diagnostic) bool { return d.File == tc.file && d.Severity == diag.SeverityError }) {
			t.Errorf("without %q, no error at %s; diagnostics:\n%s", tc.pattern, tc.file, texts(res.Diagnostics))
		}
	}
}

// secretProblems lists the secretRef uses of a Revision a Node cannot
// resolve in M1 (configuration model "secretRef"), each as "<Kind>/<name>
// <path>: <reason>": a provider other than env and file (Planned (M2)), an
// env name other than RURALZ_STATE_STORE_URL and RURALZ_SECRET_*, or a file
// name outside the default RURALZ_SECRET_ROOT, /etc/ruralz.
func secretProblems(res *pipeline.Result) []string {
	var out []string
	for _, u := range res.Validated.Secrets {
		at := fmt.Sprintf("%s/%s %s", u.Resource.Kind, u.Resource.Name, u.Path)
		switch ref := u.Ref; {
		case ref.Provider == v1alpha1.SecretProviderEnv:
			if ref.Name != "RURALZ_STATE_STORE_URL" && !strings.HasPrefix(ref.Name, "RURALZ_SECRET_") {
				out = append(out, fmt.Sprintf("%s: env name %s is not RURALZ_STATE_STORE_URL or RURALZ_SECRET_*", at, ref.Name))
			}
		case ref.Provider == v1alpha1.SecretProviderFile:
			if !strings.HasPrefix(ref.Name, "/etc/ruralz/") {
				out = append(out, fmt.Sprintf("%s: file %s is outside /etc/ruralz", at, ref.Name))
			}
		default:
			out = append(out, fmt.Sprintf("%s: provider %s is Planned (M2)", at, ref.Provider))
		}
	}
	slices.Sort(out)
	return out
}

// TestExampleBundlesServedInM1 checks that an M1 Node serves the REST
// example Bundle and the quickstart Bundle in every render: the Node's
// serve check (compile.Serve) refuses nothing, and every secretRef is one
// a Node resolves in M1 (secretProblems). Both find every later-milestone
// feature and secretRef of the shop-bundle, so the check is not vacuous.
func TestExampleBundlesServedInM1(t *testing.T) {
	s := mustSuite(t)
	for _, x := range []string{shopRestBundle, quickstartBundle} {
		renders := exampleRenders(t, s, x)
		for _, r := range sortedKeys(renders) {
			res := renders[r]
			ds, err := compile.Serve(s.node, res.Validated, res.Files)
			if err != nil {
				t.Fatal(err)
			}
			if len(ds) > 0 {
				t.Errorf("%s, render %s: an M1 Node refuses\n%s", x, r, texts(ds))
			}
			if ps := secretProblems(res); len(ps) > 0 {
				t.Errorf("%s, render %s: an M1 Node cannot resolve\n%s", x, r, strings.Join(ps, "\n"))
			}
		}
	}
	shop := exampleRenders(t, s, shopBundle)[noEnv]
	ds, err := compile.Serve(s.node, shop.Validated, shop.Files)
	if err != nil {
		t.Fatal(err)
	}
	wantServe := []string{
		`ai/models.yaml:2:7 error RZ-CFG-040 AIModel/support-chat kind: kind "AIModel" is not served by this release (Planned (M3))`,
		`ai/providers.yaml:2:7 error RZ-CFG-040 AIProvider/anthropic-prod kind: kind "AIProvider" is not served by this release (Planned (M3))`,
		`ai/providers.yaml:21:7 error RZ-CFG-040 AIProvider/openai-prod kind: kind "AIProvider" is not served by this release (Planned (M3))`,
		`plugins/geo-block.yaml:2:7 error RZ-CFG-040 Plugin/geo-block kind: kind "Plugin" is not served by this release (Planned (M2))`,
		`plugins/geo-block.yaml:25:9 error RZ-CFG-040 Policy/geo-block-default spec.type: Policy type "plugin" is not served by this release (Planned (M2))`,
		`policies/common.yaml:65:9 error RZ-CFG-040 Policy/token-budget-partner spec.type: Policy type "ai.token-budget" is not served by this release (Planned (M3))`,
		`policies/common.yaml:75:9 error RZ-CFG-040 Policy/semantic-cache-support spec.type: Policy type "ai.semantic-cache" is not served by this release (Planned (M3))`,
		`routes/cart-grpc.yaml:9:7 error RZ-CFG-040 Route/cart-grpc spec.match.grpc: field "grpc" is not served by this release (Planned (M3))`,
		`ruralz.yaml:15:14 error RZ-CFG-040 Gateway/edge spec.listeners[name=https].http3: field "http3" is not served by this release (Planned (M3))`,
		`upstreams/services.yaml:8:11 error RZ-CFG-040 Upstream/orders spec.discovery.type: type "kubernetes" is not served by this release (Planned (M2))`,
		`upstreams/services.yaml:42:13 error RZ-CFG-040 Upstream/cart spec.protocol: protocol "grpc" is not served by this release (Planned (M3))`,
		`upstreams/services.yaml:55:13 error RZ-CFG-040 Upstream/llm spec.protocol: protocol "ai" is not served by this release (Planned (M3))`,
	}
	if got := strings.Split(strings.TrimSuffix(texts(ds), "\n"), "\n"); !slices.Equal(got, wantServe) {
		t.Errorf("the serve check of the shop-bundle =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantServe, "\n"))
	}
	wantSecrets := []string{
		"AIProvider/anthropic-prod spec.credentials.apiKey: provider kubernetes is Planned (M2)",
		"AIProvider/openai-prod spec.credentials.apiKey: provider kubernetes is Planned (M2)",
		"Policy/upstream-oauth spec.config.clientSecret: provider kubernetes is Planned (M2)",
	}
	if got := secretProblems(shop); !slices.Equal(got, wantSecrets) {
		t.Errorf("secretProblems(shop-bundle) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantSecrets, "\n"))
	}
}

// TestQuickstartBundle checks what quickstart.sh relies on in the
// quickstart Bundle (Deployment topologies "Quickstart"): Route hello
// takes every path to the one Endpoint of Upstream hello, 127.0.0.1:9000,
// and quickstart-header acts on every response it returns, so a response
// carrying x-quickstart came through the Filter Chain.
func TestQuickstartBundle(t *testing.T) {
	s := mustSuite(t)
	res := exampleRenders(t, s, quickstartBundle)[noEnv]
	f := diag.Field
	if got := nodeAt(t, res, v1alpha1.KindRoute, "hello", f("spec"), f("match"), f("path"), f("prefix")); got != "/" {
		t.Errorf("Route hello path prefix = %q, want /", got)
	}
	up, ok := res.Validated.Bundle.Upstream("hello")
	if !ok || len(up.Spec.Endpoints) != 1 || up.Spec.Endpoints[0].Address != "127.0.0.1:9000" {
		t.Errorf("Upstream hello = %+v, want the one Endpoint 127.0.0.1:9000", up)
	}
	c := res.Validated.Chains["hello"]
	if c == nil {
		t.Fatal("no chain for hello")
	}
	var names []string
	for _, e := range c.Client[phase.OnResponse] {
		names = append(names, e.Policy)
	}
	if !slices.Equal(names, []string{"quickstart-header"}) {
		t.Errorf("Route hello onResponse = %q, want [quickstart-header]", names)
	}
}

// TestShopBundleDiagnostics checks the configuration model's "Diagnostics
// and source map" against examples/shop-bundle: with the mistake each
// documented diagnostic describes (the stock step's upstream misspelled
// inventroy; cart-grpc excluding ratelimit-global, which "fails with
// RZ-CFG-019"), the render without --env reports exactly that diagnostic,
// in its text form, and the RZ-CFG-009 one also in its JSON form.
func TestShopBundleDiagnostics(t *testing.T) {
	s := mustSuite(t)
	var lines []string
	if blocks := blocksOf(t, s, configModelDoc, "### Diagnostics and source map", "text"); len(blocks) == 1 {
		lines = strings.Split(strings.TrimSuffix(blocks[0], "\n"), "\n")
	}
	jsonForm := ""
	if blocks := blocksOf(t, s, configModelDoc, "### Diagnostics and source map", "json"); len(blocks) == 1 {
		jsonForm = blocks[0]
	}
	mistakes := [][3]string{
		{shopBundle + "/routes/orders-summary.yaml", "upstream: inventory\n", "upstream: inventroy\n"},
		{shopBundle + "/routes/cart-grpc.yaml", "- name: cors-default", "- name: ratelimit-global"},
	}
	if len(lines) != len(mistakes) || jsonForm == "" {
		t.Fatalf("the section has %d text lines and JSON %q, want %d lines and one JSON object", len(lines), jsonForm, len(mistakes))
	}
	for i, m := range mistakes {
		dir := filepath.Join(examplesWith(t, s, m), shopBundle)
		o, err := cliPath{s}.validate(context.Background(), run{bundle: dir})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(string(o.text)); got != lines[i] {
			t.Errorf("with %s in %s, diagnostics =\n%s\nwant the document's\n%s", m[2], m[0], got, lines[i])
		}
		if i != 0 {
			continue
		}
		var got []any
		var want any
		if err := json.Unmarshal(o.json, &got); err != nil || len(got) != 1 {
			t.Fatalf("JSON form %s: %v", o.json, err)
		}
		if err := json.Unmarshal([]byte(jsonForm), &want); err != nil {
			t.Fatalf("the document's JSON: %v", err)
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Errorf("JSON form = %s\nwant the document's %s", o.json, jsonForm)
		}
	}
}
