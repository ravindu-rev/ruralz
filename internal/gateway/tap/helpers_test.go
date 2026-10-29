// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"bytes"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// stripeCounter is an emit.Counter recording what each stripe adds.
type stripeCounter struct {
	mu sync.Mutex
	by map[emit.Stripe]uint64
}

func (c *stripeCounter) Add(s emit.Stripe, n uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.by == nil {
		c.by = map[emit.Stripe]uint64{}
	}
	c.by[s] += n
}

func (c *stripeCounter) total() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var t uint64
	for _, n := range c.by {
		t += n
	}
	return t
}

func (c *stripeCounter) stripe(s emit.Stripe) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.by[s]
}

// seqSource is a rand.Source returning vals in order, then repeating the
// last one; it counts its draws and is safe for concurrent use.
type seqSource struct {
	mu    sync.Mutex
	vals  []uint64
	draws int
}

func (s *seqSource) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.vals[min(s.draws, len(s.vals)-1)]
	s.draws++
	return v
}

func (s *seqSource) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.draws
}

// fullEvent returns an event with every member set and the wire value it
// must encode to.
func fullEvent() (Event, adminapi.TapEvent) {
	ev := Event{
		Time:     time.Date(2026, 9, 26, 12, 0, 0, 123_000_000, time.UTC),
		TraceID:  [16]byte{0x0a, 0xf7, 0x65, 0x19, 0x16, 0xcd, 0x43, 0xdd, 0x84, 0x48, 0xeb, 0x21, 0x1c, 0x80, 0x31, 0x9c},
		Listener: "public", Protocol: "http2", Route: "orders", Method: "GET", Host: "api.example", Path: "/orders/7",
		Status: 429, Code: "RZ-RL-002", Duration: 1250 * time.Microsecond, RequestBytes: 0, ResponseBytes: 121,
		Upstream: "inventory", Endpoint: "10.0.0.7:8080", Consumer: "acme",
		RequestHeaders:  http.Header{"Authorization": {"Bearer abc.def"}, "Accept": {"application/json"}},
		ResponseHeaders: http.Header{"Retry-After": {"1"}},
	}
	want := adminapi.TapEvent{
		Time: "2026-09-26T12:00:00.123Z", TraceID: "0af7651916cd43dd8448eb211c80319c", Listener: "public",
		Protocol: "http2", Route: "orders", Method: "GET", Host: "api.example", Path: "/orders/7",
		Status: 429, Code: "RZ-RL-002", DurationMs: 1.25, RequestBytes: 0, ResponseBytes: 121,
		Upstream: "inventory", Endpoint: "10.0.0.7:8080", Consumer: "acme",
		RequestHeaders:  map[string][]string{"authorization": {"[REDACTED]"}, "accept": {"application/json"}},
		ResponseHeaders: map[string][]string{"retry-after": {"1"}},
	}
	return ev, want
}

// sparseEvent returns an event with the optional members empty and its
// wire value.
func sparseEvent() (Event, adminapi.TapEvent) {
	ev := Event{
		Time:     time.Date(2026, 9, 26, 12, 0, 1, 0, time.UTC),
		TraceID:  [16]byte{0x4b, 0xf9, 0x2f, 0x35, 0x77, 0xb3, 0x4d, 0xa6, 0xa3, 0xce, 0x92, 0x9d, 0x0e, 0x0e, 0x47, 0x36},
		Listener: "public", Protocol: "http1", Route: "_unmatched", Method: "POST", Host: "api.example", Path: "/nowhere",
		Status: 404, Duration: 50 * time.Microsecond, RequestBytes: 12, ResponseBytes: 110,
	}
	want := adminapi.TapEvent{
		Time: "2026-09-26T12:00:01Z", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736", Listener: "public",
		Protocol: "http1", Route: "_unmatched", Method: "POST", Host: "api.example", Path: "/nowhere",
		Status: 404, DurationMs: 0.05, RequestBytes: 12, ResponseBytes: 110,
	}
	return ev, want
}

// drain moves everything s holds into a byte slice, as the stream does.
func drain(s *subscriber) []byte {
	var out bytes.Buffer
	for {
		b := s.pending()
		if len(b) == 0 {
			return out.Bytes()
		}
		out.Write(b)
		s.consume(len(b))
	}
}

// lines splits NDJSON into lines without their newlines.
func lines(t *testing.T, b []byte) [][]byte {
	t.Helper()
	if len(b) == 0 {
		return nil
	}
	if b[len(b)-1] != '\n' {
		t.Fatalf("stream %q does not end with a newline", b)
	}
	return bytes.Split(b[:len(b)-1], []byte("\n"))
}
