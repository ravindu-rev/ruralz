// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"net/http"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/filtertest"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Phase sets of the two placements the registry selects.
const (
	headersOnly = phase.Set(1 << phase.OnRequestHeaders)
	bodyOnly    = phase.Set(1 << phase.OnRequestBody)
)

// decisions records ruralz_auth_decisions_total.
type decisions struct {
	mu     sync.Mutex
	allows int
	denies []string
}

func (d *decisions) Allow(emit.Stripe) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.allows++
}

func (d *decisions) Deny(_ emit.Stripe, code string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.denies = append(d.denies, code)
}

// counts returns the allows and denies recorded so far.
func (d *decisions) counts() (int, []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.allows, append([]string(nil), d.denies...)
}

// testCaps are the input caps of the tests' snapshot: the default
// Gateway's, over the Route and the Consumers the tests evaluate (vars and
// the silver Consumer of TestDecisionRule).
func testCaps() expr.InputCaps {
	return celtest.Caps(
		[]*expr.Route{{Name: "orders", Labels: map[string]string{"team": "core"}}},
		&expr.Consumer{Name: "acme", Tier: "gold", Quotas: []string{"daily-tokens"}},
		&expr.Consumer{Name: "beta", Tier: "silver"},
	)
}

// buildEnv returns the BuildEnv of an authz.cel Policy named authz-test at
// Route scope with rule, subscribed to phases, compiling through a real
// CEL builder under testCaps, its decisions recorded in rec (nil records
// nothing).
func buildEnv(tb testing.TB, rule string, phases phase.Set, rec *decisions) filter.BuildEnv {
	tb.Helper()
	env := filter.BuildEnv{
		Policy: &v1alpha1.Policy{
			Metadata: v1alpha1.ObjectMeta{Name: "authz-test"},
			Spec:     v1alpha1.PolicySpec{Type: Type},
		},
		Config: &v1alpha1.AuthzCELConfig{Rule: rule},
		Scope:  phase.ScopeRoute,
		Phases: phases,
		CEL:    celtest.Compiler(tb).NewBuilder(nil, testCaps()),
		Site: func(p expr.PlaceID) expr.Site {
			return expr.Site{Place: p, Scope: phase.ScopeRoute, PolicyType: Type}
		},
	}
	if rec != nil {
		env.Metrics = &emit.PolicyMetrics{Auth: rec}
	}
	return env
}

// build compiles rule subscribed to phases, or stops the test.
func build(tb testing.TB, rule string, phases phase.Set, rec *decisions) *Filter {
	tb.Helper()
	f, err := Factory{}.Build(context.Background(), buildEnv(tb, rule, phases, rec))
	if err != nil {
		tb.Fatalf("Build(%q): %v", rule, err)
	}
	return f.(*Filter)
}

// vars returns the activation of an authenticated request: Consumer acme
// of Tier gold with quota daily-tokens, JWT claims with an orders:read
// scope, and a JSON body {"amount": 250, "items": ["a", "b"]}.
func vars(tb testing.TB) *expr.Vars {
	tb.Helper()
	return &expr.Vars{
		Request: &expr.Request{
			Method: "GET", Scheme: "https", Host: "api.shop.example", Path: "/v1/orders/42",
			Header: http.Header{"X-Tenant": {"acme"}},
			Body:   celtest.JSON(tb, `{"amount": 250, "items": ["a", "b"]}`),
		},
		Source:   &expr.Source{IP: netip.MustParseAddr("192.0.2.1"), Port: 44321},
		Route:    celtest.Route("orders", map[string]string{"team": "core"}),
		Consumer: celtest.Consumer(expr.Consumer{Name: "acme", Tier: "gold", Quotas: []string{"daily-tokens"}}),
		Auth:     &expr.Auth{Method: filter.MethodJWT, Claims: celtest.JSON(tb, `{"scope": "orders:read orders:write", "flag": "yes"}`)},
		Now:      time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC),
	}
}

// exchange returns an Exchange over v whose message is msg (a plain
// request message when nil).
func exchange(v *expr.Vars, msg filter.Message) *filtertest.Exchange {
	if msg == nil {
		msg = &filtertest.Message{}
	}
	return &filtertest.Exchange{VarsV: v, Msg: msg}
}

// failingBody is a request message whose decoded body fails with err.
type failingBody struct {
	filtertest.Message
	err error
}

// Decoded returns the injected error.
func (m *failingBody) Decoded(context.Context) (expr.Value, error) { return nil, m.err }
