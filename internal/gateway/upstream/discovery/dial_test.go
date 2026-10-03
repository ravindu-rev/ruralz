// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// errRefused stands for the egress guard refusing a socket address in
// net.Dialer.Control (05 req 9).
var errRefused = errors.New("refused by the egress guard")

// scriptedDial answers per address: an error, a block until ctx ends, or a
// connection.
type scriptedDial struct {
	mu      sync.Mutex
	tried   []string
	errs    map[string]error
	blocked map[string]bool
	entered chan string
}

func (d *scriptedDial) dial(ctx context.Context, network, address string) (net.Conn, error) {
	d.mu.Lock()
	d.tried = append(d.tried, network+" "+address)
	err, block := d.errs[address], d.blocked[address]
	d.mu.Unlock()
	if d.entered != nil {
		d.entered <- address
	}
	if block {
		<-ctx.Done()
		return nil, context.Cause(ctx)
	}
	if err != nil {
		return nil, err
	}
	c1, c2 := net.Pipe()
	_ = c2.Close()
	return c1, nil
}

func (d *scriptedDial) triedList() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.tried...)
}

func aps(s ...string) []netip.AddrPort {
	out := make([]netip.AddrPort, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddrPort(a)
	}
	return out
}

// TestDialAnswerOrder covers 05 req 5 with 05 req 9: the cached IPs are
// tried in answer order; a refused or failed address moves on to the next.
func TestDialAnswerOrder(t *testing.T) {
	d := &scriptedDial{errs: map[string]error{
		"127.0.0.1:80": errRefused,
		"10.0.0.1:80":  syscall.ECONNREFUSED,
	}}
	clk := clocktest.New(epoch())
	c, err := Dial(t.Context(), clk, d.dial, "tcp", aps("127.0.0.1:80", "10.0.0.1:80", "10.0.0.2:80", "10.0.0.3:80"), 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	want := []string{"tcp 127.0.0.1:80", "tcp 10.0.0.1:80", "tcp 10.0.0.2:80"}
	if got := d.triedList(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("tried %q, want %q", got, want)
	}
	if clk.Pending() != 0 {
		t.Fatal("budget timer left armed")
	}

	// Every address failing joins the failures.
	d = &scriptedDial{errs: map[string]error{"10.0.0.1:80": errRefused, "10.0.0.2:80": syscall.ECONNREFUSED}}
	_, err = Dial(t.Context(), clk, d.dial, "tcp", aps("10.0.0.1:80", "10.0.0.2:80"), 0)
	if !errors.Is(err, errRefused) || !errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, ErrDialBudget) {
		t.Fatalf("error %v", err)
	}
}

// TestDialBudget: one 1 s budget covers every address (05 req 5); a
// black-holed first address spends it and the dial fails as a connect
// timeout without trying the rest.
func TestDialBudget(t *testing.T) {
	clk := clocktest.New(epoch())
	d := &scriptedDial{blocked: map[string]bool{"10.0.0.1:80": true}, entered: make(chan string, 4)}
	out := make(chan error, 1)
	go func() {
		_, err := Dial(t.Context(), clk, d.dial, "tcp", aps("10.0.0.1:80", "10.0.0.2:80"), 0)
		out <- err
	}()
	<-d.entered
	clk.Advance(DialBudget - time.Millisecond)
	select {
	case err := <-out:
		t.Fatalf("returned before the budget: %v", err)
	default:
	}
	clk.Advance(time.Millisecond)
	err := <-out
	if !errors.Is(err, ErrDialBudget) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	if got := d.triedList(); len(got) != 1 {
		t.Fatalf("tried %q after the budget ran out", got)
	}
}

func TestDialNoAddressesAndParentCancel(t *testing.T) {
	d := &scriptedDial{}
	if _, err := Dial(t.Context(), clocktest.New(epoch()), d.dial, "tcp", nil, 0); !errors.Is(err, ErrNoAddresses) {
		t.Fatalf("error %v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	d = &scriptedDial{blocked: map[string]bool{"10.0.0.1:80": true}}
	_, err := Dial(ctx, clocktest.New(epoch()), d.dial, "tcp", aps("10.0.0.1:80", "10.0.0.2:80"), time.Second)
	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrDialBudget) || len(d.triedList()) != 1 {
		t.Fatalf("error %v, tried %q", err, d.triedList())
	}
}

// TestDialRealSockets dials a closed port then a listening one through
// net.Dialer on the real clock.
func TestDialRealSockets(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	closed, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closed.Addr().String()
	_ = closed.Close()
	go func() {
		if c, aerr := ln.Accept(); aerr == nil {
			_ = c.Close()
		}
	}()
	var nd net.Dialer
	c, err := Dial(t.Context(), clock.Real(), nd.DialContext, "tcp", aps(closedAddr, ln.Addr().String()), 0)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	_ = c.Close()
}
