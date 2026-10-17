// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// foldMessage is the message of an undeclared member name that folds
// onto the declared member declared.
func foldMessage(name, declared string) string {
	return fmt.Sprintf("member %q differs from the declared member %q only in letter case: encoding/json, which matches member names case-insensitively, would decode it as that member; rename or remove it", name, declared)
}

// dupMessage is the message of a member name an object holds twice.
func dupMessage(name string) string {
	return fmt.Sprintf("duplicate member name %q: the canonical form holds each name once", name)
}

// TestFoldedMemberNames covers the case-fold rule of Normalize (WP-37
// review round 5: in an open config, an undeclared member whose name
// folds onto a declared one, such as Key beside key, passed stage F and
// stage G, and while hub.Resource.Config kept the declared value, the
// Policy's spec.config text that hub.Resource.Object and every Filter
// factory's BuildEnv.Policy carry decoded with encoding/json to the
// undeclared one, which no stage had checked). Each such member is one
// RZ-CFG-005 at its name, whether the declared member is present or not,
// at any depth of a config, for every fold encoding/json applies (upper
// case, the Kelvin sign for k, the long s for s), in authored and
// canonical content, and in typed objects a converter extended; typed-map
// keys, free content and undeclared names that fold onto nothing keep
// their place (02 req 18).
func TestFoldedMemberNames(t *testing.T) {
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatal(err)
	}
	ratelimit := func(cfg string) string { return policyDoc("ratelimit", cfg) }
	tests := []struct {
		name string
		src  string
		// want lists "<path>: <message>" of each diagnostic, in order;
		// empty for an accepted resource.
		want []string
	}{
		{
			"beside the declared member, at two depths",
			ratelimit(`{"key": "consumer.name", "Key": "request.headers['x']", "limits": [{"requests": 5, "window": "1s", "perNodeCeiling": 2, "pernodeceiling": -7}]}`),
			[]string{"spec.config.Key: " + foldMessage("Key", "key"), "spec.config.limits[0].pernodeceiling: " + foldMessage("pernodeceiling", "perNodeCeiling")},
		},
		{"without the declared member", policyDoc("auth.api-key", `{"Header": "x-b"}`), []string{"spec.config.Header: " + foldMessage("Header", "header")}},
		{"upper case", policyDoc("auth.api-key", `{"header": "x-a", "HEADER": "x-c"}`), []string{"spec.config.HEADER: " + foldMessage("HEADER", "header")}},
		{"Kelvin sign", ratelimit(`{"key": "consumer.name", "Key": "x", "limits": [{"requests": 5, "window": "1s"}]}`), []string{`spec.config["Key"]: ` + foldMessage("Key", "key")}},
		{"long s", ratelimit(`{"limits": [{"requests": 5, "requeſts": 9, "window": "1s"}]}`), []string{`spec.config.limits[0]["requeſts"]: ` + foldMessage("requeſts", "requests")}},
		{"typed-map keys differ in case", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {"team": "a", "Team": "b"}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`, nil},
		{"free content below an undeclared member", policyDoc("auth.api-key", `{"header": "x", "extra": {"Header": 1, "header": 2}}`), nil},
		{"plugin config", pluginDoc(`{"a": 1, "A": 2}`), nil},
		{"JSON Schema document", policyDoc("validation.json-schema", `{"schema": {"type": "object", "Type": "x"}}`), nil},
		{"undeclared name that folds onto nothing", policyDoc("auth.api-key", `{"header": "x", "query": "q"}`), nil},
	}
	for _, tc := range tests {
		for _, role := range roles() {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				if ds, _ := views.Validate(res, f.files); ds.HasErrors() {
					t.Fatalf("stage F rejects the fixture: %q", texts(ds))
				}
				h, ds := newStage(t).Resource(res, f.files)
				var got []string
				for _, d := range ds {
					got = append(got, d.Path.String()+": "+d.Message)
					if d.Code != CodeSchema || d.Line == 0 {
						t.Errorf("%s: want RZ-CFG-005 with a location", d.AppendText(nil))
					}
				}
				if !slices.Equal(got, tc.want) || (h == nil) != (len(tc.want) > 0) {
					t.Errorf("Resource = %v,\n%q\nwant\n%q", h != nil, got, tc.want)
				}
			})
		}
	}
}

// TestFoldedMemberNamesConverted covers the case-fold rule where only a
// converter can bring an undeclared member: a closed typed object and the
// envelope, whose kind struct decode would read the member as the
// declared one.
func TestFoldedMemberNamesConverted(t *testing.T) {
	for _, tc := range []struct {
		name, at, key, want string
	}{
		{"closed typed object", "spec", "Timeout", "spec.Timeout: " + foldMessage("Timeout", "timeout")},
		{"envelope", "", "Kind", "Kind: " + foldMessage("Kind", "kind")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := convertedStage(t, Options{}, func(doc *tree.Node) (*tree.Node, diag.List) {
				n := doc
				if tc.at != "" {
					n, _ = doc.Get(tc.at)
				}
				n.Members = append(n.Members, tree.Member{Key: tc.key, KeyPos: n.Pos, Value: &tree.Node{Kind: tree.KindString, Text: "Route", Pos: n.Pos}})
				return doc, nil
			})
			f := newFixture()
			h, ds := s.Resource(f.resource(t, "r.json", route(`{"match": {}, "timeout": "1s"}`)), f.files)
			if h != nil || len(ds) != 1 || ds[0].Path.String()+": "+ds[0].Message != tc.want {
				t.Errorf("Resource = %v, %q; want %q", h != nil, texts(ds), tc.want)
			}
		})
	}
}

// TestRawConfigAgrees is the invariant the case-fold rule gives Filter
// factories: for every Policy stage G accepts, an encoding/json decode of
// the raw spec.config of hub.Resource.Object into the type's config type
// gives hub.Resource.Config. It runs over the example Bundle and over the
// configs of TestFoldedMemberNames, which round 5 accepted with a raw
// config that decoded to the undeclared values.
func TestRawConfigAgrees(t *testing.T) {
	f, rs := loadBundle(t)
	for _, src := range []string{
		policyDoc("ratelimit", `{"key": "consumer.name", "Key": "request.headers['x']", "limits": [{"requests": 5, "window": "1s", "perNodeCeiling": 2, "pernodeceiling": -7}]}`),
		policyDoc("auth.api-key", `{"Header": "x-b"}`),
		policyDoc("auth.api-key", `{"header": "x", "extra": {"Header": 1, "header": 2}}`),
		policyDoc("auth.api-key", `{"header": "x", "query": "q"}`),
		pluginDoc(`{"a": 1, "A": 2}`),
	} {
		rs = append(rs, f.resource(t, fmt.Sprintf("p%d.json", len(rs)), src))
	}
	reg := registry.New()
	s := newStage(t)
	policies := 0
	for _, r := range rs {
		h, _ := s.Resource(r, f.files)
		if h == nil || h.Kind != v1alpha1.KindPolicy {
			continue
		}
		policies++
		p := h.Object.(*v1alpha1.Policy)
		cfg, err := reg.NewConfig(p.Spec.Type)
		if err != nil || cfg == nil || len(p.Spec.Config) == 0 {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(p.Spec.Config))
		dec.UseNumber()
		if err := dec.Decode(cfg); err != nil {
			t.Errorf("%s: decode of the raw config: %v", h.ID, err)
			continue
		}
		if !reflect.DeepEqual(cfg, h.Config) {
			t.Errorf("%s: the raw config %s decodes to %+v, the typed config is %+v", h.ID, p.Spec.Config, cfg, h.Config)
		}
	}
	if policies < 5 {
		t.Errorf("%d Policies accepted, want the Bundle's and the fixtures'", policies)
	}
}

// TestDuplicateMemberNames covers the duplicate rule of Normalize (WP-37
// review round 5: a converter or FromResources tree with a member name
// twice passed stage G, the kind struct took the last value and the
// canonical encoder later refused the tree): each later member of a run
// of equal names is one RZ-CFG-005 at its name, with the first as
// related, in typed and free content, in authored and canonical content,
// whatever the authored member order, and the decode reports nothing
// below the rejected member; a name that is not UTF-8 twice is reported
// as such only.
func TestDuplicateMemberNames(t *testing.T) {
	val := func(text string) *tree.Node { return &tree.Node{Kind: tree.KindInt, Text: text} }
	tests := []struct {
		name, src, at string
		// add are the members appended to the object at at.
		add  []tree.Member
		want []string
	}{
		{
			"typed object", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 5}]}`), "spec.upstreams[0]",
			[]tree.Member{{Key: "weight", Value: val("7")}},
			[]string{"spec.upstreams[name=u].weight: " + dupMessage("weight")},
		},
		{
			"three equal names", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 5}]}`), "spec.upstreams[0]",
			[]tree.Member{{Key: "weight", Value: val("7")}, {Key: "weight", Value: val("8")}},
			[]string{"spec.upstreams[name=u].weight: " + dupMessage("weight"), "spec.upstreams[name=u].weight: " + dupMessage("weight")},
		},
		{
			// The decode would report the string as no integer too.
			"typed object, a value of the wrong type", route(`{"match": {}, "upstreams": [{"name": "u", "weight": 5}]}`), "spec.upstreams[0]",
			[]tree.Member{{Key: "weight", Value: &tree.Node{Kind: tree.KindString, Text: "x"}}},
			[]string{"spec.upstreams[name=u].weight: " + dupMessage("weight")},
		},
		{
			"free content, out of order", pluginDoc(`{"b": 1, "a": 2}`), "spec.config",
			[]tree.Member{{Key: "b", Value: val("3")}},
			[]string{"spec.config.b: " + dupMessage("b")},
		},
		{
			"names that are not UTF-8", pluginDoc(`{"a": 1}`), "spec.config",
			[]tree.Member{{Key: notUTF8, Value: val("1")}, {Key: notUTF8, Value: val("2")}},
			[]string{`spec.config["caf` + "�" + `"]: ` + invalidText("member name"), `spec.config["caf` + "�" + `"]: ` + invalidText("member name")},
		},
	}
	for _, tc := range tests {
		for _, role := range roles() {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				n := at(t, res.Root, tc.at)
				firstPos := n.Pos
				if i := slices.IndexFunc(n.Members, func(m tree.Member) bool { return m.Key == tc.add[0].Key }); i >= 0 {
					firstPos = n.Members[i].KeyPos
				}
				for _, m := range tc.add {
					m.KeyPos, m.Value.Pos = n.Pos, n.Pos
					n.Members = append(n.Members, m)
				}
				h, ds := newStage(t).Resource(res, f.files)
				var got []string
				for _, d := range ds {
					got = append(got, d.Path.String()+": "+d.Message)
					if strings.HasPrefix(d.Message, "duplicate member name") {
						want := []diag.Related{{Location: location(f.files, firstPos), Message: "first member at"}}
						if !reflect.DeepEqual(d.Related, want) {
							t.Errorf("related = %+v, want %+v", d.Related, want)
						}
					}
				}
				if h != nil || !slices.Equal(got, tc.want) {
					t.Errorf("Resource = %v,\n%q\nwant\n%q", h != nil, got, tc.want)
				}
			})
		}
	}
}
