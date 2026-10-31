// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The admin port refuses a bearer token from a non-loopback peer without
// TLS (docs/architecture/08-security-and-identity.md "Admin ports";
// docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests"): on Linux a network namespace gives the client a non-loopback
// address on a dummy device, or a veth pair where the kernel has no dummy
// driver. The case skips with a reason without the privilege to create
// the namespace or the device; stage 8 grants it.

func TestAdminPortCleartextFromNonLoopbackPeer(t *testing.T) {
	if !proc.NetNSChild(t) {
		// Root creates the network namespace alone; anyone else needs a
		// user namespace mapping them to root inside it.
		_, err := proc.RunInNetNS(t.Context(), proc.NetNSOptions{
			TestName: t.Name(),
			User:     os.Geteuid() != 0,
			Env:      []string{binaries.EnvBinDir + "=" + binDir()},
			Args:     []string{"-test.timeout=5m"},
		})
		switch {
		case errors.Is(err, proc.ErrChildSkipped):
			t.Skip(err)
		case errors.Is(err, syscall.EPERM), errors.Is(err, syscall.EACCES), errors.Is(err, syscall.EINVAL):
			t.Skipf("creating a network namespace needs CAP_SYS_ADMIN or unprivileged user namespaces, which stage 8 grants: %v", err)
		case err != nil:
			t.Fatal(err)
		}
		return
	}
	// The non-loopback address the namespace's device carries.
	peerPrefix := netip.MustParsePrefix("10.199.0.1/24")
	if err := proc.AddDevice("rz0", peerPrefix); err != nil {
		t.Skipf("no device for a non-loopback address in the network namespace: %v", err)
	}
	peer := peerPrefix.Addr().String()
	// node starts a Node whose admin port binds every interface, over TLS
	// unless cleartext.
	node := func(t *testing.T, cleartext bool) (*topology.Topology, *topology.Node) {
		t.Helper()
		top := newTopology(t, topology.Config{
			Transport: topology.Transport{CleartextAdmin: cleartext},
			Env:       []string{"RURALZ_BIND_LOOPBACK=false"},
		})
		mock := startMock(t, top, mockup.Config{})
		nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
			Bundle: bundle(gatewayDoc(webListener), upstreamDoc("backend", mock.Addr(), ""), routeDoc("r", "/", "backend")),
		})
		if err != nil {
			t.Fatal(err)
		}
		return top, nodes[0]
	}
	// fromPeer sends GET path to the Node's admin port from the
	// non-loopback address, presenting token unless it is "".
	fromPeer := func(t *testing.T, top *topology.Topology, n *topology.Node, scheme, path, token string) reply {
		t.Helper()
		d := &net.Dialer{LocalAddr: &net.TCPAddr{IP: net.ParseIP(peer)}, Timeout: 5 * time.Second}
		// The admin certificate names loopback hosts only.
		tr := &http.Transport{
			DialContext:       d.DialContext,
			TLSClientConfig:   &tls.Config{RootCAs: top.CA().Pool(), ServerName: "localhost", MinVersion: tls.VersionTLS12},
			DisableKeepAlives: true,
		}
		defer tr.CloseIdleConnections()
		url := scheme + "://" + net.JoinHostPort(peer, strconv.Itoa(n.AdminPort())) + path
		if token == "" {
			return get(t, capturing(top, tr), url)
		}
		return get(t, capturing(top, tr), url, bearer(token)...)
	}

	t.Run("cleartext", func(t *testing.T) {
		top, n := node(t, true)
		if r := fromPeer(t, top, n, "http", "/healthz", ""); r.status != http.StatusOK {
			t.Fatalf("/healthz from %s = %v, want 200: open paths need no credential", peer, r)
		}
		wantAdminRefused(t, "operator token over cleartext from "+peer, fromPeer(t, top, n, "http", "/config/dump", n.OperatorToken()), "RZ-AUTH-002")
		wantAdminRefused(t, "metrics token over cleartext from "+peer, fromPeer(t, top, n, "http", "/metrics", n.MetricsToken()), "RZ-AUTH-002")
		// The same tokens from a loopback peer over cleartext pass.
		if resp, err := n.Admin(t.Context(), "/config/dump"); err != nil || resp.Status != http.StatusOK {
			t.Fatalf("operator token over cleartext from loopback = %v %v, want 200", resp.Status, err)
		}
		// Admin without TLS is a reported cleartext hop.
		if got := freshCounter(t, top, n, "ruralz_security_cleartext_hops", "hop", "admin"); got != 1 {
			t.Fatalf("ruralz_security_cleartext_hops{hop=admin} = %v, want 1", got)
		}
	})
	t.Run("TLS", func(t *testing.T) {
		top, n := node(t, false)
		if r := fromPeer(t, top, n, "https", "/config/dump", n.OperatorToken()); r.status != http.StatusOK {
			t.Fatalf("operator token over TLS from %s = %v, want 200", peer, r)
		}
		wantAdminRefused(t, "no credential over TLS from "+peer, fromPeer(t, top, n, "https", "/config/dump", ""), "RZ-AUTH-001")
		if got := freshCounter(t, top, n, "ruralz_security_cleartext_hops", "hop", "admin"); got != 0 {
			t.Fatalf("ruralz_security_cleartext_hops{hop=admin} = %v, want 0 with admin TLS", got)
		}
	})
}
