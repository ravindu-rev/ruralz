// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel/celtest"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// outcome is what one decision looks like from outside: admitted, denied
// with RZ-AUTH-010, or undecided.
type outcome uint8

const (
	admitted outcome = iota
	denied
	undecided
)

// outcomeOf classifies r.
func outcomeOf(r filter.Result) outcome {
	switch r.Outcome {
	case filter.Continue:
		return admitted
	case filter.Respond:
		return denied
	default:
		return undecided
	}
}

// TestDecisionRule checks the decision of a rule over the base variables:
// true admits, false denies with 403 RZ-AUTH-010 and a runtime error
// cannot decide with RZ-AUTH-015, each counted in
// ruralz_auth_decisions_total with its code. The rules include the two of
// the documents (authz-gold-only and authz-orders).
func TestDecisionRule(t *testing.T) {
	silver := func(v *expr.Vars) { v.Consumer = celtest.Consumer(expr.Consumer{Name: "beta", Tier: "silver"}) }
	anonymous := func(v *expr.Vars) { v.Consumer, v.Auth = nil, nil }
	writeOnly := func(v *expr.Vars) {
		v.Auth = &expr.Auth{Method: filter.MethodJWT, Claims: celtest.JSON(t, `{"scope": "orders:write"}`)}
	}
	tests := []struct {
		name string
		rule string
		edit func(*expr.Vars)
		want outcome
	}{
		{"gold Tier admitted", `consumer != null && consumer.tier == "gold"`, nil, admitted},
		{"silver Tier denied", `consumer != null && consumer.tier == "gold"`, silver, denied},
		{"anonymous denied by a null check", `consumer != null && consumer.tier == "gold"`, anonymous, denied},
		{"scope present admitted", `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, nil, admitted},
		{"scope missing denied", `auth.claims.scope.split(" ").exists(s, s == "orders:read")`, writeOnly, denied},
		{"quota name admitted", `consumer != null && "daily-tokens" in consumer.quotas`, nil, admitted},
		{"method and path", `request.method == "GET" && request.path.startsWith("/v1/orders/")`, nil, admitted},
		{"route label", `route.labels.team == "core"`, nil, admitted},
		{"source address", `source.ip == "192.0.2.1"`, nil, admitted},
		{"false constant", `false`, nil, denied},
		{"null consumer selected", `consumer.tier == "gold"`, anonymous, undecided},
		{"null auth selected", `auth.claims.scope == "x"`, anonymous, undecided},
		{"missing claim", `auth.claims.nope == "x"`, nil, undecided},
		{"division by zero", `1 / (size(request.path) - size(request.path)) == 1`, nil, undecided},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			f := build(t, tt.rule, headersOnly, rec)
			v := vars(t)
			if tt.edit != nil {
				tt.edit(v)
			}
			r := f.Handle(t.Context(), phase.OnRequestHeaders, exchange(v, nil))
			if got := outcomeOf(r); got != tt.want {
				t.Fatalf("Handle(%q) = %+v, want outcome %d", tt.rule, r, tt.want)
			}
			allows, denies := rec.counts()
			switch tt.want {
			case admitted:
				if allows != 1 || len(denies) != 0 {
					t.Errorf("admit recorded %d allows, denies %v; want one allow", allows, denies)
				}
			case denied:
				if allows != 0 || !slices.Equal(denies, []string{CodeDenied}) {
					t.Errorf("deny recorded %d allows, denies %v; want one %s", allows, denies, CodeDenied)
				}
			case undecided:
				if r.Code != "RZ-AUTH-015" || allows != 0 || !slices.Equal(denies, []string{"RZ-AUTH-015"}) {
					t.Errorf("undecided result code %q, recorded %d allows, denies %v; want RZ-AUTH-015 counted once", r.Code, allows, denies)
				}
			}
		})
	}
}

// TestDenyIs403 checks the deny answer: a 403 problem document carrying
// RZ-AUTH-010 with no header field, body or detail, so nothing of the
// request is echoed.
func TestDenyIs403(t *testing.T) {
	f := build(t, `request.headers["x-tenant"] == "other"`, headersOnly, nil)
	r := f.Handle(t.Context(), phase.OnRequestHeaders, exchange(vars(t), nil))
	if r.Outcome != filter.Respond || r.Response == nil {
		t.Fatalf("Handle = %+v, want Respond", r)
	}
	if resp := r.Response; resp.Status != 403 || resp.Code != CodeDenied || len(resp.Header) != 0 || resp.Body != nil || resp.Detail != "" {
		t.Errorf("response = %+v, want 403 %s without header, body or detail", resp, CodeDenied)
	}
	if s := errcode.Status(CodeDenied); s != 403 {
		t.Errorf("errcode.Status(%s) = %d, want 403", CodeDenied, s)
	}
}

// TestRuntimeErrorFailsClosed checks that a runtime error cannot decide
// with the authz class code RZ-AUTH-015 and a cause the executor
// classifies: cel_error, or result_type for a dyn result that is not a
// bool. The cause wraps the *expr.EvalError, whose text holds no request
// data.
func TestRuntimeErrorFailsClosed(t *testing.T) {
	tests := []struct {
		name, rule, errType string
		kind                expr.ErrorKind
	}{
		{"null selection", `consumer.tier == "gold" && auth.claims.missing == 1`, errTypeCEL, expr.KindNoSuchKey},
		{"dyn result not a bool", `auth.claims.flag`, errTypeResultType, expr.KindResultType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := build(t, tt.rule, headersOnly, nil)
			r := f.Handle(t.Context(), phase.OnRequestHeaders, exchange(vars(t), nil))
			if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || r.Response != nil {
				t.Fatalf("Handle = %+v, want CannotDecide with RZ-AUTH-015", r)
			}
			var typed interface{ ErrorType() string }
			if !errors.As(r.Err, &typed) || typed.ErrorType() != tt.errType {
				t.Errorf("cause %v names error.type %v, want %s", r.Err, typed, tt.errType)
			}
			var ee *expr.EvalError
			if !errors.As(r.Err, &ee) || ee.Kind != tt.kind {
				t.Errorf("cause %v, want an *expr.EvalError of kind %s", r.Err, tt.kind)
			}
			if strings.Contains(r.Err.Error(), "yes") || strings.Contains(r.Err.Error(), "acme") {
				t.Errorf("cause text %q holds request data", r.Err)
			}
		})
	}
}

// TestStopsAreRuntimeErrors checks that both stops of a rule fail closed
// with RZ-AUTH-015: a tracked comprehension, over the request body, which
// no input cap bounds, evaluated under an ended request context (the
// deadline stop), and a rule that the caps leave able to pass the runtime
// limit, two header values compared by contains, whose actual cost passes
// it with the header block within its 64 KiB cap.
func TestStopsAreRuntimeErrors(t *testing.T) {
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	items := build(t, `request.body.items.exists(i, i == "z")`, bodyOnly, nil)
	v := vars(t)
	v.Request.Body = celtest.JSON(t, `{"items": [`+strings.TrimSuffix(strings.Repeat(`"a",`, 1000), ",")+`]}`)
	r := items.Handle(ended, phase.OnRequestBody, exchange(v, nil))
	var ee *expr.EvalError
	if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || !errors.As(r.Err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("comprehension under an ended context = %+v, want RZ-AUTH-015 with a deadline error", r)
	}

	costly := build(t, `request.headers["a"].contains(request.headers["b"])`, headersOnly, nil)
	v = vars(t)
	v.Request.Header = http.Header{"A": {strings.Repeat("a", 32<<10)}, "B": {strings.Repeat("b", 32<<10)}}
	r = costly.Handle(t.Context(), phase.OnRequestHeaders, exchange(v, nil))
	if r.Outcome != filter.CannotDecide || r.Code != "RZ-AUTH-015" || !errors.As(r.Err, &ee) || ee.Kind != expr.KindCostLimit {
		t.Errorf("rule past the cost limit = %+v, want RZ-AUTH-015 with a cost-limit error", r)
	}
}

// TestRuleWithinTheCapsDecidesUnstopped checks the tracking rule of the
// snapshot's input caps: the authz-orders rule, whose cost with claims of
// at most 4 × 16 KiB stays within the runtime limit, runs untracked and
// without a comprehension stop, so it decides even under an ended request
// context; built with every input unbounded, the same rule is tracked and
// the ended context stops it.
func TestRuleWithinTheCapsDecidesUnstopped(t *testing.T) {
	const rule = `auth.claims.scope.split(" ").exists(s, s == "orders:read")`
	ended, cancel := context.WithCancel(t.Context())
	cancel()
	v := vars(t)
	v.Auth = &expr.Auth{Method: filter.MethodJWT, Claims: celtest.JSON(t, `{"scope": "`+strings.Repeat("x ", 1000)+`orders:read"}`)}
	if r := build(t, rule, headersOnly, nil).Handle(ended, phase.OnRequestHeaders, exchange(v, nil)); r.Outcome != filter.Continue {
		t.Errorf("rule within the caps under an ended context = %+v, want Continue", r)
	}
	env := buildEnv(t, rule, headersOnly, nil)
	env.CEL = celtest.Compiler(t).NewBuilder(nil, expr.InputCaps{})
	f, err := Factory{}.Build(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	r := f.Handle(ended, phase.OnRequestHeaders, exchange(v, nil))
	var ee *expr.EvalError
	if r.Outcome != filter.CannotDecide || !errors.As(r.Err, &ee) || ee.Kind != expr.KindDeadline {
		t.Errorf("rule without caps under an ended context = %+v, want a deadline error", r)
	}
}

// TestDecidesOnlyInItsPhase checks the Phase rule: a rule that does not
// read request.body decides in onRequestHeaders, a body rule in
// onRequestBody, and in every other Phase the Filter continues without
// evaluating or counting anything.
func TestDecidesOnlyInItsPhase(t *testing.T) {
	tests := []struct {
		name   string
		rule   string
		phases phase.Set
		decide phase.Phase
	}{
		{"header rule", `false`, headersOnly, phase.OnRequestHeaders},
		{"body rule", `request.body.amount > 1000`, bodyOnly, phase.OnRequestBody},
		{"header rule placed in onRequestBody", `false`, bodyOnly, phase.OnRequestBody},
		{"both Phases decide once, in onRequestBody", `false`, headersOnly | bodyOnly, phase.OnRequestBody},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			f := build(t, tt.rule, tt.phases, rec)
			for p := range phase.Count {
				r := f.Handle(t.Context(), p, exchange(vars(t), nil))
				switch {
				case p == tt.decide && r.Outcome != filter.Respond:
					t.Errorf("Handle(%s) = %+v, want the deny", p, r)
				case p != tt.decide && r.Outcome != filter.Continue:
					t.Errorf("Handle(%s) = %+v, want Continue outside %s", p, r, tt.decide)
				}
			}
			if allows, denies := rec.counts(); allows != 0 || len(denies) != 1 {
				t.Errorf("recorded %d allows, denies %v; want exactly one decision", allows, denies)
			}
		})
	}
}

// TestBodyRule checks a rule over request.body in onRequestBody: the
// decoded JSON body decides, a body that is not JSON reads as null, and a
// body CEL cannot read fails closed with RZ-AUTH-015.
func TestBodyRule(t *testing.T) {
	tests := []struct {
		name string
		rule string
		body func(tb testing.TB) expr.Value
		want outcome
	}{
		{"small amount admitted", `request.body.amount < 1000`, func(tb testing.TB) expr.Value { return celtest.JSON(tb, `{"amount": 10}`) }, admitted},
		{"large amount denied", `request.body.amount < 1000`, func(tb testing.TB) expr.Value { return celtest.JSON(tb, `{"amount": 5000}`) }, denied},
		{"list in the body", `request.body.items.all(i, i != "")`, func(tb testing.TB) expr.Value { return celtest.JSON(tb, `{"items": ["a"]}`) }, admitted},
		{"not JSON reads as null", `request.body == null || request.body.amount < 1000`, func(tb testing.TB) expr.Value { return celtest.JSON(tb, `null`) }, admitted},
		{"null body selected", `request.body.amount < 1000`, func(tb testing.TB) expr.Value { return celtest.JSON(tb, `null`) }, undecided},
		{"body not available", `request.body.amount < 1000`, func(testing.TB) expr.Value { return nil }, undecided},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &decisions{}
			f := build(t, tt.rule, bodyOnly, rec)
			if !f.body || f.decide != phase.OnRequestBody {
				t.Fatalf("Filter body %v, Phase %s; want a body rule in onRequestBody", f.body, f.decide)
			}
			v := vars(t)
			v.Request.Body = tt.body(t)
			r := f.Handle(t.Context(), phase.OnRequestBody, exchange(v, nil))
			if got := outcomeOf(r); got != tt.want {
				t.Errorf("Handle = %+v, want outcome %d", r, tt.want)
			}
			if tt.want == undecided && r.Code != "RZ-AUTH-015" {
				t.Errorf("undecided code %q, want RZ-AUTH-015", r.Code)
			}
		})
	}
}

// TestBodyGateFailures checks the gate rule of a body rule: a decoded
// body over its cap (filter.ErrTooLarge, 413 RZ-RT-003) or a refused
// buffer reservation (filter.ErrBudget, 503 RZ-RT-004) cannot decide with
// that sentinel and no code of its own, before the rule runs, and is no
// authorization decision. A header rule never reads the body.
func TestBodyGateFailures(t *testing.T) {
	for _, sentinel := range []error{filter.ErrTooLarge, filter.ErrBudget} {
		rec := &decisions{}
		f := build(t, `request.body.amount < 1000`, bodyOnly, rec)
		msg := &failingBody{err: sentinel}
		r := f.Handle(t.Context(), phase.OnRequestBody, exchange(vars(t), msg))
		if r.Outcome != filter.CannotDecide || r.Code != "" || !errors.Is(r.Err, sentinel) {
			t.Errorf("Handle with %v = %+v, want CannotDecide with the sentinel and no code", sentinel, r)
		}
		if allows, denies := rec.counts(); allows != 0 || len(denies) != 0 {
			t.Errorf("gate failure %v recorded %d allows, denies %v; want none", sentinel, allows, denies)
		}
	}
	// Another error of the message leaves the decision to the rule.
	f := build(t, `request.body == null`, bodyOnly, nil)
	r := f.Handle(t.Context(), phase.OnRequestBody, exchange(vars(t), &failingBody{err: filter.ErrNotAvailable}))
	if r.Outcome != filter.Respond {
		t.Errorf("Handle with ErrNotAvailable = %+v, want the rule's deny", r)
	}
	// A header rule ignores the message's body.
	f = build(t, `true`, headersOnly, nil)
	if r := f.Handle(t.Context(), phase.OnRequestHeaders, exchange(vars(t), &failingBody{err: filter.ErrTooLarge})); r.Outcome != filter.Continue {
		t.Errorf("header rule with a failing body = %+v, want Continue", r)
	}
	// Without a message the rule still decides.
	f = build(t, `request.body.amount < 1000`, bodyOnly, nil)
	x := exchange(vars(t), nil)
	x.Msg = nil
	if r := f.Handle(t.Context(), phase.OnRequestBody, x); r.Outcome != filter.Continue {
		t.Errorf("body rule without a message = %+v, want Continue", r)
	}
}

// TestBuildRejectsInvalidConfig checks every Build refusal: a config of
// another type or an empty rule (RZ-CFG-005); a syntax, type or
// unavailable-variable error (RZ-CFG-014) and an estimate over the cost
// ceiling (RZ-CFG-015), as the compiler reports them; a body rule placed
// without onRequestBody (RZ-CFG-014); and a placement where the rule never
// runs (RZ-CFG-020).
func TestBuildRejectsInvalidConfig(t *testing.T) {
	tests := []struct {
		name string
		edit func(env *filter.BuildEnv)
		code string
	}{
		{"nil config", func(env *filter.BuildEnv) { env.Config = nil }, CodeConfig},
		{"another config type", func(env *filter.BuildEnv) { env.Config = &v1alpha1.AuthzIPConfig{} }, CodeConfig},
		{"nil authz.cel config", func(env *filter.BuildEnv) { env.Config = (*v1alpha1.AuthzCELConfig)(nil) }, CodeConfig},
		{"empty rule", rule(``), CodeConfig},
		{"syntax error", rule(`consumer.tier ==`), CodeCEL},
		{"string result", rule(`consumer.name`), CodeCEL},
		{"response is not a base variable", rule(`response.status == 200`), CodeCEL},
		{"upstream is not a base variable", rule(`upstream.name == "orders"`), CodeCEL},
		{"cost ceiling", rule(`request.headers.exists(k, request.headers.exists(j, k + j == "x"))`), CodeCost},
		{"body rule in onRequestHeaders", rule(`request.body.amount < 1000`), CodeCEL},
		{"Upstream scope", func(env *filter.BuildEnv) { env.Scope = phase.ScopeUpstream }, CodeScope},
		{"upstream-leg Phases", func(env *filter.BuildEnv) { env.Phases = phase.Of(phase.OnUpstreamRequest) }, CodeScope},
		{"no Phase", func(env *filter.BuildEnv) { env.Phases = 0 }, CodeScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := buildEnv(t, `true`, headersOnly, nil)
			tt.edit(&env)
			f, err := Factory{}.Build(t.Context(), env)
			if code, ok := errcode.CodeOf(err); f != nil || !ok || code != tt.code {
				t.Errorf("Build = %v, %v; want an %s error", f, err, tt.code)
			}
		})
	}
}

// rule returns a BuildEnv edit that sets config.rule.
func rule(src string) func(env *filter.BuildEnv) {
	return func(env *filter.BuildEnv) { env.Config = &v1alpha1.AuthzCELConfig{Rule: src} }
}

// TestBuildPreconditions checks the refusals without a code: an ended
// context returns the context's error and a Build without a CEL builder
// ErrNoCEL.
func TestBuildPreconditions(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := Factory{}.Build(ctx, buildEnv(t, `true`, headersOnly, nil))
	if _, coded := errcode.CodeOf(err); !errors.Is(err, context.Canceled) || coded {
		t.Errorf("Build with a canceled context = %v, want context.Canceled without a code", err)
	}
	env := buildEnv(t, `true`, headersOnly, nil)
	env.CEL = nil
	if _, err := (Factory{}).Build(t.Context(), env); !errors.Is(err, ErrNoCEL) {
		t.Errorf("Build without a CEL builder = %v, want ErrNoCEL", err)
	}
}

// TestBuildCompilesAtTheSite checks that Build compiles the rule at the
// site BuildEnv.Site refines for authz.cel config.rule, and at the place
// refined by scope and type when Site is unset.
func TestBuildCompilesAtTheSite(t *testing.T) {
	env := buildEnv(t, `true`, headersOnly, nil)
	var asked []expr.PlaceID
	env.Site = func(p expr.PlaceID) expr.Site {
		asked = append(asked, p)
		return expr.Site{Place: p, Scope: phase.ScopeRoute, PolicyType: Type}
	}
	if _, err := (Factory{}).Build(t.Context(), env); err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !slices.Equal(asked, []expr.PlaceID{expr.PlaceAuthzCELRule}) {
		t.Errorf("Site asked for %v, want [%s]", asked, expr.PlaceAuthzCELRule)
	}
	// A site of another place would refuse a bool rule: Build uses it.
	env.Site = func(expr.PlaceID) expr.Site { return expr.Site{Place: expr.PlaceRateLimitKey} }
	if _, err := (Factory{}).Build(t.Context(), env); err == nil {
		t.Error("Build ignored the site BuildEnv.Site returned")
	}
	env.Site = nil
	if _, err := (Factory{}).Build(t.Context(), env); err != nil {
		t.Errorf("Build without Site = %v, want a Filter", err)
	}
}

// TestConcurrentHandle evaluates one Filter from many goroutines, each
// with its own exchange, under the race detector.
func TestConcurrentHandle(t *testing.T) {
	rec := &decisions{}
	f := build(t, `auth.claims.scope.split(" ").exists(s, s == "orders:read") && request.body.amount < 1000`, bodyOnly, rec)
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			x := exchange(vars(t), nil)
			for range 50 {
				if r := f.Handle(context.Background(), phase.OnRequestBody, x); r.Outcome != filter.Continue {
					t.Errorf("Handle = %+v, want Continue", r)
					return
				}
			}
		})
	}
	wg.Wait()
	if allows, denies := rec.counts(); allows != 16*50 || len(denies) != 0 {
		t.Errorf("recorded %d allows, denies %v; want %d allows", allows, denies, 16*50)
	}
}
