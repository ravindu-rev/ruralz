// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/ratelimit"
	"github.com/ravindu-rev/ruralz/internal/gateway/handler/handlertest"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// rateLimitPolicy builds a real local-only ratelimit Policy named name on
// the kit's clock, recording fields unless headers is none.
func rateLimitPolicy(t *testing.T, k *kit, name string, headers v1alpha1.RateLimitResponseHeaders, limits ...v1alpha1.RateLimit) *snapshot.Policy {
	t.Helper()
	table, err := ratelimit.NewTable(k.clk, nil, ratelimit.TableOptions{Entries: 40, LocalOnlyEntries: 20, Shards: 1, Seed: 7})
	if err != nil {
		t.Fatal(err)
	}
	local := true
	mode := v1alpha1.FailureModeClosed
	f, err := ratelimit.Factory{Table: table}.Build(t.Context(), filter.BuildEnv{
		Policy:            &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{Type: v1alpha1.PolicyTypeRateLimit, FailureMode: &mode}},
		Config:            &v1alpha1.RateLimitConfig{Limits: limits, LocalOnly: &local, ResponseHeaders: &headers},
		Scope:             phase.ScopeRoute,
		Phases:            phase.Of(phase.OnRequestHeaders),
		CEL:               celtest.Compiler(t).NewBuilder(nil, expr.InputCaps{}),
		StateStoreTimeout: 20 * time.Millisecond,
		Clock:             k.clk,
	})
	if err != nil {
		t.Fatalf("build the ratelimit Policy: %v", err)
	}
	return handlertest.Policy(name, phase.ClassAdmission, v1alpha1.PolicyTypeRateLimit, f, phase.OnRequestHeaders)
}

// TestRateLimitFieldsFromRatelimitPolicies checks the RateLimit fields of
// the real ratelimit Policy's records: an admitted request lists every
// applied limit in RateLimit-Policy and its count in RateLimit; the 429
// lists in RateLimit the denied limit only, t equal to Retry-After; a 429
// of a Policy with responseHeaders none carries Retry-After and the other
// Policies' RateLimit-Policy items, but no RateLimit.
func TestRateLimitFieldsFromRatelimitPolicies(t *testing.T) {
	t.Run("admitted then denied", func(t *testing.T) {
		k := newKit(t)
		rl := rateLimitPolicy(t, k, "rl", v1alpha1.RateLimitResponseHeadersIETF,
			v1alpha1.RateLimit{Requests: 2, Window: v1alpha1.Duration(time.Second)},
			v1alpha1.RateLimit{Requests: 100, Window: v1alpha1.Duration(time.Minute)})
		k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", ok(), rl)}})
		res := k.get("/")
		if res.Code != http.StatusOK {
			t.Fatalf("first request = %d, want 200", res.Code)
		}
		if got := res.Header().Get("RateLimit-Policy"); got != `"rl.1";q=2;w=1, "rl.2";q=100;w=60` {
			t.Errorf("RateLimit-Policy = %q", got)
		}
		// One request spent one token of each bucket: t is the time until
		// each is whole again, rounded up.
		if got := res.Header().Get("RateLimit"); got != `"rl.1";r=1;t=1, "rl.2";r=99;t=1` {
			t.Errorf("RateLimit = %q", got)
		}
		var denied result
		for range 10 {
			if denied = k.get("/"); denied.Code == http.StatusTooManyRequests {
				break
			}
		}
		wantProblem(t, denied, http.StatusTooManyRequests, ratelimit.CodeLocalDeny)
		if got := denied.Header().Get("RateLimit-Policy"); got != `"rl.1";q=2;w=1, "rl.2";q=100;w=60` {
			t.Errorf("429 RateLimit-Policy = %q", got)
		}
		retry := denied.Header().Get("Retry-After")
		if got := denied.Header().Get("RateLimit"); retry == "" || got != `"rl.1";r=0;t=`+retry {
			t.Errorf("429 RateLimit = %q with Retry-After %q, want the denied limit only, t equal to Retry-After", got, retry)
		}
	})
	t.Run("denied without fields", func(t *testing.T) {
		k := newKit(t)
		other := rateLimitPolicy(t, k, "global", v1alpha1.RateLimitResponseHeadersIETF, v1alpha1.RateLimit{Requests: 1000, Window: v1alpha1.Duration(time.Second)})
		quiet := rateLimitPolicy(t, k, "quiet", v1alpha1.RateLimitResponseHeadersNone, v1alpha1.RateLimit{Requests: 1, Window: v1alpha1.Duration(time.Second)})
		k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", ok(), other, quiet)}})
		var denied result
		for range 10 {
			if denied = k.get("/"); denied.Code == http.StatusTooManyRequests {
				break
			}
		}
		wantProblem(t, denied, http.StatusTooManyRequests, ratelimit.CodeLocalDeny)
		if denied.Header().Get("Retry-After") == "" || denied.Header().Get("RateLimit") != "" {
			t.Errorf("429 fields = %v, want Retry-After and no RateLimit", denied.Header())
		}
		if got := denied.Header().Get("RateLimit-Policy"); got != `"global";q=1000;w=1` {
			t.Errorf("429 RateLimit-Policy = %q, want the other Policy's item", got)
		}
	})
}

// fields is a Filter recording fixed RateLimit fields in onRequestHeaders,
// then answering 429 when status says so.
func fields(status int, fs ...filter.RateLimitField) filter.Filter {
	return filterFunc(func(_ context.Context, p phase.Phase, x filter.Exchange) filter.Result {
		if p != phase.OnRequestHeaders {
			return filter.Next()
		}
		for _, f := range fs {
			x.AddRateLimitField(f)
		}
		if status == http.StatusTooManyRequests {
			return filter.Deny(status, "RZ-RL-003", http.Header{"Retry-After": {"30"}})
		}
		return filter.Next()
	})
}

// TestRateLimitFieldRules checks the field rules on recorded limits: a
// limit without a known count appears in RateLimit-Policy only; the fields
// replace any an Upstream sent; without a recorded limit the Upstream's
// pass; a Node-generated error carries them too.
func TestRateLimitFieldRules(t *testing.T) {
	upstream := http.Header{"Ratelimit": {`"up";r=5;t=1`}, "Ratelimit-Policy": {`"up";q=10;w=1`}}
	tests := []struct {
		name    string
		recs    []filter.RateLimitField
		status  int
		forward snapshot.Forwarder
		policy  string
		limit   string
		code    string
	}{
		{
			name: "unknown count",
			recs: []filter.RateLimitField{
				{Name: "quota", Q: 3000, W: 2592000},
				{Name: "rl", Q: 10, W: 1, R: 4, T: 1, Known: true},
			},
			forward: handlertest.Respond("orders", http.StatusOK, upstream, ""),
			policy:  `"quota";q=3000;w=2592000, "rl";q=10;w=1`,
			limit:   `"rl";r=4;t=1`,
		},
		{
			name:    "no known count",
			recs:    []filter.RateLimitField{{Name: "quota", Q: 3000, W: 2592000}},
			forward: handlertest.Respond("orders", http.StatusOK, upstream, ""),
			policy:  `"quota";q=3000;w=2592000`,
		},
		{
			name:    "none recorded",
			forward: handlertest.Respond("orders", http.StatusOK, upstream, ""),
			policy:  `"up";q=10;w=1`,
			limit:   `"up";r=5;t=1`,
		},
		{
			name: "quota denial",
			recs: []filter.RateLimitField{
				{Name: "rl", Q: 10, W: 1, R: 4, T: 1, Known: true},
				{Name: "quota", Q: 3000, W: 60, R: 0, T: 30, Known: true, Denied: true},
			},
			status:  http.StatusTooManyRequests,
			forward: ok(),
			policy:  `"rl";q=10;w=1, "quota";q=3000;w=60`,
			limit:   `"quota";r=0;t=30`,
			code:    "RZ-RL-003",
		},
		{
			name: "node error",
			recs: []filter.RateLimitField{{Name: "rl", Q: 10, W: 1, R: 9, T: 1, Known: true}},
			forward: handlertest.ForwardFunc(func(context.Context, *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
				return nil, errUpstream("RZ-UP-005")
			}),
			policy: `"rl";q=10;w=1`,
			limit:  `"rl";r=9;t=1`,
			code:   "RZ-UP-005",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newKit(t)
			k.publish(handlertest.Spec{Routes: []handlertest.Route{route("r", tt.forward,
				handlertest.Policy("limits", phase.ClassAdmission, v1alpha1.PolicyTypeRateLimit, fields(tt.status, tt.recs...), phase.OnRequestHeaders))}})
			res := k.get("/")
			if tt.code != "" {
				decodeProblem(t, res)
			}
			if got := res.Header().Get("RateLimit-Policy"); got != tt.policy {
				t.Errorf("RateLimit-Policy = %q, want %q", got, tt.policy)
			}
			if got := res.Header().Get("RateLimit"); got != tt.limit {
				t.Errorf("RateLimit = %q, want %q", got, tt.limit)
			}
		})
	}
}
