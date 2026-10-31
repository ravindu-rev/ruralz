// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package overlay

import (
	"math/rand/v2"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// twoRounds is a rendered schema whose dispatch takes two rounds: mode a
// selects a branch that declares sub and carries its own rule on sub,
// which makes doc a JSONSchemaDocument. selectOf reads sub only after
// the first round shows the branch declares it.
const twoRounds = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing"]}},
  "allOf": [{"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}}],
  "$defs": {
    "JSONSchemaDocument": {"type": ["object", "boolean"]},
    "Thing": {"type": "object", "properties": {"kind": {"const": "Thing"}, "spec": {"$ref": "#/$defs/Spec"}}},
    "Spec": {"type": "object", "properties": {"mode": {"type": "string"}, "n": {"type": "integer"}},
      "allOf": [
        {"if": {"properties": {"mode": {"const": "a"}}, "required": ["mode"]},
         "then": {"properties": {"sub": {"type": "string"}},
           "allOf": [{"if": {"properties": {"sub": {"const": "x"}}, "required": ["sub"]}, "then": {"properties": {"doc": {"$ref": "#/$defs/JSONSchemaDocument"}}}}]}},
        {"if": {"properties": {"n": {"const": 3}}, "required": ["n"]}, "then": {"properties": {"three": {"type": "object"}}}}]}
  }
}`

// dispatching returns the schema nodes of x that carry if/then rules
// testing a declared member, with the members and values the rules test
// (string constants, as DispatchValues lists them) and the declared
// members.
func dispatching(x *schemaidx.Index) map[*schemaidx.Node]map[string][]string {
	out := map[*schemaidx.Node]map[string][]string{}
	for f := range x.FieldsSeq() {
		for _, name := range f.Node.PropertyNames() {
			if vs := f.Node.DispatchValues(name); len(vs) > 0 {
				if out[f.Node] == nil {
					out[f.Node] = map[string][]string{}
				}
				out[f.Node][name] = vs
			}
		}
	}
	return out
}

// instances returns objects for static's dispatch: each tested member
// given each of its values, another value, a number, null or nothing,
// among declared and undeclared members before and after it, with
// repeated keys and removed (nil) members, short and long.
func instances(r *rand.Rand, static *schemaidx.Node, tested map[string][]string, extra map[string][]any) []*tree.Node {
	var names []string
	for name := range tested {
		names = append(names, name)
	}
	for name := range extra {
		names = append(names, name)
	}
	slices.Sort(names)
	declared := static.PropertyNames()
	value := func(name string) *tree.Node {
		vs := tested[name]
		switch k := r.IntN(len(vs) + 4); {
		case k < len(vs):
			return sv(vs[k])
		case k == len(vs):
			return sv("other")
		case k == len(vs)+1:
			return iv(3)
		case k == len(vs)+2:
			if ev := extra[name]; len(ev) > 0 {
				switch v := ev[r.IntN(len(ev))].(type) {
				case bool:
					return &tree.Node{Kind: tree.KindBool, Bool: v}
				case int:
					return iv(v)
				case string:
					return sv(v)
				}
			}
			return nullv()
		default:
			return obj()
		}
	}
	var out []*tree.Node
	for range 400 {
		inst := obj()
		width := []int{2, linearScan, linearScan + 1, 3 * linearScan}[r.IntN(4)]
		for range width {
			switch k := r.IntN(10); {
			case k < 4 && len(names) > 0:
				name := names[r.IntN(len(names))]
				inst.Members = append(inst.Members, tree.Member{Key: name, Value: value(name)})
			case k < 6 && len(declared) > 0:
				inst.Members = append(inst.Members, tree.Member{Key: declared[r.IntN(len(declared))], Value: iv(1)})
			case k < 7 && len(names) > 0:
				// A member mergeMap removed: nil, read as absent.
				inst.Members = append(inst.Members, tree.Member{Key: names[r.IntN(len(names))]})
			case k < 8:
				inst.Members = append(inst.Members, tree.Member{Key: "x" + strconv.Itoa(r.IntN(5))})
			default:
				inst.Members = append(inst.Members, tree.Member{Key: "u" + strconv.Itoa(r.IntN(100)), Value: sv("v")})
			}
		}
		out = append(out, inst)
	}
	return out
}

// present returns inst without its removed (nil) members, as mergeMap's
// first pass leaves an object that selectOf reads.
func present(inst *tree.Node) *tree.Node {
	c := obj()
	for _, m := range inst.Members {
		if m.Value != nil {
			c.Members = append(c.Members, m)
		}
	}
	return c
}

// TestSelectOf covers the bounded dispatch of selectOf, so schemaidx
// Select never reads every member once per rule, uncounted: on the
// committed schema and on synthetic ones with a boolean and a number
// constant and two dispatch rounds, selectOf returns what Select returns
// for the object without its removed members, for short and long
// objects, and Select reads at most the declared members.
func TestSelectOf(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: deterministic test data
	committed, _ := schemas(t).Index(v1)
	nested, err := schemaidx.Load("test/v1", []byte(nestedDispatch))
	if err != nil {
		t.Fatal(err)
	}
	two, err := schemaidx.Load("test/v1", []byte(twoRounds))
	if err != nil {
		t.Fatal(err)
	}
	extras := map[string][]any{"flag": {true, false}, "n": {3, 4}, "sub": {"x", "y"}}
	checked := 0
	for name, x := range map[string]*schemaidx.Index{"committed": committed, "nestedDispatch": nested, "twoRounds": two} {
		for static, tested := range dispatching(x) {
			for _, inst := range instances(r, static, tested, extras) {
				a, p := meterProbe()
				got := a.selectOf(static, inst)
				if want := static.Select(present(inst)); !sameSchema(got, want) {
					t.Fatalf("%s: selectOf(%s) differs from Select", name, dump(present(inst)))
				}
				if p.selectWidth > max(linearScan, len(static.PropertyNames())+len(extras)) {
					t.Errorf("%s: Select read %d members", name, p.selectWidth)
				}
				checked++
			}
		}
	}
	if checked < 1000 {
		t.Errorf("checked %d instances, want the dispatching nodes of three schemas", checked)
	}
	a, _ := meterProbe()
	if a.selectOf(nil, obj("a", iv(1))) != nil || a.selectOf(committed.Root(), nil) != committed.Root() {
		t.Error("selectOf of a nil schema or instance differs from Select")
	}
	// The second round reads a member only the first round's branch
	// declares: without the second pass, sub would be missing.
	spec, _ := two.Spec("Thing")
	inst := obj("mode", sv("a"))
	for i := range linearScan {
		inst.Members = append(inst.Members, tree.Member{Key: "u" + strconv.Itoa(i), Value: iv(i)})
	}
	inst.Members = append(inst.Members, tree.Member{Key: "sub", Value: sv("x")})
	if sel := a.selectOf(spec, inst); !opaque(memberSchema(sel, "doc")) {
		t.Error("two rounds: doc is not a JSONSchemaDocument after mode a and sub x")
	}
}

// sameSchema reports two dispatch results that are one schema: the same
// node, or, since schemaidx builds a result of two matching rules anew
// on every Select call, nodes with the same definition and declared
// members, each the same node.
func sameSchema(a, b *schemaidx.Node) bool {
	if a == b {
		return true
	}
	if a == nil || b == nil || a.Def() != b.Def() || !slices.Equal(a.PropertyNames(), b.PropertyNames()) {
		return false
	}
	for name, pa := range a.Properties() {
		if pb, _ := b.Property(name); pa != pb && (pa.Def() != pb.Def() || !slices.Equal(pa.PropertyNames(), pb.PropertyNames())) {
			return false
		}
	}
	return true
}

// TestDispatchReadsDeclared pins what selectOf relies on, over the
// committed schema: every member an if/then rule tests is a declared
// property of the object whose rule it is, at every schema path where a
// rule applies (TestDispatchTestsScalars adds that every member a rule
// requires is one it tests against a constant). A schemaidx API exporting
// the members its rules read would free the proxy from this assumption.
func TestDispatchReadsDeclared(t *testing.T) {
	for _, x := range []*schemaidx.Index{func() *schemaidx.Index { x, _ := schemas(t).Index(v1); return x }()} {
		declared := map[string]map[string]bool{}
		for f := range x.FieldsSeq() {
			id := f.Kind + " " + f.Path
			if declared[id] == nil {
				declared[id] = map[string]bool{}
			}
			for _, name := range f.Node.PropertyNames() {
				declared[id][name] = true
			}
		}
		rules := 0
		for f := range x.FieldsSeq() {
			for _, d := range f.Where {
				for _, c := range d.Conditions {
					rules++
					if !declared[f.Kind+" "+d.Path][c.Member] {
						t.Errorf("%s %q: a rule tests %s, which is not declared there", f.Kind, d.Path, c.Member)
					}
				}
			}
		}
		if rules == 0 {
			t.Fatal("no dispatch rule found")
		}
	}
}
