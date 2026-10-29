// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"net/http"
	runtimepprof "runtime/pprof"
	"strings"
	"sync"
	"testing"
)

// TestProfiles checks /debug/pprof/: the index, the net/http/pprof
// handlers and every registered runtime profile, operator only.
func TestProfiles(t *testing.T) {
	f := newFixture(t, nil)
	for _, tc := range []struct {
		path, contentType string
	}{
		{PathPprof, "text/html"},
		{PathPprof + "heap?debug=1", "text/plain"},
		{PathPprof + "goroutine?debug=1", "text/plain"},
		{PathPprof + "allocs?debug=1", "text/plain"},
		{PathPprof + "threadcreate?debug=1", "text/plain"},
		{PathPprof + "cmdline", "text/plain"},
	} {
		w := f.do(t, http.MethodGet, tc.path, operatorToken)
		if w.Code != http.StatusOK || !strings.HasPrefix(w.Header().Get("Content-Type"), tc.contentType) {
			t.Errorf("GET %s = %d %q, want 200 %s", tc.path, w.Code, w.Header().Get("Content-Type"), tc.contentType)
		}
	}
	for _, name := range []string{"profile", "symbol", "trace", "mutex", "block", "heap"} {
		if f.srv.profile(name) == nil {
			t.Errorf("profile %q is not served", name)
		}
	}
}

// TestGoroutineLeakProfile checks that the goroutine leak profile is
// served wherever the toolchain registers it (GA in Go 1.27).
func TestGoroutineLeakProfile(t *testing.T) {
	if runtimepprof.Lookup("goroutineleak") == nil {
		t.Skip("this toolchain has no goroutine leak profile")
	}
	f := newFixture(t, nil)
	if w := f.do(t, http.MethodGet, PathPprof+"goroutineleak?debug=1", operatorToken); w.Code != http.StatusOK {
		t.Fatalf("goroutineleak = %d %s", w.Code, w.Body)
	}
	if w := f.do(t, http.MethodGet, PathPprof, operatorToken); !strings.Contains(w.Body.String(), "goroutineleak") {
		t.Fatal("the index does not list goroutineleak")
	}
}

// rateRecorder records the rates a profile window sets and reports the
// first one.
type rateRecorder struct {
	mu    sync.Mutex
	rates []int
	first chan struct{}
}

func newRateRecorder() *rateRecorder { return &rateRecorder{first: make(chan struct{})} }

func (r *rateRecorder) set(rate int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.rates) == 0 {
		close(r.first)
	}
	r.rates = append(r.rates, rate)
}

func (r *rateRecorder) got() []int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.rates...)
}

// TestMutexAndBlockOnlyDuringSecondsRequest checks that mutex and block
// profiling run only while a ?seconds= request for them runs: on at its
// start with rate 1, recording every event, off at its end, and untouched
// by requests without a window.
func TestMutexAndBlockOnlyDuringSecondsRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		rate int
		win  func(*Server) *profileWindow
	}{
		{"mutex", 1, func(s *Server) *profileWindow { return s.mutex }},
		{"block", 1, func(s *Server) *profileWindow { return s.block }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, nil)
			rec := newRateRecorder()
			tc.win(f.srv).set = rec.set
			// No window: the cumulative profile, or a refused window.
			for _, q := range []string{"", "?debug=1", "?seconds=0", "?seconds=x"} {
				f.do(t, http.MethodGet, PathPprof+tc.name+q, operatorToken)
			}
			if got := rec.got(); len(got) != 0 {
				t.Fatalf("rates set without a window: %v", got)
			}
			ctx, cancel := context.WithCancel(t.Context())
			done := make(chan struct{})
			go func() {
				defer close(done)
				doCtx(ctx, t, f.srv, http.MethodGet, PathPprof+tc.name+"?seconds=30", operatorToken)
			}()
			<-rec.first
			if got := rec.got(); len(got) != 1 || got[0] != tc.rate {
				t.Fatalf("rates during the window %v, want [%d]", got, tc.rate)
			}
			cancel()
			<-done
			if got := rec.got(); len(got) != 2 || got[1] != 0 {
				t.Fatalf("rates after the window %v, want [%d 0]", got, tc.rate)
			}
		})
	}
}

// TestOverlappingWindows checks that overlapping ?seconds= requests turn
// a profile on once and off only after the last one ends.
func TestOverlappingWindows(t *testing.T) {
	rec := newRateRecorder()
	w := newProfileWindow(rec.set, 7)
	w.begin()
	w.begin()
	w.end()
	if got := rec.got(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("rates with one window left %v, want [7]", got)
	}
	w.end()
	if got := rec.got(); len(got) != 2 || got[1] != 0 {
		t.Fatalf("rates after both windows %v, want [7 0]", got)
	}
}
