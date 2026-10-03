// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"cmp"
	"math"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
)

// srvTarget is one Endpoint an SRV answer contributes before its target is
// resolved.
type srvTarget struct {
	identity string // target:port, target without trailing dot
	host     string // target without trailing dot
	port     uint16
	weight   uint32
}

// selectSRV applies 05 req 6 to an SRV answer: only the lowest-priority
// group is used; in it, a weight of 0 is excluded unless every weight of
// the group is 0, in which case each counts 1; the target loses its
// trailing dot; records with the same target and port merge (weights add,
// saturating); the result is sorted by identity. Records that cannot name
// an Endpoint are ignored first: nil, port 0, the "." target (RFC 2782:
// service not available) or a target that is not a DNS name or IP
// literal. An empty result is an empty answer.
func selectSRV(records []*net.SRV) []srvTarget {
	usable := make([]*net.SRV, 0, len(records))
	lowest := uint16(math.MaxUint16)
	for _, r := range records {
		if usableSRV(r) {
			usable = append(usable, r)
			lowest = min(lowest, r.Priority)
		}
	}
	if len(usable) == 0 {
		return nil
	}
	allZero := true
	for _, r := range usable {
		if r.Priority == lowest && r.Weight > 0 {
			allZero = false
			break
		}
	}
	out := make([]srvTarget, 0, len(usable))
	for _, r := range usable {
		if r.Priority != lowest {
			continue
		}
		w := uint32(r.Weight)
		switch {
		case allZero:
			w = 1
		case w == 0:
			continue
		}
		host := strings.TrimSuffix(r.Target, ".")
		out = append(out, srvTarget{
			identity: net.JoinHostPort(host, strconv.Itoa(int(r.Port))),
			host:     host,
			port:     r.Port,
			weight:   w,
		})
	}
	slices.SortStableFunc(out, func(a, b srvTarget) int { return cmp.Compare(a.identity, b.identity) })
	merged := out[:0]
	for _, t := range out {
		if n := len(merged); n > 0 && merged[n-1].identity == t.identity {
			merged[n-1].weight = uint32(min(uint64(merged[n-1].weight)+uint64(t.weight), math.MaxUint32))
			continue
		}
		merged = append(merged, t)
	}
	return merged
}

// usableSRV reports whether r can name an Endpoint.
func usableSRV(r *net.SRV) bool {
	if r == nil || r.Port == 0 {
		return false
	}
	host := strings.TrimSuffix(r.Target, ".")
	if host == "" {
		return false
	}
	if isName(host) {
		return true
	}
	_, err := netip.ParseAddr(host)
	return err == nil
}
