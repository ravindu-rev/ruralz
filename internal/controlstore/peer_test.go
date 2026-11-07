// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"crypto/x509"
	"net/url"
	"strings"
	"testing"
	"time"
)

// TestServerIDValidate checks the character set and length.
func TestServerIDValidate(t *testing.T) {
	for _, ok := range []ServerID{"r1", "01J9ZK3M7Q8R2T4V6X8Y0A1B2C", "control-b1.zone_a", ServerID(strings.Repeat("a", 64))} {
		if err := ok.Validate(); err != nil {
			t.Errorf("Validate(%q) = %v", ok, err)
		}
	}
	for _, bad := range []ServerID{"", "a b", "a/b", "é", ServerID(strings.Repeat("a", 65))} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate(%q) accepted", bad)
		}
	}
}

// certWith returns a certificate carrying the URI SANs.
func certWith(t *testing.T, uris ...string) *x509.Certificate {
	t.Helper()
	c := &x509.Certificate{}
	for _, s := range uris {
		u, err := url.Parse(s)
		if err != nil {
			t.Fatal(err)
		}
		c.URIs = append(c.URIs, u)
	}
	return c
}

// TestPeerIdentity reads the role and server ID a peer certificate names;
// a Node certificate, which names none, is no peer.
func TestPeerIdentity(t *testing.T) {
	u, err := PeerURI(RoleReplica, "r1")
	if err != nil {
		t.Fatal(err)
	}
	if u.String() != "urn:ruralz:replica:r1" {
		t.Errorf("PeerURI = %s", u)
	}
	got, err := PeerIdentityOf(certWith(t, "spiffe://acme/x", u.String()))
	if err != nil || got != (PeerIdentity{Role: RoleReplica, ServerID: "r1"}) {
		t.Errorf("PeerIdentityOf = %+v, %v", got, err)
	}
	relay, _ := PeerURI(RoleRelay, "eu-relay")
	if got, err := PeerIdentityOf(certWith(t, relay.String())); err != nil || got.Role != RoleRelay {
		t.Errorf("relay identity = %+v, %v", got, err)
	}
	for name, uris := range map[string][]string{
		"Node certificate": {"spiffe://ruralz/node/01J"},
		"none":             nil,
		"two identities":   {"urn:ruralz:replica:a", "urn:ruralz:replica:b"},
		"unknown role":     {"urn:ruralz:admin:a"},
		"no server ID":     {"urn:ruralz:replica"},
		"bad server ID":    {"urn:ruralz:replica:a%20b"},
	} {
		if _, err := PeerIdentityOf(certWith(t, uris...)); err == nil {
			t.Errorf("%s: PeerIdentityOf accepted", name)
		}
	}
	if _, err := PeerURI("node", "x"); err == nil {
		t.Error("PeerURI accepted the role node")
	}
}

// TestClassAdmits: Raft, forwarding, status and content need a replica
// certificate; Join takes none; no M2 class admits a relay.
func TestClassAdmits(t *testing.T) {
	for _, c := range Classes() {
		if c.Admits(RoleRelay) {
			t.Errorf("%s admits a relay", c)
		}
		if !c.Admits(RoleReplica) {
			t.Errorf("%s refuses a replica", c)
		}
		if got := c.Admits(""); got != (c == ClassJoin) {
			t.Errorf("%s admits no certificate = %v", c, got)
		}
	}
	if Class("other").Admits(RoleReplica) {
		t.Error("an unknown class admits a replica")
	}
}

// TestFenceOrder orders by epoch, then sequence.
func TestFenceOrder(t *testing.T) {
	a, b, same := Fence{Epoch: 2, Seq: 1}, Fence{Epoch: 1, Seq: 9}, Fence{Epoch: 2, Seq: 1}
	if a.Compare(b) != 1 || b.Compare(a) != -1 || a.Compare(same) != 0 {
		t.Error("a later epoch must order above any sequence of an earlier one")
	}
	if a.Next() != (Fence{Epoch: 2, Seq: 2}) {
		t.Errorf("Next = %+v", a.Next())
	}
}

// TestFreshness is the fencing state machine of Figure 2 of Scalability:
// fresh within one election timeout or while leading, stale after it,
// rebalancing 30 s later.
func TestFreshness(t *testing.T) {
	et := time.Second
	cases := []struct {
		leader bool
		age    time.Duration
		want   Freshness
	}{
		{true, time.Hour, Fresh},
		{false, 0, Fresh},
		{false, et - 1, Fresh},
		{false, et, Stale},
		{false, et + RebalanceAfter - 1, Stale},
		{false, et + RebalanceAfter, Rebalancing},
	}
	for _, tc := range cases {
		if got := FreshnessOf(tc.leader, tc.age, et); got != tc.want {
			t.Errorf("FreshnessOf(%v, %v) = %v, want %v", tc.leader, tc.age, got, tc.want)
		}
	}
}

// TestMayServeAssignment is the stale replica's serving rule.
func TestMayServeAssignment(t *testing.T) {
	cases := []struct {
		f                     Freshness
		latest, active, serve bool
	}{
		{Fresh, false, true, true},
		{Fresh, true, false, true},
		{Stale, true, false, true},
		{Stale, true, true, false},
		{Stale, false, false, false},
		{Rebalancing, true, false, true},
		{Rebalancing, true, true, false},
		{0, true, false, false},
	}
	for _, tc := range cases {
		if got := MayServeAssignment(tc.f, tc.latest, tc.active); got != tc.serve {
			t.Errorf("MayServeAssignment(%v, latest %v, active %v) = %v, want %v", tc.f, tc.latest, tc.active, got, tc.serve)
		}
	}
}
