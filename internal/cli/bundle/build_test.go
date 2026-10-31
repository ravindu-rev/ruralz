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
	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// Tests of ruralz bundle build (docs/reference/01-cli-and-api-surface.md
// "Bundle verbs in detail", "Output formats and exit codes";
// docs/architecture/02-configuration-model.md "Canonical form and
// Revision").

// TestBuildPrintsDigest: one line, rev-<12 hex>, a space and the full
// digest of the Revision.
func TestBuildPrintsDigest(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	d := build(t, dir, "", "").Digest
	run(t, "bundle", "build", dir).want(t, "build", command.ExitOK, d.Short()+" "+d.String()+"\n", "")
	envs := envFile(t)
	p := build(t, dir, envs, "prod").Digest
	run(t, "bundle", "build", "--env", "prod", "--environments", envs, dir).
		want(t, "build --env prod", command.ExitOK, p.Short()+" "+p.String()+"\n", "")
	if p == d {
		t.Error("the prod Revision equals the one without an Environment, though TIMEOUT differs")
	}
}

// TestBuildJSON: --output json prints digest, revision, environment (only
// with --env) and diagnostics; a failed build leaves out digest and
// revision.
func TestBuildJSON(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	d := build(t, dir, "", "").Digest
	run(t, "bundle", "build", "--output", "json", dir).want(t, "build --output json", command.ExitOK, `{
  "digest": "`+d.String()+`",
  "revision": "`+d.Short()+`",
  "diagnostics": []
}
`, "")
	envs := envFile(t)
	p := build(t, dir, envs, "staging").Digest
	r := run(t, "bundle", "build", "--output", "json", "--env", "staging", "--environments", envs, dir)
	r.want(t, "build --output json --env staging", command.ExitOK, `{
  "digest": "`+p.String()+`",
  "revision": "`+p.Short()+`",
  "environment": "staging",
  "diagnostics": []
}
`, "")

	bad := writeBundle(t, typoBundle())
	r = run(t, "bundle", "build", "--output", "json", bad)
	r.wantCode(t, "failed build", command.ExitNegative)
	var got map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON object: %v\n%s", err, r.stdout)
	}
	if _, ok := got["digest"]; ok {
		t.Error("a failed build prints a digest")
	}
	if _, ok := got["revision"]; ok {
		t.Error("a failed build prints a revision")
	}
	if !strings.Contains(string(got["diagnostics"]), `"code": "RZ-CFG-009"`) {
		t.Errorf("diagnostics = %s", got["diagnostics"])
	}
	if r.stderr != typoLine+"\n" {
		t.Errorf("stderr = %q, want the diagnostic as a text line", r.stderr)
	}
}

// TestBuildWarnings: warnings print on standard error in every mode and
// sit in the JSON object's diagnostics; the build succeeds.
func TestBuildWarnings(t *testing.T) {
	dir := writeBundle(t, warningBundle())
	r := run(t, "bundle", "build", dir)
	r.wantCode(t, "build", command.ExitOK)
	if !strings.HasPrefix(r.stdout, "rev-") || len(lines(r.stdout)) != 1 || !strings.Contains(r.stderr, " warning RZ-CFG-013 ") {
		t.Errorf("stdout %q, stderr %q", r.stdout, r.stderr)
	}
	r = run(t, "bundle", "build", "--output", "json", dir)
	if r.code != command.ExitOK || !strings.Contains(r.stdout, `"code": "RZ-CFG-013"`) || !strings.Contains(r.stdout, `"digest": "sha256:`) ||
		!strings.Contains(r.stderr, " warning RZ-CFG-013 ") {
		t.Errorf("JSON: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}

// TestBuildFails: an invalid Bundle exits 1 with its diagnostics on
// standard error and nothing on standard output.
func TestBuildFails(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	run(t, "bundle", "build", dir).want(t, "build", command.ExitNegative, "", typoLine+"\n")
}

// TestBuildOutputFile: --output-file receives the ruralz.canonical.v1
// content, which hashes to the printed digest; a failed build writes none.
func TestBuildOutputFile(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	rev := build(t, dir, "", "")
	out := filepath.Join(t.TempDir(), "revision.json")
	run(t, "bundle", "build", "--output-file", out, dir).
		want(t, "build --output-file", command.ExitOK, rev.Digest.Short()+" "+rev.Digest.String()+"\n", "")
	got := readFile(t, out)
	if got != string(rev.Content) || revision.Sum([]byte(got)) != rev.Digest {
		t.Errorf("--output-file holds %q, want the canonical content of %s", got, rev.Digest)
	}
	if !strings.HasPrefix(got, `{"format":"ruralz.canonical.v1","resources":[`) {
		t.Errorf("--output-file is not ruralz.canonical.v1 content: %q", got)
	}
	failed := filepath.Join(t.TempDir(), "failed.json")
	run(t, "bundle", "build", "--output-file", failed, writeBundle(t, typoBundle())).wantCode(t, "failed build", command.ExitNegative)
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Errorf("a failed build wrote --output-file: %v", err)
	}
	r := run(t, "bundle", "build", "--output-file", filepath.Join(t.TempDir(), "missing", "x.json"), dir)
	r.wantCode(t, "build into a missing directory", command.ExitNoResult)
	if r.stdout != "" {
		t.Errorf("a build whose --output-file failed printed %q", r.stdout)
	}
}

// TestBuildOnlineStage: without --offline the online stage runs, failing
// each Plugin with RZ-CFG-028 until fetching Plugin artifacts ships, so a
// Bundle with a Plugin exits 1 and prints no digest; --offline skips it,
// and a Bundle without Plugins passes it.
func TestBuildOnlineStage(t *testing.T) {
	files := simpleBundle()
	files["plugins.yaml"] = pluginDoc
	dir := writeBundle(t, files)
	run(t, "bundle", "build", dir).want(t, "build", command.ExitNegative, "",
		`plugins.yaml:1:1 error RZ-CFG-028 Plugin/geo: Plugin "geo" cannot be checked online: fetching Plugin artifacts by digest is Planned (M2)`+"\n")
	rev := build(t, dir, "", "")
	run(t, "bundle", "build", "--offline", dir).want(t, "build --offline", command.ExitOK, rev.Digest.Short()+" "+rev.Digest.String()+"\n", "")
	run(t, "bundle", "build", writeBundle(t, simpleBundle())).wantCode(t, "build without Plugins", command.ExitOK)
}

// TestBuildStandardInput: "-" builds the one-file Bundle read from
// standard input; its Revision equals the directory's.
func TestBuildStandardInput(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	rendered := run(t, "bundle", "render", dir).stdout
	d := build(t, dir, "", "").Digest
	execute(t, withStdin(testOptions(), rendered), nil, "bundle", "build", "-").
		want(t, "build -", command.ExitOK, d.Short()+" "+d.String()+"\n", "")
}

// TestBuildColors: diagnostics on a terminal standard error are colored;
// the digest line never is.
func TestBuildColors(t *testing.T) {
	o := testOptions()
	o.terminal = func(io.Writer) bool { return true }
	r := execute(t, o, nil, "bundle", "build", writeBundle(t, typoBundle()))
	if !strings.Contains(r.stderr, ansiRed+"error"+ansiReset) {
		t.Errorf("stderr = %q, want a red severity", r.stderr)
	}
	r = execute(t, o, nil, "bundle", "build", writeBundle(t, simpleBundle()))
	if strings.Contains(r.stdout, "\x1b") {
		t.Errorf("the digest line is colored: %q", r.stdout)
	}
}
