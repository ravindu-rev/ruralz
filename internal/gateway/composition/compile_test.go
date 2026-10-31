// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestCompileRefuses checks that Compile refuses, with the code validation
// reports first, every composition validation refuses, so the request path
// never meets one (docs/architecture/02-configuration-model.md "Route").
func TestCompileRefuses(t *testing.T) {
	const (
		agg = v1alpha1.CompositionModeAggregate
		seq = v1alpha1.CompositionModeSequential
	)
	big := func(n v1alpha1.ByteSize) v1alpha1.CompositionStep {
		s := stepTo("big", "a", "/a")
		s.MaxBodyBytes = &n
		return s
	}
	withWhen := stepTo("w", "a", "/a")
	withWhen.When = "true"
	both := stepTo("both", "a", "/a")
	both.PathExpression = `"/b"`
	badTarget := stepTo("t", "a", "/a")
	badTarget.Target = "data..id"
	noGroup := stepTo("c", "a", "/a")
	noGroup.Collection = ptr(true)
	badCEL := v1alpha1.CompositionStep{Name: "e", Upstream: "a", PathExpression: `request.nope`}
	stepsInAggregate := v1alpha1.CompositionStep{Name: "s", Upstream: "a", PathExpression: `"/" + steps.x.status`}
	tests := []struct {
		name string
		c    *v1alpha1.Composition
		code string
	}{
		{"no steps", composition(agg), codeSchema},
		{"unknown mode", composition("parallel", stepTo("a", "a", "/a")), codeSchema},
		{"more steps than maxCompositionSteps", composition(agg, stepTo("1", "a", "/a"), stepTo("2", "a", "/a"), stepTo("3", "a", "/a")), codeSteps},
		{"explicit bodies over maxResponseBodyBytes", composition(agg, big(65<<10)), codeStepBodies},
		{"no share left", composition(agg, big(64<<10), stepTo("free", "a", "/a")), codeStepBodies},
		{"maxBodyBytes below 1 byte", composition(agg, big(0)), codeSchema},
		{"unknown Upstream", composition(agg, stepTo("a", "nope", "/a")), codeReference},
		{"path and pathExpression", composition(agg, both), codeSchema},
		{"literal path without leading slash", composition(agg, stepTo("p", "a", "orders")), codeSchema},
		{"literal path with a dot segment", composition(agg, stepTo("p", "a", "/a/../b")), codeSchema},
		{"literal path with an encoded dot segment", composition(agg, stepTo("p", "a", "/a/%2E%2e/b")), codeSchema},
		{"literal path with an encoded NUL", composition(agg, stepTo("p", "a", "/a%00")), codeSchema},
		{"literal path with an encoded backslash", composition(agg, stepTo("p", "a", "/a%5cb")), codeSchema},
		{"literal path with an invalid escape", composition(agg, stepTo("p", "a", "/a%zz")), codeSchema},
		{"literal query with a control byte", composition(agg, stepTo("p", "a", "/a?x=\x01")), codeSchema},
		{"when on an aggregate step", composition(agg, withWhen), codeSchema},
		{"invalid target", composition(agg, badTarget), codeSchema},
		{"collection without group", composition(agg, noGroup), codeSchema},
		{"CEL type error", composition(seq, badCEL), codeCELInvalid},
		{"steps outside a sequential composition", composition(agg, stepsInAggregate), codeCELInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			f.limits.MaxCompositionSteps = 2
			_, err := f.e.Compile(f.input(tt.c))
			if got := codeOf(err); got != tt.code {
				t.Errorf("Compile = %v (code %q), want %s", err, got, tt.code)
			}
		})
	}
}

// TestStepBodies checks each step's maxBodyBytes: its explicit value, and
// for the others an equal share of what limits.maxResponseBodyBytes leaves
// after the explicit values (docs/architecture/02-configuration-model.md
// "Body buffering and limits").
func TestStepBodies(t *testing.T) {
	four := v1alpha1.ByteSize(4 << 10)
	steps := []v1alpha1.CompositionStep{
		{Name: "a", MaxBodyBytes: &four},
		{Name: "b"},
		{Name: "c"},
	}
	got, err := stepBodies(steps, 10<<10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{4 << 10, 3 << 10, 3 << 10}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("stepBodies = %v, want %v", got, want)
	}
	f := newFixture(t, "a")
	comp := f.compile(composition(v1alpha1.CompositionModeAggregate, stepTo("x", "a", "/x"), stepTo("y", "a", "/y")))
	if got := comp.steps[0].maxBody; got != 32<<10 {
		t.Errorf("default share = %d, want half of 64 KiB", got)
	}
}

// TestRequestGate checks Composition.RequestGate: the client body is gated
// when two or more steps may send it in one request (a step without a
// method always may; one with a method only when that method can reach
// the Route, OPTIONS always), never for that reason in a conditional
// composition, and whenever an expression reads request.body.
func TestRequestGate(t *testing.T) {
	post := stepTo("post", "a", "/p")
	post.Method = "POST"
	opts := stepTo("opts", "a", "/o")
	opts.Method = "OPTIONS"
	readsBody := v1alpha1.CompositionStep{Name: "rb", Upstream: "a", PathExpression: `"/" + string(request.body.id)`}
	tests := []struct {
		name    string
		mode    v1alpha1.CompositionMode
		methods []string
		steps   []v1alpha1.CompositionStep
		want    bool
	}{
		{"two steps with the incoming method", v1alpha1.CompositionModeAggregate, nil, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), stepTo("y", "a", "/y")}, true},
		{"one step only", v1alpha1.CompositionModeAggregate, nil, []v1alpha1.CompositionStep{stepTo("x", "a", "/x")}, false},
		{"POST step on a GET-only Route", v1alpha1.CompositionModeAggregate, []string{"GET"}, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), post}, false},
		{"POST step on a Route open to POST", v1alpha1.CompositionModeAggregate, []string{"GET", "POST"}, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), post}, true},
		{"POST step on a Route with any method", v1alpha1.CompositionModeSequential, nil, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), post}, true},
		{"OPTIONS step on a GET-only Route", v1alpha1.CompositionModeAggregate, []string{"GET"}, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), opts}, true},
		{"conditional runs one step", v1alpha1.CompositionModeConditional, nil, []v1alpha1.CompositionStep{stepTo("x", "a", "/x"), stepTo("y", "a", "/y")}, false},
		{"an expression reads request.body", v1alpha1.CompositionModeConditional, nil, []v1alpha1.CompositionStep{readsBody}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t, "a")
			in := f.input(composition(tt.mode, tt.steps...))
			in.Methods = tt.methods
			comp, err := f.e.Compile(in)
			if err != nil {
				t.Fatal(err)
			}
			if got := comp.RequestGate(); got != tt.want {
				t.Errorf("RequestGate() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestGates checks which steps read their body whole: every aggregate
// step, a step that sets target, select, rename or group, and in a
// sequential composition a step whose body a later expression reads
// through steps (a constant key names it; a computed key or the whole map
// gates every earlier step), while a later read of status or headers
// gates nothing.
func TestGates(t *testing.T) {
	seq := v1alpha1.CompositionModeSequential
	readBody := v1alpha1.CompositionStep{Name: "c", Upstream: "a", PathExpression: `"/c/" + string(steps.a.body.id)`}
	readStatus := v1alpha1.CompositionStep{Name: "c", Upstream: "a", Path: "/c", When: `steps.a.status == 200`}
	readAny := v1alpha1.CompositionStep{Name: "c", Upstream: "a", Path: "/c", When: `steps.size() == 2`}
	grouped := stepTo("g", "a", "/g")
	grouped.Group = "g"
	tests := []struct {
		name  string
		c     *v1alpha1.Composition
		gates []bool
	}{
		{"aggregate", composition(v1alpha1.CompositionModeAggregate, stepTo("a", "a", "/a"), stepTo("b", "a", "/b")), []bool{true, true}},
		{"sequential, nothing read", composition(seq, stepTo("a", "a", "/a"), stepTo("b", "a", "/b")), []bool{false, false}},
		{"group set", composition(seq, grouped, stepTo("b", "a", "/b")), []bool{true, false}},
		{"a later step reads the body", composition(seq, stepTo("a", "a", "/a"), stepTo("b", "a", "/b"), readBody), []bool{true, false, false}},
		{"a later step reads the status", composition(seq, stepTo("a", "a", "/a"), stepTo("b", "a", "/b"), readStatus), []bool{false, false, false}},
		{"a later step reads the whole map", composition(seq, stepTo("a", "a", "/a"), stepTo("b", "a", "/b"), readAny), []bool{true, true, false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			comp := newFixture(t, "a").compile(tt.c)
			for i, want := range tt.gates {
				if got := comp.steps[i].gate(); got != want {
					t.Errorf("step %q gate() = %v, want %v", comp.steps[i].name, got, want)
				}
			}
		})
	}
}
