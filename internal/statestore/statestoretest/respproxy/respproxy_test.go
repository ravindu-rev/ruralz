// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// startProxy starts a proxy whose reply delays run on clk.
func startProxy(t *testing.T, c Config, clk *clocktest.Fake) *Proxy {
	t.Helper()
	p, err := start(t.Context(), c, clk)
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
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}}, fakeClock())
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
	for _, e := range p.LogSince(0) {
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
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}}, clk)
	if err := p.SetReplyDelays(ReplyDelay{Command: "VSIM", Delay: time.Second}); err != nil {
		t.Fatal(err)
	}
	slow, fast := dialClient(t, p.Addr()), dialClient(t, p.Addr())
	slow.send(cmd("VSIM", "idx", "VALUES", "1", "0.5"))
	waitFor(t, "the VSIM reply held", func() bool { return held(p.Counter) == 1 })
	fast.send(cmd("EVALSHA", "s", "0"))
	if got := fast.reply(); !strings.HasPrefix(got, "-NOSCRIPT") {
		t.Fatalf("EVALSHA = %q", got)
	}
	if s := p.Snapshot(); held(p.Counter) != 1 || s.Replies != 1 {
		t.Fatalf("Snapshot = %+v, want the VSIM reply still held", s)
	}
	clk.Advance(time.Second)
	if got := slow.reply(); got != "*0" {
		t.Fatalf("VSIM = %q", got)
	}
}

// TestProxySlowRepliesHeldSideBySide checks the slow reply fault through a
// real connection: the proxy goes on reading while a reply is held, so each pipelined reply
// is held its own 4 s from when the proxy read it, side by side. A reply
// read 1 s after the first reaches the client at 5 s; were the holds one
// after another, it would wait for the first's to end and then 4 s more,
// to 8 s, past a 5 s stateStoreTimeout.
func TestProxySlowRepliesHeldSideBySide(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	up := startUpstream(t, defaultReply)
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}}, clk)
	if err := p.SetReplyDelays(ReplyDelay{Command: "EVALSHA", Delay: 4 * time.Second}); err != nil {
		t.Fatal(err)
	}
	cl := dialClient(t, p.Addr())
	cl.send(cmd("EVALSHA", "a", "0"))
	waitFor(t, "the first reply held", func() bool { return held(p.Counter) == 1 })
	clk.Advance(time.Second)
	cl.send(cmd("EVALSHA", "b", "0"))
	waitFor(t, "the second reply held beside the first", func() bool { return held(p.Counter) == 2 })
	clk.Advance(3 * time.Second)
	if got := cl.reply(); !strings.HasPrefix(got, "-NOSCRIPT") {
		t.Fatalf("first EVALSHA = %q", got)
	}
	if s := p.Snapshot(); held(p.Counter) != 1 || s.Replies != 1 {
		t.Fatalf("Snapshot = %+v at 4 s, want the second reply still held and one reply", s)
	}
	clk.Advance(time.Second)
	if got := cl.reply(); !strings.HasPrefix(got, "-NOSCRIPT") {
		t.Fatalf("second EVALSHA = %q", got)
	}
	if s := p.Snapshot(); held(p.Counter) != 0 || s.Replies != 2 {
		t.Fatalf("Snapshot = %+v at 5 s, want none held and both replies", s)
	}
}

// TestProxyCountsPipelinedBatchOnce checks the counts through a real
// connection: a batch of 64
// EVALSHA commands sent in one write counts 64 commands and 64 replies,
// each logged once with its reply, with no reply delay and with every
// EVALSHA reply held, the held replies released by a change of the delays.
func TestProxyCountsPipelinedBatchOnce(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		name := "no delay"
		if delayed {
			name = "replies held"
		}
		t.Run(name, func(t *testing.T) {
			clk := clocktest.New(time.Unix(0, 0))
			up := startUpstream(t, defaultReply)
			p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: up.addr()}}, clk)
			if delayed {
				if err := p.SetReplyDelays(ReplyDelay{Command: "EVALSHA", Delay: time.Hour}); err != nil {
					t.Fatal(err)
				}
			}
			cl := dialClient(t, p.Addr())
			var batch strings.Builder
			for i := range 64 {
				batch.WriteString(cmd("EVALSHA", "sha", "1", "rz:rl:flood:{"+strconv.Itoa(i)+"}"))
			}
			cl.send(batch.String())
			if delayed {
				waitFor(t, "a held reply", func() bool { return held(p.Counter) > 0 })
				if err := p.SetReplyDelays(); err != nil {
					t.Fatal(err)
				}
			}
			for i := range 64 {
				if got := cl.reply(); !strings.HasPrefix(got, "-NOSCRIPT") {
					t.Fatalf("reply %d = %q", i, got)
				}
			}
			s := p.Snapshot()
			if s.Commands != 64 || s.Count("EVALSHA") != 64 || s.Replies != 64 || s.Errors["NOSCRIPT"] != 64 || held(p.Counter) != 0 || s.Desyncs != 0 {
				t.Errorf("Snapshot = %+v, want 64 EVALSHA commands and replies, none held", s)
			}
			answered := 0
			for _, e := range p.LogSince(0) {
				if e.Name == "EVALSHA" && e.Reply.Kind == '-' {
					answered++
				}
			}
			if answered != 64 {
				t.Errorf("the log holds %d answered EVALSHA commands, want 64", answered)
			}
		})
	}
}

// TestProxyFaultsPassThrough checks that the fault proxy's modes act on
// the proxied connections: Reset resets a live connection, and after
// SetUpstream new connections reach the new server while counting goes
// on (a scripted failover).
func TestProxyFaultsPassThrough(t *testing.T) {
	a := startUpstream(t, func([]string) string { return "+a\r\n" })
	b := startUpstream(t, func([]string) string { return "+b\r\n" })
	p := startProxy(t, Config{Fault: faultproxy.Config{Upstream: a.addr()}}, fakeClock())
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
	p, err := start(t.Context(), Config{Fault: faultproxy.Config{Upstream: up.addr()}}, clk)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cl := dialClient(t, p.Addr())
	cl.send(cmd("PING"))
	waitFor(t, "the PING reply held", func() bool { return held(p.Counter) == 1 })
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatalf("second Close = %v", err)
	}
	if n := held(p.Counter); n != 0 {
		t.Fatalf("%d replies held after Close", n)
	}
}

// TestProxyContextEndCloses checks that the end of the Start context
// closes the proxy, held replies included.
func TestProxyContextEndCloses(t *testing.T) {
	clk := clocktest.New(time.Unix(0, 0))
	up := startUpstream(t, defaultReply)
	ctx, cancel := context.WithCancel(t.Context())
	p, err := start(ctx, Config{Fault: faultproxy.Config{Upstream: up.addr()}}, clk)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.SetReplyDelays(ReplyDelay{Delay: time.Hour}); err != nil {
		t.Fatal(err)
	}
	cl := dialClient(t, p.Addr())
	cl.send(cmd("PING"))
	waitFor(t, "the PING reply held", func() bool { return held(p.Counter) == 1 })
	cancel()
	waitFor(t, "the proxy closed", func() bool {
		return held(p.Counter) == 0 && p.SetMode(faultproxy.Pass{}) != nil
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
		{"a ConnTap of its own", Config{Fault: faultproxy.Config{Upstream: "127.0.0.1:1", ConnTap: func(faultproxy.ConnInfo) faultproxy.ConnHooks { return faultproxy.ConnHooks{} }}}},
	} {
		if p, err := Start(t.Context(), tc.c); err == nil {
			_ = p.Close()
			t.Errorf("%s: Start succeeded, want an error", tc.name)
		}
	}
}
