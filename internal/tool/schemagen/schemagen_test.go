// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	pkgDir    = "../../../pkg/config/v1alpha1"
	schemaDir = "../../../api/schema/ruralz/v1alpha1"
)

func realInput(t *testing.T) input {
	t.Helper()
	files, err := sourceFiles(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	info, err := parseSources(files)
	if err != nil {
		t.Fatal(err)
	}
	return input{source: info, resources: resourceTypes(), configs: policyConfigTypes()}
}

// TestCommittedSchemaIsCurrent fails when the committed schema files differ
// from what the Go types generate; run `make generate`.
func TestCommittedSchemaIsCurrent(t *testing.T) {
	in := realInput(t)
	for v, name := range outputs() {
		got, err := generate(in, v)
		if err != nil {
			t.Fatalf("%s: %v", v, err)
		}
		want, err := os.ReadFile(filepath.Join(schemaDir, name)) //nolint:gosec // Test reads the committed schema files.
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale; run `make generate`", name)
		}
	}
}

func TestDeterministic(t *testing.T) {
	in := realInput(t)
	a, err := generate(in, rendered)
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		b, err := generate(realInput(t), rendered)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(a, b) {
			t.Fatal("output differs between runs")
		}
	}
	if !bytes.HasSuffix(a, []byte("}\n")) || bytes.Contains(a, []byte("\r")) {
		t.Fatal("output must end with a newline and use LF line endings")
	}
	escape := `\` + "u00" // encoding/json writes <, > and & as this prefix plus hex when HTML-escaping
	if bytes.Contains(a, []byte(escape+"3c")) || bytes.Contains(a, []byte(escape+"26")) {
		t.Fatal("output must not HTML-escape")
	}
}

func decode(t *testing.T, data []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func dig(t *testing.T, m any, path ...string) any {
	t.Helper()
	for _, p := range path {
		obj, ok := m.(map[string]any)
		if !ok {
			t.Fatalf("path %v: %q is not an object", path, p)
		}
		m, ok = obj[p]
		if !ok {
			t.Fatalf("path %v: no %q", path, p)
		}
	}
	return m
}

func TestViewsDiffer(t *testing.T) {
	in := realInput(t)
	auth, err := generate(in, authoring)
	if err != nil {
		t.Fatal(err)
	}
	rend, err := generate(in, rendered)
	if err != nil {
		t.Fatal(err)
	}
	a, r := decode(t, auth), decode(t, rend)

	port := dig(t, r, "$defs", "Listener", "properties", "port").(map[string]any)
	if port["type"] != "integer" || port["anyOf"] != nil {
		t.Errorf("rendered port = %v", port)
	}
	aport := dig(t, a, "$defs", "Listener", "properties", "port").(map[string]any)
	anyOf, _ := aport["anyOf"].([]any)
	if len(anyOf) != 2 || !strings.Contains(anyOf[1].(map[string]any)["$ref"].(string), "Substitution") {
		t.Errorf("authoring port = %v", aport)
	}
	// Substitution is never offered in ref, secret or CEL fields, or the envelope.
	for _, path := range [][]string{
		{"$defs", "PolicyRef", "properties", "name"},
		{"$defs", "Certificate", "properties", "certificate"},
		{"$defs", "RouteMatch", "properties", "when"},
		{"$defs", "ObjectMeta", "properties", "name"},
	} {
		if dig(t, a, path...).(map[string]any)["anyOf"] != nil {
			t.Errorf("%v accepts substitution", path)
		}
	}
	if _, ok := dig(t, a, "$defs").(map[string]any)["Substitution"]; !ok {
		t.Error("authoring view lacks the Substitution definition")
	}
	if _, ok := dig(t, r, "$defs").(map[string]any)["Substitution"]; ok {
		t.Error("rendered view has the Substitution definition")
	}
}

func TestKeywords(t *testing.T) {
	out, err := generate(realInput(t), rendered)
	if err != nil {
		t.Fatal(err)
	}
	s := decode(t, out)
	cases := []struct {
		path []string
		want string
	}{
		{[]string{"$defs", "RouteSpec", "properties", "policies", "x-ruralz-list"}, `{"key":"name","type":"orderedMap"}`},
		{[]string{"$defs", "RouteMatch", "properties", "when", "x-ruralz-cel"}, `{"result":"bool","variables":["request","source","now"]}`},
		{[]string{"$defs", "PolicyRef", "properties", "name", "x-ruralz-ref"}, `"Policy"`},
		{[]string{"$defs", "Certificate", "properties", "privateKey", "x-ruralz-secret"}, `true`},
		{[]string{"$defs", "PluginSpec", "properties", "capabilities", "x-ruralz-impact"}, `["plugin","security"]`},
		{[]string{"$defs", "ListenerTLS", "properties", "minVersion", "default"}, `"1.3"`},
		{[]string{"$defs", "Limits", "properties", "maxBufferedBytes", "default"}, `536870912`},
		{[]string{"$defs", "AuthUpstreamOAuth2Config", "properties", "timeout", "default"}, `"2s"`},
		{[]string{"$defs", "RateLimitConfig", "properties", "limits", "minItems"}, `1`},
		{[]string{"$defs", "PathMatch", "allOf"}, `[{"oneOf":[{"required":["exact"]},{"required":["prefix"]},{"required":["template"]},{"required":["regex"]}]}]`},
		{[]string{"$defs", "UpstreamSpec", "allOf"}, `[{"not":{"required":["endpoints","discovery"]}}]`},
	}
	for _, c := range cases {
		got, err := json.Marshal(dig(t, s, c.path...))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("%v = %s, want %s", c.path, got, c.want)
		}
	}
	// Closed and open configs.
	if dig(t, s, "$defs", "AuthMTLSConfig", "additionalProperties") != false {
		t.Error("auth.mtls config must be closed")
	}
	if _, ok := dig(t, s, "$defs", "RateLimitConfig").(map[string]any)["additionalProperties"]; ok {
		t.Error("ratelimit config must be open until its feature fields are registered")
	}
	// Every Policy type selects a config.
	dispatch := dig(t, s, "$defs", "PolicySpec", "allOf").([]any)
	if len(dispatch) != 23 {
		t.Errorf("PolicySpec dispatches %d types, want 23", len(dispatch))
	}
}

func TestParseDoc(t *testing.T) {
	src := `package p

// T is a type.
// +ruralz:exactlyOneOf=a,b
type T struct {
	// A is a field
	// spanning two lines.
	// +ruralz:required
	// +ruralz:pattern=^x$
	A string ` + "`json:\"a\"`" + `
}
`
	path := filepath.Join(t.TempDir(), "p.go")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	info, err := parseSources([]string{path})
	if err != nil {
		t.Fatal(err)
	}
	f := info["T"].fields["A"]
	if f.description != "A is a field spanning two lines." {
		t.Errorf("description = %q", f.description)
	}
	if m, ok := f.get("pattern"); !ok || m.value != "^x$" {
		t.Errorf("pattern marker = %+v", m)
	}
	if _, ok := f.get("required"); !ok {
		t.Error("required marker lost")
	}
	if m, _ := info["T"].doc.get("exactlyOneOf"); m.value != "a,b" {
		t.Errorf("type marker = %+v", m)
	}

	for _, bad := range []string{"// +ruralz:bogus", "// +ruralz:required=yes", "// +ruralz:ref", "// +ruralz:ref="} {
		src := "package p\n\n" + bad + "\ntype U string\n"
		path := filepath.Join(t.TempDir(), "bad.go")
		if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := parseSources([]string{path}); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestListKeyword(t *testing.T) {
	for _, bad := range []string{"map", "orderedMap,key=", "set,key=name", "list", "map,name"} {
		if _, err := listKeyword(bad); err == nil {
			t.Errorf("listKeyword(%q): want an error", bad)
		}
	}
}

func TestRuleViolations(t *testing.T) {
	info, err := parseSources([]string{"fixture_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[OptionalBool](), "optional bool or number must be a pointer"},
		{reflect.TypeFor[UnmarkedList](), "needs +ruralz:list"},
		{reflect.TypeFor[BadRef](), "is not a kind"},
		{reflect.TypeFor[UnmarkedSecret](), "+ruralz:secret must mark"},
		{reflect.TypeFor[BadOneOf](), "names unknown field"},
		{reflect.TypeFor[NoOmitempty](), "optional field needs omitempty"},
		{reflect.TypeFor[ValueDefault](), "a field with a default must be a pointer"},
		{reflect.TypeFor[BadEnumDefault](), "is not a Mode value"},
	}
	for _, c := range cases {
		_, err := generate(input{source: info, resources: []reflect.Type{c.typ}}, rendered)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.typ.Name(), err, c.want)
		}
	}
	if _, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[Good]()}}, authoring); err != nil {
		t.Errorf("Good: %v", err)
	}
}
