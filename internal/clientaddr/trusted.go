// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
)

// Trusted is the compiled Gateway.spec.trustedProxies set. It is immutable
// and safe for concurrent use; the nil *Trusted trusts no address (the
// default, an empty list). Lookups walk a per-family binary trie in at most
// 32 or 128 steps and allocate nothing.
type Trusted struct {
	v4, v6   trie
	prefixes []netip.Prefix
}

// New compiles prefixes into a Trusted set. Each prefix is normalized as
// ParsePrefix does (masked, zone dropped, an IPv4-mapped prefix of at least
// 96 bits converted to its IPv4 prefix); invalid prefixes are ignored. An
// empty list returns a set that trusts nothing.
func New(prefixes []netip.Prefix) *Trusted {
	t := &Trusted{}
	for _, p := range prefixes {
		if p.IsValid() {
			t.prefixes = append(t.prefixes, normalizePrefix(p))
		}
	}
	slices.SortFunc(t.prefixes, comparePrefix)
	t.prefixes = slices.Compact(t.prefixes)
	for _, p := range t.prefixes {
		if p.Addr().Is4() {
			a := p.Addr().As4()
			t.v4.insert(a[:], p.Bits())
		} else {
			a := p.Addr().As16()
			t.v6.insert(a[:], p.Bits())
		}
	}
	return t
}

// Parse compiles CIDR strings (a bare address means /32 or /128) into a
// Trusted set; the first malformed entry is an error naming it.
func Parse(cidrs []string) (*Trusted, error) {
	ps := make([]netip.Prefix, 0, len(cidrs))
	for _, s := range cidrs {
		p, err := ParsePrefix(s)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p)
	}
	return New(ps), nil
}

// ErrPrefix is wrapped by ParsePrefix errors.
var ErrPrefix = errors.New("clientaddr: invalid trusted proxy prefix")

// ParsePrefix parses one trustedProxies entry: a CIDR, or a bare address
// meaning /32 or /128. The result is masked, and an IPv4-mapped IPv6
// prefix of at least 96 bits becomes the IPv4 prefix of (bits - 96), so it
// matches the unmapped client addresses (the 06 req 58 rule). Zones are
// refused.
func ParsePrefix(s string) (netip.Prefix, error) {
	if strings.Contains(s, "%") {
		return netip.Prefix{}, fmt.Errorf("%w %q: zones are not allowed", ErrPrefix, s)
	}
	var p netip.Prefix
	if strings.Contains(s, "/") {
		var err error
		if p, err = netip.ParsePrefix(s); err != nil {
			return netip.Prefix{}, fmt.Errorf("%w %q: %w", ErrPrefix, s, err)
		}
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%w %q: %w", ErrPrefix, s, err)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	return normalizePrefix(p), nil
}

// normalizePrefix masks a valid p and unmaps an IPv4-mapped prefix of at
// least 96 bits (a netip.Prefix never carries a zone).
func normalizePrefix(p netip.Prefix) netip.Prefix {
	a, bits := p.Addr(), p.Bits()
	if a.Is4In6() && bits >= 96 {
		a, bits = a.Unmap(), bits-96
	}
	return netip.PrefixFrom(a, bits).Masked()
}

// comparePrefix orders by family, address, then length.
func comparePrefix(a, b netip.Prefix) int {
	if c := a.Addr().Compare(b.Addr()); c != 0 {
		return c
	}
	return a.Bits() - b.Bits()
}

// Prefixes returns a copy of the normalized, sorted, deduplicated prefixes.
func (t *Trusted) Prefixes() []netip.Prefix {
	if t == nil {
		return nil
	}
	return slices.Clone(t.prefixes)
}

// Contains reports whether a, unmapped and without its zone, is inside the
// set. IPv4 addresses match IPv4 prefixes only (after unmapping) and IPv6
// addresses IPv6 prefixes only; an invalid address is never trusted.
func (t *Trusted) Contains(a netip.Addr) bool {
	if t == nil || !a.IsValid() {
		return false
	}
	a = a.Unmap()
	if a.Is4() {
		b := a.As4()
		return t.v4.contains(b[:])
	}
	b := a.As16()
	return t.v6.contains(b[:])
}

// trie is a binary trie over address bits; node 0 is the root.
type trie struct {
	nodes []trieNode
}

// trieNode is one bit position; child indexes are 0 when absent (the root
// is never a child).
type trieNode struct {
	child [2]uint32
	end   bool
}

// insert adds the first bits bits of addr.
func (t *trie) insert(addr []byte, bits int) {
	if len(t.nodes) == 0 {
		t.nodes = append(t.nodes, trieNode{})
	}
	n := uint32(0)
	for i := range bits {
		if t.nodes[n].end {
			return // a shorter prefix already covers this one
		}
		b := addr[i>>3] >> (7 - uint(i&7)) & 1
		next := t.nodes[n].child[b]
		if next == 0 {
			t.nodes = append(t.nodes, trieNode{})
			next = uint32(len(t.nodes) - 1) //nolint:gosec // G115: at most 129 nodes per prefix; a trustedProxies list is far below 2^32/129 entries
			t.nodes[n].child[b] = next
		}
		n = next
	}
	t.nodes[n].end = true
	t.nodes[n].child = [2]uint32{} // longer prefixes below are covered
}

// contains reports whether a prefix in t covers addr.
func (t *trie) contains(addr []byte) bool {
	if len(t.nodes) == 0 {
		return false
	}
	n := uint32(0)
	for i := range len(addr) * 8 {
		if t.nodes[n].end {
			return true
		}
		n = t.nodes[n].child[addr[i>>3]>>(7-uint(i&7))&1]
		if n == 0 {
			return false
		}
	}
	return t.nodes[n].end
}
