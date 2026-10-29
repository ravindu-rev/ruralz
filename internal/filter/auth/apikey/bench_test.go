// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"fmt"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// BenchmarkHandle measures one authentication among 10,000 Consumers: the
// lookup is one digest and one map probe whatever the Consumer count. It
// reports the allocations of an authenticated request, the identity the
// Filter sets; the onRequestHeaders rows of the per-stage latency budget
// give other Filters 2 µs p50 and 5 µs p99 (target).
func BenchmarkHandle(b *testing.B) {
	consumers := make([]*v1alpha1.Consumer, 0, 10_000)
	for i := range 10_000 {
		consumers = append(consumers, consumer(fmt.Sprintf("c%05d", i), "", literal("primary", fmt.Sprintf("key-%05d-0123456789abcdefghijkl", i))))
	}
	f := build(b, buildEnv(nil, nil, consumers...))
	key := "key-04242-0123456789abcdefghijkl"
	x := withKey(key)
	h, line := x.Hdr, x.Hdr["X-Api-Key"]
	b.ReportAllocs()
	for b.Loop() {
		// The Filter strips the field: put the same line back.
		x.Ident = nil
		h["X-Api-Key"] = line
		if r := f.Handle(b.Context(), phase.OnRequestHeaders, x); r.Outcome != filter.Continue {
			b.Fatalf("result %+v", r)
		}
	}
}
