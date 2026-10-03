// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package nodedir

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// openLocked opens a fresh data directory and takes its lock, skipping
// where flock is unsupported (R-60).
func openLocked(t *testing.T) (*Dir, *Lock) {
	t.Helper()
	d, err := Open(filepath.Join(t.TempDir(), "data"))
	if err != nil {
		t.Fatal(err)
	}
	lk, err := d.TryLock()
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skipf("flock unsupported: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lk.Release() })
	return d, lk
}

// TestTryLockSameProcess covers spec 04 requirement 7 and test plan item 3:
// a second TryLock in the same process (a separate open file description)
// fails with ErrLocked; Release lets it succeed; the lock file is 0600.
func TestTryLockSameProcess(t *testing.T) {
	d, lk := openLocked(t)
	assertPerm(t, d.LockPath(), 0o600)
	if lk.Dir() != d || !lk.Held() {
		t.Fatal("Lock does not report its directory or held state")
	}
	d2, err := Open(d.Root())
	if err != nil {
		t.Fatal(err)
	}
	if l2, err := d2.TryLock(); !errors.Is(err, ErrLocked) {
		t.Fatalf("second TryLock = %v, %v; want ErrLocked", l2, err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	l3, err := d2.TryLock()
	if err != nil {
		t.Fatalf("TryLock after Release: %v", err)
	}
	if err := l3.Release(); err != nil {
		t.Fatal(err)
	}
}

// TestNoWriteAfterRelease covers spec 04 requirement 7: the holder never
// writes holder.json or under lkg/ after releasing the lock at Drain
// start, and Release is idempotent.
func TestNoWriteAfterRelease(t *testing.T) {
	d, lk := openLocked(t)
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if lk.Held() {
		t.Fatal("Held after Release")
	}
	if err := lk.WriteHolder(goldenHolder(t)); !errors.Is(err, ErrReleased) {
		t.Errorf("WriteHolder err = %v, want ErrReleased", err)
	}
	if err := lk.WriteFile("lkg/lkg.json", []byte("{}")); !errors.Is(err, ErrReleased) {
		t.Errorf("WriteFile err = %v, want ErrReleased", err)
	}
	if err := lk.Remove("lkg/lkg.json"); !errors.Is(err, ErrReleased) {
		t.Errorf("Remove err = %v, want ErrReleased", err)
	}
	for _, p := range []string{d.HolderPath(), d.Path("lkg/lkg.json")} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("%s written after Release", p)
		}
	}
}

// TestReleaseKeepsForeignHolder: Release removes holder.json only when
// this Lock wrote it.
func TestReleaseKeepsForeignHolder(t *testing.T) {
	d, lk := openLocked(t)
	data, err := goldenHolder(t).Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.HolderPath(), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	if _, err := d.ReadHolder(); err != nil {
		t.Fatalf("holder.json removed by a Lock that did not write it: %v", err)
	}
}

// TestLockWriteFile covers holder-only writes under the data directory
// (Last-Known-Good, spec 04 requirement 7).
func TestLockWriteFile(t *testing.T) {
	d, lk := openLocked(t)
	if err := lk.WriteFile("lkg/lkg.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(d.Path("lkg/lkg.json"))
	if err != nil || string(got) != `{"a":1}` {
		t.Fatalf("content = %q, %v", got, err)
	}
	assertPerm(t, d.Path("lkg/lkg.json"), 0o600)
	if err := lk.Remove("lkg/lkg.json"); err != nil {
		t.Fatal(err)
	}
	if err := lk.Remove("lkg/lkg.json"); err != nil {
		t.Fatalf("Remove of a missing file: %v", err)
	}
	for _, rel := range []string{"../escape", "/abs", "", "lock", "holder.json", "identity/node-id", "lkg/../lock"} {
		if err := lk.WriteFile(rel, nil); !errors.Is(err, ErrBadName) {
			t.Errorf("WriteFile(%q) err = %v, want ErrBadName", rel, err)
		}
		if err := lk.Remove(rel); !errors.Is(err, ErrBadName) {
			t.Errorf("Remove(%q) err = %v, want ErrBadName", rel, err)
		}
	}
	if err := lk.WriteFile("missing/x", nil); err == nil {
		t.Error("WriteFile into a missing directory succeeded")
	}
	if err := os.MkdirAll(d.Path("lkg/sub/x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := lk.Remove("lkg/sub"); err == nil {
		t.Error("Remove of a non-empty directory succeeded")
	}
}

// TestReleaseWaitsForWrites: writes racing a Release either complete
// before it or fail with ErrReleased; none lands afterwards.
func TestReleaseWaitsForWrites(t *testing.T) {
	d, lk := openLocked(t)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 20 {
				err := lk.WriteFile(LKGDir+"/f", []byte("x"))
				if err != nil && !errors.Is(err, ErrReleased) {
					t.Errorf("WriteFile: %v", err)
				}
			}
		})
	}
	if err := lk.Release(); err != nil {
		t.Fatal(err)
	}
	fi, statErr := os.Stat(d.Path("lkg/f"))
	wg.Wait()
	fi2, statErr2 := os.Stat(d.Path("lkg/f"))
	if (statErr == nil) != (statErr2 == nil) || (statErr == nil && !os.SameFile(fi, fi2)) {
		t.Error("a write landed after Release returned")
	}
}
