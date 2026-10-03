// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"cmp"
	"math"
	"net/netip"
	"slices"
	"time"
)

// Endpoint is one member of an Upstream's Endpoint set.
type Endpoint struct {
	// Identity is the stable name the balancer, health state and
	// /debug/upstreams use: the configured address of a static Endpoint,
	// ip:port of an A/AAAA answer, target:port (no trailing dot) of an SRV
	// answer (05 reqs 5 and 6).
	Identity string
	// Host is the name for the Host header and TLS ServerName when
	// Upstream.spec.tls.sni is unset (05 req 29): the static host name, the
	// SRV target (no trailing dot) or discovery.service for A/AAAA answers;
	// "" for a static IP literal, whose IP is used instead.
	Host string
	// Port is the port dialed.
	Port uint16
	// Addrs are the cached IPs with Port, in answer order; a dial tries
	// them in this order within one 1 s budget (05 req 5). Empty for a
	// static host name that has never resolved.
	Addrs []netip.AddrPort
	// Weight is endpoints[].weight, 1 for A/AAAA answers, or the SRV
	// weight (05 req 6).
	Weight uint32
}

// equal reports whether e and o are the same member with the same
// addresses.
func (e *Endpoint) equal(o *Endpoint) bool {
	return e.Identity == o.Identity && e.Host == o.Host && e.Port == o.Port &&
		e.Weight == o.Weight && slices.Equal(e.Addrs, o.Addrs)
}

// Status is the refresh state of a Source (the "source" object of
// /debug/upstreams, 05 req 96).
type Status struct {
	// Type is "static" or "dns".
	Type string
	// LastRefresh is the end of the last refresh that got a good answer
	// (zero: none yet).
	LastRefresh time.Time
	// Stale is true while the last refresh failed and the set is the last
	// good one: the discovery_stale degraded reason (05 req 7).
	Stale bool
	// Failures counts consecutive failed refreshes.
	Failures int
	// Err is the last refresh failure; nil after a good answer.
	Err error
}

// Set is an immutable Endpoint set with its refresh status, published
// copy-on-write (05 req 8). Callers must not modify it.
type Set struct {
	// Endpoints are sorted by Identity (byte order, like balance.Normalize)
	// with unique identities.
	Endpoints []Endpoint
	// Version increases whenever Endpoints change (members, weights, hosts
	// or addresses); a status-only change keeps it.
	Version uint64
	// Status is the refresh state when the Set was published.
	Status Status

	key string  // Spec.key of the source that published it
	src *Source // the Source that published it (nil for other Sets)
}

// Len returns the number of Endpoints; a nil Set has none.
func (s *Set) Len() int {
	if s == nil {
		return 0
	}
	return len(s.Endpoints)
}

// Identities returns the Endpoint identities in order.
func (s *Set) Identities() []string {
	if s == nil {
		return nil
	}
	out := make([]string, len(s.Endpoints))
	for i := range s.Endpoints {
		out[i] = s.Endpoints[i].Identity
	}
	return out
}

// Lookup returns the Endpoint with the given identity.
func (s *Set) Lookup(identity string) (*Endpoint, bool) {
	if s == nil {
		return nil, false
	}
	i, ok := slices.BinarySearchFunc(s.Endpoints, identity, func(e Endpoint, id string) int {
		return cmp.Compare(e.Identity, id)
	})
	if !ok {
		return nil, false
	}
	return &s.Endpoints[i], true
}

// SameMembers reports whether s and o have the same identities and
// weights, so balancer structures need no rebuild (address changes only
// affect dialing).
func (s *Set) SameMembers(o *Set) bool {
	if s.Len() != o.Len() {
		return false
	}
	for i := range s.Len() {
		a, b := &s.Endpoints[i], &o.Endpoints[i]
		if a.Identity != b.Identity || a.Weight != b.Weight {
			return false
		}
	}
	return true
}

// sameEndpoints reports whether two sorted Endpoint lists are equal.
func sameEndpoints(a, b []Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !a[i].equal(&b[i]) {
			return false
		}
	}
	return true
}

// normalize sorts eps by identity and merges equal identities: weights
// add up (saturating) and addresses of the first occurrence are kept. It
// modifies and returns eps.
func normalize(eps []Endpoint) []Endpoint {
	slices.SortStableFunc(eps, func(a, b Endpoint) int { return cmp.Compare(a.Identity, b.Identity) })
	out := eps[:0]
	for _, e := range eps {
		if n := len(out); n > 0 && out[n-1].Identity == e.Identity {
			sum := uint64(out[n-1].Weight) + uint64(e.Weight)
			out[n-1].Weight = uint32(min(sum, math.MaxUint32))
			continue
		}
		out = append(out, e)
	}
	return slices.Clip(out)
}

// withPort attaches port to each address.
func withPort(addrs []netip.Addr, port uint16) []netip.AddrPort {
	if len(addrs) == 0 {
		return nil
	}
	out := make([]netip.AddrPort, len(addrs))
	for i, a := range addrs {
		out[i] = netip.AddrPortFrom(a, port)
	}
	return out
}
