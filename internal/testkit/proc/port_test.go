// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"net"
	"strconv"
	"testing"
)

// FreePort's port takes a listener, as a process the test starts binds it.
func TestFreePortTakesAListener(t *testing.T) {
	port := FreePort(t)
	if port <= 0 {
		t.Fatalf("FreePort = %d", port)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("a listener on the free port: %v", err)
	}
	_ = ln.Close()
}
