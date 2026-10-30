// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import (
	"bytes"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/precedence"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// effective returns the --effective output of route in format f.
func effective(t *testing.T, r *run, route string, f Format) []byte {
	t.Helper()
	rows, err := Effective(r.validated, route, mustStages(t).resolver)
	if err != nil {
		t.Fatalf("Effective(%s) error = %v", route, err)
	}
	var buf bytes.Buffer
	if err := WriteEffective(&buf, rows, f); err != nil {
		t.Fatalf("WriteEffective(%s) error = %v", f, err)
	}
	return buf.Bytes()
}

// TestEffectiveGolden checks the --effective table and its JSON form for
// every Route of the example Bundle, rendered without an Environment and
// for each of its Environments, against the golden corpus's
// effective/<render>/<route>.txt and .json, which hold the tables of
// every Route and of no other. The tests of internal/config/precedence
// hold stage I's rows to the same tables, so render --effective prints
// the bytes stage I pins and cannot drift from the resolver.
func TestEffectiveGolden(t *testing.T) {
	l := shopLevel(t)
	for _, env := range shopRenders(t) {
		r := shop(t, env)
		dir := "effective/" + renderName(env) + "/"
		var pinned []string
		for _, f := range l.Files {
			if route, ok := strings.CutPrefix(f.Name, dir); ok && strings.HasSuffix(route, ".txt") {
				pinned = append(pinned, strings.TrimSuffix(route, ".txt"))
			}
		}
		var routes []string
		for _, res := range r.validated.Bundle.Resources() {
			if res.Kind == v1alpha1.KindRoute {
				routes = append(routes, res.Name)
			}
		}
		slices.Sort(routes)
		if len(routes) == 0 || !slices.Equal(pinned, routes) {
			t.Fatalf("%s: the corpus pins the effective tables of %q, the render has the Routes %q", renderName(env), pinned, routes)
		}
		for _, route := range routes {
			sameAsCorpus(t, dir+route+".txt", effective(t, r, route, FormatText))
			sameAsCorpus(t, dir+route+".json", effective(t, r, route, FormatJSON))
		}
	}
}

// TestEffectiveWorkedExample checks the configuration model's worked
// example: the orders-summary rows equal its table in Phase, Leg, Policy
// and From, in order.
func TestEffectiveWorkedExample(t *testing.T) {
	rows, err := Effective(shop(t, "prod").validated, "orders-summary", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := [][4]string{
		{"onRequestHeaders", "client", "cors-partner", "Route"},
		{"onRequestHeaders", "client", "jwt-default", "Gateway"},
		{"onRequestHeaders", "client", "geo-block-default", "Gateway"},
		{"onRequestHeaders", "client", "authz-orders", "Route"},
		{"onRequestHeaders", "client", "ratelimit-global", "Gateway"},
		{"onRequestHeaders", "client", "ratelimit-orders", "Route"},
		{"onUpstreamRequest", "orders", "upstream-oauth", "Upstream orders"},
		{"onUpstreamRequest", "orders", "headers-internal", "Upstream orders"},
		{"onUpstreamRequest", "inventory", "headers-internal", "Upstream inventory"},
		{"onResponse", "client", "headers-security", "Gateway"},
		{"onResponse", "client", "cors-partner", "Route"},
		{"none", "none", "cors-default", "Gateway"},
	}
	got := make([][4]string, len(rows))
	for i, row := range rows {
		got[i] = [4]string{row.Phase, row.Leg, row.Policy, row.From}
	}
	if !slices.Equal(got, want) {
		t.Errorf("orders-summary rows:\n got %q\nwant %q", got, want)
	}
}

// TestEffectiveTableLayout checks the text form: the header line, one line
// per row with five cells, columns padded by two spaces past their widest
// cell, the last column unpadded and a final newline.
func TestEffectiveTableLayout(t *testing.T) {
	rows := []precedence.Row{
		{Phase: "onRequestHeaders", Leg: "client", Policy: "p", From: "Gateway", Reason: "own slot"},
		{Phase: "none", Leg: "none", Policy: "longer-name", From: "Gateway", Reason: "excluded by Route"},
	}
	var buf bytes.Buffer
	if err := WriteEffective(&buf, rows, FormatText); err != nil {
		t.Fatal(err)
	}
	want := "PHASE             LEG     POLICY       FROM     REASON\n" +
		"onRequestHeaders  client  p            Gateway  own slot\n" +
		"none              none    longer-name  Gateway  excluded by Route\n"
	if buf.String() != want {
		t.Errorf("table:\n%s\nwant:\n%s", buf.String(), want)
	}
}

// TestEffectiveEmptyChain checks a Route that runs no Policy: the table
// is its header alone, and the JSON form an empty array.
func TestEffectiveEmptyChain(t *testing.T) {
	sources := []source{
		{"ruralz.yaml", []byte(minimalGateway)},
		{"routes.yaml", []byte(`apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: web}
spec:
  match: {path: {prefix: /}}
  upstreams: [{name: backend}]
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata: {name: backend}
spec:
  protocol: http
  endpoints: [{address: "backend.svc:8080"}]
`)},
	}
	r := mustPipeline(t, sources, nil, subst.Map{})
	if got := string(effective(t, r, "web", FormatText)); got != "PHASE  LEG  POLICY  FROM  REASON\n" {
		t.Errorf("text = %q", got)
	}
	if got := string(effective(t, r, "web", FormatJSON)); got != "[]\n" {
		t.Errorf("JSON = %q, want []", got)
	}
	var buf bytes.Buffer
	if err := WriteEffective(&buf, nil, FormatJSON); err != nil || buf.String() != "[]\n" {
		t.Errorf("WriteEffective(nil) = %q, %v; want []", buf.String(), err)
	}
}

// TestEffectiveUnknownRoute checks that a --route naming no Route is
// ErrUnknownRoute with the nearest Route name as a hint, and none when no
// name is near.
func TestEffectiveUnknownRoute(t *testing.T) {
	r := shop(t, "prod")
	_, err := Effective(r.validated, "order-summary", nil)
	if !errors.Is(err, ErrUnknownRoute) || !strings.Contains(err.Error(), `"order-summary"`) || !strings.Contains(err.Error(), `did you mean "orders-summary"?`) {
		t.Errorf("Effective(order-summary) error = %v, want ErrUnknownRoute hinting orders-summary", err)
	}
	_, err = Effective(r.validated, "zzzzzzzzzzzz", nil)
	if !errors.Is(err, ErrUnknownRoute) || strings.Contains(err.Error(), "did you mean") {
		t.Errorf("Effective(zzz) error = %v, want ErrUnknownRoute without a hint", err)
	}
	// A Policy or Upstream name is not a Route.
	if _, err := Effective(r.validated, "orders", nil); !errors.Is(err, ErrUnknownRoute) {
		t.Errorf("Effective(orders) error = %v, want ErrUnknownRoute for an Upstream name", err)
	}
	long := strings.Repeat("x", 500)
	if _, err := Effective(r.validated, long, nil); err == nil || strings.Contains(err.Error(), long) {
		t.Error("a long --route value is quoted in full")
	}
}

// TestEffectiveInputErrors checks ErrInput for a missing Bundle and for a
// Route whose chain the pipeline did not compute, and ErrFormat for a
// YAML table.
func TestEffectiveInputErrors(t *testing.T) {
	if _, err := Effective(nil, "x", nil); !errors.Is(err, ErrInput) {
		t.Errorf("Effective(nil) error = %v, want ErrInput", err)
	}
	r := shop(t, "prod")
	v := *r.validated
	v.Chains = maps.Clone(v.Chains)
	delete(v.Chains, "cart-grpc")
	if _, err := Effective(&v, "cart-grpc", nil); !errors.Is(err, ErrInput) {
		t.Errorf("Effective without a chain error = %v, want ErrInput", err)
	}
	if _, err := Effective(&hub.Validated{}, "x", nil); !errors.Is(err, ErrInput) {
		t.Errorf("Effective(no Bundle) error = %v, want ErrInput", err)
	}
	var buf bytes.Buffer
	if err := WriteEffective(&buf, nil, FormatYAML); !errors.Is(err, ErrFormat) || buf.Len() > 0 {
		t.Errorf("WriteEffective(yaml) error = %v, wrote %d bytes; want ErrFormat and nothing", err, buf.Len())
	}
	if err := WriteEffective(failWriter{}, nil, FormatText); err == nil {
		t.Error("WriteEffective to a failing writer reported no error")
	}
}
