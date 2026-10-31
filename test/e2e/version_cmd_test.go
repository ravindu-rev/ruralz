// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// End-to-end cases of ruralz version and ruralz completion, Planned (M1)
// (docs/reference/01-cli-and-api-surface.md "Output formats and exit
// codes"): the build metadata in text and JSON, the version every
// authenticated answer of a Node built with it carries, and a completion
// script for each of the four shells.

// buildMetadata returns the version, commit and flavor the binaries under
// test were built with: internal/testkit/binaries' defaults when TestMain
// built them; for prebuilt ones, which RURALZ_TEST_BIN_DIR names and
// whose -trimpath build records no linker flags, what ruralz version
// reports, which the Node's version then cross-checks.
func buildMetadata(t testing.TB) (version, commit, flavor string, known bool) {
	t.Helper()
	if os.Getenv(binaries.EnvBinDir) == "" {
		return binaries.DefaultVersion, binaries.DefaultCommit, binaries.DefaultFlavor, true
	}
	r := cli(t, nil, cliOptions{}, "version", "--output", "json")
	var doc struct{ Version, Commit, Flavor string }
	if err := json.Unmarshal([]byte(r.stdout), &doc); err != nil {
		t.Fatalf("%s: %v", r, err)
	}
	return doc.Version, doc.Commit, doc.Flavor, false
}

// TestVersion: ruralz version prints the version, commit and flavor the
// release flags set, the served apiVersions and their schema levels, in
// text and JSON; a Node of the same build names the same version in
// Ruralz-Version.
func TestVersion(t *testing.T) {
	version, commit, flavor, known := buildMetadata(t)
	if version == "" || commit == "" || flavor == "" {
		t.Fatalf("the ruralz under test reports version %q, commit %q, flavor %q", version, commit, flavor)
	}
	if !known {
		t.Logf("prebuilt binaries: the Node's version cross-checks ruralz version's %s", version)
	}
	r := cli(t, nil, cliOptions{}, "version")
	want := []string{
		"version:      " + version,
		"commit:       " + commit,
		"flavor:       " + flavor,
		"apiVersions:  ruralz/v1alpha1",
		"schemaLevels: ruralz/v1alpha1=0",
	}
	if r.code != 0 || !slices.Equal(lines(r.stdout), want) || r.stderr != "" {
		t.Errorf("%s\nwant exit 0 and\n%s", r, strings.Join(want, "\n"))
	}
	r = cli(t, nil, cliOptions{}, "version", "--output", "json")
	var doc struct {
		Version      string         `json:"version"`
		Commit       string         `json:"commit"`
		Flavor       string         `json:"flavor"`
		APIVersions  []string       `json:"apiVersions"`
		SchemaLevels map[string]int `json:"schemaLevels"`
		PluginABI    *string        `json:"pluginAbi"`
		Control      *string        `json:"controlStream"`
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &doc) != nil || doc.Version != version || doc.Commit != commit || doc.Flavor != flavor ||
		!slices.Equal(doc.APIVersions, []string{"ruralz/v1alpha1"}) || doc.SchemaLevels["ruralz/v1alpha1"] != 0 || len(doc.SchemaLevels) != 1 ||
		doc.PluginABI != nil || doc.Control != nil {
		t.Errorf("%s\nwant the build metadata without pluginAbi and controlStream, which M1 does not serve", r)
	}
	if r := cli(t, nil, cliOptions{}, "version", "--output", "yaml"); r.code != 2 {
		t.Errorf("%s\nwant exit 2 for an output format version does not take", r)
	}

	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{Name: "backend"})
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: drainBundle(mock.Addr())})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := nodes[0].Admin(t.Context(), "/config/dump")
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.Header.Get(admin.HeaderVersion); got != version {
		t.Errorf("the Node of the same build answers %s %q, want %q", admin.HeaderVersion, got, version)
	}
}

// TestCompletion: ruralz completion prints a script for bash, zsh, fish
// and powershell, each registering completion for ruralz and offering
// every noun the build runs; a shell found on PATH checks its script's
// syntax, and bash completes a noun and a verb through the script; another
// shell name exits 2.
func TestCompletion(t *testing.T) {
	marks := map[string]string{
		"bash":       "complete -o bashdefault -o default -F _ruralz ruralz",
		"zsh":        "#compdef ruralz",
		"fish":       "complete -c ruralz",
		"powershell": "Register-ArgumentCompleter -Native -CommandName 'ruralz'",
	}
	checks := map[string][]string{
		"bash": {"bash", "-n"},
		"zsh":  {"zsh", "-n"},
		"fish": {"fish", "--no-execute"},
	}
	dir := t.TempDir()
	for _, shell := range []string{"bash", "zsh", "fish", "powershell"} {
		r := cli(t, nil, cliOptions{}, "completion", shell)
		if r.code != 0 || !strings.Contains(r.stdout, marks[shell]) || r.stderr != "" {
			t.Errorf("%s\nwant exit 0 and a script holding %q", r, marks[shell])
			continue
		}
		for _, noun := range []string{"bundle", "dev", "node", "version", "completion"} {
			if !strings.Contains(r.stdout, noun) {
				t.Errorf("the %s script never offers %s", shell, noun)
			}
		}
		script := writeFile(t, dir, "ruralz."+shell, []byte(r.stdout))
		check, ok := checks[shell]
		if !ok {
			continue
		}
		path, err := exec.LookPath(check[0])
		if err != nil {
			t.Logf("%s is not on PATH: the %s script's syntax is not checked here", check[0], shell)
			continue
		}
		cmd := exec.CommandContext(t.Context(), path, append(check[1:], script)...) //nolint:gosec // G204: a shell from PATH checking the script the test wrote
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Errorf("%s %v: %v\n%s", path, check[1:], err, out)
		}
	}
	if bash, err := exec.LookPath("bash"); err == nil {
		// Complete "ruralz bun" and "ruralz bundle " through the script.
		program := `source "$1"
COMP_WORDS=(ruralz bun); COMP_CWORD=1; _ruralz ruralz bun ruralz; printf 'noun %s\n' "${COMPREPLY[@]}"
COMP_WORDS=(ruralz bundle ""); COMP_CWORD=2; _ruralz ruralz "" bundle; printf 'verb %s\n' "${COMPREPLY[@]}"`
		cmd := exec.CommandContext(t.Context(), bash, "-c", program, "complete", filepath.Join(dir, "ruralz.bash")) //nolint:gosec // G204: bash running the script the test wrote
		out, err := cmd.CombinedOutput()
		got := lines(string(out))
		if err != nil || !slices.Contains(got, "noun bundle") || !slices.Contains(got, "verb validate") || !slices.Contains(got, "verb render") ||
			!slices.Contains(got, "verb diff") || !slices.Contains(got, "verb build") {
			t.Errorf("bash completion through the script: %v\n%s\nwant the noun bundle and its verbs", err, out)
		}
	}
	if r := cli(t, nil, cliOptions{}, "completion", "tcsh"); r.code != 2 || !strings.Contains(r.stderr, `unsupported SHELL "tcsh"`) {
		t.Errorf("%s\nwant exit 2 for another shell", r)
	}
}
