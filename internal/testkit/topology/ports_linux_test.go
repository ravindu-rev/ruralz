// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package topology

import (
	"context"
	"errors"
	"net"
	"strconv"
	"syscall"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
)

// The topology holds every port it hands out with sockets bound to it
// that never listen, until its teardown. The kernel never gives a port in
// its bind table to a bind of port 0 or to a connection's ephemeral port,
// so no other socket takes the port before the Node binds it; a bind
// without SO_REUSEADDR shows the hold, failing with EADDRINUSE. A Node's
// listener, which sets SO_REUSEADDR, binds the port and listens all the
// same, and the teardown closes the holding sockets.
func TestFreePortHeldUntilTeardown(t *testing.T) {
	topo := standinTopology(t)
	port, err := topo.freePort(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	if err := bindWithoutReuse(t.Context(), addr); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("a bind without SO_REUSEADDR to the held port = %v, want EADDRINUSE", err)
	}
	listeners := []struct {
		name   string
		listen func(ctx context.Context) (net.Listener, error)
	}{
		{"reuseport.Listen, as ruralzd and the stand-in bind", func(ctx context.Context) (net.Listener, error) {
			return reuseport.Listen(ctx, addr)
		}},
		{"net.ListenConfig.Listen", func(ctx context.Context) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(ctx, "tcp4", addr)
		}},
	}
	for _, l := range listeners {
		ln, err := l.listen(t.Context())
		if err != nil {
			t.Fatalf("%s on the held port = %v", l.name, err)
		}
		_ = ln.Close()
	}
	topo.teardown()
	topo.mu.Lock()
	held := topo.held
	topo.mu.Unlock()
	if len(held) == 0 {
		t.Fatal("the topology held no socket for the port it handed out")
	}
	for _, s := range held {
		ln, err := s.Listen()
		if ln != nil {
			_ = ln.Close()
		}
		if !errors.Is(err, net.ErrClosed) {
			t.Errorf("a holding socket after the teardown: Listen = %v, want it closed", err)
		}
	}
}

// bindWithoutReuse binds a listener to addr with SO_REUSEADDR cleared and
// closes it.
func bindWithoutReuse(ctx context.Context, addr string) error {
	lc := net.ListenConfig{Control: func(_, _ string, c syscall.RawConn) error {
		var serr error
		if err := c.Control(func(fd uintptr) {
			serr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_REUSEADDR, 0)
		}); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(ctx, "tcp4", addr)
	if err != nil {
		return err
	}
	return ln.Close()
}
