// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// gatedClock is a fake clock whose timers made after Gate wait for the
// gate before their function runs: a test orders a slot release before a
// waiter's deadline function.
type gatedClock struct {
	*clocktest.Fake
	mu   sync.Mutex
	gate chan struct{}
}

// Gate makes later timers wait for the returned channel's close.
func (c *gatedClock) Gate() chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gate = make(chan struct{})
	return c.gate
}

func (c *gatedClock) AfterFunc(d time.Duration, fn func()) clock.Timer {
	c.mu.Lock()
	g := c.gate
	c.mu.Unlock()
	if g == nil {
		return c.Fake.AfterFunc(d, fn)
	}
	return c.Fake.AfterFunc(d, func() { <-g; fn() })
}

// TestLegWaiterSlotAtDeadline checks that a waiter granted its bulkhead
// slot when its attempt deadline has passed, before the deadline's timer
// function ran, still ends as a timeout without reaching an Endpoint, 504
// RZ-UP-003, and hands the slot on.
func TestLegWaiterSlotAtDeadline(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv := newServer(t, blocking(release))
	f := newFixture(t)
	gc := &gatedClock{Fake: f.clk}
	f.m = NewManager(Options{Clock: gc, Rand: lowSource{}, Dial: f.dial.dial, Status: f.status, Tracer: f.tracer})
	u := newUpstream("orders", srv.addr)
	u.Spec.Retries = &v1alpha1.Retries{Attempts: ptr[int32](0), PerTryTimeout: dur(time.Second)}
	u.Spec.CircuitBreaker = &v1alpha1.CircuitBreaker{MaxConnections: ptr[int32](1), MaxPendingRequests: ptr[int32](1)}
	s := f.compile(nil, Config{Upstream: u})
	up := f.upstream(s, "orders")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	first := goLeg(ctx, up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "the first attempt", func() bool { return srv.hits.Load() == 1 })
	gate := gc.Gate()
	second := goLeg(t.Context(), up, outbound(exchange(http.MethodGet, "/"), nil))
	waitFor(t, "a waiter", func() bool { return up.rt.res.Bulkhead().Pending() == 1 })
	advanced := make(chan struct{})
	go func() {
		// The holder's deadline frees the slot; the waiter's deadline
		// function waits for the gate, so the slot reaches the waiter at
		// its deadline first.
		gc.Advance(time.Second)
		close(advanced)
	}()
	waitFor(t, "the slot granted to the waiter", func() bool { return up.rt.res.Bulkhead().Pending() == 0 })
	res := await(t, second)
	close(gate)
	<-advanced
	var le *LegError
	if !errors.As(res.err, &le) || codeOf(res.err) != "RZ-UP-003" || le.Kind != resilience.KindTimeout || le.Endpoint != "" {
		t.Errorf("waiter granted at its deadline = %v, want RZ-UP-003 of kind timeout without an Endpoint", res.err)
	}
	if n := srv.hits.Load(); n != 1 {
		t.Errorf("the server saw %d requests, want 1: the waiter reached an Endpoint past its deadline", n)
	}
	cancel()
	await(t, first)
	if n := up.rt.res.Bulkhead().InFlight(); n != 0 {
		t.Errorf("slots held after both legs = %d, want 0", n)
	}
}
