// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// noResponse is the message and hint of a Route match.when that reads
// response, which Route matching does not declare.
const noResponse = "CEL Route.spec.match.when: undeclared reference to 'response' (Route.spec.match.when has request, source, now)"

// TestStageJPositions covers where a CEL finding sits: at the character it
// names, escapes counted, in a plain scalar, over two lines too, a single-
// or double-quoted scalar on one line, a tagged scalar and a JSON string;
// at the field with "at L:C" ending its message in a block scalar or a
// quoted scalar over two lines; at the field, message unchanged, for an
// issue without a position.
func TestStageJPositions(t *testing.T) {
	tests := []struct {
		name, file, doc, want string
	}{
		{"plain", "r.yaml", routeWhen("r", "orders", "    when: request.method == response.status\n", ""),
			"r.yaml:8:29 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"double-quoted", "r.yaml", routeWhen("r", "orders", "    when: \"request.method == \\\"GET\\\" && response.status == 1\"\n", ""),
			"r.yaml:8:41 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"double-quoted unicode escape", "r.yaml", routeWhen("r", "orders", "    when: \"request.method == \\\"G\\u00c9T\\\" && response.status == 1\"\n", ""),
			"r.yaml:8:46 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"single-quoted", "r.yaml", routeWhen("r", "orders", "    when: 'request.method == ''GET'' && response.status == 1'\n", ""),
			"r.yaml:8:41 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"tagged", "r.yaml", routeWhen("r", "orders", "    when: !!str request.method == response.status\n", ""),
			"r.yaml:8:35 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"JSON string", "r.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {"match": {"path": {"prefix": "/r"}, "when": "request.method == \"G` +
			`\` + `u00c9T\" && response.status == 1"}, "upstreams": [{"name": "orders"}]}}`,
			"r.json:1:167 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"block scalar", "r.yaml", routeWhen("r", "orders", "    when: |\n      request.method == \"GET\" &&\n      response.status == 1\n", ""),
			"r.yaml:8:11 error RZ-CFG-014 Route/r spec.match.when: CEL Route.spec.match.when: undeclared reference to 'response' at 2:1 (Route.spec.match.when has request, source, now)"},
		{"plain over two lines", "r.yaml", routeWhen("r", "orders", "    when: request.method ==\n      response.status\n", ""),
			"r.yaml:9:7 error RZ-CFG-014 Route/r spec.match.when: " + noResponse},
		{"double-quoted over two lines", "r.yaml", routeWhen("r", "orders", "    when: \"request.method == \\\"GET\\\" &&\n      response.status == 1\"\n", ""),
			"r.yaml:8:11 error RZ-CFG-014 Route/r spec.match.when: CEL Route.spec.match.when: undeclared reference to 'response' at 1:28 (Route.spec.match.when has request, source, now)"},
		{"issue without a position", "r.yaml", routeWhen("r", "orders", "    when: request.method\n", ""),
			"r.yaml:8:11 error RZ-CFG-014 Route/r spec.match.when: CEL Route.spec.match.when: expression has type string, want bool (compare the value, as in x == \"y\")"},
	}
	p := newPipeline(t, Options{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, p, bundle(map[string]string{"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"), tc.file: tc.doc}), Request{})
			wantTexts(t, tc.name, res.Diagnostics, tc.want)
		})
	}
}

// TestStageJCost covers RZ-CFG-015: an expression over the cost ceiling,
// an issue without a position, at the field.
func TestStageJCost(t *testing.T) {
	res := run(t, newPipeline(t, Options{}), bundle(map[string]string{
		"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"),
		"r.yaml": routeWhen("r", "orders", "    when: 'request.headers.all(a, request.headers.all(b, request.headers.all(c, a + b + c != \"\")))'\n", ""),
	}), Request{})
	wantTexts(t, "cost", res.Diagnostics,
		"r.yaml:8:11 error RZ-CFG-015 Route/r spec.match.when: CEL Route.spec.match.when: estimated cost 4430019 exceeds 10000 at nominal sizes (strings 256, lists and maps 32) (nested comprehensions multiply: iterate over fewer or smaller values)")
}

// ratelimitWith returns a ratelimit Policy whose spec holds the YAML of
// extra before its config.
func ratelimitWith(name, extra string) string {
	return policyDoc(name, "ratelimit", extra+"  config:\n    limits:\n      - requests: 10\n        window: 1s\n")
}

// TestStageJPerAttachment covers a Policy some Route's chain runs: its
// fields compile once per site of its attachments, one diagnostic reports
// each distinct issue however many attachments raise it, and its related
// locations are the attaching PolicyRefs whose site yields the issue, in
// canonical order.
func TestStageJPerAttachment(t *testing.T) {
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc + "  policies:\n    - name: tr\n    - name: rl\n",
		"u.yaml":      docs(upstreamDoc("orders"), upstreamDoc("inv")+"  policies:\n    - name: tr\n"),
		"p.yaml": docs(
			policyDoc("tr", "transform.request", "  config:\n    set:\n      - target: header\n        name: x-up\n        valueExpression: upstream.name\n"),
			ratelimitWith("rl", "  when: response.status == 200\n"),
		),
		"r.yaml": docs(
			routeDoc("a", "orders", ""),
			routeDoc("b", "inv", ""),
			routeDoc("c", "orders", "  policies:\n    - name: rl\n"),
			routeDoc("d", "orders", "  policies:\n    - name: rl\n"),
		),
	}), Request{})
	wantTexts(t, "run", res.Diagnostics,
		"p.yaml:11:26 error RZ-CFG-014 Policy/tr spec.config.set[0].valueExpression: CEL transform.request config.set[].valueExpression: undeclared reference to 'upstream' (transform.request config.set[].valueExpression has request, source, route, consumer, auth, now) (attached at ruralz.yaml:11:7)",
		"p.yaml:19:9 error RZ-CFG-014 Policy/rl spec.when: CEL Policy.spec.when: undeclared reference to 'response' (Policy.spec.when has request, source, route, consumer, auth, now) (attached at ruralz.yaml:12:7) (attached at r.yaml:31:7) (attached at r.yaml:43:7)",
	)
}

// TestStageJEveryScope covers a Policy no Route's chain runs, because
// nothing attaches it or because it subscribes to no Phase where it is
// attached: its fields compile at every scope its type allows, with the
// first Phase it would run in there (the first of its type's Phases when
// its config subscribes it to none), and a finding is reported only when
// every one of those sites raises it, without related locations. A field
// that some scope accepts, as upstream at Upstream scope, is never
// refused.
func TestStageJEveryScope(t *testing.T) {
	res := run(t, newPipeline(t, Options{}), bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"u.yaml":      upstreamDoc("orders"),
		"p.yaml": docs(
			policyDoc("loose", "transform.request", "  config:\n    set:\n      - target: header\n        name: x-up\n        valueExpression: upstream.name\n"),
			ratelimitWith("never", "  when: response.status == 200\n"),
			policyDoc("idle", "headers", "  when: response.status == 200\n  config: {}\n"),
			policyDoc("idle-request", "transform.request", "  when: request.body.x == 1\n  config: {}\n"),
		),
		"r.yaml": routeDoc("a", "orders", "  policies:\n    - name: idle\n    - name: idle-request\n"),
	}), Request{})
	wantTexts(t, "run", res.Diagnostics,
		"p.yaml:19:9 error RZ-CFG-014 Policy/never spec.when: CEL Policy.spec.when: undeclared reference to 'response' (Policy.spec.when has request, source, route, consumer, auth, now)",
		"p.yaml:31:9 error RZ-CFG-014 Policy/idle spec.when: CEL Policy.spec.when: undeclared reference to 'response' (Policy.spec.when has request, source, route, consumer, auth, now)",
		"p.yaml:40:16 error RZ-CFG-014 Policy/idle-request spec.when: CEL Policy.spec.when: request.body is not available here (the request body is read only in body Phases and composition)",
	)
	if res.Validated != nil {
		t.Error("a run with findings has a Revision")
	}
}

// TestStageJRequestBodyGates covers which Policy.spec.when may read
// request.body: that of a Policy that gates the request body at its
// attachment (validation.json-schema, and transform.request whose config
// needs the body, at Route and at Upstream scope), and no other
// (ratelimit, and transform.request whose config does not need the body).
func TestStageJRequestBodyGates(t *testing.T) {
	const when = "  when: request.body.x == 1\n"
	tests := []struct {
		name, policy, upstream string
		refused                bool
	}{
		{"validation.json-schema", policyDoc("p", "validation.json-schema", when+"  config:\n    schema: {type: object}\n"), "", false},
		{"ratelimit", ratelimitWith("p", when), "", true},
		{"transform.request without a body need", policyDoc("p", "transform.request", when+"  config:\n    set:\n      - {target: header, name: x-a, valueExpression: '\"b\"'}\n"), "", true},
		{"transform.request with a body target", policyDoc("p", "transform.request", when+"  config:\n    set:\n      - {target: body, name: a, valueExpression: '\"b\"'}\n"), "", false},
		{"transform.request reading the body", policyDoc("p", "transform.request", when+"  config:\n    set:\n      - {target: header, name: x-a, valueExpression: 'string(request.body.id)'}\n"), "", false},
		{"transform.request with a body need at Upstream scope", policyDoc("p", "transform.request", when+"  config:\n    body: request.body\n"), "  policies:\n    - name: p\n", false},
	}
	p := newPipeline(t, Options{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			attach := "  policies:\n    - name: p\n"
			if tc.upstream != "" {
				attach = ""
			}
			res := run(t, p, bundle(map[string]string{
				"ruralz.yaml": gatewayDoc,
				"u.yaml":      upstreamDoc("orders") + tc.upstream,
				"p.yaml":      tc.policy,
				"r.yaml":      routeDoc("a", "orders", attach),
			}), Request{})
			refused := len(withCode(res.Diagnostics, "RZ-CFG-014")) > 0
			if refused != tc.refused || len(res.Diagnostics) > 1 {
				t.Errorf("diagnostics %q; want the when refused: %v", texts(res.Diagnostics), tc.refused)
			}
		})
	}
}

// TestStageJResponseByFirstPhase covers Policy.spec.when declaring
// response only when the Policy's first Phase at its attachment is a
// response Phase: a headers Policy with response operations only, but not
// one that edits the request first.
func TestStageJResponseByFirstPhase(t *testing.T) {
	const when = "  when: response.status == 200\n"
	p := newPipeline(t, Options{})
	for _, tc := range []struct {
		name, config string
		refused      bool
	}{
		{"response operations only", "    response:\n      set: [{name: x-a, value: b}]\n", false},
		{"request operations first", "    request:\n      set: [{name: x-a, value: b}]\n    response:\n      set: [{name: x-b, value: c}]\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, p, bundle(map[string]string{
				"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"),
				"p.yaml": policyDoc("h", "headers", when+"  config:\n"+tc.config),
				"r.yaml": routeDoc("a", "orders", "  policies:\n    - name: h\n"),
			}), Request{})
			if refused := len(res.Diagnostics) > 0; refused != tc.refused {
				t.Errorf("diagnostics %q; want the when refused: %v", texts(res.Diagnostics), tc.refused)
			}
		})
	}
}

// compositionRoute returns a Route of mode with three steps, step one
// to three, whose when expressions are whens.
func compositionRoute(mode string, whens ...string) string {
	s := "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: comp\nspec:\n  match:\n    path: {prefix: /comp}\n  composition:\n    mode: " + mode + "\n    steps:\n"
	for i, w := range whens {
		name := []string{"one", "two", "three"}[i]
		s += "      - name: " + name + "\n        upstream: orders\n        path: /" + name + "\n"
		if w != "" {
			s += "        when: '" + w + "'\n"
		}
	}
	return s
}

// TestStageJCompositionSites covers the composition fields: steps is read
// only in a sequential composition, and a constant key names an earlier
// step; hub.Validated.CEL carries each step's mode and earlier steps.
func TestStageJCompositionSites(t *testing.T) {
	p := newPipeline(t, Options{})
	files := func(route string) map[string]string {
		return map[string]string{"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"), "r.yaml": route}
	}
	ok := run(t, p, bundle(files(compositionRoute("sequential", "", "steps.one.status == 200", "steps.two.status == 200 && steps.one.status == 200"))), Request{})
	wantClean(t, "sequential", ok.Diagnostics)
	if ok.Validated == nil || len(ok.Validated.CEL) != 2 {
		t.Fatalf("CEL uses %+v", ok.Validated)
	}
	if s := ok.Validated.CEL[1].Site; s.Place != expr.PlaceStepWhen || s.Mode != v1alpha1.CompositionModeSequential || !slices.Equal(s.EarlierSteps, []string{"one", "two"}) {
		t.Errorf("site of step three = %+v", s)
	}
	later := run(t, p, bundle(files(compositionRoute("sequential", "steps.two.status == 200", "", ""))), Request{})
	if got := codes(later.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-014"}) {
		t.Errorf("a step reading a later step: %q, want RZ-CFG-014", texts(later.Diagnostics))
	}
	conditional := run(t, p, bundle(files(compositionRoute("conditional", "", "steps.one.status == 200", ""))), Request{})
	if got := codes(conditional.Diagnostics); !slices.Equal(got, []string{"RZ-CFG-014"}) {
		t.Errorf("a conditional step reading steps: %q, want RZ-CFG-014", texts(conditional.Diagnostics))
	}
}

// TestStageJListsCELUses covers hub.Validated.CEL: every expression that
// can run with its site, a Policy's field once per site of its
// attachments, a default stage G wrote at the nearest authored ancestor,
// and none for a Policy no Route's chain runs.
func TestStageJListsCELUses(t *testing.T) {
	res := run(t, newPipeline(t, Options{}), bundle(map[string]string{
		"ruralz.yaml": gatewayDoc + "  policies:\n    - name: hdr\n",
		"u.yaml":      docs(upstreamDoc("orders"), upstreamDoc("inv")+"  policies:\n    - name: hdr\n"),
		"c.yaml":      "apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  credentials:\n    apiKeys:\n      - name: primary\n        hash: \"sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6\"\n  quotas:\n    - name: daily\n      unit: requests\n      limit: 10\n      window: 1h\n",
		"p.yaml": docs(
			policyDoc("hdr", "headers", "  config:\n    request:\n      set:\n        - {name: x-a, valueExpression: route.name}\n"),
			policyDoc("q", "quota", "  config:\n    consumerQuota: daily\n"),
			ratelimitWith("unused", "  when: request.method == \"GET\"\n"),
		),
		"r.yaml": docs(routeDoc("a", "orders", "  policies:\n    - name: q\n"), routeDoc("b", "inv", "")),
	}), Request{})
	wantClean(t, "run", res.Diagnostics)
	if res.Validated == nil {
		t.Fatal("no Validated")
	}
	type use struct {
		resource, path, expr string
		scope                phase.Scope
		loc                  diag.Location
	}
	var got []use
	for _, u := range res.Validated.CEL {
		got = append(got, use{u.Resource.Kind + "/" + u.Resource.Name, u.Path.String(), u.Expr, u.Site.Scope, u.Loc})
	}
	want := []use{
		{"Policy/hdr", "spec.config.request.set[name=x-a].valueExpression", "route.name", phase.ScopeGateway, diag.Location{File: "p.yaml", Line: 10, Column: 40}},
		{"Policy/hdr", "spec.config.request.set[name=x-a].valueExpression", "route.name", phase.ScopeUpstream, diag.Location{File: "p.yaml", Line: 10, Column: 40}},
		{"Policy/q", "spec.config.key", "consumer.name", phase.ScopeRoute, diag.Location{File: "p.yaml", Line: 19, Column: 5}},
	}
	if !slices.Equal(got, want) {
		t.Errorf("CEL uses\n%+v\nwant\n%+v", got, want)
	}
}

// TestStageJUnmappedField covers ErrPlace: an x-ruralz-cel field that maps
// to no CEL place, a fault of the release, fails the run with an error
// rather than a finding.
func TestStageJUnmappedField(t *testing.T) {
	var doc map[string]any
	if err := json.Unmarshal(schema.RenderedV1alpha1(), &doc); err != nil {
		t.Fatal(err)
	}
	defs, _ := doc["$defs"].(map[string]any)
	spec, _ := defs["UpstreamSpec"].(map[string]any)
	props, _ := spec["properties"].(map[string]any)
	timeout, _ := props["timeout"].(map[string]any)
	timeout["x-ruralz-cel"] = map[string]any{"result": "string", "variables": []any{"request"}}
	rendered, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load(v1alpha1.APIVersion, rendered)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	view, err := schemaview.Compile(rendered, idx)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	schemas, err := schemaidx.NewSet(idx)
	if err != nil {
		t.Fatal(err)
	}
	views, err := schemaview.NewSet(view)
	if err != nil {
		t.Fatal(err)
	}
	p := newPipeline(t, Options{Schemas: schemas, Views: views})
	_, err = p.Run(t.Context(), Request{Source: loader.Source{FS: bundle(map[string]string{
		"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders") + "  timeout: 2s\n",
	}), Path: "."}})
	if !errors.Is(err, ErrPlace) {
		t.Errorf("Run = %v, want ErrPlace", err)
	}
}

// TestStageJTransformValueResult covers the per-target result of a
// transform set[] valueExpression: string for targets header, query and
// path, so an int there is refused, and dyn for body, which takes any
// value; hub.Validated.CEL carries each entry's result.
func TestStageJTransformValueResult(t *testing.T) {
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"u.yaml":      upstreamDoc("orders"),
		"p.yaml": docs(
			policyDoc("treq", "transform.request", "  config:\n    set:\n"+
				"      - {target: header, name: x-a, valueExpression: request.method}\n"+
				"      - {target: query, name: n, valueExpression: '1'}\n"+
				"      - {target: path, valueExpression: request.path}\n"+
				"      - {target: body, name: n, valueExpression: '1'}\n"),
			policyDoc("tres", "transform.response", "  config:\n    set:\n"+
				"      - {target: header, name: x-s, valueExpression: response.status}\n"+
				"      - {target: body, name: s, valueExpression: response.status}\n"),
		),
		"r.yaml": routeDoc("a", "orders", "  policies:\n    - name: treq\n    - name: tres\n"),
	}), Request{})
	got := withCode(res.Diagnostics, "RZ-CFG-014")
	if len(got) != 2 || len(res.Diagnostics) != 2 {
		t.Fatalf("diagnostics %q; want the query and response header values refused", texts(res.Diagnostics))
	}
	for i, path := range []string{"spec.config.set[1].valueExpression", "spec.config.set[0].valueExpression"} {
		if got[i].Path.String() != path {
			t.Errorf("finding %d at %s, want %s: %q", i, got[i].Path, path, texts(got))
		}
	}
	ok := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"u.yaml":      upstreamDoc("orders"),
		"p.yaml": policyDoc("treq", "transform.request", "  config:\n    set:\n"+
			"      - {target: header, name: x-a, valueExpression: request.method}\n"+
			"      - {target: body, name: n, valueExpression: '1'}\n"),
		"r.yaml": routeDoc("a", "orders", "  policies:\n    - name: treq\n"),
	}), Request{})
	wantClean(t, "valid", ok.Diagnostics)
	if ok.Validated == nil || len(ok.Validated.CEL) != 2 {
		t.Fatalf("CEL uses %+v", ok.Validated)
	}
	for i, want := range []expr.Result{expr.ResultString, expr.ResultDyn} {
		if got := ok.Validated.CEL[i].Site.Result; got != want {
			t.Errorf("CEL use %d result = %v, want %v", i, got, want)
		}
	}
}
