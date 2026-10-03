// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import "testing"

// Benchmarks for the per-response RateLimit field build of 05 req 68: the
// handler appends one member per applied limit into a pooled buffer, with
// no allocation.

func BenchmarkAppendRateLimitFields(b *testing.B) {
	policy := make([]byte, 0, 256)
	rl := make([]byte, 0, 256)
	b.ReportAllocs()
	for b.Loop() {
		var err error
		policy, rl = policy[:0], rl[:0]
		if policy, err = AppendRateLimitPolicy(policy, "ratelimit-global", 1000, 1); err != nil {
			b.Fatal(err)
		}
		if policy, err = AppendRateLimitPolicy(policy, "ratelimit-gold.1", 100, 1); err != nil {
			b.Fatal(err)
		}
		if rl, err = AppendRateLimit(rl, "ratelimit-global", 999, 1); err != nil {
			b.Fatal(err)
		}
		if rl, err = AppendRateLimit(rl, "ratelimit-gold.1", 99, 1); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppendList(b *testing.B) {
	l := allTypesList()
	buf := make([]byte, 0, 1024)
	b.ReportAllocs()
	for b.Loop() {
		var err error
		if buf, err = AppendList(buf[:0], l); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseList(b *testing.B) {
	const in = `"ratelimit-global";q=1000;w=1, "ratelimit-gold.1";q=100;w=1, "ratelimit-gold.2";q=5000;w=60`
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParseList(in); err != nil {
			b.Fatal(err)
		}
	}
}
