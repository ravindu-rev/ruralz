// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"crypto/x509"
	"fmt"
	"net/url"
	"strings"
)

// ServerID names one replica in Raft membership: 1 to 64 characters from
// [0-9A-Za-z._-]. A restore gives the restored replica a new one.
type ServerID string

// maxServerID bounds a server ID.
const maxServerID = 64

// Validate checks the character set and length.
func (id ServerID) Validate() error {
	if id == "" || len(id) > maxServerID {
		return fmt.Errorf("%w: server ID %q is not 1 to %d characters", ErrInvalid, id, maxServerID)
	}
	for i := range len(id) {
		if !serverIDByte(id[i]) {
			return fmt.Errorf("%w: server ID %q has a character outside [0-9A-Za-z._-]", ErrInvalid, id)
		}
	}
	return nil
}

// serverIDByte reports whether c is in [0-9A-Za-z._-].
func serverIDByte(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '.' || c == '_' || c == '-'
}

// Role is the role a peer certificate names: 8092 authorizes each
// connection class by it (docs/architecture/04-control-plane-and-gitops.md
// "Replica roles").
type Role string

// Peer certificate roles.
const (
	// RoleReplica is a Ruralz Control replica, voter or non-voter.
	RoleReplica Role = "replica"
	// RoleRelay is a read-only regional relay, Planned (M4): no M2 class
	// admits it.
	RoleRelay Role = "relay"
)

// peerURNPrefix starts the URI SAN that names a peer certificate's role
// and server ID: urn:ruralz:<role>:<server ID>. The leader writes it when
// it signs a replica's certificate at Join; the peer CA issues nothing
// else, and Node certificates, from the disjoint Node CA, carry none.
const peerURNPrefix = "urn:ruralz:"

// PeerURI returns the URI SAN of a peer certificate for role and id.
func PeerURI(role Role, id ServerID) (*url.URL, error) {
	if role != RoleReplica && role != RoleRelay {
		return nil, fmt.Errorf("%w: peer role %q", ErrInvalid, role)
	}
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return url.Parse(peerURNPrefix + string(role) + ":" + string(id))
}

// PeerIdentity is the role and server ID a verified peer certificate
// names.
type PeerIdentity struct {
	Role     Role
	ServerID ServerID
}

// PeerIdentityOf reads the identity of a verified peer certificate: its
// one urn:ruralz URI SAN. A certificate with none, with several or with a
// malformed one names no peer.
func PeerIdentityOf(cert *x509.Certificate) (PeerIdentity, error) {
	var found []string
	for _, u := range cert.URIs {
		if s := u.String(); strings.HasPrefix(s, peerURNPrefix) {
			found = append(found, strings.TrimPrefix(s, peerURNPrefix))
		}
	}
	if len(found) != 1 {
		return PeerIdentity{}, fmt.Errorf("%w: a peer certificate names one urn:ruralz identity, this one %d", ErrInvalid, len(found))
	}
	role, id, ok := strings.Cut(found[0], ":")
	if !ok {
		return PeerIdentity{}, fmt.Errorf("%w: peer identity %q is not <role>:<server ID>", ErrInvalid, found[0])
	}
	p := PeerIdentity{Role: Role(role), ServerID: ServerID(id)}
	if p.Role != RoleReplica && p.Role != RoleRelay {
		return PeerIdentity{}, fmt.Errorf("%w: peer role %q", ErrInvalid, role)
	}
	if err := p.ServerID.Validate(); err != nil {
		return PeerIdentity{}, err
	}
	return p, nil
}

// Class is one 8092 connection class. Each connection names exactly one,
// proposed by ALPN (ADR-0006 "Transport").
type Class string

// The M2 connection classes. The relay feed and content fetch, the relay
// classes, are Planned (M4).
const (
	// ClassRaft carries hashicorp/raft RPCs between Raft members.
	ClassRaft Class = "ruralz-raft"
	// ClassForward carries writes a follower forwards to the leader, which
	// re-authorizes each as its original principal.
	ClassForward Class = "ruralz-forward"
	// ClassStatus carries status forwarding to the leader: replica status
	// reports, and the Control Stream's ACK, NACK and heartbeat
	// aggregates.
	ClassStatus Class = "ruralz-status"
	// ClassContent carries content replication between replicas.
	ClassContent Class = "ruralz-content"
	// ClassJoin carries the token Join of ruralz control join, the only
	// class a connection without a client certificate may use.
	ClassJoin Class = "ruralz-join"
)

// Classes returns every M2 class in ALPN preference order.
func Classes() []Class {
	return []Class{ClassRaft, ClassForward, ClassStatus, ClassContent, ClassJoin}
}

// Admits reports whether a peer of role may use class c: Raft and write
// forwarding for Raft members, status forwarding and content replication
// for replicas, and Join for a caller without a certificate (role "").
// No M2 class admits a relay.
func (c Class) Admits(role Role) bool {
	switch c {
	case ClassRaft, ClassForward, ClassStatus, ClassContent:
		return role == RoleReplica
	case ClassJoin:
		return role == "" || role == RoleReplica
	default:
		return false
	}
}
