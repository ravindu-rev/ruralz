// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/launch"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// Tests for ruralz dev run that launch no ruralzd: the usage errors, the
// inputs refused before the launch, and the watch over a first render
// that never becomes valid. The cases with the real ruralzd are
// integration-tagged (run_integration_test.go).

// quickstart is the quickstart Bundle of docs/operations/01-deployment-topologies.md.
const quickstart = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: quickstart
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: hello
spec:
  protocol: http
  endpoints:
    - address: "127.0.0.1:9000"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: hello
spec:
  match:
    path: {prefix: /}
  upstreams:
    - name: hello
`

// selfBinary returns the test binary, an executable regular file that
// stands where --ruralzd must name one for the cases that never launch.
func selfBinary(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// runOptions are testOptions with a launcher that finds no ruralzd beside
// ruralz.
func runOptions(t *testing.T, clk *clocktest.Fake) Options {
	t.Helper()
	o := testOptions(clk)
	empty := t.TempDir()
	o.Launch.Executable = func() (string, error) { return filepath.Join(empty, "ruralz"), nil }
	o.Notify = func(chan<- os.Signal, ...os.Signal) {}
	o.StopNotify = func(chan<- os.Signal) {}
	return o
}

// TestRunUsage checks the usage errors, exit 2 before any work.
func TestRunUsage(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "bundle.yaml", quickstart)
	envs := writeFile(t, dir, "environments.yaml", "apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: dev\nspec: {}\n")
	for _, c := range []struct {
		args   []string
		stderr string
	}{
		{[]string{src, "--env", "dev"}, "--env needs --environments: fetching Environments from Ruralz Control is Planned (M2)"},
		{[]string{src, "--env=", "--environments", envs}, "--env needs an Environment name"},
		{[]string{src, "--environments", envs}, "--environments needs --env: ruralz dev run renders one Environment"},
		{[]string{src, "--environments="}, "--environments needs a file"},
		{[]string{src, "--secret-overrides="}, "--secret-overrides needs a file"},
		{[]string{src, "--ruralzd="}, "--ruralzd needs a path"},
		{[]string{"-"}, "DIR cannot be -: ruralz dev run watches its source, so it takes a directory or a file"},
		{[]string{src, "--ready-timeout", "0"}, `invalid value "0" for --ready-timeout: must be greater than zero`},
		{[]string{src, "--output", "yaml"}, `unsupported --output "yaml" (want text or json)`},
	} {
		r := execute(t, runOptions(t, clocktest.New(epoch())), nil, append([]string{"dev", "run"}, c.args...)...)
		r.want(t, strings.Join(c.args, " "), command.ExitNoResult, "", "ruralz dev run: "+c.stderr+"\nRun 'ruralz dev run --help' for usage.\n")
	}
}

// TestRunRefusedInputs checks the inputs refused before the launch, exit
// 2: a missing source, an environments file without an Environment, an
// unusable secret overrides file, no ruralzd to launch.
func TestRunRefusedInputs(t *testing.T) {
	dir := t.TempDir()
	src := writeFile(t, dir, "bundle.yaml", quickstart)
	missing := filepath.Join(dir, "absent")
	noEnv := writeFile(t, dir, "empty-envs.yaml", "")
	badOverrides := writeFile(t, dir, "overrides.yaml", "nocolon: x\n")
	empty := t.TempDir()
	// The file name the search looks for on this platform.
	ruralzd := "ruralzd"
	if runtime.GOOS == "windows" {
		ruralzd += ".exe"
	}
	for _, c := range []struct {
		name   string
		args   []string
		prefix string
	}{
		{"missing source", []string{missing}, "ruralz dev run: cannot read " + missing + ": "},
		{"no Environment", []string{src, "--env", "dev", "--environments", noEnv}, "ruralz dev run: no Environment in " + noEnv + "\n"},
		{"unusable overrides", []string{src, "--secret-overrides", badOverrides}, "ruralz dev run: --secret-overrides " + badOverrides + ": "},
		{"no ruralzd", []string{src}, "ruralz dev run: no " + ruralzd + " beside ruralz in " + empty + " or on PATH; name it with --ruralzd PATH\n"},
		{"missing --ruralzd", []string{src, "--ruralzd", missing}, "ruralz dev run: --ruralzd " + missing + ": "},
	} {
		o := runOptions(t, clocktest.New(epoch()))
		o.Launch.Executable = func() (string, error) { return filepath.Join(empty, "ruralz"), nil }
		r := execute(t, o, []string{"PATH=" + empty}, append([]string{"dev", "run"}, c.args...)...)
		if r.code != command.ExitNoResult || r.stdout != "" || !strings.HasPrefix(r.stderr, c.prefix) {
			t.Errorf("%s = exit %d, stdout %q, stderr %q; want 2 and %q", c.name, r.code, r.stdout, r.stderr, c.prefix)
		}
	}
}

// TestRunWatchesInvalidFirstRender checks that a first render that fails
// prints its diagnostics and the command keeps watching, launching
// nothing: a settled change renders again; Ctrl-C ends the watch with exit
// 130 and the private directory is removed.
func TestRunWatchesInvalidFirstRender(t *testing.T) {
	tmp := t.TempDir()
	// os.TempDir reads TMPDIR on Unix and TMP, then TEMP, on Windows.
	t.Setenv("TMPDIR", tmp)
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	clk := clocktest.New(epoch())
	dir := t.TempDir()
	bad := strings.Replace(quickstart, "    - name: hello\n", "    - name: missing\n", 1)
	src := writeFile(t, dir, "bundle.yaml", bad)
	inv := start(t, runOptions(t, clk), nil, "dev", "run", src, "--ruralzd", selfBinary(t))
	waitFor(t, "the first diagnostics", func() bool { return strings.Count(inv.stderr.String(), "RZ-CFG-009") == 1 && clk.Pending() == 1 })
	if entries, _ := os.ReadDir(tmp); len(entries) != 1 {
		t.Errorf("private directories %v, want one", entries)
	}

	// No change: the polls render nothing.
	clk.Advance(launch.PollInterval)
	waitFor(t, "the next poll", func() bool { return clk.Pending() == 1 })
	worse := strings.Replace(bad, `"127.0.0.1:9000"`, `"127.0.0.1:9001"`, 1)
	writeFile(t, dir, "bundle.yaml", worse)
	future := clk.Now().Add(time.Hour)
	if err := os.Chtimes(src, future, future); err != nil {
		t.Fatal(err)
	}
	clk.Advance(launch.PollInterval)
	waitFor(t, "the settle wait", func() bool { return clk.Pending() == 1 })
	if n := strings.Count(inv.stderr.String(), "RZ-CFG-009"); n != 1 {
		t.Fatalf("rendered %d times before the change settled", n)
	}
	clk.Advance(launch.SettleTime)
	waitFor(t, "the second diagnostics", func() bool { return strings.Count(inv.stderr.String(), "RZ-CFG-009") == 2 })
	waitFor(t, "the next poll", func() bool { return clk.Pending() == 1 })
	inv.interrupt(os.Interrupt)
	if code := inv.wait(t); code != command.ExitInterrupted || inv.stdout.String() != "" {
		t.Errorf("dev run = exit %d, stdout %q; want 130 and nothing", code, inv.stdout)
	}
	for _, l := range lines(inv.stderr.String()) {
		if !strings.Contains(l, " error RZ-CFG-009 Route/hello ") {
			t.Errorf("stderr line %q is not the diagnostic", l)
		}
	}
	if entries, _ := os.ReadDir(tmp); len(entries) != 0 {
		t.Errorf("the private directory stays: %v", entries)
	}
}

// TestRunSignalWithoutChild: a SIGTERM delivered while no ruralzd runs
// ends the command with 143.
func TestRunSignalWithoutChild(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	clk := clocktest.New(epoch())
	src := writeFile(t, t.TempDir(), "bundle.yaml", strings.Replace(quickstart, "    - name: hello\n", "    - name: missing\n", 1))
	o := runOptions(t, clk)
	sigs := make(chan chan<- os.Signal, 1)
	o.Notify = func(c chan<- os.Signal, _ ...os.Signal) { sigs <- c }
	inv := start(t, o, nil, "dev", "run", src, "--ruralzd", selfBinary(t))
	var ch chan<- os.Signal
	select {
	case ch = <-sigs:
	case <-time.After(30 * time.Second):
		t.Fatal("dev run registered no signal channel")
	}
	waitFor(t, "the watch", func() bool { return clk.Pending() == 1 })
	ch <- syscall.SIGTERM
	if code := inv.wait(t); code != command.ExitTerminated {
		t.Errorf("dev run = exit %d, want 143", code)
	}
}

// TestRunFailureColors: on a terminal that takes color, a failed render's
// severities are colored; NO_COLOR keeps them plain.
func TestRunFailureColors(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	src := writeFile(t, t.TempDir(), "bundle.yaml", strings.Replace(quickstart, "    - name: hello\n", "    - name: missing\n", 1))
	for _, c := range []struct {
		environ []string
		want    string
	}{
		{[]string{"TERM=xterm"}, " \x1b[31merror\x1b[0m RZ-CFG-009 "},
		{[]string{"NO_COLOR=1"}, " error RZ-CFG-009 "},
	} {
		clk := clocktest.New(epoch())
		o := runOptions(t, clk)
		o.terminal = func(io.Writer) bool { return true }
		inv := start(t, o, c.environ, "dev", "run", src, "--ruralzd", selfBinary(t))
		waitFor(t, "the diagnostics", func() bool { return strings.Contains(inv.stderr.String(), "RZ-CFG-009") && clk.Pending() == 1 })
		inv.interrupt(os.Interrupt)
		inv.wait(t)
		if !strings.Contains(inv.stderr.String(), c.want) {
			t.Errorf("%q: stderr %q lacks %q", c.environ, inv.stderr, c.want)
		}
	}
}

// TestShellWord checks the quoting of the dev tap command line.
func TestShellWord(t *testing.T) {
	for in, want := range map[string]string{ //nolint:gosec // G101: token file paths, not credentials.
		"http://127.0.0.1:9901":   "http://127.0.0.1:9901",
		"/tmp/ruralz-dev-1/token": "/tmp/ruralz-dev-1/token",
		"/tmp/a b/token":          "'/tmp/a b/token'",
		"/tmp/it's/token":         `'/tmp/it'\''s/token'`,
		"":                        "''",
		"/tmp/$HOME":              "'/tmp/$HOME'",
	} {
		if got := shellWord(in); got != want {
			t.Errorf("shellWord(%q) = %q, want %q", in, got, want)
		}
	}
}
