// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package node

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// TestHostWithoutProcfs: off Linux every call of the default host fails
// with errors.ErrUnsupported, so a drain that passed the platform gate, as
// with an injected GOOS, still signals nothing.
func TestHostWithoutProcfs(t *testing.T) {
	hs := Options{}.machine()
	if _, err := hs.pidNamespace(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("pidNamespace() = %v", err)
	}
	if _, err := hs.procStat(1); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("procStat() = %v", err)
	}
	if _, err := hs.locks(); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("locks() = %v", err)
	}
	if _, err := hs.inode("lock"); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("inode() = %v", err)
	}
	if _, err := hs.open(1); !errors.Is(err, errors.ErrUnsupported) {
		t.Errorf("open() = %v", err)
	}
	root := t.TempDir()
	writeHolder(t, root, testHolder(t))
	r := execute(t.Context(), Options{}, "linux", nil, "node", "drain", "--data-dir", root)
	want := "ruralz node drain: no Linux /proc on this platform: unsupported operation\n"
	if r.code != command.ExitNoResult || r.stdout != "" || r.stderr != want {
		t.Errorf("drain = exit %d, stdout %q, stderr %q; want 2, %q", r.code, r.stdout, r.stderr, want)
	}
}
