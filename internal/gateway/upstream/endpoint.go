// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"net"
	"net/netip"
	"strconv"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/discovery"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// epState is the state of one Endpoint identity for as long as it stays in
// its Upstream's set: in-flight attempts, the addresses to dial and the
// connection pools. Set changes that keep the identity keep the epState, so
// counts and pooled connections survive them and Hot Reloads.
type epState struct {
	identity string
	// net is the dialing view, replaced when discovery changes the
	// Endpoint's host or addresses.
	net atomic.Pointer[epNet]
	// inflight counts attempts holding a bulkhead slot on this Endpoint,
	// the response body included: the in-flight part of least-request's
	// load and of the per-Endpoint cap.
	inflight atomic.Int64
	// pools are the connection pools by TLS setting (poolList).
	pools atomic.Pointer[poolList]
	// removed is set once the Endpoint left the set; its pools retire.
	removed atomic.Bool
}

// epNet is how an Endpoint is reached.
type epNet struct {
	// host is the Host header and TLS server name host part: the static
	// host name, the SRV target or discovery.service; the IP for a static
	// IP literal.
	host string
	// authority is host:port, the request URL host.
	authority string
	// addrs are the cached IPs with the port, in answer order.
	addrs []netip.AddrPort
}

// newNet builds the dialing view of a discovery Endpoint.
func newNet(e *discovery.Endpoint) *epNet {
	host := e.Host
	if host == "" {
		// A static IP literal: identity is ip:port.
		if h, _, err := net.SplitHostPort(e.Identity); err == nil {
			host = h
		} else if len(e.Addrs) > 0 {
			host = e.Addrs[0].Addr().String()
		}
	}
	return &epNet{host: host, authority: net.JoinHostPort(host, strconv.Itoa(int(e.Port))), addrs: e.Addrs}
}

// sameNet reports whether n describes e unchanged.
func (n *epNet) sameNet(e *discovery.Endpoint) bool {
	if n == nil || (e.Host != "" && n.host != e.Host) || len(n.addrs) != len(e.Addrs) {
		return false
	}
	for i := range n.addrs {
		if n.addrs[i] != e.Addrs[i] {
			return false
		}
	}
	return true
}

// epInfo is one Endpoint of a published table.
type epInfo struct {
	st *epState
	// weight is the effective weight (all-zero weights weigh 1 each).
	weight uint32
}

// table is one published Endpoint list, the payload of the balancer Set
// published with the same list (balance.Set.Payload), so selection and
// the per-Endpoint state always refer to the same indices.
type table struct {
	eps []epInfo
	// total is the effective weight sum.
	total uint64
}

// newTable builds the table of a discovery Set, reusing the epStates of
// prev for identities that remain (both lists are sorted by identity). It
// returns the epStates that left.
func newTable(prev *table, ds *discovery.Set) (*table, []*epState) {
	var eps []discovery.Endpoint
	if ds != nil {
		eps = ds.Endpoints
	}
	t := &table{eps: make([]epInfo, len(eps))}
	var old []epInfo
	if prev != nil {
		old = prev.eps
	}
	var gone []*epState
	j := 0
	for i := range eps {
		e := &eps[i]
		for j < len(old) && old[j].st.identity < e.Identity {
			gone = append(gone, old[j].st)
			j++
		}
		var st *epState
		if j < len(old) && old[j].st.identity == e.Identity {
			st = old[j].st
			j++
		} else {
			st = &epState{identity: e.Identity}
		}
		if !st.net.Load().sameNet(e) {
			st.net.Store(newNet(e))
		}
		t.eps[i] = epInfo{st: st, weight: e.Weight}
	}
	for ; j < len(old); j++ {
		gone = append(gone, old[j].st)
	}
	for _, e := range t.eps {
		t.total += uint64(e.weight)
	}
	if t.total == 0 {
		// The package weight rule of balance: when every weight is 0 they
		// all weigh 1.
		for i := range t.eps {
			t.eps[i].weight = 1
		}
		t.total = uint64(len(t.eps))
	}
	return t, gone
}

// balanceEndpoints returns the balancer's view of t. The effective weights
// select exactly as the configured ones under balance's weight rule.
func (t *table) balanceEndpoints() []balance.Endpoint {
	out := make([]balance.Endpoint, len(t.eps))
	for i := range t.eps {
		out[i] = balance.Endpoint{Identity: t.eps[i].st.identity, Weight: t.eps[i].weight}
	}
	return out
}

// identities returns the Endpoint identities in order.
func (t *table) identities() []string {
	out := make([]string, len(t.eps))
	for i := range t.eps {
		out[i] = t.eps[i].st.identity
	}
	return out
}

// attemptView is the balance.View of one attempt over one published
// table: Down from health (passively ejected or actively unhealthy), and
// Avoided for an Endpoint tried in this leg, suspect, holding a stalled
// attempt past the stalled share, or at its in-flight cap
// (docs/architecture/09-traffic-management-and-resilience.md "Load
// balancing", Figure 1, and "Circuit breakers").
type attemptView struct {
	tab   *table
	hv    health.View
	tried []string
	// maxConns is circuitBreaker.maxConnections of the leg's snapshot.
	maxConns int
}

var _ balance.View = (*attemptView)(nil)

// health returns the index in the health View of table Endpoint i: the
// tracker publishes its list before the balancer, so during a set change
// the two can differ, and identities decide. ok is false for an Endpoint
// the health View does not hold yet.
func (v *attemptView) health(i int) (int, bool) {
	id := v.tab.eps[i].st.identity
	n := v.hv.Len()
	if i < n && v.hv.Identity(i) == id {
		return i, true
	}
	lo, hi := 0, n
	for lo < hi {
		m := int(uint(lo+hi) >> 1)
		if v.hv.Identity(m) < id {
			lo = m + 1
		} else {
			hi = m
		}
	}
	return lo, lo < n && v.hv.Identity(lo) == id
}

// wasTried reports whether the leg already sent an attempt to identity.
func (v *attemptView) wasTried(identity string) bool {
	for _, t := range v.tried {
		if t == identity {
			return true
		}
	}
	return false
}

// endpointCap returns Endpoint i's in-flight cap: max(8, min(2 × its
// weighted share, 50%) of maxConnections).
func (v *attemptView) endpointCap(i int) int64 {
	return int64(resilience.EndpointCap(v.maxConns, uint64(v.tab.eps[i].weight), v.tab.total))
}

// Grade implements balance.View.
func (v *attemptView) Grade(i int) balance.Grade {
	ep := &v.tab.eps[i]
	down, avoid := false, false
	if j, ok := v.health(i); ok {
		down = v.hv.Down(j)
		avoid = v.hv.Avoid(j)
	}
	avoid = avoid || v.wasTried(ep.st.identity) || ep.st.inflight.Load() >= v.endpointCap(i)
	return balance.GradeOf(down, avoid)
}

// Load implements balance.View: in-flight attempts plus the attempts that
// matched failureWhen in the last second.
func (v *attemptView) Load(i int) int64 {
	n := v.tab.eps[i].st.inflight.Load()
	if j, ok := v.health(i); ok {
		n += v.hv.Failures(j)
	}
	return n
}
