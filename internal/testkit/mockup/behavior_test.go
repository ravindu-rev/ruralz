// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// TestDelayOnLiveServer checks that a route's Delay holds its answer for
// at least the delay, while other paths answer at once.
func TestDelayOnLiveServer(t *testing.T) {
	s := start(t, Config{})
	if err := s.SetRoute("/slow", Behavior{Delay: 30 * time.Millisecond}); err != nil {
		t.Fatal(err)
	}
	c := client(t, h1, testPKI{})
	for range 3 {
		r, err := do(t, c, http.MethodGet, s.URL()+"/slow", "", nil)
		ok(t, r, err, 200)
		if r.elapsed < 30*time.Millisecond {
			t.Fatalf("delayed answer after %v, want at least 30ms", r.elapsed)
		}
	}
	r, err := do(t, c, http.MethodGet, s.URL()+"/", "", nil)
	ok(t, r, err, 200)
}

// TestWaitEnds checks the delay's wait: no delay returns at once, a delay
// returns once it passed, and the end of the request or of the Server
// ends a delay early, which the handler then aborts.
func TestWaitEnds(t *testing.T) {
	s := start(t, Config{})
	if !s.wait(context.Background(), 0) || !s.wait(context.Background(), -time.Second) {
		t.Fatal("wait without a delay = false, want true at once")
	}
	if !s.wait(context.Background(), time.Millisecond) {
		t.Fatal("wait of a passed delay = false, want true")
	}
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if s.wait(ended, time.Hour) {
		t.Fatal("wait under an ended request = true, want false")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.wait(context.Background(), time.Hour) {
		t.Fatal("wait on a closed Server = true, want false")
	}
}
