// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// opsWithImpact returns the operation lines of resource id, each followed
// by its impact classes.
func opsWithImpact(t testing.TB, d *Diff, id string) []string {
	t.Helper()
	r := resourceOf(d, id)
	if r == nil {
		t.Fatalf("no change to %s in:\n%s", id, d.AppendText(nil))
	}
	out := make([]string, len(r.Ops))
	for i := range r.Ops {
		out[i] = strings.TrimSpace(string(appendOp(nil, &r.Ops[i]))) + " [" + r.Ops[i].Impact.String() + "]"
	}
	return out
}

// TestRoutingMarkers: Route match, upstreams and composition, and
// Upstream endpoints and discovery, carry the routing class.
func TestRoutingMarkers(t *testing.T) {
	a := shop(t, "to")
	cases := []struct {
		name, file, old, new, id, want string
	}{
		{"Route match", "routes/orders-summary.yaml", "    methods: [GET]", "    methods: [GET, HEAD]", "Route/orders-summary", "+ spec.match.methods[item=HEAD] [routing]"},
		{"Route upstreams", "routes/cart-grpc.yaml", "    - name: cart\n", "    - name: cart\n      weight: 2\n", "Route/cart-grpc", "~ spec.upstreams[name=cart].weight: 1 -> 2 [routing]"},
		{"Route composition", "routes/orders-summary.yaml", "        optional: true", "        optional: false", "Route/orders-summary", "~ spec.composition.steps[name=stock].optional: true -> false [routing]"},
		{"Upstream endpoints", "upstreams/services.yaml", `    - address: "inventory.shop.svc:8080"` + "\n", `    - address: "inventory.shop.svc:8080"` + "\n      weight: 3\n", "Upstream/inventory", "~ spec.endpoints[address=inventory.shop.svc:8080].weight: 1 -> 3 [routing]"},
		{"Upstream discovery", "upstreams/services.yaml", "    namespace: shop\n", "    namespace: shop-eu\n", "Upstream/orders", "~ spec.discovery.namespace: shop -> shop-eu [routing]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, a), render(t, edit(t, a, tc.file, tc.old, tc.new)))
			sameLines(t, "ops", opsWithImpact(t, d, tc.id), []string{tc.want})
		})
	}
}

// Policies the attachment cases add to the example Bundle.
const (
	schemaPolicy = "---\napiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: schema-orders}\nspec:\n  type: validation.json-schema\n  config: {schema: {type: object}}\n"
	cachePolicy  = "---\napiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: cache-cart}\nspec:\n  type: cache\n  config: {}\n"
	pluginPolicy = "---\napiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: geo-custom}\nspec:\n  type: plugin\n  plugin: geo-block\n  filterClass: custom\n  config: {denyCountries: [AQ]}\n"
)

// TestAttachmentImpact: a change to an attachment (spec.policies,
// spec.excludePolicies) carries the impact class of the attached Policy's
// type, from its Filter class, resolved on the side the attachment is read
// from: TO for an addition, FROM for a removal, both for a move.
func TestAttachmentImpact(t *testing.T) {
	type change struct{ file, old, new string }
	cases := []struct {
		name    string
		changes []change
		id      string
		want    []string
	}{
		{
			"auth excluded",
			[]change{{"routes/cart-grpc.yaml", "    - name: cors-default                # not a browser API\n", "    - name: cors-default                # not a browser API\n    - name: jwt-default\n"}},
			"Route/cart-grpc",
			[]string{"+ spec.excludePolicies[name=jwt-default] [security]"},
		},
		{
			"cors excluded, whole list",
			[]change{{"routes/support-chat.yaml", "  upstreams:\n    - name: llm\n", "  excludePolicies:\n    - name: cors-default\n  upstreams:\n    - name: llm\n"}},
			"Route/support-chat",
			[]string{`+ spec.excludePolicies: [{"name":"cors-default"}] [security]`},
		},
		{
			"authz attached",
			[]change{{"routes/support-chat.yaml", "  upstreams:\n    - name: llm\n", "    - name: authz-orders\n  upstreams:\n    - name: llm\n"}},
			"Route/support-chat",
			[]string{"+ spec.policies[name=authz-orders] [security]"},
		},
		{
			"admission attached",
			[]change{{"routes/support-chat.yaml", "  upstreams:\n    - name: llm\n", "    - name: ratelimit-orders\n  upstreams:\n    - name: llm\n"}},
			"Route/support-chat",
			[]string{"+ spec.policies[name=ratelimit-orders] [traffic]"},
		},
		{"validation attached", []change{
			{"policies/common.yaml", "    scopes: [orders.read]\n", "    scopes: [orders.read]\n" + schemaPolicy},
			{"routes/orders-summary.yaml", "  composition:\n", "    - name: schema-orders\n  composition:\n"},
		}, "Route/orders-summary", []string{"+ spec.policies[name=schema-orders] [security]"}},
		{"cache attached, whole list", []change{
			{"policies/common.yaml", "    scopes: [orders.read]\n", "    scopes: [orders.read]\n" + cachePolicy},
			{"routes/cart-grpc.yaml", "  upstreams:\n", "  policies:\n    - name: cache-cart\n  upstreams:\n"},
		}, "Route/cart-grpc", []string{`+ spec.policies: [{"name":"cache-cart"}] [traffic]`}},
		{
			"upstream-auth attached, transform moved",
			[]change{{"upstreams/services.yaml", "  timeout: 2s\n  policies:\n    - name: headers-internal\n", "  timeout: 2s\n  policies:\n    - name: upstream-oauth\n    - name: headers-internal\n"}},
			"Upstream/inventory",
			[]string{"+ spec.policies[name=upstream-oauth] [security]", "> spec.policies[name=headers-internal]: position 1 -> 2 [traffic]"},
		},
		{"plugin in its own class attached", []change{
			{"plugins/geo-block.yaml", "    denyCountries: [\"AQ\"]\n", "    denyCountries: [\"AQ\"]\n" + pluginPolicy},
			{"routes/orders-summary.yaml", "  composition:\n", "    - name: geo-custom\n  composition:\n"},
		}, "Route/orders-summary", []string{"+ spec.policies[name=geo-custom] [plugin]"}},
		{
			"moves carry the moved Policies",
			[]change{{"routes/orders-summary.yaml", "    - name: cors-partner                # slot cors: replaces cors-default\n    - name: authz-orders\n", "    - name: authz-orders\n    - name: cors-partner\n"}},
			"Route/orders-summary",
			[]string{"> spec.policies[name=authz-orders]: position 2 -> 1 [security]", "> spec.policies[name=cors-partner]: position 1 -> 2 [security]"},
		},
		{"a removal resolves on FROM", []change{
			{"policies/common.yaml", "apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: authz-orders}\nspec:\n  type: authz.cel\n  config:\n    rule: 'auth.claims.scope.split(\" \").exists(s, s == \"orders:read\")'\n---\n", ""},
			{"routes/orders-summary.yaml", "    - name: authz-orders\n", ""},
		}, "Route/orders-summary", []string{"- spec.policies[name=authz-orders] [security]", "> spec.policies[name=ratelimit-orders]: position 3 -> 2 [traffic]"}},
		{
			"an unresolved name carries nothing",
			[]change{{"routes/support-chat.yaml", "  upstreams:\n    - name: llm\n", "    - name: ghost\n  upstreams:\n    - name: llm\n"}},
			"Route/support-chat",
			[]string{"+ spec.policies[name=ghost] []"},
		},
	}
	a := shop(t, "to")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := a
			for _, c := range tc.changes {
				b = edit(t, b, c.file, c.old, c.new)
			}
			d := compare(t, render(t, a), render(t, b))
			sameLines(t, "ops", opsWithImpact(t, d, tc.id), tc.want)
		})
	}
}

// TestAttachmentsOfWholeResources: an added or removed resource carries
// the classes of the Policies it attaches, with those of its fields.
func TestAttachmentsOfWholeResources(t *testing.T) {
	a := shop(t, "to")
	b := with(a, "routes/health.yaml", `apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: health
spec:
  match:
    path: {exact: /health}
  policies:
    - name: authz-orders
  upstreams:
    - name: inventory
`)
	for _, tc := range []struct {
		name     string
		from, to []source
	}{{"added", a, b}, {"removed", b, a}} {
		d := compare(t, render(t, tc.from), render(t, tc.to))
		if r := resourceOf(d, "Route/health"); r == nil || r.Impact.String() != "routing, security" {
			t.Errorf("%s Route = %+v, want routing (match, upstreams) and security (the authz attachment)", tc.name, r)
		}
	}
}

// TestClassImpactTable pins the class of every Filter class, and checks
// that every registered Policy type's attachment carries one.
func TestClassImpactTable(t *testing.T) {
	want := map[phase.Class]string{
		phase.ClassCORS: "security", phase.ClassAuth: "security", phase.ClassAuthz: "security",
		phase.ClassAdmission: "traffic", phase.ClassValidation: "security", phase.ClassCache: "traffic",
		phase.ClassUpstreamAuth: "security", phase.ClassTransform: "traffic", phase.ClassCustom: "plugin",
	}
	if len(want) != int(phase.NumClasses) {
		t.Fatalf("the table names %d classes, phase has %d", len(want), phase.NumClasses)
	}
	for c := range phase.NumClasses {
		if got := classImpact(c).String(); got != want[c] {
			t.Errorf("classImpact(%s) = %q, want %q", c, got, want[c])
		}
	}
	if got := classImpact(phase.NumClasses); got != 0 {
		t.Errorf("classImpact(out of range) = %s, want none", got)
	}
	for _, e := range registry.New().Entries() {
		if classImpact(e.Class) == 0 {
			t.Errorf("Policy type %s (class %s) carries no impact class", e.Type, e.Class)
		}
	}
}

// TestTypeExtras: a plugin Policy's attachment also carries plugin and an
// ai.* Policy's also carries ai, beside the class of its Filter class.
func TestTypeExtras(t *testing.T) {
	a := shop(t, "to")
	cases := []struct {
		name, file, old, new, id, want string
	}{
		{"plugin Policy in authz detached", "ruralz.yaml", "    - name: geo-block-default\n", "", "Gateway/edge", "- spec.policies[name=geo-block-default] [plugin, security]"},
		{"ai.token-budget attached", "routes/orders-summary.yaml", "  composition:\n", "    - name: token-budget-partner\n  composition:\n", "Route/orders-summary", "+ spec.policies[name=token-budget-partner] [ai, traffic]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, a), render(t, edit(t, a, tc.file, tc.old, tc.new)))
			sameLines(t, "ops", opsWithImpact(t, d, tc.id), []string{tc.want})
		})
	}
}

// TestPolicySpecCarriesTypeClass: a change to a Policy's own spec carries
// its type class (an addition TO's, a removal FROM's, anything else
// both), while a change to its metadata does not.
func TestPolicySpecCarriesTypeClass(t *testing.T) {
	a := shop(t, "to")
	cases := []struct {
		name, file, old, new, id string
		want                     []string
	}{
		{
			"auth config", "policies/common.yaml", "        audiences: [shop-api]", "        audiences: [shop-api, shop-admin]", "Policy/jwt-default",
			[]string{"+ spec.config.issuers[issuer=https://auth.shop.example].audiences[item=shop-admin] [security]"},
		},
		{
			"admission config", "policies/common.yaml", "    limits: [{requests: 1000, window: 1s}]", "    limits: [{requests: 2000, window: 1s}]", "Policy/ratelimit-global",
			[]string{`~ spec.config.limits: [{"requests":1000,"window":"1s"}] -> [{"requests":2000,"window":"1s"}] [traffic]`},
		},
		{
			"ai Policy", "policies/common.yaml", "  stateStoreTimeout: 30ms", "  stateStoreTimeout: 40ms", "Policy/token-budget-partner",
			[]string{"~ spec.stateStoreTimeout: 30ms -> 40ms [ai, traffic]"},
		},
		{
			"plugin Policy", "plugins/geo-block.yaml", `    denyCountries: ["AQ"]`, `    denyCountries: ["AQ", "KP"]`, "Policy/geo-block-default",
			[]string{`~ spec.config.denyCountries: ["AQ"] -> ["AQ","KP"] [plugin, security]`},
		},
		{
			"metadata only", "policies/common.yaml", "metadata: {name: jwt-default}", "metadata: {name: jwt-default, labels: {team: identity}}", "Policy/jwt-default",
			[]string{"+ metadata.labels: {\"team\":\"identity\"} []"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, a), render(t, edit(t, a, tc.file, tc.old, tc.new)))
			sameLines(t, "ops", opsWithImpact(t, d, tc.id), tc.want)
		})
	}
	t.Run("type change reads each side", func(t *testing.T) {
		b := edit(t, a, "policies/common.yaml", `metadata: {name: ratelimit-orders}
spec:
  type: ratelimit
  config:
    key: 'consumer == null ? source.ip : consumer.name'
    limits: [{requests: 50, window: 1s}]`, `metadata: {name: ratelimit-orders}
spec:
  type: authz.cel
  config:
    rule: 'consumer != null'`)
		d := compare(t, render(t, a), render(t, b))
		got := opsWithImpact(t, d, "Policy/ratelimit-orders")
		for _, want := range []string{
			"~ spec.type: ratelimit -> authz.cel [security, traffic]",
			`+ spec.config.rule: "consumer != null" [security]`,
			`- spec.config.limits: [{"requests":50,"window":"1s"}] [traffic]`,
		} {
			if !slices.Contains(got, want) {
				t.Errorf("ops %q lack %q", got, want)
			}
		}
	})
}

// TestPolicyResourceCarriesTypeClass: adding or removing a Policy carries
// its type class beside the classes of its fields.
func TestPolicyResourceCarriesTypeClass(t *testing.T) {
	a := shop(t, "to")
	withPlugin := edit(t, a, "plugins/geo-block.yaml", "    denyCountries: [\"AQ\"]\n", "    denyCountries: [\"AQ\"]\n"+pluginPolicy)
	without := edit(t, a, "policies/common.yaml", "apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: semantic-cache-support}   # attached nowhere: see the note after this file\nspec:\n  type: ai.semantic-cache\n  config:\n    key: \"consumer.name\"                # one partition per Consumer: no cross-tenant hits\n---\n", "")
	cases := []struct {
		name     string
		from, to []source
		id, want string
	}{
		{"added ratelimit", shop(t, "from"), a, "Policy/ratelimit-orders", "security, traffic"},
		{"removed ratelimit", a, shop(t, "from"), "Policy/ratelimit-orders", "security, traffic"},
		{"removed ai.semantic-cache", a, without, "Policy/semantic-cache-support", "ai, security, traffic"},
		{"added plugin in its own class", a, withPlugin, "Policy/geo-custom", "plugin, security"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, tc.from), render(t, tc.to))
			if r := resourceOf(d, tc.id); r == nil || r.Impact.String() != tc.want {
				t.Errorf("%s = %+v, want %s", tc.id, r, tc.want)
			}
		})
	}
}

// TestPluginSpecCarriesPlugin: every change to a Plugin's spec carries the
// plugin class, not only capabilities and image, while a change to its
// metadata does not.
func TestPluginSpecCarriesPlugin(t *testing.T) {
	a := shop(t, "to")
	cases := []struct {
		name, old, new, want string
	}{
		{"phases", "  phases: [onRequestHeaders]", "  phases: [onRequestHeaders, onLog]", "+ spec.phases[item=onLog] [plugin]"},
		{"limits", "timeout: 5ms", "timeout: 6ms", "~ spec.limits.timeout: 5ms -> 6ms [plugin]"},
		{"configSchema", "    required: [denyCountries]", "    required: [denyCountries, allowList]", `~ spec.configSchema.required: ["denyCountries"] -> ["denyCountries","allowList"] [plugin]`},
		{"metadata only", "  name: geo-block\n", "  name: geo-block\n  labels: {team: edge}\n", `+ metadata.labels: {"team":"edge"} []`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, a), render(t, edit(t, a, "plugins/geo-block.yaml", tc.old, tc.new)))
			sameLines(t, "ops", opsWithImpact(t, d, "Plugin/geo-block"), []string{tc.want})
		})
	}
}
