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

// Program compiles src at site with a new Compiler; it stops the test on
// any Issue.
func Program(tb testing.TB, site expr.Site, src string) expr.Program {
	tb.Helper()
	prg, iss := Compiler(tb).NewBuilder(nil).Compile(site, src)
	if prg == nil {
		tb.Fatalf("celtest: compile %q at %s: %+v", src, site.Place, iss)
	}
	return prg
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
