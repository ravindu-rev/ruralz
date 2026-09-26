// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Tests for architecture section 2.11 (WP-01) and R-56: every label index
// constant equals the position of its value in the catalog enumeration,
// and every Num* constant is the enumeration's length, so fixed-size
// handle arrays line up with catalog.Families (spec 09 req 43 and 44).

// labelValues returns the enumeration of label on family.
func labelValues(t *testing.T, family, label string) []string {
	t.Helper()
	f, ok := catalog.Lookup(family)
	if !ok {
		t.Fatalf("catalog has no family %s", family)
	}
	for _, l := range f.Labels {
		if l.Name == label {
			if l.Values == nil {
				t.Fatalf("%s{%s} is not an enumeration", family, label)
			}
			return l.Values
		}
	}
	t.Fatalf("%s has no label %s", family, label)
	return nil
}

func TestLabelIndexes(t *testing.T) {
	tests := []struct {
		family, label string
		// index maps each constant to its expected label value.
		index map[int]string
		num   int
	}{
		{catalog.HTTPListenerRequestsTotal, "protocol", map[int]string{ProtoHTTP1: "http1", ProtoHTTP2: "http2"}, NumProtocols},
		{catalog.ListenerOpenConnections, "protocol", map[int]string{ProtoHTTP1: "http1", ProtoHTTP2: "http2"}, NumProtocols},
		{catalog.HTTPListenerRequestsTotal, "origin", map[int]string{OriginUpstream: "upstream", OriginNode: "node", OriginDependency: "dependency"}, NumOrigins},
		{catalog.ListenerConnectionsTotal, "result", map[int]string{ConnAccepted: "accepted", ConnTLSFailure: "tls_failure", ConnRefused: "refused"}, NumConnResults},
		{catalog.CacheRequestsTotal, "result", map[int]string{CacheHit: "hit", CacheMiss: "miss", CacheBypass: "bypass", CacheStale: "stale", CacheStaleError: "stale_error"}, NumCacheResults},
		{catalog.RateLimitDecisionsTotal, "result", map[int]string{RateLimitAllow: "allow", RateLimitDenyLocal: "deny_local", RateLimitDenyGlobal: "deny_global", RateLimitFailOpen: "fail_open"}, NumRateLimitResults},
		{catalog.QuotaDecisionsTotal, "result", map[int]string{QuotaAllow: "allow", QuotaDeny: "deny", QuotaNoQuota: "no_quota", QuotaFailOpen: "fail_open"}, NumQuotaResults},
		{catalog.CacheStoreSkippedTotal, "reason", map[int]string{SkipSizeLimit: "size_limit", SkipBufferBudget: "buffer_budget", SkipMemory: "memory"}, NumSkipReasons},
		{catalog.FilterFailuresTotal, "mode", map[int]string{ModeOpen: "open", ModeClosed: "closed"}, NumModes},
		{catalog.UpstreamAttemptsTotal, "error", map[int]string{ErrNone: "none", ErrConnect: "connect", ErrTimeout: "timeout", ErrReset: "reset", ErrTLS: "tls"}, NumAttemptErrors},
		{catalog.UpstreamBreakerStateInfo, "state", map[int]string{BreakerClosed: "closed", BreakerOpen: "open", BreakerHalfOpen: "half_open"}, NumBreakerStates},
		{catalog.UpstreamEjectionsTotal, "reason", map[int]string{EjectPassive: "passive", EjectActive: "active"}, NumEjectReasons},
		{catalog.UpstreamDegradedInfo, "reason", map[int]string{UpDegradedPanic: "panic", UpDegradedDiscoveryStale: "discovery_stale", UpDegradedBalancerBudget: "balancer_budget"}, NumUpDegraded},
		{catalog.UpstreamCELErrorsTotal, "field", map[int]string{CELHashKey: "hashKey", CELRetryOn: "retryOn", CELFailureWhen: "failureWhen"}, NumCELFields},
		{catalog.UpstreamPoolConnections, "state", map[int]string{PoolIdle: "idle", PoolActive: "active"}, NumPoolStates},
		{catalog.StateCallsTotal, "result", map[int]string{StateResultOK: "ok", StateResultError: "error", StateResultTimeout: "timeout", StateResultSkipped: "skipped"}, NumStateResults},
		{catalog.StateWritesDroppedTotal, "kind", map[int]string{WriteRefund: "refund", WriteCacheSet: "cache_set", WriteCacheInvalidate: "cache_invalidate"}, NumWriteKinds},
	}
	for _, tt := range tests {
		t.Run(tt.family+"/"+tt.label, func(t *testing.T) {
			values := labelValues(t, tt.family, tt.label)
			if tt.num != len(values) || len(tt.index) != len(values) {
				t.Fatalf("Num = %d, %d constants, catalog has %d values %v", tt.num, len(tt.index), len(values), values)
			}
			for i, want := range tt.index {
				if i < 0 || i >= len(values) || values[i] != want {
					t.Errorf("index %d names %q in the catalog, want %q (values %v)", i, at(values, i), want, values)
				}
			}
		})
	}
}

func at(values []string, i int) string {
	if i < 0 || i >= len(values) {
		return "<out of range>"
	}
	return values[i]
}

// TestStateOpIndexes checks StateOp* against catalog.Ops(), the op label of
// every State Store family (R-56: script_multi and pipeline sit before
// cache_set, so statestore.OpKind values are not indexes).
func TestStateOpIndexes(t *testing.T) {
	want := map[int]string{
		StateOpGCRA: "gcra", StateOpQuota: "quota", StateOpCacheGet: "cache_get", StateOpScriptMulti: "script_multi",
		StateOpPipeline: "pipeline", StateOpCacheSet: "cache_set", StateOpCacheInvalidate: "cache_invalidate", StateOpRefund: "refund",
	}
	ops := catalog.Ops()
	if NumStateOps != len(ops) || len(want) != len(ops) {
		t.Fatalf("NumStateOps = %d, %d constants, catalog.Ops() has %d", NumStateOps, len(want), len(ops))
	}
	for i, name := range want {
		if ops[i] != name {
			t.Errorf("StateOp index %d is %q in catalog.Ops(), want %q", i, ops[i], name)
		}
	}
	for _, fam := range []struct{ family, label string }{
		{catalog.StateCallDurationSeconds, "op"}, {catalog.StateCallsTotal, "op"}, {catalog.StateOpsTotal, "kind"},
	} {
		values := labelValues(t, fam.family, fam.label)
		if len(values) != len(ops) {
			t.Fatalf("%s{%s} has %d values", fam.family, fam.label, len(values))
		}
		for i := range ops {
			if values[i] != ops[i] {
				t.Errorf("%s{%s}[%d] = %q, want %q", fam.family, fam.label, i, values[i], ops[i])
			}
		}
	}
	kinds := catalog.WriteKinds()
	for i, name := range map[int]string{WriteRefund: "refund", WriteCacheSet: "cache_set", WriteCacheInvalidate: "cache_invalidate"} {
		if kinds[i] != name {
			t.Errorf("Write index %d is %q in catalog.WriteKinds(), want %q", i, kinds[i], name)
		}
	}
}

// TestPhaseIndexes checks that PolicyMetrics arrays indexed by phase.Phase
// line up with the catalog's phase label.
func TestPhaseIndexes(t *testing.T) {
	for _, family := range []string{catalog.FilterDurationSeconds, catalog.FilterShortCircuitsTotal, catalog.FilterFailuresTotal} {
		values := labelValues(t, family, "phase")
		if len(values) != int(phase.Count) {
			t.Fatalf("%s has %d phases, phase.Count is %d", family, len(values), phase.Count)
		}
		for p := phase.OnRequestHeaders; p < phase.Count; p++ {
			if values[p] != p.String() {
				t.Errorf("%s phase[%d] = %q, want %q", family, p, values[p], p.String())
			}
		}
	}
	var pm PolicyMetrics
	if len(pm.Duration) != int(phase.Count) || len(pm.Failures[0]) != NumModes {
		t.Fatal("PolicyMetrics arrays are not sized by phase.Count and NumModes")
	}
	var sm StateMetrics
	if len(sm.Calls) != NumStateOps || len(sm.Calls[0]) != NumStateResults || len(sm.WritesDropped) != NumWriteKinds {
		t.Fatal("StateMetrics arrays are not sized by the label constants")
	}
}

func TestOutcomes(t *testing.T) {
	// ruralz.outcome values (spec 04 req 44 span attributes).
	for got, want := range map[string]string{
		OutcomeContinue: "continue", OutcomeRespond: "respond", OutcomeCannotDecide: "cannot_decide", OutcomeSkipped: "skipped",
	} {
		if got != want {
			t.Errorf("outcome %q, want %q", got, want)
		}
	}
}
