// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package celtest provides the CEL module to other packages' tests: a
// Compiler, programs compiled at a site, decoded JSON values, and Route and
// Consumer views prepared as the snapshot compiler prepares them, so a
// test evaluates real CEL without the configuration pipeline.
package celtest

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/cel/celtypes"
	"github.com/ravindu-rev/ruralz/internal/expr"
)

// Compiler returns a new CEL compiler; it stops the test on failure.
func Compiler(tb testing.TB) *cel.Compiler {
	tb.Helper()
	c, err := cel.New()
	if err != nil {
		tb.Fatalf("celtest: %v", err)
	}
	return c
}

// At returns the site of place with no attachment refinement.
func At(place expr.PlaceID) expr.Site { return expr.Site{Place: place} }

// Program compiles src at site with a new Compiler and no input caps, so
// every input counts as unbounded; it stops the test on any Issue.
func Program(tb testing.TB, site expr.Site, src string) expr.Program {
	tb.Helper()
	prg, iss := Compiler(tb).NewBuilder(nil, expr.InputCaps{}).Compile(site, src)
	if prg == nil {
		tb.Fatalf("celtest: compile %q at %s: %+v", src, site.Place, iss)
	}
	return prg
}

// Caps returns the input caps a snapshot of the default Gateway passes: a
// 64 KiB request header block (limits.maxRequestHeaderBytes) with nothing
// added by Policies, claims of at most 4 × 16 KiB, and Route and Consumer
// data at the largest of routes and consumers, which must hold every Route
// and Consumer the test evaluates.
func Caps(routes []*expr.Route, consumers ...*expr.Consumer) expr.InputCaps {
	caps := expr.InputCaps{RequestHeaderBytes: 64 << 10, ClaimsBytes: 4 * 16 << 10}
	caps.Routes.Add(nil)
	for _, r := range routes {
		caps.Routes.Add(r)
	}
	caps.Consumers.Add(nil)
	for _, c := range consumers {
		caps.Consumers.Add(c)
	}
	return caps
}

// JSON decodes text as a body or claims document without a budget; it
// stops the test on failure.
func JSON(tb testing.TB, text string) expr.Value {
	tb.Helper()
	v, _, err := Compiler(tb).DecodeJSON([]byte(text), 0)
	if err != nil {
		tb.Fatalf("celtest: decode %q: %v", text, err)
	}
	return v
}

// Route returns the Route view of name and labels, prepared once as a
// snapshot prepares it.
func Route(name string, labels map[string]string) *expr.Route {
	r := &expr.Route{Name: name, Labels: labels}
	celtypes.PrepareRoute(r)
	return r
}

// Consumer returns a copy of c prepared once as a snapshot prepares it.
func Consumer(c expr.Consumer) *expr.Consumer {
	p := c
	celtypes.PrepareConsumer(&p)
	return &p
}
