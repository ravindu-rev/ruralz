// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix && !solaris && !aix

package signing

import (
	"errors"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestCredentialFileFIFO: a FIFO named as the credential file is refused
// at once, never waited on.
func TestCredentialFileFIFO(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.json")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadCredentialFile(p)
	if !errors.Is(err, ErrCredentialFile) || !strings.Contains(err.Error(), "not a regular file") {
		t.Fatalf("ReadCredentialFile(FIFO) = %v", err)
	}
}
