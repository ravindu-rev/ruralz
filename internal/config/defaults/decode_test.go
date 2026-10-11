// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestDecodeKinds covers 01 req 38 and 02 req 5: every kind decodes into
// its v1alpha1 struct after stage G, with normalized scalars, and a Policy
// config into its registry type.
func TestDecodeKinds(t *testing.T) {
	s := newStage(t)
	f := newFixture()
	tests := []struct {
		src   string
		check func(t *testing.T, obj, cfg any)
	}{
		{
			gateway(`{"listeners": [{"name": "h", "port": 8080, "protocol": "http"}], "limits": {"maxRequestBodyBytes": "1Mi"}, "stateStore": {}}`),
			func(t *testing.T, obj, _ any) {
				g := obj.(*v1alpha1.Gateway)
				if g.Spec.Limits == nil || g.Spec.Limits.MaxRequestBodyBytes == nil || *g.Spec.Limits.MaxRequestBodyBytes != 1<<20 {
					t.Errorf("limits = %+v", g.Spec.Limits)
				}
				if g.Spec.StateStore == nil || g.Spec.StateStore.Timeout == nil || time.Duration(*g.Spec.StateStore.Timeout) != 50*time.Millisecond {
					t.Errorf("stateStore = %+v", g.Spec.StateStore)
				}
				if g.Spec.Admin != nil {
					t.Error("absent admin was created")
				}
			},
		},
		{
			route(`{"match": {"path": {"prefix": "/"}}, "timeout": "90s", "upstreams": [{"name": "u"}]}`),
			func(t *testing.T, obj, _ any) {
				r := obj.(*v1alpha1.Route)
				if r.Spec.Timeout == nil || time.Duration(*r.Spec.Timeout) != 90*time.Second || *r.Spec.Upstreams[0].Weight != 1 {
					t.Errorf("route = %+v", r.Spec)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "a:1"}]}}`,
			func(t *testing.T, obj, _ any) {
				if u := obj.(*v1alpha1.Upstream); u.Spec.Endpoints[0].Address != "a:1" {
					t.Errorf("upstream = %+v", u.Spec)
				}
			},
		},
		{
			policyDoc("ratelimit", `{"limits": [{"requests": 10, "window": "60s"}]}`),
			func(t *testing.T, obj, cfg any) {
				p := obj.(*v1alpha1.Policy)
				if p.Spec.Slot != "p" || *p.Spec.FailureMode != v1alpha1.FailureModeOpen || *p.Spec.FilterClass != v1alpha1.FilterClassAdmission || !*p.Spec.Overridable {
					t.Errorf("policy spec = %+v", p.Spec)
				}
				rl := cfg.(*v1alpha1.RateLimitConfig)
				if len(rl.Limits) != 1 || rl.Limits[0].Requests != 10 || time.Duration(rl.Limits[0].Window) != time.Minute || rl.LocalOnly == nil || *rl.LocalOnly {
					t.Errorf("ratelimit config = %+v", rl)
				}
				if !strings.Contains(string(p.Spec.Config), `"window":"1m0s"`) {
					t.Errorf("raw config = %s", p.Spec.Config)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"image": "x", "phases": ["onRequestHeaders"], "configSchema": {"maximum": 1.5}}}`,
			func(t *testing.T, obj, _ any) {
				if p := obj.(*v1alpha1.Plugin); p.Spec.ConfigSchema["maximum"] != json.Number("1.5") {
					t.Errorf("configSchema number = %#v, want json.Number", p.Spec.ConfigSchema["maximum"])
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {"basic": [{"username": "u", "hash": "h"}]}}}`,
			func(t *testing.T, obj, _ any) {
				if c := obj.(*v1alpha1.Consumer); *c.Spec.Credentials.Basic[0].Iterations != 600000 {
					t.Errorf("consumer = %+v", c.Spec)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "AIProvider", "metadata": {"name": "a"}, "spec": {"dialect": "openai"}}`,
			func(t *testing.T, obj, _ any) {
				if a := obj.(*v1alpha1.AIProvider); a.Spec.Dialect != "openai" {
					t.Errorf("aiprovider = %+v", a.Spec)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "AIModel", "metadata": {"name": "m"}, "spec": {"strategy": "fallback", "candidates": [{"name": "a", "provider": "p", "model": "x"}]}}`,
			func(t *testing.T, obj, _ any) {
				if m := obj.(*v1alpha1.AIModel); len(m.Spec.Candidates) != 1 {
					t.Errorf("aimodel = %+v", m.Spec)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Environment", "metadata": {"name": "prod"}, "spec": {"variables": {"A": "1"}}}`,
			func(t *testing.T, obj, _ any) {
				if e := obj.(*v1alpha1.Environment); e.Spec.Variables["A"] != "1" {
					t.Errorf("environment = %+v", e.Spec)
				}
			},
		},
		{
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Cluster", "metadata": {"name": "c"}, "spec": {"environment": "prod"}}`,
			func(t *testing.T, obj, _ any) {
				if _, ok := obj.(*v1alpha1.Cluster); !ok {
					t.Errorf("cluster = %T", obj)
				}
			},
		},
	}
	for _, tc := range tests {
		res := f.resource(t, "r.json", tc.src)
		t.Run(string(res.ID.Kind), func(t *testing.T) {
			h, ds := s.Resource(res, f.files)
			if len(ds) != 0 || h == nil {
				t.Fatalf("Resource: %q", texts(ds))
			}
			if metaOf(h.Object) == nil || metaOf(h.Object).Name != res.ID.Name {
				t.Errorf("object metadata = %+v", metaOf(h.Object))
			}
			tc.check(t, h.Object, h.Config)
		})
	}
}

// TestDecodeErrors covers 01 req 38: a value its Go field cannot hold is
// RZ-CFG-005 at the field's key-aware path, in the resource and in a
// Policy config, with a message that does not depend on encoding/json.
func TestDecodeErrors(t *testing.T) {
	idx := hubIndex(t)
	reg := registry.New()
	tests := []struct {
		name, src string
		want      string
	}{
		{
			"int32 weight", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 4294967296}]}`),
			"r.json:1:139 error RZ-CFG-005 Route/r spec.upstreams[name=u].weight: cannot decode: integer 4294967296 is outside the range -2147483648 to 2147483647 of this field",
		},
		{
			"int32 iterations", `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {"basic": [{"username": "u", "hash": "h", "iterations": -3000000000}]}}}`,
			"spec.credentials.basic[username=u].iterations: cannot decode: integer -3000000000 is outside the range",
		},
		{
			"config fraction in int64", policyDoc("ratelimit", `{"limits": [{"requests": 1.5, "window": "1s"}]}`),
			"Policy/p spec.config.limits[0].requests: cannot decode: 1.5 is not an integer",
		},
		{
			"config duration", policyDoc("ratelimit", `{"limits": [{"requests": 1, "window": "1x"}]}`),
			`spec.config.limits[0].window: cannot decode: invalid duration "1x"`,
		},
		{
			"resource wrong type", route(`{"match": {}, "timeout": 5}`),
			`spec.timeout: cannot decode: duration must be a string such as "50ms"`,
		},
		{
			"string for bool", gateway(`{"listeners": [{"name": "h", "proxyProtocol": "yes"}]}`),
			"spec.listeners[name=h].proxyProtocol: cannot decode: want a boolean",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture()
			res := f.resource(t, "r.json", tc.src)
			obj, cfg, ds := Decode(idx, reg, res, f.files)
			if obj != nil || cfg != nil || len(ds) != 1 || !strings.Contains(texts(ds)[0], tc.want) {
				t.Errorf("Decode = %T %T %q, want one diagnostic containing %q", obj, cfg, texts(ds), tc.want)
			}
		})
	}
	// An unknown kind has no hub type.
	f := newFixture()
	res := f.resource(t, "x.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Nope", "metadata": {"name": "n"}}`)
	if _, _, ds := Decode(idx, reg, res, f.files); len(ds) != 1 || ds[0].Code != CodeUnserved || ds[0].Path.String() != "kind" {
		t.Errorf("unknown kind: %q", texts(ds))
	}
	if obj, cfg, ds := Decode(idx, reg, nil, nil); obj != nil || cfg != nil || ds != nil {
		t.Error("Decode(nil) returned something")
	}
}

// TestDecodeOpenConfigCase covers the case-insensitive match of
// encoding/json: an undeclared member of an open config whose name
// differs from a field's only in case never reaches the typed config,
// which always agrees with the tree, while the raw config keeps it.
func TestDecodeOpenConfigCase(t *testing.T) {
	f := newFixture()
	res := f.resource(t, "p.json", policyDoc("auth.api-key", `{"header": "x-a", "Header": "x-b", "HEADER": "x-c", "extra": {"Header": 1}}`))
	obj, cfg, ds := Decode(hubIndex(t), registry.New(), res, f.files)
	if len(ds) != 0 {
		t.Fatalf("Decode: %q", texts(ds))
	}
	c := cfg.(*v1alpha1.AuthAPIKeyConfig)
	if c.Header == nil || *c.Header != "x-a" {
		t.Errorf("typed header = %v, want x-a", c.Header)
	}
	raw := string(obj.(*v1alpha1.Policy).Spec.Config)
	if !strings.Contains(raw, `"Header":"x-b"`) || !strings.Contains(raw, `"extra":{"Header":1}`) {
		t.Errorf("raw config lost members: %s", raw)
	}
	// With only the differently cased member, the field stays unset.
	res = f.resource(t, "p2.json", policyDoc("auth.api-key", `{"Header": "x-b"}`))
	_, cfg, _ = Decode(hubIndex(t), registry.New(), res, f.files)
	if c := cfg.(*v1alpha1.AuthAPIKeyConfig); c.Header != nil {
		t.Errorf("typed header = %q from a differently cased member", *c.Header)
	}
}

// TestDecodePluginConfig covers the untyped plugin config: members keep
// json.Number numbers (02 req 16).
func TestDecodePluginConfig(t *testing.T) {
	f := newFixture()
	res := f.resource(t, "p.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "plugin", "plugin": "pl", "config": {"n": 12345678901234567, "f": 0.1, "l": [1], "o": {"Z": true}}}}`)
	_, cfg, ds := Decode(hubIndex(t), registry.New(), res, f.files)
	if len(ds) != 0 {
		t.Fatalf("Decode: %q", texts(ds))
	}
	c := *cfg.(*v1alpha1.PluginConfig)
	if c["n"] != json.Number("12345678901234567") || c["f"] != json.Number("0.1") {
		t.Errorf("plugin config = %#v", c)
	}
	if o, _ := c["o"].(map[string]any); o["Z"] != true {
		t.Errorf("plugin config object = %#v", c["o"])
	}
}

// TestDecodeConfigEdges covers a Policy of an unregistered type (no typed
// config), one without config, and decoding without a registry.
func TestDecodeConfigEdges(t *testing.T) {
	idx := hubIndex(t)
	f := newFixture()
	res := f.resource(t, "p.json", policyDoc("nope", `{}`))
	if obj, cfg, ds := Decode(idx, registry.New(), res, f.files); obj == nil || cfg != nil || len(ds) != 0 {
		t.Errorf("unregistered type: %T %T %q", obj, cfg, texts(ds))
	}
	res = f.resource(t, "p2.json", `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "cors"}}`)
	if _, cfg, ds := Decode(idx, registry.New(), res, f.files); len(ds) != 0 || reflect.TypeOf(cfg) != reflect.TypeFor[*v1alpha1.CORSConfig]() {
		t.Errorf("no config: %T %q", cfg, texts(ds))
	}
	if _, cfg, ds := Decode(idx, nil, res, f.files); cfg != nil || len(ds) != 0 {
		t.Errorf("no registry: %T %q", cfg, texts(ds))
	}
}

type embeddedFields struct {
	Promoted string `json:"promoted"`
}

type fieldsFixture struct {
	embeddedFields
	Plain    int
	Tagged   string `json:"tagged,omitempty"`
	Skipped  string `json:"-"`
	unexport string
	Named    embeddedFields `json:"named"`
}

func TestJSONFields(t *testing.T) {
	ft := jsonFields(reflect.TypeFor[fieldsFixture]())
	if want := []string{"Plain", "tagged", "named", "promoted"}; !sameSet(ft.names, want) {
		t.Errorf("names = %q, want %q", ft.names, want)
	}
	if typ, ok := ft.lookup("TAGGED"); !ok || typ.Kind() != reflect.String {
		t.Errorf("case-insensitive lookup = %v, %v", typ, ok)
	}
	if _, ok := ft.lookup("missing"); ok {
		t.Error("lookup found a missing name")
	}
	_ = fieldsFixture{unexport: ""}.unexport
}

func sameSet(a, b []string) bool {
	a, b = slices.Clone(a), slices.Clone(b)
	slices.Sort(a)
	slices.Sort(b)
	return slices.Equal(a, b)
}

// TestLocatorMismatches covers the locator's type checks, which stage F
// normally makes unreachable.
func TestLocatorMismatches(t *testing.T) {
	tests := []struct {
		typ  reflect.Type
		src  string
		want string
	}{
		{reflect.TypeFor[string](), `1`, "want a string"},
		{reflect.TypeFor[int](), `"x"`, "want an integer"},
		{reflect.TypeFor[uint8](), `"x"`, "want an integer"},
		{reflect.TypeFor[uint8](), `300`, "integer 300 is outside the range 0 to 255 of this field"},
		{reflect.TypeFor[float32](), `"x"`, "want a number"},
		{reflect.TypeFor[float32](), `1e300`, "number 1e300 is outside the range of a 32-bit float"},
		{reflect.TypeFor[[]int](), `{}`, "want a list"},
		{reflect.TypeFor[[2]int](), `[1, "x"]`, "want an integer"},
		{reflect.TypeFor[map[string]int](), `[]`, "want an object"},
		{reflect.TypeFor[map[string]int](), `{"a": "x"}`, "want an integer"},
		{reflect.TypeFor[struct{ A int }](), `1`, "want an object"},
		{reflect.TypeFor[chan int](), `1`, "no decoder for Go type chan int"},
		{reflect.TypeFor[*int](), `null`, ""},
		{reflect.TypeFor[int](), `null`, ""},
		{reflect.TypeFor[any](), `[1]`, ""},
		{reflect.TypeFor[uint16](), `7`, ""},
		{reflect.TypeFor[float64](), `7.5`, ""},
		{reflect.TypeFor[bool](), `true`, ""},
		{reflect.TypeFor[json.RawMessage](), `{"a": 1}`, ""},
	}
	for _, tc := range tests {
		l := &locator{stack: stack{{}}}
		n := mustParse(t, tc.src)
		_, msg, ok := l.find(tc.typ, n, nil)
		if ok != (tc.want != "") || msg != tc.want {
			t.Errorf("find(%v, %s) = %q, %v, want %q", tc.typ, tc.src, msg, ok, tc.want)
		}
	}
	// With no value at fault, locate reports the error text at the top.
	l := &locator{stack: stack{{}}}
	path, _, msg := l.locate(reflect.TypeFor[int](), mustParse(t, `1`), nil, errors.New("boom"))
	if len(path) != 0 || msg != "cannot decode: boom" {
		t.Errorf("locate fallback = %v %q", path, msg)
	}
	if _, _, ok := l.find(reflect.TypeFor[int](), nil, nil); ok {
		t.Error("find(nil) reported")
	}
}

func TestAppendJSON(t *testing.T) {
	n := mustParse(t, `{"s": "a\"<>& ", "n": [1, 2.5, -0, true, null], "o": {}}`)
	got, err := appendJSON(nil, n, nil, nil)
	if err != nil || string(got) != `{"s":"a\"<>&`+" "+`","n":[1,2.5,-0,true,null],"o":{}}` {
		t.Errorf("appendJSON = %s, %v", got, err)
	}
	bad := &tree.Node{Kind: tree.KindString, Text: "\xff"}
	if _, err := appendJSON(nil, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "a", Value: bad}}}, nil, nil); err == nil {
		t.Error("invalid UTF-8 value encoded")
	}
	if _, err := appendJSON(nil, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "\xff", Value: bad}}}, nil, nil); err == nil {
		t.Error("invalid UTF-8 key encoded")
	}
	if _, err := appendJSON(nil, &tree.Node{Kind: tree.KindList, Items: []*tree.Node{bad}}, nil, nil); err == nil {
		t.Error("invalid UTF-8 element encoded")
	}
	if _, err := appendJSON(nil, &tree.Node{Kind: tree.Kind(99)}, nil, nil); err == nil {
		t.Error("unknown kind encoded")
	}
	for _, text := range []string{"", "abc", "01", "1.", "+1", "1e"} {
		if _, err := appendJSON(nil, &tree.Node{Kind: tree.KindFloat, Text: text}, nil, nil); err == nil {
			t.Errorf("invalid number %q encoded", text)
		}
	}
}

// TestDecodeInvalidNumberText checks that a number literal outside the
// RFC 8259 grammar, which only a tree built in code can hold, fails the
// decode in a json.RawMessage too, whose UnmarshalJSON the chunker calls
// directly.
func TestDecodeInvalidNumberText(t *testing.T) {
	res := envelope(mustParse(t, policyDoc("plugin", `{"n": 1}`)))
	at(t, res.Root, "spec.config.n").Text = "1x"
	if obj, cfg, ds := Decode(hubIndex(t), registry.New(), res, nil); obj != nil || cfg != nil || len(ds) != 1 || ds[0].Code != CodeSchema {
		t.Errorf("Decode = %T %T %q, want one RZ-CFG-005", obj, cfg, texts(ds))
	}
}

func TestIntRange(t *testing.T) {
	if got := intRange("5000000000", 32, true); got != "integer 5000000000 is outside the range 0 to 4294967295 of this field" {
		t.Errorf("unsigned = %q", got)
	}
	if got := intRange("1e3", 8, false); got != "1e3 is not an integer" {
		t.Errorf("float text = %q", got)
	}
}
