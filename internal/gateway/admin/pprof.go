// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"net/http"
	"net/http/pprof"
	"runtime"
	runtimepprof "runtime/pprof"
	"strconv"
	"sync"
)

// Profiling rates while a ?seconds= request for the mutex or block profile
// runs, recording every event for full detail; both profiles are off
// otherwise (docs/architecture/03-data-plane.md "Admin endpoints").
const (
	// MutexProfileFraction reports one in this many mutex contention
	// events (runtime.SetMutexProfileFraction): every one.
	MutexProfileFraction = 1
	// BlockProfileRate samples one blocking event per this many
	// nanoseconds spent blocked (runtime.SetBlockProfileRate): every one.
	BlockProfileRate = 1
)

// secondsParam is the query parameter of a profile window.
const secondsParam = "seconds"

// profile returns the handler of the profile path /debug/pprof/<name>,
// nil for a name that is neither a net/http/pprof handler nor a profile
// runtime/pprof registers.
func (s *Server) profile(name string) http.Handler {
	switch name {
	case "":
		return http.HandlerFunc(pprof.Index)
	case "cmdline":
		return http.HandlerFunc(pprof.Cmdline)
	case "profile":
		return http.HandlerFunc(pprof.Profile)
	case "symbol":
		return http.HandlerFunc(pprof.Symbol)
	case "trace":
		return http.HandlerFunc(pprof.Trace)
	}
	if runtimepprof.Lookup(name) == nil {
		return nil
	}
	h := pprof.Handler(name)
	switch name {
	case "mutex":
		return windowed{w: s.mutex, h: h}
	case "block":
		return windowed{w: s.block, h: h}
	}
	return h
}

// profileWindow turns one runtime profile on while at least one ?seconds=
// request for it runs, and off when the last one ends.
type profileWindow struct {
	set  func(rate int)
	rate int

	mu sync.Mutex
	n  int // requests running
}

func newProfileWindow(set func(int), rate int) *profileWindow {
	return &profileWindow{set: set, rate: rate}
}

// begin counts a request, turning the profile on for the first.
func (p *profileWindow) begin() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.n == 0 {
		p.set(p.rate)
	}
	p.n++
}

// end uncounts a request, turning the profile off after the last.
func (p *profileWindow) end() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.n--
	if p.n == 0 {
		p.set(0)
	}
}

// windowed serves a profile inside its window when the request asks for
// a delta over ?seconds= (a positive integer, as net/http/pprof reads it).
type windowed struct {
	w *profileWindow
	h http.Handler
}

func (x windowed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if sec, err := strconv.ParseInt(r.URL.Query().Get(secondsParam), 10, 64); err == nil && sec > 0 {
		x.w.begin()
		defer x.w.end()
	}
	x.h.ServeHTTP(w, r)
}

// setMutexProfileFraction sets the mutex profile fraction.
func setMutexProfileFraction(rate int) { runtime.SetMutexProfileFraction(rate) }

// setBlockProfileRate sets the block profile rate.
func setBlockProfileRate(rate int) { runtime.SetBlockProfileRate(rate) }
