// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package geoip

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/filter/authz/geoip/geoiptest"
)

// TestFIFOIsRefused: a FIFO at the path is refused as not a regular file
// without blocking the read, which a plain open would until a writer
// appears.
func TestFIFOIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "geoip.mmdb")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	st, l := &status{}, &logs{}
	db := NewDatabase(DatabaseOptions{Path: path, Clock: clocktest.New(t0), Status: st, Logger: slog.New(l)})
	if db.current() != nil || !st.raised() {
		t.Fatalf("a FIFO: database %v, raised %v; want none and raised", db.current(), st.raised())
	}
	var errText string
	l.recs[0].Attrs(func(a slog.Attr) bool {
		if a.Key == "error" {
			errText = a.Value.String()
		}
		return true
	})
	if !strings.Contains(errText, "not a regular file") {
		t.Errorf("logged error %q, want not a regular file", errText)
	}
}

// TestSymlinkTargetSwap: a new symbolic-link target, as a Kubernetes
// volume update swaps it, is a replaced file, read by the next poll at
// once.
func TestSymlinkTargetSwap(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.mmdb"), filepath.Join(dir, "b.mmdb")
	rewrite(t, a, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("GB"))), t0.Add(-time.Hour))
	rewrite(t, b, dbBytes(t, geoiptest.New(geoiptest.Options{}).Insert("81.2.69.0/24", geoiptest.Country("CN"))), t0.Add(-time.Hour))
	path := filepath.Join(dir, "GeoLite2-Country.mmdb")
	if err := os.Symlink("a.mmdb", path); err != nil {
		t.Fatal(err)
	}
	fx := &fixture{path: path, clk: clocktest.New(t0), status: &status{}, logs: &logs{}}
	fx.db = NewDatabase(DatabaseOptions{Path: path, Clock: fx.clk, Logger: slog.New(fx.logs), Status: fx.status})
	if got := fx.placed(t, addrGB); got != "GB" {
		t.Fatalf("place = %q through the first target, want GB", got)
	}
	tmp := filepath.Join(dir, ".link")
	if err := os.Symlink("b.mmdb", tmp); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	fx.poll(t)
	if got := fx.placed(t, addrGB); got != "CN" {
		t.Errorf("place = %q after one poll, want CN through the new target", got)
	}
}
