// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// segments splits a schema path into members, "[]" and "{}".
func segments(path string) []string {
	var out []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(path); i++ {
		switch {
		case path[i] == '.':
			flush()
		case strings.HasPrefix(path[i:], "[]"), strings.HasPrefix(path[i:], "{}"):
			flush()
			out = append(out, path[i:i+2])
			i++
		default:
			cur.WriteByte(path[i])
		}
	}
	flush()
	return out
}

// ensure returns the node at the schema path segs below root, creating
// objects (and one-element lists for "[]", member "k" for "{}").
func ensure(root *tree.Node, segs []string) *tree.Node {
	cur := root
	for _, s := range segs {
		switch s {
		case "[]":
			if cur.Kind != tree.KindList {
				cur.Kind, cur.Members = tree.KindList, nil
			}
			if len(cur.Items) == 0 {
				cur.Items = append(cur.Items, &tree.Node{Kind: tree.KindMap})
			}
			cur = cur.Items[0]
		default:
			name := s
			if s == "{}" {
				name = "k"
			}
			next, ok := cur.Get(name)
			if !ok {
				next = &tree.Node{Kind: tree.KindMap}
				cur.Set(name, tree.Pos{}, next)
			}
			cur = next
		}
	}
	return cur
}

// defaultCase builds a resource of f.Kind in which the parent of the
// defaulted field f.Path is present (dispatch conditions satisfied) and
// the field is absent. ok is false for a default not on an object member.
func defaultCase(f schemaidx.Field) (res *tree.Resource, parent []string, field string, ok bool) {
	segs := segments(f.Path)
	if len(segs) == 0 {
		return nil, nil, "", false
	}
	field = segs[len(segs)-1]
	if field == "[]" || field == "{}" {
		return nil, nil, "", false
	}
	root := &tree.Node{Kind: tree.KindMap}
	root.Set("apiVersion", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: v1})
	root.Set("kind", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: f.Kind})
	root.Set("metadata", tree.Pos{}, &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "name", Value: &tree.Node{Kind: tree.KindString, Text: "x"}}}})
	for _, d := range f.Where {
		obj := ensure(root, segments(d.Path))
		for _, c := range d.Conditions {
			if s, ok := c.Value.(string); ok {
				obj.Set(c.Member, tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: s})
			}
		}
	}
	parent = segs[:len(segs)-1]
	ensure(root, parent)
	res = &tree.Resource{ID: tree.ID{Kind: v1alpha1.Kind(f.Kind), Name: "x"}, APIVersion: v1, Root: root}
	return res, parent, field, true
}

// schemaFindings validates res with the rendered schema (stage F) and
// returns its findings as "<code> <path>: <message>", without locations,
// which materialized nodes share with their parent.
func schemaFindings(t *testing.T, views *schemaview.Set, res *tree.Resource) map[string]bool {
	t.Helper()
	ds, _ := views.Validate(res, nil)
	out := make(map[string]bool, len(ds))
	for _, d := range ds {
		out[d.Code+" "+d.Path.String()+": "+d.Message] = true
	}
	return out
}

// TestMaterializeEveryDefault is 02 test plan item 7 and the "defaults
// table equals schema markers" check: every default of the rendered schema,
// enumerated from schemaidx (02 req 10: table-free), materializes inside a
// present parent with exactly the schema's value, styled defaulted at the
// parent's position, validates against the rendered schema (stage F
// reports nothing new once the defaults are in: no pattern, range, enum
// or parent-level finding), and an explicit value is kept (01 req 36; 02
// req 9, 11).
func TestMaterializeEveryDefault(t *testing.T) {
	idx := hubIndex(t)
	reg := registry.New()
	views, err := schemaview.Embedded(schemas(t))
	if err != nil {
		t.Fatalf("schemaview.Embedded: %v", err)
	}
	seen := map[string]bool{}
	for f := range idx.FieldsSeq() {
		d, has := f.Node.Default()
		if !has {
			continue
		}
		res, parent, field, ok := defaultCase(f)
		if !ok {
			t.Errorf("%s %s: default not on an object member", f.Kind, f.Path)
			continue
		}
		seen[f.Kind+" "+f.Path] = true
		parentNode := ensure(res.Root, parent)
		parentNode.Pos = tree.Pos{File: 3, Line: 7, Column: 9}
		before := schemaFindings(t, views, res)
		Materialize(idx, reg, res)
		got, ok := parentNode.Get(field)
		if !ok {
			t.Errorf("%s %s: not materialized", f.Kind, f.Path)
			continue
		}
		for finding := range schemaFindings(t, views, res) {
			if !before[finding] {
				t.Errorf("%s %s: the materialized defaults fail the rendered schema: %s", f.Kind, f.Path, finding)
			}
		}
		want := f.Node.DefaultNode()
		if !equal(got, want) {
			t.Errorf("%s %s = %s, want %s (schema default %v)", f.Kind, f.Path, dump(got), dump(want), d)
		}
		if got.Style != tree.StyleDefaulted || got.Pos != parentNode.Pos {
			t.Errorf("%s %s: style %v pos %v, want defaulted at the parent %v", f.Kind, f.Path, got.Style, got.Pos, parentNode.Pos)
		}
		// An explicit value is kept.
		res, parent, field, _ = defaultCase(f)
		explicit := &tree.Node{Kind: tree.KindString, Text: "explicit"}
		ensure(res.Root, parent).Set(field, tree.Pos{}, explicit)
		Materialize(idx, reg, res)
		if got, _ := ensure(res.Root, parent).Get(field); got != explicit || got.Text != "explicit" {
			t.Errorf("%s %s: explicit value replaced by %s", f.Kind, f.Path, dump(got))
		}
	}
	// The check sees a default the schema rejects: the same case with the
	// admin port out of range and the State Store driver off its enum.
	for _, bad := range []struct{ path, text string }{{"spec.admin.port", "70000"}, {"spec.stateStore.driver", "nope"}} {
		res := &tree.Resource{
			ID: tree.ID{Kind: "Gateway", Name: "x"}, APIVersion: v1,
			Root: mustParse(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "x"}, "spec": {"admin": {}, "stateStore": {}}}`),
		}
		before := schemaFindings(t, views, res)
		Materialize(idx, reg, res)
		at(t, res.Root, bad.path).Text = bad.text
		added := 0
		for finding := range schemaFindings(t, views, res) {
			if !before[finding] && strings.Contains(finding, bad.path) {
				added++
			}
		}
		if added == 0 {
			t.Errorf("an invalid default at %s was not reported", bad.path)
		}
	}
	// The defaults the specs list (01 req 36, 02 req 10, R-21, R-62) are
	// among them.
	for _, want := range []string{
		"Gateway spec.admin.port", "Gateway spec.listeners[].proxyProtocol", "Gateway spec.listeners[].tls.minVersion",
		"Gateway spec.limits.maxBufferedBytes", "Gateway spec.limits.maxRequestBodyBytes", "Gateway spec.limits.maxRequestHeaderBytes",
		"Gateway spec.limits.maxResponseBodyBytes", "Gateway spec.limits.maxCompositionSteps", "Gateway spec.limits.maxPluginMemoryBytes",
		"Gateway spec.stateStore.driver", "Gateway spec.stateStore.topology", "Gateway spec.stateStore.timeout",
		"Gateway spec.telemetry.traceSampling", "Route spec.upstreams[].weight", "Policy spec.overridable",
		"Policy spec.config.key", "Policy spec.config.localOnly", "Policy spec.config.contentType", "Policy spec.config.timeout",
		"Policy spec.config.header", "Upstream spec.healthCheck.active.path",
	} {
		if !seen[want] {
			t.Errorf("default %s not enumerated", want)
		}
	}
}

// TestDefaultsEqualMarkers checks that the schema defaults Materialize
// reads are exactly the +ruralz:default markers of pkg/config/v1alpha1
// (one per (type, field)), so no default is lost or invented between the
// Go types and stage G.
func TestDefaultsEqualMarkers(t *testing.T) {
	markers := markerDefaults(t)
	var doc struct {
		Defs map[string]struct {
			Properties map[string]map[string]json.RawMessage `json:"properties"`
		} `json:"$defs"`
	}
	if err := json.Unmarshal(schema.RenderedV1alpha1(), &doc); err != nil {
		t.Fatal(err)
	}
	var fromSchema []string
	for def, d := range doc.Defs {
		for name, p := range d.Properties {
			if _, ok := p["default"]; ok {
				fromSchema = append(fromSchema, def+"."+name)
			}
		}
	}
	sort.Strings(fromSchema)
	if !slices.Equal(markers, fromSchema) {
		t.Errorf("+ruralz:default markers\n%v\nschema defaults\n%v", markers, fromSchema)
	}
	if len(markers) < 20 {
		t.Errorf("only %d markers found", len(markers))
	}
}

// markerDefaults reads <Type>.<json name> for every +ruralz:default marker
// in pkg/config/v1alpha1.
func markerDefaults(t *testing.T) []string {
	t.Helper()
	files, err := filepath.Glob("../../../pkg/config/v1alpha1/*.go")
	if err != nil || len(files) == 0 {
		t.Fatalf("glob pkg/config/v1alpha1: %v", err)
	}
	typeRE := regexp.MustCompile(`^type (\w+) struct`)
	fieldRE := regexp.MustCompile(`^\s+\w+\s+.*json:"([^",]+)`)
	var out []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := os.Open(name) //nolint:gosec // G304: a repository source file.
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		typ, pending := "", false
		for sc.Scan() {
			line := sc.Text()
			if m := typeRE.FindStringSubmatch(line); m != nil {
				typ, pending = m[1], false
				continue
			}
			if strings.Contains(line, "// +ruralz:default=") {
				pending = true
				continue
			}
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if m := fieldRE.FindStringSubmatch(line); m != nil && pending {
				out = append(out, typ+"."+m[1])
			}
			pending = false
		}
		_ = f.Close()
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
	}
	sort.Strings(out)
	return out
}

func policy(t testing.TB, f *fixture, name, specJSON string) *tree.Resource {
	t.Helper()
	return f.resource(t, "policies/"+name+".json",
		`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "`+name+`"}, "spec": `+specJSON+`}`)
}

// TestRegistryDefaults is 02 test plan item 8: for each of the 23 types,
// absent slot, failureMode and filterClass materialize the registry values
// (R-7: filterClass for every type, custom for plugin); explicit values
// are kept, even ones a later stage rejects.
func TestRegistryDefaults(t *testing.T) {
	idx := hubIndex(t)
	reg := registry.New()
	if n := len(reg.Entries()); n != 23 {
		t.Fatalf("registry has %d types, want 23", n)
	}
	for _, e := range reg.Entries() {
		t.Run(string(e.Type), func(t *testing.T) {
			f := newFixture()
			res := policy(t, f, "p1", `{"type": "`+string(e.Type)+`", "config": {}}`)
			Materialize(idx, reg, res)
			spec := at(t, res.Root, "spec")
			want := e.Defaults("p1")
			for key, value := range map[string]string{
				"slot": want.Slot, "failureMode": string(want.FailureMode), "filterClass": string(want.FilterClass),
			} {
				got, ok := spec.Get(key)
				if !ok || got.Text != value || got.Style != tree.StyleDefaulted || got.Pos != spec.Pos {
					t.Errorf("%s = %s, want %q defaulted at spec", key, dump(got), value)
				}
			}
			if e.Type == v1alpha1.PolicyTypePlugin && want.FilterClass != v1alpha1.FilterClassCustom {
				t.Errorf("plugin filterClass default %q, want custom", want.FilterClass)
			}
			if e.Slot == registry.SlotName && want.Slot != "p1" {
				t.Errorf("name-slot type slot %q, want the Policy name", want.Slot)
			}
			// Explicit values are kept.
			res = policy(t, f, "p2", `{"type": "`+string(e.Type)+`", "config": {}, "slot": "s", "failureMode": "open", "filterClass": "auth"}`)
			Materialize(idx, reg, res)
			for key, value := range map[string]string{"slot": "s", "failureMode": "open", "filterClass": "auth"} {
				if got := at(t, res.Root, "spec."+key); got.Text != value || got.Style == tree.StyleDefaulted {
					t.Errorf("explicit %s = %s, want %q", key, dump(got), value)
				}
			}
		})
	}
	// An unknown type, a non-string type and a spec that is not an object
	// get no registry defaults.
	f := newFixture()
	for _, spec := range []string{`{"type": "nope", "config": {}}`, `{"type": 3}`, `{"config": {}}`, `"x"`} {
		res := policy(t, f, "p", spec)
		before := dump(res.Root)
		Materialize(idx, reg, res)
		if s, _ := res.Root.Get("spec"); s.Kind == tree.KindMap {
			if _, ok := s.Get("failureMode"); ok {
				t.Errorf("spec %s got registry defaults: %s (was %s)", spec, dump(res.Root), before)
			}
		}
	}
	// Without a registry only schema defaults apply.
	res := policy(t, f, "p", `{"type": "ratelimit", "config": {"limits": []}}`)
	Materialize(idx, nil, res)
	if _, ok := at(t, res.Root, "spec").Get("slot"); ok {
		t.Error("slot materialized without a registry")
	}
	if got := at(t, res.Root, "spec.overridable"); !got.Bool {
		t.Errorf("overridable = %s, want true", dump(got))
	}
}

// TestMaterializeShapes covers 02 req 9 and 11, 01 req 36 and R-62:
// absent objects are not created, list entries and typed-map values get
// defaults, a Policy config gets its type's defaults, free content and
// the JSONSchemaDocument value get none, and a second call adds nothing.
func TestMaterializeShapes(t *testing.T) {
	idx := hubIndex(t)
	reg := registry.New()
	tests := []struct {
		name, src string
		// want maps a path to its value after Materialize; "-" means absent.
		want map[string]string
	}{
		{
			"gateway without optional objects",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": {"listeners": [{"name": "http", "port": 8080, "protocol": "http"}]}}`,
			map[string]string{
				"spec.limits": "-", "spec.stateStore": "-", "spec.admin": "-", "spec.telemetry": "-",
				"spec.listeners[0].proxyProtocol": "false", "spec.listeners[0].tls": "-",
			},
		},
		{
			"gateway with empty objects",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": {"listeners": [], "limits": {}, "stateStore": {}, "admin": {}, "telemetry": {}}}`,
			map[string]string{
				"spec.limits.maxRequestBodyBytes": "10485760", "spec.limits.maxRequestHeaderBytes": "65536",
				"spec.limits.maxBufferedBytes": "536870912", "spec.limits.maxCompositionSteps": "16",
				"spec.stateStore.driver": `"memory"`, "spec.stateStore.topology": `"standalone"`, "spec.stateStore.timeout": `"50ms"`,
				"spec.stateStore.url": "-", "spec.stateStore.cache": "-",
				"spec.admin.port": "9901", "spec.telemetry.traceSampling": "0.01",
			},
		},
		{
			"route upstream weights",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": {"match": {}, "upstreams": [{"name": "a"}, {"name": "b", "weight": 3}]}}`,
			map[string]string{"spec.upstreams[0].weight": "1", "spec.upstreams[1].weight": "3", "spec.timeout": "-"},
		},
		{
			"upstream endpoints and health check",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "a:1"}], "healthCheck": {"active": {}}}}`,
			map[string]string{"spec.endpoints[0].weight": "1", "spec.healthCheck.active.path": `"/"`, "spec.retries": "-", "spec.circuitBreaker": "-"},
		},
		{
			"quota config by dispatch",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "q"}, "spec": {"type": "quota", "config": {"consumerQuota": "c", "extra": {"key": 1}}}}`,
			map[string]string{
				"spec.config.key": `"consumer.name"`, "spec.overridable": "true", "spec.failureMode": `"open"`,
				"spec.slot": `"q"`, "spec.filterClass": `"admission"`, "spec.config.extra": `{"key":1}`,
			},
		},
		{
			"transform contentType",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "t"}, "spec": {"type": "transform.response", "config": {}}}`,
			map[string]string{"spec.config.contentType": `"application/json"`, "spec.slot": `"t"`},
		},
		{
			"oauth2 timeout",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "o"}, "spec": {"type": "auth.upstream-oauth2", "config": {"tokenUrl": "https://x"}}}`,
			map[string]string{"spec.config.timeout": `"2s"`, "spec.slot": `"upstream-auth"`, "spec.filterClass": `"upstream-auth"`},
		},
		{
			"api-key header",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "k"}, "spec": {"type": "auth.api-key", "config": {}}}`,
			map[string]string{"spec.config.header": `"x-api-key"`, "spec.slot": `"auth"`, "spec.failureMode": `"closed"`},
		},
		{
			"ratelimit localOnly",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "rl"}, "spec": {"type": "ratelimit", "config": {"limits": [{"requests": 1, "window": "1s"}]}}}`,
			map[string]string{"spec.config.localOnly": "false", "spec.config.limits[0]": `{"requests":1,"window":"1s"}`},
		},
		{
			"json schema document is data",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "v"}, "spec": {"type": "validation.json-schema",
			  "config": {"schema": {"type": "object", "properties": {"limits": {}, "stateStore": {}, "config": {"type": "quota"}}}}}}`,
			map[string]string{
				"spec.config.schema": `{"type":"object","properties":{"limits":{},"stateStore":{},"config":{"type":"quota"}}}`,
				"spec.slot":          `"validation"`,
			},
		},
		{
			"plugin config and configSchema are free",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "pl"}, "spec": {"image": "x", "phases": [], "configSchema": {"limits": {}}}}`,
			map[string]string{"spec.configSchema": `{"limits":{}}`, "spec.limits": "-"},
		},
		{
			"labels typed map",
			`{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r", "labels": {"a": "b"}}, "spec": {"match": {}}}`,
			map[string]string{"metadata.labels": `{"a":"b"}`},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res := newFixture().resource(t, "r.json", tc.src)
			n := Materialize(idx, reg, res)
			for path, want := range tc.want {
				got, ok := lookup(res.Root, path)
				switch {
				case want == "-" && ok:
					t.Errorf("%s = %s, want absent", path, dump(got))
				case want != "-" && !ok:
					t.Errorf("%s absent, want %s", path, want)
				case want != "-" && dump(got) != want:
					t.Errorf("%s = %s, want %s", path, dump(got), want)
				}
			}
			before := dump(res.Root)
			if again := Materialize(idx, reg, res); again != 0 || dump(res.Root) != before {
				t.Errorf("second Materialize added %d (first %d): %s", again, n, dump(res.Root))
			}
		})
	}
	// Nil arguments and unknown kinds do nothing.
	if Materialize(nil, reg, &tree.Resource{}) != 0 || Materialize(idx, reg, nil) != 0 ||
		Materialize(idx, reg, &tree.Resource{ID: tree.ID{Kind: "Nope"}, Root: mustParse(t, `{}`)}) != 0 {
		t.Error("Materialize on nothing added defaults")
	}
}

// TestMaterializeLargeObject covers the index Materialize builds for an
// object with many members: defaults are added once and the authored
// members stay.
func TestMaterializeLargeObject(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "q"}, "spec": {"type": "quota", "config": {"consumerQuota": "c"`)
	for i := range 100 {
		b.WriteString(`, "u` + string(rune('a'+i%26)) + string(rune('a'+i/26)) + `": 1`)
	}
	b.WriteString(`, "key": "request.host"}}}`)
	res := newFixture().resource(t, "q.json", b.String())
	Materialize(hubIndex(t), registry.New(), res)
	cfg := at(t, res.Root, "spec.config")
	if len(cfg.Members) != 102 {
		t.Errorf("config has %d members, want 102", len(cfg.Members))
	}
	if k := at(t, cfg, "key"); k.Text != "request.host" {
		t.Errorf("key = %s, want the authored value", dump(k))
	}
}
