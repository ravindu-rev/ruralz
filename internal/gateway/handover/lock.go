// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"fmt"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// Lock is the data-directory lock as the process's Drain releases it, the
// Drainer's Lock (drain.Config.Lock): the wiring creates it before the
// Node boots, when the process may not hold the lock yet, and Take records
// the lock once the process takes it, at start or as a handover's new
// process. Release, at the Drain's start, first stops the holder's socket
// and removes handover.sock while the lock is still held, so it never
// removes a successor's socket, then releases the lock (holder.json
// removed, the flock dropped), then has the handover in progress send
// released. Its methods are safe for concurrent use.
type Lock struct {
	mu       sync.Mutex
	held     *nodedir.Lock
	holder   *Holder
	released bool
}

// NewLock returns a Lock holding nothing yet.
func NewLock() *Lock { return &Lock{} }

// Take records nl, the data-directory lock the process just took. It
// reports false, releasing nl at once, when the Drain already released
// the Lock or another lock was recorded: a draining process never holds
// the lock.
func (l *Lock) Take(nl *nodedir.Lock) bool {
	if nl == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released || l.held != nil {
		_ = nl.Release()
		return false
	}
	l.held = nl
	return true
}

// Held returns the data-directory lock the process holds, for the writes
// only the lock holder makes (holder.json, Last-Known-Good); nil before
// Take and after Release.
func (l *Lock) Held() *nodedir.Lock {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	return l.held
}

// Release stops the holder's handover socket, releases the
// data-directory lock and lets the handover in progress send released,
// in that order. It is idempotent, and does nothing but mark the Lock
// released when the process holds no lock.
func (l *Lock) Release() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.released {
		return nil
	}
	l.released = true
	h := l.holder
	if h != nil {
		h.stopListening(l.held != nil)
	}
	var err error
	if l.held != nil {
		err = l.held.Release()
	}
	if h != nil {
		h.lockReleased()
	}
	return err
}

// attach runs start, which creates the holder's socket, while the process
// holds the lock and Release cannot run, and records h for Release.
func (l *Lock) attach(h *Holder, start func() error) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.released:
		return ErrReleased
	case l.held == nil:
		return ErrNotHeld
	case l.holder != nil:
		return fmt.Errorf("%w: another holder serves the handover socket", ErrConfig)
	}
	if err := start(); err != nil {
		return err
	}
	l.holder = h
	return nil
}

// detach forgets h, closing its socket and removing handover.sock while
// the lock is still held.
func (l *Lock) detach(h *Holder) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != h {
		return
	}
	l.holder = nil
	h.stopListening(l.held != nil && !l.released)
}
