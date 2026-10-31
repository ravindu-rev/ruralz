// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests of ruralz bundle render --api-version VERSION --output-dir DIR
// (docs/reference/01-cli-and-api-surface.md "Bundle verbs in detail").

// convertBundle is a Bundle with an unsubstituted ${VAR}, a $${ escape in
// a label and a prod overlay.
func convertBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml":    gatewayDoc,
		"upstreams.yaml": upstreamDoc("  timeout: ${UP_TIMEOUT:-2s}\n"),
		"routes/orders.json": `{"apiVersion": "ruralz/v1alpha1", "kind": "Route",
 "metadata": {"name": "orders", "labels": {"template": "/v1/$${id}"}},
 "spec": {"match": {"path": {"prefix": "/orders"}}, "upstreams": [{"name": "orders"}], "timeout": "${TIMEOUT:-5s}"}}
`,
		"overlays/prod/upstream.yaml": `# retry once more in prod
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  retries: {attempts: 3}
`,
	}
}

// convertArgs returns the arguments of a conversion of dir into out with
// the environments file envs.
func convertArgs(dir, out, envs string, extra ...string) []string {
	args := []string{"bundle", "render", "--api-version", v1alpha1.APIVersion, "--output-dir", out, "--environments", envs}
	return append(append(args, extra...), dir)
}

// TestConvertChecksEveryEnvironment: each base and overlay file is
// rewritten into DIR at its own path and in its own format, unmerged and
// unsubstituted; both trees build for every Environment in file order and
// one line per Environment says their Revisions are equal; exit 0.
func TestConvertChecksEveryEnvironment(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	envs := envFile(t)
	staging, prod := build(t, dir, envs, "staging").Digest, build(t, dir, envs, "prod").Digest
	out := filepath.Join(t.TempDir(), "converted")
	run(t, convertArgs(dir, out, envs)...).want(t, "convert", command.ExitOK,
		"staging: "+staging.Short()+" -> "+staging.Short()+" equal\nprod: "+prod.Short()+" -> "+prod.Short()+" equal\n", "")
	if got := readFile(t, filepath.Join(out, "upstreams.yaml")); !strings.Contains(got, `timeout: "${UP_TIMEOUT:-2s}"`) {
		t.Errorf("upstreams.yaml lost its ${VAR}:\n%s", got)
	}
	route := readFile(t, filepath.Join(out, "routes", "orders.json"))
	var doc map[string]any
	if err := json.Unmarshal([]byte(route), &doc); err != nil || !strings.Contains(route, `"${TIMEOUT:-5s}"`) || !strings.Contains(route, `"/v1/$${id}"`) {
		t.Errorf("routes/orders.json is not the JSON source, unsubstituted (%v):\n%s", err, route)
	}
	overlay := readFile(t, filepath.Join(out, "overlays", "prod", "upstream.yaml"))
	if strings.Contains(overlay, "#") || !strings.Contains(overlay, "attempts: 3") {
		t.Errorf("the overlay is not rewritten without its comment:\n%s", overlay)
	}

	out2 := filepath.Join(t.TempDir(), "converted")
	r := run(t, convertArgs(dir, out2, envs, "--output", "json")...)
	r.wantCode(t, "convert --output json", command.ExitOK)
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil || len(got) != 2 {
		t.Fatalf("stdout = %q (%v)", r.stdout, err)
	}
	src, _ := got[1]["source"].(map[string]any)
	conv, _ := got[1]["converted"].(map[string]any)
	if got[1]["environment"] != "prod" || got[1]["equal"] != true || src["digest"] != prod.String() || src["revision"] != prod.Short() ||
		conv["digest"] != prod.String() || got[1]["diff"] != nil {
		t.Errorf("prod result = %v", got[1])
	}
}

// TestConvertEnv: with --env only that Environment is checked.
func TestConvertEnv(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	envs := envFile(t)
	prod := build(t, dir, envs, "prod").Digest
	out := filepath.Join(t.TempDir(), "converted")
	run(t, convertArgs(dir, out, envs, "--env", "prod")...).want(t, "convert --env prod", command.ExitOK,
		"prod: "+prod.Short()+" -> "+prod.Short()+" equal\n", "")
}

// TestConvertUnservedVersion: an unserved VERSION is RZ-CFG-007 and exits
// 2, writing nothing.
func TestConvertUnservedVersion(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	out := filepath.Join(t.TempDir(), "converted")
	r := run(t, "bundle", "render", "--api-version", "ruralz/v9", "--output-dir", out, "--environments", envFile(t), dir)
	r.want(t, "convert to ruralz/v9", command.ExitNoResult, "",
		"- error RZ-CFG-007: --api-version \"ruralz/v9\" is not served by this release (served apiVersions: ruralz/v1alpha1)\n")
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("DIR exists after a refused conversion: %v", err)
	}
}

// TestConvertRefusedSources: a source document whose apiVersion is not
// served is RZ-CFG-007 and exits 1, and so does a file that does not
// parse; neither writes anything.
func TestConvertRefusedSources(t *testing.T) {
	for name, content := range map[string]string{
		"unserved": "apiVersion: ruralz/v9\nkind: Upstream\nmetadata: {name: legacy}\nspec: {}\n",
		"broken":   "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: [\n",
	} {
		t.Run(name, func(t *testing.T) {
			files := convertBundle()
			files["legacy.yaml"] = content
			dir := writeBundle(t, files)
			out := filepath.Join(t.TempDir(), "converted")
			r := run(t, convertArgs(dir, out, envFile(t))...)
			r.wantCode(t, "convert", command.ExitNegative)
			if !strings.HasPrefix(r.stderr, "legacy.yaml:") || r.stdout != "" {
				t.Errorf("stdout %q, stderr %q: want the finding of legacy.yaml", r.stdout, r.stderr)
			}
			if name == "unserved" && !strings.Contains(r.stderr, " error RZ-CFG-007 ") {
				t.Errorf("stderr = %q, want RZ-CFG-007", r.stderr)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("DIR exists after a refused conversion: %v", err)
			}
			r = run(t, convertArgs(dir, out, envFile(t), "--output", "json")...)
			var ds []map[string]any
			if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil || len(ds) == 0 {
				t.Errorf("--output json stdout = %q (%v), want the diagnostics array", r.stdout, err)
			}
		})
	}
}

// TestConvertOutputDir: DIR must not exist or must be empty, and must lie
// outside the source Bundle; otherwise exit 2 and nothing is written.
func TestConvertOutputDir(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	envs := envFile(t)
	full := t.TempDir()
	writeFiles(t, full, map[string]string{"keep.txt": "x"})
	r := run(t, convertArgs(dir, full, envs)...)
	if r.code != command.ExitNoResult || !strings.Contains(r.stderr, "is not empty") {
		t.Errorf("non-empty DIR = exit %d, stderr %q", r.code, r.stderr)
	}
	if entries, _ := os.ReadDir(full); len(entries) != 1 {
		t.Errorf("a refused DIR holds %d entries", len(entries))
	}
	inside := filepath.Join(dir, "converted")
	r = run(t, convertArgs(dir, inside, envs)...)
	if r.code != command.ExitNoResult || !strings.Contains(r.stderr, "inside the source Bundle") {
		t.Errorf("DIR inside the Bundle = exit %d, stderr %q", r.code, r.stderr)
	}
	empty := t.TempDir()
	run(t, convertArgs(dir, empty, envs)...).wantCode(t, "empty DIR", command.ExitOK)
	if _, err := os.Stat(filepath.Join(empty, "ruralz.yaml")); err != nil {
		t.Errorf("empty DIR not filled: %v", err)
	}
}

// TestConvertInvalidTree: a tree that fails to build prints "-" for the
// Revision it lacks and its diagnostics on standard error, after
// "source: " or "converted: " and naming the Environment; the line ends in
// invalid, the JSON side carries diagnostics in place of revision and
// digest, and the command exits 1.
func TestConvertInvalidTree(t *testing.T) {
	files := convertBundle()
	files["routes/orders.json"] = strings.Replace(files["routes/orders.json"], `[{"name": "orders"}]`, `[{"name": "ordrs"}]`, 1)
	dir := writeBundle(t, files)
	envs := writeFile(t, "environments.yaml", environmentDoc("staging", " {}"))
	out := filepath.Join(t.TempDir(), "converted")
	r := run(t, convertArgs(dir, out, envs)...)
	r.wantCode(t, "convert", command.ExitNegative)
	if r.stdout != "staging: - -> - invalid\n" {
		t.Errorf("stdout = %q", r.stdout)
	}
	got := lines(r.stderr)
	if len(got) != 2 || !strings.HasPrefix(got[0], "source: routes/orders.json:") || !strings.HasPrefix(got[1], "converted: routes/orders.json:") ||
		!strings.HasSuffix(got[0], " [environment=staging]") || !strings.Contains(got[1], " error RZ-CFG-009 ") {
		t.Errorf("stderr =\n%s", r.stderr)
	}
	out2 := filepath.Join(t.TempDir(), "converted")
	r = run(t, convertArgs(dir, out2, envs, "--output", "json")...)
	var res []struct {
		Source    map[string]json.RawMessage `json:"source"`
		Converted map[string]json.RawMessage `json:"converted"`
		Equal     bool                       `json:"equal"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || len(res) != 1 {
		t.Fatalf("stdout = %q (%v)", r.stdout, err)
	}
	if _, ok := res[0].Source["digest"]; ok || res[0].Equal || !strings.Contains(string(res[0].Source["diagnostics"]), "RZ-CFG-009") {
		t.Errorf("invalid source JSON = %+v", res[0])
	}
}

// timeoutConverter is a ruralz/v1alpha1 converter whose FromHub writes
// every Route timeout as 9s, so a converted tree builds another Revision.
type timeoutConverter struct{}

func (timeoutConverter) APIVersion() string { return v1alpha1.APIVersion }

func (timeoutConverter) ToHub(_ string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	return doc, nil
}

func (timeoutConverter) FromHub(kind string, doc *tree.Node, _ bool) (*tree.Node, diag.List) {
	if spec, ok := doc.Get("spec"); ok && kind == string(v1alpha1.KindRoute) {
		if t, ok := spec.Get("timeout"); ok {
			t.Text = "9s"
		}
	}
	return doc, nil
}

// TestConvertDiffers: Revisions that differ end the line in differs, the
// human diff of that Environment follows the lines, the JSON result
// carries the ruralz.diff.v1 document, and the command exits 1.
func TestConvertDiffers(t *testing.T) {
	reg, err := convert.NewRegistry(convert.Lifecycle{}, timeoutConverter{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := newTestPipeline(pipeline.Options{Converters: reg})
	if err != nil {
		t.Fatal(err)
	}
	o := testOptions()
	o.pipelineFunc = func(Options) (*pipeline.Pipeline, error) { return p, nil }
	dir := writeBundle(t, convertBundle())
	envs := writeFile(t, "environments.yaml", environmentDoc("staging", " {}"))
	before := build(t, dir, envs, "staging").Digest
	out := filepath.Join(t.TempDir(), "converted")
	r := execute(t, o, nil, convertArgs(dir, out, envs)...)
	r.wantCode(t, "convert", command.ExitNegative)
	after := build(t, out, envs, "staging").Digest
	want := "staging: " + before.Short() + " -> " + after.Short() + " differs\n\n" +
		"ruralz bundle diff: " + before.Short() + " -> " + after.Short() + " (environment: staging)\n\n" +
		"~ Route/orders                                  routes/orders.json           [traffic]\n" +
		"    ~ spec.timeout: 5s -> 9s\n\n" +
		"1 resource changed: 0 added, 0 removed, 1 modified; 0 Route chains changed; impact: traffic\n"
	if r.stdout != want || r.stderr != "" {
		t.Errorf("stdout =\n%s\nstderr %q\nwant\n%s", r.stdout, r.stderr, want)
	}
	o.terminal = func(io.Writer) bool { return true }
	colored := execute(t, o, nil, convertArgs(dir, filepath.Join(t.TempDir(), "c"), envs)...)
	if !strings.Contains(colored.stdout, ansiYellow+"    ~ spec.timeout: 5s -> 9s"+ansiReset) {
		t.Errorf("the diff on a terminal is not colored:\n%s", colored.stdout)
	}
	report := filepath.Join(t.TempDir(), "report.txt")
	execute(t, o, nil, convertArgs(dir, filepath.Join(t.TempDir(), "f"), envs, "--output-file", report)...).
		wantCode(t, "convert --output-file", command.ExitNegative)
	if got := readFile(t, report); got != want {
		t.Errorf("--output-file on a terminal holds\n%q\nwant the plain report\n%q", got, want)
	}
	r = execute(t, o, nil, convertArgs(dir, filepath.Join(t.TempDir(), "j"), envs, "--output", "json")...)
	var res []struct {
		Equal bool            `json:"equal"`
		Diff  json.RawMessage `json:"diff"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &res); err != nil || len(res) != 1 || res[0].Equal ||
		!strings.Contains(string(res[0].Diff), `"format": "ruralz.diff.v1"`) || strings.Contains(r.stdout, "\x1b") {
		t.Errorf("JSON = %q (%v)", r.stdout, err)
	}
}

// TestConvertOneFileBundle: a one-file Bundle is rewritten into DIR under
// its own name and checked like a directory.
func TestConvertOneFileBundle(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	rendered := writeFile(t, "rendered.yaml", run(t, "bundle", "render", dir).stdout)
	envs := envFile(t)
	out := filepath.Join(t.TempDir(), "converted")
	r := run(t, convertArgs(rendered, out, envs, "--env", "prod")...)
	r.wantCode(t, "convert a one-file Bundle", command.ExitOK)
	if !strings.HasPrefix(r.stdout, "prod: rev-") || !strings.HasSuffix(r.stdout, " equal\n") {
		t.Errorf("stdout = %q", r.stdout)
	}
	if got := readFile(t, filepath.Join(out, "rendered.yaml")); !strings.Contains(got, "kind: Gateway") {
		t.Errorf("DIR/rendered.yaml =\n%s", got)
	}
}

// TestConvertOutputFile: --output-file receives the report.
func TestConvertOutputFile(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	envs := envFile(t)
	report := filepath.Join(t.TempDir(), "report.txt")
	run(t, convertArgs(dir, filepath.Join(t.TempDir(), "c"), envs, "--output-file", report)...).want(t, "convert --output-file", command.ExitOK, "", "")
	if got := readFile(t, report); !strings.HasPrefix(got, "staging: rev-") || len(lines(got)) != 2 {
		t.Errorf("report =\n%s", got)
	}
}

// TestConvertEnvironmentsFail: a file whose every Environment fails its
// checks prints the file's findings and exits 1, writing nothing.
func TestConvertEnvironmentsFail(t *testing.T) {
	dir := writeBundle(t, convertBundle())
	envs := writeFile(t, "environments.yaml", environmentDoc("prod", "\n  overlay: 7"))
	out := filepath.Join(t.TempDir(), "converted")
	r := run(t, convertArgs(dir, out, envs)...)
	r.wantCode(t, "convert", command.ExitNegative)
	if !strings.Contains(r.stderr, " error RZ-CFG-005 Environment/prod") {
		t.Errorf("stderr = %q, want the Environment's finding", r.stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("DIR exists: %v", err)
	}
}
