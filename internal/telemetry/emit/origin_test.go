// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package emit

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Spec 09 req 44 and test 12: origin classification over errcode.All().
// Every RZ-UP-* code and RZ-AI-004, -005 and -013 is dependency, any other
// code (RZ-STS included) is node, and a passed-through Upstream status is
// upstream whatever code the exchange carries.
func TestOriginOf_Test12(t *testing.T) {
	dependency := map[string]bool{"RZ-AI-004": true, "RZ-AI-005": true, "RZ-AI-013": true}
	all := errcode.All()
	if len(all) == 0 {
		t.Fatal("errcode.All() is empty")
	}
	areas := map[errcode.Area]int{}
	for _, c := range all {
		want := OriginNode
		if c.Area == errcode.AreaUP || dependency[c.ID] {
			want = OriginDependency
		}
		if got := OriginOf(c.ID, false); got != want {
			t.Errorf("OriginOf(%q, false) = %d, want %d", c.ID, got, want)
		}
		if got := OriginOf(c.ID, true); got != OriginUpstream {
			t.Errorf("OriginOf(%q, true) = %d, want upstream", c.ID, got)
		}
		areas[c.Area]++
	}
	// The table covers the areas the rule distinguishes.
	for _, a := range []errcode.Area{errcode.AreaUP, errcode.AreaAI, errcode.AreaSTS, errcode.AreaRT} {
		if areas[a] == 0 {
			t.Errorf("no %s code in errcode.All()", a)
		}
	}
	for code, want := range map[string]int{
		"":           OriginNode, // a Node-generated response without a code
		"RZ-AI-003":  OriginNode,
		"RZ-AI-014":  OriginNode,
		"RZ-STS-001": OriginNode,
	} {
		if got := OriginOf(code, false); got != want {
			t.Errorf("OriginOf(%q, false) = %d, want %d", code, got, want)
		}
	}
	if got := OriginOf("", true); got != OriginUpstream {
		t.Errorf("passed-through status = %d, want upstream", got)
	}
}
