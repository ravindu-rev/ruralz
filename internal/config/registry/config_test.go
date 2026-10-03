// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the typed config constructor (R-7, R-62; 01 req 38 "Policy
// spec.config is also decoded into the registry's config type"). Every
// expectation is read at run time from the committed schema and the
// v1alpha1 sources, so the table stays correct whichever additive WP-28
// schema is merged.

// configTypeName returns the Go type name NewConfig of e constructs.
func configTypeName(t *testing.T, e Entry) string {
	t.Helper()
	if e.NewConfig == nil {
		t.Fatalf("%s has no config type", e.Type)
	}
	v := e.NewConfig()
	rt := reflect.TypeOf(v)
	if rt == nil || rt.Kind() != reflect.Pointer {
		t.Fatalf("%s NewConfig returned %T, want a pointer", e.Type, v)
	}
	if rt.Elem().PkgPath() != reflect.TypeFor[v1alpha1.Policy]().PkgPath() {
		t.Fatalf("%s NewConfig returned %T, not a v1alpha1 type", e.Type, v)
	}
	return rt.Elem().Name()
}

// enumDef is a $defs entry with an enum.
type enumDef[T any] struct {
	Enum []T `json:"enum"`
}

// policySpecDef is the PolicySpec $defs entry: its if/then dispatch on
// spec.type.
type policySpecDef struct {
	AllOf []struct {
		If struct {
			Properties struct {
				Type struct {
					Const v1alpha1.PolicyType `json:"const"`
				} `json:"type"`
			} `json:"properties"`
		} `json:"if"`
		Then struct {
			Properties struct {
				Config struct {
					Ref string `json:"$ref"`
				} `json:"config"`
			} `json:"properties"`
		} `json:"then"`
	} `json:"allOf"`
}

// schemaDef decodes the $defs entry name of the rendered schema into v.
func schemaDef(t *testing.T, name string, v any) {
	t.Helper()
	var doc struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if err := json.Unmarshal(schema.RenderedV1alpha1(), &doc); err != nil {
		t.Fatal(err)
	}
	raw, ok := doc.Defs[name]
	if !ok {
		t.Fatalf("rendered schema has no $defs/%s", name)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("$defs/%s: %v", name, err)
	}
}

// TestNewConfigMatchesSchemaDispatch: the registry lists exactly the
// schema's PolicyType enum, and each type's NewConfig builds the config
// type the PolicySpec if/then dispatch names (02 req 11, R-62).
func TestNewConfigMatchesSchemaDispatch(t *testing.T) {
	var (
		types    enumDef[v1alpha1.PolicyType]
		classes  enumDef[v1alpha1.FilterClass]
		modes    enumDef[v1alpha1.FailureMode]
		spec     policySpecDef
		dispatch = map[v1alpha1.PolicyType]string{}
	)
	schemaDef(t, "PolicyType", &types)
	schemaDef(t, "FilterClass", &classes)
	schemaDef(t, "FailureMode", &modes)
	schemaDef(t, "PolicySpec", &spec)
	reg := New()
	if got := reg.Types(); !slices.Equal(got, types.Enum) {
		t.Errorf("registry types = %v\nschema PolicyType enum = %v", got, types.Enum)
	}
	for _, rule := range spec.AllOf {
		typ := rule.If.Properties.Type.Const
		ref := rule.Then.Properties.Config.Ref
		if typ == "" || ref == "" {
			continue
		}
		dispatch[typ] = strings.TrimPrefix(ref, "#/$defs/")
	}
	if len(dispatch) != len(reg.Types()) {
		t.Errorf("schema dispatches %d types, registry has %d", len(dispatch), len(reg.Types()))
	}
	for _, e := range reg.Entries() {
		if got, w := configTypeName(t, e), dispatch[e.Type]; got != w {
			t.Errorf("%s: NewConfig builds %s, schema dispatches to %s", e.Type, got, w)
		}
	}
	// Every schema Filter class is a phase.Class the registry can name.
	for _, c := range classes.Enum {
		if _, ok := phase.ParseClass(c); !ok {
			t.Errorf("schema Filter class %q is not a phase.Class", c)
		}
	}
	if int(phase.NumClasses) != len(classes.Enum) {
		t.Errorf("schema has %d Filter classes, phase has %d", len(classes.Enum), phase.NumClasses)
	}
	for _, e := range reg.Entries() {
		if !slices.Contains(modes.Enum, e.DefaultFailureMode) {
			t.Errorf("%s: default failureMode %q is not in the schema enum", e.Type, e.DefaultFailureMode)
		}
	}
}

// v1alpha1Sources parses the non-test Go files of pkg/config/v1alpha1.
func v1alpha1Sources(t *testing.T) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", "pkg", "config", "v1alpha1", "*.go"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("glob v1alpha1 sources: %v (%d files)", err, len(paths))
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestNewConfigMatchesSourceMarkers: every PolicyType constant declared in
// pkg/config/v1alpha1 is registered, and NewConfig builds the type whose
// "+ruralz:policyType=<type>" marker names it ("every type has a config
// type or explicit none"; keyed by existing config type names, R-62).
func TestNewConfigMatchesSourceMarkers(t *testing.T) {
	var consts []v1alpha1.PolicyType
	markers := map[v1alpha1.PolicyType]string{}
	for _, f := range v1alpha1Sources(t) {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gd.Specs {
				switch sp := spec.(type) {
				case *ast.ValueSpec:
					if id, ok := sp.Type.(*ast.Ident); !ok || id.Name != "PolicyType" || gd.Tok != token.CONST {
						continue
					}
					for _, v := range sp.Values {
						if lit, ok := v.(*ast.BasicLit); ok && lit.Kind == token.STRING {
							s, err := strconv.Unquote(lit.Value)
							if err != nil {
								t.Fatal(err)
							}
							consts = append(consts, v1alpha1.PolicyType(s))
						}
					}
				case *ast.TypeSpec:
					doc := sp.Doc
					if doc == nil {
						doc = gd.Doc
					}
					if doc == nil {
						continue
					}
					for _, c := range doc.List {
						if _, typ, ok := strings.Cut(c.Text, "+ruralz:policyType="); ok {
							typ = strings.TrimSpace(typ)
							if prev, dup := markers[v1alpha1.PolicyType(typ)]; dup {
								t.Errorf("policyType %s marked on %s and %s", typ, prev, sp.Name.Name)
							}
							markers[v1alpha1.PolicyType(typ)] = sp.Name.Name
						}
					}
				}
			}
		}
	}
	reg := New()
	if got := reg.Types(); !slices.Equal(got, consts) {
		t.Errorf("registry types = %v\nv1alpha1 PolicyType constants = %v", got, consts)
	}
	if len(markers) != len(reg.Types()) {
		t.Errorf("%d policyType markers, registry has %d types", len(markers), len(reg.Types()))
	}
	for _, e := range reg.Entries() {
		w, ok := markers[e.Type]
		if !ok {
			t.Errorf("%s: no +ruralz:policyType marker in v1alpha1", e.Type)
			continue
		}
		if got := configTypeName(t, e); got != w {
			t.Errorf("%s: NewConfig builds %s, marker is on %s", e.Type, got, w)
		}
	}
}

// TestNewConfigDecodes: each constructor returns a fresh zero config that
// spec.config decodes into (01 req 38), and Registry.NewConfig agrees.
func TestNewConfigDecodes(t *testing.T) {
	reg := New()
	for _, e := range reg.Entries() {
		t.Run(string(e.Type), func(t *testing.T) {
			a, b := e.NewConfig(), e.NewConfig()
			// Zero-size configs may share an address; others must not.
			if reflect.TypeOf(a).Elem().Size() > 0 && reflect.ValueOf(a).Pointer() == reflect.ValueOf(b).Pointer() {
				t.Error("NewConfig returned the same value twice")
			}
			if !reflect.ValueOf(a).Elem().IsZero() {
				t.Error("NewConfig returned a non-zero config")
			}
			if err := json.Unmarshal([]byte(`{}`), a); err != nil {
				t.Errorf("decode {}: %v", err)
			}
			c, err := reg.NewConfig(e.Type)
			if err != nil || reflect.TypeOf(c) != reflect.TypeOf(a) {
				t.Errorf("Registry.NewConfig = %T, %v; want %T", c, err, a)
			}
		})
	}
	if _, err := reg.NewConfig("rateLimit"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("NewConfig of an unknown type: %v, want ErrUnknownType", err)
	}
	// A registry row without a config type yields nil, nil ("explicit
	// none"); v1alpha1 has none today, so check the mechanism directly.
	none := &Registry{entries: []Entry{{Type: "x.none"}}, byType: map[v1alpha1.PolicyType]int{"x.none": 0}}
	if c, err := none.NewConfig("x.none"); c != nil || err != nil {
		t.Errorf("NewConfig of a type without config = %v, %v", c, err)
	}
}

// TestNewConfigSample decodes a representative config of each served type
// with a list or CEL field, to show the constructors are the decode
// targets stage G uses.
func TestNewConfigSample(t *testing.T) {
	reg := New()
	tests := []struct {
		typ   v1alpha1.PolicyType
		json  string
		check func(any) bool
	}{
		{v1alpha1.PolicyTypeRateLimit, `{"key":"consumer.name","limits":[{"requests":10,"window":"1s"}]}`, func(v any) bool {
			c := v.(*v1alpha1.RateLimitConfig)
			return c.Key == "consumer.name" && len(c.Limits) == 1 && c.Limits[0].Requests == 10
		}},
		{v1alpha1.PolicyTypeAuthzCEL, `{"rule":"true"}`, func(v any) bool { return v.(*v1alpha1.AuthzCELConfig).Rule == "true" }},
		{v1alpha1.PolicyTypeHeaders, `{"request":{"set":[{"name":"x","value":"1"}]}}`, func(v any) bool {
			c := v.(*v1alpha1.HeadersConfig)
			return c.Request != nil && len(c.Request.Set) == 1 && c.Response == nil
		}},
		{v1alpha1.PolicyTypePlugin, `{"any":["thing"]}`, func(v any) bool { return len(*v.(*v1alpha1.PluginConfig)) == 1 }},
	}
	for _, tt := range tests {
		c, err := reg.NewConfig(tt.typ)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(tt.json), c); err != nil {
			t.Errorf("%s: %v", tt.typ, err)
			continue
		}
		if !tt.check(c) {
			t.Errorf("%s: decoded %+v", tt.typ, c)
		}
	}
}
