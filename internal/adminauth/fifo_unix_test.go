// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package adminauth

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Tests that a FIFO at a token or ca.crt path is refused as ErrNotRegular
// without blocking (opening a FIFO for reading waits for a writer), at
// start (spec 06 requirement 93) and on a request-path refresh.

// mkfifo creates a FIFO at dir/name and returns its path.
func mkfifo(t *testing.T, dir, name string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	return p
}

// within fails t when f does not return within 10 seconds.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s blocked on a FIFO", what)
	}
}

func TestFIFOTokenFileRefusedReq93(t *testing.T) {
	dir := t.TempDir()
	fifo := mkfifo(t, dir, "fifo")
	within(t, "New", func() {
		for _, s := range []Settings{{TokenFile: fifo}, {MetricsTokenFile: fifo}} {
			if _, err := New(s, Options{}); !errors.Is(err, ErrNotRegular) {
				t.Errorf("New(%+v) = %v, want ErrNotRegular", s, err)
			}
		}
	})
	within(t, "readRegular", func() {
		if _, _, err := readRegular(fifo, 1); !errors.Is(err, ErrNotRegular) {
			t.Errorf("readRegular = %v", err)
		}
	})
	// The non-blocking open itself reads a regular file normally.
	p := writeFile(t, dir, "regular", operatorToken)
	if b, fi, err := readRegular(p, 1024); err != nil || string(b) != operatorToken || !fi.Mode().IsRegular() {
		t.Fatalf("readRegular = %q, %v", b, err)
	}
}

func TestFIFORotationKeepsLastToken(t *testing.T) {
	dir := t.TempDir()
	logs := &logBuffer{}
	p := writeFile(t, dir, "operator", operatorToken)
	a, err := New(Settings{TokenFile: p}, Options{Logger: newLogger(logs)})
	if err != nil {
		t.Fatal(err)
	}
	fifo := mkfifo(t, dir, "fifo")
	if err := os.Rename(fifo, p); err != nil {
		t.Fatal(err)
	}
	within(t, "Authenticate", func() {
		for range 3 {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/tap", nil)
			r.RemoteAddr = "127.0.0.1:9"
			r.Header.Set("Authorization", "Bearer "+operatorToken)
			if pr, err := a.Authenticate(r); err != nil || pr.Kind != KindOperator {
				t.Errorf("Authenticate = %+v, %v", pr, err)
			}
		}
	})
	failures := logs.lines("admin token rotation failed, keeping the last valid token")
	if len(failures) != 1 || !strings.Contains(failures[0], ErrNotRegular.Error()) {
		t.Fatalf("failure records:\n%s", logs.String())
	}
	within(t, "Reload", func() {
		if err := a.Reload(); !errors.Is(err, ErrNotRegular) {
			t.Errorf("Reload = %v", err)
		}
	})
}

func TestFIFOClientCARefused(t *testing.T) {
	d := tlsDir(t, newCA(t, "operators"), false)
	mkfifo(t, d, ClientCAFile)
	within(t, "New", func() {
		if _, err := New(Settings{TLSDir: d}, Options{}); !errors.Is(err, ErrNotRegular) {
			t.Errorf("New = %v", err)
		}
	})
}
