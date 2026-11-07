// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"errors"
	"fmt"
	"sync"
	"time"

	hraft "github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
	"go.etcd.io/bbolt"
)

// appliedIndexKey returns the stable-store key of the persisted applied
// index.
func appliedIndexKey() []byte { return []byte("ruralz.appliedIndex") }

// boltLockWait bounds bbolt's wait for its file lock: without one it
// waits forever while another process holds the Control Store, so a
// second process fails instead of hanging.
const boltLockWait = time.Second

// commitLog is the raft-boltdb/v2 log and stable store, which also keeps
// the persisted commit index: the highest index the state machine
// applied, recorded after every log write and at a clean shutdown. Every
// applied entry is committed and stored here first, so the index never
// names an uncommitted or missing entry, and losing a write in a crash
// only leaves an older committed index. At start the store replays the
// committed entries up to it itself, before raft starts
// (docs/architecture/04-control-plane-and-gitops.md "Readiness on port
// 9902"): raft-boltdb/v2 v2.4.2 implements no CommitTrackingLogStore,
// and raft's own RestoreCommittedLogs replay skips the configuration
// entries it replays, losing the membership they hold.
type commitLog struct {
	*raftboltdb.BoltStore

	mu        sync.Mutex
	applied   uint64
	persisted uint64
}

// openLog opens the bbolt file at path.
func openLog(path string) (*commitLog, error) {
	bs, err := raftboltdb.New(raftboltdb.Options{Path: path, BoltOptions: &bbolt.Options{Timeout: boltLockWait}})
	if err != nil {
		return nil, fmt.Errorf("controlstore: open %s: %w", path, err)
	}
	l := &commitLog{BoltStore: bs}
	idx, err := bs.GetUint64(appliedIndexKey())
	switch {
	case err == nil:
		l.persisted = idx
	case errors.Is(err, raftboltdb.ErrKeyNotFound):
	default:
		_ = bs.Close()
		return nil, fmt.Errorf("controlstore: read the applied index: %w", err)
	}
	return l, nil
}

// noteApplied records the state machine's applied index in memory.
func (l *commitLog) noteApplied(idx uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if idx > l.applied {
		l.applied = idx
	}
}

// StoreLogs stores the entries, then persists the applied index.
func (l *commitLog) StoreLogs(logs []*hraft.Log) error {
	if err := l.BoltStore.StoreLogs(logs); err != nil {
		return err
	}
	l.mu.Lock()
	idx := l.applied
	l.mu.Unlock()
	return l.persist(idx)
}

// StoreLog stores one entry, then persists the applied index.
func (l *commitLog) StoreLog(log *hraft.Log) error { return l.StoreLogs([]*hraft.Log{log}) }

// persistApplied records the applied index at a clean shutdown, after
// the last log write.
func (l *commitLog) persistApplied(idx uint64) error {
	l.noteApplied(idx)
	return l.persist(idx)
}

// persist writes idx when it is above the persisted index.
func (l *commitLog) persist(idx uint64) error {
	l.mu.Lock()
	if idx <= l.persisted {
		l.mu.Unlock()
		return nil
	}
	l.mu.Unlock()
	if err := l.SetUint64(appliedIndexKey(), idx); err != nil {
		return fmt.Errorf("controlstore: persist the applied index: %w", err)
	}
	l.mu.Lock()
	if idx > l.persisted {
		l.persisted = idx
	}
	l.mu.Unlock()
	return nil
}

// committed returns the persisted commit index, never above the last
// entry.
func (l *commitLog) committed() (uint64, error) {
	last, err := l.LastIndex()
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return min(l.persisted, last), nil
}
