// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package lkg

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// TestReadsRefuseLinksAndFIFOs: a content file or pointer replaced by a
// symbolic link is never followed, and a FIFO is refused without blocking
// the boot.
func TestReadsRefuseLinksAndFIFOs(t *testing.T) {
	r := newRig(t)
	a := rev("A")
	outside := t.TempDir() + "/elsewhere"
	if err := os.WriteFile(outside, []byte("A"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, r.s.Path(ContentName(a.Digest))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.Revision(a.Digest); err == nil {
		t.Error("Revision followed a symbolic link")
	}

	b := rev("B")
	if err := syscall.Mkfifo(r.s.Path(ContentName(b.Digest)), 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := r.s.Revision(b.Digest)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Revision read a FIFO")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Revision blocked on a FIFO")
	}

	if err := os.Symlink(outside, r.s.Path(LKGFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.s.LastKnownGood(); err == nil {
		t.Error("LastKnownGood followed a symbolic-link pointer")
	}
}
