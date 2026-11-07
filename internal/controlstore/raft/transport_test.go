// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
)

// transportFixture is one 8092 transport with recording handlers.
type transportFixture struct {
	t     *testing.T
	pki   *testPKI
	tr    *transport
	layer *streamLayer
	addr  string

	mu      sync.Mutex
	revoked map[controlstore.ServerID]bool
	calls   chan *PeerConn
}

// newTransportFixture starts a transport for server ID "server".
func newTransportFixture(t *testing.T) *transportFixture { return newTransportFixtureOf(t, false) }

// newTransportFixtureOf starts the transport unless unstarted.
func newTransportFixtureOf(t *testing.T, unstarted bool) *transportFixture {
	t.Helper()
	f := &transportFixture{t: t, pki: newPKI(t), revoked: map[controlstore.ServerID]bool{}, calls: make(chan *PeerConn, 8)}
	ln := listen(t, "")
	f.addr = ln.Addr().String()
	h := func(_ context.Context, pc *PeerConn) { f.calls <- pc }
	authz := func(p controlstore.PeerIdentity) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.revoked[p.ServerID] {
			return errors.New("revoked")
		}
		return nil
	}
	f.tr = newTransport(ln, f.addr, f.pki.source(t, "server"), clocktest.New(testStart), clock.Real(), slog.New(slog.DiscardHandler), authz,
		map[controlstore.Class]Handler{controlstore.ClassJoin: h, controlstore.ClassForward: h, controlstore.ClassStatus: h, controlstore.ClassContent: h})
	f.layer = newStreamLayer(f.tr)
	if !unstarted {
		f.tr.start()
	}
	t.Cleanup(f.tr.close)
	return f
}

// client dials with cert (nil for none), offering protos, as TLS 1.3 or
// tls12, and returns the connection after the handshake, or the error.
func (f *transportFixture) client(cert *tls.Certificate, tls12 bool, protos ...string) (*tls.Conn, error) {
	cfg := &tls.Config{
		MinVersion: tls.VersionTLS12, MaxVersion: tls.VersionTLS13, RootCAs: f.pki.peer.Pool(), ServerName: "127.0.0.1",
		NextProtos: protos, Time: func() time.Time { return testStart },
	}
	if tls12 {
		cfg.MaxVersion = tls.VersionTLS12
	}
	if cert != nil {
		cfg.Certificates = []tls.Certificate{*cert}
	}
	d := &tls.Dialer{Config: cfg}
	ctx, cancel := context.WithTimeout(context.Background(), waitBound)
	defer cancel()
	c, err := d.DialContext(ctx, "tcp", f.addr)
	if err != nil {
		return nil, err
	}
	f.t.Cleanup(func() { _ = c.Close() })
	return c.(*tls.Conn), nil
}

// refused reports whether the server ends the connection: the handshake
// fails, or the first read gets the alert or EOF of a refused connection.
func refused(c *tls.Conn, err error) bool {
	if err != nil {
		return true
	}
	_ = c.SetReadDeadline(time.Now().Add(waitBound))
	_, rerr := c.Read(make([]byte, 1))
	return rerr != nil && !errors.Is(rerr, os.ErrDeadlineExceeded)
}

// accepted waits, under the bound, for a Raft-class connection on the
// stream layer.
func (f *transportFixture) accepted() bool {
	select {
	case c := <-f.tr.raftCh:
		_ = c.Close()
		return true
	case <-time.After(waitBound):
		return false
	}
}

// pendingRaft reports whether a Raft-class connection waits for Accept.
// A refused connection's handler returned before it could, so after
// refused reported the close this is exact.
func (f *transportFixture) pendingRaft() bool {
	select {
	case c := <-f.tr.raftCh:
		_ = c.Close()
		return true
	default:
		return false
	}
}

// TestPeerLayerAdmitsReplicasOnRaft: a replica certificate reaches raft's
// Accept on the Raft class.
func TestPeerLayerAdmitsReplicasOnRaft(t *testing.T) {
	f := newTransportFixture(t)
	cert := f.pki.replicaCert(t, "r2")
	if _, err := f.client(&cert, false, string(controlstore.ClassRaft)); err != nil {
		t.Fatal(err)
	}
	if !f.accepted() {
		t.Error("a replica's Raft connection never reached Accept")
	}
}

// TestPeerLayerRefusesNodeCertificates: 8092 rejects a Node CA
// certificate, even one naming a replica (TB-11), on every class.
func TestPeerLayerRefusesNodeCertificates(t *testing.T) {
	f := newTransportFixture(t)
	u, _ := controlstore.PeerURI(controlstore.RoleReplica, "rogue")
	rogue := leaf(t, f.pki.node, u.String())
	node := leaf(t, f.pki.node, "spiffe://ruralz/node/01J")
	for _, cert := range []tls.Certificate{rogue, node} {
		for _, class := range controlstore.Classes() {
			if !refused(f.client(&cert, false, string(class))) {
				t.Errorf("class %s admitted a Node CA certificate", class)
			}
		}
	}
	if f.pendingRaft() {
		t.Error("a Node certificate reached raft")
	}
}

// TestPeerLayerJoinOnlyWithoutCertificate: only Join takes a connection
// without a client certificate.
func TestPeerLayerJoinOnlyWithoutCertificate(t *testing.T) {
	f := newTransportFixture(t)
	for _, class := range controlstore.Classes() {
		c, err := f.client(nil, false, string(class))
		if class == controlstore.ClassJoin {
			if err != nil {
				t.Fatalf("Join without a certificate: %v", err)
			}
			pc := <-f.calls
			if pc.Class != controlstore.ClassJoin || pc.Peer != (controlstore.PeerIdentity{}) || pc.Certificate != nil {
				t.Errorf("Join handler got %+v", pc)
			}
			continue
		}
		if !refused(c, err) {
			t.Errorf("class %s admitted a connection without a certificate", class)
		}
	}
	cert := f.pki.replicaCert(t, "r2")
	if _, err := f.client(&cert, false, string(controlstore.ClassJoin)); err != nil {
		t.Fatal(err)
	}
	if pc := <-f.calls; pc.Peer.ServerID != "r2" {
		t.Errorf("Join with a replica certificate got %+v", pc.Peer)
	}
}

// TestPeerLayerRoleRules: a relay certificate opens no M2 class; a peer CA
// certificate without an identity opens none; a revoked server ID opens
// none.
func TestPeerLayerRoleRules(t *testing.T) {
	f := newTransportFixture(t)
	ru, _ := controlstore.PeerURI(controlstore.RoleRelay, "relay-eu")
	relay := leaf(t, f.pki.peer, ru.String())
	anonymous := leaf(t, f.pki.peer)
	for _, class := range controlstore.Classes() {
		if !refused(f.client(&relay, false, string(class))) {
			t.Errorf("class %s admitted a relay", class)
		}
		if !refused(f.client(&anonymous, false, string(class))) {
			t.Errorf("class %s admitted a certificate without a peer identity", class)
		}
	}
	f.mu.Lock()
	f.revoked["old"] = true
	f.mu.Unlock()
	old := f.pki.replicaCert(t, "old")
	for _, class := range []controlstore.Class{controlstore.ClassRaft, controlstore.ClassStatus, controlstore.ClassContent, controlstore.ClassJoin} {
		if !refused(f.client(&old, false, string(class))) {
			t.Errorf("class %s admitted a revoked server ID", class)
		}
	}
	if f.pendingRaft() {
		t.Error("a refused connection reached raft")
	}
	select {
	case pc := <-f.calls:
		t.Errorf("a refused connection reached a handler: %+v", pc.Peer)
	default:
	}
}

// TestPeerLayerOneClassTLS13: a connection names exactly one known class,
// over TLS 1.3 only.
func TestPeerLayerOneClassTLS13(t *testing.T) {
	f := newTransportFixture(t)
	cert := f.pki.replicaCert(t, "r2")
	cases := map[string][]string{
		"no class":      nil,
		"two classes":   {string(controlstore.ClassRaft), string(controlstore.ClassContent)},
		"unknown class": {"h2"},
	}
	for name, protos := range cases {
		if !refused(f.client(&cert, false, protos...)) {
			t.Errorf("%s was admitted", name)
		}
	}
	if !refused(f.client(&cert, true, string(controlstore.ClassRaft))) {
		t.Error("TLS 1.2 was admitted")
	}
}

// fakePeer is a TLS server presenting cert for one class.
func fakePeer(t *testing.T, cert tls.Certificate) string {
	t.Helper()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, NextProtos: []string{string(controlstore.ClassStatus)},
		ClientAuth: tls.RequireAnyClientCert,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	ctx := t.Context()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_ = c.(*tls.Conn).HandshakeContext(ctx)
				_, _ = io.Copy(io.Discard, c)
			}()
		}
	}()
	return ln.Addr().String()
}

// TestDialVerifiesPeer: a dialed peer must chain to the peer CA, carry a
// replica identity and not be revoked.
func TestDialVerifiesPeer(t *testing.T) {
	f := newTransportFixture(t)
	ru, _ := controlstore.PeerURI(controlstore.RoleRelay, "relay")
	cases := map[string]tls.Certificate{
		"Node CA":   leaf(t, f.pki.node, "urn:ruralz:replica:x"),
		"relay":     leaf(t, f.pki.peer, ru.String()),
		"anonymous": leaf(t, f.pki.peer),
		"revoked":   f.pki.replicaCert(t, "old"),
	}
	f.mu.Lock()
	f.revoked["old"] = true
	f.mu.Unlock()
	for name, cert := range cases {
		addr := fakePeer(t, cert)
		if tc, _, err := f.tr.dial(waitCtx(t), addr, controlstore.ClassStatus); err == nil {
			f.tr.release(tc)
			t.Errorf("%s: dial succeeded", name)
		}
	}
	addr := fakePeer(t, f.pki.replicaCert(t, "good"))
	tc, id, err := f.tr.dial(waitCtx(t), addr, controlstore.ClassStatus)
	if err != nil {
		t.Fatal(err)
	}
	f.tr.release(tc)
	if id != (controlstore.PeerIdentity{Role: controlstore.RoleReplica, ServerID: "good"}) {
		t.Errorf("identity = %+v", id)
	}
}

// waitCtx bounds a test step.
func waitCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	t.Cleanup(cancel)
	return ctx
}

// TestPeerLayerAcceptsOnlyOnceStarted is the regression test of a status
// report reaching its handler while the replica was still opening, before
// raft existed: until start, a connecting peer is never accepted, so no
// handler runs.
func TestPeerLayerAcceptsOnlyOnceStarted(t *testing.T) {
	f := newTransportFixtureOf(t, true)
	tcp, err := (&net.Dialer{}).DialContext(waitCtx(t), "tcp", f.addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tcp.Close() })
	f.tr.mu.Lock()
	n := len(f.tr.conns)
	f.tr.mu.Unlock()
	if n != 0 {
		t.Fatalf("an unstarted transport accepted %d connections", n)
	}
	f.tr.start()
	waitFor(t, "the started transport to accept", func() bool {
		f.tr.mu.Lock()
		defer f.tr.mu.Unlock()
		return len(f.tr.conns) == 1
	})
}
