// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Fixture kinds of the requiredIf marker.
const (
	KindCondGood      Kind = "CondGood"
	KindCondUnknown   Kind = "CondUnknown"
	KindCondMalformed Kind = "CondMalformed"
	KindCondBadValue  Kind = "CondBadValue"
	KindCondFlag      Kind = "CondFlag"
	KindCondBadFlag   Kind = "CondBadFlag"
)

// CondDriver is a fixture enum for the requiredIf condition.
type CondDriver string

// Fixture drivers.
const (
	CondDriverMemory CondDriver = "memory"
	CondDriverRedis  CondDriver = "redis"
)

// CondGood requires url with driver redis.
type CondGood struct {
	Metadata Meta         `json:"metadata"`
	Spec     CondGoodSpec `json:"spec"`
}

// CondGoodSpec requires url when driver is redis.
// +ruralz:requiredIf=url,driver=redis
type CondGoodSpec struct {
	// Driver selects a backend.
	Driver *CondDriver `json:"driver,omitempty"`
	// URL is the backend address.
	URL string `json:"url,omitempty"`
}

// CondUnknown names a field the struct lacks.
type CondUnknown struct {
	Metadata Meta            `json:"metadata"`
	Spec     CondUnknownSpec `json:"spec"`
}

// CondUnknownSpec names a missing field.
// +ruralz:requiredIf=address,driver=redis
type CondUnknownSpec struct {
	// Driver selects a backend.
	Driver *CondDriver `json:"driver,omitempty"`
}

// CondMalformed has a requiredIf without a condition.
type CondMalformed struct {
	Metadata Meta              `json:"metadata"`
	Spec     CondMalformedSpec `json:"spec"`
}

// CondMalformedSpec has no member=value part.
// +ruralz:requiredIf=url
type CondMalformedSpec struct {
	// Driver selects a backend.
	Driver *CondDriver `json:"driver,omitempty"`
	// URL is the backend address.
	URL string `json:"url,omitempty"`
}

// CondBadValue conditions on a value outside the member's enum.
type CondBadValue struct {
	Metadata Meta             `json:"metadata"`
	Spec     CondBadValueSpec `json:"spec"`
}

// CondBadValueSpec names a driver that does not exist.
// +ruralz:requiredIf=url,driver=rediss
type CondBadValueSpec struct {
	// Driver selects a backend.
	Driver *CondDriver `json:"driver,omitempty"`
	// URL is the backend address.
	URL string `json:"url,omitempty"`
}

// CondFlag requires url when the bool enabled is true.
type CondFlag struct {
	Metadata Meta         `json:"metadata"`
	Spec     CondFlagSpec `json:"spec"`
}

// CondFlagSpec requires url when enabled is true.
// +ruralz:requiredIf=url,enabled=true
type CondFlagSpec struct {
	// Enabled switches the backend on.
	Enabled *bool `json:"enabled,omitempty"`
	// URL is the backend address.
	URL string `json:"url,omitempty"`
}

// CondBadFlag conditions a bool member on a value that is not a bool.
type CondBadFlag struct {
	Metadata Meta            `json:"metadata"`
	Spec     CondBadFlagSpec `json:"spec"`
}

// CondBadFlagSpec names a bool value that does not exist.
// +ruralz:requiredIf=url,enabled=yes
type CondBadFlagSpec struct {
	// Enabled switches the backend on.
	Enabled *bool `json:"enabled,omitempty"`
	// URL is the backend address.
	URL string `json:"url,omitempty"`
}

// condSources parses the fixtures of both fixture files.
func condSources(t *testing.T) sourceInfo {
	t.Helper()
	info, err := parseSources([]string{"fixture_test.go", "required_if_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestRequiredIfMarker checks the struct marker requiredIf=<field>,
// <member>=<value>: it emits an if/then rule requiring field when member
// equals value, in both views; it refuses an unknown field, a value
// without a condition, and a value outside the member's enum.
func TestRequiredIfMarker(t *testing.T) {
	info := condSources(t)
	want := `[{"if":{"properties":{"driver":{"const":"redis"}},"required":["driver"]},"then":{"required":["url"]}}]`
	for _, v := range []view{authoring, rendered} {
		out, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[CondGood]()}}, v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(dig(t, decode(t, out), "$defs", "CondGoodSpec", "allOf"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("view %v: CondGoodSpec allOf = %s, want %s", v, got, want)
		}
	}
	for _, c := range []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[CondUnknown](), `names unknown field "address"`},
		{reflect.TypeFor[CondMalformed](), "want <field>,<member>=<value>"},
		{reflect.TypeFor[CondBadValue](), `"rediss" is not a CondDriver value`},
		{reflect.TypeFor[CondBadFlag](), `"yes" is not true or false`},
	} {
		_, err := generate(input{source: info, resources: []reflect.Type{c.typ}}, rendered)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.typ.Name(), err, c.want)
		}
	}
}

// TestRequiredIfBoolMember checks requiredIf on a bool member: the
// condition's const is the boolean, never the string "true", so the rule
// applies to enabled: true and not to enabled: false or an absent member.
func TestRequiredIfBoolMember(t *testing.T) {
	info := condSources(t)
	want := `[{"if":{"properties":{"enabled":{"const":true}},"required":["enabled"]},"then":{"required":["url"]}}]`
	for _, v := range []view{authoring, rendered} {
		out, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[CondFlag]()}}, v)
		if err != nil {
			t.Fatal(err)
		}
		got, err := json.Marshal(dig(t, decode(t, out), "$defs", "CondFlagSpec", "allOf"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("view %v: CondFlagSpec allOf = %s, want %s", v, got, want)
		}
	}
}

// TestStepCollectionNeedsGroup checks the CompositionStep rule of the
// committed schema (docs/architecture/02-configuration-model.md "Semantic
// validation"): collection: true without group is RZ-CFG-005 at the step,
// at stage F; with group, collection false or absent, the step validates.
func TestStepCollectionNeedsGroup(t *testing.T) {
	views := embeddedViews(t)
	tests := []struct {
		name, fields string
		want         []string
	}{
		{"collection without group", `"collection": true`, []string{"RZ-CFG-005 spec.composition.steps[name=a]"}},
		{"collection with group", `"collection": true, "group": "items"`, nil},
		{"collection false without group", `"collection": false`, nil},
		{"no collection", `"target": "data"`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {` +
				`"match": {"path": {"prefix": "/r"}}, "composition": {"mode": "aggregate", "steps": [` +
				`{"name": "a", "upstream": "u", "path": "/a", ` + tt.fields + `}]}}}`
			if got := validateJSON(t, views, v1alpha1.KindRoute, src); strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("Validate = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStateStoreRedisNeedsURL checks the State Store rule of the
// committed schema: a Gateway whose stateStore sets driver redis without
// url is RZ-CFG-005 at spec.stateStore, while driver redis with url,
// driver memory, or no driver at all, validate.
func TestStateStoreRedisNeedsURL(t *testing.T) {
	for _, v := range []view{authoring, rendered} {
		got, err := json.Marshal(dig(t, committed(t, v), "$defs", "StateStore", "allOf"))
		if err != nil {
			t.Fatal(err)
		}
		if want := `[{"if":{"properties":{"driver":{"const":"redis"}},"required":["driver"]},"then":{"required":["url"]}}]`; string(got) != want {
			t.Errorf("view %v: StateStore allOf = %s, want %s", v, got, want)
		}
	}
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	views, err := schemaview.Embedded(idx)
	if err != nil {
		t.Fatal(err)
	}
	url := `, "url": {"secretRef": {"provider": "env", "name": "RURALZ_STATE_STORE_URL"}}`
	tests := []struct {
		name, stateStore string
		want             []string // "<code> <path>"
	}{
		{"redis without url", `{"driver": "redis"}`, []string{"RZ-CFG-005 spec.stateStore"}},
		{"redis without url, with a timeout", `{"driver": "redis", "timeout": "50ms"}`, []string{"RZ-CFG-005 spec.stateStore"}},
		{"redis with url", `{"driver": "redis"` + url + `}`, nil},
		{"memory without url", `{"driver": "memory"}`, nil},
		{"no driver", `{"timeout": "50ms"}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": {` +
				`"listeners": [{"name": "http", "port": 8080, "protocol": "http"}], "stateStore": ` + tt.stateStore + `}}`
			var got []string
			for _, d := range validateGateway(t, views, src) {
				got = append(got, d.Code+" "+d.Path.String())
			}
			if strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("Validate = %q, want %q", got, tt.want)
			}
		})
	}
}

// validateGateway parses src as one JSON resource and validates it with
// the embedded rendered views.
func validateGateway(t *testing.T, views *schemaview.Set, src string) diag.List {
	t.Helper()
	files := &tree.FileTable{}
	id := files.Add(tree.File{Path: "gateway.json", Role: tree.RoleBase})
	docs, diags, err := profile.Parse(t.Context(), []byte(src), id, "gateway.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("parse: %d documents, %v, %v", len(docs), diags, err)
	}
	res := &tree.Resource{ID: tree.ID{Kind: v1alpha1.KindGateway, Name: "edge"}, APIVersion: v1alpha1.APIVersion, Root: docs[0].Root, Start: docs[0].Start}
	l, _ := views.Validate(res, files)
	return l
}

// validateJSON parses src as one JSON resource of kind and validates it
// with the embedded rendered views, returning "<code> <path>" per finding.
func validateJSON(t *testing.T, views *schemaview.Set, kind v1alpha1.Kind, src string) []string {
	t.Helper()
	files := &tree.FileTable{}
	id := files.Add(tree.File{Path: "r.json", Role: tree.RoleBase})
	docs, diags, err := profile.Parse(t.Context(), []byte(src), id, "r.json", profile.FormatJSON, profile.Options{})
	if err != nil || len(diags) != 0 || len(docs) != 1 {
		t.Fatalf("parse: %d documents, %v, %v", len(docs), diags, err)
	}
	res := &tree.Resource{ID: tree.ID{Kind: kind, Name: "r"}, APIVersion: v1alpha1.APIVersion, Root: docs[0].Root, Start: docs[0].Start}
	l, _ := views.Validate(res, files)
	var out []string
	for _, d := range l {
		out = append(out, d.Code+" "+d.Path.String())
	}
	return out
}

// embeddedViews returns the rendered views of the committed schemas.
func embeddedViews(t *testing.T) *schemaview.Set {
	t.Helper()
	idx, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	views, err := schemaview.Embedded(idx)
	if err != nil {
		t.Fatal(err)
	}
	return views
}

// TestRingHashNeedsHashKey checks the LoadBalancing rule of the committed
// schema (docs/architecture/02-configuration-model.md "Upstream"):
// algorithm ring-hash without hashKey is RZ-CFG-005 at
// spec.loadBalancing, reported once, at stage F; ring-hash with a hashKey,
// another algorithm or none validate.
func TestRingHashNeedsHashKey(t *testing.T) {
	views := embeddedViews(t)
	tests := []struct {
		name, lb string
		want     []string
	}{
		{"ring-hash without hashKey", `{"algorithm": "ring-hash"}`, []string{"RZ-CFG-005 spec.loadBalancing"}},
		{"ring-hash with hashKey", `{"algorithm": "ring-hash", "hashKey": "source.ip"}`, nil},
		{"least-request without hashKey", `{"algorithm": "least-request"}`, nil},
		{"no algorithm", `{}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "r"}, "spec": {` +
				`"protocol": "http", "endpoints": [{"address": "a.svc:80"}], "loadBalancing": ` + tt.lb + `}}`
			if got := validateJSON(t, views, v1alpha1.KindUpstream, src); strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("Validate = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPluginPolicyNeedsPlugin checks the PolicySpec rule of the committed
// schema: type plugin without plugin is RZ-CFG-005 at spec, at stage F;
// with plugin, or another type without it, the Policy validates.
func TestPluginPolicyNeedsPlugin(t *testing.T) {
	views := embeddedViews(t)
	tests := []struct {
		name, spec string
		want       []string
	}{
		{"plugin without plugin", `"type": "plugin", "config": {}`, []string{"RZ-CFG-005 spec"}},
		{"plugin with plugin", `"type": "plugin", "plugin": "geo-block", "config": {}`, nil},
		{"cors without plugin", `"type": "cors", "config": {}`, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "r"}, "spec": {` + tt.spec + `}}`
			if got := validateJSON(t, views, v1alpha1.KindPolicy, src); strings.Join(got, "|") != strings.Join(tt.want, "|") {
				t.Errorf("Validate = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestStepMaxBodyBytesAtLeastOne checks a composition step's maxBodyBytes
// in the committed schema: at least 1 byte as an integer or a quantity
// string, so no spelling of zero reaches the step's leg, which reads 0 as
// "no cap of its own".
func TestStepMaxBodyBytesAtLeastOne(t *testing.T) {
	views := embeddedViews(t)
	const at = "RZ-CFG-005 spec.composition.steps[name=a].maxBodyBytes"
	for _, tt := range []struct {
		value string
		ok    bool
	}{
		{`1`, true},
		{`"1"`, true},
		{`"1Mi"`, true},
		{`"0.5Ki"`, true},
		{`"10.25Mi"`, true},
		{`"010"`, true},
		{`0`, false},
		{`"0"`, false},
		{`"00"`, false},
		{`"0Ki"`, false},
		{`"0.0Mi"`, false},
		{`"000.000Gi"`, false},
	} {
		src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {` +
			`"match": {"path": {"prefix": "/r"}}, "composition": {"mode": "aggregate", "steps": [` +
			`{"name": "a", "upstream": "u", "path": "/a", "maxBodyBytes": ` + tt.value + `}]}}}`
		got := validateJSON(t, views, v1alpha1.KindRoute, src)
		switch {
		case tt.ok && len(got) > 0:
			t.Errorf("maxBodyBytes %s: Validate = %q, want no finding", tt.value, got)
		case !tt.ok && (len(got) == 0 || !strings.HasPrefix(got[0], at)):
			t.Errorf("maxBodyBytes %s: Validate = %q, want %s", tt.value, got, at)
		}
	}
}
