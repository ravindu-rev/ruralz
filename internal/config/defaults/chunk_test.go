// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"reflect"
	"strconv"
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
	w := newWork(nil)
	w.chunk = limit
	obj, cfg, _, failed := decode(idx, reg, res, nil, w)
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
// the scale tests, the decode test inputs, and their failures.
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
		route(`{"Spec": 1, "match": {"hosts": ["a", "b"]}, "TIMEOUT": "1s", "timeout": "2s"}`),
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
// them (01 req 53), and that a small one takes one call.
func TestChunkedDecodeSplits(t *testing.T) {
	s := newStage(t)
	h, ds := s.Resource(envelope(mustParse(t, scaleCases()["map list"](12_000))), nil)
	if h == nil {
		t.Fatalf("Resource: %q", texts(ds))
	}
	res := &tree.Resource{ID: h.ID, APIVersion: v1, Root: h.Tree}
	w := newWork(nil)
	if _, _, _, failed := decode(hubIndex(t), nil, res, nil, w); failed {
		t.Fatal("decode failed")
	}
	// 12,000 endpoints of three nodes each: at least 36,000/256 pieces.
	if w.decodes < 36_000/decodeChunk {
		t.Errorf("%d encoding/json calls for 12,000 endpoints, want at least %d", w.decodes, 36_000/decodeChunk)
	}
	w = newWork(nil)
	if _, _, _, failed := decode(hubIndex(t), nil, envelope(mustParse(t, route(`{"match": {}}`))), nil, w); failed || w.decodes != 1 {
		t.Errorf("small resource: %d encoding/json calls, want 1", w.decodes)
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
	c := &chunker{w: newWork(nil), limit: 4}
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
// pointers, an interface, a []byte, a type with its own UnmarshalJSON, a
// null and a JSON type mismatch, each equal to one encoding/json call.
func TestChunkedDecodeShapes(t *testing.T) {
	type shapes struct {
		Ptr    *struct{ L []int }              `json:"ptr"`
		Map    map[string][]int                `json:"map"`
		Ptrs   []*struct{ A int }              `json:"ptrs"`
		Any    any                             `json:"any"`
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
			c := &chunker{w: newWork(nil), limit: limit}
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
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c", "labels": {"a": "b"}}, "spec": {"credentials": {}, "tags": ["b", "a"]}}`,
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
