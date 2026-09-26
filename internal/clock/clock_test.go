// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clock

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Architecture section 0 convention 4: Real is the only time.Now caller;
// these tests pin its basic behavior so clocktest.Fake can mirror it.

func TestRealNowAndSince(t *testing.T) {
	c := Real()
	a := c.Now()
	b := c.Now()
	if b.Before(a) {
		t.Fatalf("Now went backwards: %v then %v", a, b)
	}
	if d := c.Since(a); d < 0 {
		t.Fatalf("Since = %v, want >= 0", d)
	}
	if a.Location() != time.Local {
		t.Fatalf("Now location = %v, want Local like time.Now", a.Location())
	}
}

func TestRealTimerFires(t *testing.T) {
	c := Real()
	tm := c.NewTimer(time.Millisecond)
	if tm.C() == nil {
		t.Fatal("NewTimer C() = nil")
	}
	select {
	case <-tm.C():
	case <-time.After(10 * time.Second):
		t.Fatal("timer did not fire")
	}
	if tm.Stop() {
		t.Error("Stop after fire reported active")
	}
	if tm.Reset(time.Hour) {
		t.Error("Reset of a fired timer reported active")
	}
	if !tm.Stop() {
		t.Error("Stop of an armed timer reported inactive")
	}
}

func TestRealZeroTimerFiresAtOnce(t *testing.T) {
	tm := Real().NewTimer(0)
	select {
	case <-tm.C():
	case <-time.After(10 * time.Second):
		t.Fatal("NewTimer(0) did not fire")
	}
}

func TestRealAfterFunc(t *testing.T) {
	c := Real()
	done := make(chan struct{})
	tm := c.AfterFunc(time.Millisecond, func() { close(done) })
	if tm.C() != nil {
		t.Error("AfterFunc timer C() is not nil")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("AfterFunc callback did not run")
	}

	stopped := c.AfterFunc(time.Hour, func() { t.Error("stopped AfterFunc ran") })
	if !stopped.Stop() {
		t.Error("Stop of a pending AfterFunc reported inactive")
	}
}

func TestRealSleep(t *testing.T) {
	c := Real()
	if err := c.Sleep(t.Context(), time.Millisecond); err != nil {
		t.Fatalf("Sleep = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.Sleep(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep on a canceled context = %v, want context.Canceled", err)
	}
}
