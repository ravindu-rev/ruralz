// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// dagSchema returns a one-kind schema (kind K) whose spec is $defs/d0 and
// whose definitions d0 to d<depth-1> each reference the next under two
// members of nameLen characters: depth+2 resolved nodes per member name,
// but 2^(depth+1)-1 schema paths below spec.
func dagSchema(depth, nameLen int) string {
	a, b := strings.Repeat("a", nameLen), strings.Repeat("b", nameLen)
	defs := make([]string, 0, depth+1)
	for i := range depth {
		next := fmt.Sprintf("#/$defs/d%d", i+1)
		defs = append(defs, fmt.Sprintf(`"d%d":{"type":"object","properties":{"%s":{"$ref":"%s"},"%s":{"$ref":"%s"}}}`,
			i, a, next, b, next))
	}
	defs = append(defs, fmt.Sprintf(`"d%d":{"type":"string"}`, depth))
	return `{"properties":{"kind":{"enum":["K"]}},"allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},` +
		`"then":{"properties":{"spec":{"$ref":"#/$defs/d0"}}}}],"$defs":{` + strings.Join(defs, ",") + `}}`
}

func TestLoadBoundsFieldEnumeration(t *testing.T) {
	// Fields lists paths, not nodes: a DAG of definitions multiplies them.
	// Load rejects a schema past maxFields fields or maxFieldBytes bytes of
	// schema paths, without enumerating further, so Fields and FieldsSeq
	// stay bounded for every Index (the depth-22 case had 8,388,609 fields).
	for _, tc := range []struct {
		name           string
		depth, nameLen int
	}{
		{"fields depth 22", 22, 1},
		{"fields depth 15", 15, 1}, // 65,537 fields, one past the bound
		{"path bytes", 10, 300},    // 2,049 fields of long paths
	} {
		_, err := Load("test/v1", []byte(dagSchema(tc.depth, tc.nameLen)))
		if !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "schema paths") {
			t.Errorf("%s: Load = %v, want the field enumeration bound", tc.name, err)
		}
	}
	// Under the bounds the schema loads and enumerates completely.
	x := mustLoad(t, dagSchema(12, 1))
	fields := x.Fields()
	if want := 1<<13 - 1 + 2; len(fields) != want {
		t.Fatalf("Fields() = %d fields, want %d", len(fields), want)
	}
	// The committed schema leaves the bounds ample headroom.
	committed, size := 0, 0
	for f := range v1(t).FieldsSeq() {
		committed++
		size += len(f.Path)
	}
	if committed > maxFields/50 || size > maxFieldBytes/50 {
		t.Errorf("the committed schema enumerates %d fields of %d path bytes; the bounds leave too little headroom",
			committed, size)
	}
}

// whereText writes the dispatches of f as "<path>:<member>=<value>,..."
// or "<path>:none", joined by ";".
func whereText(f Field) string {
	var out []string
	for _, d := range f.Where {
		if d.Conditions == nil {
			out = append(out, d.Path+":none")
			continue
		}
		var conds []string
		for _, c := range d.Conditions {
			conds = append(conds, c.Member+"="+jsonText(c.Value))
		}
		out = append(out, d.Path+":"+strings.Join(conds, ","))
	}
	return strings.Join(out, ";")
}

// matchingTogether dispatches spec on rules whose conditions hold
// together: the second repeats the first's, and the third tests a superset
// of them, so an object with t "a" matches the first two and one with t "a"
// and u 1 all three.
const matchingTogether = `{"properties":{"kind":{"enum":["K"]}},
  "allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"spec":{"$ref":"#/$defs/S"}}}}],
  "$defs":{"S":{"type":"object","additionalProperties":false,"properties":{"t":{"type":"string"},"u":{"type":"integer"}},"allOf":[
    {"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"then":{"properties":{"x":{}}}},
    {"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"then":{"properties":{"y":{}}},"else":{"properties":{"n":{}}}},
    {"if":{"properties":{"t":{"const":"a"},"u":{"const":1}},"required":["t","u"]},"then":{"properties":{"z":{}}},"else":{"properties":{"m":{}}}}]}}}`

func TestFieldsRulesMatchingTogether(t *testing.T) {
	// Each rule's Dispatch is the result for an object with just the
	// members it tests: every rule those members satisfy applies its then
	// branch, the others their else branches, and a rule repeating an
	// earlier one's conditions adds nothing. Every field resolves by Lookup
	// to the node Fields lists, and Select returns one node for each such
	// object on every call.
	x := mustLoad(t, matchingTogether)
	var got []string
	key := freeName(x)
	for _, f := range x.Fields() {
		if len(f.Where) == 0 {
			continue
		}
		got = append(got, f.Path+" "+whereText(f))
		res, path := instanceFor(f, key)
		if info, ok := x.Lookup(f.Kind, res, path); !ok || info.Node != f.Node {
			t.Errorf("Lookup(%s %s) = %v %v, want the listed node", f.Path, whereText(f), info.Node.PropertyNames(), ok)
		}
	}
	want := []string{
		`spec.m spec:t="a"`,
		`spec.x spec:t="a"`,
		`spec.y spec:t="a"`,
		`spec.x spec:t="a",u=1`,
		`spec.y spec:t="a",u=1`,
		`spec.z spec:t="a",u=1`,
		`spec.m spec:none`,
		`spec.n spec:none`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("dispatch fields =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	spec, _ := x.Spec("K")
	for _, tc := range []struct {
		inst  string
		names []string
	}{
		{`{"t":"a"}`, []string{"m", "t", "u", "x", "y"}},
		{`{"t":"a","u":1}`, []string{"t", "u", "x", "y", "z"}},
		{`{"t":"b"}`, []string{"m", "n", "t", "u"}},
	} {
		sel := spec.Select(parseTree(t, tc.inst))
		if again := spec.Select(parseTree(t, tc.inst)); again != sel || !slices.Equal(sel.PropertyNames(), tc.names) {
			t.Errorf("Select(%s) = %v (same node twice: %v), want %v", tc.inst, sel.PropertyNames(), again == sel, tc.names)
		}
	}
}

func TestFieldsTestedMemberHoldsConst(t *testing.T) {
	// A branch that gives the member its rule tests a schema of its own
	// (here an annotated object with a dispatch of its own) lists the member
	// under the rule's Dispatch, where it holds the scalar const, and no
	// position below it; below the member as an object, which matches no
	// rule, the positions are listed without that Dispatch.
	x := mustLoad(t, `{"properties":{"kind":{"enum":["K"]}},
	  "allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"properties":{"spec":{"$ref":"#/$defs/S"}}}}],
	  "$defs":{
	    "S":{"type":"object","properties":{"t":{"type":["string","object"],"properties":{"u":{"type":"string"}}}},
	      "allOf":[{"if":{"properties":{"t":{"const":"a"}},"required":["t"]},"then":{"properties":{"t":{"$ref":"#/$defs/T"}}}}]},
	    "T":{"x-ruralz-impact":["traffic"],"additionalProperties":false,"properties":{"v":{}},
	      "allOf":[{"if":{"properties":{"u":{"const":"b"}},"required":["u"]},"then":{"properties":{"w":{}}}}]}}}`)
	var got []string
	key := freeName(x)
	for _, f := range x.Fields() {
		got = append(got, strings.TrimSpace(f.Path+" "+whereText(f)))
		res, path := instanceFor(f, key)
		if info, ok := x.Lookup(f.Kind, res, path); !ok || info.Node != f.Node {
			t.Errorf("Lookup(%s %s) = %v, want the listed node", f.Path, whereText(f), ok)
		}
		if f.Path == "spec.t" && len(f.Where) > 0 && f.Node.Keywords().Impact != ImpactTraffic {
			t.Errorf("spec.t under %s: impact %s, want traffic", whereText(f), f.Node.Keywords().Impact)
		}
	}
	want := []string{"", "kind", "spec", "spec.t", "spec.t.u", `spec.t spec:t="a"`}
	if !slices.Equal(got, want) {
		t.Errorf("Fields =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFieldsSeq(t *testing.T) {
	// FieldsSeq yields what Fields lists, in order, and stops early.
	for _, x := range []*Index{mustLoad(t, synthetic), v1(t)} {
		want := x.Fields()
		var got []Field
		for f := range x.FieldsSeq() {
			got = append(got, f)
		}
		if !slices.EqualFunc(got, want, func(a, b Field) bool {
			return a.Kind == b.Kind && a.Path == b.Path && a.Node == b.Node && len(a.Where) == len(b.Where)
		}) {
			t.Errorf("FieldsSeq differs from Fields (%d vs %d fields)", len(got), len(want))
		}
		n := 0
		for range x.FieldsSeq() {
			if n++; n == 3 {
				break
			}
		}
		if n != 3 {
			t.Errorf("early stop after %d fields", n)
		}
	}
	// A break at any position stops the enumeration, including one at a
	// field just before an annotating dispatch branch is emitted: a later
	// call of yield would panic (range function continued iteration).
	for _, src := range []string{keywordSchema, synthetic} {
		x := mustLoad(t, src)
		total := len(x.Fields())
		for stop := 1; stop <= total; stop++ {
			n := 0
			for range x.FieldsSeq() {
				if n++; n == stop {
					break
				}
			}
			if n != stop {
				t.Errorf("break after field %d of %d: %d fields yielded", stop, total, n)
			}
		}
	}
	// A yielded Field owns its Where: changing it changes no other field
	// of the same enumeration (several fields share one dispatch).
	var withWhere []Field
	for f := range mustLoad(t, synthetic).FieldsSeq() {
		if len(f.Where) > 0 {
			withWhere = append(withWhere, f)
		}
	}
	if len(withWhere) < 2 {
		t.Fatalf("%d fields under a dispatch", len(withWhere))
	}
	withWhere[0].Where[0].Path = "mutated"
	for _, f := range withWhere[1:] {
		if f.Where[0].Path == "mutated" {
			t.Fatalf("%s: Where aliases another field's", f.Path)
		}
	}
}
