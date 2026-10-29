// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The documents that define the files of examples/.
const (
	// configModelPath is the configuration model: the example Bundle and
	// the REST example Bundle.
	configModelPath = "../../../docs/architecture/02-configuration-model.md"
	// topologiesPath is Deployment topologies: the quickstart Bundle.
	topologiesPath = "../../../docs/operations/01-deployment-topologies.md"
)

// The Bundles of examples/, each a directory beside control/.
const (
	// shopBundle is the example Bundle: "The shop-bundle from Bundle
	// layout and merge" (CM "Complete annotated example Bundle").
	shopBundle = "shop-bundle"
	// shopRestBundle is the part of it an M1 Node serves (CM "REST
	// example Bundle").
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
		{configModelPath, "## Complete annotated example Bundle", shopBundle},
		{configModelPath, "## REST example Bundle", shopRestBundle},
		{topologiesPath, "#### Quickstart", quickstartBundle},
	}
}

// docSection returns the text below the heading line heading of the
// document at p, such as "### Worked example", up to the next heading of
// its level or a higher one; lines inside fenced code blocks are never
// headings.
func docSection(t *testing.T, p, heading string) string {
	t.Helper()
	data, err := os.ReadFile(p) //nolint:gosec // G304: a document of the repository under test.
	if err != nil {
		t.Fatal(err)
	}
	level := strings.Index(heading, " ")
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if l != heading {
			continue
		}
		var out []string
		fence := false
		for _, l := range lines[i+1:] {
			if strings.HasPrefix(l, "```") {
				fence = !fence
			}
			if n := len(l) - len(strings.TrimLeft(l, "#")); !fence && n > 0 && n <= level && strings.HasPrefix(l[n:], " ") {
				break
			}
			out = append(out, l)
		}
		return strings.Join(out, "\n")
	}
	t.Fatalf("%s has no heading %q", p, heading)
	return ""
}

// fenced is one fenced code block: its info string, its lines, each ending
// in a newline, and its caption, the last non-blank line before it.
type fenced struct{ caption, lang, body string }

// fencedBlocks returns the fenced code blocks of text in order.
func fencedBlocks(text string) []fenced {
	var out []fenced
	var cur *fenced
	var body strings.Builder
	caption := ""
	for _, l := range strings.Split(text, "\n") {
		switch {
		case cur == nil && strings.HasPrefix(l, "```"):
			cur = &fenced{caption: caption, lang: strings.TrimPrefix(l, "```")}
			body.Reset()
		case cur != nil && strings.HasPrefix(l, "```"):
			cur.body = body.String()
			out = append(out, *cur)
			cur = nil
		case cur != nil:
			body.WriteString(l + "\n")
		case strings.TrimSpace(l) != "":
			caption = l
		}
	}
	return out
}

// isExampleFile reports whether the documents show a file of this name in
// full: a configuration file discovery reads (profile.FormatOf) or a
// .ruralzignore.
func isExampleFile(name string) bool {
	base := path.Base(name)
	_, ok := profile.FormatOf(base)
	return ok || base == ".ruralzignore"
}

// captionFiles returns the files a caption names: the backticked file
// names before its first comma, such as ai/providers.yaml and
// ai/models.yaml in "`ai/providers.yaml` and `ai/models.yaml`:". A name
// after the comma, as in "`routes/x.draft.yaml`, which the `*.draft.yaml`
// pattern keeps out", belongs to the caption's prose.
func captionFiles(caption string) []string {
	head, _, _ := strings.Cut(caption, ",")
	var out []string
	parts := strings.Split(head, "`")
	for i := 1; i < len(parts); i += 2 {
		if isExampleFile(parts[i]) {
			out = append(out, parts[i])
		}
	}
	return out
}

// exampleRel returns the examples/ path of a file a caption names: a
// control/ file sits beside the Bundles, a name under examples/ is given
// from the repository root, and any other name is relative to dir.
func exampleRel(dir, name string) string {
	switch {
	case strings.HasPrefix(name, controlDir+"/"):
		return name
	case strings.HasPrefix(name, "examples/"):
		return strings.TrimPrefix(name, "examples/")
	default:
		return dir + "/" + name
	}
}

// firstDiff describes the first line where got and want differ.
func firstDiff(got, want string) string {
	g, w := strings.Split(got, "\n"), strings.Split(want, "\n")
	for i := range max(len(g), len(w)) {
		var gl, wl string
		if i < len(g) {
			gl = g[i]
		}
		if i < len(w) {
			wl = w[i]
		}
		if gl != wl || i >= len(g) || i >= len(w) {
			return fmt.Sprintf("line %d: examples/ %q, document %q", i+1, gl, wl)
		}
	}
	return "no difference"
}

// TestExamplesMatchDocument checks that examples/ holds what the documents
// show, byte for byte (exampleSources): each block of a section whose
// caption names files is those files joined by "---" lines, every section
// shows at least one file, no file is shown twice, and every configuration
// file and .ruralzignore under examples/ is shown.
func TestExamplesMatchDocument(t *testing.T) {
	shown := map[string]string{}
	for _, src := range exampleSources() {
		blocks := 0
		for _, b := range fencedBlocks(docSection(t, src.doc, src.heading)) {
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
				data, err := os.ReadFile(filepath.Join(examplesRoot, filepath.FromSlash(rel)))
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
	err := fs.WalkDir(os.DirFS(examplesRoot), ".", func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && isExampleFile(p) && shown[p] == "" {
			t.Errorf("examples/%s is in no document section", p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// examplesWith returns the files of examples/ with each change applied: in
// file, old replaced once by repl, or the file removed when old and repl
// are both empty.
func examplesWith(t *testing.T, changes ...[3]string) fstest.MapFS {
	t.Helper()
	fsys := fstest.MapFS{}
	src := os.DirFS(examplesRoot)
	err := fs.WalkDir(src, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(src, p)
		fsys[p] = &fstest.MapFile{Data: data}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		f, ok := fsys[c[0]]
		switch {
		case !ok:
			t.Fatalf("examples/%s does not exist", c[0])
		case c[1] == "" && c[2] == "":
			delete(fsys, c[0])
		case !bytes.Contains(f.Data, []byte(c[1])):
			t.Fatalf("examples/%s does not hold %q", c[0], c[1])
		default:
			f.Data = bytes.Replace(f.Data, []byte(c[1]), []byte(c[2]), 1)
		}
	}
	return fsys
}

// bundleRenders loads the Bundle dir of examples/ without --env and for
// each Environment of examples/control/, by Environment name ("" without
// --env), failing t on any diagnostic.
func bundleRenders(t *testing.T, dir string) map[string]*loaded {
	t.Helper()
	st := mustStages(t)
	fsys := os.DirFS(examplesRoot)
	envs, ds, err := readControl(t.Context(), st, fsys, controlDir)
	if err != nil || len(ds) > 0 {
		t.Fatalf("readControl() = %v, %v", ds, err)
	}
	out := map[string]*loaded{}
	for _, r := range renderOf(envs) {
		l, ds, err := loadBundle(t.Context(), st, fsys, dir, r)
		if err != nil || len(ds) > 0 {
			t.Fatalf("loadBundle(%s, environment %q) = %v, %v", dir, r.env, ds, err)
		}
		out[r.env] = l
	}
	return out
}

// nodeAt returns the text of the node at p in the resource kind/name of l,
// or "<absent>" when there is none.
func nodeAt(t *testing.T, l *loaded, kind v1alpha1.Kind, name string, p ...diag.PathElem) string {
	t.Helper()
	r, ok := l.bundle.Get(hub.ID{Kind: kind, Name: name})
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
// Environment renders its own (CM "Environment").
func distinctRevisions(t *testing.T, renders map[string]*loaded) {
	t.Helper()
	digests := map[revision.Digest]string{}
	for _, env := range slices.Sorted(maps.Keys(renders)) {
		l := renders[env]
		if l.rev.Digest != revision.Sum(l.rev.Content) || l.rev.Digest.IsZero() {
			t.Errorf("environment %q: digest %s does not hash the content", env, l.rev.Digest)
		}
		if other, dup := digests[l.rev.Digest]; dup {
			t.Errorf("environments %q and %q render one Revision %s", other, env, l.rev.Digest)
		}
		digests[l.rev.Digest] = env
	}
}

// TestShopBundleRenders checks what each render of the example Bundle
// holds, as its comments state (CM "Complete annotated example Bundle"):
// the OTLP endpoint is the Environment's variable, and without --env the
// default that "keeps local runs working"; overlays/prod/scale.yaml
// replaces the retry attempts and the breaker's consecutiveFailures and
// keeps the other retry and breaker fields ("scalar replaced; other retry
// fields kept"), and merges the secondary candidate in place ("position
// kept"); overlays/staging/models.yaml "removes the fallback candidate in
// staging"; semantic-cache-support stays attached nowhere; and each
// Environment renders its own Revision.
func TestShopBundleRenders(t *testing.T) {
	renders := bundleRenders(t, shopBundle)
	if got := slices.Sorted(maps.Keys(renders)); !slices.Equal(got, []string{"", "prod", "staging"}) {
		t.Fatalf("renders = %q, want without --env, prod and staging", got)
	}
	f, k := diag.Field, diag.Keyed
	for _, tc := range []struct {
		env, endpoint, attempts, failures, secondaryWhen string
		candidates                                       []string
	}{
		{"", "https://otel-collector:4317", "2", "5", "ai.estimatedInputTokens < 100000", []string{"primary", "secondary"}},
		{"staging", "https://otel-collector.observability:4317", "2", "5", "<absent>", []string{"primary"}},
		{"prod", "https://otel-collector.observability:4317", "3", "10", "ai.estimatedInputTokens < 50000", []string{"primary", "secondary"}},
	} {
		l := renders[tc.env]
		checkOrdersRender(t, l, tc.env, tc.endpoint, tc.attempts, tc.failures)
		if got := nodeAt(t, l, v1alpha1.KindAIModel, "support-chat", f("spec"), f("candidates"), k("name", "secondary"), f("when")); got != tc.secondaryWhen {
			t.Errorf("environment %q: secondary candidate when = %q, want %q", tc.env, got, tc.secondaryWhen)
		}
		model, _ := l.bundle.Get(hub.ID{Kind: v1alpha1.KindAIModel, Name: "support-chat"})
		var names []string
		if list, ok := model.Tree.At(diag.Path{f("spec"), f("candidates")}); ok {
			for _, c := range list.Items {
				if n, ok := c.Get("name"); ok {
					names = append(names, n.Text)
				}
			}
		}
		if !slices.Equal(names, tc.candidates) {
			t.Errorf("environment %q: AIModel support-chat candidates = %q, want %q", tc.env, names, tc.candidates)
		}
		if _, ok := l.bundle.Policy("semantic-cache-support"); !ok {
			t.Errorf("environment %q: no Policy semantic-cache-support", tc.env)
		}
		if as := attachments(l.chains)["semantic-cache-support"]; len(as) > 0 {
			t.Errorf("environment %q: semantic-cache-support attached at %v, want nowhere", tc.env, as)
		}
	}
	distinctRevisions(t, renders)
}

// checkOrdersRender checks the Gateway edge OTLP endpoint and the Upstream
// orders retry and breaker fields of one render of the shop-bundle or the
// shop-rest-bundle, which share both: the endpoint and the fields the prod
// overlay replaces take the values given, and every other field shown
// keeps its base value.
func checkOrdersRender(t *testing.T, l *loaded, env, endpoint, attempts, failures string) {
	t.Helper()
	f := diag.Field
	for _, c := range []struct{ got, want, what string }{
		{nodeAt(t, l, v1alpha1.KindGateway, "edge", f("spec"), f("telemetry"), f("otlp"), f("endpoint")), endpoint, "Gateway edge OTLP endpoint"},
		{nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("attempts")), attempts, "Upstream orders retries.attempts"},
		{nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("perTryTimeout")), "1s", "Upstream orders retries.perTryTimeout"},
		{
			nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("retries"), f("retryOn")),
			`error != null ? error.kind in ["connect", "reset"] : response.status == 503`, "Upstream orders retries.retryOn",
		},
		{nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("circuitBreaker"), f("consecutiveFailures")), failures, "Upstream orders circuitBreaker.consecutiveFailures"},
		{nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("circuitBreaker"), f("openDuration")), "30s", "Upstream orders circuitBreaker.openDuration"},
	} {
		if c.got != c.want {
			t.Errorf("environment %q: %s = %q, want %q", env, c.what, c.got, c.want)
		}
	}
}

// TestShopRestBundleRenders checks each render of the REST example Bundle
// (CM "REST example Bundle"): the OTLP endpoint is the Environment's
// variable or the default without --env, prod applies the orders patch and
// keeps the other retry and breaker fields, staging has no overlay, and
// each Environment renders its own Revision.
func TestShopRestBundleRenders(t *testing.T) {
	renders := bundleRenders(t, shopRestBundle)
	if got := slices.Sorted(maps.Keys(renders)); !slices.Equal(got, []string{"", "prod", "staging"}) {
		t.Fatalf("renders = %q, want without --env, prod and staging", got)
	}
	for _, tc := range []struct{ env, endpoint, attempts, failures string }{
		{"", "https://otel-collector:4317", "2", "5"},
		{"staging", "https://otel-collector.observability:4317", "2", "5"},
		{"prod", "https://otel-collector.observability:4317", "3", "10"},
	} {
		checkOrdersRender(t, renders[tc.env], tc.env, tc.endpoint, tc.attempts, tc.failures)
	}
	distinctRevisions(t, renders)
}

// tableCells returns the cells of a Markdown table row with backticks
// removed; nil for any other line.
func tableCells(line string) []string {
	if !strings.HasPrefix(line, "|") || !strings.HasSuffix(line, "|") {
		return nil
	}
	cells := strings.Split(strings.Trim(line, "|"), "|")
	for i, c := range cells {
		cells[i] = strings.ReplaceAll(strings.TrimSpace(c), "`", "")
	}
	return cells
}

// chainTable returns the Phase, Leg, Policy and From columns of the
// effective-chain table of a document section, a "| Phase | Leg | Policy |
// From | Reason |" table, row by row.
func chainTable(t *testing.T, p, heading string) [][4]string {
	t.Helper()
	var rows [][4]string
	for _, line := range strings.Split(docSection(t, p, heading), "\n") {
		c := tableCells(line)
		if len(c) != 5 || c[0] == "Phase" || strings.HasPrefix(c[0], "---") {
			continue
		}
		rows = append(rows, [4]string{c[0], c[1], c[2], c[3]})
	}
	if len(rows) == 0 {
		t.Fatalf("%q in %s has no effective-chain table", heading, p)
	}
	return rows
}

// chainRows returns the Phase, Leg, Policy and From of the rows render
// --effective prints for a Route of l (precedence.Resolver.Rows).
func chainRows(t *testing.T, l *loaded, route string) [][4]string {
	t.Helper()
	c := l.chains[route]
	if c == nil {
		t.Fatalf("no chain for %s", route)
	}
	var rows [][4]string
	for _, r := range mustStages(t).resolver.Rows(c, l.bundle) {
		rows = append(rows, [4]string{r.Phase, r.Leg, r.Policy, r.From})
	}
	return rows
}

// TestShopBundleWorkedExample checks the configuration model's "Worked
// example" against examples/shop-bundle: the effective chain of
// orders-summary is the table's Phase, Leg, Policy and From columns, row
// for row, over the legs orders and inventory; on support-chat
// apikey-partner replaces jwt-default in slot auth; cart-grpc excludes
// cors-default.
func TestShopBundleWorkedExample(t *testing.T) {
	l := bundleRenders(t, shopBundle)[""]
	if got, want := chainRows(t, l, "orders-summary"), chainTable(t, configModelPath, "### Worked example"); !slices.Equal(got, want) {
		t.Errorf("orders-summary rows =\n%q\nwant the document's\n%q", got, want)
	}
	var legs []string
	for _, leg := range l.chains["orders-summary"].Legs {
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
		c := l.chains[tc.route]
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
// Bundle section states (CM "REST example Bundle"): orders-summary runs
// the Worked example chain without geo-block-default, and partner-orders
// runs the section's table, row for row.
func TestShopRestBundleChains(t *testing.T) {
	l := bundleRenders(t, shopRestBundle)[""]
	var want [][4]string
	for _, r := range chainTable(t, configModelPath, "### Worked example") {
		if r[2] != "geo-block-default" {
			want = append(want, r)
		}
	}
	if got := chainRows(t, l, "orders-summary"); !slices.Equal(got, want) {
		t.Errorf("orders-summary rows =\n%q\nwant the Worked example without geo-block-default\n%q", got, want)
	}
	if got, want := chainRows(t, l, "partner-orders"), chainTable(t, configModelPath, "## REST example Bundle"); !slices.Equal(got, want) {
		t.Errorf("partner-orders rows =\n%q\nwant the document's\n%q", got, want)
	}
}

// TestShopBundleIgnoreFile checks the shop-bundle's .ruralzignore (CM
// "Complete annotated example Bundle"): discovery leaves out the draft
// Route and the sample response, which both exist, and each pattern is
// what keeps its file out: without the pattern, discovery reads the file
// and the render fails there.
func TestShopBundleIgnoreFile(t *testing.T) {
	src := os.DirFS(examplesRoot)
	base, _, err := bundleFiles(src, shopBundle)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ pattern, file string }{
		{"*.draft.yaml\n", "routes/orders-history.draft.yaml"},
		{"samples/\n", "samples/orders-summary.json"},
	} {
		if _, err := fs.Stat(src, path.Join(shopBundle, tc.file)); err != nil {
			t.Errorf("examples/%s/%s: %v", shopBundle, tc.file, err)
		}
		if slices.Contains(base, tc.file) {
			t.Errorf("discovery reads %s", tc.file)
		}
		fsys := examplesWith(t, [3]string{shopBundle + "/.ruralzignore", tc.pattern, ""})
		without, _, err := bundleFiles(fsys, shopBundle)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(without, tc.file) {
			t.Errorf("without %q, discovery does not read %s", tc.pattern, tc.file)
		}
		_, ds, err := loadBundle(t.Context(), mustStages(t), fsys, shopBundle, render{})
		if err != nil {
			t.Fatal(err)
		}
		at := false
		for _, d := range ds {
			at = at || (d.File == tc.file && d.Severity == diag.SeverityError)
		}
		if !at {
			t.Errorf("without %q, no error at %s; diagnostics:\n%s", tc.pattern, tc.file, texts(ds))
		}
	}
}

// laterKinds are the Bundle kinds an M1 Node does not serve, with their
// milestones (CM "Scope of kinds").
func laterKinds() map[v1alpha1.Kind]string {
	return map[v1alpha1.Kind]string{
		v1alpha1.KindPlugin:     "M2",
		v1alpha1.KindAIProvider: "M3",
		v1alpha1.KindAIModel:    "M3",
	}
}

// laterProtocols are the Upstream protocols an M1 Node does not serve,
// with their milestones (CM "Upstream": http alone is Planned (M1)).
func laterProtocols() map[v1alpha1.UpstreamProtocol]string {
	return map[v1alpha1.UpstreamProtocol]string{
		v1alpha1.UpstreamProtocolGRPC:      "M3",
		v1alpha1.UpstreamProtocolGraphQL:   "M3",
		v1alpha1.UpstreamProtocolWebSocket: "M3",
		v1alpha1.UpstreamProtocolAI:        "M3",
		v1alpha1.UpstreamProtocolKafka:     "M4",
		v1alpha1.UpstreamProtocolNATS:      "M4",
		v1alpha1.UpstreamProtocolMQTT:      "M4",
	}
}

// laterFeatures lists what of a rendered Bundle an M1 Node refuses or
// cannot resolve, each as "<Kind>/<name> <path>: <reason>": a kind of
// laterKinds; a Policy type the registry does not serve (RZ-CFG-040,
// registry.Registry.CheckServed); an Upstream protocol of laterProtocols,
// kubernetes discovery (Planned (M2)) or messaging (Planned (M4)); a
// listener with http3 (Planned (M3)); a grpc or graphql match (Planned
// (M3)) or a topic (Planned (M4)); and a secretRef provider other than env
// and file (Planned (M2)), an env name other than RURALZ_STATE_STORE_URL
// and RURALZ_SECRET_*, or a file name outside the default
// RURALZ_SECRET_ROOT, /etc/ruralz, either of which is RZ-CFG-026 on a Node
// (CM "secretRef").
func laterFeatures(st *stages, b *hub.Bundle) []string {
	var out []string
	add := func(r *hub.Resource, p, format string, args ...any) {
		out = append(out, fmt.Sprintf("%s %s: %s", r.ID, p, fmt.Sprintf(format, args...)))
	}
	for _, r := range b.Resources() {
		if m, ok := laterKinds()[r.Kind]; ok {
			add(r, "kind", "%s is Planned (%s)", r.Kind, m)
		}
		switch o := r.Object.(type) {
		case *v1alpha1.Policy:
			if d, unserved := st.registry.CheckServed(o); unserved {
				add(r, "spec.type", "%s", d.Message)
			}
		case *v1alpha1.Upstream:
			if m, ok := laterProtocols()[o.Spec.Protocol]; ok {
				add(r, "spec.protocol", "%s is Planned (%s)", o.Spec.Protocol, m)
			}
			if d := o.Spec.Discovery; d != nil && d.Type == v1alpha1.DiscoveryTypeKubernetes {
				add(r, "spec.discovery.type", "kubernetes is Planned (M2)")
			}
			if o.Spec.Messaging != nil {
				add(r, "spec.messaging", "messaging is Planned (M4)")
			}
		case *v1alpha1.Gateway:
			for _, l := range o.Spec.Listeners {
				if l.HTTP3 != nil && *l.HTTP3 {
					add(r, "spec.listeners[name="+l.Name+"].http3", "HTTP/3 is Planned (M3)")
				}
			}
		case *v1alpha1.Route:
			m := o.Spec.Match
			if m.GRPC != nil {
				add(r, "spec.match.grpc", "gRPC matching is Planned (M3)")
			}
			if m.GraphQL != nil {
				add(r, "spec.match.graphql", "GraphQL matching is Planned (M3)")
			}
			if m.Topic != "" {
				add(r, "spec.match.topic", "topic matching is Planned (M4)")
			}
		default:
		}
		_ = st.hubIndex().Walk(string(r.Kind), r.Tree, func(c *schemaidx.Cursor) error {
			last, _ := c.Last()
			if !c.Info.InSecret || last.Name != "secretRef" || c.Node.Kind != tree.KindMap {
				return nil
			}
			provider, _ := c.Node.Get("provider")
			name, _ := c.Node.Get("name")
			p := c.Path().String()
			switch {
			case provider == nil || name == nil:
			case provider.Text == "env":
				if name.Text != "RURALZ_STATE_STORE_URL" && !strings.HasPrefix(name.Text, "RURALZ_SECRET_") {
					add(r, p, "env name %s is not RURALZ_STATE_STORE_URL or RURALZ_SECRET_*", name.Text)
				}
			case provider.Text == "file":
				if !strings.HasPrefix(name.Text, "/etc/ruralz/") {
					add(r, p, "file %s is outside /etc/ruralz", name.Text)
				}
			default:
				add(r, p, "provider %s is Planned (M2)", provider.Text)
			}
			return nil
		})
	}
	return out
}

// TestExampleBundlesServedInM1 checks that an M1 Node serves the REST
// example Bundle and the quickstart Bundle in every render: laterFeatures
// finds nothing in them. It finds every later-milestone feature of the
// shop-bundle, so the check is not vacuous.
func TestExampleBundlesServedInM1(t *testing.T) {
	st := mustStages(t)
	for _, dir := range []string{shopRestBundle, quickstartBundle} {
		renders := bundleRenders(t, dir)
		for _, env := range slices.Sorted(maps.Keys(renders)) {
			if got := laterFeatures(st, renders[env].bundle); len(got) > 0 {
				t.Errorf("%s, environment %q: an M1 Node refuses\n%s", dir, env, strings.Join(got, "\n"))
			}
		}
	}
	want := []string{
		"Gateway/edge spec.listeners[name=https].http3: HTTP/3 is Planned (M3)",
		"Upstream/cart spec.protocol: grpc is Planned (M3)",
		"Upstream/llm spec.protocol: ai is Planned (M3)",
		"Upstream/orders spec.discovery.type: kubernetes is Planned (M2)",
		"Plugin/geo-block kind: Plugin is Planned (M2)",
		`Policy/geo-block-default spec.type: Policy type "plugin" is not served by this release (Planned (M2))`,
		`Policy/semantic-cache-support spec.type: Policy type "ai.semantic-cache" is not served by this release (Planned (M3))`,
		`Policy/token-budget-partner spec.type: Policy type "ai.token-budget" is not served by this release (Planned (M3))`,
		"Policy/upstream-oauth spec.config.clientSecret.secretRef: provider kubernetes is Planned (M2)",
		"AIProvider/anthropic-prod kind: AIProvider is Planned (M3)",
		"AIProvider/anthropic-prod spec.credentials.apiKey.secretRef: provider kubernetes is Planned (M2)",
		"AIProvider/openai-prod kind: AIProvider is Planned (M3)",
		"AIProvider/openai-prod spec.credentials.apiKey.secretRef: provider kubernetes is Planned (M2)",
		"AIModel/support-chat kind: AIModel is Planned (M3)",
		"Route/cart-grpc spec.match.grpc: gRPC matching is Planned (M3)",
	}
	if got := laterFeatures(st, bundleRenders(t, shopBundle)[""].bundle); !slices.Equal(got, want) {
		t.Errorf("laterFeatures(shop-bundle) =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestQuickstartBundle checks what quickstart.sh relies on in the
// quickstart Bundle (Deployment topologies "Quickstart"): Route hello
// takes every path to the one Endpoint of Upstream hello, 127.0.0.1:9000,
// and quickstart-header acts on every response it returns, so a response
// carrying x-quickstart came through the Filter Chain.
func TestQuickstartBundle(t *testing.T) {
	l := bundleRenders(t, quickstartBundle)[""]
	f := diag.Field
	if got := nodeAt(t, l, v1alpha1.KindRoute, "hello", f("spec"), f("match"), f("path"), f("prefix")); got != "/" {
		t.Errorf("Route hello path prefix = %q, want /", got)
	}
	up, ok := l.bundle.Upstream("hello")
	if !ok || len(up.Spec.Endpoints) != 1 || up.Spec.Endpoints[0].Address != "127.0.0.1:9000" {
		t.Errorf("Upstream hello = %+v, want the one Endpoint 127.0.0.1:9000", up)
	}
	c := l.chains["hello"]
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
	var lines []string
	var jsonForm string
	for _, b := range fencedBlocks(docSection(t, configModelPath, "### Diagnostics and source map")) {
		switch b.lang {
		case "text":
			lines = strings.Split(strings.TrimSuffix(b.body, "\n"), "\n")
		case "json":
			jsonForm = b.body
		default:
		}
	}
	mistakes := [][3]string{
		{shopBundle + "/routes/orders-summary.yaml", "upstream: inventory\n", "upstream: inventroy\n"},
		{shopBundle + "/routes/cart-grpc.yaml", "- name: cors-default", "- name: ratelimit-global"},
	}
	if len(lines) != len(mistakes) || jsonForm == "" {
		t.Fatalf("the section has %d text lines and JSON %q, want %d lines and one JSON object", len(lines), jsonForm, len(mistakes))
	}
	st := mustStages(t)
	for i, m := range mistakes {
		_, ds, err := loadBundle(t.Context(), st, examplesWith(t, m), shopBundle, render{})
		if err != nil {
			t.Fatal(err)
		}
		if got := strings.TrimSpace(texts(ds)); got != lines[i] {
			t.Errorf("with %s in %s, diagnostics =\n%s\nwant the document's\n%s", m[2], m[0], got, lines[i])
		}
		if i != 0 || len(ds) != 1 {
			continue
		}
		var buf bytes.Buffer
		if err := diag.WriteJSON(&buf, ds); err != nil {
			t.Fatal(err)
		}
		var got []any
		var want any
		if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(jsonForm), &want); err != nil {
			t.Fatalf("the document's JSON: %v", err)
		}
		if !reflect.DeepEqual(got[0], want) {
			t.Errorf("JSON form = %s\nwant the document's %s", buf.String(), jsonForm)
		}
	}
}

// TestLoadBundleRenders checks that a render applies its Environment: the
// base tree of TestCheckBundles substitutes PORT, the Environment's
// variable, else its default, and applies overlays/env1/ only for env1,
// and the two renders hold different Revisions.
func TestLoadBundleRenders(t *testing.T) {
	fsys := fstest.MapFS{}
	for name, data := range checkBase() {
		fsys[name] = &fstest.MapFile{Data: []byte(data)}
	}
	st := mustStages(t)
	envs, ds, err := readControl(t.Context(), st, fsys, controlDir)
	if err != nil || len(ds) > 0 || len(envs) != 1 {
		t.Fatalf("readControl() = %+v, %v, %v; want env1 alone", envs, ds, err)
	}
	f := diag.Field
	var digests []revision.Digest
	for _, tc := range []struct {
		r             render
		port, timeout string
	}{
		{render{}, "8080", "<absent>"},
		{envs[0].render(), "8081", "2s"},
	} {
		l, ds, err := loadBundle(t.Context(), st, fsys, "b", tc.r)
		if err != nil || len(ds) > 0 {
			t.Fatalf("loadBundle(environment %q) = %v, %v", tc.r.env, ds, err)
		}
		if got := nodeAt(t, l, v1alpha1.KindGateway, "edge", f("spec"), f("listeners"), diag.Keyed("name", "http"), f("port")); got != tc.port {
			t.Errorf("environment %q: port = %s, want %s", tc.r.env, got, tc.port)
		}
		if got := nodeAt(t, l, v1alpha1.KindUpstream, "orders", f("spec"), f("timeout")); got != tc.timeout {
			t.Errorf("environment %q: Upstream timeout = %s, want %s", tc.r.env, got, tc.timeout)
		}
		digests = append(digests, l.rev.Digest)
	}
	if digests[0] == digests[1] {
		t.Errorf("both renders hold Revision %s", digests[0])
	}
}
