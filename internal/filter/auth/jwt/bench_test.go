// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
)

// BenchmarkHandle measures one authentication from the cached key set per
// algorithm; RS256 with a 2,048-bit key is the onRequestHeaders auth.jwt
// row of the per-stage latency budget (45 µs p50, 100 µs p99, target).
func BenchmarkHandle(b *testing.B) {
	fx := newFixture(b, options{})
	for _, k := range []*mockidp.Key{fx.keys.rs, fx.keys.ps, fx.keys.es, fx.keys.ed} {
		b.Run(string(k.Alg), func(b *testing.B) {
			x := bearer(mint(b, k, nil, map[string]any{"scope": "orders:read orders:write", "tenant": "acme"}))
			b.ReportAllocs()
			for b.Loop() {
				x.Ident = nil
				if r := fx.f.Handle(b.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
					b.Fatalf("result %+v", r)
				}
			}
		})
	}
}
