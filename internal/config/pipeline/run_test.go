// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/render"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// validBundle is a Bundle without findings: a Gateway, an Upstream with a
// CA bundle secret, a headers Policy with a CEL value and a Route
// attaching it.
func validBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml": gatewayDoc,
		"upstreams.yaml": docs(upstreamDoc("orders"),
			upstreamDoc("secure")+"  tls:\n    caCertificate: {secretRef: {provider: file, name: /etc/ruralz/ca.crt}}\n"),
		"policies.yaml": policyDoc("stamp", "headers",
			"  config:\n    request:\n      set:\n        - name: x-route\n          valueExpression: route.name\n"),
		"routes.yaml": routeDoc("a", "orders", "  policies:\n    - name: stamp\n") + "---\n" + routeDoc("b", "secure", ""),
	}
}

// TestRunValidBundle covers a run without findings: the pipeline's output
// holds the hub Bundle, every Route's chain, the Revision and its digest,
// every secretRef use and every CEL expression with its site, and nothing
// is kept for the rendered Bundle unless asked.
func TestRunValidBundle(t *testing.T) {
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(validBundle()), Request{})
	wantClean(t, "run", res.Diagnostics)
	if res.Stopped || res.Files == nil || res.Rendered != nil || res.Environment != "" || res.Overlay != "" {
		t.Errorf("Result = %+v", res)
	}
	v := res.Validated
	if v == nil {
		t.Fatal("no Validated")
	}
	if got := len(v.Bundle.Resources()); got != 6 {
		t.Errorf("%d resources, want 6", got)
	}
	if v.Revision.Digest.IsZero() || v.Revision.Digest != revision.Sum(v.Revision.Content) ||
		!bytes.HasPrefix(v.Revision.Content, []byte(`{"format":"ruralz.canonical.v1","resources":[`)) {
		t.Errorf("Revision %s over %q", v.Revision.Digest, v.Revision.Content)
	}
	if !slices.Equal(sortedKeys(v.Chains), []string{"a", "b"}) {
		t.Errorf("Chains = %v", sortedKeys(v.Chains))
	}
	if es := v.Chains["a"].Client[phase.OnRequestHeaders]; len(es) != 1 || es[0].Policy != "stamp" {
		t.Errorf("Route a onRequestHeaders = %+v", es)
	}
	if len(v.Secrets) != 1 || v.Secrets[0].Resource != (diag.ResourceID{Kind: "Upstream", Name: "secure"}) ||
		v.Secrets[0].Destination != hub.DestinationLocal {
		t.Errorf("Secrets = %+v", v.Secrets)
	}
	want := expr.Site{Place: expr.PlaceHeadersRequestValue, Scope: phase.ScopeRoute, PolicyType: v1alpha1.PolicyTypeHeaders}
	if len(v.CEL) != 1 || v.CEL[0].Expr != "route.name" || siteKey(v.CEL[0].Site) != siteKey(want) ||
		v.CEL[0].Loc != (diag.Location{File: "policies.yaml", Line: 11, Column: 28}) {
		t.Errorf("CEL = %+v", v.CEL)
	}
}

// sortedKeys returns the keys of m, sorted.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// aiFiles are the resources of an ai Upstream whose AIModel sets no
// limits.maxOutputTokens, with a Consumer token quota and an
// ai.token-budget Policy that Route chat attaches: RZ-CFG-034 at stage K.
func aiFiles(model string) string {
	return docs(
		"apiVersion: ruralz/v1alpha1\nkind: AIProvider\nmetadata:\n  name: provider\nspec:\n  dialect: openai\n  credentials:\n    apiKey: {secretRef: {provider: env, name: RURALZ_SECRET_AI}}\n",
		"apiVersion: ruralz/v1alpha1\nkind: AIModel\nmetadata:\n  name: chat\nspec:\n  strategy: fallback\n  candidates:\n    - name: primary\n      provider: provider\n      model: gpt\n"+model,
		"apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: llm\nspec:\n  protocol: ai\n  ai:\n    models:\n      - name: chat\n",
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  credentials:\n    apiKeys:\n      - name: primary\n        hash: \"sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6\"\n  quotas:\n    - name: daily-tokens\n      unit: tokens\n      limit: 1000\n      window: 24h\n",
		policyDoc("budget", "ai.token-budget", "  config:\n    consumerQuota: daily-tokens\n"),
		routeDoc("chat", "llm", "  policies:\n    - name: budget\n"),
	)
}

// TestRunReportsEveryStageInOneRun covers "every error in one run": the
// findings of stages F, H, I, J and K of one Bundle come back together,
// and the Bundle has no Revision.
func TestRunReportsEveryStageInOneRun(t *testing.T) {
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"ai.yaml":     aiFiles(""),
		"f.yaml":      upstreamDoc("orders") + "  unknown: 1\n",
		"h.yaml":      routeDoc("missing-target", "nowhere", ""),
		"i.yaml": docs(
			policyDoc("cors-a", "cors", "  config:\n    allowOrigins: [\"https://a.example\"]\n"),
			policyDoc("cors-b", "cors", "  config:\n    allowOrigins: [\"https://b.example\"]\n"),
			routeDoc("slots", "llm", "  policies:\n    - name: cors-a\n    - name: cors-b\n"),
		),
		"j.yaml": routeWhen("cel", "llm", "    when: response.status == 200\n", ""),
	}), Request{})
	for _, code := range []string{"RZ-CFG-006", "RZ-CFG-009", "RZ-CFG-018", "RZ-CFG-014", "RZ-CFG-034"} {
		if len(withCode(res.Diagnostics, code)) != 1 {
			t.Errorf("want one %s among\n%s", code, strings.Join(texts(res.Diagnostics), "\n"))
		}
	}
	if res.Validated != nil || res.Stopped {
		t.Errorf("Validated %v, Stopped %v; want no Revision and a run to the end", res.Validated, res.Stopped)
	}
}

// TestRunLeavesOutRestatedFindings covers stages G to K running on what
// passed the stages before them: a finding that only restates a failure of
// stage F or G is left out, and one that does not stays.
func TestRunLeavesOutRestatedFindings(t *testing.T) {
	steps := func(n int) string {
		s := "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: comp\nspec:\n  match:\n    path: {prefix: /comp}\n  composition:\n    mode: aggregate\n    steps:\n"
		for i := range n {
			name := "s" + string(rune('a'+i))
			s += "      - name: " + name + "\n        upstream: orders\n        path: /x\n        group: " + name + "\n"
		}
		return s
	}
	consumer := func(quota string) string {
		return "apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: acme\nspec:\n  bogus: 1\n  credentials:\n    apiKeys:\n      - name: primary\n        hash: \"sha256:d1bec0f9342e5607570b2636ed0256023b13be7d7d4bb45e1bed3cfee80dc1b6\"\n  quotas:\n    - name: " + quota + "\n      unit: requests\n      limit: 10\n      window: 1h\n"
	}
	quota := policyDoc("q", "quota", "  config:\n    consumerQuota: daily\n") + "---\n" + routeDoc("quota-route", "orders", "  policies:\n    - name: q\n")
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"reference to a resource that failed stage F", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders") + "---\n" + upstreamDoc("broken") + "  unknown: 1\n",
			"r.yaml":      routeDoc("a", "broken", ""),
		}, []string{"RZ-CFG-006"}},
		{"reference to a resource that failed stage G", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders") + "---\n" + upstreamDoc("broken") + "  timeout: 99999999999999999h\n",
			"r.yaml":      routeDoc("a", "broken", ""),
		}, []string{"RZ-CFG-005"}},
		{"wrong-kind name of a target that failed", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders") + "---\n" + upstreamDoc("stamp") + "  unknown: 1\n",
			"p.yaml":      policyDoc("stamp", "headers", "  config:\n    response:\n      set: [{name: x-a, value: b}]\n"),
			"r.yaml":      routeDoc("a", "stamp", ""),
		}, []string{"RZ-CFG-006"}},
		{"reference to a missing target named like a failed resource of another kind", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders"),
			"p.yaml":      policyDoc("pol", "headers", "  config:\n    response:\n      set: [{name: x-a, value: b}]\n  unknown: 1\n"),
			"r.yaml":      routeDoc("a", "pol", ""),
		}, []string{"RZ-CFG-006", "RZ-CFG-009"}},
		{"quota a failed Consumer declares", map[string]string{
			"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"), "c.yaml": consumer("daily"), "q.yaml": quota,
		}, []string{"RZ-CFG-006"}},
		{"quota no Consumer declares", map[string]string{
			"ruralz.yaml": gatewayDoc, "u.yaml": upstreamDoc("orders"), "c.yaml": consumer("weekly"), "q.yaml": quota,
		}, []string{"RZ-CFG-006", "RZ-CFG-009"}},
		{"value a substitution error left in place", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders") + "---\n" + upstreamDoc("held") + "  timeout: ${NOPE}\n",
			"r.yaml":      routeDoc("a", "held", ""),
		}, []string{"RZ-CFG-010"}},
		{"$patch a base document holds", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("patched") + "  $patch: replace\n",
			"r.yaml":      routeDoc("a", "patched", ""),
		}, []string{"RZ-CFG-006"}},
		{"duplicate identity whose first declaration stays", map[string]string{
			"ruralz.yaml": gatewayDoc,
			"u.yaml":      upstreamDoc("orders") + "---\n" + upstreamDoc("orders"),
			"r.yaml":      routeDoc("a", "orders", ""),
		}, []string{"RZ-CFG-008"}},
		{"step limit a failed Gateway authors", map[string]string{
			"ruralz.yaml": gatewayDoc + "  limits:\n    maxCompositionSteps: 20\n  bogus: 1\n",
			"u.yaml":      upstreamDoc("orders"),
			"r.yaml":      steps(17),
		}, []string{"RZ-CFG-006"}},
		{"step limit a failed Gateway leaves to its default", map[string]string{
			"ruralz.yaml": gatewayDoc + "  bogus: 1\n",
			"u.yaml":      upstreamDoc("orders"),
			"r.yaml":      steps(17),
		}, []string{"RZ-CFG-006", "RZ-CFG-031"}},
	}
	p := newPipeline(t, Options{})
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, p, bundle(tc.files), Request{})
			got := codes(res.Diagnostics)
			slices.Sort(got)
			want := slices.Sorted(slices.Values(tc.want))
			if !slices.Equal(got, want) {
				t.Errorf("codes %q, want %q:\n%s", got, want, strings.Join(texts(res.Diagnostics), "\n"))
			}
		})
	}
}

// TestRunStopsAtDiagnosticsCap covers the cap: past Limits.MaxDiagnostics
// the run keeps the first findings in sort order, adds one RZ-CFG-001 and
// stops, so no later stage runs.
func TestRunStopsAtDiagnosticsCap(t *testing.T) {
	p := newPipeline(t, Options{Limits: loader.Limits{MaxDiagnostics: 2}})
	res := run(t, p, bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		// A stage H finding that would sort first, had stage H run.
		"a.yaml": routeDoc("a", "nowhere", ""),
		"u.yaml": docs(upstreamDoc("u1")+"  x: 1\n", upstreamDoc("u2")+"  x: 1\n", upstreamDoc("u3")+"  x: 1\n"),
	}), Request{})
	wantTexts(t, "run", res.Diagnostics,
		"- error RZ-CFG-001: too many diagnostics; stopped after 2",
		`u.yaml:9:3 error RZ-CFG-006 Upstream/u1 spec.x: unknown field "x"`,
		`u.yaml:19:3 error RZ-CFG-006 Upstream/u2 spec.x: unknown field "x"`,
	)
	if !res.Stopped || res.Validated != nil {
		t.Errorf("Stopped %v, Validated %v; want a stopped run without a Revision", res.Stopped, res.Validated)
	}
}

// TestRunWarningsKeepTheRevision covers warnings: an RZ-CFG-013 warning is
// reported and the run still reaches its digest.
func TestRunWarningsKeepTheRevision(t *testing.T) {
	files := validBundle()
	files["upstreams.yaml"] = docs(upstreamDoc("orders"), upstreamDoc("secure")+"  tls:\n    sni: ${API_TOKEN:-secure.internal}\n")
	res := run(t, newPipeline(t, Options{}), bundle(files), Request{})
	if len(res.Diagnostics) == 0 || res.Diagnostics.HasErrors() || res.Diagnostics[0].Code != "RZ-CFG-013" {
		t.Errorf("diagnostics %q, want RZ-CFG-013 warnings only", texts(res.Diagnostics))
	}
	if res.Validated == nil {
		t.Error("a run with warnings only has no Revision")
	}
}

// TestRunSameForEveryWorkerCount covers determinism: the diagnostics of a
// failing Bundle and the digest of a valid one do not depend on the worker
// count.
func TestRunSameForEveryWorkerCount(t *testing.T) {
	failing := map[string]string{
		"ruralz.yaml": gatewayDoc,
		"ai.yaml":     aiFiles(""),
		"u.yaml":      docs(upstreamDoc("orders")+"  x: 1\n", upstreamDoc("inv")),
		"r.yaml": docs(
			routeDoc("a", "nowhere", ""),
			routeWhen("b", "inv", "    when: response.status == 1\n", ""),
			routeWhen("c", "inv", "    when: request.method == 1\n", ""),
		),
	}
	var wantDiags []string
	var wantDigest revision.Digest
	for _, workers := range []int{1, 2, 3, 8} {
		p := newPipeline(t, Options{Workers: workers})
		got := texts(run(t, p, bundle(failing), Request{}).Diagnostics)
		valid := run(t, p, bundle(validBundle()), Request{})
		if workers == 1 {
			wantDiags, wantDigest = got, valid.Validated.Revision.Digest
			if len(got) < 4 {
				t.Fatalf("too few findings to compare: %q", got)
			}
			continue
		}
		if !slices.Equal(got, wantDiags) {
			t.Errorf("%d workers: diagnostics\n%s\nwant\n%s", workers, strings.Join(got, "\n"), strings.Join(wantDiags, "\n"))
		}
		if valid.Validated.Revision.Digest != wantDigest {
			t.Errorf("%d workers: digest %s, want %s", workers, valid.Validated.Revision.Digest, wantDigest)
		}
	}
}

// TestRunVariables covers the sources of ${VAR}: an Environment's
// spec.variables with Request.Environment, never Request.Variables, and
// Request.Variables, the process environment, without one.
func TestRunVariables(t *testing.T) {
	files := bundle(map[string]string{
		"ruralz.yaml": gatewayDoc,
		"u.yaml":      strings.Replace(upstreamDoc("orders"), "orders.internal:80", "${HOST}:80", 1),
		"r.yaml":      routeDoc("a", "orders", ""),
	})
	control := fstest.MapFS{"envs.yaml": &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: prod\nspec:\n  variables:\n    HOST: prod.internal\n")}}
	p := newPipeline(t, Options{})
	address := func(res *Result) string {
		t.Helper()
		if res.Validated == nil {
			t.Fatalf("no Validated: %q", texts(res.Diagnostics))
		}
		u, _ := res.Validated.Bundle.Upstream("orders")
		return u.Spec.Endpoints[0].Address
	}
	env := run(t, p, files, Request{
		Environments: loader.Source{FS: control, Path: "envs.yaml"}, Environment: "prod",
		Variables: subst.Map{"HOST": "process.internal"},
	})
	if got := address(env); got != "prod.internal:80" || env.Environment != "prod" {
		t.Errorf("with --env: address %q, Environment %q; want the Environment's variable", got, env.Environment)
	}
	proc := run(t, p, files, Request{Variables: subst.Map{"HOST": "process.internal"}})
	if got := address(proc); got != "process.internal:80" {
		t.Errorf("without --env: address %q, want the process environment's variable", got)
	}
	none := run(t, p, files, Request{})
	if len(withCode(none.Diagnostics, "RZ-CFG-010")) != 1 || none.Validated != nil {
		t.Errorf("without variables: %q, want RZ-CFG-010", texts(none.Diagnostics))
	}
}

// TestRunRequestErrors covers the errors of Run that are not findings: an
// Environment without an environments file, a source that cannot be read,
// and a canceled context; and an Environment that cannot be used, which
// stops the run with RZ-CFG-009.
func TestRunRequestErrors(t *testing.T) {
	p := newPipeline(t, Options{})
	m := bundle(validBundle())
	if _, err := p.Run(t.Context(), Request{Source: loader.Source{FS: m, Path: "."}, Environment: "prod"}); !errors.Is(err, loader.ErrOptions) {
		t.Errorf("Environment without a file: %v, want loader.ErrOptions", err)
	}
	if _, err := p.Run(t.Context(), Request{Source: loader.Source{FS: m, Path: "missing"}}); !errors.Is(err, loader.ErrSource) {
		t.Errorf("missing source: %v, want loader.ErrSource", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := p.Run(ctx, Request{Source: loader.Source{FS: m, Path: "."}}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v, want context.Canceled", err)
	}
	control := fstest.MapFS{"envs.yaml": &fstest.MapFile{Data: []byte("apiVersion: ruralz/v1alpha1\nkind: Environment\nmetadata:\n  name: prod\nspec: {}\n")}}
	res := run(t, p, m, Request{Environments: loader.Source{FS: control, Path: "envs.yaml"}, Environment: "qa"})
	if !res.Stopped || res.Validated != nil || !slices.Equal(codes(res.Diagnostics), []string{"RZ-CFG-009"}) {
		t.Errorf("undeclared Environment: Stopped %v, %q", res.Stopped, texts(res.Diagnostics))
	}
}

// errBoom is the panic value of a check in TestRunPanicReachesCaller.
var errBoom = errors.New("boom")

// TestRunPanicReachesCaller covers a panic in a stage: an injected check
// that panics surfaces on the goroutine that called Run, wrapping the
// panic value, after every goroutine of the run stopped.
func TestRunPanicReachesCaller(t *testing.T) {
	p := newPipeline(t, Options{Workers: 4, Checks: hub.Checks{
		v1alpha1.PolicyTypeHeaders: func(*hub.Resource, func(diag.Path) diag.Location, func(diag.Diagnostic)) { panic(errBoom) },
	}})
	defer func() {
		v := recover()
		err, ok := v.(error)
		if !ok || !errors.Is(err, errBoom) {
			t.Errorf("recovered %v, want a panic wrapping errBoom", v)
		}
	}()
	run(t, p, bundle(validBundle()), Request{})
	t.Error("Run returned despite a panicking check")
}

// TestRunRendered covers Request.Render: the resources as stage E left
// them, without defaults, which WriteRendered writes as a one-file Bundle
// whose run yields the same Revision, and as the JSON array of the same
// resources for tooling, both writing a literal ${ back as $${.
func TestRunRendered(t *testing.T) {
	files := validBundle()
	files["policies.yaml"] = policyDoc("stamp", "headers",
		"  config:\n    request:\n      set:\n        - name: x-route\n          valueExpression: route.name\n        - name: x-literal\n          value: \"$${KEEP}\"\n")
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(files), Request{Render: true})
	wantClean(t, "run", res.Diagnostics)
	if len(res.Rendered) != 6 {
		t.Fatalf("Rendered holds %d resources, want 6", len(res.Rendered))
	}
	for _, r := range res.Rendered {
		if r.ID.Kind == v1alpha1.KindUpstream {
			ep, _ := r.Root.At(diag.Path{diag.Field("spec"), diag.Field("endpoints"), diag.Index(0)})
			if _, ok := ep.Get("weight"); ok {
				t.Errorf("rendered %s holds the default weight", r.ID)
			}
		}
	}
	for _, f := range []render.Format{render.FormatYAML, render.FormatJSON} {
		var out bytes.Buffer
		ds, err := p.WriteRendered(t.Context(), &out, res, f)
		if err != nil || len(ds) > 0 {
			t.Fatalf("WriteRendered(%s) = %q, %v", f, texts(ds), err)
		}
		if !strings.Contains(out.String(), "$${KEEP}") {
			t.Errorf("%s output lacks the $${ escape:\n%s", f, out.String())
		}
		if f == render.FormatJSON {
			var array []map[string]any
			if err := json.Unmarshal(out.Bytes(), &array); err != nil || len(array) != 6 || array[0]["kind"] != "Gateway" {
				t.Errorf("JSON form: %d resources, %v; want 6 in canonical order", len(array), err)
			}
			continue
		}
		again, err := p.Run(t.Context(), Request{Source: loader.Source{FS: bundle(map[string]string{"rendered": out.String()}), Path: "rendered"}})
		if err != nil {
			t.Fatalf("Run(rendered %s): %v", f, err)
		}
		wantClean(t, "rendered "+f.String(), again.Diagnostics)
		if again.Validated == nil || again.Validated.Revision.Digest != res.Validated.Revision.Digest {
			t.Errorf("%s: the rendered Bundle's Revision differs from the source's", f)
		}
	}
}

// TestWriteRenderedNeedsRenderedRun covers ErrInput of WriteRendered and
// Effective: a run without Request.Render, or without a digest.
func TestWriteRenderedNeedsRenderedRun(t *testing.T) {
	p := newPipeline(t, Options{})
	plain := run(t, p, bundle(validBundle()), Request{})
	failing := run(t, p, bundle(map[string]string{"ruralz.yaml": gatewayDoc, "r.yaml": routeDoc("a", "x", "")}), Request{Render: true})
	if failing.Rendered != nil {
		t.Error("a failing run kept rendered resources")
	}
	for name, res := range map[string]*Result{"not rendered": plain, "failing": failing, "nil": nil} {
		if _, err := p.WriteRendered(t.Context(), &bytes.Buffer{}, res, render.FormatYAML); !errors.Is(err, ErrInput) {
			t.Errorf("WriteRendered(%s) = %v, want ErrInput", name, err)
		}
	}
	if _, err := p.Effective(failing, "a"); !errors.Is(err, ErrInput) {
		t.Errorf("Effective(failing) = %v, want ErrInput", err)
	}
}

// TestEffective covers the effective Filter Chain rows of a run, as
// ruralz bundle render --effective prints them, and an unknown Route.
func TestEffective(t *testing.T) {
	p := newPipeline(t, Options{})
	res := run(t, p, bundle(validBundle()), Request{})
	rows, err := p.Effective(res, "a")
	if err != nil || len(rows) != 1 || rows[0].Policy != "stamp" || rows[0].Phase != "onRequestHeaders" {
		t.Errorf("Effective(a) = %+v, %v", rows, err)
	}
	if _, err := p.Effective(res, "aa"); !errors.Is(err, render.ErrUnknownRoute) || !strings.Contains(err.Error(), `did you mean "a"?`) {
		t.Errorf("Effective(aa) = %v, want ErrUnknownRoute with a hint", err)
	}
}

// TestRunStopsAtLoadLimit covers a limit of the load: RZ-CFG-001 stops the
// run before stage F, without a Revision.
func TestRunStopsAtLoadLimit(t *testing.T) {
	p := newPipeline(t, Options{Limits: loader.Limits{MaxResources: 2}})
	res := run(t, p, bundle(validBundle()), Request{})
	if !res.Stopped || res.Validated != nil || !slices.Equal(codes(res.Diagnostics), []string{"RZ-CFG-001"}) {
		t.Errorf("Stopped %v, %q; want a stopped run with one RZ-CFG-001", res.Stopped, texts(res.Diagnostics))
	}
}

// TestRunConcurrent covers a Pipeline shared by concurrent runs, as one
// process serves every load: each run's result is the one it has alone.
func TestRunConcurrent(t *testing.T) {
	p := newPipeline(t, Options{Workers: 2})
	failing := bundle(map[string]string{"ruralz.yaml": gatewayDoc, "ai.yaml": aiFiles(""), "r.yaml": routeWhen("w", "llm", "    when: response.status == 1\n", "")})
	valid := bundle(validBundle())
	wantFailing := texts(run(t, p, failing, Request{}).Diagnostics)
	wantDigest := run(t, p, valid, Request{}).Validated.Revision.Digest
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if i%2 == 0 {
				res, err := p.Run(t.Context(), Request{Source: loader.Source{FS: failing, Path: "."}})
				if err != nil || !slices.Equal(texts(res.Diagnostics), wantFailing) {
					t.Errorf("concurrent failing run: %v, %q", err, texts(res.Diagnostics))
				}
				return
			}
			res, err := p.Run(t.Context(), Request{Source: loader.Source{FS: valid, Path: "."}})
			if err != nil || res.Validated == nil || res.Validated.Revision.Digest != wantDigest {
				t.Errorf("concurrent valid run: %v, %v", err, res)
			}
		})
	}
	wg.Wait()
}
