// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
)

// End-to-end cases of the ruralz bundle verbs Planned (M1)
// (docs/engineering/03-testing-and-quality-strategy.md "End-to-end
// tests"), each documented exit code and output form
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"), on the process harness: the REST example Bundle with the e2e
// Environment, served by a Node whose Revision and /config/dump the
// commands' outputs must match.

// invalidBundle is a Bundle whose Route names a missing Upstream.
func invalidBundle(t testing.TB) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("testdata", "bundles", "invalid"))
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

// invalidLine is the text diagnostic of invalidBundle, as validate,
// render and build print it.
const invalidLine = `ruralz.yaml:19:13 error RZ-CFG-009 Route/orders spec.upstreams[name=orders].name: Upstream "orders" not found ("orders" is a Route, not an Upstream) (declared in ruralz.yaml:10:1)`

// checkInvalidJSON checks a JSON diagnostics array holding invalidBundle's
// one finding.
func checkInvalidJSON(t testing.TB, raw []byte) {
	t.Helper()
	var diags []struct {
		Code, Severity, File, Message string
		Line, Column                  int
		Resource                      struct{ Kind, Name string }
	}
	if err := json.Unmarshal(raw, &diags); err != nil {
		t.Fatalf("decode the diagnostics: %v\n%s", err, raw)
	}
	if len(diags) != 1 {
		t.Fatalf("diagnostics %+v, want one", diags)
	}
	d := diags[0]
	if d.Code != "RZ-CFG-009" || d.Severity != "error" || d.File != "ruralz.yaml" || d.Line != 19 || d.Column != 13 ||
		d.Resource.Kind != "Route" || d.Resource.Name != "orders" || d.Message != `Upstream "orders" not found` {
		t.Errorf("diagnostic %+v, want RZ-CFG-009 at ruralz.yaml:19:13 on Route/orders", d)
	}
}

// TestBundleCommands runs the bundle verbs against the REST example
// Bundle served by one Node.
func TestBundleCommands(t *testing.T) {
	s := startShopRest(t, shopRestOptions{nodes: 1})
	t.Run("Validate", func(t *testing.T) { bundleValidate(t, s) })
	t.Run("Render", func(t *testing.T) { bundleRender(t, s) })
	t.Run("Build", func(t *testing.T) { bundleBuild(t, s) })
	t.Run("Diff", func(t *testing.T) { bundleDiff(t, s) })
}

// envFlags are the Bundle flags of the e2e Environment.
func (s *shopRest) envFlags() []string {
	return []string{"--env", shopRestEnv, "--environments", s.envFile}
}

// bundleValidate: a clean Bundle exits 0 with nothing in text and [] in
// JSON, once with --env and once per Environment of --environments
// without it; a failing one exits 1 with its diagnostic in both forms;
// a missing source exits 2.
func bundleValidate(t *testing.T, s *shopRest) {
	for _, flags := range [][]string{s.envFlags(), {"--environments", s.envFile}} {
		r := cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "validate"}, flags...), s.bundleDir)...)
		if r.code != 0 || r.stdout != "" {
			t.Errorf("a clean Bundle: %s\nwant exit 0 and no output", r)
		}
		r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "validate", "--output", "json"}, flags...), s.bundleDir)...)
		if r.code != 0 || strings.TrimSpace(r.stdout) != "[]" {
			t.Errorf("a clean Bundle in JSON: %s\nwant exit 0 and []", r)
		}
	}
	r := cli(t, s.top, cliOptions{}, "bundle", "validate", invalidBundle(t))
	if r.code != 1 || r.stdout != invalidLine+"\n" {
		t.Errorf("a failing Bundle: %s\nwant exit 1 and\n%s", r, invalidLine)
	}
	r = cli(t, s.top, cliOptions{}, "bundle", "validate", "--output", "json", invalidBundle(t))
	if r.code != 1 {
		t.Errorf("a failing Bundle in JSON: %s\nwant exit 1", r)
	}
	checkInvalidJSON(t, []byte(r.stdout))
	missing := filepath.Join(t.TempDir(), "missing")
	if r := cli(t, s.top, cliOptions{}, "bundle", "validate", missing); r.code != 2 || !strings.Contains(r.stderr, "cannot read "+missing) {
		t.Errorf("a missing source: %s\nwant exit 2 naming it", r)
	}
}

// renderedResources returns the kind and name of each resource of a
// rendered Bundle in YAML, in order.
func renderedResources(t testing.TB, yaml []byte) []string {
	t.Helper()
	docs, diags, err := profile.Parse(t.Context(), yaml, 1, "render.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		t.Fatalf("the render does not parse: %v %v", err, diags)
	}
	var out []string
	for _, d := range docs {
		kind, _ := d.Root.Get("kind")
		meta, _ := d.Root.Get("metadata")
		name, _ := meta.Get("name")
		out = append(out, kind.Text+"/"+name.Text)
	}
	return out
}

// bundleRender: the YAML render is the Bundle the Node serves, by its
// Revision; the JSON form holds the same resources in the same order;
// --effective prints the table with and without --route and the rows in
// JSON; --api-version converts into a new directory and reports each
// Environment's Revisions equal, in text and JSON; an invalid Bundle exits
// 1 with its diagnostics, in JSON on standard output too; a flag
// combination outside the documented ones, an unknown Route and an
// unserved apiVersion exit 2.
func bundleRender(t *testing.T, s *shopRest) {
	n := s.nodes[0]
	r := cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "render"}, s.envFlags()...), s.bundleDir)...)
	if r.code != 0 || r.stderr != "" {
		t.Fatalf("render: %s", r)
	}
	if digest, err := revisionDigest([]byte(r.stdout)); err != nil || digest != n.Revision() {
		t.Errorf("the YAML render builds to %s (%v), want the served Revision %s", digest, err, n.Revision())
	}
	yamlOrder := renderedResources(t, []byte(r.stdout))

	r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "render", "--output", "json"}, s.envFlags()...), s.bundleDir)...)
	var resources []struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
		Metadata   struct {
			Name string `json:"name"`
		} `json:"metadata"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &resources) != nil {
		t.Fatalf("render --output json: %s", r)
	}
	var jsonOrder []string
	for _, res := range resources {
		jsonOrder = append(jsonOrder, res.Kind+"/"+res.Metadata.Name)
	}
	if !slices.Equal(jsonOrder, yamlOrder) {
		t.Errorf("the JSON render holds %v, want the YAML render's %v", jsonOrder, yamlOrder)
	}

	header := regexp.MustCompile(`^ROUTE +PHASE +LEG +POLICY +FROM +REASON$`)
	r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "render", "--effective"}, s.envFlags()...), s.bundleDir)...)
	rows := lines(r.stdout)
	if r.code != 0 || len(rows) == 0 || !header.MatchString(rows[0]) {
		t.Fatalf("render --effective: %s", r)
	}
	wantRows := 1
	for _, route := range []string{"orders-summary", "partner-orders"} {
		chain := s.effective(t, route)
		wantRows += len(chain)
		var routeRows []string
		for _, l := range rows[1:] {
			if f := strings.Fields(l); len(f) > 0 && f[0] == route {
				routeRows = append(routeRows, l)
			}
		}
		if len(routeRows) != len(chain) {
			t.Errorf("render --effective prints %d rows of %s, want %d", len(routeRows), route, len(chain))
		}
		for i, l := range routeRows {
			if i < len(chain) {
				f := strings.Fields(l)
				if len(f) < 4 || f[1] != chain[i].Phase || f[2] != chain[i].Leg || f[3] != chain[i].Policy {
					t.Errorf("row %d of %s = %q, want %s %s %s", i, route, l, chain[i].Phase, chain[i].Leg, chain[i].Policy)
				}
			}
		}
	}
	if len(rows) != wantRows {
		t.Errorf("render --effective prints %d lines, want the header and %d rows", len(rows), wantRows-1)
	}
	r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "render", "--effective", "--route", "partner-orders"}, s.envFlags()...), s.bundleDir)...)
	routeHeader := regexp.MustCompile(`^PHASE +LEG +POLICY +FROM +REASON$`)
	if rows := lines(r.stdout); r.code != 0 || len(rows) != 1+len(s.effective(t, "partner-orders")) || !routeHeader.MatchString(rows[0]) ||
		!strings.HasPrefix(rows[1], "onRequestHeaders   client  apikey-partner") {
		t.Errorf("render --effective --route partner-orders: %s", r)
	}
	r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "render", "--effective", "--output", "json"}, s.envFlags()...), s.bundleDir)...)
	var all []struct {
		Route  string `json:"route"`
		Phase  string `json:"phase"`
		Policy string `json:"policy"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &all) != nil || len(all) != wantRows-1 ||
		!strings.HasPrefix(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(r.stdout), "[")), `{
    "route": "orders-summary"`) {
		t.Errorf("render --effective --output json: %s\nwant %d rows, each led by route", r, wantRows-1)
	}

	for _, format := range []string{"text", "json"} {
		out := filepath.Join(t.TempDir(), "converted")
		r = cli(t, s.top, cliOptions{dir: s.work}, "bundle", "render", "--api-version", "ruralz/v1alpha1", "--output-dir", out,
			"--environments", s.envFile, "--output", format, s.bundleDir)
		rev := displayRevision(t, n.Revision())
		if format == "text" {
			if r.code != 0 || r.stdout != shopRestEnv+": "+rev+" -> "+rev+" equal\n" {
				t.Errorf("render --api-version: %s", r)
			}
		} else {
			var res []struct {
				Environment string `json:"environment"`
				Equal       bool   `json:"equal"`
				Source      struct {
					Digest string `json:"digest"`
				} `json:"source"`
				Converted struct {
					Digest string `json:"digest"`
				} `json:"converted"`
			}
			if r.code != 0 || json.Unmarshal([]byte(r.stdout), &res) != nil || len(res) != 1 || res[0].Environment != shopRestEnv ||
				!res[0].Equal || res[0].Source.Digest != n.Revision() || res[0].Converted.Digest != n.Revision() {
				t.Errorf("render --api-version --output json: %s", r)
			}
		}
		// Every base and overlay file is rewritten at its own path, and
		// the converted tree renders to the served Revision.
		err := filepath.WalkDir(s.bundleDir, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			rel, err := filepath.Rel(s.bundleDir, p)
			if err == nil {
				_, err = os.Stat(filepath.Join(out, rel))
			}
			return err
		})
		if err != nil {
			t.Errorf("render --api-version --output-dir: %v", err)
		}
		built := cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "build", "--offline"}, s.envFlags()...), out)...)
		if want := rev + " " + n.Revision() + "\n"; built.code != 0 || built.stdout != want {
			t.Errorf("the converted tree builds to %s\nwant %q", built, want)
		}
	}

	r = cli(t, s.top, cliOptions{}, "bundle", "render", invalidBundle(t))
	if r.code != 1 || r.stdout != "" || r.stderr != invalidLine+"\n" {
		t.Errorf("render of an invalid Bundle: %s\nwant exit 1, the diagnostic on standard error", r)
	}
	r = cli(t, s.top, cliOptions{}, "bundle", "render", "--output", "json", invalidBundle(t))
	if r.code != 1 {
		t.Errorf("render --output json of an invalid Bundle: %s\nwant exit 1", r)
	}
	checkInvalidJSON(t, []byte(r.stdout))
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"--route", "partner-orders"}, "--route needs --effective"},
		{[]string{"--effective", "--route", "partner-order"}, `unknown Route "partner-order" (did you mean "partner-orders"?)`},
		{[]string{"--api-version", "ruralz/v9", "--output-dir", filepath.Join(t.TempDir(), "x")}, `"ruralz/v9" is not served`},
	} {
		args := append(append(append([]string{"bundle", "render"}, s.envFlags()...), c.args...), s.bundleDir)
		if r := cli(t, s.top, cliOptions{dir: s.work}, args...); r.code != 2 || !strings.Contains(r.stderr, c.want) {
			t.Errorf("%s\nwant exit 2 with %q", r, c.want)
		}
	}
}

// bundleBuild: build prints rev-<12 hex> and the full digest of the
// served Revision; built from the Node's test render it prints the
// digest the Node reports, and --output-file writes the canonical
// document /config/dump holds, byte for byte; --output json carries the
// digest, the revision, the Environment and the diagnostics; an invalid
// Bundle exits 1 and a missing source 2.
func bundleBuild(t *testing.T, s *shopRest) {
	n := s.nodes[0]
	r := cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "build", "--offline"}, s.envFlags()...), s.bundleDir)...)
	if want := displayRevision(t, n.Revision()) + " " + n.Revision() + "\n"; r.code != 0 || r.stdout != want {
		t.Errorf("build: %s\nwant %q, the served Revision", r, want)
	}
	r = cli(t, s.top, cliOptions{dir: s.work}, append(append([]string{"bundle", "build", "--output", "json"}, s.envFlags()...), s.bundleDir)...)
	var doc struct {
		Digest      string `json:"digest"`
		Revision    string `json:"revision"`
		Environment string `json:"environment"`
		Diagnostics []any  `json:"diagnostics"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &doc) != nil || doc.Digest != n.Revision() ||
		doc.Revision != displayRevision(t, n.Revision()) || doc.Environment != shopRestEnv || len(doc.Diagnostics) != 0 {
		t.Errorf("build --output json: %s", r)
	}

	canonical := filepath.Join(t.TempDir(), "revision.json")
	r = cli(t, s.top, cliOptions{}, "bundle", "build", "--offline", "--output-file", canonical, n.ConfigPath())
	if want := displayRevision(t, n.TestDigest()) + " " + n.TestDigest() + "\n"; r.code != 0 || r.stdout != want {
		t.Errorf("build of the Node's test render: %s\nwant %q, the digest the Node reports", r, want)
	}
	resp, err := n.Admin(t.Context(), "/config/dump")
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Content json.RawMessage `json:"content"`
		Digest  string          `json:"digest"`
	}
	if err := json.Unmarshal(resp.Body, &dump); err != nil || dump.Digest != n.TestDigest() {
		t.Fatalf("/config/dump: %v, digest %s, want %s", err, dump.Digest, n.TestDigest())
	}
	if got := readFile(t, canonical); !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(dump.Content)) {
		t.Errorf("build --output-file wrote\n%.400s\nwant the canonical document of /config/dump\n%.400s", got, dump.Content)
	}

	if r := cli(t, s.top, cliOptions{}, "bundle", "build", "--offline", invalidBundle(t)); r.code != 1 || r.stdout != "" || r.stderr != invalidLine+"\n" {
		t.Errorf("build of an invalid Bundle: %s\nwant exit 1, the diagnostic on standard error", r)
	}
	missing := filepath.Join(t.TempDir(), "missing")
	if r := cli(t, s.top, cliOptions{}, "bundle", "build", missing); r.code != 2 {
		t.Errorf("build of a missing source: %s\nwant exit 2", r)
	}
}

// bundleDiff covers the source forms scenario 6 leaves: two directories,
// the example rendered for staging and for prod, exit 1 with the prod
// overlay's two replacements; two files, the same render, exit 0; a saved
// dump against the live Node's admin URL, exit 0; and exit 2 for a
// Revision source (Planned (M2)) and for an admin side whose token the
// Node refuses.
func bundleDiff(t *testing.T, s *shopRest) {
	n := s.nodes[0]
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	example := filepath.Join(root, "examples", shopRestExample)
	environments := filepath.Join(root, "examples", "control", "environments.yaml")
	r := cli(t, s.top, cliOptions{}, "bundle", "diff", "--from-env", "staging", "--to-env", "prod", "--environments", environments, example, example)
	want := []string{
		"~ spec.circuitBreaker.consecutiveFailures: 5 -> 10",
		"~ spec.retries.attempts: 2 -> 3",
	}
	var ops []string
	for _, l := range lines(r.stdout) {
		if strings.HasPrefix(l, "    ") {
			ops = append(ops, strings.TrimSpace(l))
		}
	}
	if r.code != 1 || !slices.Equal(ops, want) || !strings.Contains(r.stdout, "(environment: staging -> prod)") ||
		!strings.Contains(r.stdout, "~ Upstream/orders") {
		t.Errorf("directories for staging and prod: %s\nwant exit 1 and the operations %q on Upstream/orders", r, want)
	}

	rendered := writeFile(t, t.TempDir(), "rendered.yaml", s.rendered)
	if r := cli(t, s.top, cliOptions{}, "bundle", "diff", rendered, rendered); r.code != 0 || !strings.Contains(r.stdout, "impact: none") {
		t.Errorf("two files of one render: %s\nwant exit 0", r)
	}

	admin := []string{"--admin-token-file", n.TokenFile(), "--ca-file", s.ca}
	dump := filepath.Join(t.TempDir(), "dump.json")
	if r := cli(t, s.top, cliOptions{}, append([]string{"node", "dump", "--admin", n.AdminURL(), "--output-file", dump}, admin...)...); r.code != 0 {
		t.Fatalf("save the dump: %s", r)
	}
	r = cli(t, s.top, cliOptions{}, append([]string{"bundle", "diff", dump, n.AdminURL(), "--output", "json"}, admin...)...)
	d := decodeDiff(t, r)
	if r.code != 0 || len(d.Res) != 0 || d.From.Dump != dump || d.To.Admin != n.AdminURL() || d.From.Digest != n.TestDigest() || d.To.Digest != n.TestDigest() {
		t.Errorf("a saved dump against the live Node: %s\nwant exit 0, both sides the Node's %s", r, n.TestDigest())
	}

	if r := cli(t, s.top, cliOptions{}, "bundle", "diff", "rev-0123456789ab", rendered); r.code != 2 || !strings.Contains(r.stderr, "Planned (M2)") {
		t.Errorf("a Revision source: %s\nwant exit 2, Planned (M2)", r)
	}
	wrong := writeFile(t, t.TempDir(), "wrong-token", []byte(s.top.Canary()+"\n"))
	r = cli(t, s.top, cliOptions{}, "bundle", "diff", n.AdminURL(), rendered, "--admin-token-file", wrong, "--ca-file", s.ca)
	if r.code != 2 || !regexp.MustCompile(`401 RZ-AUTH-00[12] `).MatchString(r.stderr) {
		t.Errorf("an admin side with a refused token: %s\nwant exit 2 with the 401 problem", r)
	}
	if _, err := os.Stat(dump); err != nil {
		t.Error(err)
	}
}
