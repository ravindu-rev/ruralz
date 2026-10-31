// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"strconv"
	"strings"
	"testing"
)

// largeBundle returns a Bundle of n Routes to one Upstream, each with its
// own match.when and a ratelimit Policy keyed by its own expression, so
// stage J compiles 2n distinct expressions.
func largeBundle(n int) map[string]string {
	var routes, policies strings.Builder
	for i := range n {
		s := strconv.Itoa(i)
		if i > 0 {
			routes.WriteString("---\n")
			policies.WriteString("---\n")
		}
		routes.WriteString(routeWhen("r"+s, "orders", "    when: request.headers[\"x-n\"] == \""+s+"\"\n", "  policies:\n    - name: rl"+s+"\n"))
		policies.WriteString(policyDoc("rl"+s, "ratelimit", "  config:\n    key: 'source.ip + \""+s+"\"'\n    limits:\n      - requests: 10\n        window: 1s\n"))
	}
	return map[string]string{
		"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"),
		"routes.yaml": routes.String(), "policies.yaml": policies.String(),
	}
}

// BenchmarkRun measures a run of 2,000 resources: the configuration model
// targets 10,000 resources in under 2 seconds on a four-core laptop.
func BenchmarkRun(b *testing.B) {
	p := newPipeline(b, Options{Workers: 4})
	m := bundle(largeBundle(1000))
	b.ResetTimer()
	for b.Loop() {
		res := run(b, p, m, Request{})
		if res.Validated == nil {
			b.Fatalf("no Validated: %q", texts(res.Diagnostics)[:min(3, len(res.Diagnostics))])
		}
	}
}
