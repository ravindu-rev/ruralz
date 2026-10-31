// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// TestFreeLoopbackPort: the port binds on 127.0.0.1, and a taken port is
// never returned.
func TestFreeLoopbackPort(t *testing.T) {
	first, err := freeLoopbackPort(t.Context(), func(int) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(first)))
	if err != nil {
		t.Fatalf("port %d does not bind: %v", first, err)
	}
	_ = ln.Close()
	for range 20 {
		p, err := freeLoopbackPort(t.Context(), func(p int) bool { return p == first })
		if err != nil || p == first {
			t.Fatalf("freeLoopbackPort = %d, %v; %d is taken", p, err, first)
		}
	}
	if _, err := freeLoopbackPort(t.Context(), func(int) bool { return true }); command.Code(t.Context(), err) != command.ExitNoResult {
		t.Errorf("freeLoopbackPort with every port taken = %v", err)
	}
}

// TestCheckPortsFree: free ports pass; a busy listener port and a busy
// admin port exit 2 naming them.
func TestCheckPortsFree(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	busyPort := busy.Addr().(*net.TCPAddr).Port
	free, err := freeLoopbackPort(t.Context(), func(p int) bool { return p == busyPort })
	if err != nil {
		t.Fatal(err)
	}
	render := func(listener, admin int) *Render {
		return &Render{
			Listeners: []Listener{{Port: Port{Name: "public", Port: listener, TestPort: listener}, Protocol: "http"}},
			Admin:     Port{Name: "admin", Port: admin, TestPort: admin},
		}
	}
	if err := CheckPortsFree(t.Context(), render(free, free)); err != nil {
		t.Errorf("free ports: %v", err)
	}
	err = CheckPortsFree(t.Context(), render(busyPort, free))
	if command.Code(t.Context(), err) != command.ExitNoResult || !strings.HasPrefix(err.Error(), "listener public: port "+strconv.Itoa(busyPort)+" is busy: ") ||
		!strings.HasSuffix(err.Error(), "; free it or use --ephemeral-ports") {
		t.Errorf("busy listener port: %v", err)
	}
	err = CheckPortsFree(t.Context(), render(free, busyPort))
	if command.Code(t.Context(), err) != command.ExitNoResult || !strings.HasPrefix(err.Error(), "the admin port: port "+strconv.Itoa(busyPort)+" is busy: ") {
		t.Errorf("busy admin port: %v", err)
	}
}
