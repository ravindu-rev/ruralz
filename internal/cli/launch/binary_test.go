// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// writeBinary writes an executable regular file named ruralzd (with .exe
// on Windows) in dir and returns its path.
func writeBinary(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, binaryName())
	if err := os.WriteFile(p, []byte("binary"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, 0o700); err != nil { //nolint:gosec // G302: an executable the test runs nothing of.
		t.Fatal(err)
	}
	return p
}

// ioWithPath returns invocation streams whose environment holds PATH.
func ioWithPath(path string) command.IO {
	return command.NewIO(nil, nil, []string{"PATH=" + path}, nil, "")
}

// exeIn returns an Executable hook placing ruralz in dir.
func exeIn(dir string) func() (string, error) {
	return func() (string, error) { return filepath.Join(dir, "ruralz"), nil }
}

// TestFindRuralzdFlag: --ruralzd names the binary and skips the search;
// a missing file, a directory and, on Unix, a file without an execute bit
// exit 2 naming the flag.
func TestFindRuralzdFlag(t *testing.T) {
	dir := t.TempDir()
	bin := writeBinary(t, dir)
	beside := t.TempDir()
	writeBinary(t, beside)
	got, err := FindRuralzd(ioWithPath(""), bin, Options{Executable: exeIn(beside)})
	if err != nil || got != bin {
		t.Fatalf("FindRuralzd(--ruralzd %s) = %q, %v", bin, got, err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	bad := map[string]string{
		filepath.Join(dir, "missing"): "file does not exist",
		dir:                           "not a regular file",
	}
	if runtime.GOOS != "windows" {
		bad[plain] = "not executable"
	}
	for p, msg := range bad {
		_, err := FindRuralzd(ioWithPath(""), p, Options{})
		if command.Code(t.Context(), err) != command.ExitNoResult || !strings.Contains(err.Error(), "--ruralzd "+p+": ") || !strings.Contains(err.Error(), msg) {
			t.Errorf("FindRuralzd(--ruralzd %s) = %v, want exit 2 with %q", p, err, msg)
		}
	}
}

// TestFindRuralzdBesideRuralz: the ruralzd beside the running ruralz comes
// before PATH.
func TestFindRuralzdBesideRuralz(t *testing.T) {
	beside := t.TempDir()
	want := writeBinary(t, beside)
	onPath := t.TempDir()
	writeBinary(t, onPath)
	got, err := FindRuralzd(ioWithPath(onPath), "", Options{Executable: exeIn(beside)})
	if err != nil || got != want {
		t.Fatalf("FindRuralzd = %q, %v; want %q", got, err, want)
	}
}

// TestFindRuralzdBesideResolvesLinks: ruralz reached through a symbolic
// link finds the ruralzd beside its target.
func TestFindRuralzdBesideResolvesLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on Windows")
	}
	target := t.TempDir()
	want := writeBinary(t, target)
	if err := os.WriteFile(filepath.Join(target, "ruralz"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	links := t.TempDir()
	link := filepath.Join(links, "ruralz")
	if err := os.Symlink(filepath.Join(target, "ruralz"), link); err != nil {
		t.Fatal(err)
	}
	resolved, err := filepath.EvalSymlinks(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := FindRuralzd(ioWithPath(""), "", Options{Executable: func() (string, error) { return link, nil }})
	if err != nil || got != resolved {
		t.Fatalf("FindRuralzd = %q, %v; want %q", got, err, resolved)
	}
}

// TestFindRuralzdOnPath: without one beside ruralz, the first ruralzd on
// PATH, relative and empty entries skipped.
func TestFindRuralzdOnPath(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	want := writeBinary(t, first)
	writeBinary(t, second)
	path := strings.Join([]string{"", "relative", t.TempDir(), first, second}, string(os.PathListSeparator))
	got, err := FindRuralzd(ioWithPath(path), "", Options{Executable: exeIn(t.TempDir())})
	if err != nil || got != want {
		t.Fatalf("FindRuralzd = %q, %v; want %q", got, err, want)
	}
}

// TestFindRuralzdNone: no ruralzd in either place exits 2 naming both.
func TestFindRuralzdNone(t *testing.T) {
	beside := t.TempDir()
	_, err := FindRuralzd(ioWithPath(t.TempDir()), "", Options{Executable: exeIn(beside)})
	want := "no " + binaryName() + " beside ruralz in " + beside + " or on PATH; name it with --ruralzd PATH"
	if command.Code(t.Context(), err) != command.ExitNoResult || err.Error() != want {
		t.Fatalf("FindRuralzd = %v, want exit 2 with %q", err, want)
	}
	_, err = FindRuralzd(ioWithPath(""), "", Options{Executable: func() (string, error) { return "", errors.New("unknown") }})
	if err == nil || err.Error() != "no "+binaryName()+" beside ruralz or on PATH; name it with --ruralzd PATH" {
		t.Fatalf("FindRuralzd without an executable path = %v", err)
	}
}
