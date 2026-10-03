// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestRouteDeadline_05Req4 applies the 15 s default Route timeout.
func TestRouteDeadline_05Req4(t *testing.T) {
	if got := RouteDeadline(epoch, 0); !got.Equal(epoch.Add(15 * time.Second)) {
		t.Errorf("default: %v", got)
	}
	if got := RouteDeadline(epoch, -time.Second); !got.Equal(epoch.Add(15 * time.Second)) {
		t.Errorf("negative: %v", got)
	}
	if got := RouteDeadline(epoch, 3*time.Second); !got.Equal(epoch.Add(3 * time.Second)) {
		t.Errorf("3s: %v", got)
	}
}

// TestLegDeadline_05Req24 nests the leg deadline in the Route deadline.
func TestLegDeadline_05Req24(t *testing.T) {
	route := epoch.Add(10 * time.Second)
	tests := []struct {
		name    string
		route   time.Time
		start   time.Time
		timeout time.Duration
		want    time.Time
	}{
		{"Upstream timeout absent: the Route's", route, epoch, 0, route},
		{"shorter Upstream timeout", route, epoch, 3 * time.Second, epoch.Add(3 * time.Second)},
		{"longer Upstream timeout clamped by the Route", route, epoch, 30 * time.Second, route},
		{"late leg start clamped", route, epoch.Add(8 * time.Second), 3 * time.Second, route},
		{"no Route bound", time.Time{}, epoch, time.Second, epoch.Add(time.Second)},
		{"no bound at all", time.Time{}, epoch, 0, time.Time{}},
	}
	for _, tt := range tests {
		if got := LegDeadline(tt.route, tt.start, tt.timeout); !got.Equal(tt.want) {
			t.Errorf("%s: LegDeadline = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestPerTryTimeout_05Req4 derives the default perTryTimeout as the leg
// time left divided by the retries left plus one, at each attempt start.
func TestPerTryTimeout_05Req4(t *testing.T) {
	leg := epoch.Add(9 * time.Second)
	tests := []struct {
		name        string
		configured  time.Duration
		deadline    time.Time
		now         time.Time
		retriesLeft int
		want        time.Duration
	}{
		{"configured wins", 2 * time.Second, leg, epoch, 2, 2 * time.Second},
		{"first of three attempts", 0, leg, epoch, 2, 3 * time.Second},
		{"second of three attempts", 0, leg, epoch.Add(3 * time.Second), 1, 3 * time.Second},
		{"last attempt takes the rest", 0, leg, epoch.Add(6 * time.Second), 0, 3 * time.Second},
		{"negative retries left count as none", 0, leg, epoch, -1, 9 * time.Second},
		{"no leg deadline", 0, time.Time{}, epoch, 1, 0},
		{"no time left", 0, leg, leg.Add(time.Second), 1, 0},
		{"one nanosecond left", 0, leg, leg.Add(-time.Nanosecond), 5, time.Nanosecond},
	}
	for _, tt := range tests {
		if got := PerTryTimeout(tt.configured, tt.deadline, tt.now, tt.retriesLeft); got != tt.want {
			t.Errorf("%s: PerTryTimeout = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestAttemptDeadline_05Req24 nests the attempt deadline in the leg's.
func TestAttemptDeadline_05Req24(t *testing.T) {
	leg := epoch.Add(5 * time.Second)
	if got := AttemptDeadline(leg, epoch, 2*time.Second); !got.Equal(epoch.Add(2 * time.Second)) {
		t.Errorf("per-try bound: %v", got)
	}
	if got := AttemptDeadline(leg, epoch.Add(4*time.Second), 2*time.Second); !got.Equal(leg) {
		t.Errorf("leg clamp: %v", got)
	}
	if got := AttemptDeadline(leg, epoch, 0); !got.Equal(leg) {
		t.Errorf("no per-try: %v", got)
	}
	if got := AttemptDeadline(time.Time{}, epoch, time.Second); !got.Equal(epoch.Add(time.Second)) {
		t.Errorf("no leg bound: %v", got)
	}
	if got := Earliest(time.Time{}, time.Time{}); !got.IsZero() {
		t.Errorf("Earliest of none: %v", got)
	}
	if got := Earliest(leg, epoch); !got.Equal(epoch) {
		t.Errorf("Earliest: %v", got)
	}
}

// TestWithDeadline_05Req25 drives the attempt context with a fake clock.
func TestWithDeadline_05Req25(t *testing.T) {
	t.Run("fires at the deadline with its cause", func(t *testing.T) {
		clk := newClock()
		ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Second), ErrAttemptTimeout)
		defer d.Cancel()
		clk.Advance(999 * time.Millisecond)
		if ctx.Err() != nil || d.Expired() {
			t.Fatal("canceled before the deadline")
		}
		clk.Advance(time.Millisecond)
		<-ctx.Done()
		if !errors.Is(context.Cause(ctx), ErrAttemptTimeout) || !d.Expired() {
			t.Fatalf("cause %v, expired %v", context.Cause(ctx), d.Expired())
		}
		if d.Stop() {
			t.Fatal("Stop after the deadline reported not fired")
		}
		if _, ok := ctx.Deadline(); ok {
			t.Fatal("the context reports a wall-clock deadline")
		}
	})
	t.Run("Stop at headers keeps the context alive", func(t *testing.T) {
		clk := newClock()
		ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Second), ErrLegTimeout)
		if !d.Stop() {
			t.Fatal("Stop before the deadline reported fired")
		}
		if !d.Stop() {
			t.Fatal("a second Stop reported fired")
		}
		clk.Advance(time.Hour)
		if ctx.Err() != nil || d.Expired() {
			t.Fatal("a stopped deadline canceled its context")
		}
		d.Cancel()
		d.Cancel()
		if !errors.Is(context.Cause(ctx), context.Canceled) || d.Expired() {
			t.Fatalf("after Cancel: cause %v", context.Cause(ctx))
		}
	})
	t.Run("a late timer never cancels a stopped deadline", func(t *testing.T) {
		clk := newClock()
		ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Second), ErrAttemptTimeout)
		defer d.Cancel()
		d.Stop()
		d.fire() // a timer callback already running when Stop won
		if ctx.Err() != nil || d.Expired() {
			t.Fatal("fire after Stop canceled the context")
		}
	})
	t.Run("a passed deadline fires at once", func(t *testing.T) {
		clk := newClock()
		ctx, d := WithDeadline(context.Background(), clk, clk.Now(), ErrLegTimeout)
		defer d.Cancel()
		if ctx.Err() == nil || !d.Expired() || clk.Pending() != 0 {
			t.Fatal("a passed deadline did not fire synchronously")
		}
	})
	t.Run("zero deadline sets no timer", func(t *testing.T) {
		clk := newClock()
		ctx, d := WithDeadline(context.Background(), clk, time.Time{}, ErrLegTimeout)
		if clk.Pending() != 0 || !d.Stop() {
			t.Fatal("zero deadline armed a timer")
		}
		clk.Advance(time.Hour)
		if ctx.Err() != nil {
			t.Fatal("zero deadline canceled")
		}
		d.Cancel()
		if ctx.Err() == nil {
			t.Fatal("Cancel did not cancel")
		}
	})
	t.Run("parent cancellation propagates", func(t *testing.T) {
		clk := newClock()
		parent, cancel := context.WithCancel(context.Background())
		ctx, d := WithDeadline(parent, clk, clk.Now().Add(time.Second), ErrLegTimeout)
		defer d.Cancel()
		cancel()
		if ctx.Err() == nil || d.Expired() {
			t.Fatal("parent cancellation did not reach the attempt")
		}
	})
}

// TestHeadersStopAttemptTimer_05Req25 is 05 section 6 test 4: once
// response headers arrive the attempt timer is stopped, so a body slower
// than perTryTimeout is not cut.
func TestHeadersStopAttemptTimer_05Req25(t *testing.T) {
	t.Parallel()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		<-release
		_, _ = io.WriteString(w, "slow body")
	}))
	t.Cleanup(srv.Close)
	clk := newClock()
	ctx, d := WithDeadline(context.Background(), clk, clk.Now().Add(time.Second), ErrAttemptTimeout)
	defer d.Cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	tr := transport(t, nil, nil)
	resp, err := tr.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if !d.Stop() {
		t.Fatal("the deadline fired before the headers")
	}
	clk.Advance(time.Minute) // far past perTryTimeout
	close(release)
	body, err := io.ReadAll(resp.Body)
	if err != nil || string(body) != "slow body" {
		t.Fatalf("body %q, err %v", body, err)
	}
}
