// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package binaries

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the controllable-clock build (BuildOptions.TestClock): the tag
// reaches ruralzd and only ruralzd, Set.TestClock and FromDir report it,
// and the build compiles the repository's real internal/clock pair
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope").

// clockProbe is the fixture ruralzd: it starts as ruralzd's Run does, with
// the process clock first and exit 2 without it, then waits for a 10 s
// timer that only the harness's clock commands fire. The test build arms
// the timer before it writes its build line, so a harness that advances
// the clock once it read the line never arms the timer past an advance.
const clockProbe = `package main

import (
	"os"
	"strconv"
	"time"

	"` + Module + `/internal/clock"
)

func main() {
	c, err := clock.Process()
	if err != nil {
		_, _ = os.Stderr.WriteString(err.Error() + "\n")
		os.Exit(2)
	}
	if !clock.TestClockBuild {
		_, _ = os.Stdout.WriteString("testclock " + strconv.FormatBool(clock.TestClockBuild) + "\n")
		return
	}
	start := c.Now()
	timer := c.NewTimer(10 * time.Second)
	_, _ = os.Stdout.WriteString("testclock " + strconv.FormatBool(clock.TestClockBuild) + "\n")
	<-timer.C()
	_, _ = os.Stdout.WriteString("fired after " + c.Since(start).String() + "\n")
}
`

// cliProbe is the fixture ruralz, which reports the build it got.
const cliProbe = `package main

import (
	"os"
	"strconv"

	"` + Module + `/internal/clock"
)

func main() {
	_, _ = os.Stdout.WriteString("ruralz testclock " + strconv.FormatBool(clock.TestClockBuild) + "\n")
}
`

// clockFixture is fixture with a copy of the repository's internal/clock
// sources, the tagged pair included, and the two probes as its commands.
func clockFixture(t *testing.T) string {
	t.Helper()
	root := fixture(t)
	repo, err := RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	sources, err := filepath.Glob(filepath.Join(repo, "internal", "clock", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(root, "internal", "clock")
	if err := os.MkdirAll(dst, 0o750); err != nil {
		t.Fatal(err)
	}
	copied := 0
	for _, src := range sources {
		if strings.HasSuffix(src, "_test.go") {
			continue
		}
		data, err := os.ReadFile(src) //nolint:gosec // G304: a repository source file
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, filepath.Base(src)), data, 0o600); err != nil { //nolint:gosec // G703: a file in the test's temporary directory
			t.Fatal(err)
		}
		copied++
	}
	if copied < 2 {
		t.Fatalf("copied %d internal/clock sources, want the clock and its process pair", copied)
	}
	for name, src := range map[string]string{"cmd/ruralzd/main.go": clockProbe, "cmd/ruralz/main.go": cliProbe} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(name)), []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestBuildTestClockTagsRuralzdOnly(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	root := clockFixture(t)
	dir := filepath.Join(t.TempDir(), "testclock")
	set, err := Build(t.Context(), root, dir, BuildOptions{TestClock: true})
	if err != nil {
		t.Fatal(err)
	}
	if !set.TestClock {
		t.Fatal("Set.TestClock = false for a TestClock build")
	}
	if got := settings(t, set.Ruralzd)["-tags"]; got != TestClockTag {
		t.Errorf("ruralzd -tags = %q, want %q", got, TestClockTag)
	}
	if got := settings(t, set.Ruralz)["-tags"]; got != "" {
		t.Errorf("ruralz -tags = %q, want none", got)
	}
	// The release flags stay.
	if s := settings(t, set.Ruralzd); s["CGO_ENABLED"] != "0" || s["-trimpath"] != "true" {
		t.Errorf("ruralzd build settings = %v", s)
	}
	if got := run(t, set.Ruralz); got != "ruralz testclock false" {
		t.Errorf("ruralz output = %q, want the release clock", got)
	}
	if ok, err := HasTestClock(set.Ruralzd); err != nil || !ok {
		t.Errorf("HasTestClock(ruralzd) = %v, %v; want true", ok, err)
	}
	if ok, err := HasTestClock(set.Ruralz); err != nil || ok {
		t.Errorf("HasTestClock(ruralz) = %v, %v; want false", ok, err)
	}
	if got, err := FromDir(dir); err != nil || !got.TestClock || got.Ruralzd != set.Ruralzd {
		t.Errorf("FromDir(testclock build) = %+v, %v; want TestClock", got, err)
	}

	release, err := Build(t.Context(), root, filepath.Join(t.TempDir(), "release"), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if release.TestClock {
		t.Error("Set.TestClock = true for a release build")
	}
	if got := settings(t, release.Ruralzd)["-tags"]; got != "" {
		t.Errorf("release ruralzd -tags = %q, want none", got)
	}
	// Without the tag Process is the real clock: no descriptor needed.
	if got := run(t, release.Ruralzd); got != "testclock false" {
		t.Errorf("release ruralzd output = %q", got)
	}
	if got, err := FromDir(filepath.Dir(release.Ruralzd)); err != nil || got.TestClock {
		t.Errorf("FromDir(release build) = %+v, %v; want no TestClock", got, err)
	}
	if _, err := HasTestClock(filepath.Join(root, "go.mod")); err == nil {
		t.Error("HasTestClock(not a Go binary) = nil error")
	}
}

// With RURALZ_TEST_BIN_DIR set, a TestClock request takes the prebuilt
// binaries as they are, and Set.TestClock reports their kind, so suites
// skip the controllable-clock cases against a release build.
func TestBinDirTakesEitherBuild(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	root := clockFixture(t)
	tagged, err := Build(t.Context(), root, filepath.Join(t.TempDir(), "testclock"), BuildOptions{TestClock: true})
	if err != nil {
		t.Fatal(err)
	}
	release, err := Build(t.Context(), root, filepath.Join(t.TempDir(), "release"), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name string
		set  Set
	}{{"controllable-clock build", tagged}, {"release build", release}} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(EnvBinDir, filepath.Dir(tt.set.Ruralzd))
			// Nothing is built: the repository root does not exist.
			got, err := Build(t.Context(), filepath.Join(t.TempDir(), "no-repo"), t.TempDir(), BuildOptions{TestClock: true})
			if err != nil {
				t.Fatal(err)
			}
			if got.Ruralzd != tt.set.Ruralzd || got.TestClock != tt.set.TestClock {
				t.Errorf("Build with %s = %+v, want %+v", EnvBinDir, got, tt.set)
			}
		})
	}
}

// exitCode runs bin and returns its exit code and output.
func exitCode(t *testing.T, bin string) (int, string) {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), bin).CombinedOutput()
	if err == nil {
		return 0, string(out)
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("%s: %v", bin, err)
	}
	return ee.ExitCode(), string(out)
}
