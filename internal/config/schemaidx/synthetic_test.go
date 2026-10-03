// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// synthetic exercises what the committed schema does not use yet:
// x-ruralz-since (02 req 23 and 75), x-ruralz-validations, if/then/else
// with two rules, recursion, number and object defaults.
const synthetic = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "https://example.invalid/test.schema.json",
  "type": "object",
  "required": ["apiVersion", "kind", "metadata", "spec"],
  "properties": {"apiVersion": {"const": "test/v1"}, "kind": {"enum": ["Thing", "Loop"]}},
  "allOf": [
    {"if": {"properties": {"kind": {"const": "Thing"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Thing"}},
    {"if": {"properties": {"kind": {"const": "Loop"}}, "required": ["kind"]}, "then": {"$ref": "#/$defs/Loop"}}
  ],
  "$defs": {
    "Meta": {"type": "object", "additionalProperties": false, "required": ["name"], "properties": {
      "name": {"type": "string"},
      "labels": {"type": "object", "additionalProperties": {"type": "string"}}}},
    "Thing": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Thing"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"$ref": "#/$defs/ThingSpec"}}},
    "ThingSpec": {"type": "object", "additionalProperties": false, "properties": {
      "old": {"type": "integer"},
      "added": {"type": "integer", "default": 3, "x-ruralz-since": 2},
      "later": {"type": "string", "x-ruralz-since": 1,
        "x-ruralz-validations": [{"rule": "self != ''", "message": "empty"}, {"rule": "size(self) < 9"}]},
      "mode": {"type": "string", "enum": ["a", "b"]},
      "opts": {"type": "object"},
      "size": {"$ref": "#/$defs/ByteSize", "default": 1024},
      "ratio": {"type": "number", "default": 0.5},
      "count": {"type": "integer", "default": 1e3},
      "wait": {"$ref": "#/$defs/Duration", "default": "1s"},
      "port": {"$ref": "#/$defs/IntOrString"},
      "price": {"$ref": "#/$defs/Decimal"},
      "obj": {"type": "object", "default": {"b": 1, "a": ["x", true, null]}},
      "tags": {"type": "array", "items": {"type": "string"}, "x-ruralz-list": {"type": "set"}},
      "nums": {"type": "array", "items": {"type": "number"}, "x-ruralz-list": {"type": "set"}},
      "flags": {"type": "array", "items": {"type": "boolean"}, "x-ruralz-list": {"type": "set"}},
      "objs": {"type": "array", "items": {"type": "object"}, "x-ruralz-list": {"type": "set"}},
      "entries": {"type": "array", "items": {"$ref": "#/$defs/Entry"}, "x-ruralz-list": {"type": "map", "key": "id"}},
      "ordered": {"type": "array", "items": {"$ref": "#/$defs/Entry"}, "x-ruralz-list": {"type": "orderedMap", "key": "id"}},
      "raw": {"type": "array", "items": {"type": "object"}, "x-ruralz-list": {"type": "atomic"}},
      "free": {"type": "array"},
      "secret": {"$ref": "#/$defs/Secret", "x-ruralz-secret": true, "x-ruralz-impact": ["security"]},
      "target": {"type": "string", "x-ruralz-ref": "Thing"},
      "rule": {"type": "string", "x-ruralz-cel": {"variables": ["request", "now"], "result": "bool"}},
      "variant": {"$ref": "#/$defs/Variant", "x-ruralz-impact": ["traffic", "ai"]},
      "never": false,
      "any": true}},
    "Entry": {"type": "object", "additionalProperties": false, "required": ["id"], "properties": {
      "id": {"type": "string"}, "val": {"$ref": "#/$defs/Duration"}}},
    "Secret": {"type": "object", "additionalProperties": false, "properties": {
      "ref": {"type": "object", "properties": {"name": {"type": "string"}}}}},
    "Variant": {"type": "object", "properties": {
        "type": {"type": "string"}, "config": {"type": "object"}, "extra": {"type": "string"}},
      "allOf": [
        {"not": {"required": ["type", "flag"]}},
        {"if": {"properties": {"type": {"const": "x"}}, "required": ["type"]},
         "then": {"properties": {"config": {"$ref": "#/$defs/XConfig"}}},
         "else": {"properties": {"extra": {"default": "not-x"}}}},
        {"if": {"properties": {"flag": {"const": true}}, "required": ["flag"]},
         "then": {"properties": {"config": {"$ref": "#/$defs/FlagConfig"}}}}]},
    "XConfig": {"type": "object", "additionalProperties": false, "properties": {"x": {"type": "integer", "default": 7}}},
    "FlagConfig": {"type": "object", "properties": {"f": {"type": "boolean", "default": true}}},
    "Loop": {"type": "object", "additionalProperties": false, "properties": {
      "apiVersion": {"const": "test/v1"}, "kind": {"const": "Loop"},
      "metadata": {"$ref": "#/$defs/Meta"}, "spec": {"$ref": "#/$defs/Tree"}}},
    "Tree": {"type": "object", "additionalProperties": false, "properties": {
      "value": {"type": "string"},
      "children": {"type": "array", "items": {"$ref": "#/$defs/Tree"}, "x-ruralz-list": {"type": "atomic"}}}},
    "Duration": {"type": "string", "pattern": "^.+$"},
    "Decimal": {"type": "string"},
    "IntOrString": {"anyOf": [{"type": "integer"}, {"type": "string"}]},
    "ByteSize": {"anyOf": [{"type": "integer", "minimum": 0}, {"type": "string"}]}
  }
}`

func thingSpec(t *testing.T, x *Index) *Node {
	t.Helper()
	spec, ok := x.Spec("Thing")
	if !ok {
		t.Fatal("no Thing spec")
	}
	return spec
}

func TestSinceAndLevel(t *testing.T) {
	// 02 req 23 and 75 with a synthetic schema: the level is the maximum
	// x-ruralz-since; each field reports its own level and default.
	x := mustLoad(t, synthetic)
	if x.Level() != 2 {
		t.Fatalf("Level() = %d, want 2", x.Level())
	}
	spec := thingSpec(t, x)
	for name, want := range map[string]int{"old": 0, "added": 2, "later": 1} {
		p, _ := spec.Property(name)
		if p.Since() != want || p.Keywords().Since != want {
			t.Errorf("%s: Since() = %d, want %d", name, p.Since(), want)
		}
	}
	added, _ := spec.Property("added")
	if d, ok := added.Default(); !ok || d != json.Number("3") {
		t.Errorf("added default = %v %v", d, ok)
	}
	if got := mustLoad(t, strings.Replace(synthetic, `"x-ruralz-since": 2`, `"x-ruralz-since": 0`, 1)).Level(); got != 1 {
		t.Errorf("Level() without the level-2 field = %d, want 1", got)
	}
}

func TestValidationsParsed(t *testing.T) {
	// x-ruralz-validations are parsed, not evaluated, in M1.
	p, _ := thingSpec(t, mustLoad(t, synthetic)).Property("later")
	want := []Validation{{Rule: "self != ''", Message: "empty"}, {Rule: "size(self) < 9"}}
	got := p.Keywords().Validations
	if !slices.Equal(got, want) {
		t.Fatalf("Validations = %+v, want %+v", got, want)
	}
	got[0].Rule = "mutated"
	if p.Keywords().Validations[0].Rule != "self != ''" {
		t.Error("Keywords exposes the index's validations")
	}
}

func TestTypesScalarsEnums(t *testing.T) {
	// 01 req 29 and 37, 02 req 15: types, scalar definitions and enums.
	spec := thingSpec(t, mustLoad(t, synthetic))
	for _, tc := range []struct {
		name   string
		types  TypeSet
		scalar Scalar
		str    string
	}{
		{"old", TypeInteger, ScalarNone, "integer"},
		{"ratio", TypeNumber | TypeInteger, ScalarNone, "number"},
		{"size", TypeInteger | TypeString, ScalarByteSize, "integer|string"},
		{"port", TypeInteger | TypeString, ScalarIntOrString, "integer|string"},
		{"wait", TypeString, ScalarDuration, "string"},
		{"price", TypeString, ScalarDecimal, "string"},
		{"mode", TypeString, ScalarNone, "string"},
		{"tags", TypeArray, ScalarNone, "array"},
		{"opts", TypeObject, ScalarNone, "object"},
		{"never", 0, ScalarNone, ""},
		{"any", AllTypes, ScalarNone, "null|boolean|object|array|number|string"},
	} {
		p, ok := spec.Property(tc.name)
		if !ok || p.Types() != tc.types || p.Scalar() != tc.scalar || p.Types().String() != tc.str {
			t.Errorf("%s: types %s (%v) scalar %s, want %s scalar %s", tc.name, p.Types(), ok, p.Scalar(), tc.str, tc.scalar)
		}
	}
	mode, _ := spec.Property("mode")
	if !slices.Equal(mode.Enum(), []string{"a", "b"}) {
		t.Errorf("mode enum = %v", mode.Enum())
	}
	if old, _ := spec.Property("old"); old.Enum() != nil {
		t.Errorf("old enum = %v", old.Enum())
	}
	for s, want := range map[Scalar]string{ScalarNone: "", ScalarDuration: "Duration", ScalarByteSize: "ByteSize", ScalarDecimal: "Decimal", ScalarIntOrString: "IntOrString"} {
		if s.String() != want {
			t.Errorf("Scalar(%d).String() = %q", s, s.String())
		}
	}
	if TypeInteger.String() != "integer" || (TypeNull|TypeBoolean).String() != "null|boolean" {
		t.Error("TypeSet.String")
	}
}

func TestDefaultNodes(t *testing.T) {
	// 01 req 36 and 37: defaults become defaulted tree nodes; a number at
	// an integer position is integer text; objects keep member order by
	// name.
	spec := thingSpec(t, mustLoad(t, synthetic))
	for _, tc := range []struct {
		name string
		kind tree.Kind
		text string
	}{
		{"size", tree.KindInt, "1024"},
		{"count", tree.KindInt, "1000"},
		{"ratio", tree.KindFloat, "0.5"},
		{"wait", tree.KindString, "1s"},
		{"added", tree.KindInt, "3"},
	} {
		p, _ := spec.Property(tc.name)
		dn := p.DefaultNode()
		if dn == nil || dn.Kind != tc.kind || dn.Text != tc.text || dn.Style != tree.StyleDefaulted {
			t.Errorf("%s: DefaultNode = %+v, want %v %q", tc.name, dn, tc.kind, tc.text)
		}
	}
	obj, _ := spec.Property("obj")
	dn := obj.DefaultNode()
	if dn.Kind != tree.KindMap || len(dn.Members) != 2 || dn.Members[0].Key != "a" || dn.Members[1].Key != "b" {
		t.Fatalf("obj DefaultNode = %+v", dn)
	}
	a := dn.Members[0].Value
	if a.Kind != tree.KindList || len(a.Items) != 3 || a.Items[1].Kind != tree.KindBool || a.Items[2].Kind != tree.KindNull {
		t.Errorf("obj.a = %+v", a)
	}
	d, _ := obj.Default()
	d.(map[string]any)["b"] = "mutated"
	if d2, _ := obj.Default(); d2.(map[string]any)["b"] != json.Number("1") {
		t.Error("Default exposes the index's value")
	}
	var names []string
	for name := range spec.Defaults() {
		names = append(names, name)
	}
	if !slices.Equal(names, []string{"added", "count", "obj", "ratio", "size", "wait"}) {
		t.Errorf("Defaults() = %v", names)
	}
	for range spec.Defaults() {
		break // early stop is honored
	}
	for range spec.Properties() {
		break
	}
}

func TestDefaultNodeNestedIntegers(t *testing.T) {
	// 01 req 37 inside structured defaults, and 02 req 10 (a new
	// +ruralz:default needs no code change): each member and element of an
	// object or array default takes its own schema, typed-map values and
	// dispatch included; a number at an integer-only position is integer
	// text, elsewhere it keeps its text.
	x := mustLoad(t, `{"properties":{"o":{"type":"object","properties":{
	    "n":{"type":"integer"},"r":{"type":"number"},
	    "l":{"type":"array","items":{"type":"integer"}},
	    "m":{"type":"object","additionalProperties":{"type":"integer"}},
	    "v":{"type":"object","properties":{"t":{"type":"string"}},"allOf":[
	      {"if":{"properties":{"t":{"const":"i"}},"required":["t"]},"then":{"properties":{"k":{"type":"integer"}}}}]}},
	  "default":{"n":1.0,"r":2.0,"l":[1e1,2.0,3],"m":{"a":3.0},"v":{"t":"i","k":4.0},"f":5.0,"w":{"k":6.0}}}}}`)
	o, _ := x.Root().Property("o")
	dn := o.DefaultNode()
	for _, tc := range []struct {
		path []any
		kind tree.Kind
		text string
	}{
		{[]any{"n"}, tree.KindInt, "1"},
		{[]any{"r"}, tree.KindFloat, "2.0"},
		{[]any{"l", 0}, tree.KindInt, "10"},
		{[]any{"l", 1}, tree.KindInt, "2"},
		{[]any{"l", 2}, tree.KindInt, "3"},
		{[]any{"m", "a"}, tree.KindInt, "3"},
		{[]any{"v", "k"}, tree.KindInt, "4"},     // through the dispatch on t
		{[]any{"f"}, tree.KindFloat, "5.0"},      // undeclared member of an open object
		{[]any{"w", "k"}, tree.KindFloat, "6.0"}, // no schema below w
	} {
		n, ok := dn.At(pathOf(tc.path...))
		if !ok || n.Kind != tc.kind || n.Text != tc.text || n.Style != tree.StyleDefaulted {
			t.Errorf("default %v = %+v, want %v %q", tc.path, n, tc.kind, tc.text)
		}
	}
	// Normalization works on the copy: the stored default keeps its
	// authored text.
	if d, _ := o.Default(); d.(map[string]any)["n"] != json.Number("1.0") {
		t.Errorf("Default().n = %v, want the authored 1.0", d.(map[string]any)["n"])
	}
}

func TestDispatchElseAndCombinations(t *testing.T) {
	// 01 req 25 and 02 req 11: if/then/else dispatch evaluated on the
	// instance, including else branches and combinations of two rules
	// that Load does not precompute.
	spec := thingSpec(t, mustLoad(t, synthetic))
	variant, _ := spec.Property("variant")
	if got := variant.DispatchValues("type"); !slices.Equal(got, []string{"x"}) {
		t.Errorf("DispatchValues(type) = %v", got)
	}
	if got := variant.DispatchValues("nothing"); got != nil {
		t.Errorf("DispatchValues(nothing) = %v", got)
	}
	cases := []struct {
		inst       string
		configDef  string
		extraDflt  bool
		configKeys []string
	}{
		{`{}`, "", true, nil},
		{`{"type":"y"}`, "", true, nil},
		{`{"type":"x"}`, "XConfig", false, []string{"x"}},
		{`{"flag":true}`, "FlagConfig", true, []string{"f"}},
		{`{"flag":"true"}`, "", true, nil},
		{`{"type":"x","flag":true}`, "XConfig", false, []string{"f", "x"}},
	}
	for _, tc := range cases {
		for range 2 { // the second round reads the same result
			sel := variant.Select(parseTree(t, tc.inst))
			cfg, _ := sel.Property("config")
			extra, _ := sel.Property("extra")
			_, hasDflt := extra.Default()
			if cfg.Def() != tc.configDef || hasDflt != tc.extraDflt || !slices.Equal(cfg.PropertyNames(), tc.configKeys) {
				t.Errorf("Select(%s): config %q %v, extra default %v", tc.inst, cfg.Def(), cfg.PropertyNames(), hasDflt)
			}
		}
	}
	if variant.SelectValue("type", "x") == variant {
		t.Error("SelectValue(type, x) changed nothing")
	}
	if v := variant.Select(nil); v != variant {
		t.Error("Select(nil) changed the node")
	}
	if v := variant.Select(parseTree(t, `[1]`)); v != variant {
		t.Error("Select(list) changed the node")
	}
	if _, ok := variant.PolicyConfig("y"); ok {
		t.Error("PolicyConfig(y) matched")
	}
	if cfg, ok := variant.PolicyConfig("x"); !ok || cfg.Def() != "XConfig" {
		t.Errorf("PolicyConfig(x) = %q %v", cfg.Def(), ok)
	}
}

func TestRecursiveSchema(t *testing.T) {
	x := mustLoad(t, synthetic)
	res := parseTree(t, `{"spec":{"children":[{"children":[{"value":"leaf"}]}]}}`)
	info, ok := x.Lookup("Loop", res, pathOf("spec", "children", 0, "children", 0, "value"))
	if !ok || !info.Node.Types().Has(TypeString) {
		t.Fatalf("deep recursive lookup = %+v %v", info, ok)
	}
	var paths []string
	for _, f := range x.Fields() {
		if f.Kind == "Loop" {
			paths = append(paths, f.Path)
		}
	}
	if !slices.Contains(paths, "spec.children[]") || slices.Contains(paths, "spec.children[].children[].children[]") {
		t.Errorf("recursive Fields = %v", paths)
	}
	visits := 0
	if err := x.Walk("Loop", res, func(*Cursor) error { visits++; return nil }); err != nil || visits != 7 {
		t.Errorf("Walk visits %d, err %v", visits, err)
	}
}

func TestFieldsWhere(t *testing.T) {
	x := mustLoad(t, synthetic)
	var got []string
	for _, f := range x.Fields() {
		if strings.HasPrefix(f.Path, "spec.variant.") && len(f.Where) > 0 {
			var conds []string
			for _, d := range f.Where {
				if d.Conditions == nil {
					conds = append(conds, d.Path+":none")
				}
				for _, c := range d.Conditions {
					conds = append(conds, d.Path+":"+c.Member+"="+jsonText(c.Value))
				}
			}
			got = append(got, f.Path+" "+strings.Join(conds, ","))
		}
	}
	want := []string{
		`spec.variant.config spec.variant:type="x"`,
		`spec.variant.config.x spec.variant:type="x"`,
		`spec.variant.config spec.variant:flag=true`,
		`spec.variant.config.f spec.variant:flag=true`,
		`spec.variant.extra spec.variant:flag=true`,
		`spec.variant.extra spec.variant:none`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("dispatch fields =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestFieldsResolveSynthetic(t *testing.T) {
	// Every enumerated field, including else-branch results, resolves by
	// Lookup on an instance built from its Where (as for the committed
	// schema in TestEverySchemaPathResolvable).
	// The static entries below spec.variant (no Where) describe the schema
	// before dispatch, which no instance selects once else branches exist:
	// an instance matching no rule gets the else results.
	x := mustLoad(t, synthetic)
	for _, f := range x.Fields() {
		res, path := instanceFor(f)
		info, ok := x.Lookup(f.Kind, res, path)
		static := len(f.Where) == 0 && strings.HasPrefix(f.Path, "spec.variant")
		if !ok || !info.Known() || (!static && info.Node != f.Node) {
			t.Errorf("Lookup(%s %s %v) = %v %v", f.Kind, f.Path, f.Where, info.Node.Def(), ok)
		}
	}
}

func jsonText(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestNilNodeIsEmpty(t *testing.T) {
	var n *Node
	if n.Def() != "" || n.Scalar() != ScalarNone || n.Types() != 0 || n.Enum() != nil || n.Required() != nil ||
		n.PropertyNames() != nil || n.Closed() || n.Open() || n.Since() != 0 || n.DefaultNode() != nil ||
		n.DispatchValues("type") != nil || n.Select(parseTree(t, `{}`)) != nil || n.SelectValue("a", "b") != nil {
		t.Error("nil Node is not empty")
	}
	if _, ok := n.Property("a"); ok {
		t.Error("nil Property")
	}
	if _, ok := n.Items(); ok {
		t.Error("nil Items")
	}
	if _, ok := n.Values(); ok {
		t.Error("nil Values")
	}
	if _, ok := n.Default(); ok {
		t.Error("nil Default")
	}
	if lt, key := n.List(); lt != ListNone || key != "" {
		t.Error("nil List")
	}
	if k := n.Keywords(); !isZeroKeywords(k) {
		t.Error("nil Keywords")
	}
	if _, ok := n.PolicyConfig("x"); ok {
		t.Error("nil PolicyConfig")
	}
	for range n.Properties() {
		t.Error("nil Properties yields")
	}
	if (Info{}).Known() || !isZeroKeywords((Info{}).Keywords()) {
		t.Error("zero Info")
	}
}

func TestListAndImpactNames(t *testing.T) {
	for lt, want := range map[ListType]string{ListNone: "", ListMap: "map", ListOrderedMap: "orderedMap", ListSet: "set", ListAtomic: "atomic"} {
		if lt.String() != want {
			t.Errorf("ListType(%d) = %q", lt, lt.String())
		}
		if got, ok := parseListType(want); want != "" && (!ok || got != lt) {
			t.Errorf("parseListType(%q) = %v %v", want, got, ok)
		}
	}
	all := ImpactAI | ImpactMetadata | ImpactPlugin | ImpactRouting | ImpactSecurity | ImpactTraffic
	if got := all.String(); got != "ai, metadata, plugin, routing, security, traffic" {
		t.Errorf("Impact.String() = %q", got)
	}
	for _, name := range all.Strings() {
		if i, ok := ParseImpact(name); !ok || !all.Has(i) || impactName(99) != "" {
			t.Errorf("ParseImpact(%q)", name)
		}
	}
	if _, ok := ParseImpact("nope"); ok || Impact(0).Strings() != nil {
		t.Error("ParseImpact(nope)")
	}
}

func TestLoadErrors(t *testing.T) {
	// The vocabulary asserts keyword shapes; constructs outside the
	// supported subset are errors, never silently misread.
	obj := func(s string) string { return `{"properties":{"f":` + s + `}}` }
	kinds := `{"properties":{"kind":{"enum":["A"]}},"allOf":[{"if":{"properties":{"kind":{"const":"A"}},"required":["kind"]},"then":{"$ref":"#/$defs/A"}}],"$defs":{"A":{"properties":{"r":%s}}}}`
	for _, tc := range []struct{ name, doc, want string }{
		{"not JSON", `{`, "unexpected EOF"},
		{"trailing data", `{} {}`, "trailing data"},
		{"array document", `[]`, "must be a JSON object"},
		{"scalar schema", obj(`1`), "object or a boolean"},
		{"remote ref", obj(`{"$ref":"http://x/y"}`), "only local $ref"},
		{"dangling ref", obj(`{"$ref":"#/$defs/Missing"}`), "no member"},
		{"ref into scalar", `{"$defs":{"a":{"type":"string"}},"properties":{"f":{"$ref":"#/$defs/a/type/x"}}}`, "scalar"},
		{"ref bad index", `{"allOf":[{"type":"string"}],"properties":{"f":{"$ref":"#/allOf/5"}}}`, "no element"},
		{"ref percent", obj(`{"$ref":"#/$defs/a%20b"}`), "JSON pointer"},
		{"ref not pointer", obj(`{"$ref":"#abc"}`), "JSON pointer"},
		{"bad type", obj(`{"type":"strng"}`), "unknown type"},
		{"type number", obj(`{"type":5}`), "type must be"},
		{"type list", obj(`{"type":["string",5]}`), "array of strings"},
		{"enum", obj(`{"enum":"a"}`), "enum must be"},
		{"properties", `{"properties":[]}`, "properties must be"},
		{"required", obj(`{"required":[1]}`), "required must be"},
		{"tuple items", obj(`{"items":[{}]}`), "array-form items"},
		{"empty allOf", obj(`{"allOf":[]}`), "non-empty array"},
		{"defs", `{"$defs":[]}`, "$defs must be"},
		{"if keyword", obj(`{"allOf":[{"if":{"type":"object"},"then":{}}]}`), "if supports only"},
		{"if not object", obj(`{"if":true}`), "if must be"},
		{"if properties", obj(`{"if":{"properties":1}}`), "if properties must be"},
		{"if non-const", obj(`{"if":{"properties":{"a":{"enum":["x"]}}}}`), "const"},
		{"if object const", obj(`{"if":{"properties":{"a":{"const":{}}}}}`), "const"},
		{"patternProperties", obj(`{"patternProperties":{}}`), "not supported"},
		{"prefixItems", obj(`{"prefixItems":[]}`), "not supported"},
		{"anchor", obj(`{"$anchor":"a"}`), "not supported"},
		{"nested id", obj(`{"$id":"x"}`), "only at the document root"},
		{"anyOf properties", obj(`{"anyOf":[{"properties":{"a":{}}},{"type":"string"}]}`), "branches may not"},
		{"oneOf via ref", `{"$defs":{"a":{"x-ruralz-secret":true}},"properties":{"f":{"oneOf":[{"$ref":"#/$defs/a"}]}}}`, "branches may not"},
		{"list shape", obj(`{"x-ruralz-list":"set"}`), "must be an object"},
		{"list member", obj(`{"x-ruralz-list":{"type":"set","sorted":true}}`), "unknown member"},
		{"list type", obj(`{"x-ruralz-list":{"type":"bag"}}`), "not map, orderedMap"},
		{"list no key", obj(`{"x-ruralz-list":{"type":"map"}}`), "needs a key"},
		{"list key on set", obj(`{"x-ruralz-list":{"type":"set","key":"id"}}`), "takes no key"},
		{"ref empty", obj(`{"x-ruralz-ref":""}`), "kind name"},
		{"ref not kind", strings.Replace(kinds, "%s", `{"x-ruralz-ref":"B"}`, 1), "not a resource kind"},
		{"secret", obj(`{"x-ruralz-secret":"yes"}`), "must be a boolean"},
		{"cel shape", obj(`{"x-ruralz-cel":{"variables":["a"]}}`), "x-ruralz-cel must be"},
		{"cel vars", obj(`{"x-ruralz-cel":{"variables":[],"result":"bool"}}`), "must not be empty"},
		{"cel var type", obj(`{"x-ruralz-cel":{"variables":[1],"result":"bool"}}`), "array of strings"},
		{"cel result", obj(`{"x-ruralz-cel":{"variables":["a"],"result":"int"}}`), "not bool, string or dyn"},
		{"impact shape", obj(`{"x-ruralz-impact":"security"}`), "array of strings"},
		{"impact empty", obj(`{"x-ruralz-impact":[]}`), "must not be empty"},
		{"impact class", obj(`{"x-ruralz-impact":["speed"]}`), "unknown impact class"},
		{"since negative", obj(`{"x-ruralz-since":-1}`), "non-negative integer"},
		{"since fraction", obj(`{"x-ruralz-since":1.5}`), "non-negative integer"},
		{"since string", obj(`{"x-ruralz-since":"1"}`), "non-negative integer"},
		{"validations shape", obj(`{"x-ruralz-validations":{}}`), "must be an array"},
		{"validation entry", obj(`{"x-ruralz-validations":[1]}`), "a validation is"},
		{"validation rule", obj(`{"x-ruralz-validations":[{"message":"m"}]}`), "a validation is"},
		{"validation member", obj(`{"x-ruralz-validations":[{"rule":"r","level":1}]}`), "a validation is"},
		{"validation message", obj(`{"x-ruralz-validations":[{"rule":"r","message":1}]}`), "a validation is"},
		{"unknown extension", obj(`{"x-ruralz-open":true}`), "unknown keyword x-ruralz-open"},
		{"ref to non-schema", `{"properties":{"f":{"type":"string"},"g":{"$ref":"#/properties/f/type"}}}`, "object or a boolean"},
		{"defs member", `{"$defs":{"a":1}}`, "object or a boolean"},
		{"validation unknown string", obj(`{"x-ruralz-validations":[{"rule":"r","level":"high"}]}`), "a validation is"},
		{"if required", obj(`{"if":{"required":[1]}}`), "required must be"},
		{"apiVersion", `{"properties":{"apiVersion":{"const":"other/v1"}}}`, "not \"test/v1\""},
	} {
		_, err := Load("test/v1", []byte(tc.doc))
		if err == nil || !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Load error = %v, want ErrSchema containing %q", tc.name, err, tc.want)
		}
	}
}

func TestLoadBoundsDispatchCombinations(t *testing.T) {
	// k rules whose branches reference themselves under a member the root
	// also declares accumulate every subset of branches below that member:
	// 2^k merged positions. Load stops at its bound with ErrSchema (a small
	// bound keeps the test fast; Load uses maxNodes).
	var rules []string
	for i := range 14 {
		m := "m" + strings.Repeat("x", i)
		rules = append(rules, `{"if":{"properties":{"`+m+`":{"const":1}},"required":["`+m+`"]},`+
			`"then":{"properties":{"x":{"$ref":"#/allOf/`+strconv.Itoa(i)+`/then"}}}}`)
	}
	doc := `{"properties":{"x":{"$ref":"#"}},"allOf":[` + strings.Join(rules, ",") + `]}`
	if _, err := load("test/v1", []byte(doc), 2000); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "resolved schema positions") {
		t.Fatalf("load = %v, want the node bound", err)
	}
	if x := v1(t); len(x.memo) > maxNodes/50 {
		t.Errorf("the committed schema needs %d positions; maxNodes leaves too little headroom", len(x.memo))
	}
}

func TestDispatchScalarConsts(t *testing.T) {
	// Dispatch consts of every scalar type; numbers compare by value.
	x := mustLoad(t, `{"properties":{"o":{"type":"object","properties":{"n":{},"b":{},"z":{}},"allOf":[
	  {"if":{"properties":{"n":{"const":2}},"required":["n"]},"then":{"properties":{"a":{"const":1.5}}}},
	  {"if":{"properties":{"b":{"const":false}},"required":["b"]},"then":{"properties":{"c":{"enum":[{"x":1},[1],null,true,"s",2]}}}},
	  {"if":{"properties":{"z":{"const":null}},"required":["z"]},"then":{"properties":{"d":{"const":"k"}}}}]}}}`)
	o, _ := x.Root().Property("o")
	for _, tc := range []struct {
		inst, member string
		types        TypeSet
	}{
		{`{"n":2.0}`, "a", TypeNumber | TypeInteger},
		{`{"n":2}`, "a", TypeNumber | TypeInteger},
		{`{"b":false}`, "c", TypeObject | TypeArray | TypeNull | TypeBoolean | TypeString | TypeInteger},
		{`{"z":null}`, "d", TypeString},
	} {
		p, ok := o.Select(parseTree(t, tc.inst)).Property(tc.member)
		if !ok || p.Types() != tc.types {
			t.Errorf("Select(%s).%s: %v types %s, want %s", tc.inst, tc.member, ok, p.Types(), tc.types)
		}
	}
	for _, inst := range []string{`{"n":"2"}`, `{"n":3}`, `{"b":true}`, `{"b":0}`, `{"z":0}`, `{"n":[2]}`} {
		if sel := o.Select(parseTree(t, inst)); sel != o {
			t.Errorf("Select(%s) matched a rule", inst)
		}
	}
	// Enums of two conjunctive parts intersect by JSON value; the types
	// intersect per part (a superset of the values' types).
	y := mustLoad(t, `{"allOf":[{"enum":[{"a":1},[1,2],"x",true]},{"enum":[{"a":1.0},[1,2],"y",true,null]}]}`)
	if got := y.Root().Enum(); len(got) != 0 || y.Root().Types() != TypeObject|TypeArray|TypeBoolean|TypeString {
		t.Errorf("intersected enum %v types %s", got, y.Root().Types())
	}
	if !jsonEqual(map[string]any{"a": []any{json.Number("1")}}, map[string]any{"a": []any{json.Number("1.0")}}) ||
		jsonEqual(json.Number("1"), "1") || jsonEqual(struct{}{}, struct{}{}) || jsonEqual(true, "true") || jsonEqual(nil, false) {
		t.Error("jsonEqual")
	}
	if numberEqual("1", "x") {
		t.Error("numberEqual(1, x)")
	}
}

func TestKindDispatchEdgeCases(t *testing.T) {
	// A rule without then dispatches nothing; a second rule for one kind
	// adds no second resource.
	x := mustLoad(t, `{"properties":{"kind":{"enum":["A","B"]}},"allOf":[
	  {"if":{"properties":{"kind":{"const":"A"}},"required":["kind"]},"then":{"$ref":"#/$defs/A"}},
	  {"if":{"properties":{"kind":{"const":"A"}},"required":["kind"]},"then":{"properties":{"extra":{}}}},
	  {"if":{"properties":{"kind":{"const":"B"}},"required":["kind"]}}],
	  "$defs":{"A":{"properties":{"spec":{"type":"object"}}}}}`)
	if got := x.Kinds(); !slices.Equal(got, []string{"A"}) {
		t.Fatalf("Kinds() = %v", got)
	}
	a, _ := x.Resource("A")
	if !slices.Equal(a.PropertyNames(), []string{"extra", "kind", "spec"}) {
		t.Errorf("Resource(A) properties %v", a.PropertyNames())
	}
}

func TestLoadAccepts(t *testing.T) {
	for _, doc := range []string{
		`{}`,
		`{"properties":{"a":true,"b":false},"additionalProperties":true}`,
		`{"allOf":[{"type":"string"}],"properties":{"f":{"$ref":"#/allOf/0"}}}`,
		`{"$defs":{"a~b":{"type":"string"},"c/d":{"type":"integer"}},"properties":{"f":{"$ref":"#/$defs/a~0b"},"g":{"$ref":"#/$defs/c~1d"},"h":{"$ref":"#"}}}`,
		`{"then":{"type":"string"},"not":{"type":"null"},"description":"ignored","minimum":1,"x-other":1}`,
		// Navigation-neutral applicators constrain keys or require an
		// element; they change no member's or element's schema.
		`{"type":"object","propertyNames":{"pattern":"^[a-z]+$"},"dependentRequired":{"a":["b"]},` +
			`"properties":{"l":{"type":"array","items":{"type":"string"},"contains":{"const":"x"},"minContains":1}}}`,
	} {
		x, err := Load("test/v1", []byte(doc))
		if err != nil {
			t.Errorf("Load(%s): %v", doc, err)
			continue
		}
		if x.Root() == nil || len(x.Kinds()) != 0 {
			t.Errorf("Load(%s): root %v kinds %v", doc, x.Root(), x.Kinds())
		}
	}
	x := mustLoad(t, `{"$defs":{"a~b":{"type":"string"},"c/d":{"type":"integer"}},"properties":{"f":{"$ref":"#/$defs/a~0b"},"g":{"$ref":"#/$defs/c~1d"},"h":{"$ref":"#"}}}`)
	f, _ := x.Root().Property("f")
	g, _ := x.Root().Property("g")
	h, _ := x.Root().Property("h")
	if f.Def() != "a~b" || g.Def() != "c/d" || h.Types() != AllTypes {
		t.Errorf("escaped refs: %q %q %s", f.Def(), g.Def(), h.Types())
	}
	if d, ok := x.Def("c/d"); !ok || d.Types() != TypeInteger {
		t.Errorf("Def(c/d) = %v %v", d, ok)
	}
	if _, ok := x.Def("missing"); ok {
		t.Error("Def(missing) found")
	}
}

func TestSets(t *testing.T) {
	a := mustLoad(t, `{}`)
	if _, err := NewSet(a, a); err == nil {
		t.Error("NewSet with a duplicate apiVersion succeeded")
	}
	schemaFile := func(v string) *fstest.MapFile {
		return &fstest.MapFile{Data: []byte(`{"properties":{"apiVersion":{"const":"ruralz/` + v + `"}}}`)}
	}
	s, err := loadFS(fstest.MapFS{
		"ruralz/v2/rendered.schema.json":  schemaFile("v2"),
		"ruralz/v1/rendered.schema.json":  schemaFile("v1"),
		"ruralz/v1/authoring.schema.json": {Data: []byte(`not read`)},
	})
	if err != nil || !slices.Equal(s.Served(), []string{"ruralz/v1", "ruralz/v2"}) {
		t.Fatalf("loadFS = %v, %v", s, err)
	}
	if _, err := loadFS(fstest.MapFS{}); !errors.Is(err, ErrSchema) {
		t.Errorf("empty FS: %v", err)
	}
	if _, err := loadFS(fstest.MapFS{"ruralz/v3/rendered.schema.json": schemaFile("v1")}); !errors.Is(err, ErrSchema) {
		t.Errorf("mismatched apiVersion: %v", err)
	}
	if _, err := loadFS(fstest.MapFS{"ruralz/v3/rendered.schema.json": &fstest.MapFile{Mode: 0o20000000000}}); err == nil {
		t.Error("unreadable file loaded")
	}
}
