// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"cel.dev/cel-go/checker"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// newCompiler returns a Compiler or stops the test.
func newCompiler(tb testing.TB) *Compiler {
	tb.Helper()
	c, err := New()
	if err != nil {
		tb.Fatalf("New: %v", err)
	}
	return c
}

// at returns the site of place with no refinement.
func at(place expr.PlaceID) expr.Site { return expr.Site{Place: place} }

// sequential returns a sequential composition site with earlier steps.
func sequential(place expr.PlaceID, earlier ...string) expr.Site {
	return expr.Site{Place: place, Mode: v1alpha1.CompositionModeSequential, EarlierSteps: earlier}
}

// rejectCase is one rejected expression with its first Issue.
type rejectCase struct {
	name string
	site expr.Site
	src  string
	want expr.Issue
}

// TestCheckRejects pins the first Issue of each class of RZ-CFG-014 and
// RZ-CFG-015: code, byte offset, line, code-point column, message and hint.
func TestCheckRejects(t *testing.T) {
	match := expr.PlaceRouteMatchWhen
	matchHint := "Route.spec.match.when has request, source, now"
	cases := []rejectCase{
		{"syntax", at(match), `request.method ==`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 17, Line: 1, Column: 18,
			Message: "CEL Route.spec.match.when: Syntax error: mismatched input '<EOF>' expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}",
		}},
		{"whitespace only", at(match), "  ", expr.Issue{
			Code: "RZ-CFG-014", Offset: 2, Line: 1, Column: 3,
			Message: "CEL Route.spec.match.when: Syntax error: mismatched input '<EOF>' expecting {'[', '{', '(', '.', '-', '!', 'true', 'false', 'null', NUM_FLOAT, NUM_INT, NUM_UINT, STRING, BYTES, IDENTIFIER}",
		}},
		{"unavailable variable", at(match), `response.status == 200`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL Route.spec.match.when: undeclared reference to 'response'", Hint: matchHint,
		}},
		{"position on a later line", at(match), "request.method == \"GET\" &&\n  héllo", expr.Issue{
			Code: "RZ-CFG-014", Offset: 30, Line: 2, Column: 4,
			Message: "CEL Route.spec.match.when: Syntax error: token recognition error at: 'é'",
		}},
		{"undefined field", at(match), `request.bogus == "x"`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 7, Line: 1, Column: 8,
			Message: "CEL Route.spec.match.when: undefined field 'bogus'",
		}},
		{"no overload", at(match), `request.method + 1 == 2`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 15, Line: 1, Column: 16,
			Message: "CEL Route.spec.match.when: found no matching overload for '_+_' applied to '(string, int)'",
		}},
		{"optional syntax is not enabled", at(match), `request.?x == "y"`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 7, Line: 1, Column: 8,
			Message: "CEL Route.spec.match.when: unsupported syntax '.?'",
		}},
		{"bindings are not enabled", at(match), `cel.bind(x, 1, x == 1)`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL Route.spec.match.when: undeclared reference to 'cel'",
		}},
		{"math is not enabled", at(match), `math.greatest(1, 2) == 2`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL Route.spec.match.when: undeclared reference to 'math'",
		}},
		{"duration literal", at(match), `duration("1x") > duration("1s")`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 9, Line: 1, Column: 10,
			Message: "CEL Route.spec.match.when: invalid duration argument",
		}},
		{"timestamp literal", at(match), `now > timestamp("bad")`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 16, Line: 1, Column: 17,
			Message: "CEL Route.spec.match.when: invalid timestamp argument",
		}},
		{"regex literal", at(match), `request.path.matches("(")`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 21, Line: 1, Column: 22,
			Message: "CEL Route.spec.match.when: invalid matches argument",
		}},
		{"request body in a key", at(expr.PlaceRateLimitKey), `request.body.x`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 7, Line: 1, Column: 8,
			Message: "CEL ratelimit config.key: request.body is not available here",
			Hint:    "the request body is read only in body Phases and composition",
		}},
		{"request body presence test", at(expr.PlaceHashKey), `has(request.body) ? "a" : "b"`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 3, Line: 1, Column: 4,
			Message: "CEL Upstream.spec.loadBalancing.hashKey: request.body is not available here",
			Hint:    "the request body is read only in body Phases and composition",
		}},
		{"response body in headers", at(expr.PlaceHeadersResponseValue), `string(response.body.total)`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 15, Line: 1, Column: 16,
			Message: "CEL headers config.response.set[].valueExpression: response.body is not available here",
			Hint:    "the response body is read only by transform.response",
		}},
		{"request body in when without a gate", expr.Site{Place: expr.PlacePolicyWhen}, `request.body.x == 1`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 7, Line: 1, Column: 8,
			Message: "CEL Policy.spec.when: request.body is not available here",
			Hint:    "the request body is read only in body Phases and composition",
		}},
		{"response in a request-Phase when", expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestHeaders}, `response.status == 200`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL Policy.spec.when: undeclared reference to 'response'",
			Hint:    "Policy.spec.when has request, source, route, consumer, auth, now",
		}},
		{"upstream outside Upstream scope", expr.Site{Place: expr.PlaceTransformRequestBody, Scope: phase.ScopeRoute}, `upstream.name`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL transform.request config.body: undeclared reference to 'upstream'",
			Hint:    "transform.request config.body has request, source, route, consumer, auth, now",
		}},
		{"struct construction", at(match), `ruralz.Request{method: "GET"}.method == "GET"`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 14, Line: 1, Column: 15,
			Message: "CEL Route.spec.match.when: ruralz.Request values cannot be constructed",
			Hint:    "the data plane builds the Ruralz views",
		}},
		{"named time zone", at(match), `now.getHours("Europe/Oslo") == 1`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 13, Line: 1, Column: 14,
			Message: `CEL Route.spec.match.when: the time zone must be the literal "UTC" or a UTC offset such as "+02:00"`,
			Hint:    "a named time zone reads zone data at every evaluation",
		}},
		{"computed time zone", at(match), `now.getMinutes(request.headers["tz"]) == 1`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 30, Line: 1, Column: 31,
			Message: `CEL Route.spec.match.when: the time zone must be the literal "UTC" or a UTC offset such as "+02:00"`,
			Hint:    "a named time zone reads zone data at every evaluation",
		}},
		{"offset out of range", at(match), `now.getHours("+24:00") == 1`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 13, Line: 1, Column: 14,
			Message: `CEL Route.spec.match.when: the time zone must be the literal "UTC" or a UTC offset such as "+02:00"`,
			Hint:    "a named time zone reads zone data at every evaluation",
		}},
		{"steps in aggregate", expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeAggregate}, `steps.order.status == 200`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 0, Line: 1, Column: 1,
			Message: "CEL Route.spec.composition.steps[].when: steps is available only in sequential compositions",
			Hint:    `this composition is "aggregate"`,
		}},
		{"steps in conditional", expr.Site{Place: expr.PlaceStepPathExpression, Mode: v1alpha1.CompositionModeConditional}, `"/x/" + string(size(steps))`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 20, Line: 1, Column: 21,
			Message: "CEL Route.spec.composition.steps[].pathExpression: steps is available only in sequential compositions",
			Hint:    `this composition is "conditional"`,
		}},
		{"later step", sequential(expr.PlaceStepWhen, "order"), `steps.later.status == 200`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 5, Line: 1, Column: 6,
			Message: `CEL Route.spec.composition.steps[].when: step "later" is not an earlier step of this composition`,
			Hint:    "steps holds the steps that ran before this one, in list order",
		}},
		{"later step by index", sequential(expr.PlaceStepWhen, "order"), `steps["later"].status == 200`, expr.Issue{
			Code: "RZ-CFG-014", Offset: 5, Line: 1, Column: 6,
			Message: `CEL Route.spec.composition.steps[].when: step "later" is not an earlier step of this composition`,
			Hint:    "steps holds the steps that ran before this one, in list order",
		}},
		{"string in a bool place", at(match), `"x"`, expr.Issue{
			Code: "RZ-CFG-014", Offset: -1,
			Message: "CEL Route.spec.match.when: expression has type string, want bool",
			Hint:    `compare the value, as in x == "y"`,
		}},
		{"int in a string place", at(expr.PlaceRateLimitKey), `source.port`, expr.Issue{
			Code: "RZ-CFG-014", Offset: -1,
			Message: "CEL ratelimit config.key: expression has type int, want string",
			Hint:    "convert it with string(...)",
		}},
		{"nested comprehensions", at(expr.PlaceTransformRequestBody), `request.body.items.all(i, i.tags.all(t, t.tags.all(u, u == "x")))`, expr.Issue{
			Code: "RZ-CFG-015", Offset: -1,
			Message: "CEL transform.request config.body: estimated cost 169123 exceeds 10000 at nominal sizes (strings 256, lists and maps 32)",
			Hint:    "nested comprehensions multiply: iterate over fewer or smaller values",
		}},
		{"nested header comprehensions", at(match), `request.headers.exists(k, request.headers.exists(j, k + j == "x"))`, expr.Issue{
			Code: "RZ-CFG-015", Offset: -1,
			Message: "CEL Route.spec.match.when: estimated cost 60643 exceeds 10000 at nominal sizes (strings 256, lists and maps 32)",
			Hint:    "nested comprehensions multiply: iterate over fewer or smaller values",
		}},
		{"json.encode", at(expr.PlaceTransformRequestBody), `json.encode(request.body)`, expr.Issue{
			Code: "RZ-CFG-015", Offset: -1,
			Message: "CEL transform.request config.body: estimated cost is unbounded at nominal sizes (strings 256, lists and maps 32)",
			Hint:    "json.encode has no cost bound; a transform body may return the map or list itself",
		}},
		{"unknown place", at("Route.spec.bogus"), `true`, expr.Issue{
			Code: "RZ-CFG-014", Offset: -1,
			Message: "CEL Route.spec.bogus: not a CEL place",
		}},
	}
	c := newCompiler(t)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			iss := c.Check(tc.site, tc.src)
			if len(iss) == 0 {
				t.Fatalf("Check(%q) = no Issue, want %+v", tc.src, tc.want)
			}
			if !reflect.DeepEqual(iss[0], tc.want) {
				t.Errorf("Check(%q)[0] =\n  %+v\nwant\n  %+v", tc.src, iss[0], tc.want)
			}
			b := c.NewBuilder(nil)
			if prg, biss := b.Compile(tc.site, tc.src); prg != nil || !reflect.DeepEqual(biss, iss) {
				t.Errorf("Builder.Compile(%q) = %v, %+v; want nil and the Check Issues", tc.src, prg, biss)
			}
		})
	}
}

// TestCheckReportsEveryFinding checks that one expression reports all of its
// site findings, in position order, with the result type and cost after them.
func TestCheckReportsEveryFinding(t *testing.T) {
	c := newCompiler(t)
	iss := c.Check(at(expr.PlaceHashKey), `has(request.body) && now.getHours("Europe/Oslo") == 1`)
	var got []string
	for _, is := range iss {
		got = append(got, is.Code+" "+strings.TrimPrefix(is.Message, "CEL Upstream.spec.loadBalancing.hashKey: "))
	}
	want := []string{
		"RZ-CFG-014 request.body is not available here",
		`RZ-CFG-014 the time zone must be the literal "UTC" or a UTC offset such as "+02:00"`,
		"RZ-CFG-014 expression has type bool, want string",
	}
	if !slices.Equal(got, want) {
		t.Errorf("Issues =\n  %q\nwant\n  %q", got, want)
	}
}

// TestCheckAccepts checks the expressions of the documents' examples and
// the accepted forms of each rule in their sites.
func TestCheckAccepts(t *testing.T) {
	cases := []struct {
		site expr.Site
		src  string
	}{
		{at(expr.PlaceRouteMatchWhen), `request.headers["x-tenant"] == "acme"`},
		{at(expr.PlaceRouteMatchWhen), `true`},
		{at(expr.PlaceRouteMatchWhen), `now.getHours("UTC") < 12 && now.getHours("+02:00") > 1 && now.getHours("-05:30") >= 0`},
		{at(expr.PlacePolicyWhen), `request.method != "OPTIONS"`},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestBody, GatesBody: true}, `request.body.kind == "order"`},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse, PolicyType: v1alpha1.PolicyTypeTransformResponse}, `response.body.total > 10`},
		{expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse}, `response.status >= 400`},
		{at(expr.PlaceAuthzCELRule), `auth.claims.scope.split(" ").exists(s, s == "orders:read")`},
		{at(expr.PlaceAuthzCELRule), `consumer != null && consumer.tier == "gold"`},
		{at(expr.PlaceAuthzCELRule), `consumer != null && "daily-tokens" in consumer.quotas`},
		{at(expr.PlaceAuthzCELRule), `request.body.amount < 1000`},
		{at(expr.PlaceRateLimitKey), `source.ip`},
		{at(expr.PlaceRateLimitKey), `consumer == null ? source.ip : consumer.name`},
		{at(expr.PlaceQuotaKey), `consumer.name`},
		{at(expr.PlaceCacheKey), `consumer.name`},
		{at(expr.PlaceHeadersRequestValue), `consumer == null ? "anonymous" : consumer.name`},
		{at(expr.PlaceHeadersRequestValue), `"x-api-key" in request.headers ? "keyed" : "open"`},
		{at(expr.PlaceHeadersResponseValue), `upstream == null ? "" : upstream.name`},
		{at(expr.PlaceHashKey), `request.headers["x-user"]`},
		{at(expr.PlaceRetryOn), `error != null ? error.kind in ["connect", "reset"] : response.status in [502, 503]`},
		{at(expr.PlaceRetryOn), `error != null ? error.kind in ["connect", "reset"] : response.status == 503`},
		{at(expr.PlaceRetryOn), expr.DefaultRetryOn},
		{at(expr.PlaceFailureWhen), `error != null || response.status >= 500`},
		{at(expr.PlaceFailureWhen), expr.DefaultFailureWhen},
		{at(expr.PlaceAccessLogWhen), `response.status >= 400 || duration > duration("1s")`},
		{at(expr.PlaceAccessLogWhen), `response.status >= 400`},
		{sequential(expr.PlaceStepPathExpression), `"/orders/" + request.pathParams.orderId`},
		{sequential(expr.PlaceStepPathExpression, "order"), `"/stock?order=" + string(steps.order.body.id)`},
		{sequential(expr.PlaceStepWhen, "order"), `has(steps.order) && steps["order"].status == 200 && "order" in steps`},
		{expr.Site{Place: expr.PlaceStepWhen, Mode: v1alpha1.CompositionModeConditional}, `request.body.kind == "x"`},
		{expr.Site{Place: expr.PlaceTransformRequestBody, Scope: phase.ScopeUpstream}, `{"id": request.body.id, "via": upstream.name}`},
		{at(expr.PlaceTransformRequestValue), `string(request.body.id)`},
		{at(expr.PlaceTransformResponseBody), `response.body.items.map(i, i.sku)`},
		{at(expr.PlaceTransformResponseValue), `string(response.body.total)`},
		{at(expr.PlaceSemanticCacheKey), `consumer.name`},
		{at(expr.PlaceCandidateWhen), `ai.estimatedInputTokens < 100000`},
		{at(expr.PlaceMessagingKey), `consumer.name`},
	}
	c := newCompiler(t)
	for _, tc := range cases {
		if iss := c.Check(tc.site, tc.src); len(iss) != 0 {
			t.Errorf("Check(%s, %q) = %+v, want none", tc.site.Place, tc.src, iss)
		}
	}
}

// TestCheckEmptyIsAbsent checks that an empty field is absent everywhere.
func TestCheckEmptyIsAbsent(t *testing.T) {
	c := newCompiler(t)
	if iss := c.Check(at(expr.PlaceRouteMatchWhen), ""); iss != nil {
		t.Errorf("Check(\"\") = %+v, want nil", iss)
	}
	if prg, iss := c.NewBuilder(nil).Compile(at(expr.PlaceRouteMatchWhen), ""); prg != nil || iss != nil {
		t.Errorf("Compile(\"\") = %v, %+v; want nil, nil", prg, iss)
	}
}

// TestCostCeilingBoundary checks that an estimate of exactly 10,000 passes
// and one unit more fails, and that an unbounded estimate fails.
func TestCostCeilingBoundary(t *testing.T) {
	an := &analysis{}
	if _, _, bad := costFinding(checker.CostEstimate{Min: 1, Max: staticCostCeiling}, an); bad {
		t.Error("an estimate at the ceiling failed")
	}
	if _, _, bad := costFinding(checker.CostEstimate{Max: staticCostCeiling + 1}, an); !bad {
		t.Error("an estimate above the ceiling passed")
	}
	msg, _, bad := costFinding(checker.CostEstimate{Max: math.MaxUint64}, an)
	if !bad || !strings.HasPrefix(msg, "estimated cost is unbounded") {
		t.Errorf("unbounded estimate = %q, %v", msg, bad)
	}
}

// TestNominalSizes checks the nominal estimator: a string reached from a
// variable counts 256, a list or map 32, and dyn by its use.
func TestNominalSizes(t *testing.T) {
	c := newCompiler(t)
	cases := []struct {
		site expr.Site
		src  string
		max  uint64
	}{
		// Two lookups of 3 units (variable, field, index) and an equality of
		// two 256-byte strings, ceil(256 * 0.1) = 26.
		{at(expr.PlaceRouteMatchWhen), `request.headers["a"] == request.headers["b"]`, 32},
		// The same comparison and the loop's own units for each of 32 headers.
		{at(expr.PlaceRouteMatchWhen), `request.headers.exists(k, request.headers[k] == request.headers["b"])`, 1187},
		// A dyn range is a 32-entry container; its elements are dyn values
		// compared with a one-character literal.
		{at(expr.PlaceAuthzCELRule), `request.body.exists(x, x == 1)`, 195},
		// Types and objects have size 1, as at runtime: comparing them is
		// not unbounded.
		{at(expr.PlaceAuthzCELRule), `type(request.body.a) == int`, 5},
		{at(expr.PlaceAuthzCELRule), `consumer == consumer`, 3},
	}
	for _, tc := range cases {
		cc, iss := c.compile(tc.site, tc.src)
		if cc == nil {
			t.Errorf("compile(%q) = %+v", tc.src, iss)
			continue
		}
		if cc.cost.Max != tc.max {
			t.Errorf("cost(%q).Max = %d, want %d", tc.src, cc.cost.Max, tc.max)
		}
	}
}

// TestDiagnosticsAreDeterministic compiles the rejected corpus forward and
// backward on fresh Compilers and expects identical Issues.
func TestDiagnosticsAreDeterministic(t *testing.T) {
	corpus := []rejectCase{
		{site: at(expr.PlaceRouteMatchWhen), src: `cel.bind(x, 1, x == 1) && response.status == 1`},
		{site: at(expr.PlaceHashKey), src: `has(request.body) && now.getHours("Europe/Oslo") == 1`},
		{site: sequential(expr.PlaceStepWhen, "a"), src: `steps.z.status == 1 && steps.y.status == 2 && steps.a.status == 3`},
		{site: at(expr.PlaceTransformRequestBody), src: `request.body.items.all(i, i.tags.all(t, t.tags.all(u, u == "x")))`},
	}
	run := func(order []int) [][]expr.Issue {
		c := newCompiler(t)
		out := make([][]expr.Issue, len(corpus))
		for _, i := range order {
			out[i] = c.Check(corpus[i].site, corpus[i].src)
		}
		return out
	}
	forward, backward := []int{0, 1, 2, 3}, []int{3, 2, 1, 0}
	a, b := run(forward), run(backward)
	if !reflect.DeepEqual(a, b) {
		t.Errorf("Issues differ by order:\n%+v\n%+v", a, b)
	}
	if steps := a[2]; len(steps) != 2 || steps[0].Offset > steps[1].Offset {
		t.Errorf("step findings = %+v, want two in position order", steps)
	}
}
