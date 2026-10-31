// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/testkit/l4lb"
)

// errTorn is returned when a dependency starts after the teardown began.
var errTorn = errors.New("topology: torn down")

// BalancerOptions configure an L4 balancer over the Nodes.
type BalancerOptions struct {
	// ProxyProtocol starts each backend connection with a PROXY protocol
	// v2 header carrying the client's address, for a scenario that needs
	// per-client source.ip; the balanced listener must set
	// proxyProtocol: true in the Bundle.
	ProxyProtocol bool
	// Config holds the other balancer settings: listen address, probe
	// interval, timeout and thresholds, idle timeout, slow start, limits,
	// redispatch and dial timeout. The harness sets Backends, ProbeTLS and
	// ProxyProtocol, and leaves Clock unset: the balancer runs in real
	// time.
	Config l4lb.Config
}

// Balancer is an L4 balancer over one listener of the topology's Nodes.
type Balancer struct {
	*l4lb.Balancer
	listener string
	addMu    sync.Mutex // makes add's check and AddBackend one step
}

// Listener returns the name of the balanced listener.
func (b *Balancer) Listener() string { return b.listener }

// StartBalancer starts an L4 balancer splicing to the listener named
// listener of every Node that has one and probing each Node's /readyz,
// over the admin TLS the harness chose; Nodes added later join it as a
// Cluster scales out. It lives until the teardown, which keeps its
// transition log as failure evidence.
func (t *Topology) StartBalancer(ctx context.Context, listener string, o BalancerOptions) (*Balancer, error) {
	c := o.Config
	c.Backends = nil
	c.ProxyProtocol = o.ProxyProtocol
	c.Clock = nil
	c.ProbeTLS = t.adminTLS()
	for _, n := range t.Nodes() {
		if be, ok := backendOf(n, listener); ok {
			c.Backends = append(c.Backends, be)
		}
	}
	lb, err := l4lb.Start(t.ctx, c) //nolint:contextcheck // The balancer outlives the call: it runs until the teardown cancels the topology's context.
	if err != nil {
		return nil, fmt.Errorf("topology: %w", err)
	}
	b := &Balancer{Balancer: lb, listener: listener}
	if !t.track(func() { t.balancers = append(t.balancers, b) }) {
		_ = lb.Close()
		return nil, errTorn
	}
	// A Node added between the listing above and the tracking joins here.
	for _, n := range t.Nodes() {
		if err := b.add(ctx, n); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// add makes n a backend unless it has no such listener or already is one.
func (b *Balancer) add(ctx context.Context, n *Node) error {
	be, ok := backendOf(n, b.listener)
	if !ok {
		return nil
	}
	b.addMu.Lock()
	defer b.addMu.Unlock()
	if _, present := b.Backend(be.Name); present {
		return nil
	}
	if err := b.AddBackend(ctx, be); err != nil {
		return fmt.Errorf("topology: %w", err)
	}
	return nil
}

// backendOf returns n's backend for listener.
func backendOf(n *Node, listener string) (l4lb.Backend, bool) {
	addr := n.Addr(listener)
	if addr == "" {
		return l4lb.Backend{}, false
	}
	return l4lb.Backend{Name: n.Name(), Addr: addr, ReadyURL: n.AdminURL() + "/readyz"}, true
}
