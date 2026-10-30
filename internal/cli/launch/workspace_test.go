// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// TestNewWorkspace checks the private directory: mode 0700 with symbolic
// links resolved, the data directory, the operator token file of mode 0600
// holding a token the Node's rule accepts, the secret root a sibling of
// both, and Remove deleting it all.
func TestNewWorkspace(t *testing.T) {
	ws, err := NewWorkspace("ruralz-test-", bytes.NewReader(bytes.Repeat([]byte{7}, tokenBytes)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ws.Remove() })
	if resolved, err := filepath.EvalSymlinks(ws.Root()); err != nil || resolved != ws.Root() {
		t.Errorf("root %s resolves to %s, %v", ws.Root(), resolved, err)
	}
	if !strings.HasPrefix(filepath.Base(ws.Root()), "ruralz-test-") {
		t.Errorf("root %s lacks the prefix", ws.Root())
	}
	fi, err := os.Stat(ws.Root())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0o700 {
		t.Errorf("root mode %v, want 0700", fi.Mode().Perm())
	}
	if fi, err := os.Stat(ws.DataDir()); err != nil || !fi.IsDir() {
		t.Errorf("data directory: %v", err)
	}
	tok, err := adminauth.ReadToken(ws.TokenFile())
	if err != nil {
		t.Fatalf("the token file fails the Node's rule: %v", err)
	}
	if len(tok) != 43 {
		t.Errorf("token of %d characters, want 43", len(tok))
	}
	if fi, err := os.Stat(ws.TokenFile()); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600) {
		t.Errorf("token file: %v, mode %v", err, fi.Mode())
	}
	for _, p := range []string{ws.DataDir(), ws.TokenFile()} {
		if rel, err := filepath.Rel(ws.SecretRoot(), p); err == nil && !strings.HasPrefix(rel, "..") {
			t.Errorf("the secret root %s contains %s", ws.SecretRoot(), p)
		}
	}
	if ws.Config() != filepath.Join(ws.Root(), ConfigFile) {
		t.Errorf("Config = %s", ws.Config())
	}
	if err := ws.Remove(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Root()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Remove left %s: %v", ws.Root(), err)
	}
}

// TestNewWorkspaceRandomFails: a failing random source exits 2 and leaves
// nothing behind.
func TestNewWorkspaceRandomFails(t *testing.T) {
	t.Setenv("TMPDIR", t.TempDir())
	before, _ := os.ReadDir(os.TempDir())
	_, err := NewWorkspace("ruralz-test-", io.LimitReader(bytes.NewReader(nil), 0))
	if command.Code(nil, err) != command.ExitNoResult || !strings.Contains(err.Error(), "draw the operator token") {
		t.Fatalf("NewWorkspace = %v", err)
	}
	if runtime.GOOS == "windows" {
		return // TMPDIR does not move the Windows temporary directory.
	}
	after, _ := os.ReadDir(os.TempDir())
	if len(after) != len(before) {
		t.Errorf("a failed NewWorkspace left %d entries in the temporary directory", len(after)-len(before))
	}
}

// TestWriteAtomic: a replaced file holds the new bytes with mode 0600 and
// no temporary file stays.
func TestWriteAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	for _, s := range []string{"one", "two"} {
		if err := writeAtomic(p, []byte(s)); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p) //nolint:gosec // G304: a file in the test's temporary directory.
		if err != nil || string(got) != s {
			t.Fatalf("read %q, %v; want %q", got, err, s)
		}
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, want 0600", fi.Mode().Perm())
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("entries %v, want the file alone", entries)
	}
}
