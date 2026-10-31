// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
)

// FreePort returns a port on 127.0.0.1 that no other socket takes before
// the test ends, as HoldPort does, and fails the test when there is none.
func FreePort(t testing.TB) int {
	t.Helper()
	port, err := HoldPort(t.Context(), t)
	if err != nil {
		t.Fatal(err)
	}
	return port
}

// HoldPort returns a port on 127.0.0.1 that no other socket takes before
// the test ends. A port found by closing a listener is free only for a
// moment: until the test or a process it starts binds it, or binds it
// again after a restart, a connection's ephemeral port or another test's
// bind of port 0 can take it. On Linux a socket bound to the port that
// never listens holds it until the test ends: the kernel gives a bound
// port to no connection and no bind of port 0, while a listener that sets
// SO_REUSEADDR, as Go's, redis-server's and the Node's do, binds it and
// listens all the same. Elsewhere the port is free when HoldPort returns.
func HoldPort(ctx context.Context, t testing.TB) (int, error) {
	s, err := reuseport.Bind(ctx, "127.0.0.1:0")
	if errors.Is(err, errors.ErrUnsupported) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
		if err != nil {
			return 0, fmt.Errorf("proc: free port: %w", err)
		}
		defer func() { _ = ln.Close() }()
		return ln.Addr().(*net.TCPAddr).Port, nil
	}
	if err != nil {
		return 0, fmt.Errorf("proc: free port: %w", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.Addr().(*net.TCPAddr).Port, nil
}
