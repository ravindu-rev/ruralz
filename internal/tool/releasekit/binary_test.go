// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"debug/buildinfo"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"testing"
)

// goodFacts are the facts of a release build of ruralz for linux/amd64.
func goodFacts(version string) buildFacts {
	return buildFacts{
		Info: &buildinfo.BuildInfo{
			GoVersion: "go1.27.3",
			Path:      modulePath + "/cmd/ruralz",
			Main:      debug.Module{Path: modulePath, Version: "v" + version},
			Settings: []debug.BuildSetting{
				{Key: "-trimpath", Value: "true"},
				{Key: "CGO_ENABLED", Value: "0"},
				{Key: "GOARCH", Value: "amd64"},
				{Key: "GOOS", Value: "linux"},
				{Key: "vcs.revision", Value: fixtureCommit},
				{Key: "vcs.modified", Value: "false"},
			},
		},
		Stripped: true,
		Format:   "elf",
	}
}

func withSetting(f *buildFacts, key, value string) {
	f.Info.Settings = slices.DeleteFunc(f.Info.Settings, func(s debug.BuildSetting) bool { return s.Key == key })
	if value != "" {
		f.Info.Settings = append(f.Info.Settings, debug.BuildSetting{Key: key, Value: value})
	}
}

// TestBuildProblems: each release build rule, broken alone, gives its own
// finding, and a release build gives none.
func TestBuildProblems(t *testing.T) {
	a := artifact{"ruralz", "linux", "amd64", supportProduction}
	cases := []struct {
		name, version, want string
		change              func(*buildFacts)
	}{
		{"release build", "0.1.0", "", func(*buildFacts) {}},
		{"development build", "0.0.0-dev", "", func(f *buildFacts) {
			f.Info.Main.Version = "v0.1.1-0.20261001-abc+dirty"
			withSetting(f, "vcs.modified", "true")
		}},
		{"development build without Git", "0.0.0-dev", "", func(f *buildFacts) { withSetting(f, "vcs.revision", ""); withSetting(f, "vcs.modified", "") }},
		{"main package", "0.1.0", "main package", func(f *buildFacts) { f.Info.Path = modulePath + "/cmd/ruralzd" }},
		{"main module", "0.1.0", "main module", func(f *buildFacts) { f.Info.Main.Path = "example.com/fork" }},
		{"toolchain", "0.1.0", "want the pinned toolchain go1.27.3", func(f *buildFacts) { f.Info.GoVersion = "go1.26.8" }},
		{"GOOS", "0.1.0", "GOOS=\"darwin\"", func(f *buildFacts) { withSetting(f, "GOOS", "darwin") }},
		{"GOARCH", "0.1.0", "GOARCH=\"arm64\"", func(f *buildFacts) { withSetting(f, "GOARCH", "arm64") }},
		{"cgo", "0.1.0", "CGO_ENABLED=\"1\"", func(f *buildFacts) { withSetting(f, "CGO_ENABLED", "1") }},
		{"trimpath", "0.1.0", "-trimpath=\"\"", func(f *buildFacts) { withSetting(f, "-trimpath", "") }},
		{"GOEXPERIMENT", "0.1.0", "GOEXPERIMENT=\"jsonv2\", want none", func(f *buildFacts) { withSetting(f, "GOEXPERIMENT", "jsonv2") }},
		{"build tags", "0.1.0", "-tags=\"netgo\", want none", func(f *buildFacts) { withSetting(f, "-tags", "netgo") }},
		{"format", "0.1.0", "pe executable, want elf", func(f *buildFacts) { f.Format = "pe" }},
		{"not stripped", "0.1.0", "-s -w", func(f *buildFacts) { f.Stripped = false }},
		{"other commit", "0.0.0-dev", "built from commit", func(f *buildFacts) { withSetting(f, "vcs.revision", strings.Repeat("f", 40)) }},
		{"release without Git", "0.1.0", "no Git commit recorded", func(f *buildFacts) { withSetting(f, "vcs.revision", "") }},
		{"modified checkout", "0.1.0-rc.1", "modified checkout", func(f *buildFacts) { f.Info.Main.Version = "v0.1.0-rc.1"; withSetting(f, "vcs.modified", "true") }},
		{"module version", "0.1.0", "module version v0.0.9", func(f *buildFacts) { f.Info.Main.Version = "v0.0.9" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := goodFacts(c.version)
			c.change(&f)
			got := buildProblems(f, a, c.version, fixtureCommit, "go1.27.3")
			switch {
			case c.want == "" && len(got) > 0:
				t.Errorf("unexpected problems %q", got)
			case c.want != "" && !hasFinding(got, c.want):
				t.Errorf("problems %q lack %q", got, c.want)
			case c.want != "" && len(got) != 1:
				t.Errorf("problems %q, want exactly one", got)
			}
		})
	}
}

// buildTiny builds a one-file program for a platform, with or without
// -s -w, and returns the executable.
func buildTiny(t *testing.T, goos, goarch string, strip bool) []byte {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "go.mod"), "module example.com/tiny\n\ngo 1.26\n")
	writeFile(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	args := []string{"build", "-trimpath", "-o", "tiny"}
	if strip {
		args = append(args, "-ldflags=-s -w")
	}
	cmd := exec.CommandContext(t.Context(), "go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch, "GOFLAGS=-mod=mod", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(dir, "tiny")) //nolint:gosec // The test's own build output.
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestSymbolState: -s -w is detected on ELF, Mach-O and PE executables.
func TestSymbolState(t *testing.T) {
	for _, p := range [][3]string{{"linux", "amd64", "elf"}, {"darwin", "arm64", "macho"}, {"windows", "amd64", "pe"}} {
		for _, strip := range []bool{true, false} {
			data := buildTiny(t, p[0], p[1], strip)
			facts, err := readBuildFacts(data)
			if err != nil {
				t.Fatalf("%s/%s: %v", p[0], p[1], err)
			}
			if facts.Format != p[2] || facts.Stripped != strip {
				t.Errorf("%s/%s strip=%v: format %s stripped %v", p[0], p[1], strip, facts.Format, facts.Stripped)
			}
			if facts.Info.Path != "example.com/tiny" {
				t.Errorf("build information path %q", facts.Info.Path)
			}
		}
	}
	if _, err := readBuildFacts([]byte("not an executable")); err == nil {
		t.Error("readBuildFacts accepted a non-executable")
	}
}
