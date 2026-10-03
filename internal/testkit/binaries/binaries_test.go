// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package binaries

import (
	"context"
	"debug/buildinfo"
	"debug/elf"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Tests for 11 test plan item 12 (binaries: an overlay build produces a
// binary whose version output differs as injected), 11 req 32 (release
// flags CGO_ENABLED=0, -trimpath, -ldflags -X, or RURALZ_TEST_BIN_DIR) and
// R-52 (-s -w). They build a small fixture module with the Ruralz module
// path, so they stay hermetic and fast; binaries_integration_test.go builds
// the real cmd/ruralzd and cmd/ruralz.

// fixture writes a module shaped like the repository: internal/buildinfo
// with the three linker-set variables, and two commands printing them.
func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"go.mod": "module " + Module + "\n\ngo 1.26\n",
		"internal/buildinfo/buildinfo.go": `package buildinfo

var (
	version = "0.0.0-dev"
	commit  = "none"
	flavor  = "none"
)

// String reports the build metadata and the overlay marker.
func String() string { return version + " " + commit + " " + flavor + " " + Marker }
`,
		"internal/buildinfo/marker.go": "package buildinfo\n\n// Marker is replaced by overlay builds.\nconst Marker = \"base\"\n",
		"cmd/ruralzd/main.go": `package main

import (
	"os"

	"` + Module + `/internal/buildinfo"
)

func main() { _, _ = os.Stdout.WriteString("ruralzd " + buildinfo.String() + "\n") }
`,
		"cmd/ruralz/main.go": `package main

import (
	"os"

	"` + Module + `/internal/buildinfo"
)

func main() { _, _ = os.Stdout.WriteString("ruralz " + buildinfo.String() + "\n") }
`,
	}
	for name, data := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func run(t *testing.T, bin string) string {
	t.Helper()
	out, err := exec.CommandContext(t.Context(), bin).CombinedOutput()
	if err != nil {
		t.Fatalf("%s: %v\n%s", bin, err, out)
	}
	return strings.TrimSpace(string(out))
}

func settings(t *testing.T, bin string) map[string]string {
	t.Helper()
	info, err := buildinfo.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, s := range info.Settings {
		out[s.Key] = s.Value
	}
	return out
}

func TestBuildReleaseFlags(t *testing.T) { // 11 req 32; R-52
	t.Setenv(EnvBinDir, "")
	root := fixture(t)
	dir := filepath.Join(t.TempDir(), "bin")
	set, err := Build(t.Context(), root, dir, BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if set.Ruralzd != filepath.Join(dir, "ruralzd") || set.Ruralz != filepath.Join(dir, "ruralz") {
		t.Fatalf("Set = %+v", set)
	}
	if got := run(t, set.Ruralz); got != "ruralz 0.0.0-test unknown default base" {
		t.Fatalf("ruralz output = %q", got)
	}
	if got := run(t, set.Ruralzd); got != "ruralzd 0.0.0-test unknown default base" {
		t.Fatalf("ruralzd output = %q", got)
	}
	s := settings(t, set.Ruralzd)
	if s["CGO_ENABLED"] != "0" || s["-trimpath"] != "true" || s["GOOS"] != runtime.GOOS || s["GOARCH"] != runtime.GOARCH {
		t.Fatalf("build settings = %v", s)
	}
	// Go omits -ldflags from the build information under -trimpath; check
	// its effect instead: -s drops the symbol table, -w the DWARF data.
	if runtime.GOOS == "linux" {
		f, err := elf.Open(set.Ruralzd)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		if _, err := f.Symbols(); !errors.Is(err, elf.ErrNoSymbols) {
			t.Fatalf("symbol table present (err %v): -s missing", err)
		}
		if f.Section(".debug_info") != nil {
			t.Fatal("DWARF present: -w missing")
		}
	}
	if !strings.HasPrefix(LDFlags("v", "c", "f"), "-s -w -X "+Module+"/internal/buildinfo.version=v ") {
		t.Fatalf("LDFlags = %q", LDFlags("v", "c", "f"))
	}
	if set.Path(Ruralzd) != set.Ruralzd || set.Path(Ruralz) != set.Ruralz || set.Path("x") != "" {
		t.Fatal("Set.Path")
	}
}

func TestBuildOverlayVariant(t *testing.T) { // 11 test plan item 12; 11 req 45, 47
	t.Setenv(EnvBinDir, t.TempDir()) // ignored: the build asks for a variant
	root := fixture(t)
	repl := filepath.Join(t.TempDir(), "marker.go")
	if err := os.WriteFile(repl, []byte("package buildinfo\n\nconst Marker = \"overlay\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	base, err := Build(t.Context(), root, t.TempDir(), BuildOptions{Version: "0.1.0-e2e.a", Commit: "aaaa", Flavor: "default", Commands: []string{Ruralz}})
	if err != nil {
		t.Fatal(err)
	}
	variant, err := Build(t.Context(), root, t.TempDir(), BuildOptions{
		Version:  "0.1.0-e2e.b",
		Overlay:  map[string]string{"internal/buildinfo/marker.go": repl},
		Commands: []string{Ruralz},
	})
	if err != nil {
		t.Fatal(err)
	}
	if base.Ruralzd != "" || variant.Ruralzd != "" {
		t.Fatalf("ruralzd built although not requested: %+v %+v", base, variant)
	}
	a, b := run(t, base.Ruralz), run(t, variant.Ruralz)
	if a != "ruralz 0.1.0-e2e.a aaaa default base" || b != "ruralz 0.1.0-e2e.b unknown default overlay" {
		t.Fatalf("outputs = %q, %q", a, b)
	}
	// The overlay file is removed after the build.
	matches, _ := filepath.Glob(filepath.Join(os.TempDir(), "ruralz-overlay-*.json"))
	for _, m := range matches {
		if data, err := os.ReadFile(m); err == nil && strings.Contains(string(data), repl) { //nolint:gosec // G304: temp files
			t.Fatalf("overlay file %s left behind", m)
		}
	}
}

func TestBuildOverlayDelete(t *testing.T) {
	root := fixture(t)
	// Deleting the marker file breaks the build: the overlay reaches go.
	_, err := Build(t.Context(), root, t.TempDir(), BuildOptions{Overlay: map[string]string{"internal/buildinfo/marker.go": ""}})
	if err == nil || !strings.Contains(err.Error(), "Marker") {
		t.Fatalf("Build without marker.go = %v, want an undefined Marker error", err)
	}
}

func TestBuildCrossPlatform(t *testing.T) {
	root := fixture(t)
	set, err := Build(t.Context(), root, t.TempDir(), BuildOptions{GOOS: "linux", GOARCH: "arm64", Commands: []string{Ruralzd}})
	if err != nil {
		t.Fatal(err)
	}
	if s := settings(t, set.Ruralzd); s["GOARCH"] != "arm64" || s["GOOS"] != "linux" {
		t.Fatalf("cross build settings = %v", s)
	}
	win, err := Build(t.Context(), root, t.TempDir(), BuildOptions{GOOS: "windows", GOARCH: "amd64", Commands: []string{Ruralz}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(win.Ruralz, "ruralz.exe") {
		t.Fatalf("windows binary = %s", win.Ruralz)
	}
	got, err := FromDir(filepath.Dir(win.Ruralz), Ruralz)
	if err != nil || got.Ruralz != win.Ruralz {
		t.Fatalf("FromDir(windows) = %+v, %v", got, err)
	}
}

func TestBinDirShortCircuit(t *testing.T) { // 11 req 32: RURALZ_TEST_BIN_DIR
	dir := t.TempDir()
	for _, n := range []string{"ruralzd", "ruralz"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o700); err != nil { //nolint:gosec // G306: an executable stand-in
			t.Fatal(err)
		}
	}
	t.Setenv(EnvBinDir, dir)
	// The repository root is not even read: nothing is built.
	set, err := Build(t.Context(), filepath.Join(dir, "no-repo"), t.TempDir(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if set.Ruralzd != filepath.Join(dir, "ruralzd") || set.Ruralz != filepath.Join(dir, "ruralz") {
		t.Fatalf("Set = %+v", set)
	}
	if err := os.Remove(filepath.Join(dir, "ruralz")); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(t.Context(), dir, t.TempDir(), BuildOptions{}); err == nil {
		t.Fatal("missing prebuilt ruralz: want error")
	}
	if set, err := Build(t.Context(), dir, t.TempDir(), BuildOptions{Commands: []string{Ruralzd}}); err != nil || set.Ruralz != "" {
		t.Fatalf("ruralzd only = %+v, %v", set, err)
	}
	if err := os.Mkdir(filepath.Join(dir, "ruralz"), 0o750); err != nil {
		t.Fatal(err)
	}
	if _, err := FromDir(dir); err == nil {
		t.Fatal("directory named ruralz: want error")
	}
}

func TestBuildErrors(t *testing.T) {
	t.Setenv(EnvBinDir, "")
	root := fixture(t)
	ctx := context.Background()
	tests := []struct {
		name string
		o    BuildOptions
		want string
	}{
		{"version with space", BuildOptions{Version: "1 2"}, "whitespace"},
		{"commit with quote", BuildOptions{Commit: `a"b`}, "quotes"},
		{"bad command", BuildOptions{Commands: []string{"../x"}}, "bad command"},
		{"overlay escape", BuildOptions{Overlay: map[string]string{"../x.go": "/dev/null"}}, "not a path inside"},
		{"overlay absolute", BuildOptions{Overlay: map[string]string{"/etc/x.go": "/dev/null"}}, "not a path inside"},
		{"overlay missing replacement", BuildOptions{Overlay: map[string]string{"x.go": filepath.Join(root, "missing.go")}}, "missing.go"},
		{"no such command", BuildOptions{Commands: []string{"nope"}}, "go build"},
		{"bad go command", BuildOptions{GoCmd: filepath.Join(root, "no-go")}, "no-go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Build(ctx, root, t.TempDir(), tt.o)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Build err = %v, want %q", err, tt.want)
			}
		})
	}
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Build(ctx, root, filepath.Join(blocker, "sub"), BuildOptions{}); err == nil {
		t.Fatal("output under a file: want error")
	}
}

func TestGoCommand(t *testing.T) {
	if p, err := goCommand("/x/go"); err != nil || p != "/x/go" {
		t.Fatalf("explicit = %s, %v", p, err)
	}
	gr := t.TempDir()
	t.Setenv("GOROOT", gr)
	if err := os.MkdirAll(filepath.Join(gr, "bin"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gr, "bin", "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if p, err := goCommand(""); err != nil || p != filepath.Join(gr, "bin", "go") {
		t.Fatalf("GOROOT go = %s, %v", p, err)
	}
	t.Setenv("GOROOT", "")
	t.Setenv("PATH", t.TempDir())
	if _, err := goCommand(""); err == nil {
		t.Fatal("no go on PATH: want error")
	}
}

func TestRepoRoot(t *testing.T) {
	root, err := RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "cmd", "ruralzd", "main.go")); err != nil {
		t.Fatalf("RepoRoot = %s: %v", root, err)
	}
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "go.mod"), []byte("module example.com/x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RepoRoot(other); err == nil {
		t.Fatal("outside the module: want error")
	}
	if got := modulePath([]byte("// c\nmodule \"" + Module + "\"\n")); got != Module {
		t.Fatalf("modulePath = %q", got)
	}
	if got := modulePath([]byte("go 1.26\n")); got != "" {
		t.Fatalf("modulePath without module = %q", got)
	}
}
