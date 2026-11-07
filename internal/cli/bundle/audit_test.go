// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// auditGoldenRoot is the golden corpus of the configuration conformance
// suite, read in place.
const auditGoldenRoot = "../../../test/conformance/config/golden"

// auditTranscript is the golden form of one run: its exit code, standard
// output and standard error.
func auditTranscript(r result) string {
	return "exit " + strconv.Itoa(r.code) + "\n--- stdout ---\n" + r.stdout + "--- stderr ---\n" + r.stderr
}

// auditSecureDocs is a Bundle without findings: an https listener, a TLS
// Upstream and a Route that authenticates with an API key.
func auditSecureDocs() map[string]string {
	return map[string]string{
		"ruralz.yaml": `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: api
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}
`,
		"upstreams.yaml": upstreamDoc("  tls: {}\n"),
		"policies.yaml":  policyDoc("key", "auth.api-key", "  config: {}\n"),
		"consumers.yaml": `apiVersion: ruralz/v1alpha1
kind: Consumer
metadata:
  name: acme
spec:
  credentials:
    apiKeys:
      - name: primary
        hash: "sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6"
`,
		"routes/orders.yaml": routeDoc("orders", "orders", "  policies: [{name: key}]\n"),
	}
}

// auditOpenPluginDocs is simpleBundle with an enforcing Plugin whose
// Policy is set to open, which is an error finding.
func auditOpenPluginDocs() map[string]string {
	files := simpleBundle()
	files["plugins.yaml"] = strings.Replace(pluginDoc, "capabilities: [request.headers.read]",
		"capabilities: [request.headers.read, response.send]", 1)
	files["policies.yaml"] = policyDoc("geo-open", "plugin", "  plugin: geo\n  failureMode: open\n  config: {}\n")
	files["routes/orders.yaml"] = routeDoc("orders", "orders", "  timeout: ${TIMEOUT:-5s}\n  policies: [{name: geo-open}]\n")
	return files
}

// TestAuditSpec: ruralz bundle audit takes one optional DIR, "." by
// default, that completes files, the Bundle flags and --output text or
// json, and runs.
func TestAuditSpec(t *testing.T) {
	s := AuditSpec(Options{})
	if s.Name() != "bundle audit" || s.Summary != "Security and best-practice findings" || s.New == nil || s.Planned != "" {
		t.Fatalf("AuditSpec = %+v, want the runnable bundle audit row", s)
	}
	if len(s.Args) != 1 || s.Args[0].Name != "DIR" || !s.Args[0].Optional || s.Args[0].Default != "." ||
		s.Args[0].Complete != command.CompleteFile || len(s.Args[0].Values) != 0 {
		t.Errorf("args = %+v, want one optional DIR defaulting to . that completes files", s.Args)
	}
	type flag struct{ name, def string }
	var got []flag
	for _, f := range command.FlagSpecsOf(s) {
		got = append(got, flag{f.Name, f.Default})
		if f.Name == "output" && !slices.Equal(f.Values, []string{"text", "json"}) {
			t.Errorf("--output values = %q, want text and json", f.Values)
		}
	}
	if want := []flag{{"env", ""}, {"environments", ""}, {"output", "text"}}; !slices.Equal(got, want) {
		t.Errorf("flags = %v, want %v", got, want)
	}
}

// TestAuditHelp pins the help of ruralz bundle audit on every platform.
func TestAuditHelp(t *testing.T) {
	for _, goos := range []string{"linux", "darwin", "windows"} {
		var out, errOut strings.Builder
		sio := command.NewIO(&out, &errOut, nil, epoch, goos)
		code := command.Execute(t.Context(), sio, "ruralz", Specs(testOptions()), []string{"bundle", "audit", "--help"})
		if code != command.ExitOK || errOut.Len() != 0 {
			t.Fatalf("%s: bundle audit --help = exit %d, stderr %q", goos, code, errOut.String())
		}
		golden(t, "audit/help.txt", out.String())
	}
}

// TestAuditClean: a Bundle without findings prints nothing, or [] with
// --output json, and exits 0.
func TestAuditClean(t *testing.T) {
	dir := writeBundle(t, auditSecureDocs())
	run(t, "bundle", "audit", dir).want(t, "audit of a clean Bundle", command.ExitOK, "", "")
	run(t, "bundle", "audit", "--output", "json", dir).want(t, "audit --output json of a clean Bundle", command.ExitOK, "[]\n", "")
}

// TestAuditWarnings: warnings only print one line each, or one JSON array,
// and exit 0: a cleartext listener and Upstream and a Route without
// authentication.
func TestAuditWarnings(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	r := run(t, "bundle", "audit", dir)
	r.wantCode(t, "audit with warnings", command.ExitOK)
	golden(t, "audit/warnings.txt", auditTranscript(r))
	r = run(t, "bundle", "audit", "--output", "json", dir)
	r.wantCode(t, "audit --output json with warnings", command.ExitOK)
	golden(t, "audit/warnings.json", auditTranscript(r))
}

// TestAuditErrorFinding: an error finding, an enforcing Plugin set to
// open, exits 1 beside the warnings.
func TestAuditErrorFinding(t *testing.T) {
	dir := writeBundle(t, auditOpenPluginDocs())
	r := run(t, "bundle", "audit", dir)
	r.wantCode(t, "audit with an error finding", command.ExitNegative)
	golden(t, "audit/error.txt", auditTranscript(r))
}

// TestAuditInvalidBundle: an invalid Bundle prints its diagnostics as
// findings whose rule is their code, runs no rule, and exits 1.
func TestAuditInvalidBundle(t *testing.T) {
	files := simpleBundle()
	files["routes/orders.yaml"] = routeDoc("orders", "ordrs", "")
	dir := writeBundle(t, files)
	r := run(t, "bundle", "audit", dir)
	r.wantCode(t, "audit of an invalid Bundle", command.ExitNegative)
	golden(t, "audit/invalid.txt", auditTranscript(r))
	r = run(t, "bundle", "audit", "--output", "json", dir)
	r.wantCode(t, "audit --output json of an invalid Bundle", command.ExitNegative)
	golden(t, "audit/invalid.json", auditTranscript(r))
}

// TestAuditWarningsStayWarnings: the run's own warnings, such as a
// credential-shaped literal in plaintext Plugin config (RZ-CFG-013), are
// findings that keep the exit code at 0.
func TestAuditWarningsStayWarnings(t *testing.T) {
	files := simpleBundle()
	files["plugins.yaml"] = pluginDoc
	files["policies.yaml"] = policyDoc("geo-keyed", "plugin",
		"  plugin: geo\n  config: {backend: \"https://admin:hunter2@backend.internal/\"}\n")
	dir := writeBundle(t, files)
	r := run(t, "bundle", "audit", dir)
	r.wantCode(t, "audit with a credential-shaped literal", command.ExitOK)
	if !strings.Contains(r.stdout, " warning RZ-CFG-013 Policy/geo-keyed spec.config.backend: ") {
		t.Errorf("audit stdout lacks the RZ-CFG-013 warning of the Plugin config:\n%s", r.stdout)
	}
	golden(t, "audit/key-like.txt", auditTranscript(r))
}

// TestAuditEnvironments: with --environments and no --env the audit runs
// once per Environment in file order, naming it in each finding, in text
// and JSON; with --env it runs that Environment alone and names none.
func TestAuditEnvironments(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	r := run(t, "bundle", "audit", dir, "--environments", envs)
	r.wantCode(t, "audit per Environment", command.ExitOK)
	golden(t, "audit/environments.txt", auditTranscript(r))
	if s, p := strings.Index(r.stdout, "[environment=staging]"), strings.Index(r.stdout, "[environment=prod]"); s < 0 || p < s {
		t.Errorf("staging, declared first, does not come first:\n%s", r.stdout)
	}
	r = run(t, "bundle", "audit", dir, "--environments", envs, "--output", "json")
	r.wantCode(t, "audit --output json per Environment", command.ExitOK)
	golden(t, "audit/environments.json", auditTranscript(r))
	r = run(t, "bundle", "audit", dir, "--environments", envs, "--env", "prod")
	r.wantCode(t, "audit --env prod", command.ExitOK)
	if strings.Contains(r.stdout, "[environment=") {
		t.Errorf("audit --env names the Environment:\n%s", r.stdout)
	}
	golden(t, "audit/env.txt", auditTranscript(r))
}

// TestAuditSharedError: a finding of stages A to C prints once without an
// Environment, and since it leaves the Bundle invalid for every
// Environment, no rule runs and the audit exits 1.
func TestAuditSharedError(t *testing.T) {
	files := simpleBundle()
	files["upstreams-2.yaml"] = upstreamDoc("")
	dir := writeBundle(t, files)
	r := run(t, "bundle", "audit", dir, "--environments", envFile(t))
	r.wantCode(t, "audit of a Bundle with a shared error", command.ExitNegative)
	if strings.Contains(r.stdout, "cleartext-hop") || strings.Contains(r.stdout, "route-without-auth") {
		t.Errorf("rules ran on a Bundle with a shared error:\n%s", r.stdout)
	}
	golden(t, "audit/shared-error.txt", auditTranscript(r))
}

// TestAuditUsage: --output yaml, --env without --environments, an empty
// --environments and an extra argument are usage errors, exit 2.
func TestAuditUsage(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	tests := []struct {
		name   string
		args   []string
		stderr string
	}{
		{
			name: "--output yaml", args: []string{"--output", "yaml", dir},
			stderr: "ruralz bundle audit: unsupported --output \"yaml\" (want text or json)\nRun 'ruralz bundle audit --help' for usage.\n",
		},
		{
			name: "--env without --environments", args: []string{"--env", "prod", dir},
			stderr: "ruralz bundle audit: --env needs --environments: fetching Environments from Ruralz Control is Planned (M2)\nRun 'ruralz bundle audit --help' for usage.\n",
		},
		{
			name: "an empty --environments", args: []string{"--environments", "", dir},
			stderr: "ruralz bundle audit: --environments needs a file\nRun 'ruralz bundle audit --help' for usage.\n",
		},
		{
			name: "an extra argument", args: []string{dir, "more"},
			stderr: "ruralz bundle audit: unexpected argument \"more\"\nRun 'ruralz bundle audit --help' for usage.\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := run(t, append([]string{"bundle", "audit"}, tc.args...)...)
			r.want(t, tc.name, command.ExitNoResult, "", tc.stderr)
		})
	}
}

// TestAuditNoResult: an unreadable source and an --environments file
// without an Environment exit 2 and print no finding.
func TestAuditNoResult(t *testing.T) {
	dir := t.TempDir()
	r := run(t, "bundle", "audit", filepath.Join(dir, "missing"))
	r.wantCode(t, "audit of a missing source", command.ExitNoResult)
	if got := scrub(r.stderr, dir); r.stdout != "" || !strings.HasPrefix(got, "ruralz bundle audit: cannot read $DIR/missing: ") {
		t.Errorf("audit of a missing source: stdout %q, stderr %q", r.stdout, got)
	}
	empty := writeFile(t, "environments.yaml", "# no Environment\n")
	r = run(t, "bundle", "audit", writeBundle(t, simpleBundle()), "--environments", empty)
	r.want(t, "audit with an --environments file without an Environment", command.ExitNoResult, "",
		"ruralz bundle audit: no Environment in "+empty+"\n")
}

// TestAuditDefaultDir: without DIR the audit reads ".", which is no
// Bundle in the package directory: RZ-CFG-016, exit 1.
func TestAuditDefaultDir(t *testing.T) {
	r := run(t, "bundle", "audit")
	r.wantCode(t, "audit of .", command.ExitNegative)
	if !strings.Contains(r.stdout, " error RZ-CFG-016") {
		t.Errorf("audit of . lacks RZ-CFG-016:\n%s", r.stdout)
	}
}

// TestAuditColors: on a terminal that takes color the severities are
// colored as validate colors them; NO_COLOR, TERM=dumb and JSON stay plain.
func TestAuditColors(t *testing.T) {
	dir := writeBundle(t, auditOpenPluginDocs())
	o := testOptions()
	o.terminal = func(io.Writer) bool { return true }
	r := execute(t, o, []string{"TERM=xterm"}, "bundle", "audit", dir)
	if !strings.Contains(r.stdout, " "+ansiYellow+"warning"+ansiReset+" cleartext-hop ") ||
		!strings.Contains(r.stdout, " "+ansiRed+"error"+ansiReset+" plugin-enforcing-open ") {
		t.Errorf("audit on a terminal is not colored:\n%q", r.stdout)
	}
	for _, environ := range [][]string{{"TERM=xterm", "NO_COLOR=1"}, {"TERM=dumb"}} {
		if r := execute(t, o, environ, "bundle", "audit", dir); strings.Contains(r.stdout, "\x1b[") {
			t.Errorf("audit with %q is colored:\n%q", environ, r.stdout)
		}
	}
	if r := execute(t, o, []string{"TERM=xterm"}, "bundle", "audit", "--output", "json", dir); strings.Contains(r.stdout, "\x1b[") {
		t.Errorf("audit --output json is colored:\n%q", r.stdout)
	}
}

// TestAuditGoldenCorpus runs the audit over every entry of the golden
// corpus (docs/engineering/03-testing-and-quality-strategy.md "Security
// scanning"): its Bundle, a diff entry's to/, without --env and, for an
// entry with environments.yaml, once per Environment. Any finding the
// committed transcript does not expect, or one it expects that is gone,
// fails.
func TestAuditGoldenCorpus(t *testing.T) {
	entries, err := os.ReadDir(auditGoldenRoot)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		n++
		t.Run(e.Name(), func(t *testing.T) {
			root := filepath.Join(auditGoldenRoot, e.Name())
			dir := filepath.Join(root, "bundle")
			if _, err := os.Stat(dir); err != nil {
				dir = filepath.Join(root, "to")
			}
			got := auditTranscript(run(t, "bundle", "audit", dir))
			if envs := filepath.Join(root, "environments.yaml"); auditFileExists(envs) {
				got += auditTranscript(run(t, "bundle", "audit", dir, "--environments", envs))
			}
			golden(t, "audit/corpus/"+e.Name()+".txt", got)
		})
	}
	if n < 10 {
		t.Fatalf("the golden corpus holds %d entries", n)
	}
}

// auditFileExists reports whether path names an existing file.
func auditFileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}
