// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package handover

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/ulid"
)

// testRecord returns a holder.json record of process pid.
func testRecord(t *testing.T, pid int) nodedir.Holder {
	t.Helper()
	id, err := ulid.Parse("01JBKQ6Z5Q8V8F0Y1N2M3P4R5S")
	if err != nil {
		t.Fatal(err)
	}
	return nodedir.Holder{Format: nodedir.HolderFormat, PID: pid, NodeID: id, Version: "0.1.0-test"}
}

// TestLockTakeAndRelease checks the Drain's lock: Take records the lock
// the process took, Held returns it, and Release drops it once, so
// another process can take it, removing holder.json.
func TestLockTakeAndRelease(t *testing.T) {
	d := dataDir(t)
	nl, err := d.TryLock()
	if err != nil {
		t.Fatal(err)
	}
	l := NewLock()
	if l.Held() != nil {
		t.Fatal("a new Lock holds a lock")
	}
	if !l.Take(nl) {
		t.Fatal("Take refused the first lock")
	}
	if l.Held() != nl {
		t.Fatal("Held does not return the lock taken")
	}
	if err := nl.WriteHolder(testRecord(t, 4242)); err != nil {
		t.Fatal(err)
	}
	if _, err := d.TryLock(); !errors.Is(err, nodedir.ErrLocked) {
		t.Fatalf("TryLock while held = %v, want ErrLocked", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release: %v", err)
	}
	if l.Held() != nil {
		t.Error("Held after Release returns a lock")
	}
	if _, err := d.ReadHolder(); !errors.Is(err, nodedir.ErrNoHolder) {
		t.Errorf("holder.json after Release: %v, want it removed", err)
	}
	other := takeLock(t, d)
	if l.Take(other) {
		t.Fatal("Take after Release recorded a lock")
	}
	if other.Held() {
		t.Error("Take after Release kept the lock instead of releasing it")
	}
}

// TestLockReleaseBeforeTake checks a new process whose Drain started
// before it took the lock: Release marks the Lock released, and the lock
// taken afterwards is released at once, so a draining process never holds
// it.
func TestLockReleaseBeforeTake(t *testing.T) {
	d := dataDir(t)
	l := NewLock()
	if err := l.Release(); err != nil {
		t.Fatalf("Release holding nothing: %v", err)
	}
	nl := takeLock(t, d)
	if l.Take(nl) {
		t.Fatal("Take after Release recorded the lock")
	}
	if _, err := d.TryLock(); err != nil {
		t.Errorf("TryLock after the refused Take = %v, want the lock free", err)
	}
	if l.Take(nil) {
		t.Error("Take(nil) succeeded")
	}
}
