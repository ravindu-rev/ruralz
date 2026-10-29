// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package basic

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// BenchmarkCacheHit measures the common path of an auth.basic request: a
// success cache hit, which skips the throttle and the hash ("A hit skips
// throttle and hash"); a miss costs one PBKDF2 at the credential's
// iteration count on top.
func BenchmarkCacheHit(b *testing.B) {
	fx := newFixture(b, consumer("acme", credential("acme-batch", "s3cret", 0)))
	value := basicValue("acme-batch", "s3cret")
	x := login("192.0.2.1", "acme-batch", "s3cret")
	if r := fx.f.Handle(b.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
		b.Fatalf("result %+v", r)
	}
	line := []string{value}
	b.ReportAllocs()
	for b.Loop() {
		// The Filter strips the field: put the same line back.
		x.Ident = nil
		x.Hdr["Authorization"] = line
		if r := fx.f.Handle(b.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			b.Fatalf("result %+v", r)
		}
	}
}
