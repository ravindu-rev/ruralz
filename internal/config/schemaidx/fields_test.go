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
