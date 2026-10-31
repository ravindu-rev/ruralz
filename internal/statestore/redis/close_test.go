// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// TestCloseRefusesLaterCalls checks Close: every connection closes, later
// calls fail with RZ-STS-002 like calls on a closed connection, nothing
// is admitted, and a second Close returns nil.
func TestCloseRefusesLaterCalls(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	f.consume(gcraCall(statestore.DigestOf("k"), oneLimit()))
	if err := f.d.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if f.d.conns.len() != 0 {
		t.Fatalf("%d connections open after Close", f.d.conns.len())
	}
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	f.consume(c)
	l := lookupCall(statestore.CacheKey{}, statestore.Digest{})
	f.read(l)
	w := &statestore.Write{Kind: statestore.OpCacheInvalidate, Timeout: callTO}
	f.write(w)
	if c.Err != statestore.ErrFailed || l.Err != statestore.ErrFailed || w.Err != statestore.ErrFailed {
		t.Fatalf("after Close: consume %v, read %v, write %v; want RZ-STS-002", c.Err, l.Err, w.Err)
	}
	if f.d.AdmitStoreBytes(statestore.CacheKey{}, 0) {
		t.Fatal("a store was admitted after Close")
	}
	if err := f.d.Close(t.Context()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestCloseEndsAHeldCall checks that Close ends a call whose reply is
// held, with RZ-STS-002, and waits for its goroutine.
func TestCloseEndsAHeldCall(t *testing.T) {
	f := newFixture(t, fixtureOptions{})
	f.ready()
	hold := f.srv.inject(&fault{cmd: "EVALSHA", hold: make(chan struct{})})
	defer close(hold.hold)
	c := gcraCall(statestore.DigestOf("k"), oneLimit())
	done := background(func() { f.consume(c) })
	waitFor(t, "the call to be sent", func() bool { return f.srv.count("EVALSHA") == 1 })
	if err := f.d.Close(t.Context()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	<-done
	if c.Err != statestore.ErrFailed {
		t.Fatalf("held call at Close: Err %v, want RZ-STS-002", c.Err)
	}
	if n := f.d.goroutines.Load(); n != 0 {
		t.Fatalf("%d driver goroutines after Close", n)
	}
}

// TestCloseDuringFirstDial checks that Close cuts a blocked first
// connection short instead of waiting for the dial timeout.
func TestCloseDuringFirstDial(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	f := newFixture(t, fixtureOptions{setup: func(s *fakeServer) {
		s.inject(&fault{cmd: "SCRIPT LOAD", hold: release})
	}})
	waitFor(t, "the first dial", func() bool { return f.srv.count("SCRIPT LOAD") == 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	if err := f.d.Close(ctx); err != nil {
		t.Fatalf("Close during the first dial: %v", err)
	}
}

// TestLogsNeverCarryTheURL checks that logs name shards ss-<8 hex>, never
// an address, a port or a password.
func TestLogsNeverCarryTheURL(t *testing.T) {
	f := newFixture(t, fixtureOptions{
		fake:   fakeOptions{password: "s3cretpw", reject: true},
		urlFor: func(s *fakeServer) string { return "redis://:s3cretpw@" + s.addr },
	})
	f.ready()
	for range 10 {
		f.consume(gcraCall(statestore.DigestOf("k"), oneLimit()))
		f.clk.Advance(400 * time.Millisecond)
	}
	text := f.logs.text()
	if !strings.Contains(text, "shard=ss-") {
		t.Fatalf("no shard id in the logs:\n%s", text)
	}
	_, port, _ := strings.Cut(f.srv.addr, ":")
	for _, leak := range []string{"s3cretpw", "127.0.0.1", port, strconv.Quote(f.srv.addr)} {
		if strings.Contains(text, leak) {
			t.Fatalf("the logs carry %q:\n%s", leak, text)
		}
	}
}
