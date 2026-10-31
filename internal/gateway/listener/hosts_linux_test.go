// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package listener

import (
	"errors"
	"net"
	"net/netip"
	"strconv"
	"syscall"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
)

// twoHosts are two loopback addresses Linux binds without configuration,
// standing in for 127.0.0.1 and ::1 of RURALZ_BIND_LOOPBACK on hosts
// without IPv6: every address of 127.0.0.0/8 reaches the loopback device.
var twoHosts = []string{"127.0.0.1", "127.0.0.2"}

// hostsOption binds the Set's listeners to twoHosts.
func hostsOption(o *Options) { o.Hosts = twoHosts }

// addrOn returns host:port.
func addrOn(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

// TestListenerBindsEveryHost checks the bind of
// docs/architecture/03-data-plane.md "Listeners and protocols" with
// several hosts: a listener binds one socket per host, all on one port, in
// host order; each socket serves the listener through its shared Server
// shards; StopAccepting closes every socket, while a connection either
// socket accepted keeps being served.
func TestListenerBindsEveryHost(t *testing.T) {
	h := newHarness(t, hostsOption)
	h.activate(t, snap(httpListener("web", 0)), nil)
	addrs := h.set.Addrs("web")
	if len(addrs) != len(twoHosts) {
		t.Fatalf("Addrs = %v, want one per host of %v", addrs, twoHosts)
	}
	port := portOfAddr(addrs[0])
	for i, a := range addrs {
		ta := a.(*net.TCPAddr)
		if ta.IP.String() != twoHosts[i] || ta.Port != port {
			t.Errorf("Addrs[%d] = %v, want %s on port %d", i, a, twoHosts[i], port)
		}
	}
	if got := h.set.Addr("web").String(); got != addrs[0].String() {
		t.Errorf("Addr = %s, want the first host's %s", got, addrs[0])
	}
	var kept []*rawConn
	for _, host := range twoHosts {
		c := dialRaw(t, addrOn(host, port), nil)
		r := c.mustRoundTrip(t)
		if field(r.body, 1) != "web" {
			t.Errorf("via %s: %q, want listener web", host, r.body)
		}
		kept = append(kept, c)
	}
	if !h.set.Bound() {
		t.Fatal("Bound = false with two listening sockets")
	}
	h.set.StopAccepting()
	for _, host := range twoHosts {
		if !refused(t, addrOn(host, port)) {
			t.Errorf("%s still accepts after StopAccepting", host)
		}
	}
	for i, c := range kept {
		if r := c.mustRoundTrip(t); field(r.body, 1) != "web" {
			t.Errorf("connection %d after StopAccepting: %q", i, r.body)
		}
	}
	if h.set.Bound() {
		t.Error("Bound = true after StopAccepting")
	}
}

// TestHostBindFailureReleasesEveryHost checks that a port one host cannot
// bind rejects the Revision with RZ-CFG-039 and leaves no socket of the
// listener bound on the other hosts.
func TestHostBindFailureReleasesEveryHost(t *testing.T) {
	var lc net.ListenConfig
	// A plain socket on the second host only: the first binds, the second
	// fails.
	taken, err := lc.Listen(t.Context(), "tcp4", addrOn(twoHosts[1], 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	port := taken.Addr().(*net.TCPAddr).Port
	h := newHarness(t, hostsOption)
	_, err = h.set.Prepare(t.Context(), snap(httpListener("web", port)), nil)
	if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Fatalf("Prepare = %v, want %s", err, codeBind)
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Errorf("Prepare error = %v, want it to wrap EADDRINUSE", err)
	}
	// The first host's socket was released: a plain socket binds it.
	free, err := lc.Listen(t.Context(), "tcp4", addrOn(twoHosts[0], port))
	if err != nil {
		t.Fatalf("the first host's port is still held: %v", err)
	}
	_ = free.Close()
}

// TestHolderProbesEveryHost checks the lock holder's probe bind on every
// host: a port another process listens on with SO_REUSEPORT on the second
// host only, which a plain bind there would join, refuses the Revision.
func TestHolderProbesEveryHost(t *testing.T) {
	foreign, err := reuseport.Listen(t.Context(), addrOn(twoHosts[1], 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = foreign.Close() })
	port := foreign.Addr().(*net.TCPAddr).Port
	h := newHarness(t, hostsOption, func(o *Options) { o.Steer = true })
	_, err = h.set.Prepare(t.Context(), snap(httpListener("web", port)), nil)
	if code, _ := errcode.CodeOf(err); code != codeBind || !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("holder Prepare on a port foreign on one host = %v, want %s wrapping EADDRINUSE", err, codeBind)
	}
}

// TestHandoverSteersEveryHostGroup checks the handover steering of
// ADR-0015 with several hosts: each host's reuseport group is pinned to
// the holder, steered to the successor by SteerToSuccessor and pinned
// back by Pin, so every new connection on every address follows.
func TestHandoverSteersEveryHostGroup(t *testing.T) {
	holder := newHarness(t, hostsOption, func(o *Options) { o.Steer = true; o.Handler = newTestHandler("holder") })
	holder.activate(t, snap(httpListener("web", 0)), nil)
	port := portOfAddr(holder.set.Addr("web"))
	successor := newHarness(t, hostsOption, func(o *Options) { o.Handler = newTestHandler("successor") })
	successor.activate(t, snap(httpListener("web", port)), nil)
	check := func(stage, want string) {
		t.Helper()
		for _, host := range twoHosts {
			if got := tagsOf(t, addrOn(host, port), nil); got[want] != connections {
				t.Fatalf("%s, via %s: %v, want every connection on the %s", stage, host, got, want)
			}
		}
	}
	check("successor listening, holder pinned", "holder")
	if err := holder.set.SteerToSuccessor(); err != nil {
		t.Fatalf("SteerToSuccessor: %v", err)
	}
	check("steered", "successor")
	if err := holder.set.Pin(); err != nil {
		t.Fatalf("re-pin: %v", err)
	}
	check("re-pinned", "holder")
}

// TestReplacementLingersOnEveryHost checks a Hot Reload replacing a
// listener on its port with several hosts: right after Commit every new
// connection on every address reaches the new sockets; after the Linger
// the old sockets are closed and each host's group holds the new socket
// alone.
func TestReplacementLingersOnEveryHost(t *testing.T) {
	h := newHarness(t, hostsOption, func(o *Options) { o.Steer = true })
	h.activate(t, snap(httpListener("web", 0)), nil)
	old := h.endpoint(t, "web")
	port := portOfAddr(h.set.Addr("web"))
	next := httpListener("web", port)
	next.ProxyProtocol = true
	h.activate(t, snap(next), nil)
	hdr := proxyV2(netip.MustParseAddrPort("192.0.2.30:1000"), netip.MustParseAddrPort("198.51.100.1:80"))
	for _, host := range twoHosts {
		if got := tagsOf(t, addrOn(host, port), hdr); got["set"] != connections {
			t.Fatalf("during the Linger, via %s: %v, want every connection on the new socket", host, got)
		}
	}
	h.clk.Advance(Linger)
	recv(t, "the replaced sockets to close", old.stopped)
	h.set.mu.Lock()
	defer h.set.mu.Unlock()
	for _, host := range twoHosts {
		if g := h.set.groups[groupKey{host: host, port: port}]; g == nil || g.Len() != 1 {
			t.Errorf("group of %s after the Linger = %v, want the new socket alone", host, g)
		}
	}
}

// TestBindAdminPortsEveryHost checks BindAdminPorts: one AdminPort per
// host, all on one port, each listening and pinned in the lock holder; a
// port taken on one host binds none; no host binds every interface.
func TestBindAdminPortsEveryHost(t *testing.T) {
	ports, err := BindAdminPorts(t.Context(), twoHosts, AdminPortOptions{Steer: true})
	if err != nil {
		t.Fatalf("BindAdminPorts: %v", err)
	}
	if len(ports) != len(twoHosts) {
		t.Fatalf("BindAdminPorts = %d ports, want %d", len(ports), len(twoHosts))
	}
	port := portOfAddr(ports[0].Addr())
	for i, p := range ports {
		t.Cleanup(p.StopAccepting)
		ta := p.Addr().(*net.TCPAddr)
		if ta.IP.String() != twoHosts[i] || ta.Port != port {
			t.Errorf("admin port %d bound %v, want %s on port %d", i, ta, twoHosts[i], port)
		}
		if _, err := p.Listen(); err != nil {
			t.Errorf("Listen %v: %v", ta, err)
		}
		if refused(t, ta.String()) {
			t.Errorf("admin socket %v refuses connections", ta)
		}
	}

	var lc net.ListenConfig
	taken, err := lc.Listen(t.Context(), "tcp4", addrOn(twoHosts[1], 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taken.Close() })
	busy := taken.Addr().(*net.TCPAddr).Port
	if _, err := BindAdminPorts(t.Context(), twoHosts, AdminPortOptions{Port: busy, Steer: true}); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("BindAdminPorts on a port taken on one host = %v, want EADDRINUSE", err)
	} else if code, _ := errcode.CodeOf(err); code != codeBind {
		t.Errorf("BindAdminPorts error code = %q, want %s", code, codeBind)
	}
	free, err := lc.Listen(t.Context(), "tcp4", addrOn(twoHosts[0], busy))
	if err != nil {
		t.Fatalf("the first host's admin socket was kept: %v", err)
	}
	_ = free.Close()

	all, err := BindAdminPorts(t.Context(), nil, AdminPortOptions{Steer: true})
	if err != nil {
		t.Fatalf("BindAdminPorts on every interface: %v", err)
	}
	t.Cleanup(all[0].StopAccepting)
	if len(all) != 1 || !all[0].Addr().(*net.TCPAddr).IP.IsUnspecified() {
		t.Errorf("BindAdminPorts without hosts = %v, want one socket on every interface", all)
	}
}
