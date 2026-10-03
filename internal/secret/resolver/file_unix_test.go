// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package resolver

import (
	"io/fs"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/secret"
)

func TestFileFIFONeverOpened(t *testing.T) {
	// A FIFO under the root is refused without opening it (opening a FIFO
	// with no writer would block the resolver).
	h := newHarness(t, nil)
	if err := syscall.Mkfifo(filepath.Join(h.root, "fifo"), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	diags := h.resolveDiags(use(fileRef(h.path("fifo"), ""), secret.KindOpaque))
	if !strings.HasSuffix(diags[0].Message, errNotRegular.Error()) {
		t.Errorf("message = %q", diags[0].Message)
	}
}

func TestFileIdentity(t *testing.T) {
	h := newHarness(t, nil)
	h.write("f", "v")
	root, err := h.r.openRoot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	fi, err := root.Stat("f")
	if err != nil {
		t.Fatal(err)
	}
	if _, ino, _, ok := fileIdentity(fi); !ok || ino == 0 {
		t.Errorf("fileIdentity = ino %d ok %v", ino, ok)
	}
	if _, _, _, ok := fileIdentity(fakeInfo{fi}); ok {
		t.Error("fileIdentity of a FileInfo without Stat_t")
	}
	// Without an identity the owner check cannot run and passes.
	if err := h.r.checkOwnerMode(fakeInfo{fi}); err != nil {
		t.Errorf("checkOwnerMode = %v", err)
	}
}

// fakeInfo is a FileInfo without system data.
type fakeInfo struct{ fs.FileInfo }

func (fakeInfo) Sys() any { return nil }
