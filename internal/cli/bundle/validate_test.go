// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"encoding/json"
	"io"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
)

// Tests of ruralz bundle validate (docs/reference/01-cli-and-api-surface.md
// "Bundle verbs in detail", "Global flags", "Output formats and exit
// codes").

// typoBundle is simpleBundle with the Route naming Upstream "ordrs".
func typoBundle() map[string]string {
	files := simpleBundle()
	files["routes/orders.yaml"] = routeDoc("orders", "ordrs", "")
	return files
}

// typoLine is the diagnostic of typoBundle.
const typoLine = `routes/orders.yaml:9:13 error RZ-CFG-009 Route/orders spec.upstreams[name=ordrs].name: Upstream "ordrs" not found (did you mean "orders"?)`

// TestValidateClean: a valid Bundle prints nothing, or [] with --output
// json, and exits 0.
func TestValidateClean(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	run(t, "bundle", "validate", dir).want(t, "validate", command.ExitOK, "", "")
	run(t, "bundle", "validate", "--output", "json", dir).want(t, "validate --output json", command.ExitOK, "[]\n", "")
}

// TestValidateDefaultsToCurrentDirectory: DIR defaults to ".".
func TestValidateDefaultsToCurrentDirectory(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	t.Chdir(dir)
	run(t, "bundle", "validate").want(t, "validate without DIR", command.ExitNegative, typoLine+"\n", "")
}

// TestValidateText: one diagnostic per line,
// "file:line:column severity code Kind/name path: message", as the data on
// standard output; an error exits 1.
func TestValidateText(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	run(t, "bundle", "validate", dir).want(t, "validate", command.ExitNegative, typoLine+"\n", "")
}

// TestValidateJSON: --output json prints the diagnostics as one array in
// the configuration model's JSON form.
func TestValidateJSON(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	r := run(t, "bundle", "validate", "--output", "json", dir)
	r.wantCode(t, "validate --output json", command.ExitNegative)
	var got []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, r.stdout)
	}
	if len(got) != 1 || got[0]["code"] != "RZ-CFG-009" || got[0]["severity"] != "error" ||
		got[0]["file"] != "routes/orders.yaml" || got[0]["line"] != float64(9) || got[0]["hint"] != `did you mean "orders"?` {
		t.Errorf("diagnostics = %v", got)
	}
	if !strings.HasPrefix(r.stdout, "[\n  {\n    \"code\": \"RZ-CFG-009\",\n") || r.stderr != "" {
		t.Errorf("stdout is not indented by two spaces, or stderr %q:\n%s", r.stderr, r.stdout)
	}
}

// warningBundle is simpleBundle whose Route timeout reads a variable named
// like a secret, the RZ-CFG-013 warning.
func warningBundle() map[string]string {
	files := simpleBundle()
	files["routes/orders.yaml"] = routeDoc("orders", "orders", "  timeout: ${API_TOKEN:-5s}\n")
	return files
}

// TestValidateWarningsExitZero: warnings print but never change the exit
// code.
func TestValidateWarningsExitZero(t *testing.T) {
	dir := writeBundle(t, warningBundle())
	r := run(t, "bundle", "validate", dir)
	r.wantCode(t, "validate", command.ExitOK)
	if !strings.Contains(r.stdout, " warning RZ-CFG-013 ") || len(lines(r.stdout)) != 1 {
		t.Errorf("stdout = %q, want one RZ-CFG-013 warning", r.stdout)
	}
}

// TestValidateProcessEnvironment: without --env the process environment
// supplies ${VAR}: an invalid value fails the Bundle.
func TestValidateProcessEnvironment(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	execute(t, testOptions(), []string{"TIMEOUT=7s"}, "bundle", "validate", dir).want(t, "TIMEOUT=7s", command.ExitOK, "", "")
	r := execute(t, testOptions(), []string{"TIMEOUT=soon"}, "bundle", "validate", dir)
	r.wantCode(t, "TIMEOUT=soon", command.ExitNegative)
	if !strings.Contains(r.stdout, "routes/orders.yaml:") || !strings.Contains(r.stdout, " error RZ-CFG-005 Route/orders spec.timeout") {
		t.Errorf("stdout = %q, want the RZ-CFG-005 of the substituted timeout", r.stdout)
	}
}

// overlayBundle is simpleBundle with a prod overlay that sets an invalid
// Upstream retry count, so only a render for prod fails.
func overlayBundle() map[string]string {
	files := simpleBundle()
	files["overlays/prod/upstream.yaml"] = `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  retries: {attempts: -1}
`
	return files
}

// TestValidateEnv: --env renders with that Environment's overlay and
// variables from --environments, never the process environment.
func TestValidateEnv(t *testing.T) {
	dir := writeBundle(t, overlayBundle())
	envs := envFile(t)
	execute(t, testOptions(), []string{"TIMEOUT=soon"}, "bundle", "validate", "--env", "staging", "--environments", envs, dir).
		want(t, "--env staging", command.ExitOK, "", "")
	r := run(t, "bundle", "validate", "--env", "prod", "--environments", envs, dir)
	r.wantCode(t, "--env prod", command.ExitNegative)
	if len(lines(r.stdout)) != 1 || !strings.HasPrefix(r.stdout, "overlays/prod/upstream.yaml:") || !strings.Contains(r.stdout, " error RZ-CFG-005 Upstream/orders spec.retries.attempts") || strings.Contains(r.stdout, "[environment=") {
		t.Errorf("--env prod stdout = %q, want one finding of the prod overlay naming no Environment", r.stdout)
	}
	r = run(t, "bundle", "validate", "--env", "qa", "--environments", envs, dir)
	r.wantCode(t, "--env qa", command.ExitNegative)
	if !strings.Contains(r.stdout, " error RZ-CFG-009 Environment/qa: ") {
		t.Errorf("--env qa stdout = %q, want RZ-CFG-009 for the undeclared Environment", r.stdout)
	}
}

// TestValidateEachEnvironment: with --environments and no --env, validate
// runs once per Environment in file order; findings of stages A to C print
// once and name no Environment, those of stages D to M once per
// Environment, naming it; the exit code is the worst.
func TestValidateEachEnvironment(t *testing.T) {
	files := overlayBundle()
	files["extra.yaml"] = "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: 7}\n"
	dir := writeBundle(t, files)
	envs := writeFile(t, "environments.yaml", docs(
		environmentDoc("prod", "\n  variables:\n    TIMEOUT: 4s"),
		environmentDoc("staging", "\n  variables:\n    TIMEOUT: soon"),
	))
	r := run(t, "bundle", "validate", "--environments", envs, dir)
	r.wantCode(t, "validate --environments", command.ExitNegative)
	got := lines(r.stdout)
	if len(got) != 3 {
		t.Fatalf("stdout = %q, want three lines", r.stdout)
	}
	switch {
	case !strings.HasPrefix(got[0], "extra.yaml:") || strings.Contains(got[0], "[environment="):
		t.Errorf("line 1 = %q, want the shared finding of extra.yaml without an Environment", got[0])
	case !strings.HasPrefix(got[1], "overlays/prod/upstream.yaml:") || !strings.HasSuffix(got[1], " [environment=prod]"):
		t.Errorf("line 2 = %q, want the prod overlay's finding naming prod, first in file order", got[1])
	case !strings.HasPrefix(got[2], "routes/orders.yaml:") || !strings.HasSuffix(got[2], " [environment=staging]"):
		t.Errorf("line 3 = %q, want staging's substitution finding naming staging", got[2])
	}
	r = run(t, "bundle", "validate", "--environments", envs, "--output", "json", dir)
	var ds []struct {
		File        string `json:"file"`
		Environment string `json:"environment"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, r.stdout)
	}
	if len(ds) != 3 || ds[0].Environment != "" || ds[1].Environment != "prod" || ds[2].Environment != "staging" {
		t.Errorf("JSON diagnostics = %+v, want the shared one, then prod's, then staging's", ds)
	}
	clean := writeBundle(t, simpleBundle())
	run(t, "bundle", "validate", "--environments", envFile(t), clean).want(t, "clean Bundle per Environment", command.ExitOK, "", "")
}

// TestNoEnvironmentInFile: every command that reads an --environments file
// that declares no Environment exits 2 with "no Environment in FILE".
func TestNoEnvironmentInFile(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := writeFile(t, "clusters.yaml", `apiVersion: ruralz/v1alpha1
kind: Cluster
metadata:
  name: eu
spec:
  environment: prod
`)
	out := filepath.Join(t.TempDir(), "out")
	for _, args := range [][]string{
		{"bundle", "validate", "--environments", envs, dir},
		{"bundle", "validate", "--env", "prod", "--environments", envs, dir},
		{"bundle", "render", "--env", "prod", "--environments", envs, dir},
		{"bundle", "render", "--api-version", "ruralz/v1alpha1", "--output-dir", out, "--environments", envs, dir},
		{"bundle", "build", "--env", "prod", "--environments", envs, dir},
		{"bundle", "diff", "--env", "prod", "--environments", envs, dir, dir},
	} {
		r := run(t, args...)
		r.want(t, strings.Join(args[:2], " "), command.ExitNoResult, "", "ruralz "+strings.Join(args[:2], " ")+": no Environment in "+envs+"\n")
	}
}

// TestEnvironmentFlagRules: --env needs --environments, since fetching
// Environments from Ruralz Control is Planned (M2), and neither flag takes
// an empty value; render and build refuse --environments without --env.
func TestEnvironmentFlagRules(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"validate", "--env", "prod", dir}, "--env needs --environments: fetching Environments from Ruralz Control is Planned (M2)"},
		{[]string{"build", "--env", "prod", dir}, "--env needs --environments: fetching Environments from Ruralz Control is Planned (M2)"},
		{[]string{"validate", "--env", "", "--environments", envs, dir}, "--env needs an Environment name"},
		{[]string{"validate", "--environments", "", dir}, "--environments needs a file"},
		{[]string{"render", "--environments", envs, dir}, "--environments needs --env: ruralz bundle render renders one Environment"},
		{[]string{"render", "--effective", "--environments", envs, dir}, "--environments needs --env: ruralz bundle render renders one Environment"},
		{[]string{"build", "--environments", envs, dir}, "--environments needs --env: ruralz bundle build renders one Environment"},
	} {
		r := run(t, append([]string{"bundle"}, tc.args...)...)
		path := "ruralz bundle " + tc.args[0]
		r.want(t, strings.Join(tc.args, " "), command.ExitNoResult, "", path+": "+tc.msg+"\nRun '"+path+" --help' for usage.\n")
	}
}

// TestUnreadableSource: a source that is missing exits 2, naming it; so
// does an unreadable --environments file.
func TestUnreadableSource(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing")
	for _, verb := range []string{"validate", "render", "build"} {
		r := run(t, "bundle", verb, missing)
		r.wantCode(t, verb+" of a missing source", command.ExitNoResult)
		if !strings.HasPrefix(r.stderr, "ruralz bundle "+verb+": cannot read "+missing+": ") || r.stdout != "" {
			t.Errorf("%s stderr = %q, stdout %q", verb, r.stderr, r.stdout)
		}
	}
	dir := writeBundle(t, simpleBundle())
	r := run(t, "bundle", "validate", "--env", "prod", "--environments", missing, dir)
	r.wantCode(t, "missing --environments", command.ExitNoResult)
	if !strings.HasPrefix(r.stderr, "ruralz bundle validate: cannot read --environments "+missing+": ") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

// TestNotADirectoryOrFile: a source that is neither a directory nor a
// regular file exits 2.
func TestNotADirectoryOrFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no character device path the test can name")
	}
	r := run(t, "bundle", "validate", "/dev/null")
	r.wantCode(t, "validate /dev/null", command.ExitNoResult)
	if !strings.Contains(r.stderr, "neither a directory nor a regular file") {
		t.Errorf("stderr = %q", r.stderr)
	}
}

// TestDirectoryWithoutRootFile: a directory without ruralz.yaml is
// RZ-CFG-016, an invalid Bundle.
func TestDirectoryWithoutRootFile(t *testing.T) {
	dir := writeBundle(t, map[string]string{"upstreams.yaml": upstreamDoc("orders", "")})
	r := run(t, "bundle", "validate", dir)
	r.wantCode(t, "validate", command.ExitNegative)
	if !strings.Contains(r.stdout, " error RZ-CFG-016") {
		t.Errorf("stdout = %q, want RZ-CFG-016", r.stdout)
	}
}

// TestValidateOnline: --online fails each Plugin with RZ-CFG-028, the
// fetch being Planned (M2), and passes a Bundle without Plugins; without
// --online the Plugin passes.
func TestValidateOnline(t *testing.T) {
	files := simpleBundle()
	files["plugins.yaml"] = pluginDoc
	withPlugin := writeBundle(t, files)
	run(t, "bundle", "validate", withPlugin).want(t, "offline", command.ExitOK, "", "")
	run(t, "bundle", "validate", "--online", withPlugin).want(t, "--online", command.ExitNegative,
		`plugins.yaml:1:1 error RZ-CFG-028 Plugin/geo: Plugin "geo" cannot be checked online: fetching Plugin artifacts by digest is Planned (M2)`+"\n", "")
	run(t, "bundle", "validate", "--online", writeBundle(t, simpleBundle())).want(t, "--online without Plugins", command.ExitOK, "", "")
	envs := envFile(t)
	r := run(t, "bundle", "validate", "--online", "--environments", envs, withPlugin)
	r.wantCode(t, "--online per Environment", command.ExitNegative)
	if got := lines(r.stdout); len(got) != 2 || !strings.HasSuffix(got[0], "[environment=staging]") || !strings.HasSuffix(got[1], "[environment=prod]") {
		t.Errorf("stdout = %q, want RZ-CFG-028 once per Environment in file order", r.stdout)
	}
}

// TestStandardInput: "-" reads standard input to its end as a one-file
// Bundle whose diagnostics name the file "-".
func TestStandardInput(t *testing.T) {
	stream := docs(gatewayDoc, upstreamDoc("orders", ""), routeDoc("orders", "ordrs", ""))
	r := execute(t, withStdin(testOptions(), stream), nil, "bundle", "validate", "-")
	r.wantCode(t, "validate -", command.ExitNegative)
	if !strings.HasPrefix(r.stdout, "-:") || !strings.Contains(r.stdout, "RZ-CFG-009 Route/orders") {
		t.Errorf("stdout = %q, want RZ-CFG-009 in file -", r.stdout)
	}
	execute(t, withStdin(testOptions(), docs(gatewayDoc, upstreamDoc("orders", ""), routeDoc("orders", "orders", ""))), nil,
		"bundle", "validate", "-").want(t, "valid standard input", command.ExitOK, "", "")
	o := testOptions()
	o.Stdin = failingReader{}
	r = execute(t, o, nil, "bundle", "validate", "-")
	r.want(t, "failing standard input", command.ExitNoResult, "", "ruralz bundle validate: read standard input: unexpected EOF\n")
}

// TestSourceLimit: a source past the source limit is RZ-CFG-001, and
// standard input is read only to one byte past the limit, however long it
// is.
func TestSourceLimit(t *testing.T) {
	small, err := newPipelineWithLimits(loader.Limits{MaxSourceBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	o := testOptions()
	o.pipelineFunc = func(Options) (*pipeline.Pipeline, error) { return small, nil }
	o.Stdin = io.MultiReader(strings.NewReader("apiVersion: ruralz/v1alpha1\n"), endless{})
	r := execute(t, o, nil, "bundle", "validate", "-")
	r.wantCode(t, "endless standard input", command.ExitNegative)
	if !strings.Contains(r.stdout, "error RZ-CFG-001") || !strings.Contains(r.stdout, "passes the limit of 64 bytes") {
		t.Errorf("stdout = %q, want RZ-CFG-001 at the source limit", r.stdout)
	}
	dir := writeBundle(t, simpleBundle())
	r = execute(t, o, nil, "bundle", "validate", dir)
	r.wantCode(t, "a Bundle past the limit", command.ExitNegative)
	if !strings.Contains(r.stdout, "error RZ-CFG-001") {
		t.Errorf("stdout = %q, want RZ-CFG-001", r.stdout)
	}
}

// newPipelineWithLimits builds a test pipeline under limits.
func newPipelineWithLimits(l loader.Limits) (*pipeline.Pipeline, error) {
	return newTestPipeline(pipeline.Options{Limits: l})
}

// TestValidateColors: on a terminal that takes color the severity is
// colored, error red and warning yellow; NO_COLOR, TERM=dumb and JSON
// leave it plain.
func TestValidateColors(t *testing.T) {
	o := testOptions()
	o.terminal = func(io.Writer) bool { return true }
	dir := writeBundle(t, typoBundle())
	r := execute(t, o, nil, "bundle", "validate", dir)
	want := strings.Replace(typoLine, " error ", " "+ansiRed+"error"+ansiReset+" ", 1) + "\n"
	r.want(t, "colored", command.ExitNegative, want, "")
	warn := writeBundle(t, warningBundle())
	if r := execute(t, o, nil, "bundle", "validate", warn); !strings.Contains(r.stdout, " "+ansiYellow+"warning"+ansiReset+" RZ-CFG-013 ") {
		t.Errorf("warning not yellow: %q", r.stdout)
	}
	for _, env := range [][]string{{"NO_COLOR=1"}, {"TERM=dumb"}} {
		execute(t, o, env, "bundle", "validate", dir).want(t, env[0], command.ExitNegative, typoLine+"\n", "")
	}
	execute(t, o, []string{"NO_COLOR="}, "bundle", "validate", dir).want(t, "empty NO_COLOR", command.ExitNegative, want, "")
	if r := execute(t, o, nil, "bundle", "validate", "--output", "json", dir); strings.Contains(r.stdout, "\x1b") {
		t.Errorf("JSON is colored: %q", r.stdout)
	}
	if r := run(t, "bundle", "validate", dir); strings.Contains(r.stdout, "\x1b") {
		t.Errorf("output to a pipe is colored: %q", r.stdout)
	}
}
