// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
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
		want := emit.OriginNode
		if c.Area == errcode.AreaUP || dependency[c.ID] {
			want = emit.OriginDependency
		}
		if got := OriginOf(c.ID, false); got != want {
			t.Errorf("OriginOf(%q, false) = %d, want %d", c.ID, got, want)
		}
		if got := OriginOf(c.ID, true); got != emit.OriginUpstream {
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
		"":           emit.OriginNode, // a Node-generated response without a code
		"RZ-AI-003":  emit.OriginNode,
		"RZ-AI-014":  emit.OriginNode,
		"RZ-STS-001": emit.OriginNode,
	} {
		if got := OriginOf(code, false); got != want {
			t.Errorf("OriginOf(%q, false) = %d, want %d", code, got, want)
		}
	}
	if got := OriginOf("", true); got != emit.OriginUpstream {
		t.Errorf("passed-through status = %d, want upstream", got)
	}
}

// The classified origin indexes the origin label of
// ruralz_http_listener_requests_total.
func TestOriginLabel_Req44(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, b := admitBind(t, r, emit.Shape{Listeners: []string{"public"}})
	defer b.Release()
	lr := pl.Listener("public").Requests
	lr.Inc(0, emit.ProtoHTTP1, 200, OriginOf("", true))
	lr.Inc(0, emit.ProtoHTTP1, 502, OriginOf("RZ-UP-001", false))
	lr.Inc(0, emit.ProtoHTTP1, 503, OriginOf("RZ-STS-001", false))
	pts := collectNow(t, r, clk)
	for _, tt := range []struct{ class, origin string }{{"2xx", "upstream"}, {"5xx", "dependency"}, {"5xx", "node"}} {
		key := fmt.Sprintf(`%s{listener="public",origin=%q,protocol="http1",status_class=%q}`, catalog.HTTPListenerRequestsTotal, tt.origin, tt.class)
		if got := mustGet(t, pts, key).value; got != 1 {
			t.Errorf("%s = %v, want 1", key, got)
		}
	}
	// The catalog enumeration order is the emit index order.
	var origins []string
	for _, l := range catalog.Families() {
		if l.Name != catalog.HTTPListenerRequestsTotal {
			continue
		}
		for _, lab := range l.Labels {
			if lab.Name == "origin" {
				origins = lab.Values
			}
		}
	}
	if strings.Join(origins, ",") != "upstream,node,dependency" || emit.OriginUpstream != 0 || emit.OriginNode != 1 || emit.OriginDependency != 2 {
		t.Errorf("origin values %v do not match the emit indexes", origins)
	}
}
