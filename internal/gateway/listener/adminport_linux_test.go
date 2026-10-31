// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package listener

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// bindAdmin binds an AdminPort on loopback, closed at the end of the test.
func bindAdmin(t *testing.T, port int, steer bool) *AdminPort {
	t.Helper()
	p, err := BindAdminPort(t.Context(), AdminPortOptions{Host: "127.0.0.1", Port: port, Steer: steer})
	if err != nil {
		t.Fatalf("BindAdminPort(%d, steer %t): %v", port, steer, err)
	}
	t.Cleanup(func() { _ = p.Close() })
	return p
}

// serveTag serves ln with a server answering tag, as the admin server
// would serve the AdminPort's listener; it returns Serve's result.
func serveTag(t *testing.T, ln net.Listener, tag string) <-chan error {
	t.Helper()
	srv := &http.Server{
		Handler:           http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, tag) }),
		ReadHeaderTimeout: time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return done
}

// adminTags sends one request on each of connections fresh connections to
// addr and counts the answers by tag ("" for no answer).
func adminTags(t *testing.T, addr string) map[string]int {
	t.Helper()
	tags := make(map[string]int)
	tr := &http.Transport{DisableKeepAlives: true}
	defer tr.CloseIdleConnections()
	c := &http.Client{Transport: tr, Timeout: 10 * time.Second}
	for range connections {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := c.Do(req)
		if err != nil {
			tags[""]++
			continue
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		tags[string(b)]++
	}
	return tags
}

// TestAdminPortHolderProbes checks the admin port's probe bind: a lock
// holder's admin port on a port another process listens on with
// SO_REUSEPORT is RZ-CFG-039, while a successor binds beside the
// listening socket without a probe.
func TestAdminPortHolderProbes(t *testing.T) {
	port := foreignPort(t)
	_, err := BindAdminPort(t.Context(), AdminPortOptions{Host: "127.0.0.1", Port: port, Steer: true})
	if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Fatalf("holder BindAdminPort on a foreign port = %v, want %s", err, codeBind)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("holder BindAdminPort error = %v, want it to wrap EADDRINUSE", err)
	}
	bindAdmin(t, port, false)
}

// TestAdminPortHandover checks the admin port through a handover
// (ADR-0015): the holder's port listens at once and is pinned, so the
// successor's bound and then listening socket receives nothing; steering
// moves every new connection to the successor, the holder's re-pin brings
// them back, and after the holder stops accepting and the successor pins,
// the successor receives them all.
func TestAdminPortHandover(t *testing.T) {
	holder := bindAdmin(t, 0, true)
	hln, err := holder.Listen()
	if err != nil {
		t.Fatalf("holder Listen: %v", err)
	}
	holderDone := serveTag(t, hln, "holder")
	addr := holder.Addr().String()
	port := holder.Addr().(*net.TCPAddr).Port

	successor := bindAdmin(t, port, false)
	if got := successor.Addr().String(); got != addr {
		t.Fatalf("successor address %s, want %s", got, addr)
	}
	if got := adminTags(t, addr); got["holder"] != connections {
		t.Fatalf("successor bound: %v, want every connection on the holder", got)
	}
	sln, err := successor.Listen()
	if err != nil {
		t.Fatalf("successor Listen: %v", err)
	}
	serveTag(t, sln, "successor")
	if got := adminTags(t, addr); got["holder"] != connections {
		t.Fatalf("successor listening, holder pinned: %v, want every connection on the holder", got)
	}
	if err := holder.SteerToSuccessor(); err != nil {
		t.Fatalf("SteerToSuccessor: %v", err)
	}
	if got := adminTags(t, addr); got["successor"] != connections {
		t.Fatalf("steered: %v, want every connection on the successor", got)
	}
	if err := holder.Pin(); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	if got := adminTags(t, addr); got["holder"] != connections {
		t.Fatalf("re-pinned: %v, want every connection on the holder", got)
	}
	if err := holder.SteerToSuccessor(); err != nil {
		t.Fatalf("SteerToSuccessor: %v", err)
	}
	holder.StopAccepting()
	if err := recv(t, "the holder's Serve to return", holderDone); !errors.Is(err, net.ErrClosed) {
		t.Errorf("holder Serve after StopAccepting = %v, want net.ErrClosed", err)
	}
	if err := successor.Pin(); err != nil {
		t.Fatalf("successor Pin: %v", err)
	}
	if got := adminTags(t, addr); got["successor"] != connections {
		t.Fatalf("holder stopped, successor pinned: %v, want every connection on the successor", got)
	}
}

// TestAdminPortPinBeforeListen checks that a socket pinned before it
// listens is pinned once it listens: a socket that joins the group later
// receives nothing, where an unpinned group would hash connections over
// both.
func TestAdminPortPinBeforeListen(t *testing.T) {
	p := bindAdmin(t, 0, false)
	if err := p.Pin(); err != nil {
		t.Fatalf("Pin before Listen: %v", err)
	}
	ln, err := p.Listen()
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	serveTag(t, ln, "pinned")
	later := bindAdmin(t, p.Addr().(*net.TCPAddr).Port, false)
	lln, err := later.Listen()
	if err != nil {
		t.Fatalf("later Listen: %v", err)
	}
	serveTag(t, lln, "later")
	if got := adminTags(t, p.Addr().String()); got["pinned"] != connections {
		t.Fatalf("pinned at Listen: %v, want every connection on the pinned socket", got)
	}
}

// TestAdminPortStop checks that StopAccepting closes a bound socket and a
// listening one, is idempotent, and that Listen then fails with
// ErrStopped.
func TestAdminPortStop(t *testing.T) {
	bound := bindAdmin(t, 0, false)
	addr := bound.Addr().String()
	bound.StopAccepting()
	bound.StopAccepting()
	if _, err := bound.Listen(); !errors.Is(err, ErrStopped) {
		t.Errorf("Listen after StopAccepting = %v, want ErrStopped", err)
	}
	if err := bound.Pin(); err != nil {
		t.Errorf("Pin after StopAccepting = %v, want nil", err)
	}
	if err := bound.SteerToSuccessor(); err != nil {
		t.Errorf("SteerToSuccessor after StopAccepting = %v, want nil", err)
	}
	// The port is free again: a plain listener binds it.
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatalf("listen on the stopped admin port: %v", err)
	}
	_ = ln.Close()

	holder := bindAdmin(t, 0, true)
	if err := holder.WaitLingers(t.Context()); err != nil {
		t.Errorf("WaitLingers = %v, want nil", err)
	}
	if err := holder.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	var d net.Dialer
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if c, err := d.DialContext(ctx, "tcp", "127.0.0.1:"+strconv.Itoa(holder.Addr().(*net.TCPAddr).Port)); err == nil {
		_ = c.Close()
		t.Error("a dial to the closed admin port succeeded")
	}
}
