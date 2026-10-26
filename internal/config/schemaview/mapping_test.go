// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// finding is the position-independent part of a diagnostic.
type finding struct {
	code, path, msg, hint string
}

func findings(l diag.List) []finding {
	out := make([]finding, 0, len(l))
	for _, d := range l {
		out = append(out, finding{d.Code, d.Path.String(), d.Message, d.Hint})
	}
	return out
}

// envelope wraps a spec in a resource of kind named "x".
func envelope(kind, spec string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "` + kind + `", "metadata": {"name": "x"}, "spec": ` + spec + `}`
}

// policy wraps a config in a Policy of type typ.
func policy(typ, config string) string {
	return envelope("Policy", `{"type": "`+typ+`", "config": `+config+`}`)
}

// gateway wraps Gateway spec members after one valid listener.
func gateway(members string) string {
	return envelope("Gateway", `{"listeners": [{"name": "http", "port": 8080, "protocol": "http"}]`+members+`}`)
}

// TestErrorMapping checks the mapping of each error kind of the rendered
// view to its code, key-aware path, message and hint.
func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		src  string
		want []finding
	}{
		// additionalProperties: RZ-CFG-006 per unknown member, nearest hint.
		{
			"unknown field hint", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "timout": "1s"}`),
			[]finding{{CodeUnknownField, "spec.timout", `unknown field "timout"`, `did you mean "timeout"?`}},
		},
		{
			"unknown fields each", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "b": 1, "a": 2}`),
			[]finding{{CodeUnknownField, "spec.b", `unknown field "b"`, ""}, {CodeUnknownField, "spec.a", `unknown field "a"`, `did you mean "ai"?`}},
		},
		{
			"metadata.namespace", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "namespace": "n"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
			[]finding{{CodeUnknownField, "metadata.namespace", `unknown field "namespace": Bundles have no namespace`, ""}},
		},
		{
			"top-level status", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}, "status": {}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
			[]finding{{CodeUnknownField, "status", `unknown field "status": only Ruralz Control writes status`, ""}},
		},
		// The two dedicated messages belong to metadata.namespace and a
		// top-level status only: the same names anywhere else are
		// ordinary unknown fields with the nearest-name hint
		// (TestLeafMessages has the cases with a hint).
		{
			"status below the root", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "status": {}}`),
			[]finding{{CodeUnknownField, "spec.status", `unknown field "status"`, ""}},
		},
		{
			"status in a keyed entry", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1", "status": 1}]}`),
			[]finding{{CodeUnknownField, "spec.endpoints[address=a:1].status", `unknown field "status"`, ""}},
		},
		{
			"namespace below the root", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "namespace": "n"}`),
			[]finding{{CodeUnknownField, "spec.namespace", `unknown field "namespace"`, ""}},
		},
		{
			"status and namespace in a keyed entry", envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "status": 1, "namespace": 2}]}`),
			[]finding{
				{CodeUnknownField, "spec.listeners[name=a].status", `unknown field "status"`, ""},
				{CodeUnknownField, "spec.listeners[name=a].namespace", `unknown field "namespace"`, ""},
			},
		},
		{
			"$patch in a base file", envelope("Route", `{"$patch": "replace", "match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}`),
			[]finding{{CodeUnknownField, "spec.$patch", "$patch is valid only in overlays", ""}},
		},
		{
			"closed config", policy("auth.basic", `{"realm": "x"}`),
			[]finding{{CodeUnknownField, "spec.config.realm", `unknown field "realm"`, ""}},
		},
		{
			"closed config hint", policy("authz.ip", `{"alow": ["10.0.0.0/8"], "deny": []}`),
			[]finding{{CodeUnknownField, "spec.config.alow", `unknown field "alow"`, `did you mean "allow"?`}},
		},
		// enum: nearest value.
		{
			"enum hint", policy("rateLimit", `{}`),
			[]finding{{CodeSchema, "spec.type", "is not one of the 23 allowed values", `did you mean "ratelimit"?`}},
		},
		{
			"short enum listed", envelope("Upstream", `{"protocol": "htp", "endpoints": [{"address": "a:1"}]}`),
			[]finding{{CodeSchema, "spec.protocol", `must be one of "http", "grpc", "graphql", "websocket", "kafka", "nats", "mqtt", "ai"`, `did you mean "http"?`}},
		},
		// required: at the parent, naming the field.
		{
			"required spec", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x"}}`,
			[]finding{{CodeSchema, "", "missing required field spec", ""}},
		},
		{
			"required name", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
			[]finding{{CodeSchema, "metadata", "missing required field name", ""}},
		},
		// type: every allowed type, quote hint for typed plain scalars.
		{
			"type", gateway(`, "admin": {"port": "abc"}`),
			[]finding{{CodeSchema, "spec.admin.port", "must be an integer, not a string", ""}},
		},
		{
			"type quote hint", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": 777}]}`),
			[]finding{{CodeSchema, "spec.endpoints[address=777].address", "must be a string, not an integer", "quote the value to make it a string"}},
		},
		{
			"type in keyed list", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u", "weight": "x"}]}`),
			[]finding{{CodeSchema, "spec.upstreams[name=u].weight", "must be an integer, not a string", ""}},
		},
		{
			"type array object or boolean", policy("validation.json-schema", `{"schema": "abc"}`),
			[]finding{{CodeSchema, "spec.config.schema", "must be an object or a boolean, not a string", ""}},
		},
		{
			"IntOrString anyOf types", envelope("Upstream", `{"protocol": "http", "discovery": {"type": "dns", "service": "a.example", "port": true}}`),
			[]finding{{CodeSchema, "spec.discovery.port", "must be an integer or a string, not a boolean", "quote the value to make it a string"}},
		},
		{
			"IntOrString valid", envelope("Upstream", `{"protocol": "http", "discovery": {"type": "dns", "service": "a.example", "port": "http"}}`),
			nil,
		},
		// pattern, minimum/maximum, minItems, minProperties, maxLength.
		{
			"pattern syntax name", `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "Bad_Name"}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
			[]finding{{CodeSchema, "metadata.name", "must be an RFC 1123 label of 1 to 63 characters: lower-case letters, digits and '-', starting and ending with a letter or digit", ""}},
		},
		{
			"OTLP endpoint syntax", gateway(`, "telemetry": {"otlp": {"endpoint": "http://user:pass@collector:4317/v1"}}`),
			[]finding{{CodeSchema, "spec.telemetry.otlp.endpoint", "must be an http or https URL with a host and an optional port, no path other than /, and no user information, query or fragment", ""}},
		},
		{
			"maximum", gateway(`, "admin": {"port": 70000}`),
			[]finding{{CodeSchema, "spec.admin.port", "must be at most 65535", ""}},
		},
		{
			"minimum", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u", "weight": -1}]}`),
			[]finding{{CodeSchema, "spec.upstreams[name=u].weight", "must be at least 0", ""}},
		},
		{
			"minItems", envelope("Gateway", `{"listeners": []}`),
			[]finding{{CodeSchema, "spec.listeners", "must have at least 1 entry", ""}},
		},
		{
			"minProperties", envelope("Route", `{"match": {}, "upstreams": [{"name": "u"}]}`),
			[]finding{{CodeSchema, "spec.match", "must have at least 1 member", ""}},
		},
		{
			"maxLength", policy("transform.request", `{"replace": [{"pattern": "`+strings.Repeat("a", 1025)+`", "replacement": ""}]}`),
			[]finding{{CodeSchema, "spec.config.replace[0].pattern", "must be at most 1024 characters long", ""}},
		},
		{
			"minLength", envelope("Consumer", `{"credentials": {"jwt": [{"issuer": "i", "subject": ""}]}}`),
			[]finding{{CodeSchema, "spec.credentials.jwt[issuer=i].subject", "must not be empty", ""}},
		},
		// Combination keywords generated by exactlyOneOf, atMostOneOf, atLeastOneOf.
		{
			"exactlyOneOf both", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "composition": {"mode": "sequential", "steps": [{"name": "s", "upstream": "u", "path": "/a", "pathExpression": "'/b'"}]}}`),
			[]finding{{CodeSchema, "spec.composition.steps[name=s]", "exactly one of path, pathExpression is allowed, found path and pathExpression", ""}},
		},
		{
			"exactlyOneOf none", envelope("Route", `{"match": {"path": {"prefix": "/"}}}`),
			[]finding{{CodeSchema, "spec", "exactly one of upstreams, composition is required", ""}},
		},
		{
			"atMostOneOf", envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "discovery": {"type": "dns", "service": "a.example"}}`),
			[]finding{{CodeSchema, "spec", "at most one of endpoints, discovery is allowed, found endpoints and discovery", ""}},
		},
		{
			"atLeastOneOf", policy("authz.ip", `{}`),
			[]finding{{CodeSchema, "spec.config", "at least one of allow, deny is required", ""}},
		},
		// Duration and ByteSize use the CM syntax text.
		{
			"duration", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "5 sec"}`),
			[]finding{{CodeSchema, "spec.timeout", "must be a Go duration such as 50ms", ""}},
		},
		{
			"duration type", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": 5}`),
			[]finding{{CodeSchema, "spec.timeout", "must be a Go duration such as 50ms", ""}},
		},
		{
			"bytesize pattern", gateway(`, "limits": {"maxRequestBodyBytes": "10MB"}`),
			[]finding{{CodeSchema, "spec.limits.maxRequestBodyBytes", "must be an integer or quantity such as 10Mi", ""}},
		},
		{
			"bytesize negative", gateway(`, "limits": {"maxRequestBodyBytes": -1}`),
			[]finding{{CodeSchema, "spec.limits.maxRequestBodyBytes", "must be an integer or quantity such as 10Mi", ""}},
		},
		{
			"bytesize type", gateway(`, "limits": {"maxRequestBodyBytes": true}`),
			[]finding{{CodeSchema, "spec.limits.maxRequestBodyBytes", "must be an integer or quantity such as 10Mi", ""}},
		},
		// x-ruralz-secret positions: RZ-CFG-012, never the value.
		{
			"secret literal", gateway(`, "stateStore": {"driver": "redis", "url": "rediss://u:p@h:6380"}`),
			[]finding{{CodeLiteral, "spec.stateStore.url", "a literal value is not allowed in a secret field; reference the secret as {secretRef: {provider, name}}", ""}},
		},
		{
			"secret object", gateway(`, "stateStore": {"driver": "redis", "url": {"value": "x"}}`),
			[]finding{{CodeLiteral, "spec.stateStore.url", "invalid secret reference: the field accepts only secretRef; the field needs secretRef; write it as {secretRef: {provider, name}}", ""}},
		},
		{
			"secretRef literal", envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": "abc"}]}}`),
			[]finding{{CodeLiteral, "spec.credentials.apiKeys[name=k].secretRef", "a literal value is not allowed in a secret field; reference the secret as {provider, name}", ""}},
		},
		{
			"secretRef invalid", envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": {"provider": "vaults", "name": ""}}]}}`),
			[]finding{{CodeLiteral, "spec.credentials.apiKeys[name=k].secretRef", `invalid secret reference: name must not be empty; provider must be one of "env", "file", "kubernetes", "vault"; write it as {provider, name}`, ""}},
		},
		{
			"nested secretRef", gateway(`, "stateStore": {"driver": "redis", "url": {"secretRef": {"provider": "env"}}}`),
			[]finding{{CodeLiteral, "spec.stateStore.url", "invalid secret reference: secretRef needs name; write it as {secretRef: {provider, name}}", ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := findings(validateJSON(t, tc.src))
			if len(got) != len(tc.want) {
				t.Fatalf("Validate() = %+v, want %+v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("finding %d = %+v, want %+v", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestUniqueness checks keyed-list and set uniqueness at the second entry
// with the first as the related location.
func TestUniqueness(t *testing.T) {
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"}, "spec": {
  "listeners": [
    {"name": "https", "port": 8443, "protocol": "https"},
    {"name": "https", "port": 9443, "protocol": "https"}
  ],
  "trustedProxies": ["10.0.0.0/8", "10.0.0.0/8"],
  "policies": [{"name": "p"}, {"name": "p"}]
}}`
	got := validateJSON(t, src)
	want := []struct {
		f       finding
		line    int
		related diag.Related
	}{
		{
			finding{CodeSchema, "spec.listeners[name=https]", `duplicate entry: name "https" is already used by an earlier entry`, ""},
			4,
			diag.Related{Location: diag.Location{File: "r.json", Line: 3, Column: 14}, Message: "first entry at"},
		},
		{
			finding{CodeSchema, "spec.trustedProxies[item=10.0.0.0/8]", `duplicate element "10.0.0.0/8" in a set`, ""},
			6,
			diag.Related{Location: diag.Location{File: "r.json", Line: 6, Column: 22}, Message: "first element at"},
		},
		{
			finding{CodeSchema, "spec.policies[name=p]", `duplicate entry: name "p" is already used by an earlier entry`, ""},
			7,
			diag.Related{Location: diag.Location{File: "r.json", Line: 7, Column: 25}, Message: "first entry at"},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("Validate() = %s", texts(got))
	}
	for i, w := range want {
		if f := findings(got[i : i+1])[0]; f != w.f || got[i].Line != w.line || len(got[i].Related) != 1 || got[i].Related[0] != w.related {
			t.Errorf("finding %d = %s, want %+v at line %d related %+v", i, texts(got[i:i+1]), w.f, w.line, w.related)
		}
	}
}

// TestDuplicateKeysByValue compares keyed-list keys by canonical JSON:
// the string "1" and the integer 1 are different keys, so a mistyped key
// gets its type error and no spurious duplicate, while 1 and 1.0 are one
// key.
func TestDuplicateKeysByValue(t *testing.T) {
	got := findings(validateJSON(t, envelope("Gateway", `{"listeners": [
  {"name": "1", "port": 8080, "protocol": "http"},
  {"name": 1, "port": 8081, "protocol": "http"}]}`)))
	want := []finding{{CodeSchema, "spec.listeners[name=1].name", "must be a string, not an integer", "quote the value to make it a string"}}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("Validate(\"1\" and 1) = %+v, want %+v", got, want)
	}
	got = findings(validateWith(t, syntheticView(t), `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {"name": "a"},
  "spec": {"keyed": [{"name": "x"}, {"name": 1}, {"name": "x"}, {"name": 1.0}]}}`))
	want = []finding{
		{CodeSchema, "spec.keyed[name=1].name", "must be a string, not an integer", "quote the value to make it a string"},
		{CodeSchema, "spec.keyed[name=x]", `duplicate entry: name "x" is already used by an earlier entry`, ""},
		{CodeSchema, "spec.keyed[name=1.0]", "duplicate entry: name 1 is already used by an earlier entry", ""},
		{CodeSchema, "spec.keyed[name=1.0].name", "must be a string, not a number", "quote the value to make it a string"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("Validate(numbers) = %+v, want %+v", got, want)
	}
}

// TestSetNumbersByValue compares set elements by value, as the canonical
// form does: 1.50 and 1.5 are one element.
func TestSetNumbersByValue(t *testing.T) {
	got := findings(validateWith(t, syntheticView(t), `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {"name": "a"}, "spec": {"set": [1.50, 1.5, {"x": 1}, {"x": 1.0}]}}`))
	want := []finding{
		{CodeSchema, "spec.set[item=1.5]", "duplicate element 1.5 in a set", ""},
		{CodeSchema, `spec.set[item={"x":1}]`, `duplicate element {"x":1} in a set`, ""},
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("Validate() = %+v, want %+v", got, want)
	}
}

// TestReservedPrefix checks label and annotation keys under
// ruralz.io/, with the key's position.
func TestReservedPrefix(t *testing.T) {
	src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x",
  "labels": {"ruralz.io/team": "a", "team": "b", "Ruralz.IO/x": "c"},
  "annotations": {"ruralz.io/conversion-data": "{}", "ruralz.io/patch": "delete", "shop.example/owner": "c"}},
  "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`
	got := validateJSON(t, src)
	want := []finding{
		{CodeSchema, `metadata.labels["ruralz.io/team"]`, `label key "ruralz.io/team" uses the reserved prefix ruralz.io/`, ""},
		{CodeSchema, `metadata.annotations["ruralz.io/patch"]`, `annotation key "ruralz.io/patch" uses the reserved prefix ruralz.io/`, "only ruralz.io/conversion-data may use the prefix"},
	}
	f := findings(got)
	if len(f) != len(want) {
		t.Fatalf("Validate() = %s", texts(got))
	}
	for i := range want {
		if f[i] != want[i] {
			t.Errorf("finding %d = %+v, want %+v", i, f[i], want[i])
		}
	}
	if got[0].Line != 2 || got[0].Column != 14 {
		t.Errorf("first finding at %d:%d, want the key at 2:14", got[0].Line, got[0].Column)
	}
}

// TestReservedPrefixExact checks that the reserved prefix and the
// conversion-data exemption compare bytes exactly, as the configuration
// model writes them: a key that differs from ruralz.io/ in case, or that
// only contains it, is an ordinary key.
func TestReservedPrefixExact(t *testing.T) {
	for _, tc := range []struct {
		name, field, key string
		// reserved is whether the key is RZ-CFG-005.
		reserved bool
	}{
		{"label with the prefix", "labels", "ruralz.io/team", true},
		{"label that is the prefix alone", "labels", "ruralz.io/", true},
		{"label in mixed case", "labels", "Ruralz.IO/x", false},
		{"label in upper case", "labels", "RURALZ.IO/team", false},
		{"label without the slash", "labels", "ruralz.io", false},
		{"label with the prefix inside", "labels", "shop.ruralz.io/team", false},
		{"label with a longer domain", "labels", "ruralz.iox/team", false},
		{"label named conversion-data", "labels", "ruralz.io/conversion-data", true},
		{"annotation with the prefix", "annotations", "ruralz.io/patch", true},
		{"annotation conversion-data", "annotations", "ruralz.io/conversion-data", false},
		{"annotation conversion-data in mixed case", "annotations", "Ruralz.io/conversion-data", false},
		{"annotation conversion-data in a cased name", "annotations", "ruralz.io/Conversion-Data", true},
		{"annotation conversion-data with a suffix", "annotations", "ruralz.io/conversion-data2", true},
		{"annotation in upper case", "annotations", "RURALZ.IO/patch", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "` + tc.field + `": {` + quote(tc.key) + `: "v"}},
  "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`
			got := validateJSON(t, src)
			if !tc.reserved {
				if len(got) != 0 {
					t.Errorf("Validate(%s %q) = %s, want no finding", tc.field, tc.key, texts(got))
				}
				return
			}
			if len(got) != 1 || got[0].Code != CodeSchema || !strings.Contains(got[0].Message, "uses the reserved prefix ruralz.io/") {
				t.Errorf("Validate(%s %q) = %s, want one RZ-CFG-005 for the reserved prefix", tc.field, tc.key, texts(got))
			}
		})
	}
}

// TestPositions checks the source positions of each finding kind: the
// value, the unknown key, the parent object of a missing field.
func TestPositions(t *testing.T) {
	src := `{
  "apiVersion": "ruralz/v1alpha1",
  "kind": "Upstream",
  "metadata": {"name": "u"},
  "spec": {
    "protocol": "htp",
    "endpoints": [{"address": "a:1"}, {"weight": 2}],
    "timout": "1s",
    "retries": {}
  }
}`
	got := validateJSON(t, src)
	want := []string{
		`r.json:6:17 error RZ-CFG-005 Upstream/u spec.protocol: must be one of "http", "grpc", "graphql", "websocket", "kafka", "nats", "mqtt", "ai" (did you mean "http"?)`,
		`r.json:7:39 error RZ-CFG-005 Upstream/u spec.endpoints[1]: missing required field address`,
		`r.json:8:5 error RZ-CFG-006 Upstream/u spec.timout: unknown field "timout" (did you mean "timeout"?)`,
	}
	if strings.TrimSpace(texts(got)) != strings.Join(want, "\n") {
		t.Errorf("Validate() =\n%s\nwant\n%s", texts(got), strings.Join(want, "\n"))
	}
}

// TestDefaultedNodePointsAtParent checks that a finding on a node without
// a source position (a materialized default) points at the nearest
// positioned ancestor.
func TestDefaultedNodePointsAtParent(t *testing.T) {
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", gateway(`, "admin": {"port": 1}`))
	admin, _ := res.Root.At(diag.Path{diag.Field("spec"), diag.Field("admin")})
	port, _ := admin.Get("port")
	port.Kind, port.Text, port.Style, port.Pos = tree.KindString, "x", tree.StyleDefaulted, tree.Pos{}
	got := testView(t).Validate(res, files)
	if len(got) != 1 || got[0].Path.String() != "spec.admin.port" || got[0].Line != 1 || got[0].Column != int(admin.Pos.Column) {
		t.Errorf("Validate() = %s, want spec.admin.port at the admin object 1:%d", texts(got), admin.Pos.Column)
	}
}

// TestSecretValueNeverEchoed checks that no form of a diagnostic contains
// a literal from a secret position, an unknown member name inside a
// secret, or a value from a nested secretRef.
func TestSecretValueNeverEchoed(t *testing.T) {
	const sentinel = "SENTINEL-7f3a"
	for _, src := range []string{
		envelope("Gateway", `{"listeners": [{"name": "h", "port": 1, "protocol": "https", "tls": {"certificates": [{"name": "c", "certificate": {"secretRef": {"provider": "file", "name": "/c"}}, "privateKey": "-----BEGIN PRIVATE KEY-----`+sentinel+`"}]}}]}`),
		gateway(`, "stateStore": {"driver": "redis", "url": {"` + sentinel + `": "` + sentinel + `"}}`),
		gateway(`, "stateStore": {"driver": "redis", "url": {"secretRef": {"provider": "` + sentinel + `", "name": "` + sentinel + `", "` + sentinel + `": 1}}}`),
		envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": ["`+sentinel+`"]}]}}`),
		envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "secretRef": {"provider": "env", "name": 7, "`+sentinel+`": "`+sentinel+`"}}]}}`),
	} {
		got := validateJSON(t, src)
		if len(got) == 0 {
			t.Errorf("Validate() = no findings for %s", src)
			continue
		}
		var js strings.Builder
		if err := diag.WriteJSON(&js, got); err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{texts(got), js.String()} {
			if strings.Contains(out, sentinel) {
				t.Errorf("diagnostic output contains the secret sentinel:\n%s", out)
			}
		}
		for _, d := range got {
			if d.Code != CodeLiteral {
				t.Errorf("finding %s, want only %s", texts(diag.List{d}), CodeLiteral)
			}
		}
	}
}

// TestSecretPerPosition checks that each x-ruralz-secret position with a
// non-conforming value gets its own RZ-CFG-012 at its own location with
// only its own reasons, even when two positions share a path text: the
// secretRef of two keyed entries with one key, in both orders, and of
// three entries: grouping findings by path text would send the second
// entry's reasons to the first entry's location, or lose them.
func TestSecretPerPosition(t *testing.T) {
	const (
		obj     = `{"provider": "env", "name": "A", "x": 1}`
		literal = `"LITERAL"`
		objMsg  = "invalid secret reference: the field accepts only key, name or provider; write it as {provider, name}"
		litMsg  = "a literal value is not allowed in a secret field; reference the secret as {provider, name}"
		path    = "spec.credentials.apiKeys[name=k].secretRef"
	)
	for _, tc := range []struct {
		name  string
		refs  []string
		wants []string
	}{
		{"object then literal", []string{obj, literal}, []string{objMsg, litMsg}},
		{"literal then object", []string{literal, obj}, []string{litMsg, objMsg}},
		{"three entries", []string{obj, literal, obj}, []string{objMsg, litMsg, objMsg}},
		{"two literals", []string{literal, literal}, []string{litMsg, litMsg}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entries []string
			for _, ref := range tc.refs {
				entries = append(entries, `{"name": "k", "secretRef": `+ref+`}`)
			}
			src := envelope("Consumer", `{"credentials": {"apiKeys": [`+strings.Join(entries, ", ")+`]}}`)
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", src)
			keys, _ := res.Root.Get("spec")
			keys, _ = keys.Get("credentials")
			keys, _ = keys.Get("apiKeys")
			var secrets []diag.Diagnostic
			for _, d := range testView(t).Validate(res, files) {
				if d.Code == CodeLiteral {
					secrets = append(secrets, d)
				}
			}
			if len(secrets) != len(tc.refs) {
				t.Fatalf("Validate() = %d RZ-CFG-012 findings, want one per entry (%d):\n%s", len(secrets), len(tc.refs), texts(secrets))
			}
			for i, d := range secrets {
				ref, _ := keys.Items[i].Get("secretRef")
				if d.Path.String() != path || d.Column != int(ref.Pos.Column) || d.Message != tc.wants[i] {
					t.Errorf("finding %d = %s at column %d, want %q at column %d (entry %d)", i, d.Message, d.Column, tc.wants[i], ref.Pos.Column, i)
				}
			}
		})
	}
}

// TestLeafMessages words the kinds the rendered view never produces
// through a synthetic schema (every error kind maps to a registered
// code).
func TestLeafMessages(t *testing.T) {
	v := syntheticView(t)
	for _, tc := range []struct {
		spec string
		want []finding
	}{
		{`{"count": 7}`, []finding{{CodeSchema, "spec.count", "must be a multiple of 5", ""}}},
		{`{"count": 0}`, []finding{{CodeSchema, "spec.count", "must be greater than 0", ""}}},
		{`{"count": 100}`, []finding{{CodeSchema, "spec.count", "must be less than 100", ""}}},
		{`{"ratio": 0.25}`, []finding{{CodeSchema, "spec.ratio", "must be at least 0.5", ""}}},
		{`{"tags": ["a", "a"]}`, []finding{{CodeSchema, "spec.tags", "entries 0 and 1 are equal", ""}}},
		{`{"tags": ["a", "b", "c"]}`, []finding{{CodeSchema, "spec.tags", "must have at most 2 entries", ""}}},
		{`{"attrs": {"a": 1, "b": 2}}`, []finding{{CodeSchema, "spec.attrs", "must have at most 1 member", ""}}},
		{`{"never": 1}`, []finding{{CodeSchema, "spec.never", "is not allowed here", ""}}},
		{`{"either": 2}`, []finding{{CodeSchema, "spec.either", "must be at least 5", ""}}},
		{`{"either": []}`, []finding{{CodeSchema, "spec.either", "must be a string or an integer, not an array", ""}}},
		{`{"overlap": 5}`, []finding{{CodeSchema, "spec.overlap", "matches more than one alternative; exactly one is allowed", ""}}},
		{`{"notString": "s"}`, []finding{{CodeSchema, "spec.notString", "matches a form that is not allowed here", ""}}},
		// atMostOneOf over three fields is not with an anyOf of required
		// pairs (schemagen fieldCombination); the message names every
		// field in schema order.
		{`{"atMost": {"a": 1, "c": 2}}`, []finding{{CodeSchema, "spec.atMost", "at most one of a, b, c is allowed, found a and c", ""}}},
		{`{"atMost": {"c": 1, "b": 2, "a": 3}}`, []finding{{CodeSchema, "spec.atMost", "at most one of a, b, c is allowed, found a, b and c", ""}}},
		{`{"atMost": {"b": 1, "d": 2}}`, nil},
		{`{"notAll": {"a": 1, "b": 2, "c": 3}}`, []finding{{CodeSchema, "spec.notAll", "matches a form that is not allowed here", ""}}},
		{`{"anyNum": "abcd"}`, []finding{{CodeSchema, "spec.anyNum", "must be at most 3 characters long", ""}}},
		{`{"anyNum": true}`, []finding{{CodeSchema, "spec.anyNum", "must be an integer or a string, not a boolean", "quote the value to make it a string"}}},
		{`{"anyNum": 1}`, []finding{{CodeSchema, "spec.anyNum", "must be at least 10", ""}}},
		{`{"fixed": {"a": 2}}`, []finding{{CodeSchema, "spec.fixed", `must be {"a":1}`, ""}}},
		{`{"has": ["y"]}`, []finding{{CodeSchema, "spec.has", "must contain a matching entry", ""}}},
		{`{"hasTwo": ["x"]}`, []finding{{CodeSchema, "spec.hasTwo", "must contain at least 2 matching entries", ""}}},
		{`{"hasTwo": ["x", "x", "x", "x"]}`, []finding{{CodeSchema, "spec.hasTwo", "must contain at most 3 matching entries", ""}}},
		{`{"dep": {"a": 1}}`, []finding{{CodeSchema, "spec.dep", "a requires b", ""}}},
		{`{"color": "rde"}`, []finding{{CodeSchema, "spec.color", `must be one of "red", "green", 3`, `did you mean "red"?`}}},
		{`{"secret": {"ref": {"id": "BAD", "n": "x"}, "extra": 1}}`, []finding{{
			CodeLiteral, "spec.secret",
			"invalid secret reference: ref.id does not match the required pattern; ref.n must be an integer; the field accepts only ref; write it as {ref: {id}}", "",
		}}},
		{`{"secret": "literal"}`, []finding{{CodeLiteral, "spec.secret", "a literal value is not allowed in a secret field; reference the secret as {ref: {id}}", ""}}},
		// Every keyword inside a secret gives a reason worded from the schema
		// alone, never the value: const, maxLength and minimum take the
		// default reason; not and a oneOf that several branches match report
		// no cause, so a reason quoting the value would fail here.
		{`{"secret": {"ref": {"id": "a", "c": "SENTINEL-c"}}}`, []finding{{CodeLiteral, "spec.secret", "invalid secret reference: ref.c is not valid; write it as {ref: {id}}", ""}}},
		{`{"secret": {"ref": {"id": "a", "m": "SENTINEL-m"}}}`, []finding{{CodeLiteral, "spec.secret", "invalid secret reference: ref.m is not valid; write it as {ref: {id}}", ""}}},
		{`{"secret": {"ref": {"id": "a", "min": 1}}}`, []finding{{CodeLiteral, "spec.secret", "invalid secret reference: ref.min is not valid; write it as {ref: {id}}", ""}}},
		{`{"secret": {"ref": {"id": "a", "nt": "SENTINEL-nt"}}}`, []finding{{CodeLiteral, "spec.secret", "invalid secret reference: ref.nt is not valid; write it as {ref: {id}}", ""}}},
		{`{"secret": {"ref": {"id": "a", "one": 7}}}`, []finding{{CodeLiteral, "spec.secret", "invalid secret reference: ref.one is not valid; write it as {ref: {id}}", ""}}},
		{`{"secret": {"ref": {"id": "a", "c": "SENTINEL-c", "m": "SENTINEL-m", "min": 1, "nt": "SENTINEL-nt", "one": 7}}}`, []finding{{
			CodeLiteral, "spec.secret",
			"invalid secret reference: ref.c is not valid; ref.m is not valid; ref.min is not valid; ref.nt is not valid; ref.one is not valid; write it as {ref: {id}}", "",
		}}},
		// The dedicated namespace and status messages apply at metadata
		// and at the resource root only; a closed metadata object below
		// the root and a status below the root get the ordinary message
		// and the nearest-name hint.
		{`{"metadata": {"name": "n", "namespace": "x"}}`, []finding{{CodeUnknownField, "spec.metadata.namespace", `unknown field "namespace"`, `did you mean "namespaces"?`}}},
		{`{"status": {}}`, []finding{{CodeUnknownField, "spec.status", `unknown field "status"`, `did you mean "stats"?`}}},
		{`{"opaque": {"k": "v"}}`, []finding{{CodeLiteral, "spec.opaque", "invalid secret reference: a nested value must be an integer; write it as a secretRef", ""}}},
		{`{"keyed": [{"name": "a", "v": "x"}, {"name": "a"}]}`, []finding{
			{CodeSchema, "spec.keyed[name=a].v", "must be an integer, not a string", ""},
			{CodeSchema, "spec.keyed[name=a]", `duplicate entry: name "a" is already used by an earlier entry`, ""},
		}},
		{`{"free": {"anything": [1, {"x": null}]}}`, nil},
	} {
		got := findings(validateWith(t, v, `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {"name": "a"}, "spec": `+tc.spec+`}`))
		if len(got) != len(tc.want) {
			t.Errorf("spec %s: Validate() = %+v, want %+v", tc.spec, got, tc.want)
			continue
		}
		for i := range got {
			if got[i] != tc.want[i] {
				t.Errorf("spec %s: finding %d = %+v, want %+v", tc.spec, i, got[i], tc.want[i])
			}
		}
	}
}

// TestUnknownKind validates a resource whose kind the view does not
// dispatch against the root schema, which names the kinds.
func TestUnknownKind(t *testing.T) {
	got := findings(validateJSON(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Rout", "metadata": {"name": "x"}, "spec": {}}`))
	if len(got) != 1 || got[0].code != CodeSchema || got[0].path != "kind" || got[0].hint != `did you mean "Route"?` {
		t.Errorf("Validate() = %+v, want one RZ-CFG-005 at kind hinting Route", got)
	}
	got = findings(validateJSON(t, `[{"kind": "Route"}]`))
	if len(got) != 1 || got[0].msg != "must be an object, not an array" || got[0].path != "" {
		t.Errorf("Validate(list document) = %+v, want one type finding at the root", got)
	}
}

// TestMessagesWithoutSchema covers the wording helpers on kinds and inputs
// the validator does not produce for these schemas.
func TestMessagesWithoutSchema(t *testing.T) {
	m := newMapper(testView(t), resourceOf(nil), nil, MaxDiagnostics, Options{})
	p := position{}
	for _, tc := range []struct {
		k    jsonschema.ErrorKind
		want string
	}{
		{&kind.PropertyNames{Property: "abcd"}, `member name "abcd" is not allowed`},
		{&kind.Dependency{Prop: "a", Missing: []string{"b", "c"}}, "a requires b and c"},
		{&kind.MaxContains{Want: 1}, "must contain at most 1 matching entry"},
		{&kind.ContentEncoding{Want: "base64"}, "does not match the schema (contentEncoding)"},
	} {
		msg, _ := m.leafMessage(&jsonschema.ValidationError{ErrorKind: tc.k}, p)
		if msg != tc.want {
			t.Errorf("leafMessage(%T) = %q, want %q", tc.k, msg, tc.want)
		}
	}
	if got := quote(strings.Repeat("é", 70)); !strings.HasSuffix(got, `..."`) || strings.Count(got, "é") != maxQuoted {
		t.Errorf("quote(long) = %q, want %d code points and an ellipsis", got, maxQuoted)
	}
	if got := shorten(strings.Repeat("x", 70)); len(got) != maxQuoted+3 {
		t.Errorf("shorten(long) = %q", got)
	}
	if got := joinOr(nil); got != "none" {
		t.Errorf("joinOr(nil) = %q", got)
	}
	if got := joinTypes([]string{"integer", "string", "null"}); got != "an integer, a string or null" {
		t.Errorf("joinTypes() = %q", got)
	}
	if got := typeName(&tree.Node{Kind: tree.KindNull}); got != "null" {
		t.Errorf("typeName(null) = %q", got)
	}
}
