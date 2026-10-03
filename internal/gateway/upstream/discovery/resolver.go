// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"slices"
)

// Resolver is the DNS client a Source uses. *net.Resolver implements it;
// tests pass a fake.
type Resolver interface {
	// LookupIPAddr returns the A and AAAA answers for host.
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
	// LookupSRV returns the SRV answers for _service._proto.name.
	LookupSRV(ctx context.Context, service, proto, name string) (string, []*net.SRV, error)
}

// NewResolver returns the production Resolver: the pure-Go net.Resolver,
// so lookups behave the same with CGO_ENABLED=0 on every platform (05 req
// 6; Tech stack "Service discovery").
func NewResolver() Resolver { return &net.Resolver{PreferGo: true} }

// errEmptyAnswer is the failure of an answer without usable records; like
// NXDOMAIN it empties the set only after 3 consecutive refreshes (05 req 7).
var errEmptyAnswer = errors.New("discovery: empty answer")

// notFound reports whether err is NXDOMAIN or an empty answer (the name
// exists without records of the type): the failure class that empties the
// set after 3 consecutive refreshes (05 req 7). Timeouts, refused queries
// and server failures are ordinary failures that keep the last set forever.
func notFound(err error) bool {
	if errors.Is(err, errEmptyAnswer) {
		return true
	}
	var de *net.DNSError
	return errors.As(err, &de) && de.IsNotFound
}

// malformedOnly reports whether err only says that the resolver dropped
// some of an SRV answer's records and returned the others: since Go 1.20,
// (*net.Resolver).LookupSRV filters out records whose target is not a
// valid domain name and returns the remaining ones together with a
// *net.DNSError that is neither not-found, timeout nor temporary. Such an
// answer is good; the dropped records are unusable like those selectSRV
// ignores. Without remaining records the error stands.
func malformedOnly(err error, records []*net.SRV) bool {
	if len(records) == 0 {
		return false
	}
	var de *net.DNSError
	return errors.As(err, &de) && !de.IsNotFound && !de.IsTimeout && !de.IsTemporary
}

// ipsOf converts a lookup answer to addresses in answer order, IPv4-mapped
// IPv6 addresses unmapped, duplicates and invalid entries dropped.
func ipsOf(answer []net.IPAddr) []netip.Addr {
	out := make([]netip.Addr, 0, len(answer))
	for _, ia := range answer {
		a, ok := netip.AddrFromSlice(ia.IP)
		if !ok {
			continue
		}
		a = a.Unmap()
		if ia.Zone != "" && a.Is6() {
			a = a.WithZone(ia.Zone)
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	return out
}
