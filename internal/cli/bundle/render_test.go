// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests of ruralz bundle render without --api-version
// (docs/reference/01-cli-and-api-surface.md "Bundle verbs in detail";
// docs/architecture/02-configuration-model.md "Rendered Bundle").

// renderBundle is a Bundle whose files are not in canonical order: the
// Route file sorts before the Upstream file, and the Route's timeout reads
// ${TIMEOUT:-5s} beside a label holding a literal $${ escape; the prod
// overlay changes the Upstream's timeout.
func renderBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml": gatewayDoc,
		"a-routes.yaml": `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
  labels:
    template: "/v1/$${id}"
spec:
  match:
    path: {prefix: /orders}
  upstreams:
    - name: orders
  timeout: ${TIMEOUT:-5s}
`,
		"b-upstreams.yaml": upstreamDoc("  timeout: 2s\n"),
		"overlays/prod/upstream.yaml": `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  timeout: 9s
`,
	}
}

// renderedYAML is the rendered Bundle of renderBundle with the Route
// timeout t and the Upstream timeout u.
func renderedYAML(t, u string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  protocol: http
  endpoints:
    - address: "orders.internal:80"
  timeout: "` + u + `"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
  labels:
    template: "/v1/$${id}"
spec:
  match:
    path:
      prefix: /orders
  upstreams:
    - name: orders
  timeout: "` + t + `"
`
}

// TestRenderYAML: the rendered Bundle is one YAML stream in canonical
// order, ${VAR} substituted from the process environment without --env,
// every literal ${ written back as $${, nothing defaulted.
func TestRenderYAML(t *testing.T) {
	dir := writeBundle(t, renderBundle())
	run(t, "bundle", "render", dir).want(t, "render", command.ExitOK, renderedYAML("5s", "2s"), "")
	execute(t, testOptions(), []string{"TIMEOUT=7s"}, "bundle", "render", dir).
		want(t, "render with TIMEOUT=7s", command.ExitOK, renderedYAML("7s", "2s"), "")
}

// TestRenderEnv: --env applies the Environment's overlay and variables.
func TestRenderEnv(t *testing.T) {
	dir := writeBundle(t, renderBundle())
	run(t, "bundle", "render", "--env", "prod", "--environments", envFile(t), dir).
		want(t, "render --env prod", command.ExitOK, renderedYAML("4s", "9s"), "")
}

// TestRenderIdempotent: rendering the rendered Bundle again, a one-file
// Bundle, yields the same bytes, from a file and from standard input.
func TestRenderIdempotent(t *testing.T) {
	dir := writeBundle(t, renderBundle())
	first := run(t, "bundle", "render", dir)
	file := writeFile(t, "rendered.yaml", first.stdout)
	run(t, "bundle", "render", file).want(t, "render of the rendered file", command.ExitOK, first.stdout, "")
	execute(t, withStdin(testOptions(), first.stdout), nil, "bundle", "render", "-").
		want(t, "render of standard input", command.ExitOK, first.stdout, "")
}

// TestRenderJSON: --output json writes the same resources, in the same
// order, as one JSON array.
func TestRenderJSON(t *testing.T) {
	dir := writeBundle(t, renderBundle())
	r := run(t, "bundle", "render", "--output", "json", dir)
	r.wantCode(t, "render --output json", command.ExitOK)
	var got []struct {
		Kind     string         `json:"kind"`
		Metadata map[string]any `json:"metadata"`
		Spec     map[string]any `json:"spec"`
	}
	if err := json.Unmarshal([]byte(r.stdout), &got); err != nil {
		t.Fatalf("stdout is not a JSON array: %v\n%s", err, r.stdout)
	}
	var kinds []string
	for _, g := range got {
		kinds = append(kinds, g.Kind)
	}
	if !slices.Equal(kinds, []string{"Gateway", "Upstream", "Route"}) || got[2].Spec["timeout"] != "5s" {
		t.Errorf("JSON resources = %+v", got)
	}
	if !strings.Contains(r.stdout, `"template": "/v1/$${id}"`) {
		t.Errorf("JSON does not keep the $${ escape:\n%s", r.stdout)
	}
}

// TestRenderOutputFile: --output-file receives the rendered Bundle, and
// standard output stays empty.
func TestRenderOutputFile(t *testing.T) {
	dir := writeBundle(t, renderBundle())
	out := filepath.Join(t.TempDir(), "rendered.yaml")
	run(t, "bundle", "render", "--output-file", out, dir).want(t, "render --output-file", command.ExitOK, "", "")
	if got := readFile(t, out); got != renderedYAML("5s", "2s") {
		t.Errorf("--output-file holds\n%s", got)
	}
	r := run(t, "bundle", "render", "--output-file", filepath.Join(t.TempDir(), "missing", "x.yaml"), dir)
	r.wantCode(t, "render into a missing directory", command.ExitNoResult)
}

// TestRenderFailsOnDiagnostics: an invalid Bundle prints its diagnostics
// as text lines on standard error, and with --output json also as one
// array on standard output; it exits 1 and writes no Bundle.
func TestRenderFailsOnDiagnostics(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	run(t, "bundle", "render", dir).want(t, "render", command.ExitNegative, "", typoLine+"\n")
	r := run(t, "bundle", "render", "--output", "json", dir)
	r.wantCode(t, "render --output json", command.ExitNegative)
	var ds []map[string]any
	if err := json.Unmarshal([]byte(r.stdout), &ds); err != nil || len(ds) != 1 || ds[0]["code"] != "RZ-CFG-009" {
		t.Errorf("stdout = %q (%v), want the diagnostics array", r.stdout, err)
	}
	if r.stderr != typoLine+"\n" {
		t.Errorf("stderr = %q, want the text line", r.stderr)
	}
	out := filepath.Join(t.TempDir(), "rendered.yaml")
	run(t, "bundle", "render", "--output-file", out, dir).want(t, "render --output-file", command.ExitNegative, "", typoLine+"\n")
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Errorf("--output-file of an invalid Bundle exists: %v", err)
	}
}

// TestRenderSecretLiteral: a secret field that holds anything but a
// reference fails the render with RZ-CFG-012 and exit 1, naming neither
// the value, and nothing is written.
func TestRenderSecretLiteral(t *testing.T) {
	files := simpleBundle()
	files["upstreams.yaml"] = upstreamDoc("  tls:\n    caCertificate: inline-ca-canary-7Hq2\n")
	dir := writeBundle(t, files)
	r := run(t, "bundle", "render", dir)
	r.wantCode(t, "render", command.ExitNegative)
	if r.stdout != "" || !strings.Contains(r.stderr, " error RZ-CFG-012 Upstream/orders spec.tls.caCertificate") ||
		strings.Contains(r.stderr, "canary") {
		t.Errorf("stdout %q, stderr %q: want RZ-CFG-012 at the field, without the value", r.stdout, r.stderr)
	}
}

// TestRenderWarnings: warnings print on standard error while the Bundle is
// written.
func TestRenderWarnings(t *testing.T) {
	dir := writeBundle(t, warningBundle())
	r := run(t, "bundle", "render", dir)
	r.wantCode(t, "render", command.ExitOK)
	if !strings.Contains(r.stderr, " warning RZ-CFG-013 ") || !strings.Contains(r.stdout, "kind: Route") {
		t.Errorf("stdout %q, stderr %q: want the Bundle and the warning on standard error", r.stdout, r.stderr)
	}
	o := testOptions()
	o.terminal = func(io.Writer) bool { return true }
	if r := execute(t, o, nil, "bundle", "render", dir); !strings.Contains(r.stderr, ansiYellow+"warning"+ansiReset) || strings.Contains(r.stdout, "\x1b") {
		t.Errorf("on a terminal: stdout %q, stderr %q; want the warning colored and the Bundle plain", r.stdout, r.stderr)
	}
}

// effectiveBundle is a Bundle with a Gateway CORS Policy, a Route a that
// replaces it, a Route b that inherits it and a Route c that excludes it.
func effectiveBundle() map[string]string {
	return map[string]string{
		"ruralz.yaml":    strings.Replace(gatewayDoc, "spec:\n", "spec:\n  policies:\n    - name: cors-default\n", 1),
		"upstreams.yaml": upstreamDoc(""),
		"policies.yaml": docs(
			policyDoc("cors-default", "cors", "  config:\n    allowOrigins: [\"https://shop.example\"]\n"),
			policyDoc("cors-partner", "cors", "  config:\n    allowOrigins: [\"https://partner.example\"]\n"),
		),
		"routes.yaml": docs(
			routeDoc("b", "orders", ""),
			routeDoc("a", "orders", "  policies:\n    - name: cors-partner\n"),
			routeDoc("c", "orders", "  excludePolicies:\n    - name: cors-default\n"),
		),
	}
}

// TestRenderEffectiveRoute: --effective --route NAME prints that Route's
// effective Filter Chain, the PHASE, LEG, POLICY, FROM and REASON table, or
// with --output json the rows.
func TestRenderEffectiveRoute(t *testing.T) {
	dir := writeBundle(t, effectiveBundle())
	run(t, "bundle", "render", "--effective", "--route", "a", dir).want(t, "--route a", command.ExitOK, ""+
		"PHASE             LEG     POLICY        FROM     REASON\n"+
		"onRequestHeaders  client  cors-partner  Route    slot cors, replaces cors-default\n"+
		"onResponse        client  cors-partner  Route    slot cors, replaces cors-default\n"+
		"none              none    cors-default  Gateway  replaced in slot cors by cors-partner\n", "")
	r := run(t, "bundle", "render", "--effective", "--route", "c", "--output", "json", dir)
	r.want(t, "--route c --output json", command.ExitOK, `[
  {
    "phase": "none",
    "leg": "none",
    "policy": "cors-default",
    "type": "cors",
    "filterClass": "cors",
    "slot": "cors",
    "from": "Gateway",
    "reason": "excluded by Route"
  }
]
`, "")
}

// TestRenderEffectiveUnknownRoute: a NAME that is no Route of the Bundle
// exits 2 with the nearest Route name as a hint.
func TestRenderEffectiveUnknownRoute(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	run(t, "bundle", "render", "--effective", "--route", "ordrs", dir).
		want(t, "--route ordrs", command.ExitNoResult, "", "ruralz bundle render: unknown Route \"ordrs\" (did you mean \"orders\"?)\n")
}

// TestRenderEffectiveAllRoutes: --effective without --route prints the
// chains of every Route in canonical order by name, led by a ROUTE column
// or, in JSON, a member route; a Route that runs no Policy adds no row, and
// a Bundle without a Route prints the header alone, or [].
func TestRenderEffectiveAllRoutes(t *testing.T) {
	files := effectiveBundle()
	files["ruralz.yaml"] = gatewayDoc
	files["routes.yaml"] = docs(
		routeDoc("b", "orders", ""),
		routeDoc("a", "orders", "  policies:\n    - name: cors-partner\n"),
	)
	dir := writeBundle(t, files)
	run(t, "bundle", "render", "--effective", dir).want(t, "--effective", command.ExitOK, ""+
		"ROUTE  PHASE             LEG     POLICY        FROM   REASON\n"+
		"a      onRequestHeaders  client  cors-partner  Route  slot cors\n"+
		"a      onResponse        client  cors-partner  Route  slot cors\n", "")
	r := run(t, "bundle", "render", "--effective", "--output", "json", dir)
	r.wantCode(t, "--effective --output json", command.ExitOK)
	if !strings.HasPrefix(r.stdout, "[\n  {\n    \"route\": \"a\",\n    \"phase\": \"onRequestHeaders\",\n") {
		t.Errorf("JSON rows do not lead with route:\n%s", r.stdout)
	}
	var rows []routeRow
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil || len(rows) != 2 {
		t.Errorf("JSON rows = %+v (%v)", rows, err)
	}

	gatewayOnly := writeBundle(t, map[string]string{"ruralz.yaml": gatewayDoc})
	run(t, "bundle", "render", "--effective", gatewayOnly).
		want(t, "no Route", command.ExitOK, "ROUTE  PHASE  LEG  POLICY  FROM  REASON\n", "")
	run(t, "bundle", "render", "--effective", "--output", "json", gatewayOnly).want(t, "no Route, JSON", command.ExitOK, "[]\n", "")
}

// TestRenderEffectiveWorkedExample: the shop-bundle example's
// orders-summary prints the rows of the configuration model's worked
// example in that order.
func TestRenderEffectiveWorkedExample(t *testing.T) {
	r := run(t, "bundle", "render", "--effective", "--route", "orders-summary", "--output", "json",
		filepath.Join(examplesDir, "shop-bundle"))
	r.wantCode(t, "--route orders-summary", command.ExitOK)
	var rows []struct{ Phase, Leg, Policy, From string }
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("stdout is not JSON: %v", err)
	}
	var got []string
	for _, row := range rows {
		got = append(got, row.Phase+" "+row.Leg+" "+row.Policy+" "+row.From)
	}
	want := []string{
		"onRequestHeaders client cors-partner Route",
		"onRequestHeaders client jwt-default Gateway",
		"onRequestHeaders client geo-block-default Gateway",
		"onRequestHeaders client authz-orders Route",
		"onRequestHeaders client ratelimit-global Gateway",
		"onRequestHeaders client ratelimit-orders Route",
		"onUpstreamRequest orders upstream-oauth Upstream orders",
		"onUpstreamRequest orders headers-internal Upstream orders",
		"onUpstreamRequest inventory headers-internal Upstream inventory",
		"onResponse client headers-security Gateway",
		"onResponse client cors-partner Route",
		"none none cors-default Gateway",
	}
	if !slices.Equal(got, want) {
		t.Errorf("rows =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

// TestRenderEffectiveFailsOnDiagnostics: --effective on an invalid Bundle
// exits 1 with its diagnostics, before any Route is looked up.
func TestRenderEffectiveFailsOnDiagnostics(t *testing.T) {
	dir := writeBundle(t, typoBundle())
	run(t, "bundle", "render", "--effective", "--route", "missing", dir).want(t, "--effective", command.ExitNegative, "", typoLine+"\n")
}

// TestRenderEffectiveOutputFile: --output-file receives the --effective
// output.
func TestRenderEffectiveOutputFile(t *testing.T) {
	dir := writeBundle(t, effectiveBundle())
	out := filepath.Join(t.TempDir(), "chain.txt")
	run(t, "bundle", "render", "--effective", "--route", "b", "--output-file", out, dir).want(t, "--output-file", command.ExitOK, "", "")
	if got := readFile(t, out); !strings.HasPrefix(got, "PHASE ") || !strings.Contains(got, "slot cors, inherited") {
		t.Errorf("--output-file holds\n%s", got)
	}
}

// TestRenderFlagCombinations: --route needs --effective; --api-version
// needs --output-dir and --environments and excludes --effective and
// --route; --output-dir needs --api-version; text output needs --effective
// or --api-version. Any other combination is a usage error, exit 2.
func TestRenderFlagCombinations(t *testing.T) {
	dir := writeBundle(t, simpleBundle())
	envs := envFile(t)
	out := filepath.Join(t.TempDir(), "out")
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--route", "orders"}, "--route needs --effective"},
		{[]string{"--route", "orders", "--effective=false"}, "--route needs --effective"},
		{[]string{"--output-dir", out}, "--output-dir needs --api-version"},
		{[]string{"--api-version", "ruralz/v1alpha1", "--environments", envs}, "--api-version needs --output-dir"},
		{[]string{"--api-version", "ruralz/v1alpha1", "--output-dir", out}, "--api-version needs --environments: it checks the conversion for every Environment of the file"},
		{[]string{"--api-version", "ruralz/v1alpha1", "--output-dir", out, "--environments", envs, "--effective"}, "--api-version excludes --effective and --route"},
		{[]string{"--api-version", "ruralz/v1alpha1", "--output-dir", out, "--environments", envs, "--route", "x"}, "--api-version excludes --effective and --route"},
		{[]string{"--output", "text"}, "unsupported --output \"text\" (want yaml or json)"},
		{[]string{"--effective=false", "--output", "text"}, "--output text needs --effective or --api-version"},
		{[]string{"--effective", "--output", "yaml"}, "unsupported --output \"yaml\" (want text or json)"},
	} {
		r := run(t, append(append([]string{"bundle", "render"}, tc.args...), dir)...)
		r.want(t, strings.Join(tc.args, " "), command.ExitNoResult, "",
			"ruralz bundle render: "+tc.msg+"\nRun 'ruralz bundle render --help' for usage.\n")
	}
	r := execute(t, withStdin(testOptions(), "x"), nil, "bundle", "render", "--api-version", "ruralz/v1alpha1",
		"--output-dir", out, "--environments", envs, "-")
	r.want(t, "--api-version -", command.ExitNoResult, "",
		"ruralz bundle render: --api-version converts the files of a Bundle and reads no standard input\nRun 'ruralz bundle render --help' for usage.\n")
	run(t, "bundle", "render", "--effective=false", dir).want(t, "--effective=false", command.ExitOK, renderedSimple(), "")
}

// renderedSimple is the rendered Bundle of simpleBundle.
func renderedSimple() string {
	return `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: orders
spec:
  protocol: http
  endpoints:
    - address: "orders.internal:80"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: orders
spec:
  match:
    path:
      prefix: /orders
  upstreams:
    - name: orders
  timeout: "5s"
`
}
