// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

func startProxy(t *testing.T, c Config) *Proxy {
	t.Helper()
	p, err := Start(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

// TestProxyCountsThroughFaultProxy checks the proxy end to end: commands
// and replies cross unchanged and are counted per name, error code and
// connection.
func TestProxyCountsThroughFaultProxy(t *testing.T) {
	up := startUpstream(t, defaultReply)
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}})
	cl := dialClient(t, p.Addr())
	cl.send(cmd("PING") + cmd("EVALSHA", "s", "1", "rl:{a}"))
	if got := cl.reply(); got != "+PONG" {
		t.Fatalf("PING = %q", got)
	}
	if got := cl.reply(); !strings.HasPrefix(got, "-NOSCRIPT ") {
		t.Fatalf("EVALSHA = %q, want NOSCRIPT", got)
	}
	cl.send(cmd("GET", "k"))
	if got := cl.reply(); got != "$-1" {
		t.Fatalf("GET = %q", got)
	}
	s := p.Snapshot()
	if s.Commands != 3 || s.Replies != 3 || s.Count("EVALSHA") != 1 || s.Errors["NOSCRIPT"] != 1 || s.Desyncs != 0 {
		t.Errorf("Snapshot = %+v", s)
	}
	conns := p.Conns()
	if len(conns) != 1 {
		t.Fatalf("Conns = %v, want one", conns)
	}
	for _, e := range p.Log() {
		if e.Conn != conns[0].ID {
			t.Errorf("%s on connection %d, want %d", e.Name, e.Conn, conns[0].ID)
		}
	}
	if st := p.Stats(); st.BytesUp == 0 || st.BytesDown == 0 {
		t.Errorf("fault proxy Stats = %+v, want bytes both ways", st)
	}
}

// TestProxySlowReplyDelaysOnlyMatchingReplies checks the slow reply
// fault through real connections: the held reply of one command leaves
// another connection's replies on time, as a slow vector search must
// never delay a rate-limit reply.
func TestProxySlowReplyDelaysOnlyMatchingReplies(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	up := startUpstream(t, defaultReply)
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}, Counter: CounterConfig{Clock: clk}})
	if err := p.SetReplyDelays(ReplyDelay{Command: "VSIM", Delay: time.Second}); err != nil {
		t.Fatal(err)
	}
	slow, fast := dialClient(t, p.Addr()), dialClient(t, p.Addr())
	slow.send(cmd("VSIM", "idx", "VALUES", "1", "0.5"))
	waitFor(t, "the VSIM reply held", func() bool { return p.Snapshot().Held == 1 })
	fast.send(cmd("EVALSHA", "s", "0"))
	if got := fast.reply(); !strings.HasPrefix(got, "-NOSCRIPT") {
		t.Fatalf("EVALSHA = %q", got)
	}
	if s := p.Snapshot(); s.Held != 1 || s.Replies != 1 {
		t.Fatalf("Snapshot = %+v, want the VSIM reply still held", s)
	}
	clk.Advance(time.Second)
	if got := slow.reply(); got != "*0" {
		t.Fatalf("VSIM = %q", got)
	}
}

// TestProxyFaultsPassThrough checks that the fault proxy's modes act on
// the proxied connections: Reset resets a live connection, and after
// SetUpstream new connections reach the new server while counting goes
// on (a scripted failover).
func TestProxyFaultsPassThrough(t *testing.T) {
	a := startUpstream(t, func([]string) string { return "+a\r\n" })
	b := startUpstream(t, func([]string) string { return "+b\r\n" })
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: a.addr()}})
	live := dialClient(t, p.Addr())
	live.send(cmd("PING"))
	if got := live.reply(); got != "+a" {
		t.Fatalf("PING = %q, want the first server", got)
	}
	if err := p.SetMode(faultproxy.Reset{}); err != nil {
		t.Fatal(err)
	}
	if _, err := live.read(); !isReset(err) {
		t.Fatalf("read after Reset = %v, want a reset", err)
	}
	p.SetUpstream(b.addr())
	if err := p.SetMode(faultproxy.Pass{}); err != nil {
		t.Fatal(err)
	}
	fresh := dialClient(t, p.Addr())
	fresh.send(cmd("PING"))
	if got := fresh.reply(); got != "+b" {
		t.Fatalf("PING after SetUpstream = %q, want the second server", got)
	}
	if s := p.Snapshot(); s.Count("PING") != 2 || s.Replies != 2 {
		t.Errorf("Snapshot = %+v, want both PINGs and replies", s)
	}
}

// TestProxyCloseReleasesHeldReply checks that Close never waits on a
// held reply: a reply held for an hour on a clock that never moves.
func TestProxyCloseReleasesHeldReply(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	up := startUpstream(t, defaultReply)
	p, err := Start(t.Context(), Config{Fault: faultproxy.Config{Upstream: up.addr()}, Counter: CounterConfig{Clock: clk}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cl := dialClient(t, p.Addr())
	cl.send(cmd("PING"))
	waitFor(t, "the PING reply held", func() bool { return p.Snapshot().Held == 1 })
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if s := p.Snapshot(); s.Held != 0 {
		t.Fatalf("Held = %d after Close", s.Held)
	}
}

// TestProxyContextEndCloses checks that the end of the Start context
// closes the proxy, held replies included.
func TestProxyContextEndCloses(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	up := startUpstream(t, defaultReply)
	ctx, cancel := context.WithCancel(t.Context())
	p, err := Start(ctx, Config{Fault: faultproxy.Config{Upstream: up.addr()}, Counter: CounterConfig{Clock: clk}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cl := dialClient(t, p.Addr())
	cl.send(cmd("PING"))
	waitFor(t, "the PING reply held", func() bool { return p.Snapshot().Held == 1 })
	cancel()
	waitFor(t, "the proxy closed", func() bool {
		return p.Snapshot().Held == 0 && p.SetMode(faultproxy.Pass{}) != nil
	})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestStartErrors checks the refused configurations.
func TestStartErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		c    Config
	}{
		{"no upstream", Config{}},
		{"a ConnTap of its own", Config{Fault: faultproxy.Config{Upstream: "127.0.0.1:1", ConnTap: func(faultproxy.ConnInfo) faultproxy.TapFunc { return nil }}}},
	} {
		if p, err := Start(t.Context(), tc.c); err == nil {
			_ = p.Close()
			t.Errorf("%s: Start succeeded, want an error", tc.name)
		}
	}
}
