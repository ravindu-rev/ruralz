// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package binaries builds the Ruralz binaries under test (11 req 32): once
// per test binary, from TestMain, with the release flags (CGO_ENABLED=0,
// -trimpath and -ldflags "-s -w -X ..."; R-52 makes the stripped binary the
// shipped one), or taken prebuilt from RURALZ_TEST_BIN_DIR. Variants for
// Zero-Downtime Upgrade refusal and rollback tests (11 req 45, 47) are
// built with go build -overlay, replacing single source files.
//
// Nothing is cached in package state: TestMain builds a Set and passes it
// down.
//
//	func TestMain(m *testing.M) {
//		dir, _ := os.MkdirTemp("", "ruralz-bin-")
//		root, _ := binaries.RepoRoot(".")
//		bins, err := binaries.Build(context.Background(), root, dir, binaries.BuildOptions{})
//		...
//	}
package binaries

import (
	"context"
	"encoding/json"
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
// (for example dist/linux_amd64); Build uses them instead of building when
// no variant is requested.
const EnvBinDir = "RURALZ_TEST_BIN_DIR"

// Build metadata defaults.
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

// Set holds absolute paths of the built binaries; a binary not built is "".
type Set struct {
	Ruralzd string
	Ruralz  string
}

// BuildOptions selects a build.
type BuildOptions struct {
	// Version, Commit and Flavor are set with -X in internal/buildinfo;
	// defaults DefaultVersion, DefaultCommit and DefaultFlavor.
	Version, Commit, Flavor string
	// Overlay replaces source files for go build -overlay: repository
	// relative file (slash-separated) to replacement path; an empty
	// replacement deletes the file from the build.
	Overlay map[string]string
	// GOOS and GOARCH default to the host.
	GOOS, GOARCH string
	// Commands lists the cmd/ directories to build; default ruralzd and
	// ruralz.
	Commands []string
	// GoCmd is the go command; default $GOROOT/bin/go when GOROOT is set,
	// else "go" from PATH.
	GoCmd string
	// Env is appended to the build environment.
	Env []string
}

// variant reports whether o asks for anything a prebuilt release binary
// cannot provide.
func (o BuildOptions) variant() bool {
	return len(o.Overlay) > 0 || o.Version != "" || o.Commit != "" || o.Flavor != "" ||
		(o.GOOS != "" && o.GOOS != runtime.GOOS) || (o.GOARCH != "" && o.GOARCH != runtime.GOARCH)
}

// LDFlags returns the release -ldflags value: stripped (-s -w, R-52) with
// the build metadata set in internal/buildinfo, matching the Makefile's
// LDFLAGS.
func LDFlags(version, commit, flavor string) string {
	bi := Module + "/internal/buildinfo."
	return "-s -w -X " + bi + "version=" + version + " -X " + bi + "commit=" + commit + " -X " + bi + "flavor=" + flavor
}

// Build compiles the commands under repoRoot/cmd into dir with
// CGO_ENABLED=0, -trimpath and LDFlags. When RURALZ_TEST_BIN_DIR is set
// and o asks for no variant (no Overlay, no build metadata, host
// platform), the prebuilt binaries there are returned instead.
func Build(ctx context.Context, repoRoot, dir string, o BuildOptions) (Set, error) {
	cmds := o.Commands
	if len(cmds) == 0 {
		cmds = []string{Ruralzd, Ruralz}
	}
	if d := os.Getenv(EnvBinDir); d != "" && !o.variant() {
		return FromDir(d, cmds...)
	}
	version, commit, flavor := orDefault(o.Version, DefaultVersion), orDefault(o.Commit, DefaultCommit), orDefault(o.Flavor, DefaultFlavor)
	for name, v := range map[string]string{"Version": version, "Commit": commit, "Flavor": flavor} {
		if strings.ContainsAny(v, " \t\n'\"`\\") {
			return Set{}, fmt.Errorf("binaries: %s %q contains whitespace or quotes", name, v)
		}
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
	goos, goarch := orDefault(o.GOOS, runtime.GOOS), orDefault(o.GOARCH, runtime.GOARCH)
	args := []string{"build", "-trimpath", "-ldflags", LDFlags(version, commit, flavor), "-o", out + string(filepath.Separator)}
	if len(o.Overlay) > 0 {
		path, err := writeOverlay(root, o.Overlay)
		if err != nil {
			return Set{}, err
		}
		defer func() { _ = os.Remove(path) }()
		args = append(args, "-overlay", path)
	}
	for _, c := range cmds {
		args = append(args, "./cmd/"+c)
	}
	goCmd, err := goCommand(o.GoCmd)
	if err != nil {
		return Set{}, err
	}
	cmd := exec.CommandContext(ctx, goCmd, args...) //nolint:gosec // G204: the go command with arguments built above
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+goos, "GOARCH="+goarch)
	cmd.Env = append(cmd.Env, o.Env...)
	if outb, err := cmd.CombinedOutput(); err != nil {
		return Set{}, fmt.Errorf("binaries: go %s: %w\n%s", strings.Join(args, " "), err, outb)
	}
	return collect(out, goos, cmds)
}

// FromDir returns the binaries named cmds in dir (with ".exe" when the
// files carry it), failing when one is missing or not a regular file.
func FromDir(dir string, cmds ...string) (Set, error) {
	if len(cmds) == 0 {
		cmds = []string{Ruralzd, Ruralz}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Set{}, fmt.Errorf("binaries: %w", err)
	}
	goos := runtime.GOOS
	if _, err := os.Stat(filepath.Join(abs, cmds[0]+".exe")); err == nil {
		goos = "windows"
	}
	return collect(abs, goos, cmds)
}

func collect(dir, goos string, cmds []string) (Set, error) {
	var s Set
	for _, c := range cmds {
		p := filepath.Join(dir, c)
		if goos == "windows" {
			p += ".exe"
		}
		st, err := os.Stat(p)
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

// writeOverlay writes the go build -overlay JSON file and returns its path.
func writeOverlay(root string, overlay map[string]string) (string, error) {
	replace := map[string]string{}
	keys := make([]string, 0, len(overlay))
	for k := range overlay {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, rel := range keys {
		local := filepath.FromSlash(rel)
		if rel == "" || filepath.IsAbs(local) || !filepath.IsLocal(local) {
			return "", fmt.Errorf("binaries: overlay key %q is not a path inside the repository", rel)
		}
		repl := overlay[rel]
		if repl != "" {
			abs, err := filepath.Abs(repl)
			if err != nil {
				return "", fmt.Errorf("binaries: overlay %q: %w", rel, err)
			}
			if _, err := os.Stat(abs); err != nil {
				return "", fmt.Errorf("binaries: overlay %q: %w", rel, err)
			}
			repl = abs
		}
		replace[filepath.Join(root, local)] = repl
	}
	b, err := json.Marshal(map[string]any{"Replace": replace})
	if err != nil {
		return "", fmt.Errorf("binaries: overlay: %w", err)
	}
	f, err := os.CreateTemp("", "ruralz-overlay-*.json")
	if err != nil {
		return "", fmt.Errorf("binaries: overlay: %w", err)
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("binaries: overlay: %w", err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(f.Name())
		return "", fmt.Errorf("binaries: overlay: %w", err)
	}
	return f.Name(), nil
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

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
