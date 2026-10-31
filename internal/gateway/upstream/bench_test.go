// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// BenchmarkLeg measures one pooled-connection GET leg end to end on
// loopback: the attempt loop, the transport (net/http's allocations
// included) and the body read. Compare -benchmem across changes.
func BenchmarkLeg(b *testing.B) {
	srv := newServer(b, status(http.StatusOK, "ok")).addr
	m := NewManager(Options{Clock: clock.Real(), Dial: newDial().dial})
	s, err := m.Compile(b.Context(), Input{Upstreams: []Config{{Upstream: newUpstream("orders", srv)}}})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	s.Activate()
	u, _ := s.Upstream("orders")
	x := exchange(http.MethodGet, "/v1/orders/42")
	o := outbound(x, nil)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		o.Deadline = time.Now().Add(15 * time.Second)
		r, err := u.Leg(b.Context(), o, Target{})
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
	}
}
