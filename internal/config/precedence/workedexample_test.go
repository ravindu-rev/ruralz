// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// The example Bundle of docs/architecture/02-configuration-model.md
// ("Complete annotated example Bundle"), verbatim under
// testdata/shop-bundle: 02 test 15, 02 req 35 and 51 to 52, 04 req 40,
// 10 req 43.

const shopBundle = "testdata/shop-bundle"

// TestWorkedExample checks that the orders-summary rows equal the
// configuration model's "Worked example" table in Phase, Leg, Policy and
// From (02 req 52, golden R-79; the worked-example chain moved from
// WP-04).
func TestWorkedExample(t *testing.T) {
	b, files := loadBundle(t, shopBundle)
	chains, ds := stageI(t, b, files, 1)
	if len(ds) > 0 {
		t.Fatalf("example Bundle: unexpected diagnostics:\n%s", diagText(t, ds))
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
	var got [][4]string
	for _, r := range Rows(chains["orders-summary"], b) {
		got = append(got, [4]string{r.Phase, r.Leg, r.Policy, r.From})
	}
	if !slices.Equal(got, want) {
		t.Errorf("orders-summary rows:\n got %q\nwant %q", got, want)
	}
}

// TestShopBundleChains checks the other example Routes (02 test 15):
// support-chat replaces jwt-default with apikey-partner in slot auth and
// runs token-budget-partner in onRequestBody, onChunk and onLog; cart-grpc
// excludes cors-default and runs headers-security in onResponse only.
func TestShopBundleChains(t *testing.T) {
	b, files := loadBundle(t, shopBundle)
	chains, _ := stageI(t, b, files, 4)
	if len(chains) != 3 {
		t.Fatalf("chains = %d, want 3", len(chains))
	}

	chat := chains["support-chat"]
	if got := names(chat.Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"cors-default", "apikey-partner", "geo-block-default", "ratelimit-global"}) {
		t.Errorf("support-chat onRequestHeaders = %q", got)
	}
	for _, p := range []phase.Phase{phase.OnRequestBody, phase.OnChunk, phase.OnLog} {
		if got := names(chat.Client[p]); !slices.Equal(got, []string{"token-budget-partner"}) {
			t.Errorf("support-chat %s = %q, want [token-budget-partner]", p, got)
		}
	}
	if want := []hub.Removed{{Policy: "jwt-default", Slot: "auth", By: "apikey-partner", Reason: hub.Replaced}}; !slices.Equal(chat.Removed, want) {
		t.Errorf("support-chat Removed = %+v, want %+v", chat.Removed, want)
	}
	if e := chat.Client[phase.OnRequestHeaders][1]; e.Replaces != "jwt-default" || e.Slot != "auth" || e.Scope != phase.ScopeRoute || e.Position != 0 {
		t.Errorf("apikey-partner entry = %+v", e)
	}
	if len(chat.Legs) != 1 || chat.Legs[0].Upstream != "llm" {
		t.Errorf("support-chat legs = %+v, want [llm]", chat.Legs)
	}

	cart := chains["cart-grpc"]
	if got := names(cart.Client[phase.OnRequestHeaders]); !slices.Equal(got, []string{"jwt-default", "geo-block-default", "ratelimit-global"}) {
		t.Errorf("cart-grpc onRequestHeaders = %q", got)
	}
	if got := names(cart.Client[phase.OnResponse]); !slices.Equal(got, []string{"headers-security"}) {
		t.Errorf("cart-grpc onResponse = %q, want [headers-security]", got)
	}
	if want := []hub.Removed{{Policy: "cors-default", Slot: "cors", Reason: hub.Excluded}}; !slices.Equal(cart.Removed, want) {
		t.Errorf("cart-grpc Removed = %+v, want %+v", cart.Removed, want)
	}

	// The orders-summary legs follow the composition steps (02 req 41).
	orders := chains["orders-summary"]
	var legs []string
	for _, l := range orders.Legs {
		legs = append(legs, l.Upstream)
	}
	if !slices.Equal(legs, []string{"orders", "inventory"}) {
		t.Errorf("orders-summary legs = %q, want [orders inventory]", legs)
	}
	// Entry fields the data plane and stage J read (03 req 11).
	geo := orders.Client[phase.OnRequestHeaders][2]
	if geo.Policy != "geo-block-default" || geo.Class != phase.ClassAuthz || geo.Scope != phase.ScopeGateway || geo.Position != 4 ||
		geo.FirstPhase() != phase.OnRequestHeaders || geo.Ref.File != "ruralz.yaml" || geo.Ref.Line != 47 || geo.Ref.Column != 7 {
		t.Errorf("geo-block-default entry = %+v", geo)
	}
	if hs := orders.Client[phase.OnResponse][0]; hs.Policy != "headers-security" || hs.Overridable || hs.FirstPhase() != phase.OnResponse {
		t.Errorf("headers-security entry = %+v", hs)
	}
}

// TestShopBundleGolden pins the text and JSON effective tables of every
// example Route (02 req 51 and 52 reason templates; 10 req 43).
func TestShopBundleGolden(t *testing.T) {
	b, files := loadBundle(t, shopBundle)
	chains, _ := stageI(t, b, files, 2)
	for _, route := range []string{"cart-grpc", "orders-summary", "support-chat"} {
		rows := Rows(chains[route], b)
		golden(t, filepath.Join("testdata", "golden", route+".txt"), table(t, rows))
		golden(t, filepath.Join("testdata", "golden", route+".json"), rowsJSON(t, rows))
	}
}

func names(es []hub.Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Policy
	}
	return out
}
