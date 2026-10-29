// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"fmt"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// TestBuilderCompilesEachSiteOnce checks that one site and source share one
// program, and that a different place or environment does not.
func TestBuilderCompilesEachSiteOnce(t *testing.T) {
	c := newCompiler(t)
	b := c.NewBuilder(nil, expr.InputCaps{})
	src := `consumer == null ? source.ip : consumer.name`
	p1, _ := b.Compile(at(expr.PlaceRateLimitKey), src)
	p2, _ := b.Compile(expr.Site{Place: expr.PlaceRateLimitKey, Scope: phase.ScopeRoute}, src)
	p3, _ := b.Compile(at(expr.PlaceQuotaKey), src)
	if p1 == nil || p1 != p2 {
		t.Errorf("same site and source: %p, %p", p1, p2)
	}
	if p3 == nil || p3 == p1 {
		t.Errorf("another place shares the program: %p, %p", p1, p3)
	}
	when := `request.method == "GET"`
	req, _ := b.Compile(expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnRequestHeaders}, when)
	resp, _ := b.Compile(expr.Site{Place: expr.PlacePolicyWhen, FirstPhase: phase.OnResponse}, when)
	if req == nil || resp == nil || req == resp {
		t.Errorf("environments with and without response share a program: %p, %p", req, resp)
	}
	if n := b.Build().Len(); n != 4 {
		t.Errorf("Build().Len() = %d, want 4", n)
	}
}

// TestBuilderReusesPreviousSet checks that a Hot Reload reuses the active
// snapshot's programs and drops the ones it no longer references.
func TestBuilderReusesPreviousSet(t *testing.T) {
	c := newCompiler(t)
	keep, drop := `request.headers["x-tenant"] == "acme"`, `request.method == "GET"`
	b1 := c.NewBuilder(nil, expr.InputCaps{})
	kept, _ := b1.Compile(at(expr.PlaceRouteMatchWhen), keep)
	if _, iss := b1.Compile(at(expr.PlaceRouteMatchWhen), drop); iss != nil {
		t.Fatalf("Compile(%q) = %+v", drop, iss)
	}
	set1 := b1.Build()
	b2 := c.NewBuilder(set1, expr.InputCaps{})
	again, _ := b2.Compile(at(expr.PlaceRouteMatchWhen), keep)
	if again != kept {
		t.Errorf("reload recompiled %q: %p, want %p", keep, again, kept)
	}
	fresh, _ := b2.Compile(at(expr.PlaceRouteMatchWhen), `request.method == "POST"`)
	if fresh == nil {
		t.Fatal("new expression did not compile")
	}
	set2 := b2.Build()
	if set2.Len() != 2 || set1.Len() != 2 {
		t.Errorf("Len = %d (prev %d), want 2 and 2", set2.Len(), set1.Len())
	}
	if _, ok := set2.(*ProgramSet).progs[programKey{site: siteKeyOf(t, at(expr.PlaceRouteMatchWhen)), src: drop}]; ok {
		t.Errorf("set2 keeps the unreferenced %q", drop)
	}
}

// siteKeyOf returns the site key of site.
func siteKeyOf(t *testing.T, site expr.Site) siteKey {
	t.Helper()
	se, ok := resolve(site)
	if !ok {
		t.Fatalf("resolve(%+v) failed", site)
	}
	return se.key()
}

// TestBuilderDoesNotKeepErrors checks that a rejected source has no program
// in the set and is checked again by the next Builder.
func TestBuilderDoesNotKeepErrors(t *testing.T) {
	c := newCompiler(t)
	bad := `response.status == 200`
	b1 := c.NewBuilder(nil, expr.InputCaps{})
	if prg, iss := b1.Compile(at(expr.PlaceRouteMatchWhen), bad); prg != nil || len(iss) == 0 {
		t.Fatalf("Compile(%q) = %v, %+v; want an Issue", bad, prg, iss)
	}
	set := b1.Build()
	if set.Len() != 0 {
		t.Errorf("set holds %d programs, want 0", set.Len())
	}
	if prg, iss := c.NewBuilder(set, expr.InputCaps{}).Compile(at(expr.PlaceRouteMatchWhen), bad); prg != nil || len(iss) == 0 {
		t.Errorf("second Builder: %v, %+v; want an Issue", prg, iss)
	}
}

// TestBuilderStepsAreSiteSpecific checks that a source valid after one set of
// earlier steps is rejected after another.
func TestBuilderStepsAreSiteSpecific(t *testing.T) {
	c := newCompiler(t)
	b := c.NewBuilder(nil, expr.InputCaps{})
	src := `steps.order.status == 200`
	if prg, iss := b.Compile(sequential(expr.PlaceStepWhen, "order"), src); prg == nil {
		t.Errorf("after order: %+v", iss)
	}
	if prg, iss := b.Compile(sequential(expr.PlaceStepWhen, "stock"), src); prg != nil || len(iss) == 0 {
		t.Errorf("after stock: %v, %+v; want an Issue", prg, iss)
	}
}

// TestBuilderConcurrentCompile compiles overlapping expressions from many
// goroutines and expects one program per distinct source.
func TestBuilderConcurrentCompile(t *testing.T) {
	c := newCompiler(t)
	b := c.NewBuilder(nil, expr.InputCaps{})
	const distinct = 20
	got := make([][]expr.Program, 16)
	var wg sync.WaitGroup
	for g := range got {
		wg.Go(func() {
			for i := range distinct * 3 {
				src := fmt.Sprintf(`request.headers["x-%d"] == "v"`, i%distinct)
				prg, iss := b.Compile(at(expr.PlaceRouteMatchWhen), src)
				if prg == nil {
					t.Errorf("Compile(%q) = %+v", src, iss)
					return
				}
				got[g] = append(got[g], prg)
			}
		})
	}
	wg.Wait()
	for g := range got {
		for i, prg := range got[g] {
			if prg != got[0][i] {
				t.Fatalf("goroutine %d, expression %d: %p, want %p", g, i, prg, got[0][i])
			}
		}
	}
	if n := b.Build().Len(); n != distinct {
		t.Errorf("Build().Len() = %d, want %d", n, distinct)
	}
}

// TestProgramSetLenOfNil checks that a nil set is empty.
func TestProgramSetLenOfNil(t *testing.T) {
	var s *ProgramSet
	if s.Len() != 0 {
		t.Errorf("nil Len = %d", s.Len())
	}
	if b := newCompiler(t).NewBuilder(s, expr.InputCaps{}); b == nil {
		t.Error("NewBuilder(nil set) = nil")
	}
}
