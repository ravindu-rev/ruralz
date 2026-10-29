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
	} {
		_, err := generate(input{source: info, resources: []reflect.Type{c.typ}}, rendered)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.typ.Name(), err, c.want)
		}
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
