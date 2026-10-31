// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// TestProxyModes checks the fault proxy's modes: with PassFirst it
// forwards unchanged until a mode request names the configured delay,
// which O2 sends once the Node's connections to the State Store are set
// up, and "pass" returns to forwarding; without PassFirst the delay holds
// from the start; another mode name is refused.
func TestProxyModes(t *testing.T) {
	echo, err := StartEcho(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = echo.Close() })
	delay := faultproxy.Delay{Mean: 1500 * time.Millisecond, StdDev: time.Millisecond}

	p, err := StartProxy(t.Context(), ProxyConfig{Upstream: echo.Ready().Addr, Mean: delay.Mean, StdDev: delay.StdDev, PassFirst: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if m := p.p.Mode(); m != (faultproxy.Pass{}) {
		t.Errorf("PassFirst proxy starts in %v, want pass", m)
	}
	if r := p.Handle(t.Context(), Request{Op: OpMode, Name: ProxyDelay}); !r.OK {
		t.Fatalf("mode delay: %+v", r)
	}
	if m := p.p.Mode(); m != delay {
		t.Errorf("after mode delay: %v, want %v", m, delay)
	}
	if r := p.Handle(t.Context(), Request{Op: OpMode, Name: ProxyPass}); !r.OK {
		t.Fatalf("mode pass: %+v", r)
	}
	if m := p.p.Mode(); m != (faultproxy.Pass{}) {
		t.Errorf("after mode pass: %v, want pass", m)
	}
	if r := p.Handle(t.Context(), Request{Op: OpMode, Name: "blackhole"}); r.OK {
		t.Error("mode blackhole accepted")
	}

	q, err := StartProxy(t.Context(), ProxyConfig{Upstream: echo.Ready().Addr, Mean: delay.Mean, StdDev: delay.StdDev})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = q.Close() })
	if m := q.p.Mode(); m != delay {
		t.Errorf("proxy without PassFirst starts in %v, want %v", m, delay)
	}
}

// TestSinkModes checks the sink's modes over the protocol: "stall" holds
// exports so the Node's telemetry queues fill, "accept" answers them
// again, another name is refused; and a window may fold only records
// whose attribute is present (Where).
func TestSinkModes(t *testing.T) {
	s, err := StartSink(t.Context(), SinkConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := session(t, s)
	if _, err := c.Do(Request{Op: OpMode, Name: SinkStall}); err != nil {
		t.Fatal(err)
	}
	if m := s.s.Mode(); m != (otlpsink.Stall{}) {
		t.Errorf("after mode stall: %v", m)
	}
	if _, err := c.Do(Request{Op: OpMode, Name: SinkAccept}); err != nil {
		t.Fatal(err)
	}
	if m := s.s.Mode(); m != (otlpsink.Accept{}) {
		t.Errorf("after mode accept: %v", m)
	}
	if _, err := c.Do(Request{Op: OpMode, Name: "drop"}); err == nil {
		t.Error("mode drop accepted")
	}
	base := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, err := c.Do(Request{Op: OpWindow, Name: "skipped", Start: base, End: base.Add(time.Minute), Where: "failure_modes"}); err != nil {
		t.Fatal(err)
	}
	if r, err := c.Do(Request{Op: OpResult, Name: "skipped"}); err != nil || r.Window == nil {
		t.Errorf("filtered window result = %+v, %v", r, err)
	}
}

// TestMockHeader checks that the mock adds its configured headers to
// every response, such as the Cache-Control the cache key flood's
// entries need.
func TestMockHeader(t *testing.T) {
	m, err := StartMock(t.Context(), MockConfig{BodySize: 16, Header: http.Header{"Cache-Control": {"public, max-age=60"}, "Vary": {"X-Bench-Key"}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.Close() })
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+m.Ready().Addr+"/bench/1k", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=60" {
		t.Errorf("Cache-Control = %q", got)
	}
	if got := resp.Header.Get("Vary"); got != "X-Bench-Key" {
		t.Errorf("Vary = %q", got)
	}
}
