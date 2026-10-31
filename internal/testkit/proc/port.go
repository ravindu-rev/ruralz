// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"errors"
	"net"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
)

// FreePort returns a port on 127.0.0.1 that no other socket takes before
// the test ends. A port found by closing a listener is free only for a
// moment: until the test or a process it starts binds it, a connection's
// ephemeral port or another test's bind of port 0 can take it. On Linux a
// socket bound to the port that never listens holds it until the test
// ends: the kernel gives a bound port to no connection and no bind of port
// 0, while a listener that sets SO_REUSEADDR, as Go's, redis-server's and
// the Node's do, binds it and listens all the same. Elsewhere the port is
// free when FreePort returns.
func FreePort(t testing.TB) int {
	t.Helper()
	s, err := reuseport.Bind(t.Context(), "127.0.0.1:0")
	if errors.Is(err, errors.ErrUnsupported) {
		ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("proc: free port: %v", err)
		}
		defer func() { _ = ln.Close() }()
		return ln.Addr().(*net.TCPAddr).Port
	}
	if err != nil {
		t.Fatalf("proc: free port: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.Addr().(*net.TCPAddr).Port
}
