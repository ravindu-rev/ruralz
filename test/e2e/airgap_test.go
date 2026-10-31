// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"errors"
	"net"
	"slices"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The air-gapped start of docs/engineering/03-testing-and-quality-
// strategy.md "End-to-end tests": inside a loopback-only network
// namespace, a Node on the REST example Bundle, with every dependency on
// loopback, becomes ready within the readiness wait, passes scenario 2's
// checks and shows only the expected degraded reasons, and every
// non-loopback dial fails without delaying readiness.

// expectedDegraded are the degraded reasons the air-gapped Node shows: the
// REST example Bundle's Upstreams are plain http, which the e2e
// Environment points at the mock Upstreams on loopback, so the Node counts
// cleartext hops (docs/architecture/10-observability.md "Degraded
// states").
func expectedDegraded() []string { return []string{"cleartext_hop"} }

// nonLoopback are addresses outside every loopback network, one per
// family, from the documentation ranges, which no host answers.
func nonLoopback() []string { return []string{"192.0.2.1:443", "[2001:db8::1]:443"} }

// dialFailsAtOnce is how long a refused dial may take: in a namespace
// without a route it fails with ENETUNREACH at once, never on a timeout.
const dialFailsAtOnce = 2 * time.Second

// TestAirGappedStart runs the air-gapped start in a namespace of its own.
func TestAirGappedStart(t *testing.T) {
	if !inNetNS(t, nil) {
		return
	}
	ctx := t.Context()
	for _, addr := range nonLoopback() {
		start := time.Now()
		dctx, cancel := context.WithTimeout(ctx, dialFailsAtOnce)
		c, err := (&net.Dialer{}).DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			_ = c.Close()
			t.Fatalf("a dial to %s left the loopback-only namespace", addr)
		}
		// ENETUNREACH without a route; EAFNOSUPPORT on a host without IPv6.
		if took := time.Since(start); !errors.Is(err, syscall.ENETUNREACH) && !errors.Is(err, syscall.EAFNOSUPPORT) {
			t.Errorf("a dial to %s failed after %v with %v, want a refusal at once, not a timeout", addr, took, err)
		}
	}
	if _, err := net.DefaultResolver.LookupHost(ctx, "auth.shop.example"); err == nil {
		t.Error("a name outside the namespace resolved")
	}

	// startShopRest waits up to the readiness wait, topology's
	// ReadyTimeout, for each Node's /readyz.
	s := startShopRest(t, shopRestOptions{nodes: 1})
	t.Run("EffectiveChains", func(t *testing.T) { checkEffectiveChains(t, s) })
	t.Run("Cases", func(t *testing.T) { runShopRestCases(t, s) })
	t.Run("Spans", func(t *testing.T) { checkFilterSpans(t, s) })
	t.Run("UpstreamLegs", func(t *testing.T) { checkUpstreamLegs(t, s) })
	t.Run("RateLimits", func(t *testing.T) { checkRateLimits(t, s) })
	t.Run("Quota", func(t *testing.T) { checkQuota(t, s) })
	t.Run("Degraded", func(t *testing.T) { checkDegraded(t, s.nodes[0], expectedDegraded()) })
}

// checkDegraded holds the reasons the Node's ruralz_node_degraded_info
// raises, read from /metrics with the metrics token, equal to want.
func checkDegraded(t *testing.T, n *topology.Node, want []string) {
	resp, err := n.AdminAs(t.Context(), "/metrics", topology.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	set, err := promtext.ParseBytes(resp.Body)
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	var raised []string
	for _, s := range set.Samples("ruralz_node_degraded_info", nil) {
		if s.Value == 1 {
			raised = append(raised, s.Labels["reason"])
		}
	}
	slices.Sort(raised)
	if !slices.Equal(raised, want) {
		t.Errorf("degraded reasons %q, want only %q", raised, want)
	}
}
