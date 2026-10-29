// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Allocation gate and benchmarks of authz.cel in onRequestHeaders
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": 2 µs at p50 and 5 µs at p99, 0
// allocations, targets).

// TestHandleAddsNoAllocations checks that an admitting Handle allocates
// nothing beyond the rule's own evaluation, for the documents' rules.
func TestHandleAddsNoAllocations(t *testing.T) {
	if raceEnabled {
		t.Skip("cel-go's pools drop items at random under -race")
	}
	for _, tc := range []struct {
		src    string
		phases phase.Set
		p      phase.Phase
	}{
		{`consumer != null && consumer.tier == "gold"`, headersOnly, phase.OnRequestHeaders},
		{`auth.claims.scope.split(" ").exists(s, s == "orders:read")`, headersOnly, phase.OnRequestHeaders},
		{`request.body.amount < 1000`, bodyOnly, phase.OnRequestBody},
	} {
		f := build(t, tc.src, tc.phases, &decisions{})
		x := exchange(vars(t), nil)
		ctx := t.Context()
		if r := f.Handle(ctx, tc.p, x); r.Outcome != filter.Continue {
			t.Fatalf("Handle(%q) = %+v, want Continue", tc.src, r)
		}
		handle := testing.AllocsPerRun(200, func() { f.Handle(ctx, tc.p, x) })
		eval := testing.AllocsPerRun(200, func() { _, _ = f.rule.EvalBool(ctx, x.Vars()) })
		if handle > eval {
			t.Errorf("Handle(%q) allocates %.0f per run, its evaluation %.0f: the Filter adds allocations", tc.src, handle, eval)
		}
	}
}

// TestUntrackedRulesAllocateNothing checks the stage budget end to end for
// rules over the prepared Consumer and Route, the request method and an
// expression without a comprehension: deciding allocates nothing at all.
// The quota and Route-label rules run untracked because the snapshot's
// Consumers and Routes bound the values they scan (expr.InputCaps).
func TestUntrackedRulesAllocateNothing(t *testing.T) {
	if raceEnabled {
		t.Skip("allocation counts are not compared under -race")
	}
	for _, src := range []string{
		`consumer != null && consumer.tier == "gold"`,
		`consumer != null && "daily-tokens" in consumer.quotas`,
		`route.labels.team == "core" && consumer.name == "acme"`,
		`route.labels.team.startsWith("co")`,
		`request.method == "GET" && auth.method == "jwt"`,
	} {
		f := build(t, src, headersOnly, &decisions{})
		x := exchange(vars(t), nil)
		ctx := t.Context()
		if r := f.Handle(ctx, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			t.Fatalf("Handle(%q) = %+v, want Continue", src, r)
		}
		if n := testing.AllocsPerRun(200, func() { f.Handle(ctx, phase.OnRequestHeaders, x) }); n != 0 {
			t.Errorf("Handle(%q) allocates %v per decision, want 0", src, n)
		}
	}
}

// benchmarkRule measures an admitting Handle of src in onRequestHeaders.
func benchmarkRule(b *testing.B, src string) {
	f := build(b, src, headersOnly, &decisions{})
	x := exchange(vars(b), nil)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if r := f.Handle(ctx, phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			b.Fatalf("Handle = %+v, want Continue", r)
		}
	}
}

// BenchmarkGoldTier is the authz-gold-only rule of Security and identity.
func BenchmarkGoldTier(b *testing.B) {
	benchmarkRule(b, `consumer != null && consumer.tier == "gold"`)
}

// BenchmarkOrdersScope is the authz-orders rule of the example Bundle, a
// comprehension that runs untracked under the snapshot's caps.
func BenchmarkOrdersScope(b *testing.B) {
	benchmarkRule(b, `auth.claims.scope.split(" ").exists(s, s == "orders:read")`)
}

// BenchmarkQuota is a rule over the Consumer's quotas, untracked under the
// snapshot's caps.
func BenchmarkQuota(b *testing.B) {
	benchmarkRule(b, `consumer != null && "daily-tokens" in consumer.quotas`)
}
