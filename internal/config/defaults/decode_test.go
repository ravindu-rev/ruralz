// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
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
// encoding/json in Decode alone: an undeclared member of an open config
// whose name differs from a field's only in case never reaches the typed
// config, which always agrees with the tree, while the raw config keeps
// it. Stage G rejects such a member in Normalize, before the decode
// (TestFoldedMemberNames, TestRawConfigAgrees).
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
		_, msg, ok := firstFailure(l, tc.typ, n, nil)
		if ok != (tc.want != "") || msg != tc.want {
			t.Errorf("walk(%v, %s) = %q, %v, want %q", tc.typ, tc.src, msg, ok, tc.want)
		}
	}
	if _, _, ok := firstFailure(&locator{stack: stack{{}}}, reflect.TypeFor[int](), nil, nil); ok {
		t.Error("walk(nil) reported")
	}
}

// firstFailure runs l over n (Go type t, schema s) and returns its first
// failure: the path and the message without the "cannot decode: " every
// decode message starts with.
func firstFailure(l *locator, t reflect.Type, n *tree.Node, s *schemaidx.Node) (path diag.Path, msg string, ok bool) {
	l.report = func(p diag.Path, _ *tree.Node, m string) bool {
		if !ok {
			path, msg, ok = p, strings.TrimPrefix(m, "cannot decode: "), true
		}
		return false
	}
	l.locateAll(t, n, s)
	return path, msg, ok
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
	// An integral literal too large for the canonical form, which only
	// canonical content brings to the decode (R-64), is out of range.
	if got := intRange("1e9223372036854775807", 32, false); got != "integer 1e9223372036854775807 is outside the range -2147483648 to 2147483647 of this field" {
		t.Errorf("huge exponent = %q", got)
	}
	if got := intRange("1e-9223372036854775808", 32, false); got != "1e-9223372036854775808 is not an integer" {
		t.Errorf("tiny exponent = %q", got)
	}
}

// strictObject fails to decode JSON text that holds the undeclared
// member REQUESTS.
type strictObject struct{}

func (*strictObject) UnmarshalJSON(b []byte) error {
	if strings.Contains(string(b), "REQUESTS") {
		return errors.New("saw an undeclared member")
	}
	return nil
}

// TestLocatorDeclaredOnly covers 01 req 38 for a config decode that
// fails (WP-37 review round 3: the locator walked undeclared members the
// decode never saw, and blamed one that matched a field case-insensitively):
// the failure is reported at the declared member that failed, in a struct
// (an open RateLimit entry with an undeclared REQUESTS beside a burst of
// 1e+30, which canonical content brings this far, R-64), in a Go map
// whose schema declares properties, and for a type with its own
// UnmarshalJSON, which gets the declared members only; the kind struct
// decode, which sees every member, still matches names
// case-insensitively.
func TestLocatorDeclaredOnly(t *testing.T) {
	idx := hubIndex(t)
	f := newFixture()
	res := f.resourceRole(t, "c.json", policyDoc("ratelimit", `{"limits": [{"REQUESTS": "x", "burst": 1e+30, "requests": 1, "window": "1s"}]}`), tree.RoleCanonical)
	_, _, ds := Decode(idx, registry.New(), res, f.files)
	want := "Policy/p spec.config.limits[0].burst: cannot decode: integer 1e+30 is outside the range -9223372036854775808 to 9223372036854775807 of this field"
	if len(ds) != 1 || !strings.Contains(texts(ds)[0], want) {
		t.Errorf("Decode = %q, want one diagnostic containing %q", texts(ds), want)
	}
	rateLimit, ok := idx.Def("RateLimit")
	if !ok {
		t.Fatal("no RateLimit definition")
	}
	n := mustParse(t, `{"REQUESTS": "x", "requests": 1, "window": "1s"}`)
	for _, tc := range []struct {
		name     string
		declared bool
		typ      reflect.Type
		at, msg  string
	}{
		{"config struct", true, reflect.TypeFor[v1alpha1.RateLimit](), "", ""},
		{"config map", true, reflect.TypeFor[map[string]int64](), "window", "want an integer"},
		{"kind struct", false, reflect.TypeFor[v1alpha1.RateLimit](), "REQUESTS", "want an integer"},
		{"kind map", false, reflect.TypeFor[map[string]int64](), "REQUESTS", "want an integer"},
		// A type with its own UnmarshalJSON gets the members the decode
		// would hand it.
		{"config unmarshaler", true, reflect.TypeFor[strictObject](), "", ""},
		{"kind unmarshaler", false, reflect.TypeFor[strictObject](), "", "saw an undeclared member"},
	} {
		l := &locator{stack: stack{{node: n}}, declared: tc.declared}
		path, msg, found := firstFailure(l, tc.typ, n, rateLimit)
		at := ""
		if found {
			at = path.String()
		}
		if at != tc.at || msg != tc.msg {
			t.Errorf("%s: find = %q at %q, want %q at %q", tc.name, msg, at, tc.msg, tc.at)
		}
	}
}

// TestLocatorScaleLinear covers the locator's one path per failed decode
// (01 req 38 and 53; WP-37 review round 3 named the locator's stack with
// the normalizer's): a decode failure in the last entry of a map list of
// n entries, each named by its key member, costs about eight times the
// work units for eight times the entries, since the locator walks the
// tree once and names each element on the failure's path once.
func TestLocatorScaleLinear(t *testing.T) {
	idx := hubIndex(t)
	cost := func(n int) int {
		parts := make([]string, n)
		for i := range parts {
			parts[i] = `{"name": "u` + strconv.Itoa(i) + `", "weight": 1}`
		}
		parts[n-1] = `{"name": "last", "weight": 4294967296}`
		res := envelope(mustParse(t, route(`{"match": {}, "upstreams": [`+strings.Join(parts, ", ")+`]}`)))
		w := newWork()
		_, _, ds, failed := decode(idx, nil, res, nil, w, nil)
		if !failed || len(ds) != 1 || ds[0].Path.String() != "spec.upstreams[name=last].weight" {
			t.Fatalf("decode = %v, %q; want one failure at the last entry", failed, texts(ds))
		}
		return w.total
	}
	a, b := cost(1000), cost(8000)
	if ratio := float64(b) / float64(a); ratio < 4 || ratio > 16 {
		t.Errorf("work units: 1000 entries %d, 8000 entries %d: ratio %.2f, want about 8", a, b, ratio)
	}
}

// TestDecodeEveryFailure covers 02 req 2 and 01 req 38 for the decode
// (WP-37 review round 3: each resource reported at most one decode
// failure, none when normalization failed, and no config failure once the
// kind struct failed): every value that does not fit is reported at its
// field, in one run, the config's too, and after a normalization error
// the decode still reports the values normalization did not, each failure
// once, up to the diagnostic budget.
func TestDecodeEveryFailure(t *testing.T) {
	consumer := `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {"basic": [
	  {"username": "a", "hash": "h", "iterations": 4294967296}, {"username": "b", "hash": "h", "iterations": 4294967297}]}}}`
	policy := `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "ratelimit", "overridable": "yes",
	  "config": {"limits": [{"requests": "x", "window": "1s"}, {"requests": 1, "window": "1s", "burst": "y"}]}}}`
	tests := []struct {
		name  string
		src   string
		edit  func(t *testing.T, res *tree.Resource)
		cap   int
		paths []string
	}{
		{
			name: "two weights", src: route(`{"match": {}, "upstreams": [{"name": "a", "weight": 4294967296}, {"name": "b", "weight": 4294967297}]}`),
			paths: []string{"spec.upstreams[name=a].weight", "spec.upstreams[name=b].weight"},
		},
		{
			name: "two credentials", src: consumer,
			paths: []string{"spec.credentials.basic[username=a].iterations", "spec.credentials.basic[username=b].iterations"},
		},
		{
			name: "kind and config", src: policy,
			paths: []string{"spec.overridable", "spec.config.limits[0].requests", "spec.config.limits[1].burst"},
		},
		{
			name: "after a normalization error", src: route(`{"match": {}, "timeout": "9999999h", "upstreams": [{"name": "a", "weight": 4294967296}]}`),
			paths: []string{"spec.timeout", "spec.upstreams[name=a].weight"},
		},
		// The config type then comes from the tree, the kind struct not
		// being decoded.
		{
			name: "config after a normalization error",
			src: `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "ratelimit", "stateStoreTimeout": "9999999h",
			  "config": {"limits": [{"requests": "x", "window": "1s"}]}}}`,
			paths: []string{"spec.stateStoreTimeout", "spec.config.limits[0].requests"},
		},
		// A value normalization reported is not reported again by the
		// decode, which fails on it too.
		{name: "byte size", src: gateway(`{"limits": {"maxBufferedBytes": "1x"}}`), paths: []string{"spec.limits.maxBufferedBytes"}},
		{
			name: "integer range", src: route(`{"match": {}, "upstreams": [{"name": "a", "weight": 9007199254740993}]}`),
			paths: []string{"spec.upstreams[name=a].weight"},
		},
		{
			name: "duration not UTF-8", src: route(`{"match": {}, "timeout": "1s"}`),
			edit:  func(t *testing.T, res *tree.Resource) { at(t, res.Root, "spec.timeout").Text = "1\xffs" },
			paths: []string{"spec.timeout"},
		},
		{
			name: "config text not UTF-8", src: policyDoc("auth.api-key", `{"header": "x"}`),
			edit:  func(t *testing.T, res *tree.Resource) { at(t, res.Root, "spec.config.header").Text = "x\xff" },
			paths: []string{"spec.config.header"},
		},
		{name: "null in free content", src: pluginDoc(`{"a": null}`), paths: []string{"spec.config.a"}},
		// The budget bounds the decode's failures: cap 1 keeps two.
		{
			name: "budget", src: route(`{"match": {}, "upstreams": [{"name": "a", "weight": 4294967296}, {"name": "b", "weight": 4294967297}, {"name": "c", "weight": 4294967298}]}`),
			cap: 1, paths: []string{"spec.upstreams[name=a].weight", "spec.upstreams[name=b].weight"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := stageWith(t, Options{MaxDiagnostics: tc.cap})
			f := newFixture()
			res := f.resource(t, "r.json", tc.src)
			if tc.edit != nil {
				tc.edit(t, res)
			}
			h, ds := s.Resource(res, f.files)
			var paths []string
			for _, d := range ds {
				paths = append(paths, d.Path.String())
				if d.Code != CodeSchema || d.Line == 0 {
					t.Errorf("diagnostic %s, want RZ-CFG-005 with a position", d.AppendText(nil))
				}
			}
			if h != nil || !slices.Equal(paths, tc.paths) {
				t.Errorf("Resource = %v, %q; want the paths %q", h != nil, texts(ds), tc.paths)
			}
		})
	}
}

// TestLocatorManyFailuresLinear covers the locator's paths when many
// failures lie below one list element (02 req 2, 01 req 53): each
// element on the paths is named once (stack.path keeps the name), so n
// failing members of one set element, which is named by its RFC 8785
// text of n members, cost about eight times the work units for eight
// times the members, not sixty-four.
func TestLocatorManyFailuresLinear(t *testing.T) {
	spec, ok := setSchema(t).Spec("Thing")
	if !ok {
		t.Fatal("no Thing spec")
	}
	set := memberSchema(spec, "s")
	cost := func(n int) int {
		var b strings.Builder
		for i := range n {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(`"A` + strconv.Itoa(1_000_000+i) + `": 300`)
		}
		list := mustParse(t, `[{`+b.String()+`}]`)
		w := newWork()
		found := 0
		l := &locator{stack: stack{{node: list}}, w: w, report: func(p diag.Path, _ *tree.Node, _ string) bool {
			if found++; len(p) != 2 || p[0].Kind != diag.ElemItem {
				t.Fatalf("path %s, want the set element and the member", p)
			}
			return true
		}}
		if got := l.locateAll(reflect.TypeFor[[]map[string]int8](), list, set); got != n || found != n {
			t.Fatalf("%d failures found, %d reported, want %d", got, found, n)
		}
		return w.total
	}
	a, b := cost(500), cost(4000)
	if ratio := float64(b) / float64(a); ratio < 4 || ratio > 16 {
		t.Errorf("work units: 500 failures %d, 4000 failures %d: ratio %.2f, want about 8", a, b, ratio)
	}
}
