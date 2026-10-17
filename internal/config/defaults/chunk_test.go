// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// wholeDecode decodes res as one encoding/json call per tree, the oracle
// the chunker must agree with: the kind struct from the whole envelope
// and, for a registered Policy type, the config from its declared
// members.
func wholeDecode(t testing.TB, idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource) (obj, cfg any, failed bool) {
	t.Helper()
	obj, ok := newObject(res.ID.Kind)
	if !ok {
		return nil, nil, true
	}
	data, err := appendJSON(nil, res.Root, nil, nil)
	if err == nil {
		err = (*work)(nil).decodeInto(data, obj)
	}
	if err != nil {
		return nil, nil, true
	}
	p, isPolicy := obj.(*v1alpha1.Policy)
	if !isPolicy {
		return obj, nil, false
	}
	cfg, err = reg.NewConfig(p.Spec.Type)
	if err != nil || cfg == nil {
		return obj, nil, false
	}
	spec, _ := res.Root.Get("spec")
	cfgNode, ok := spec.Get("config")
	if !ok {
		return obj, cfg, false
	}
	specSchema, _ := idx.Spec(string(v1alpha1.KindPolicy))
	data, err = appendJSON(nil, cfgNode, &declaredOnly{memberSchema(specSchema.Select(spec), "config")}, nil)
	if err == nil {
		err = (*work)(nil).decodeInto(data, cfg)
	}
	if err != nil {
		return nil, nil, true
	}
	return obj, cfg, false
}

// sameDecode checks that decoding res in pieces of at most limit nodes
// gives what one encoding/json call gives: the same failure, or equal
// kind structs and configs.
func sameDecode(t testing.TB, idx *schemaidx.Index, reg *registry.Registry, res *tree.Resource, limit int) {
	t.Helper()
	wantObj, wantCfg, wantFailed := wholeDecode(t, idx, reg, res)
	w := newWork()
	w.chunk = limit
	obj, cfg, _, failed := decode(idx, reg, res, nil, w, nil)
	if failed != wantFailed {
		t.Fatalf("limit %d: %s/%s failed = %v, whole decode failed = %v: %s", limit, res.ID.Kind, res.ID.Name, failed, wantFailed, dump(res.Root))
	}
	if failed {
		return
	}
	if !reflect.DeepEqual(obj, wantObj) || !reflect.DeepEqual(cfg, wantCfg) {
		t.Fatalf("limit %d: %s/%s decodes differently in pieces\n%#v\n%#v\nwant\n%#v\n%#v", limit, res.ID.Kind, res.ID.Name, obj, cfg, wantObj, wantCfg)
	}
}

// chunkLimits are the piece sizes the equivalence tests use: every value
// split, small pieces, and the production size.
func chunkLimits() []int { return []int{1, 2, 3, 7, 64, decodeChunk} }

// TestChunkedDecodeEqualsWhole covers the chunker (01 req 38 and 53):
// decoding in pieces gives the kind structs and configs one encoding/json
// call gives, for the example Bundle after stage G, the large shapes of
// the scale tests (a plugin config and a Plugin configSchema, both
// any-valued, included), the decode test inputs, and their failures.
func TestChunkedDecodeEqualsWhole(t *testing.T) {
	s := newStage(t)
	idx := hubIndex(t)
	reg := registry.New()
	f, rs := loadBundle(t)
	var hubs []*tree.Resource
	for _, r := range rs {
		h, ds := s.Resource(r, f.files)
		if h == nil {
			t.Fatalf("Resource: %q", texts(ds))
		}
		hubs = append(hubs, &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree})
	}
	for name, build := range scaleCases() {
		h, ds := s.Resource(envelope(mustParse(t, build(600))), nil)
		if h == nil {
			t.Fatalf("%s: %q", name, texts(ds))
		}
		hubs = append(hubs, &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree})
	}
	for _, src := range []string{
		route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`),
		policyDoc("ratelimit", `{"limits": [{"requests": 1.5, "window": "1s"}]}`),
		policyDoc("ratelimit", `{"limits": [{"requests": 1, "window": "1x"}]}`),
		route(`{"match": {}, "timeout": 5}`),
		gateway(`{"listeners": [{"name": "h", "proxyProtocol": "yes"}]}`),
		policyDoc("auth.api-key", `{"header": "x-a", "Header": "x-b", "HEADER": "x-c", "extra": {"Header": 1}}`),
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": {"n": 12345678901234567, "f": 0.1, "l": [1, {"a": [2]}], "o": {"Z": true}}}}`,
		policyDoc("validation.json-schema", `{"schema": {"type": "object", "properties": {"a": {"enum": [1, 2, 3]}}}}`),
		// Any-valued positions (01 req 53): a plugin config and a Plugin
		// configSchema with objects and lists at several depths.
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": {"x": [[1, [2, [3, {"y": [4, 5, 6]}]]], {}, [], {"z": {"w": [true, "s", 1.5]}}], "e": {}, "l": []}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"configSchema": {"type": "object", "properties": {"a": {"enum": [1, "x", [2, {"k": [3]}]]}, "b": {"items": {"type": "string"}}}, "required": ["a"]}}}`,
		route(`{"Spec": 1, "match": {"hosts": ["a", "b"]}, "TIMEOUT": "1s", "timeout": "2s"}`),
		// A large member after one that folds to the same field (WP-37
		// review round 3): encoding/json decodes the second into the
		// slice the first filled, keeping weight 5 in element 0, at the
		// top of the spec, through a struct member of the envelope, with
		// a Unicode fold (U+017F folds to s), and in the other order; in
		// a config, only declared members count.
		route(`{"UPSTREAMS": [{"name": "a", "weight": 5}], "match": {}, "upstreams": [{"name": "x"}, {"name": "y"}]}`),
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "SPEC": {"upstreams": [{"name": "a", "weight": 5}]},
		  "spec": {"match": {}, "upstreams": [{"name": "x"}, {"name": "y"}]}}`,
		route("{\"up\u017ftreams\": [{\"name\": \"a\", \"weight\": 5}], \"match\": {}, \"upstreams\": [{\"name\": \"x\"}, {\"name\": \"y\"}]}"),
		route(`{"match": {}, "upstreams": [{"name": "x"}, {"name": "y"}], "UPSTREAMS": [{"name": "a", "weight": 5}]}`),
		policyDoc("ratelimit", `{"LIMITS": [{"requests": 5, "window": "1s", "burst": 3}], "limits": [{"requests": 1, "window": "1s"}, {"requests": 2, "window": "2s"}]}`),
	} {
		res := envelope(mustParse(t, src))
		hubs = append(hubs, res)
	}
	for _, res := range hubs {
		for _, limit := range chunkLimits() {
			sameDecode(t, idx, reg, res, limit)
		}
	}
}

// TestChunkedDecodeSplits checks that a large resource is decoded in many
// small encoding/json calls, so the worker reads its yield timer between
// them (01 req 53), and that a small one takes one call: no call decodes
// more than decodeChunk nodes, for every large shape of the scale tests,
// typed and any-valued (a plugin config, a Plugin configSchema), and the
// calls number at least the nodes decoded over decodeChunk.
func TestChunkedDecodeSplits(t *testing.T) {
	s := newStage(t)
	idx := hubIndex(t)
	reg := registry.New()
	// The large value of each shape, which encoding/json calls decode;
	// "" for one they never see: the undeclared members of an open
	// config are not decoded into its config type, and the
	// JSONSchemaDocument goes to its own UnmarshalJSON.
	large := map[string]string{
		"set of strings": "spec.tags", "map list": "spec.endpoints", "host set": "spec.match.hosts",
		"labels": "metadata.labels", "open config members": "", "json schema document": "",
		"atomic list of objects": "spec.config.limits", "plugin config": "spec.config.x",
		"plugin config schema": "spec.configSchema.properties",
	}
	const n = 3000
	for name, build := range scaleCases() {
		t.Run(name, func(t *testing.T) {
			path, ok := large[name]
			if !ok {
				t.Fatalf("no large value named for scale case %q", name)
			}
			h, ds := s.Resource(envelope(mustParse(t, build(n))), nil)
			if h == nil {
				t.Fatalf("Resource: %q", texts(ds))
			}
			nodes := 0
			if path != "" {
				nodes = size(at(t, h.Tree, path), math.MaxInt)
			}
			w := newWork()
			if _, _, _, failed := decode(idx, reg, &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree}, nil, w, nil); failed {
				t.Fatal("decode failed")
			}
			if w.widest > decodeChunk {
				t.Errorf("one encoding/json call decoded more than %d nodes", decodeChunk)
			}
			if want := nodes / decodeChunk; w.decodes < want {
				t.Errorf("%d encoding/json calls for %d nodes at %s, want at least %d", w.decodes, nodes, path, want)
			}
		})
	}
	w := newWork()
	if _, _, _, failed := decode(idx, nil, envelope(mustParse(t, route(`{"match": {}}`))), nil, w, nil); failed || w.decodes != 1 {
		t.Errorf("small resource: %d encoding/json calls, want 1", w.decodes)
	}
}

// TestSplittable covers the targets the chunker splits: a nil empty
// interface for an object or a list, never one holding a value, one with
// methods, or a scalar or null value, and the containers encoding/json
// fills piece by piece.
func TestSplittable(t *testing.T) {
	obj, list, str, null := mustParse(t, `{"a": 1}`), mustParse(t, `[1]`), mustParse(t, `"s"`), mustParse(t, `null`)
	held := reflect.New(reflect.TypeFor[any]()).Elem()
	held.Set(reflect.ValueOf(map[string]any{}))
	zero := func(t reflect.Type) reflect.Value { return reflect.New(t).Elem() }
	tests := []struct {
		name string
		v    reflect.Value
		n    *tree.Node
		want bool
	}{
		{"nil any, object", zero(reflect.TypeFor[any]()), obj, true},
		{"nil any, list", zero(reflect.TypeFor[any]()), list, true},
		{"nil any, string", zero(reflect.TypeFor[any]()), str, false},
		{"nil any, null", zero(reflect.TypeFor[any]()), null, false},
		{"any holding a map", held, obj, false},
		{"interface with methods", zero(reflect.TypeFor[fmt.Stringer]()), obj, false},
		{"pointer", zero(reflect.TypeFor[*struct{ A int }]()), obj, true},
		{"struct, object", zero(reflect.TypeFor[struct{ A int }]()), obj, true},
		{"struct, list", zero(reflect.TypeFor[struct{ A int }]()), list, false},
		{"string map", zero(reflect.TypeFor[map[string]int]()), obj, true},
		{"int map", zero(reflect.TypeFor[map[int]int]()), obj, false},
		{"slice", zero(reflect.TypeFor[[]int]()), list, true},
		{"bytes", zero(reflect.TypeFor[[]byte]()), list, false},
		{"array", zero(reflect.TypeFor[[2]int]()), list, false},
		{"text type", zero(reflect.TypeFor[big.Int]()), obj, false},
		{"duration", zero(reflect.TypeFor[v1alpha1.Duration]()), str, false},
	}
	for _, tc := range tests {
		if got := splittable(tc.v, tc.n); got != tc.want {
			t.Errorf("splittable(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestDecodedTypes lists every Go type stage G decodes into, from the
// kind structs and the registry's config types, so a new kind of type in
// pkg/config/v1alpha1 gets reviewed against the chunker (01 req 53):
// every container is one the chunker splits, the only interface is the
// empty one, the only embedded struct is TypeMeta with its two strings,
// and the types with their own UnmarshalJSON are the scalars, which take
// only short strings or numbers, and the two whose one call is linear in
// the value (json.RawMessage, JSONSchemaDocument).
func TestDecodedTypes(t *testing.T) {
	unmarshalers := map[reflect.Type]bool{
		reflect.TypeFor[v1alpha1.Duration](): true, reflect.TypeFor[v1alpha1.ByteSize](): true,
		reflect.TypeFor[v1alpha1.IntOrString](): true, reflect.TypeFor[json.RawMessage](): true,
		reflect.TypeFor[v1alpha1.JSONSchemaDocument](): true,
	}
	seen := map[reflect.Type]bool{}
	found := map[reflect.Type]bool{}
	var walk func(rt reflect.Type, where string)
	walk = func(rt reflect.Type, where string) {
		if seen[rt] {
			return
		}
		seen[rt] = true
		if reflect.PointerTo(rt).Implements(reflect.TypeFor[json.Unmarshaler]()) {
			if !unmarshalers[rt] {
				t.Errorf("%s: %s has its own UnmarshalJSON, which the chunker cannot split", where, rt)
			}
			found[rt] = true
			return
		}
		if textType(rt) {
			t.Errorf("%s: %s decodes through UnmarshalText, which the chunker cannot split", where, rt)
			return
		}
		switch rt.Kind() {
		case reflect.Pointer, reflect.Slice:
			if rt.Kind() == reflect.Slice && rt.Elem().Kind() == reflect.Uint8 {
				t.Errorf("%s: %s is a []byte, which the chunker cannot split", where, rt)
			}
			walk(rt.Elem(), where+"[]")
		case reflect.Map:
			if rt.Key().Kind() != reflect.String || textType(rt.Key()) {
				t.Errorf("%s: %s has keys the chunker cannot split", where, rt)
			}
			walk(rt.Elem(), where+"{}")
		case reflect.Interface:
			if rt.NumMethod() != 0 {
				t.Errorf("%s: %s is an interface with methods", where, rt)
			}
			found[rt] = true
		case reflect.Struct:
			for i := range rt.NumField() {
				f := rt.Field(i)
				if f.Anonymous && f.Type != reflect.TypeFor[v1alpha1.TypeMeta]() {
					t.Errorf("%s: %s embeds %s, whose large members the chunker cannot split", where, rt, f.Type)
				}
				if strings.Contains(f.Tag.Get("json"), ",string") {
					t.Errorf("%s: %s.%s is a \",string\" field", where, rt, f.Name)
				}
				walk(f.Type, where+"."+f.Name)
			}
		case reflect.Bool, reflect.String, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
			reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		default:
			t.Errorf("%s: %s is a %s, which the chunker does not split", where, rt, rt.Kind())
		}
	}
	for _, k := range []v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindRoute, v1alpha1.KindUpstream, v1alpha1.KindPolicy, v1alpha1.KindPlugin,
		v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel, v1alpha1.KindEnvironment, v1alpha1.KindCluster,
	} {
		obj, ok := newObject(k)
		if !ok {
			t.Fatalf("no hub type for %s", k)
		}
		walk(reflect.TypeOf(obj).Elem(), string(k))
	}
	reg := registry.New()
	for _, typ := range reg.Types() {
		cfg, err := reg.NewConfig(typ)
		if err != nil || cfg == nil {
			continue
		}
		walk(reflect.TypeOf(cfg).Elem(), string(typ))
	}
	for rt := range unmarshalers {
		if !found[rt] {
			t.Errorf("%s is no longer decoded: update the chunker's documentation", rt)
		}
	}
	if !found[reflect.TypeFor[any]()] {
		t.Error("no any-valued position is decoded: update the chunker's documentation")
	}
	for i := range reflect.TypeFor[v1alpha1.TypeMeta]().NumField() {
		if f := reflect.TypeFor[v1alpha1.TypeMeta]().Field(i); f.Type.Kind() != reflect.String {
			t.Errorf("TypeMeta.%s is a %s, not a string", f.Name, f.Type)
		}
	}
}

type chunkEmbedded struct {
	Inner string `json:"inner"`
}

type chunkFixture struct {
	chunkEmbedded
	Plain  []int
	Tagged []int `json:"tagged"`
	// Two fields named B: encoding/json picks the tagged one.
	A       []int `json:"B"` //nolint:tagliatelle // A JSON name equal to another field's Go name is the case under test.
	B       []int
	Quoted  int   `json:"quoted,string"`
	Skipped []int `json:"-"`
	Dash    []int `json:"-,"` //nolint:staticcheck // SA5008: the JSON name "-" is the case under test.
	private []int
}

// TestMemberTarget covers the fields a large member value is decoded into
// directly: an exact JSON name of a field of the struct itself, never a
// promoted, ambiguous (B), ",string", skipped or unexported field, and a
// new element of a map.
func TestMemberTarget(t *testing.T) {
	v := reflect.New(reflect.TypeFor[chunkFixture]()).Elem()
	for name, want := range map[string]string{
		"Plain": "Plain", "tagged": "Tagged", "-": "Dash",
		"plain": "", "inner": "", "B": "", "quoted": "", "Skipped": "", "private": "", "missing": "",
	} {
		into, ok := memberTarget(v, name)
		got := ""
		if ok {
			for i := range v.NumField() {
				if v.Field(i).Addr().Pointer() == into.Addr().Pointer() && v.Field(i).Type() == into.Type() {
					got = v.Type().Field(i).Name
				}
			}
		}
		if got != want {
			t.Errorf("memberTarget(%q) = %q, want %q", name, got, want)
		}
	}
	m := reflect.New(reflect.TypeFor[map[string][]int]()).Elem()
	if into, ok := memberTarget(m, "k"); !ok || into.Type() != reflect.TypeFor[[]int]() || !into.CanSet() {
		t.Errorf("map member target = %v, %v", into, ok)
	}
	// A large value under each kind of field decodes as one call would.
	src := `{"Plain": [` + ints(40) + `], "tagged": [` + ints(40) + `], "B": [` + ints(40) + `], "quoted": "7", "inner": "x", "-": [` + ints(40) + `], "Skipped": [1]}`
	n := mustParse(t, src)
	var want, got chunkFixture
	if err := (*work)(nil).decodeInto([]byte(dump(n)), &want); err != nil {
		t.Fatal(err)
	}
	c := &chunker{w: newWork(), limit: 4, bytes: decodeBytes}
	if err := c.decode(n, reflect.ValueOf(&got).Elem(), nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) || got.private != nil {
		t.Errorf("chunked = %+v\nwant      %+v", got, want)
	}
}

// ints returns "1, 2, ..., n".
func ints(n int) string {
	out := ""
	for i := range n {
		if i > 0 {
			out += ", "
		}
		out += strconv.Itoa(i + 1)
	}
	return out
}

// TestChunkedDecodeShapes covers the containers the chunker splits and
// those it decodes whole: a pointer, a map of slices, a slice of
// pointers, an empty interface holding objects and lists at any depth
// (01 req 53), an interface with methods, a []byte, a type with its own
// UnmarshalJSON, a null and a JSON type mismatch, each equal to one
// encoding/json call.
func TestChunkedDecodeShapes(t *testing.T) {
	type shapes struct {
		Ptr    *struct{ L []int }              `json:"ptr"`
		Map    map[string][]int                `json:"map"`
		Ptrs   []*struct{ A int }              `json:"ptrs"`
		Any    any                             `json:"any"`
		Anys   []any                           `json:"anys"`
		AnyMap map[string]any                  `json:"anyMap"`
		Iface  fmt.Stringer                    `json:"iface"`
		Bytes  []byte                          `json:"bytes"`
		Raw    json.RawMessage                 `json:"raw"`
		Dur    *v1alpha1.Duration              `json:"dur"`
		Nested map[string]struct{ L []string } `json:"nested"`
		Null   []int                           `json:"null"`
	}
	for _, src := range []string{
		`{"ptr": {"L": [` + ints(30) + `]}, "map": {"a": [` + ints(30) + `], "b": [1]}, "ptrs": [{"A": 1}, {"A": 2}, {"A": 3}, {"A": 4}, {"A": 5}],
		  "any": {"x": [` + ints(30) + `]}, "bytes": [` + ints(30) + `], "raw": {"r": [` + ints(30) + `]}, "dur": "1s",
		  "nested": {"k": {"L": ["a", "b", "c", "d", "e", "f"]}}, "null": null}`,
		`{"ptr": "nope"}`,
		// Interface targets (01 req 53): objects and lists at any depth,
		// empty ones, leaves of each JSON type and nulls.
		`{"any": [` + ints(30) + `]}`,
		`{"any": {}, "anys": [], "anyMap": {}}`,
		`{"any": [[1, 2.5e3, "s"], [true, false, [{"x": [` + ints(20) + `]}, {}]], {"y": {"z": [[], [-0.0]]}}]}`,
		`{"anys": [{"a": [` + ints(20) + `]}, [` + ints(20) + `], "s", 1, true, null, {}]}`,
		`{"anyMap": {"a": [` + ints(20) + `], "b": {"c": {"d": [` + ints(20) + `]}}, "e": null, "f": "s"}}`,
		`{"any": "s"}`,
		`{"any": 12345678901234567890}`,
		`{"any": true}`,
		`{"any": null}`,
		`{"iface": {"a": [` + ints(30) + `]}}`,
		`{"any": [1, 2, 3, 4, 5], "anys": [{"k": 1}, {"k": 2}, {"k": 3}]}`,
		`{"map": {"a": "nope", "b": [` + ints(30) + `]}}`,
		`{"ptrs": [{"A": 1}, {"A": "x"}, {"A": 3}, {"A": 4}, {"A": 5}, {"A": 6}]}`,
		`{"dur": "1x"}`,
		`{"nested": [1, 2, 3, 4, 5]}`,
	} {
		// The oracle decodes the compact text the chunker writes, so a
		// json.RawMessage holds the same bytes.
		var want shapes
		wantErr := (*work)(nil).decodeInto([]byte(dump(mustParse(t, src))), &want)
		for _, limit := range chunkLimits() {
			var got shapes
			c := &chunker{w: newWork(), limit: limit, bytes: decodeBytes}
			err := c.decode(mustParse(t, src), reflect.ValueOf(&got).Elem(), nil)
			if (err != nil) != (wantErr != nil) {
				t.Fatalf("limit %d: %.40s: error %v, whole %v", limit, src, err, wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Errorf("limit %d: %.40s: chunked %+v\nwant %+v", limit, src, got, want)
			}
		}
	}
}

// FuzzChunkedDecode checks the chunker against one encoding/json call on
// arbitrary resource documents, before and after stage G: the same
// failure or the same kind struct and config, for several piece sizes.
func FuzzChunkedDecode(f *testing.F) {
	for _, seed := range []string{
		route(`{"match": {"hosts": ["a", "b"]}, "timeout": "90s", "upstreams": [{"name": "b", "weight": 2}, {"name": "a"}]}`),
		route(`{"Timeout": "1s", "timeout": "2s", "upstreams": [{"name": "u", "weight": "x"}]}`),
		gateway(`{"listeners": [{"name": "h", "port": 80, "protocol": "http"}], "limits": {"maxRequestBodyBytes": 1048576}}`),
		policyDoc("ratelimit", `{"limits": [{"requests": 1, "window": "1s"}, {"requests": 2, "window": "2s"}], "Limits": []}`),
		policyDoc("auth.api-key", `{"header": "x-a", "Header": "x-b", "extra": {"Header": 1}}`),
		policyDoc("plugin", `{"a": [1, null, {"b": 2}]}`),
		policyDoc("plugin", `{"x": [[1, [2, [3, {"y": [4, 5, 6]}]]], {}, [], {"z": {"w": [true, "s", 1.5]}}]}`),
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"configSchema": {"type": "object", "properties": {"a": {"enum": [1, "x", [2]]}, "b": {"items": {"type": "string"}}}}}}`,
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c", "labels": {"a": "b"}}, "spec": {"credentials": {}, "tags": ["b", "a"]}}`,
		route(`{"UPSTREAMS": [{"name": "a", "weight": 5}], "match": {}, "upstreams": [{"name": "x"}, {"name": "y"}]}`),
	} {
		f.Add(seed)
	}
	s := newStage(f)
	idx := hubIndex(f)
	reg := registry.New()
	f.Fuzz(func(t *testing.T, src string) {
		root, err := parse(src, 0)
		if err != nil || root.Kind != tree.KindMap {
			t.Skip()
		}
		res := envelope(root)
		if _, ok := newObject(res.ID.Kind); !ok {
			res.ID.Kind = v1alpha1.KindRoute
		}
		for _, limit := range []int{1, 3, 16} {
			sameDecode(t, idx, reg, res, limit)
		}
		res.APIVersion = v1
		if h, _ := s.Resource(res, nil); h != nil {
			for _, limit := range []int{1, 3, 16} {
				sameDecode(t, idx, reg, &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree}, limit)
			}
		}
	})
}

// TestFoldName checks foldName against strings.EqualFold, the matching
// encoding/json applies to member names, on every pair of a corpus with
// ASCII case, multi-member fold orbits (k, K and the Kelvin sign; s, S
// and the long s; the three sigmas), invalid UTF-8, which both read as
// U+FFFD, and different lengths.
func TestFoldName(t *testing.T) {
	names := []string{
		"", "a", "A", "upstreams", "UPSTREAMS", "upſtreams", "UpStReAmS", "upstream", "k", "K", "K", "s", "S", "ſ",
		"σ", "ς", "Σ", "\xff", "\xfe", "�", "ß", "SS", "ẞ", "i", "İ", "ı", "aK", "aK",
	}
	for _, a := range names {
		for _, b := range names {
			if got, want := foldName(a) == foldName(b), strings.EqualFold(a, b); got != want {
				t.Errorf("foldName(%q) == foldName(%q) is %v, strings.EqualFold %v", a, b, got, want)
			}
		}
	}
}

// TestChunkedDecodeFoldSplits covers which members the chunker's fold
// check counts (WP-37 review round 3): in a config, an undeclared member
// that folds to a declared one is never decoded, so the large declared
// member is still decoded in pieces; in the kind struct, a member that
// folds to the large one sends it whole to encoding/json, which merges
// the two as one call over the tree does.
func TestChunkedDecodeFoldSplits(t *testing.T) {
	idx := hubIndex(t)
	limits := make([]string, 600)
	for i := range limits {
		limits[i] = `{"requests": ` + strconv.Itoa(i+1) + `, "window": "1s"}`
	}
	cfg := envelope(mustParse(t, policyDoc("ratelimit", `{"LIMITS": [], "limits": [`+strings.Join(limits, ", ")+`]}`)))
	w := newWork()
	if _, _, _, failed := decode(idx, registry.New(), cfg, nil, w, nil); failed || w.widest > decodeChunk {
		t.Errorf("config: failed %v, widest call %d nodes; want pieces of at most %d", failed, w.widest, decodeChunk)
	}
	ups := make([]string, 600)
	for i := range ups {
		ups[i] = `{"name": "u` + strconv.Itoa(i) + `"}`
	}
	route := envelope(mustParse(t, route(`{"UPSTREAMS": [{"name": "a", "weight": 5}], "match": {}, "upstreams": [`+strings.Join(ups, ", ")+`]}`)))
	w = newWork()
	obj, _, _, failed := decode(idx, nil, route, nil, w, nil)
	if failed || w.widest <= decodeChunk {
		t.Fatalf("kind struct: failed %v, widest call %d nodes; want the folded member decoded whole", failed, w.widest)
	}
	if r := obj.(*v1alpha1.Route); len(r.Spec.Upstreams) != 600 || r.Spec.Upstreams[0].Weight == nil || *r.Spec.Upstreams[0].Weight != 5 {
		t.Errorf("kind struct: upstreams[0] = %+v, want the merge of encoding/json (weight 5)", r.Spec.Upstreams[0])
	}
}

// TestChunkBoundary covers the node bound of a decode piece at its edges
// (01 req 53; WP-37 review round 3: a batch counted its members but not
// the object or array around them, so a split piece could decode limit+1
// nodes, while work.widest recorded limit, and no test told a value of
// exactly limit nodes from a larger one): for each limit, a list and an
// object of exactly limit nodes take one call of limit nodes and one size
// scan; one of limit+1 nodes, and a list whose one element has limit
// nodes, are split into calls of at most limit nodes, counted from the
// JSON text each call gets, as work.widest records them.
func TestChunkBoundary(t *testing.T) {
	list := func(n int) string { return "[" + ints(n) + "]" }
	object := func(n int) string {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `"k` + strconv.Itoa(i) + `": ` + strconv.Itoa(i)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	for _, limit := range []int{2, 3, 4, 7} {
		for _, tc := range []struct {
			name   string
			src    string
			target func() reflect.Value
			calls  int // 0: more than one
		}{
			{"list of limit nodes", list(limit - 1), func() reflect.Value { return reflect.New(reflect.TypeFor[[]int]()).Elem() }, 1},
			{"object of limit nodes", object(limit - 1), func() reflect.Value { return reflect.New(reflect.TypeFor[map[string]int]()).Elem() }, 1},
			{"list of limit+1 nodes", list(limit), func() reflect.Value { return reflect.New(reflect.TypeFor[[]int]()).Elem() }, 0},
			{"object of limit+1 nodes", object(limit), func() reflect.Value { return reflect.New(reflect.TypeFor[map[string]int]()).Elem() }, 0},
			{"element of limit nodes", "[" + list(limit-1) + "]", func() reflect.Value { return reflect.New(reflect.TypeFor[[][]int]()).Elem() }, 1},
			{"member of limit nodes", `{"a": ` + list(limit-1) + `}`, func() reflect.Value { return reflect.New(reflect.TypeFor[map[string][]int]()).Elem() }, 1},
		} {
			t.Run(strconv.Itoa(limit)+"/"+tc.name, func(t *testing.T) {
				n := mustParse(t, tc.src)
				w := newWork()
				widest := 0
				w.decoded = func(text []byte) { widest = max(widest, size(mustParse(t, string(text)), math.MaxInt)) }
				c := &chunker{w: w, limit: limit, bytes: decodeBytes}
				got := tc.target()
				if err := c.decode(n, got, nil); err != nil {
					t.Fatal(err)
				}
				want := tc.target()
				if err := (*work)(nil).decodeInto([]byte(dump(n)), want.Addr().Interface()); err != nil || !reflect.DeepEqual(got.Interface(), want.Interface()) {
					t.Fatalf("chunked %v, one call %v (%v)", got, want, err)
				}
				if widest > limit || w.widest != widest {
					t.Errorf("widest call decoded %d nodes, recorded %d; want at most %d", widest, w.widest, limit)
				}
				if tc.calls == 1 && w.decodes != 1 {
					t.Errorf("%d calls, want 1", w.decodes)
				}
				if tc.calls == 0 && w.decodes < 2 {
					t.Errorf("%d calls, want the value split", w.decodes)
				}
			})
		}
	}
	// A value of exactly limit nodes is decoded whole: one size scan of
	// it, then its text, and no scan of its members.
	n := mustParse(t, `[1, 2, 3]`)
	text := newWork()
	if _, err := appendJSON(nil, n, nil, text); err != nil {
		t.Fatal(err)
	}
	w := newWork()
	var v []int
	if err := (&chunker{w: w, limit: 4, bytes: decodeBytes}).decode(n, reflect.ValueOf(&v).Elem(), nil); err != nil {
		t.Fatal(err)
	}
	if want := 4 + text.total; w.total != want {
		t.Errorf("a value of limit nodes counted %d units, want %d: one size scan and its text", w.total, want)
	}
}

// TestChunkedDecodeErrors covers the error returns of the split decode
// (WP-37 review round 3: no test reached them): a number literal outside
// the grammar, or a string or member name that is not valid UTF-8, which
// only a tree built in code holds, inside a value above the piece size
// fails the chunked decode with appendJSON's error, as one call over the
// tree fails, for each place it can sit: an any-valued object and list, a
// typed list's batch element, a struct's batch member value, a map's
// batch member name, and a Duration decoded on its own (the
// UnmarshalJSON path); and the stage's decode reports it as RZ-CFG-005
// with no object.
func TestChunkedDecodeErrors(t *testing.T) {
	type target struct {
		Any  any                         `json:"any"`
		List []struct{ S string }        `json:"list"`
		Obj  struct{ A, B, C, D string } `json:"obj"`
		Map  map[string]int              `json:"map"`
		Dur  v1alpha1.Duration           `json:"dur"`
	}
	text := func(s string) func(n *tree.Node) { return func(n *tree.Node) { n.Text = s } }
	for _, tc := range []struct {
		name  string
		src   string
		path  string
		edit  func(n *tree.Node)
		limit int
		want  string
	}{
		{"any object", `{"any": {"a": 1, "b": 2, "c": 3, "d": 4, "e": 5, "f": 6}}`, "any.e", text("1x"), 3, "invalid number"},
		{"any list", `{"any": [1, 2, 3, 4, 5, 6, 7, 8]}`, "any[5]", text("1x"), 3, "invalid number"},
		{"typed list element", `{"list": [{"S": "a"}, {"S": "b"}, {"S": "c"}, {"S": "d"}, {"S": "e"}]}`, "list[3].S", text("\xff"), 3, "invalid UTF-8"},
		{"struct member value", `{"obj": {"A": "a", "B": "b", "C": "c", "D": "d"}}`, "obj.C", text("\xff"), 3, "invalid UTF-8"},
		{"map member name", `{"map": {"a": 1, "b": 2, "c": 3, "d": 4, "e": 5}}`, "map", func(n *tree.Node) { n.Members[3].Key = "\xff" }, 3, "invalid UTF-8"},
		{"duration on its own", `{"dur": "1s", "any": [1, 2]}`, "dur", text("1\xffs"), 1, "invalid UTF-8"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := mustParse(t, tc.src)
			tc.edit(at(t, n, tc.path))
			if _, err := appendJSON(nil, n, nil, nil); err == nil {
				t.Fatal("one call over the tree would not fail")
			}
			var got target
			err := (&chunker{w: newWork(), limit: tc.limit, bytes: decodeBytes}).decode(n, reflect.ValueOf(&got).Elem(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("chunked decode error %v, want appendJSON's %q", err, tc.want)
			}
		})
	}
	// Through the stage's decode, of a Plugin whose configSchema (any)
	// is split: RZ-CFG-005 and no object.
	res := envelope(mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"configSchema": {"enum": [1, 2, 3, 4, 5, 6, 7, 8]}}}`))
	at(t, res.Root, "spec.configSchema.enum[6]").Text = "1x"
	w := newWork()
	w.chunk = 3
	if obj, _, ds, failed := decode(hubIndex(t), registry.New(), res, nil, w, nil); obj != nil || !failed || len(ds) != 1 || ds[0].Code != CodeSchema {
		t.Errorf("decode = %v, %v, %q; want one RZ-CFG-005 and no object", obj, failed, texts(ds))
	}
}
