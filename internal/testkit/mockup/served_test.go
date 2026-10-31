// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

// TestOnServed checks the hook a benchmark measures the mock's own latency
// with: it runs once per answered request, after the handler wrote the
// response, with the delay the Behavior drew and an elapsed time that
// covers that delay.
func TestOnServed(t *testing.T) {
	var mu sync.Mutex
	type call struct{ elapsed, delay time.Duration }
	var calls []call
	s := start(t, Config{
		Behavior: Behavior{BodySize: 1024},
		LogSize:  -1,
		OnServed: func(elapsed, delay time.Duration) {
			mu.Lock()
			calls = append(calls, call{elapsed, delay})
			mu.Unlock()
		},
	})
	if err := s.SetRoute("/delayed", Behavior{Delay: Fixed(time.Millisecond)}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/", "/delayed", "/"} {
		resp, err := http.Get(s.URL() + path) //nolint:noctx // a loopback test request
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 3 {
		t.Fatalf("OnServed ran %d times, want 3", len(calls))
	}
	if calls[0].delay != 0 || calls[2].delay != 0 {
		t.Errorf("undelayed requests reported delays %v and %v", calls[0].delay, calls[2].delay)
	}
	if calls[1].delay != time.Millisecond || calls[1].elapsed < time.Millisecond {
		t.Errorf("delayed request: delay %v, elapsed %v; want the 1ms delay within the elapsed time", calls[1].delay, calls[1].elapsed)
	}
}
