// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"path/filepath"
	"testing"

	hraft "github.com/hashicorp/raft"
)

// entries returns command entries first to last.
func entries(first, last uint64) []*hraft.Log {
	var out []*hraft.Log
	for i := first; i <= last; i++ {
		out = append(out, &hraft.Log{Index: i, Term: 1, Type: hraft.LogCommand, Data: []byte("x")})
	}
	return out
}

// committedOf reads the persisted commit index.
func committedOf(t *testing.T, l *commitLog) uint64 {
	t.Helper()
	idx, err := l.committed()
	if err != nil {
		t.Fatal(err)
	}
	return idx
}

// TestCommitIndexPersistedWithLogWrites: the applied index is persisted
// by the next log write, survives a reopen and never reads above the last
// entry.
func TestCommitIndexPersistedWithLogWrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft.db")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l.noteApplied(3)
	if got := committedOf(t, l); got != 0 {
		t.Errorf("committed = %d before any write, want 0", got)
	}
	if err := l.StoreLogs(entries(1, 2)); err != nil {
		t.Fatal(err)
	}
	if got := committedOf(t, l); got != 2 {
		t.Errorf("committed = %d, want 2: never above the last entry", got)
	}
	if err := l.StoreLog(entries(3, 3)[0]); err != nil {
		t.Fatal(err)
	}
	l.noteApplied(1)
	if err := l.StoreLogs(entries(4, 4)); err != nil {
		t.Fatal(err)
	}
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	l, err = openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if got := committedOf(t, l); got != 3 {
		t.Errorf("committed after reopen = %d, want 3 (a lower applied index never lowers it)", got)
	}
}

// TestCommitIndexNotPersistedWhenWritesFail: a failing log write persists
// nothing.
func TestCommitIndexNotPersistedWhenWritesFail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "raft.db")
	l, err := openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l.noteApplied(5)
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	if err := l.StoreLogs(entries(1, 5)); err == nil {
		t.Fatal("StoreLogs on a closed store succeeded")
	}
	l, err = openLog(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if got := committedOf(t, l); got != 0 {
		t.Errorf("committed = %d after a failed write, want 0", got)
	}
}

// TestCommitIndexPersistedAtShutdown: a clean shutdown records the applied
// index, which a follower advanced by heartbeats without a log write; it
// never lowers the persisted index.
func TestCommitIndexPersistedAtShutdown(t *testing.T) {
	l, err := openLog(filepath.Join(t.TempDir(), "raft.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	if err := l.StoreLogs(entries(1, 5)); err != nil {
		t.Fatal(err)
	}
	if err := l.persistApplied(5); err != nil {
		t.Fatal(err)
	}
	if err := l.persistApplied(2); err != nil {
		t.Fatal(err)
	}
	if got := committedOf(t, l); got != 5 {
		t.Errorf("committed = %d, want the applied index 5", got)
	}
}
