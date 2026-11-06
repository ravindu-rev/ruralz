// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package binaries builds the Ruralz binaries under test: once per test
// binary, from TestMain, with the release flags (CGO_ENABLED=0, -trimpath
// and -ldflags "-s -w -X ..."; the shipped binary is the stripped one), or
// taken prebuilt from RURALZ_TEST_BIN_DIR.
//
// BuildOptions.TestClock builds ruralzd as the controllable-clock test
// build the correctness suites drive
// (docs/engineering/03-testing-and-quality-strategy.md "Hermetic rule
// scope"): the release flags plus the build tag TestClockTag, which picks
// the commanded process clock in internal/clock and changes nothing else.
// It is never released: release tooling refuses every binary built with a
// tag. ruralz is never built with it. Set.TestClock reports which kind of
// ruralzd a Set holds, read from the binary's build information for a
// prebuilt one, so a suite run against a release build in
// RURALZ_TEST_BIN_DIR skips the cases that need the controllable clock.
//
// Nothing is cached in package state: TestMain builds a Set and passes it
// down.
//
//	func TestMain(m *testing.M) {
//		dir, _ := os.MkdirTemp("", "ruralz-bin-")
//		root, _ := binaries.RepoRoot(".")
//		bins, err := binaries.Build(context.Background(), root, dir, binaries.BuildOptions{TestClock: true})
//		...
//	}
package binaries

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
)

// Module is the Ruralz module path.
const Module = "github.com/ravindu-rev/ruralz"

// EnvBinDir names a directory holding prebuilt ruralzd and ruralz binaries
// (for example dist/linux_amd64); Build uses them instead of building.
const EnvBinDir = "RURALZ_TEST_BIN_DIR"

// The build metadata Build sets.
const (
	DefaultVersion = "0.0.0-test"
	DefaultCommit  = "unknown"
	DefaultFlavor  = "default"
)

// Binary names.
const (
	Ruralzd = "ruralzd"
	Ruralz  = "ruralz"
)

// TestClockTag is the build tag of the controllable-clock ruralzd build;
// it sits only on the pair of internal/clock files that picks the process
// clock.
const TestClockTag = "ruralz_testclock"

// Set holds absolute paths of the built binaries; a binary not built is "".
type Set struct {
	Ruralzd string
	Ruralz  string
	// TestClock reports that Ruralzd is the controllable-clock build: it
	// follows the process harness's clock commands on file descriptor 3
	// and exits 2 when started without them.
	TestClock bool
}

// BuildOptions selects a build.
type BuildOptions struct {
	// Commands lists the cmd/ directories to build; default ruralzd and
	// ruralz.
	Commands []string
	// GoCmd is the go command; default $GOROOT/bin/go when GOROOT is set,
	// else "go" from PATH.
	GoCmd string
	// TestClock builds ruralzd as the controllable-clock test build, with
	// the build tag TestClockTag; ruralz is built without it. With
	// RURALZ_TEST_BIN_DIR set, the prebuilt binaries are used whatever
	// their kind, and Set.TestClock reports it.
	TestClock bool
}

// LDFlags returns the release -ldflags value: stripped (-s -w) with the
// build metadata set in internal/buildinfo, matching the Makefile's
// LDFLAGS.
func LDFlags(version, commit, flavor string) string {
	bi := Module + "/internal/buildinfo."
	return "-s -w -X " + bi + "version=" + version + " -X " + bi + "commit=" + commit + " -X " + bi + "flavor=" + flavor
}

// Build compiles the commands under repoRoot/cmd into dir for the host
// platform with CGO_ENABLED=0, -trimpath and LDFlags of DefaultVersion,
// DefaultCommit and DefaultFlavor, and ruralzd also with the tag
// TestClockTag when o.TestClock is set. When RURALZ_TEST_BIN_DIR is set,
// the prebuilt binaries there are returned instead.
func Build(ctx context.Context, repoRoot, dir string, o BuildOptions) (Set, error) {
	cmds := o.Commands
	if len(cmds) == 0 {
		cmds = []string{Ruralzd, Ruralz}
	}
	if d := os.Getenv(EnvBinDir); d != "" {
		return FromDir(d, cmds...)
	}
	for _, c := range cmds {
		if c == "" || strings.ContainsAny(c, `/\`) || c == "." || c == ".." {
			return Set{}, fmt.Errorf("binaries: bad command name %q", c)
		}
	}
	root, err := filepath.Abs(repoRoot)
	if err != nil {
		return Set{}, fmt.Errorf("binaries: %w", err)
	}
	out, err := filepath.Abs(dir)
	if err != nil {
		return Set{}, fmt.Errorf("binaries: %w", err)
	}
	if err := os.MkdirAll(out, 0o750); err != nil {
		return Set{}, fmt.Errorf("binaries: %w", err)
	}
	base := []string{"build", "-trimpath", "-ldflags", LDFlags(DefaultVersion, DefaultCommit, DefaultFlavor), "-o", out + string(filepath.Separator)}
	goCmd, err := goCommand(o.GoCmd)
	if err != nil {
		return Set{}, err
	}
	// Build tags apply to a whole go build, so the controllable-clock
	// ruralzd gets its own.
	var tagged, plain []string
	for _, c := range cmds {
		if o.TestClock && c == Ruralzd {
			tagged = append(tagged, c)
		} else {
			plain = append(plain, c)
		}
	}
	for _, g := range []struct {
		tags string
		cmds []string
	}{{TestClockTag, tagged}, {"", plain}} {
		if len(g.cmds) == 0 {
			continue
		}
		args := slices.Clone(base)
		if g.tags != "" {
			args = append(args, "-tags", g.tags)
		}
		for _, c := range g.cmds {
			args = append(args, "./cmd/"+c)
		}
		cmd := exec.CommandContext(ctx, goCmd, args...) //nolint:gosec // G204: the go command with arguments built above
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
		if outb, err := cmd.CombinedOutput(); err != nil {
			return Set{}, fmt.Errorf("binaries: go %s: %w\n%s", strings.Join(args, " "), err, outb)
		}
	}
	set, err := collect(out, runtime.GOOS, cmds)
	if err != nil {
		return Set{}, err
	}
	set.TestClock = len(tagged) > 0
	return set, nil
}

// FromDir returns the binaries named cmds in dir (with ".exe" when the
// files carry it), failing when one is missing or not a regular file.
// Set.TestClock reports whether the ruralzd there was built with
// TestClockTag; a file without Go build information, such as a stand-in
// script, counts as a build without it.
func FromDir(dir string, cmds ...string) (Set, error) {
	if len(cmds) == 0 {
		cmds = []string{Ruralzd, Ruralz}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Set{}, fmt.Errorf("binaries: %w", err)
	}
	goos := runtime.GOOS
	if _, err := os.Stat(filepath.Join(abs, cmds[0]+".exe")); err == nil { //nolint:gosec // G703: a directory the calling test supplies
		goos = "windows"
	}
	s, err := collect(abs, goos, cmds)
	if err != nil {
		return Set{}, err
	}
	if s.Ruralzd != "" {
		s.TestClock, _ = HasTestClock(s.Ruralzd)
	}
	return s, nil
}

// HasTestClock reports whether the Go binary at path was built with the
// tag TestClockTag, read from its build information. It fails for a file
// that is not a Go binary.
func HasTestClock(path string) (bool, error) {
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("binaries: %w", err)
	}
	for _, s := range info.Settings {
		if s.Key != "-tags" {
			continue
		}
		for tag := range strings.SplitSeq(s.Value, ",") {
			if tag == TestClockTag {
				return true, nil
			}
		}
	}
	return false, nil
}

func collect(dir, goos string, cmds []string) (Set, error) {
	var s Set
	for _, c := range cmds {
		p := filepath.Join(dir, c)
		if goos == "windows" {
			p += ".exe"
		}
		st, err := os.Stat(p) //nolint:gosec // G703: a binary name under a directory the calling test supplies
		if err != nil {
			return Set{}, fmt.Errorf("binaries: %w", err)
		}
		if !st.Mode().IsRegular() {
			return Set{}, fmt.Errorf("binaries: %s is not a regular file", p)
		}
		switch c {
		case Ruralzd:
			s.Ruralzd = p
		case Ruralz:
			s.Ruralz = p
		}
	}
	return s, nil
}

// Path returns the path of the named binary in s, or "".
func (s Set) Path(name string) string {
	switch name {
	case Ruralzd:
		return s.Ruralzd
	case Ruralz:
		return s.Ruralz
	}
	return ""
}

// goCommand resolves the go command.
func goCommand(explicit string) (string, error) {
	if explicit != "" {
		return explicit, nil
	}
	if gr := os.Getenv("GOROOT"); gr != "" {
		p := filepath.Join(gr, "bin", "go")
		if runtime.GOOS == "windows" {
			p += ".exe"
		}
		if _, err := os.Stat(p); err == nil { //nolint:gosec // G703: the go command under the caller's GOROOT
			return p, nil
		}
	}
	p, err := exec.LookPath("go")
	if err != nil {
		return "", fmt.Errorf("binaries: go command not found: %w", err)
	}
	return p, nil
}

// RepoRoot walks up from start to the directory whose go.mod declares
// Module.
func RepoRoot(start string) (string, error) {
	dir, err := filepath.Abs(start)
	if err != nil {
		return "", fmt.Errorf("binaries: %w", err)
	}
	for {
		data, err := os.ReadFile(filepath.Join(dir, "go.mod")) //nolint:gosec // G304: fixed file name under a walked directory
		if err == nil && modulePath(data) == Module {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("binaries: no go.mod for " + Module + " above " + start)
		}
		dir = parent
	}
}

// modulePath returns the module directive of a go.mod file.
func modulePath(gomod []byte) string {
	for line := range strings.SplitSeq(string(gomod), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module"); ok {
			return strings.Trim(strings.TrimSpace(rest), `"`)
		}
	}
	return ""
}
