// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// sameLines fails unless got equals want.
func sameLines(t *testing.T, what string, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("%s:\n got %q\nwant %q", what, got, want)
	}
}

// TestIdenticalStatesHaveNoChanges: diff(A, A) is empty in both forms,
// whatever each side was read from.
func TestIdenticalStatesHaveNoChanges(t *testing.T) {
	a := render(t, shop(t, "to"))
	for _, b := range []*hub.Validated{a, render(t, shop(t, "to")), dumped(t, a)} {
		d := compare(t, a, b)
		if d.Changed() || len(d.Resources) != 0 || len(d.Effective) != 0 || d.Summary != (Summary{}) {
			t.Errorf("Compare(A, A) changed:\n%s", d.AppendText(nil))
		}
	}
}

// TestRenameIsRemovalPlusAddition: resources match by identity and a
// rename is never inferred.
func TestRenameIsRemovalPlusAddition(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "upstreams/services.yaml", "  name: cart\n", "  name: cart-v2\n")
	b = edit(t, b, "routes/cart-grpc.yaml", "    - name: cart\n", "    - name: cart-v2\n")
	d := compare(t, render(t, a), render(t, b))
	var got []string
	for _, r := range d.Resources {
		got = append(got, r.Change.String()+" "+r.ID.String())
	}
	sameLines(t, "resources", got, []string{"modified Route/cart-grpc", "added Upstream/cart-v2", "removed Upstream/cart"})
	sameLines(t, "Route ops", opLines(t, d, "Route/cart-grpc"), []string{
		`+ spec.upstreams[name=cart-v2]: {"name":"cart-v2","weight":1}`,
		`- spec.upstreams[name=cart]: {"name":"cart","weight":1}`,
	})
	for _, r := range d.Resources {
		if r.Change != Modified && r.Ops != nil {
			t.Errorf("%s %s carries ops %v; added and removed resources carry identity only", r.Change, r.ID, r.Ops)
		}
	}
}

// TestFormattingAndFileMovesNeverAppear: the comparison uses the canonical
// form, so moving documents between files, flow and block style, JSON
// sources, comments, member order and equivalent spellings change nothing;
// a real change then reports TO's file.
func TestFormattingAndFileMovesNeverAppear(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "policies/common.yaml", `apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: cors-default}
spec:
  type: cors
  config: {allowOrigins: ["https://shop.example"], allowMethods: [GET, POST]}
---
`, "")
	b = with(b, "policies/cors/default.yaml", `# moved out of common.yaml, rewritten in block style
spec:
  config:
    allowMethods:
      - POST
      - GET
    allowOrigins: ['https://shop.example']
  type: cors
metadata:
  name: cors-default
kind: Policy
apiVersion: ruralz/v1alpha1
`)
	b = edit(t, b, "routes/orders-summary.yaml", "  timeout: 4s", "  timeout: 4000ms")
	da, db := render(t, a), render(t, b)
	if da.Revision.Digest != db.Revision.Digest {
		t.Fatalf("fixture: the reformatted Bundle has another Revision")
	}
	if d := compare(t, da, db); d.Changed() {
		t.Errorf("formatting changed the diff:\n%s", d.AppendText(nil))
	}
	c := edit(t, b, "policies/cors/default.yaml", "      - GET\n", "      - GET\n      - PUT\n")
	d := compare(t, da, render(t, c))
	r := resourceOf(d, "Policy/cors-default")
	if r == nil || r.Source != "policies/cors/default.yaml" {
		t.Fatalf("source of the changed Policy = %+v, want TO's file policies/cors/default.yaml", r)
	}
	sameLines(t, "ops", opLines(t, d, "Policy/cors-default"), []string{"+ spec.config.allowMethods[item=PUT]"})
}

// TestSourceFallsBackToFrom: a removed resource names FROM's file, and a
// side read from canonical content names none.
func TestSourceFallsBackToFrom(t *testing.T) {
	a := render(t, shop(t, "from"))
	b := render(t, shop(t, "to"))
	d := compare(t, a, b)
	if r := resourceOf(d, "Upstream/legacy-orders"); r == nil || r.Source != "upstreams/services.yaml" {
		t.Errorf("removed Upstream source = %+v, want FROM's upstreams/services.yaml", r)
	}
	d = compare(t, dumped(t, a), b)
	if r := resourceOf(d, "Upstream/legacy-orders"); r == nil || r.Source != "" {
		t.Errorf("removed Upstream from a dump has source %+v, want none", r)
	}
	if r := resourceOf(d, "Route/orders-summary"); r == nil || r.Source != "routes/orders-summary.yaml" {
		t.Errorf("modified Route source = %+v, want TO's file", r)
	}
	text := string(compare(t, dumped(t, a), dumped(t, b)).AppendText(nil))
	ident := "- Upstream/legacy-orders"
	if !strings.Contains(text, "\n"+ident+strings.Repeat(" ", identityWidth-len(ident))+"[routing, traffic]\n") {
		t.Errorf("without file names the source column is still written:\n%s", text)
	}
}

// reversed returns v with the list at path (a field path under the
// resource id) reversed, so the engine compares trees whose map or set
// lists are out of order.
func reversed(t *testing.T, v *hub.Validated, id string, path ...string) *hub.Validated {
	t.Helper()
	return altered(t, v, id, func(root *tree.Node) {
		n := at(root, path...)
		if n == nil || n.Kind != tree.KindList || len(n.Items) < 2 {
			t.Fatalf("no list of two or more at %s %v", id, path)
		}
		slices.Reverse(n.Items)
	})
}

// TestCanonicalEnvelope: the trees compare as the canonical form holds
// them: apiVersion and the conversion-data annotation never appear (an
// annotations member holding only that one is left out whole), and numbers
// compare by their RFC 8785 text.
func TestCanonicalEnvelope(t *testing.T) {
	a := render(t, shop(t, "to"))
	b := altered(t, a, "Gateway/edge", func(root *tree.Node) {
		root.Set("apiVersion", tree.Pos{}, str("ruralz/v1beta1"))
		at(root, "metadata").Set("annotations", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: conversionData, Value: str(`{"x":1}`)}}})
		at(root, "spec", "telemetry").Set("traceSampling", tree.Pos{}, &tree.Node{Kind: tree.KindFloat, Text: "0.050"})
		at(root, "spec", "admin").Set("port", tree.Pos{}, &tree.Node{Kind: tree.KindInt, Text: "9902"})
	})
	sameLines(t, "Gateway ops", opLines(t, compare(t, a, b), "Gateway/edge"), []string{"~ spec.admin.port: 9901 -> 9902"})
	c := altered(t, a, "Route/orders-summary", func(root *tree.Node) {
		at(root, "metadata").Set("annotations", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{
			{Key: conversionData, Value: str("{}")}, {Key: "shop.example/owner", Value: str("commerce")},
		}})
	})
	sameLines(t, "Route ops", opLines(t, compare(t, a, c), "Route/orders-summary"), []string{`+ metadata.annotations: {"shop.example/owner":"commerce"}`})
}

// TestEqualDigestsAreEqual: equal digests are equal states, compared
// without reading the trees.
func TestEqualDigestsAreEqual(t *testing.T) {
	a := render(t, shop(t, "to"))
	b := altered(t, a, "Route/orders-summary", func(root *tree.Node) {
		at(root, "spec").Set("timeout", tree.Pos{}, str("9s"))
	})
	b.Revision = a.Revision
	if d := compare(t, a, b); d.Changed() {
		t.Errorf("states with one digest differ:\n%s", d.AppendText(nil))
	}
}

// TestSchemalessListsReplaceWhole: a list in schemaless content, such as
// a Plugin configSchema, has no x-ruralz-list type and is replaced whole.
func TestSchemalessListsReplaceWhole(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "plugins/geo-block.yaml", "    required: [denyCountries]", "    required: [denyCountries, allowList]")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "Plugin/geo-block"), []string{`~ spec.configSchema.required: ["denyCountries"] -> ["denyCountries","allowList"]`})
}

// TestKeyedListWithoutUsableKeys: a keyed list in which two entries share
// a key, which stage G never hands over, is replaced whole.
func TestKeyedListWithoutUsableKeys(t *testing.T) {
	a := render(t, shop(t, "to"))
	b := altered(t, a, "Gateway/edge", func(root *tree.Node) {
		ls := at(root, "spec", "listeners")
		ls.Items = append(ls.Items, ls.Items[0].Clone())
	})
	ops := opLines(t, compare(t, a, b), "Gateway/edge")
	if len(ops) != 1 || !strings.HasPrefix(ops[0], "~ spec.listeners: [") {
		t.Errorf("ops = %q, want one whole-list replace", ops)
	}
}

// TestListTypesDifferCompareWhole: when the sides' schemas give one list
// two types (a dispatch that differs), it compares whole.
func TestListTypesDifferCompareWhole(t *testing.T) {
	idx, err := schemaidx.Load("test/v1", []byte(sinceSchema))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := idx.Def("GatewaySpec")
	atomicList, _ := spec.Property("items")
	mapList, _ := spec.Property("named")
	list := func(depth string) *tree.Node {
		return &tree.Node{Kind: tree.KindList, Items: []*tree.Node{{Kind: tree.KindMap, Members: []tree.Member{
			{Key: "name", Value: str("a")}, {Key: "depth", Value: &tree.Node{Kind: tree.KindInt, Text: depth}},
		}}}}
	}
	for _, tc := range []struct {
		name   string
		sa, sb *schemaidx.Node
		want   string
	}{
		{"same type", mapList, mapList, "~ x[name=a].depth: 1 -> 2"},
		{"types differ", atomicList, mapList, `~ x: [{"depth":1,"name":"a"}] -> [{"depth":2,"name":"a"}]`},
	} {
		c := newComparer(idx, registry.New(), nil, nil)
		c.path = diag.Path{diag.Field("x")}
		c.list(list("1"), list("2"), pos{static: tc.sa, s: tc.sa}, pos{static: tc.sb, s: tc.sb})
		if len(c.ops) != 1 || strings.TrimSpace(string(appendOp(nil, &c.ops[0]))) != tc.want {
			t.Errorf("%s: ops %+v, want %s", tc.name, c.ops, tc.want)
		}
	}
}

// TestMapAndSetReorderingIsNoChange: map lists compare by key and set
// lists by element, so reordering them is never a change, even in trees
// stage G did not sort.
func TestMapAndSetReorderingIsNoChange(t *testing.T) {
	a := render(t, shop(t, "to"))
	b := render(t, edit(t, shop(t, "to"), "routes/orders-summary.yaml", "  timeout: 4s", "  timeout: 3s"))
	b = reversed(t, b, "Gateway/edge", "spec", "listeners")
	b = reversed(t, b, "Plugin/geo-block", "spec", "capabilities")
	if b.Revision.Digest == a.Revision.Digest {
		t.Fatal("fixture: no difference left to compare")
	}
	d := compare(t, a, b)
	if len(d.Resources) != 1 {
		t.Fatalf("reordered lists were reported:\n%s", d.AppendText(nil))
	}
	sameLines(t, "ops", opLines(t, d, "Route/orders-summary"), []string{"~ spec.timeout: 4s -> 3s"})
}

// TestOrderedMapMoves: an orderedMap entry whose position changes moves,
// with zero-based positions in JSON and one-based ones in the human form;
// an insertion moves the entries after it.
func TestOrderedMapMoves(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "routes/orders-summary.yaml", "  policies:\n", "  policies:\n    - name: headers-internal\n")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "Route/orders-summary"), []string{
		"+ spec.policies[name=headers-internal]",
		"> spec.policies[name=cors-partner]: position 1 -> 2",
		"> spec.policies[name=authz-orders]: position 2 -> 3",
		"> spec.policies[name=ratelimit-orders]: position 3 -> 4",
	})
	r := resourceOf(d, "Route/orders-summary")
	if mv := r.Ops[1]; mv.Kind != OpMove || mv.FromIndex != 0 || mv.ToIndex != 1 {
		t.Errorf("move op = %+v, want zero-based 0 -> 1", mv)
	}
	var doc struct {
		Resources []struct {
			Ops []struct {
				Op   string          `json:"op"`
				From json.RawMessage `json:"from"`
				To   json.RawMessage `json:"to"`
			} `json:"ops"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(d.AppendJSON(nil), &doc); err != nil {
		t.Fatal(err)
	}
	if mv := doc.Resources[0].Ops[1]; mv.Op != "move" || string(mv.From) != "0" || string(mv.To) != "1" {
		t.Errorf("JSON move = %s %s -> %s, want zero-based 0 -> 1", mv.Op, mv.From, mv.To)
	}
}

// TestKeyedEntries: map and orderedMap entries are added, removed and
// compared field by field by key; an entry holding only its key shows no
// value.
func TestKeyedEntries(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "ruralz.yaml", "    - name: http\n      protocol: http\n      port: 8080\n", "    - name: http\n      protocol: http\n      port: 8081\n    - name: metrics\n      protocol: http\n      port: 9100\n")
	b = edit(t, b, "upstreams/services.yaml", "    - name: headers-internal\n---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: inventory", "---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: inventory")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "Gateway ops", opLines(t, d, "Gateway/edge"), []string{
		"~ spec.listeners[name=http].port: 8080 -> 8081",
		`+ spec.listeners[name=metrics]: {"name":"metrics","port":9100,"protocol":"http","proxyProtocol":false}`,
	})
	sameLines(t, "Upstream ops", opLines(t, d, "Upstream/orders"), []string{"- spec.policies[name=headers-internal]"})
}

// TestAtomicListReplacedWhole: an atomic list is one replace.
func TestAtomicListReplacedWhole(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "policies/common.yaml", "limits: [{requests: 1000, window: 1s}]", "limits: [{requests: 1000, window: 1s}, {requests: 20000, window: 1m}]")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "Policy/ratelimit-global"), []string{
		`~ spec.config.limits: [{"requests":1000,"window":"1s"}] -> [{"requests":1000,"window":"1s"},{"requests":20000,"window":"1m0s"}]`,
	})
}

// TestScalarsAndTypes: scalars replace; values compare in canonical form,
// so equivalent spellings never diff.
func TestScalarsAndTypes(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "ruralz.yaml", "    traceSampling: 0.05", "    traceSampling: 0.050")
	b = edit(t, b, "ruralz.yaml", "    maxRequestBodyBytes: 10Mi", "    maxRequestBodyBytes: 10485760")
	if d := compare(t, render(t, a), render(t, b)); d.Changed() {
		t.Fatalf("equivalent spellings diffed:\n%s", d.AppendText(nil))
	}
	b = edit(t, b, "ruralz.yaml", "    traceSampling: 0.050", "    traceSampling: 0.5")
	b = edit(t, b, "ruralz.yaml", "      http3: true", "      http3: false")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "Gateway/edge"), []string{
		"~ spec.listeners[name=https].http3: true -> false",
		"~ spec.telemetry.traceSampling: 0.05 -> 0.5",
	})
}

// TestOpsOrder: a resource lists replace, add, remove, then move, each in
// document order.
func TestOpsOrder(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "ai/models.yaml", "  limits: {maxInputTokens: 32000, maxOutputTokens: 2048}", "  limits: {maxInputTokens: 16000}")
	b = edit(t, b, "ai/models.yaml", "    - name: primary\n      provider: anthropic-prod\n      model: claude-sonnet-4-5\n", "")
	b = edit(t, b, "ai/models.yaml", "      model: gpt-4.1-mini\n", "      model: gpt-4.1\n")
	b = edit(t, b, "ai/models.yaml", "      when: \"ai.estimatedInputTokens < 100000\"\n", "      when: \"ai.estimatedInputTokens < 100000\"\n    - name: tertiary\n      provider: openai-prod\n      model: gpt-4.1-nano\n")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "AIModel/support-chat"), []string{
		"~ spec.candidates[name=secondary].model: gpt-4.1-mini -> gpt-4.1",
		"~ spec.limits.maxInputTokens: 32000 -> 16000",
		`+ spec.candidates[name=tertiary]: {"model":"gpt-4.1-nano","name":"tertiary","provider":"openai-prod"}`,
		`- spec.candidates[name=primary]: {"model":"claude-sonnet-4-5","name":"primary","provider":"anthropic-prod"}`,
		"- spec.limits.maxOutputTokens: 2048",
		"> spec.candidates[name=secondary]: position 2 -> 1",
	})
}

// TestImpactClasses: a change carries the classes on its path and inside
// its values; an added or removed resource those of every field it holds;
// a resource and the summary the union.
func TestImpactClasses(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "ruralz.yaml", "    - name: http\n      protocol: http\n      port: 8080\n", `    - name: http
      protocol: http
      port: 8080
    - name: internal
      protocol: https
      port: 9443
      tls:
        certificates:
          - name: internal
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/internal.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/internal.key}}
`)
	b = edit(t, b, "routes/orders-summary.yaml", "  name: orders-summary\n", "  name: orders-summary\n  labels:\n    team: commerce\n")
	d := compare(t, render(t, a), render(t, b))
	gw := resourceOf(d, "Gateway/edge")
	if gw == nil || len(gw.Ops) != 1 || gw.Ops[0].Impact.String() != "routing, security" {
		t.Errorf("adding a listener with TLS keys: ops %+v, want one op with routing (path) and security (inside)", gw)
	}
	if r := resourceOf(d, "Route/orders-summary"); r == nil || r.Impact != 0 {
		t.Errorf("a change without classes on its path: %+v, want no impact", r)
	}
	if got := d.Summary.Impact.String(); got != "routing, security" {
		t.Errorf("summary impact = %q, want the union routing, security", got)
	}
	d = compare(t, render(t, shop(t, "from")), render(t, shop(t, "to")))
	for id, want := range map[string]string{"Policy/ratelimit-orders": "security, traffic", "Upstream/legacy-orders": "routing, traffic"} {
		if r := resourceOf(d, id); r == nil || r.Impact.String() != want {
			t.Errorf("%s impact = %+v, want %s (the classes of the fields it holds, a Policy's type class too)", id, r, want)
		}
	}
}

// TestCapabilityGrant: every change to Plugin spec.capabilities or
// spec.image is flagged, and an added Plugin; a removed one is not.
func TestCapabilityGrant(t *testing.T) {
	a := shop(t, "to")
	cases := []struct {
		name string
		b    []source
		id   string
		want string
	}{
		{"capability added", edit(t, a, "plugins/geo-block.yaml", "response.send, log.write]", "response.send, log.write, request.body.read]"), "Plugin/geo-block", "+ spec.capabilities[item=request.body.read]  (Capability grant)"},
		{"capability removed", edit(t, a, "plugins/geo-block.yaml", "response.send, log.write]", "response.send]"), "Plugin/geo-block", "- spec.capabilities[item=log.write]  (Capability grant)"},
		{"image changed", edit(t, a, "plugins/geo-block.yaml", "geo-block:1.4.0@sha256:fc37", "geo-block:1.4.1@sha256:ab37"), "Plugin/geo-block", "~ spec.image: ghcr.io/acme/ruralz-plugins/geo-block:1.4.0@sha256:fc37c2971bb6e82aa12a871aed718818afcf45ab42bd093f0cf52f4ad8045435 -> ghcr.io/acme/ruralz-plugins/geo-block:1.4.1@sha256:ab37c2971bb6e82aa12a871aed718818afcf45ab42bd093f0cf52f4ad8045435  (Capability grant)"},
		{"limits changed", edit(t, a, "plugins/geo-block.yaml", "timeout: 5ms", "timeout: 6ms"), "Plugin/geo-block", "~ spec.limits.timeout: 5ms -> 6ms"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := compare(t, render(t, a), render(t, tc.b))
			sameLines(t, "ops", opLines(t, d, tc.id), []string{tc.want})
			r := resourceOf(d, tc.id)
			flagged := strings.HasSuffix(tc.want, capabilityNote)
			if (r.Flags == FlagCapabilityGrant) != flagged || slices.Equal(r.Flags.Strings(), []string{"capabilityGrant"}) != flagged {
				t.Errorf("flags = %v, want flagged %v", r.Flags.Strings(), flagged)
			}
		})
	}
	added := with(a, "plugins/rewrite.yaml", `apiVersion: ruralz/v1alpha1
kind: Plugin
metadata:
  name: rewrite
spec:
  image: ghcr.io/acme/ruralz-plugins/rewrite:1.0.0@sha256:0c37c2971bb6e82aa12a871aed718818afcf45ab42bd093f0cf52f4ad8045435
  abi: ruralz.plugin.v1
  phases: [onRequestHeaders]
  capabilities: [request.headers.write]
`)
	d := compare(t, render(t, a), render(t, added))
	if r := resourceOf(d, "Plugin/rewrite"); r == nil || r.Flags != FlagCapabilityGrant {
		t.Errorf("added Plugin = %+v, want flagged", r)
	}
	if text := string(d.AppendText(nil)); !strings.Contains(text, "[plugin, security]  (Capability grant)\n") {
		t.Errorf("added Plugin line lacks the note:\n%s", text)
	}
	d = compare(t, render(t, added), render(t, a))
	if r := resourceOf(d, "Plugin/rewrite"); r == nil || r.Flags != 0 {
		t.Errorf("removed Plugin = %+v, want not flagged", r)
	}
}

// TestSecretValuesShowOnlyTheReference: a change at a secret position
// shows the secretRef, carries the security class and never a resolved
// value; the diff resolves nothing.
func TestSecretValuesShowOnlyTheReference(t *testing.T) {
	const canary = "canary-7f3e9b2c-secret-value"
	t.Setenv("RURALZ_SECRET_CANARY", canary)
	a := shop(t, "to")
	b := edit(t, a, "ruralz.yaml", "url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}", "url: {secretRef: {provider: env, name: RURALZ_SECRET_CANARY}}")
	b = edit(t, b, "upstreams/services.yaml", "caCertificate: {secretRef: {provider: file, name: /etc/ruralz/ca/ca.crt}}", "caCertificate: {secretRef: {provider: file, name: /etc/ruralz/ca/ca.json, key: pem}}")
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "Gateway ops", opLines(t, d, "Gateway/edge"), []string{"~ spec.stateStore.url.secretRef.name: RURALZ_STATE_STORE_URL -> RURALZ_SECRET_CANARY"})
	sameLines(t, "Upstream ops", opLines(t, d, "Upstream/cart"), []string{
		"~ spec.tls.caCertificate.secretRef.name: /etc/ruralz/ca/ca.crt -> /etc/ruralz/ca/ca.json",
		"+ spec.tls.caCertificate.secretRef.key: pem",
	})
	for _, id := range []string{"Gateway/edge", "Upstream/cart"} {
		if r := resourceOf(d, id); !r.Impact.Has(schemaidx.ImpactSecurity) {
			t.Errorf("%s impact %s lacks security", id, r.Impact)
		}
	}
	for _, out := range [][]byte{d.AppendText(nil), d.AppendJSON(nil)} {
		if strings.Contains(string(out), canary) {
			t.Fatalf("output holds the resolved secret:\n%s", out)
		}
	}
}

// withPrivateKey returns v with the Gateway's TLS private key replaced by
// key, which stage F would refuse unless it is a reference.
func withPrivateKey(t *testing.T, v *hub.Validated, key *tree.Node) *hub.Validated {
	t.Helper()
	return altered(t, v, "Gateway/edge", func(root *tree.Node) {
		for _, l := range at(root, "spec", "listeners").Items {
			if certs := at(l, "tls", "certificates"); certs != nil {
				certs.Items[0].Set("privateKey", tree.Pos{}, key)
			}
		}
	})
}

// secretRef returns a SecretValue whose secretRef holds members.
func secretRef(members ...tree.Member) *tree.Node {
	return &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "secretRef", Value: &tree.Node{Kind: tree.KindMap, Members: members}}}}
}

// TestSecretNonReferenceIsRefused: a value the diff would show at a secret
// position that is not a well-formed reference (a literal, an undeclared
// member, a provider outside its enum, a member of the wrong type) fails
// with RZ-CFG-012 in either direction, naming the path but never the
// value, as render refuses it.
func TestSecretNonReferenceIsRefused(t *testing.T) {
	const canary = "-----BEGIN PRIVATE KEY----- canary-91c4"
	keyPath := "Gateway/edge spec.listeners[name=https].tls.certificates[name=api-shop].privateKey"
	cases := []struct {
		name string
		key  *tree.Node
		path string
	}{
		{"literal", str(canary), keyPath},
		{"undeclared member", secretRef(
			tree.Member{Key: "provider", Value: str("file")}, tree.Member{Key: "name", Value: str("/etc/ruralz/tls/tls.key")},
			tree.Member{Key: "value", Value: str(canary)},
		), keyPath + ".secretRef.value"},
		{"provider outside its enum", secretRef(
			tree.Member{Key: "provider", Value: str(canary)}, tree.Member{Key: "name", Value: str("/etc/ruralz/tls/tls.key")},
		), keyPath + ".secretRef.provider"},
		{"member of the wrong type", secretRef(
			tree.Member{Key: "provider", Value: str("file")}, tree.Member{Key: "name", Value: &tree.Node{Kind: tree.KindList, Items: []*tree.Node{str(canary)}}},
		), keyPath + ".secretRef.name"},
	}
	a := render(t, shop(t, "to"))
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := withPrivateKey(t, a, tc.key)
			for _, pair := range [][2]*hub.Validated{{a, b}, {b, a}} {
				_, err := engine(t, 1).Compare(context.Background(), Side{State: pair[0]}, Side{State: pair[1]})
				if code := codeOf(err); code != CodeLiteral {
					t.Fatalf("Compare() error = %v, want %s", err, CodeLiteral)
				}
				if msg := err.Error(); strings.Contains(msg, "canary") || !strings.Contains(msg, tc.path+":") {
					t.Errorf("error %q leaks the value or misses the path %s", msg, tc.path)
				}
			}
		})
	}
	// A well-formed reference with another name is shown.
	ok := withPrivateKey(t, a, secretRef(tree.Member{Key: "name", Value: str("/etc/ruralz/tls/other.key")}, tree.Member{Key: "provider", Value: str("file")}))
	sameLines(t, "ops", opLines(t, compare(t, a, ok), "Gateway/edge"), []string{
		"~ spec.listeners[name=https].tls.certificates[name=api-shop].privateKey.secretRef.name: /etc/ruralz/tls/tls.key -> /etc/ruralz/tls/other.key",
	})
}

// TestPolicyTypeChange: a Policy whose type changes compares field by
// field under each side's own config schema.
func TestPolicyTypeChange(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "policies/common.yaml", `metadata: {name: authz-orders}
spec:
  type: authz.cel
  config:
    rule: 'auth.claims.scope.split(" ").exists(s, s == "orders:read")'`, `metadata: {name: authz-orders}
spec:
  type: authz.ip
  config:
    allow: [10.0.0.0/8]`)
	d := compare(t, render(t, a), render(t, b))
	sameLines(t, "ops", opLines(t, d, "Policy/authz-orders"), []string{
		"~ spec.type: authz.cel -> authz.ip",
		`+ spec.config.allow: ["10.0.0.0/8"]`,
		`- spec.config.rule: "auth.claims.scope.split(\" \").exists(s, s == \"orders:read\")"`,
	})
}

// TestJSONOpValues: JSON operations carry values in canonical form, keyed
// paths as single-member objects and set elements as {"item": value}.
func TestJSONOpValues(t *testing.T) {
	a := shop(t, "to")
	b := edit(t, a, "routes/orders-summary.yaml", "    methods: [GET]", "    methods: [GET, HEAD]")
	b = edit(t, b, "ruralz.yaml", "      port: 8080\n", "      port: 8081\n")
	d := compare(t, render(t, a), render(t, b))
	var doc struct {
		Resources []struct {
			Kind string `json:"kind"`
			Ops  []struct {
				Op   string            `json:"op"`
				Path []json.RawMessage `json:"path"`
				From json.RawMessage   `json:"from"`
				To   json.RawMessage   `json:"to"`
			} `json:"ops"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(d.AppendJSON(nil), &doc); err != nil {
		t.Fatalf("JSON form does not parse: %v", err)
	}
	got := map[string]string{}
	for _, r := range doc.Resources {
		for _, op := range r.Ops {
			var path []string
			for _, p := range op.Path {
				path = append(path, compact(t, p))
			}
			got[r.Kind+" "+op.Op+" "+strings.Join(path, ",")] = compact(t, op.From) + " " + compact(t, op.To)
		}
	}
	want := map[string]string{
		`Gateway replace "spec","listeners",{"name":"http"},"port"`: "8080 8081",
		`Route add "spec","match","methods",{"item":"HEAD"}`:        ` "HEAD"`,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("op %s = %q, want %q (all: %q)", k, got[k], v, got)
		}
	}
}

// compact returns raw JSON without whitespace, or "" for none.
func compact(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
