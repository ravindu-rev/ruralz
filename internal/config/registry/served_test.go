// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// hubIndex returns the schema index of the served apiVersion, the hub of
// M1.
func hubIndex(t *testing.T) *schemaidx.Index {
	t.Helper()
	set, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	idx, ok := set.Index(v1alpha1.APIVersion)
	if !ok {
		t.Fatal("no index of the served apiVersion")
	}
	return idx
}

// checkDoc parses one YAML resource from file and runs the serve check of
// r on it, returning the findings in text form.
func checkDoc(t *testing.T, r *Registry, file, src string) []string {
	t.Helper()
	files := &tree.FileTable{}
	id := files.Add(tree.File{Path: file, Role: tree.RoleBase})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), id, file, profile.FormatYAML, profile.Options{})
	if err != nil || len(ds) > 0 || len(docs) != 1 {
		t.Fatalf("parse %s: %v %v (%d documents)", file, err, ds, len(docs))
	}
	root := docs[0].Root
	kind, _ := root.Get("kind")
	meta, _ := root.Get("metadata")
	name, _ := meta.Get("name")
	got, err := r.CheckFeatures(hubIndex(t), v1alpha1.Kind(kind.Text), name.Text, root, files.Location)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(got))
	for i, d := range got {
		if d.Code != CodeUnserved || d.Severity != diag.SeverityError {
			t.Errorf("%s: finding %+v is not an RZ-CFG-040 error", file, d)
		}
		out[i] = string(d.AppendText(nil))
	}
	return out
}

// TestCheckFeaturesM1 checks the serve check of M1 against the RZ-CFG-040
// row of docs/architecture/02-configuration-model.md "Error codes": each
// unserved kind, field and enum value a resource holds is one finding at
// its source location, served values and absent fields none.
func TestCheckFeaturesM1(t *testing.T) {
	reg := New()
	cases := []struct {
		name, src string
		want      []string
	}{
		{"http3 listener", `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata: {name: edge}
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
    - name: https
      protocol: https
      port: 8443
      http3: true
`, []string{`gw.yaml:12:14 error RZ-CFG-040 Gateway/edge spec.listeners[name=https].http3: field "http3" is not served by this release (Planned (M3))`}},
		// http3 has no default: an explicit false is a value other than it.
		{"http3 false", `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata: {name: edge}
spec:
  listeners:
    - {name: http, protocol: http, port: 8080, http3: false}
`, []string{`gw.yaml:6:55 error RZ-CFG-040 Gateway/edge spec.listeners[name=http].http3: field "http3" is not served by this release (Planned (M3))`}},
		{"served gateway", `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata: {name: edge}
spec:
  listeners:
    - {name: http, protocol: http, port: 8080, proxyProtocol: true}
`, nil},
		{"route matches", `apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: r}
spec:
  match:
    grpc: {service: shop.Cart}
    graphql: {operationType: query}
    topic: orders
  upstreams: [{name: up}]
`, []string{
			`gw.yaml:6:11 error RZ-CFG-040 Route/r spec.match.grpc: field "grpc" is not served by this release (Planned (M3))`,
			`gw.yaml:7:14 error RZ-CFG-040 Route/r spec.match.graphql: field "graphql" is not served by this release (Planned (M3))`,
			`gw.yaml:8:12 error RZ-CFG-040 Route/r spec.match.topic: field "topic" is not served by this release (Planned (M4))`,
		}},
		{"served route", `apiVersion: ruralz/v1alpha1
kind: Route
metadata: {name: r}
spec:
  match: {path: {prefix: /}, methods: [GET]}
  upstreams: [{name: up}]
`, nil},
		{"grpc upstream", `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata: {name: up}
spec:
  protocol: grpc
  endpoints: [{address: up.internal:80}]
`, []string{`gw.yaml:5:13 error RZ-CFG-040 Upstream/up spec.protocol: protocol "grpc" is not served by this release (Planned (M3))`}},
		{"kubernetes discovery and messaging", `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata: {name: up}
spec:
  protocol: kafka
  discovery: {type: kubernetes, service: orders}
  messaging: {topic: orders}
`, []string{
			`gw.yaml:5:13 error RZ-CFG-040 Upstream/up spec.protocol: protocol "kafka" is not served by this release (Planned (M4))`,
			`gw.yaml:6:21 error RZ-CFG-040 Upstream/up spec.discovery.type: type "kubernetes" is not served by this release (Planned (M2))`,
			`gw.yaml:7:14 error RZ-CFG-040 Upstream/up spec.messaging: field "messaging" is not served by this release (Planned (M4))`,
		}},
		{"served upstream", `apiVersion: ruralz/v1alpha1
kind: Upstream
metadata: {name: up}
spec:
  protocol: http
  discovery: {type: dns, service: orders.internal, port: 80}
`, nil},
		{"plugin kind", `apiVersion: ruralz/v1alpha1
kind: Plugin
metadata: {name: p}
spec: {image: "registry.example/p@sha256:0000000000000000000000000000000000000000000000000000000000000000"}
`, []string{`gw.yaml:2:7 error RZ-CFG-040 Plugin/p kind: kind "Plugin" is not served by this release (Planned (M2))`}},
		{"ai provider kind", `apiVersion: ruralz/v1alpha1
kind: AIProvider
metadata: {name: p}
spec: {}
`, []string{`gw.yaml:2:7 error RZ-CFG-040 AIProvider/p kind: kind "AIProvider" is not served by this release (Planned (M3))`}},
		{"ai model kind", `apiVersion: ruralz/v1alpha1
kind: AIModel
metadata: {name: m}
spec: {}
`, []string{`gw.yaml:2:7 error RZ-CFG-040 AIModel/m kind: kind "AIModel" is not served by this release (Planned (M3))`}},
		// Policy types are CheckServed's.
		{"unserved policy type", `apiVersion: ruralz/v1alpha1
kind: Policy
metadata: {name: geo}
spec:
  type: authz.geoip
  config: {}
`, nil},
	}
	for _, c := range cases {
		got := checkDoc(t, reg, "gw.yaml", c.src)
		if !slices.Equal(got, c.want) {
			t.Errorf("%s: findings\n%s\nwant\n%s", c.name, strings.Join(got, "\n"), strings.Join(c.want, "\n"))
		}
	}
}

// TestCheckFeaturesDefault checks the default rule: a feature present with
// its schema default, as stage G materializes it, is served, and any other
// value is not.
func TestCheckFeaturesDefault(t *testing.T) {
	reg := New()
	reg.features = newFeatures([]Feature{{Kind: v1alpha1.KindGateway, Path: "spec.admin.port", Milestone: "M2"}})
	doc := func(port string) string {
		return "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge}\nspec:\n  listeners: [{name: http, protocol: http, port: 8080}]\n  admin: {port: " + port + "}\n"
	}
	if got := checkDoc(t, reg, "gw.yaml", doc("9901")); len(got) != 0 {
		t.Errorf("admin.port at its default 9901: findings %v, want none", got)
	}
	want := []string{`gw.yaml:6:17 error RZ-CFG-040 Gateway/edge spec.admin.port: field "port" is not served by this release (Planned (M2))`}
	if got := checkDoc(t, reg, "gw.yaml", doc("9902")); !slices.Equal(got, want) {
		t.Errorf("admin.port 9902: findings %v, want %v", got, want)
	}
	// An enum value equal to its field's default is served too.
	reg.features = newFeatures([]Feature{{Kind: v1alpha1.KindGateway, Path: "spec.stateStore.driver", Value: "memory", Milestone: "M2"}})
	src := "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: {name: edge}\nspec:\n  listeners: [{name: http, protocol: http, port: 8080}]\n  stateStore: {driver: memory}\n"
	if got := checkDoc(t, reg, "gw.yaml", src); len(got) != 0 {
		t.Errorf("driver at its default memory: findings %v, want none", got)
	}
}

// TestCheckFeaturesPaths checks how rows meet the tree: a Policy config
// row applies to Policies of its type only, a typed-map row to each map
// value, and a field inside a reported field is not reported again.
func TestCheckFeaturesPaths(t *testing.T) {
	reg := New()
	reg.features = newFeatures([]Feature{
		{Kind: v1alpha1.KindPolicy, PolicyType: v1alpha1.PolicyTypeCache, Path: "spec.config.key", Milestone: "M2"},
		{Kind: v1alpha1.KindRoute, Path: "metadata.labels{}", Milestone: "M2"},
		{Kind: v1alpha1.KindUpstream, Path: "spec.healthCheck", Milestone: "M2"},
		{Kind: v1alpha1.KindUpstream, Path: "spec.healthCheck.active", Milestone: "M3"},
	})
	cache := "apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: c}\nspec:\n  type: cache\n  config: {key: route.name}\n"
	want := []string{`p.yaml:6:17 error RZ-CFG-040 Policy/c spec.config.key: field "key" is not served by this release (Planned (M2))`}
	if got := checkDoc(t, reg, "p.yaml", cache); !slices.Equal(got, want) {
		t.Errorf("cache config.key: findings %v, want %v", got, want)
	}
	quota := "apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata: {name: q}\nspec:\n  type: quota\n  config: {key: route.name, consumerQuota: monthly}\n"
	if got := checkDoc(t, reg, "p.yaml", quota); len(got) != 0 {
		t.Errorf("quota config.key: findings %v, want none (the row is cache's)", got)
	}
	route := "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: r\n  labels: {team: shop, tier: gold}\nspec:\n  match: {path: {prefix: /}}\n  upstreams: [{name: up}]\n"
	want = []string{
		`r.yaml:5:18 error RZ-CFG-040 Route/r metadata.labels.team: field "labels" is not served by this release (Planned (M2))`,
		`r.yaml:5:30 error RZ-CFG-040 Route/r metadata.labels.tier: field "labels" is not served by this release (Planned (M2))`,
	}
	if got := checkDoc(t, reg, "r.yaml", route); !slices.Equal(got, want) {
		t.Errorf("labels: findings\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	up := "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata: {name: up}\nspec:\n  protocol: http\n  endpoints: [{address: up.internal:80}]\n  healthCheck:\n    active: {path: /healthz}\n"
	want = []string{`u.yaml:8:5 error RZ-CFG-040 Upstream/up spec.healthCheck: field "healthCheck" is not served by this release (Planned (M2))`}
	if got := checkDoc(t, reg, "u.yaml", up); !slices.Equal(got, want) {
		t.Errorf("nested rows: findings %v, want %v", got, want)
	}
}

// TestFeatures checks the generated table this release reads: every row
// names a milestone later than ReleaseMilestone, and New indexes each.
func TestFeatures(t *testing.T) {
	rows := Features()
	if len(rows) == 0 {
		t.Fatal("the served-feature table is empty")
	}
	release, ok := milestoneOf(ReleaseMilestone)
	if !ok {
		t.Fatalf("ReleaseMilestone %q is not a milestone", ReleaseMilestone)
	}
	reg := New()
	for _, r := range rows {
		if n, ok := milestoneOf(r.Milestone); !ok || n <= release {
			t.Errorf("row %+v: milestone not after %s", r, ReleaseMilestone)
		}
		if r.Path == "" {
			if _, ok := reg.features.kinds[r.Kind]; !ok {
				t.Errorf("row %+v not indexed", r)
			}
			continue
		}
		if !slices.Contains(reg.features.byPath[r.Kind][r.Path], r) {
			t.Errorf("row %+v not indexed", r)
		}
	}
}

// milestoneOf returns n of "M<n>".
func milestoneOf(s string) (int, bool) {
	rest, ok := strings.CutPrefix(s, "M")
	if !ok || len(rest) != 1 || rest[0] < '1' || rest[0] > '9' {
		return 0, false
	}
	return int(rest[0] - '0'), true
}
