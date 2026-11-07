// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// waitDone waits for a blocked wait to return, under the bound.
func waitDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(waitBound):
		t.Fatal("the limiter never released the writer")
		return nil
	}
}

// mustWait fails the test when a wait the fake clock has armed already
// returned.
func mustWait(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("%s did not wait: it returned %v", what, err)
	default:
	}
}

// TestLimiterHoldsPeerRate: a full bucket passes one second of rate at
// once, and the next byte waits for the clock.
func TestLimiterHoldsPeerRate(t *testing.T) {
	fake := clocktest.New(testStart)
	l := newLimiter(fake, 1000)
	ctx := t.Context()
	if err := l.wait(ctx, 1000, false); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- l.wait(ctx, 500, false) }()
	armed(t, fake)
	mustWait(t, done, "a write past the rate")
	fake.Advance(500 * time.Millisecond)
	if err := waitDone(t, done); err != nil {
		t.Fatal(err)
	}
}

// TestContentYieldsToRaft: while a Raft write waits, a content write waits
// behind it even once tokens return.
func TestContentYieldsToRaft(t *testing.T) {
	fake := clocktest.New(testStart)
	l := newLimiter(fake, 1000)
	ctx := t.Context()
	if err := l.wait(ctx, 1000, false); err != nil {
		t.Fatal(err)
	}
	raftDone := make(chan error, 1)
	go func() { raftDone <- l.wait(ctx, 800, true) }()
	armed(t, fake)
	contentDone := make(chan error, 1)
	go func() { contentDone <- l.wait(ctx, 100, false) }()
	waitFor(t, "the content writer to queue behind Raft", func() bool {
		l.mu.Lock()
		defer l.mu.Unlock()
		return l.raftWaiting == 1
	})
	fake.Advance(800 * time.Millisecond)
	if err := waitDone(t, raftDone); err != nil {
		t.Fatal(err)
	}
	armed(t, fake)
	mustWait(t, contentDone, "the content write without tokens")
	fake.Advance(100 * time.Millisecond)
	if err := waitDone(t, contentDone); err != nil {
		t.Fatal(err)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.raftWaiting != 0 {
		t.Errorf("raftWaiting = %d after both writes", l.raftWaiting)
	}
}

// TestLimiterLargeWriteAndCancel: a write larger than the bucket waits
// for a full one and takes it into debt; a canceled wait returns.
func TestLimiterLargeWriteAndCancel(t *testing.T) {
	fake := clocktest.New(testStart)
	l := newLimiter(fake, 1000)
	if err := l.wait(t.Context(), 5000, true); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.wait(ctx, 1, false) }()
	armed(t, fake)
	cancel()
	if err := waitDone(t, done); !errors.Is(err, context.Canceled) {
		t.Errorf("wait = %v, want context.Canceled", err)
	}
	if err := l.wait(t.Context(), 0, false); err != nil {
		t.Errorf("a zero-byte wait = %v", err)
	}
}

// TestLimitersPerPeer keeps one bucket per peer.
func TestLimitersPerPeer(t *testing.T) {
	ls := newLimiters(clocktest.New(testStart), PeerRate)
	a, again, b := ls.of("a"), ls.of("a"), ls.of("b")
	if a != again || a == b {
		t.Error("limiters are not per peer")
	}
}

// armed waits until a wait has armed a fake timer.
func armed(t *testing.T, fake *clocktest.Fake) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	defer cancel()
	if err := fake.BlockUntil(ctx, 1); err != nil {
		t.Fatalf("no wait armed a timer: %v", err)
	}
}
