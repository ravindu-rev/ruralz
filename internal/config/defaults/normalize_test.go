// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package defaults

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// normalized runs Normalize on a resource parsed from src (file r.json,
// role base) and returns it with the diagnostic texts.
func normalized(t *testing.T, src string) (*tree.Resource, []string) {
	t.Helper()
	f := newFixture()
	res := f.resource(t, "r.json", src)
	ds := Normalize(hubIndex(t), res, f.files)
	return res, texts(ds)
}

func route(spec string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "r"}, "spec": ` + spec + `}`
}

func gateway(spec string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "edge"}, "spec": ` + spec + `}`
}

func policyDoc(typ, config string) string {
	return `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "p"}, "spec": {"type": "` + typ + `", "config": ` + config + `}}`
}

// TestNormalizeDurations covers the normalization of durations.
func TestNormalizeDurations(t *testing.T) {
	tests := []struct{ in, want, err string }{
		{"90s", "1m30s", ""},
		{"1.5h", "1h30m0s", ""},
		{"1000ms", "1s", ""},
		{"0", "0s", ""},
		{"0.5s", "500ms", ""},
		{"1h0m0s", "1h0m0s", ""},
		{"1m30s", "1m30s", ""},
		{"1500us", "1.5ms", ""},
		{"2µs", "2µs", ""},
		{"9223372036854775807ns", "2562047h47m16.854775807s", ""},
		{"9223372036854775808ns", "", `r.json:1:99 error RZ-CFG-005 Route/r spec.timeout: duration "9223372036854775808ns" is out of range: the largest is 2562047h47m16.854775807s`},
		{"3000000h", "", `duration "3000000h" is out of range`},
		{"1x", "", `invalid duration "1x": want a Go duration such as 50ms`},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			res, ds := normalized(t, route(`{"timeout": "`+tc.in+`"}`))
			if tc.err != "" {
				if len(ds) != 1 || !strings.Contains(ds[0], tc.err) {
					t.Fatalf("diagnostics = %q, want one containing %q", ds, tc.err)
				}
				return
			}
			if len(ds) != 0 {
				t.Fatalf("diagnostics = %q", ds)
			}
			if got := at(t, res.Root, "spec.timeout"); got.Text != tc.want {
				t.Errorf("Duration %q = %q, want %q", tc.in, got.Text, tc.want)
			}
		})
	}
}

// TestNormalizeByteSizes covers the normalization of byte sizes, with the
// number range check after normalization.
func TestNormalizeByteSizes(t *testing.T) {
	tests := []struct{ in, want, err string }{
		{`"10Mi"`, "10485760", ""},
		{`"64Ki"`, "65536", ""},
		{`"1.5Gi"`, "1610612736", ""},
		{`"1k"`, "1000", ""},
		{`"4096"`, "4096", ""},
		{`4096`, "4096", ""},
		{`4096.0`, "4096", ""},
		{`1e3`, "1000", ""},
		{`"7Pi"`, "7881299347898368", ""},
		{`"8Pi"`, "", "integer 9007199254740992 is outside the I-JSON range -9007199254740991 to 9007199254740991"},
		{`"100Ei"`, "", `byte size "100Ei" is too large`},
		{`"1.5"`, "", `byte size "1.5" is not a whole number of bytes`},
		{`1e300`, "", "integer 1e300 is outside the I-JSON range"},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			res, ds := normalized(t, gateway(`{"listeners": [], "limits": {"maxRequestBodyBytes": `+tc.in+`}}`))
			if tc.err != "" {
				if len(ds) != 1 || !strings.Contains(ds[0], tc.err) || !strings.Contains(ds[0], "spec.limits.maxRequestBodyBytes:") {
					t.Fatalf("diagnostics = %q, want one at the field containing %q", ds, tc.err)
				}
				return
			}
			if len(ds) != 0 {
				t.Fatalf("diagnostics = %q", ds)
			}
			got := at(t, res.Root, "spec.limits.maxRequestBodyBytes")
			if got.Kind != tree.KindInt || got.Text != tc.want {
				t.Errorf("ByteSize %s = %s (kind %d), want integer %s", tc.in, got.Text, got.Kind, tc.want)
			}
		})
	}
}

// TestNormalizeDecimalAndIntOrString covers the normalization of decimals
// and IntOrString values.
func TestNormalizeDecimalAndIntOrString(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"3.00", "3"}, {"0.30", "0.3"}, {"007", "7"}, {"0.0", "0"}, {"15.50", "15.5"}, {"0", "0"}, {"10", "10"}, {"1.5x", "1.5x"},
	} {
		res, ds := normalized(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "AIProvider", "metadata": {"name": "p"},
 "spec": {"dialect": "openai", "pricing": {"models": [{"model": "m", "inputPerMillionTokens": "`+tc.in+`"}]}}}`)
		if len(ds) != 0 {
			t.Fatalf("diagnostics = %q", ds)
		}
		if got := at(t, res.Root, "spec.pricing.models[0].inputPerMillionTokens"); got.Kind != tree.KindString || got.Text != tc.want {
			t.Errorf("Decimal %q = %s, want %q", tc.in, dump(got), tc.want)
		}
	}
	for _, tc := range []struct{ in, want string }{{`8080`, `8080`}, {`"http"`, `"http"`}, {`"8080"`, `"8080"`}, {`8080.0`, `8080`}} {
		res, ds := normalized(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"},
 "spec": {"protocol": "http", "discovery": {"type": "dns", "port": `+tc.in+`}}}`)
		if len(ds) != 0 {
			t.Fatalf("diagnostics = %q", ds)
		}
		if got := dump(at(t, res.Root, "spec.discovery.port")); got != tc.want {
			t.Errorf("IntOrString %s = %s, want %s", tc.in, got, tc.want)
		}
	}
}

// TestNormalizeNumbers checks that integral numbers at integer positions
// become integer text, numbers elsewhere keep their literal, and every
// number outside the canonical form's range is RZ-CFG-005, free content
// included.
func TestNormalizeNumbers(t *testing.T) {
	tests := []struct {
		name, src, path, want, err string
	}{
		{"integer text", route(`{"upstreams": [{"name": "u", "weight": 2.0}]}`), "spec.upstreams[0].weight", "2", ""},
		{"exponent integer", route(`{"upstreams": [{"name": "u", "weight": 1e3}]}`), "spec.upstreams[0].weight", "1000", ""},
		{"negative zero", route(`{"upstreams": [{"name": "u", "weight": -0.0}]}`), "spec.upstreams[0].weight", "0", ""},
		{"number keeps literal", gateway(`{"listeners": [], "telemetry": {"traceSampling": 0.50}}`), "spec.telemetry.traceSampling", "0.50", ""},
		{"number exponent kept", gateway(`{"listeners": [], "telemetry": {"traceSampling": 5E-1}}`), "spec.telemetry.traceSampling", "5E-1", ""},
		{"max safe integer", route(`{"upstreams": [{"name": "u", "weight": 9007199254740991}]}`), "spec.upstreams[0].weight", "9007199254740991", ""},
		{"min safe integer free", policyDoc("quota", `{"consumerQuota": "c", "x": -9007199254740991}`), "spec.config.x", "-9007199254740991", ""},
		{"float above 2^53 free", policyDoc("quota", `{"consumerQuota": "c", "x": 1.2345678901234568e20}`), "spec.config.x", "1.2345678901234568e20", ""},
		{
			"2^53", route(`{"upstreams": [{"name": "u", "weight": 9007199254740992}]}`), "", "",
			"r.json:1:126 error RZ-CFG-005 Route/r spec.upstreams[name=u].weight: integer 9007199254740992 is outside the I-JSON range -9007199254740991 to 9007199254740991",
		},
		{"below -(2^53-1) free", policyDoc("quota", `{"consumerQuota": "c", "x": -9007199254740992}`), "", "", "spec.config.x: integer -9007199254740992 is outside the I-JSON range"},
		{"21 digits", policyDoc("quota", `{"consumerQuota": "c", "x": [123456789012345678901]}`), "", "", "spec.config.x[0]: integer 123456789012345678901 is outside the I-JSON range"},
		{"double overflow", policyDoc("quota", `{"consumerQuota": "c", "x": 1e400}`), "", "", "spec.config.x: number 1e400 is outside the range of an IEEE 754 double"},
		{"integral float too large", route(`{"upstreams": [{"name": "u", "weight": 1e16}]}`), "", "", "integer 1e16 is outside the I-JSON range"},
		{"inside json schema document", policyDoc("validation.json-schema", `{"schema": {"maximum": 99999999999999999999}}`), "", "", "spec.config.schema.maximum: integer 99999999999999999999 is outside the I-JSON range"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, ds := normalized(t, tc.src)
			if tc.err != "" {
				if len(ds) != 1 || !strings.Contains(ds[0], tc.err) {
					t.Fatalf("diagnostics = %q, want one containing %q", ds, tc.err)
				}
				return
			}
			if len(ds) != 0 {
				t.Fatalf("diagnostics = %q", ds)
			}
			if got := at(t, res.Root, tc.path); got.Text != tc.want {
				t.Errorf("%s = %s, want %s", tc.path, dump(got), tc.want)
			}
		})
	}
}

// TestCanonicalExemptFromRange checks that canonical content re-entering
// through FromResources is exempt from the number range rule: it keeps
// integer-looking doubles above 2^53, while the same content authored is
// RZ-CFG-005.
func TestCanonicalExemptFromRange(t *testing.T) {
	src := policyDoc("quota", `{"consumerQuota": "c", "x": 123456789012345680000, "y": 1e+21}`)
	f := newFixture()
	res := f.resourceRole(t, "lkg.json", src, tree.RoleCanonical)
	if ds := Normalize(hubIndex(t), res, f.files); len(ds) != 0 {
		t.Errorf("canonical content: %q", texts(ds))
	}
	res = f.resource(t, "policy.json", src)
	if ds := Normalize(hubIndex(t), res, f.files); len(ds) != 1 {
		t.Errorf("authored content: %q, want one range error", texts(ds))
	}
	// Without a file table the content counts as authored.
	res = f.resourceRole(t, "lkg2.json", src, tree.RoleCanonical)
	if ds := Normalize(hubIndex(t), res, nil); len(ds) != 1 || ds[0].File != "" || ds[0].Line != 1 {
		t.Errorf("no file table: %q", texts(ds))
	}
}

// TestNormalizeHosts covers the host case normalization: Route
// match.hosts and Gateway listeners[].hostnames are lower-cased (ASCII) and
// sorted as sets; a duplicate only lower-casing reveals is RZ-CFG-005.
func TestNormalizeHosts(t *testing.T) {
	res, ds := normalized(t, route(`{"match": {"hosts": ["Shop.Example", "*.EU.shop.example", "ÄPI.example"], "methods": ["POST", "GET"]}}`))
	if len(ds) != 0 {
		t.Fatalf("diagnostics = %q", ds)
	}
	if got := dump(at(t, res.Root, "spec.match")); got != `{"hosts":["*.eu.shop.example","shop.example","Äpi.example"],"methods":["GET","POST"]}` {
		t.Errorf("match = %s", got)
	}
	res, ds = normalized(t, gateway(`{"listeners": [{"name": "h", "port": 1, "protocol": "http", "hostnames": ["B.example", "a.EXAMPLE"]}]}`))
	if len(ds) != 0 {
		t.Fatalf("diagnostics = %q", ds)
	}
	if got := dump(at(t, res.Root, "spec.listeners[0].hostnames")); got != `["a.example","b.example"]` {
		t.Errorf("hostnames = %s", got)
	}
	_, ds = normalized(t, route(`{"match": {"hosts": ["a.example", "A.example"]}}`))
	want := []string{`r.json:1:121 error RZ-CFG-005 Route/r spec.match.hosts[item=a.example]: duplicate element "a.example" in a set after normalization (first element at r.json:1:108)`}
	if !slices.Equal(ds, want) {
		t.Errorf("diagnostics = %q\nwant %q", ds, want)
	}
	// Other string sets keep their case.
	res, _ = normalized(t, `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {}, "tags": ["B", "a"]}}`)
	if got := dump(at(t, res.Root, "spec.tags")); got != `["B","a"]` {
		t.Errorf("tags = %s", got)
	}
}

// TestNormalizeLists covers lists at stage G: set lists sorted by RFC 8785
// bytes (objects too), map lists sorted by key, orderedMap and atomic lists
// in authored order, and duplicates normalization reveals reported.
func TestNormalizeLists(t *testing.T) {
	tests := []struct {
		name, src, path, want string
	}{
		{"string set", route(`{"match": {"methods": ["POST", "GET", "DELETE"]}}`), "spec.match.methods", `["DELETE","GET","POST"]`},
		{
			"object set", policyDoc("auth.mtls", `{"caCertificate": {"secretRef": {"provider": "env", "name": "X"}}, "subjects": [{"uriSan": "spiffe://b"}, {"subject": "CN=a"}, {"uriSan": "spiffe://a"}]}`),
			"spec.config.subjects", `[{"subject":"CN=a"},{"uriSan":"spiffe://a"},{"uriSan":"spiffe://b"}]`,
		},
		{
			"map by key", `{"apiVersion": "ruralz/v1alpha1", "kind": "Upstream", "metadata": {"name": "u"}, "spec": {"protocol": "http", "endpoints": [{"address": "c:1"}, {"address": "a:1", "weight": 2}, {"address": "b:1"}]}}`,
			"spec.endpoints", `[{"address":"a:1","weight":2},{"address":"b:1"},{"address":"c:1"}]`,
		},
		{
			"listeners by name", gateway(`{"listeners": [{"name": "https", "port": 443, "protocol": "https"}, {"name": "http", "port": 80, "protocol": "http"}]}`),
			"spec.listeners", `[{"name":"http","port":80,"protocol":"http"},{"name":"https","port":443,"protocol":"https"}]`,
		},
		{"orderedMap kept", gateway(`{"listeners": [], "policies": [{"name": "z"}, {"name": "a"}, {"name": "m"}]}`), "spec.policies", `[{"name":"z"},{"name":"a"},{"name":"m"}]`},
		{
			"atomic kept", policyDoc("ratelimit", `{"limits": [{"requests": 9, "window": "60s"}, {"requests": 1, "window": "1s"}]}`),
			"spec.config.limits", `[{"requests":9,"window":"1m0s"},{"requests":1,"window":"1s"}]`,
		},
		{"unannotated list in free content kept", policyDoc("quota", `{"consumerQuota": "c", "x": [3, 1, 2]}`), "spec.config.x", `[3,1,2]`},
		{"route listeners set", route(`{"match": {}, "listeners": ["https", "http"]}`), "spec.listeners", `["http","https"]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, ds := normalized(t, tc.src)
			if len(ds) != 0 {
				t.Fatalf("diagnostics = %q", ds)
			}
			if got := dump(at(t, res.Root, tc.path)); got != tc.want {
				t.Errorf("%s = %s, want %s", tc.path, got, tc.want)
			}
		})
	}
}

// TestNormalizeListErrors covers the keyed-list rules that stage F
// normally reports first: a missing key and a duplicate key are
// RZ-CFG-005 at the entry.
func TestNormalizeListErrors(t *testing.T) {
	_, ds := normalized(t, gateway(`{"listeners": [{"name": "a"}, {"port": 1}, {"name": "a"}], "policies": [{"name": "p"}, {"name": "p"}]}`))
	want := []string{
		`r.json:1:122 error RZ-CFG-005 Gateway/edge spec.listeners[1]: list entry has no name key member`,
		`r.json:1:144 error RZ-CFG-005 Gateway/edge spec.listeners[name=a]: duplicate entry: name "a" is already used by an earlier entry after normalization (first entry at r.json:1:116)`,
		`r.json:1:188 error RZ-CFG-005 Gateway/edge spec.policies[name=p]: duplicate entry: name "p" is already used by an earlier entry after normalization (first entry at r.json:1:173)`,
	}
	if !slices.Equal(ds, want) {
		t.Errorf("diagnostics =\n%s\nwant\n%s", strings.Join(ds, "\n"), strings.Join(want, "\n"))
	}
}

// TestJSONSchemaDocument covers the JSONSchemaDocument value at stage G:
// the inline JSON Schema of a validation.json-schema Policy keeps every
// list (enum, required, prefixItems) in authored order, has its members
// sorted generically, its numbers checked, and gets no defaults. A null in
// it is RZ-CFG-005 like any other (TestNullRejected).
func TestJSONSchemaDocument(t *testing.T) {
	src := policyDoc("validation.json-schema", `{"schema": {"type": "object", "required": ["b", "a"],
 "properties": {"b": {"enum": [3, 1, "null", "x"]}, "a": {"type": "integer", "default": 1.0}},
 "prefixItems": [{"const": 2}, {"const": 1}]}}`)
	res, ds := normalized(t, src)
	if len(ds) != 0 {
		t.Fatalf("diagnostics = %q", ds)
	}
	want := `{"prefixItems":[{"const":2},{"const":1}],"properties":{"a":{"default":1.0,"type":"integer"},"b":{"enum":[3,1,"null","x"]}},"required":["b","a"],"type":"object"}`
	if got := dump(at(t, res.Root, "spec.config.schema")); got != want {
		t.Errorf("schema = %s\nwant   %s", got, want)
	}
	// A boolean schema is data too.
	res, ds = normalized(t, policyDoc("validation.json-schema", `{"schema": true}`))
	if len(ds) != 0 || !at(t, res.Root, "spec.config.schema").Bool {
		t.Errorf("boolean schema: %s %q", dump(res.Root), ds)
	}
	// A null is not data: the canonical form has none.
	_, ds = normalized(t, policyDoc("validation.json-schema", `{"schema": {"enum": ["a", null]}}`))
	want = `r.json:1:159 error RZ-CFG-005 Policy/p spec.config.schema.enum[1]: null is not allowed: the canonical form has no null; remove the element`
	if !slices.Equal(ds, []string{want}) {
		t.Errorf("null in an enum: %q\nwant %q", ds, want)
	}
}

// TestNullRejected checks that a JSON null never reaches the hub. Stage F
// admits one only where the schema says nothing (free content), so each
// case here passes stage F, and Normalize reports RZ-CFG-005 at the null's
// key-aware path, typed and free content alike, in authored and canonical
// content; Resource then returns no hub.Resource, so the canonical encoder
// never sees a null.
func TestNullRejected(t *testing.T) {
	const msg = ": null is not allowed: the canonical form has no null; remove the "
	tests := []struct {
		name, src string
		want      []string
	}{
		{
			"json schema enum", policyDoc("validation.json-schema", `{"schema": {"enum": ["a", null]}}`),
			[]string{"spec.config.schema.enum[1]" + msg + "element"},
		},
		{
			"json schema default", policyDoc("validation.json-schema", `{"schema": {"default": null}}`),
			[]string{"spec.config.schema.default" + msg + "member"},
		},
		{
			"open config member", policyDoc("quota", `{"consumerQuota": "c", "x": null}`),
			[]string{"spec.config.x" + msg + "member"},
		},
		{
			"nested free content", policyDoc("quota", `{"consumerQuota": "c", "x": {"b": [1, null], "a": null}}`),
			[]string{"spec.config.x.a" + msg + "member", "spec.config.x.b[1]" + msg + "element"},
		},
		{
			"plugin config", policyDoc("plugin", `{"x": null}`),
			[]string{"spec.config.x" + msg + "member"},
		},
		{
			"plugin configSchema", `{"apiVersion": "ruralz/v1alpha1", "kind": "Plugin", "metadata": {"name": "w"}, "spec": {"configSchema": {"const": null}}}`,
			[]string{"spec.configSchema.const" + msg + "member"},
		},
		{
			"typed field", route(`{"match": {}, "timeout": null}`),
			[]string{"spec.timeout" + msg + "member"},
		},
		{
			"keyed list entry", route(`{"match": {}, "upstreams": [{"name": "a", "weight": null}]}`),
			[]string{"spec.upstreams[name=a].weight" + msg + "member"},
		},
	}
	s := newStage(t)
	for _, tc := range tests {
		for _, role := range roles() {
			t.Run(tc.name+"/"+role.name, func(t *testing.T) {
				f := newFixture()
				res := f.resourceRole(t, "r.json", tc.src, role.role)
				ds := Normalize(hubIndex(t), res, f.files)
				var got []string
				for _, d := range ds {
					if d.Code != CodeSchema || d.Line == 0 {
						t.Errorf("diagnostic %s: want RZ-CFG-005 with a location", d.AppendText(nil))
					}
					got = append(got, d.Path.String()+": "+d.Message)
				}
				if !slices.Equal(got, tc.want) {
					t.Errorf("diagnostics = %q\nwant %q", got, tc.want)
				}
				if h, ds := s.Resource(f.resourceRole(t, "again.json", tc.src, role.role), f.files); h != nil || !diagnosticsOK(ds) || len(ds) != len(tc.want) {
					t.Errorf("Resource = %v, %q; want no hub resource and the %d nulls", h, texts(ds), len(tc.want))
				}
			})
		}
	}
}

// diagnosticsOK reports a non-empty list of RZ-CFG-005 errors.
func diagnosticsOK(ds diag.List) bool {
	return len(ds) > 0 && !slices.ContainsFunc(ds, func(d diag.Diagnostic) bool { return d.Code != CodeSchema })
}

// TestMemberOrder checks that object members are sorted by UTF-16 code
// units, here the RFC 8785 section 3.2.3 example, in free content and in
// typed objects alike.
func TestMemberOrder(t *testing.T) {
	src := policyDoc("quota", `{"x": {"€": 1, "\r": 2, "\ufb33": 3, "1": 4, "😀": 5, "\u0080": 6, "ö": 7}, "key": "k", "consumerQuota": "c"}`)
	res, ds := normalized(t, src)
	if len(ds) != 0 {
		t.Fatalf("diagnostics = %q", ds)
	}
	var keys []string
	for _, m := range at(t, res.Root, "spec.config.x").Members {
		keys = append(keys, m.Key)
	}
	want := []string{"\r", "1", "\u0080", "ö", "€", "😀", "\ufb33"}
	if !slices.Equal(keys, want) {
		t.Errorf("member order = %q, want %q", keys, want)
	}
	var top []string
	for _, m := range res.Root.Members {
		top = append(top, m.Key)
	}
	if !slices.Equal(top, []string{"apiVersion", "kind", "metadata", "spec"}) {
		t.Errorf("envelope order = %q", top)
	}
	if got := dump(at(t, res.Root, "spec.config")); !strings.HasPrefix(got, `{"consumerQuota":"c","key":"k","x":`) {
		t.Errorf("config order = %s", got)
	}
}

// TestPresence checks that empty objects, lists and strings stay present;
// nothing is added by Normalize.
func TestPresence(t *testing.T) {
	src := gateway(`{"listeners": [], "limits": {}, "trustedProxies": [], "telemetry": {"otlp": {"endpoint": ""}}}`)
	res, ds := normalized(t, src)
	if len(ds) != 0 {
		t.Fatalf("diagnostics = %q", ds)
	}
	if got := dump(at(t, res.Root, "spec")); got != `{"limits":{},"listeners":[],"telemetry":{"otlp":{"endpoint":""}},"trustedProxies":[]}` {
		t.Errorf("spec = %s", got)
	}
}

// TestNormalizeIdempotent checks that normalizing a normalized tree is a
// no-op.
func TestNormalizeIdempotent(t *testing.T) {
	for _, src := range []string{
		route(`{"timeout": "90s", "match": {"hosts": ["B.example", "a.example"], "methods": ["POST", "GET"]}, "upstreams": [{"name": "b", "weight": 2.0}, {"name": "a"}]}`),
		gateway(`{"listeners": [{"name": "b", "port": 2, "protocol": "http"}, {"name": "a", "port": 1, "protocol": "http"}], "limits": {"maxRequestBodyBytes": "10Mi"}, "trustedProxies": ["10.0.0.0/8", "1.2.3.4/32"]}`),
		policyDoc("validation.json-schema", `{"schema": {"enum": [2, 1], "b": {"z": 1, "a": 2}}}`),
	} {
		res, ds := normalized(t, src)
		if len(ds) != 0 {
			t.Fatalf("diagnostics = %q", ds)
		}
		once := res.Root.Clone()
		if ds := Normalize(hubIndex(t), res, nil); len(ds) != 0 || !equal(res.Root, once) {
			t.Errorf("second Normalize: %q\n%s\nwant %s", texts(ds), dump(res.Root), dump(once))
		}
	}
}

// TestSecretValuesNotShown covers the rule that a message never holds a
// value below an x-ruralz-secret field (SI Secrets rule 2).
func TestSecretValuesNotShown(t *testing.T) {
	idx := hubIndex(t)
	st, ok := idx.Def("SecretValue")
	if !ok {
		t.Fatal("no SecretValue definition")
	}
	gw, _ := idx.Spec("Gateway")
	ss, _ := gw.Property("stateStore")
	url, _ := ss.Select(nil).Property("url")
	z := &normalizer{res: &tree.Resource{}, stack: stack{{}, {schema: url, name: "url"}, {schema: st, name: "secretRef"}}}
	if got := z.shown("hunter2"); got != "the value" {
		t.Errorf("shown below a secret = %q", got)
	}
	if got := z.shownJSON([]byte(`"hunter2"`)); got != "value" {
		t.Errorf("shownJSON below a secret = %q", got)
	}
	z.stack = stack{{}}
	if got := z.shown("plain"); got != `"plain"` {
		t.Errorf("shown = %q", got)
	}
}

// TestDiagnosticCap covers the cap for Normalize: a hostile resource
// cannot build an unbounded list, and it returns one diagnostic beyond
// the default cap, so a diag.Collector of that cap sees it was reached.
func TestDiagnosticCap(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"consumerQuota": "c", "x": [`)
	for i := range DefaultMaxDiagnostics + 5 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString("1e999")
	}
	b.WriteString(`]}`)
	_, ds := normalized(t, policyDoc("quota", b.String()))
	if len(ds) != DefaultMaxDiagnostics+1 {
		t.Errorf("got %d diagnostics, want %d", len(ds), DefaultMaxDiagnostics+1)
	}
}

func TestNormalizeNil(t *testing.T) {
	if ds := Normalize(nil, &tree.Resource{}, nil); ds != nil {
		t.Errorf("Normalize(nil index) = %v", ds)
	}
	if ds := Normalize(hubIndex(t), nil, nil); ds != nil {
		t.Errorf("Normalize(nil) = %v", ds)
	}
	// An unknown kind is normalized as free content.
	res := &tree.Resource{ID: tree.ID{Kind: "Nope"}, Root: mustParse(t, `{"b": [2, 1], "a": 1e999}`)}
	ds := Normalize(hubIndex(t), res, nil)
	if len(ds) != 1 || dump(res.Root) != `{"a":1e999,"b":[2,1]}` {
		t.Errorf("unknown kind: %s %q", dump(res.Root), texts(ds))
	}
}

func TestStackPath(t *testing.T) {
	list := &schemaidx.Node{}
	item := mustParse(t, `{"name": "x"}`)
	s := stack{{}, {name: "spec"}, {item: true, list: list, node: item, index: 4}}
	if got := s.path(nil).String(); got != "spec[4]" {
		t.Errorf("path = %q", got)
	}
	if got := (stack{}).path(nil); len(got) != 0 {
		t.Errorf("empty path = %v", got)
	}
	if got := s.pos(tree.Pos{Line: 9}); got.Line != 1 {
		t.Errorf("pos = %+v, want the item's", got)
	}
	if got := (stack{{}}).pos(tree.Pos{Line: 9}); got.Line != 9 {
		t.Errorf("pos fallback = %+v", got)
	}
}

// TestHugeExponents covers number literals whose exponent is at or beyond
// the int range, at every kind of integer-only position (Route weight,
// Gateway listener port and limits.maxRequestBodyBytes, ratelimit requests
// and burst, Consumer basic iterations): stage G reports RZ-CFG-005 at the
// field and never panics, for authored and for canonical content, since
// integerText's exponent arithmetic never wraps.
func TestHugeExponents(t *testing.T) {
	positions := []struct {
		name string
		doc  func(w string) string
		path string
	}{
		{"weight", func(w string) string {
			return route(`{"match": {}, "upstreams": [{"name": "u", "weight": ` + w + `}]}`)
		}, "spec.upstreams[name=u].weight"},
		{"port", func(w string) string {
			return gateway(`{"listeners": [{"name": "h", "port": ` + w + `, "protocol": "http"}]}`)
		}, "spec.listeners[name=h].port"},
		{"maxRequestBodyBytes", func(w string) string {
			return gateway(`{"listeners": [], "limits": {"maxRequestBodyBytes": ` + w + `}}`)
		}, "spec.limits.maxRequestBodyBytes"},
		{"requests", func(w string) string {
			return policyDoc("ratelimit", `{"limits": [{"requests": `+w+`, "window": "1s"}]}`)
		}, "spec.config.limits[0].requests"},
		{"burst", func(w string) string {
			return policyDoc("ratelimit", `{"limits": [{"requests": 1, "window": "1s", "burst": `+w+`}]}`)
		}, "spec.config.limits[0].burst"},
		{"iterations", func(w string) string {
			return `{"apiVersion": "ruralz/v1alpha1", "kind": "Consumer", "metadata": {"name": "c"}, "spec": {"credentials": {"basic": [{"username": "u", "hash": "h", "iterations": ` + w + `}]}}}`
		}, "spec.credentials.basic[username=u].iterations"},
	}
	// Authored content gets the range rule; canonical content and a
	// non-integral literal reach the typed decode, whose message depends
	// on the field's Go type.
	literals := []struct {
		text, authored, canonical string
	}{
		{"1e9223372036854775807", "is outside the I-JSON range", "cannot decode"},
		{"1E+9223372036854775807", "is outside the I-JSON range", "cannot decode"},
		{"-1e9223372036854775807", "is outside the I-JSON range", "cannot decode"},
		{"1.5e9223372036854775807", "is outside the I-JSON range", "cannot decode"},
		{"10e9223372036854775807", "is outside the I-JSON range", "cannot decode"},
		{"1e999999999999999999999", "is outside the I-JSON range", "cannot decode"},
		{"1e-9223372036854775808", "cannot decode", "cannot decode"},
		{"1.5e-9223372036854775808", "cannot decode", "cannot decode"},
	}
	s := newStage(t)
	for _, p := range positions {
		for _, l := range literals {
			for _, role := range []tree.Role{tree.RoleBase, tree.RoleCanonical} {
				want := l.authored
				if role == tree.RoleCanonical {
					want = l.canonical
				}
				t.Run(p.name+"/"+l.text+"/"+strconv.Itoa(int(role)), func(t *testing.T) {
					f := newFixture()
					h, ds := s.Resource(f.resourceRole(t, "r.json", p.doc(l.text), role), f.files)
					if h != nil || len(ds) != 1 || ds[0].Code != CodeSchema || ds[0].Path.String() != p.path || !strings.Contains(ds[0].Message, want) {
						t.Errorf("Resource = %v, %q; want one RZ-CFG-005 at %s containing %q", h, texts(ds), p.path, want)
					}
				})
			}
		}
	}
}

// TestByteSizeMessage covers the message of a ByteSize that
// v1alpha1.ParseByteSize rejects, whose error quotes the whole value: the
// reason is kept and the value clipped; an error that does not quote the
// value gets the package's own message; below a secret field the value is
// not shown.
func TestByteSizeMessage(t *testing.T) {
	long := strings.Repeat("9", 100) + "Ki"
	z := &normalizer{res: &tree.Resource{}, stack: stack{{}}}
	_, err := v1alpha1.ParseByteSize(long)
	if got, want := z.byteSizeMessage(long, err), `byte size "`+strings.Repeat("9", 64)+`..." is too large`; got != want {
		t.Errorf("quoting error: %q, want %q", got, want)
	}
	if got, want := z.byteSizeMessage(long, errors.New("bad")), `invalid byte size "`+strings.Repeat("9", 64)+`..."`; got != want {
		t.Errorf("other error: %q, want %q", got, want)
	}
	gw, _ := hubIndex(t).Spec("Gateway")
	ss, _ := gw.Property("stateStore")
	url, _ := ss.Select(nil).Property("url")
	z.stack = stack{{}, {schema: url, name: "url"}}
	if got := z.byteSizeMessage(long, err); got != "byte size the value is too large" {
		t.Errorf("below a secret: %q", got)
	}
}

// TestNormalizeElementNames covers how Normalize names the list elements
// on a diagnostic's path: every diagnostic below one element gets the
// element's name, computed once when the element is done, so a set
// element is named by its normalized RFC 8785 text (its duration
// normalized and its inner set sorted, whatever member the diagnostic is
// at); each element of a list gets its own name; a diagnostic recorded
// after the list (a sibling member) is not renamed; and a keyed entry is
// named by its key member for every diagnostic below it.
func TestNormalizeElementNames(t *testing.T) {
	idx := setSchema(t)
	thing := func(spec string) *tree.Resource {
		return envelope(mustParse(t, `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {}, "spec": `+spec+`}`))
	}
	paths := func(ds diag.List) []string {
		out := make([]string, len(ds))
		for i, d := range ds {
			out[i] = d.Path.String()
		}
		return out
	}
	for _, tc := range []struct {
		name string
		run  func() diag.List
		want []string
	}{
		{"set element normalized", func() diag.List {
			ds, _ := normalize(idx, thing(`{"s": [{"t": ["b", "a"], "d": "90s", "a": null, "z": null}]}`), nil, newWork())
			return ds
		}, []string{
			`spec.s[item={"a":null,"d":"1m30s","t":["a","b"],"z":null}].a`,
			`spec.s[item={"a":null,"d":"1m30s","t":["a","b"],"z":null}].z`,
		}},
		{"each element its own", func() diag.List {
			ds, _ := normalize(idx, thing(`{"s": [{"a": null, "k": 1}, {"a": null, "k": 2}], "y": null}`), nil, newWork())
			return ds
		}, []string{`spec.s[item={"a":null,"k":1}].a`, `spec.s[item={"a":null,"k":2}].a`, `spec.y`}},
		{"nested elements", func() diag.List {
			ds, _ := normalize(idx, thing(`{"s": [{"x": [{"a": null}, {"b": [null]}]}]}`), nil, newWork())
			return ds
		}, []string{`spec.s[item={"x":[{"a":null},{"b":[null]}]}].x[0].a`, `spec.s[item={"x":[{"a":null},{"b":[null]}]}].x[1].b[0]`}},
		{"keyed entry", func() diag.List {
			res := envelope(mustParse(t, policyDoc("auth.jwt", `{"issuers": [{"issuer": "i", "jwksUrl": "https://x.example/j", "audiences": ["a"], "A": null, "B": 9007199254740993}]}`)))
			return Normalize(hubIndex(t), res, nil)
		}, []string{`spec.config.issuers[issuer=i].A`, `spec.config.issuers[issuer=i].B`}},
	} {
		if got := paths(tc.run()); !slices.Equal(got, tc.want) {
			t.Errorf("%s: paths\n%s\nwant\n%s", tc.name, strings.Join(got, "\n"), strings.Join(tc.want, "\n"))
		}
	}
}
