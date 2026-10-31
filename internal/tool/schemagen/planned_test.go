// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// registryDir is the package whose release milestone schemagen reads and
// whose served-feature table it writes.
const registryDir = "../../../internal/config/registry"

// Fixture kinds of the planned marker.
const (
	KindPlanned       Kind = "Planned"
	KindPlannedKind   Kind = "PlannedKind"
	KindPlannedPolicy Kind = "PlannedPolicy"
)

// PlannedProto is a fixture enum with unserved values.
type PlannedProto string

// Fixture protocols.
const (
	// PlannedProtoHTTP is served.
	PlannedProtoHTTP PlannedProto = "http"
	// PlannedProtoGRPC is not served yet.
	// +ruralz:planned=M3
	PlannedProtoGRPC PlannedProto = "grpc"
	// PlannedProtoKafka is not served yet either.
	// +ruralz:planned=M4
	PlannedProtoKafka PlannedProto = "kafka"
)

// Planned holds planned fields and enum values at several depths.
type Planned struct {
	Metadata Meta        `json:"metadata"`
	Spec     PlannedSpec `json:"spec"`
}

// PlannedSpec reaches its planned members through a field, a list and a
// typed map.
type PlannedSpec struct {
	// Proto is an enum with planned values.
	Proto PlannedProto `json:"proto,omitempty"`
	// Protos is a set of them.
	// +ruralz:list=set
	Protos []PlannedProto `json:"protos,omitempty"`
	// Items hold a planned field.
	// +ruralz:list=map,key=name
	Items []PlannedItem `json:"items,omitempty"`
	// ByName maps names to items.
	ByName map[string]PlannedItem `json:"byName,omitempty"`
	// Flag is planned.
	// +ruralz:planned=M2
	Flag *bool `json:"flag,omitempty"`
}

// PlannedItem holds a planned field.
type PlannedItem struct {
	// Name names the item.
	// +ruralz:required
	Name string `json:"name"`
	// Extra is planned.
	// +ruralz:planned=M5
	Extra string `json:"extra,omitempty"`
}

// PlannedKind is a kind whose spec type is planned.
type PlannedKind struct {
	Metadata Meta            `json:"metadata"`
	Spec     PlannedKindSpec `json:"spec"`
}

// PlannedKindSpec is unserved as a whole.
// +ruralz:planned=M2
type PlannedKindSpec struct {
	// Note is a field.
	Note string `json:"note,omitempty"`
}

// PlannedPolicy has a config dispatched by its type.
type PlannedPolicy struct {
	Metadata Meta       `json:"metadata"`
	Spec     PolicySpec `json:"spec"`
}

// PolicySpec is a fixture Policy spec: its config is dispatched by type.
type PolicySpec struct {
	// Type selects the config.
	// +ruralz:required
	Type string `json:"type"`
	// Config is the typed config.
	// +ruralz:required
	Config json.RawMessage `json:"config"`
}

// PlannedConfig is a fixture Policy config.
// +ruralz:policyType=limit
type PlannedConfig struct {
	// Burst is planned.
	// +ruralz:planned=M3
	Burst *int32 `json:"burst,omitempty"`
}

// plannedSources parses the fixtures of the planned marker.
func plannedSources(t *testing.T) sourceInfo {
	t.Helper()
	info, err := parseSources([]string{"fixture_test.go", "planned_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestPlannedTable checks the rows of +ruralz:planned: a kind's spec type
// is one row for the kind, a field one row at each schema path its kinds
// reach it by (fields joined by ".", "[]" for list elements, "{}" for
// typed-map values), an enum constant one row with its value at each path
// of a field of its type, and a Policy config field one row under
// spec.config with its Policy type; rows are sorted.
func TestPlannedTable(t *testing.T) {
	in := input{
		source:    plannedSources(t),
		resources: []reflect.Type{reflect.TypeFor[Planned](), reflect.TypeFor[PlannedKind](), reflect.TypeFor[PlannedPolicy]()},
		configs:   []reflect.Type{reflect.TypeFor[PlannedConfig]()},
	}
	got, err := plannedTable(in, 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []feature{
		{kind: "Planned", path: "spec.byName{}.extra", milestone: "M5"},
		{kind: "Planned", path: "spec.flag", milestone: "M2"},
		{kind: "Planned", path: "spec.items[].extra", milestone: "M5"},
		{kind: "Planned", path: "spec.proto", value: "grpc", milestone: "M3"},
		{kind: "Planned", path: "spec.proto", value: "kafka", milestone: "M4"},
		{kind: "Planned", path: "spec.protos[]", value: "grpc", milestone: "M3"},
		{kind: "Planned", path: "spec.protos[]", value: "kafka", milestone: "M4"},
		{kind: "PlannedKind", milestone: "M2"},
		{kind: "PlannedPolicy", policyType: "limit", path: "spec.config.burst", milestone: "M3"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("plannedTable =\n%+v\nwant\n%+v", got, want)
	}
	src := string(plannedSource(got))
	for _, line := range []string{
		`		{Kind: "Planned", Path: "spec.proto", Value: "grpc", Milestone: "M3"},`,
		`		{Kind: "PlannedKind", Milestone: "M2"},`,
		`		{Kind: "PlannedPolicy", PolicyType: "limit", Path: "spec.config.burst", Milestone: "M3"},`,
	} {
		if !strings.Contains(src, line+"\n") {
			t.Errorf("plannedSource lacks the line %q:\n%s", line, src)
		}
	}
	if !strings.HasPrefix(src, "// Code generated by schemagen. DO NOT EDIT.\n\npackage registry\n") {
		t.Errorf("plannedSource header:\n%s", src)
	}
	if empty := string(plannedSource(nil)); !strings.Contains(empty, "\treturn nil\n") {
		t.Errorf("plannedSource of no row:\n%s", empty)
	}
}

// TestPlannedMilestones checks that a +ruralz:planned value must be a
// milestone after the release's own, up to M5, wherever the marker is.
func TestPlannedMilestones(t *testing.T) {
	in := input{source: plannedSources(t), resources: []reflect.Type{reflect.TypeFor[Planned](), reflect.TypeFor[PlannedKind]()}}
	if _, err := plannedTable(in, 1); err != nil {
		t.Fatalf("release M1: %v", err)
	}
	// At release M2 the M2 markers of the fixtures are served and refused.
	if _, err := plannedTable(in, 2); err == nil || !strings.Contains(err.Error(), "after the release's own (M2)") {
		t.Errorf("release M2: err = %v, want the M2 markers refused", err)
	}
	for _, bad := range []string{"M1", "M6", "M02", "m3", "M", "3", "M+3", "M3x"} {
		cases := map[string]string{
			"field":    "type T struct {\n\t// F is a field.\n\t// +ruralz:planned=" + bad + "\n\tF string `json:\"f,omitempty\"`\n}\n",
			"constant": "type E string\n\nconst (\n\t// EA is a value.\n\t// +ruralz:planned=" + bad + "\n\tEA E = \"a\"\n)\n",
			"spec":     "// PlannedKindSpec is a spec type.\n// +ruralz:planned=" + bad + "\ntype PlannedKindSpec struct{}\n",
		}
		for where, src := range cases {
			info := parseTemp(t, src)
			_, err := plannedTable(input{source: info, resources: []reflect.Type{reflect.TypeFor[PlannedKind]()}}, 1)
			if err == nil || !strings.Contains(err.Error(), "want a milestone after the release's own (M1), up to M5") {
				t.Errorf("%s %q: err = %v, want the milestone refused", where, bad, err)
			}
		}
	}
}

// TestPlannedMarkerPlaces checks where +ruralz:planned may stand: not on a
// struct type that is no kind's spec type, not on a Kind or PolicyType
// constant, not on a named string type, and a constant takes no other
// marker.
func TestPlannedMarkerPlaces(t *testing.T) {
	kinds := []reflect.Type{reflect.TypeFor[PlannedKind]()}
	cases := []struct {
		name, src, want string
	}{
		{"struct", "// T is a struct.\n// +ruralz:planned=M2\ntype T struct{}\n", "is only for a kind's spec type"},
		{"kind constant", "type Kind string\n\nconst (\n\t// KindX is a kind.\n\t// +ruralz:planned=M2\n\tKindX Kind = \"X\"\n)\n", "goes on the kind's spec type"},
		{"policy type constant", "type PolicyType string\n\nconst (\n\t// PolicyTypeX is a type.\n\t// +ruralz:planned=M2\n\tPolicyTypeX PolicyType = \"x\"\n)\n", "Policy type registry"},
	}
	for _, c := range cases {
		_, err := plannedTable(input{source: parseTemp(t, c.src), resources: kinds}, 1)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
	// A named string type takes no planned marker; its constants do.
	info := parseTemp(t, "// S is a string.\n// +ruralz:planned=M2\ntype S string\n")
	g := &generator{in: input{source: sourceInfo{"PlannedProto": {doc: info["S"].doc}}}, defs: map[string]schema{}, building: map[string]bool{}, known: newWellKnown()}
	if _, err := g.stringDef(reflect.TypeFor[PlannedProto]()); err == nil || !strings.Contains(err.Error(), "marker planned is not allowed on a string type") {
		t.Errorf("string type: err = %v, want the marker refused", err)
	}
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte("package p\n\ntype E string\n\nconst (\n\t// EA is a value.\n\t// +ruralz:required\n\tEA E = \"a\"\n)\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := parseSources([]string{path}); err == nil || !strings.Contains(err.Error(), "marker required is not allowed on a constant") {
		t.Errorf("constant marker: err = %v, want it refused", err)
	}
}

// TestPlannedNoSchemaKeyword checks that the marker emits no schema
// keyword: a planned field, spec type or enum value keeps the schema it
// had without the marker.
func TestPlannedNoSchemaKeyword(t *testing.T) {
	info := plannedSources(t)
	for _, typ := range []reflect.Type{reflect.TypeFor[Planned](), reflect.TypeFor[PlannedKind]()} {
		out, err := generate(input{source: info, resources: []reflect.Type{typ}}, rendered)
		if err != nil {
			t.Fatalf("%s: %v", typ.Name(), err)
		}
		if bytes.Contains(out, []byte("ruralz:planned")) || bytes.Contains(out, []byte("x-ruralz-planned")) {
			t.Errorf("%s: the schema carries the marker:\n%s", typ.Name(), out)
		}
	}
	out, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[Planned]()}}, rendered)
	if err != nil {
		t.Fatal(err)
	}
	enum, err := json.Marshal(dig(t, decode(t, out), "$defs", "PlannedProto", "enum"))
	if err != nil {
		t.Fatal(err)
	}
	if string(enum) != `["http","grpc","kafka"]` {
		t.Errorf("PlannedProto enum = %s, want every value", enum)
	}
}

// parseTemp parses src, a file of package p without its package clause.
func parseTemp(t *testing.T, src string) sourceInfo {
	t.Helper()
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte("package p\n\n"+src), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := parseSources([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestReadRelease checks that schemagen reads the release milestone from
// the ReleaseMilestone string constant of internal/config/registry.
func TestReadRelease(t *testing.T) {
	n, err := readRelease(registryDir)
	if err != nil || n != 1 {
		t.Errorf("readRelease(registry) = %d, %v; want 1", n, err)
	}
	cases := []struct {
		name, src, want string
	}{
		{"absent", "const Other = \"M1\"\n", "no ReleaseMilestone string constant"},
		{"not a literal", "const ReleaseMilestone = Other\n\nconst Other = \"M1\"\n", "is not a string literal"},
		{"not a milestone", "const ReleaseMilestone = \"one\"\n", "is not a milestone before M5"},
		{"last milestone", "const ReleaseMilestone = \"M5\"\n", "is not a milestone before M5"},
	}
	for _, c := range cases {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "r.go"), []byte("package registry\n\n"+c.src), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readRelease(dir); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.want)
		}
	}
}

// TestCommittedPlannedIsCurrent fails when the committed served-feature
// table differs from what the markers generate; run `make generate`.
func TestCommittedPlannedIsCurrent(t *testing.T) {
	release, err := readRelease(registryDir)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := plannedTable(realInput(t), release)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join(registryDir, plannedFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(plannedSource(rows), want) {
		t.Errorf("%s is stale; run `make generate`", plannedFile)
	}
}

// TestM1Planned pins the served-feature table of M1 to the RZ-CFG-040 row
// of docs/architecture/02-configuration-model.md "Error codes": the kinds
// Plugin, AIProvider and AIModel; an Upstream protocol other than http,
// kubernetes discovery and messaging; a listener's http3; a Route's
// match.grpc, match.graphql and match.topic. Policy types take theirs from
// the Policy type registry, so none is a row.
func TestM1Planned(t *testing.T) {
	rows, err := plannedTable(realInput(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	want := []feature{
		{kind: "AIModel", milestone: "M3"},
		{kind: "AIProvider", milestone: "M3"},
		{kind: "Gateway", path: "spec.listeners[].http3", milestone: "M3"},
		{kind: "Plugin", milestone: "M2"},
		{kind: "Route", path: "spec.match.graphql", milestone: "M3"},
		{kind: "Route", path: "spec.match.grpc", milestone: "M3"},
		{kind: "Route", path: "spec.match.topic", milestone: "M4"},
		{kind: "Upstream", path: "spec.discovery.type", value: "kubernetes", milestone: "M2"},
		{kind: "Upstream", path: "spec.messaging", milestone: "M4"},
		{kind: "Upstream", path: "spec.protocol", value: "ai", milestone: "M3"},
		{kind: "Upstream", path: "spec.protocol", value: "graphql", milestone: "M3"},
		{kind: "Upstream", path: "spec.protocol", value: "grpc", milestone: "M3"},
		{kind: "Upstream", path: "spec.protocol", value: "kafka", milestone: "M4"},
		{kind: "Upstream", path: "spec.protocol", value: "mqtt", milestone: "M4"},
		{kind: "Upstream", path: "spec.protocol", value: "nats", milestone: "M4"},
		{kind: "Upstream", path: "spec.protocol", value: "websocket", milestone: "M3"},
	}
	if !slices.Equal(rows, want) {
		t.Errorf("M1 served-feature table =\n%+v\nwant\n%+v", rows, want)
	}
	for _, r := range rows {
		if r.value == "http" || r.value == "dns" {
			t.Errorf("row %+v marks a served value", r)
		}
	}
}

// TestPlannedPathsResolve checks every row of the committed table against
// the generated schema: its path is a schema path of its kind
// (schemaidx.Index.Fields), and its value one of the enum there, so the
// serve check, which walks the schema, finds each row.
func TestPlannedPathsResolve(t *testing.T) {
	set, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	idx, ok := set.Index(v1alpha1.APIVersion)
	if !ok {
		t.Fatal("no index of the served apiVersion")
	}
	type key struct{ kind, path string }
	fields := map[key]*schemaidx.Node{}
	for f := range idx.FieldsSeq() {
		fields[key{f.Kind, f.Path}] = f.Node
	}
	rows, err := plannedTable(realInput(t), 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.path == "" {
			if _, ok := idx.Resource(r.kind); !ok {
				t.Errorf("row %+v: no kind %q in the schema", r, r.kind)
			}
			continue
		}
		n, ok := fields[key{r.kind, r.path}]
		if !ok {
			t.Errorf("row %+v: %s holds no schema path %q", r, r.kind, r.path)
			continue
		}
		if r.value != "" && !slices.Contains(n.Enum(), r.value) {
			t.Errorf("row %+v: %q is not in the enum %v", r, r.value, n.Enum())
		}
	}
}
