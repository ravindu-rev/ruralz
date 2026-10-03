// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// committed decodes the committed file of a view; TestCommittedSchemaIsCurrent
// proves it equals the generator's output.
func committed(t *testing.T, v view) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(schemaDir, outputs()[v]))
	if err != nil {
		t.Fatal(err)
	}
	return decode(t, data)
}

// definitions returns the $defs of a decoded view.
func definitions(t *testing.T, doc map[string]any) map[string]any {
	t.Helper()
	defs, ok := doc["$defs"].(map[string]any)
	if !ok {
		t.Fatal("no $defs")
	}
	return defs
}

// propertyKeyword collects keyword from every property of every definition,
// keyed "Definition.property".
func propertyKeyword(t *testing.T, doc map[string]any, keyword string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for name, d := range definitions(t, doc) {
		props, _ := d.(map[string]any)["properties"].(map[string]any)
		for p, ps := range props {
			if v, ok := ps.(map[string]any)[keyword]; ok {
				out[name+"."+p] = v
			}
		}
	}
	return out
}

// TestDefaults pins every schema default of the rendered view: the M0 set
// (02 req 10, 01 req 36) plus the M1 markers of resolution R-21: the State
// Store timeout (08 req 6), the request limits, the auth.api-key header
// (06 req 15), the trace sampling ratio (OQ-observability-1 (a)) and the
// static Upstream defaults of OQ-traffic-management-and-resilience-5 and -6
// (05 req 4). retryOn and failureWhen stay runtime rules and have none.
func TestDefaults(t *testing.T) {
	want := map[string]string{ //nolint:gosec // Schema defaults, among them a header name, not credentials.
		"ActiveHealthCheck.healthyThreshold":   `2`,
		"ActiveHealthCheck.interval":           `"10s"`,
		"ActiveHealthCheck.path":               `"/"`,
		"ActiveHealthCheck.timeout":            `"2s"`,
		"ActiveHealthCheck.unhealthyThreshold": `3`,
		"Admin.port":                           `9901`,
		"AuthAPIKeyConfig.header":              `"x-api-key"`,
		"AuthUpstreamOAuth2Config.timeout":     `"2s"`,
		"AuthUpstreamSigV4Config.payload":      `"signed"`,
		"BasicCredential.iterations":           `600000`,
		"CircuitBreaker.consecutiveFailures":   `5`,
		"CircuitBreaker.failureRatio":          `0.5`,
		"CircuitBreaker.halfOpenSuccesses":     `3`,
		"CircuitBreaker.maxConnections":        `1024`,
		"CircuitBreaker.maxPendingRequests":    `256`,
		"CircuitBreaker.minimumLegs":           `20`,
		"CircuitBreaker.openDuration":          `"30s"`,
		"CompositionStep.collection":           `false`,
		"CompositionStep.optional":             `false`,
		"Endpoint.weight":                      `1`,
		"Limits.maxBufferedBytes":              `536870912`,
		"Limits.maxCompositionSteps":           `16`,
		"Limits.maxPluginMemoryBytes":          `2147483648`,
		"Limits.maxRequestBodyBytes":           `10485760`,
		"Limits.maxRequestHeaderBytes":         `65536`,
		"Limits.maxResponseBodyBytes":          `10485760`,
		"Listener.proxyProtocol":               `false`,
		"ListenerTLS.minVersion":               `"1.3"`,
		"LoadBalancing.algorithm":              `"least-request"`,
		"PassiveHealthCheck.consecutiveErrors": `5`,
		"PassiveHealthCheck.ejectionTime":      `"30s"`,
		"PluginLimits.memoryBytes":             `16777216`,
		"PluginLimits.timeout":                 `"5ms"`,
		"PolicySpec.overridable":               `true`,
		"QuotaConfig.key":                      `"consumer.name"`,
		"RateLimitConfig.localOnly":            `false`,
		"Retries.attempts":                     `1`,
		"RouteUpstream.weight":                 `1`,
		"StateStore.driver":                    `"memory"`,
		"StateStore.timeout":                   `"50ms"`,
		"StateStore.topology":                  `"standalone"`,
		"StateStoreCache.topology":             `"standalone"`,
		"Telemetry.traceSampling":              `0.01`,
		"TransformRequestConfig.contentType":   `"application/json"`,
		"TransformResponseConfig.contentType":  `"application/json"`,
	}
	for _, v := range []view{rendered, authoring} {
		got := map[string]string{}
		for k, dv := range propertyKeyword(t, committed(t, v), "default") {
			b, err := json.Marshal(dv)
			if err != nil {
				t.Fatal(err)
			}
			got[k] = string(b)
		}
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if got[k] != want[k] {
				t.Errorf("%s view: default of %s = %q, want %s", v, k, got[k], want[k])
			}
		}
		for _, k := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[k]; !ok {
				t.Errorf("%s view: unexpected default %s = %s", v, k, got[k])
			}
		}
		// Context-derived and CEL defaults are runtime rules, never schema
		// defaults (R-21; 02 section 7, OQ-traffic-management-and-resilience-6).
		for _, runtime := range []string{"Retries.retryOn", "CircuitBreaker.failureWhen", "Retries.perTryTimeout", "RouteSpec.timeout", "UpstreamSpec.timeout", "PolicySpec.stateStoreTimeout"} {
			if _, ok := got[runtime]; ok {
				t.Errorf("%s view: %s is a runtime rule, not a schema default", v, runtime)
			}
		}
	}
}

// TestDefaultsValidate checks every default against its own property schema
// in the committed rendered view (02 test plan item 7: every default
// validates), so materialization never produces an invalid document.
func TestDefaultsValidate(t *testing.T) {
	doc := committed(t, rendered)
	defs := definitions(t, doc)
	resolve := func(s map[string]any) map[string]any {
		if r, ok := s["$ref"].(string); ok {
			d, ok := defs[strings.TrimPrefix(r, defsPrefix)].(map[string]any)
			if !ok {
				t.Fatalf("unresolved %s", r)
			}
			merged := maps.Clone(d)
			maps.Copy(merged, s)
			return merged
		}
		return s
	}
	for _, name := range slices.Sorted(maps.Keys(defs)) {
		props, _ := defs[name].(map[string]any)["properties"].(map[string]any)
		for _, p := range slices.Sorted(maps.Keys(props)) {
			ps := props[p].(map[string]any)
			dv, ok := ps["default"]
			if !ok {
				continue
			}
			if err := validateScalar(resolve(ps), dv); err != nil {
				t.Errorf("%s.%s default %v: %v", name, p, dv, err)
			}
		}
	}
}

// validateScalar is a small draft 2020-12 check of the scalar keywords the
// generator emits: type, enum, pattern, minimum, maximum and anyOf.
func validateScalar(s map[string]any, v any) error {
	if branches, ok := s["anyOf"].([]any); ok {
		var errs []string
		for _, b := range branches {
			err := validateScalar(b.(map[string]any), v)
			if err == nil {
				return nil
			}
			errs = append(errs, err.Error())
		}
		return &schemaError{"no anyOf branch matches: " + strings.Join(errs, "; ")}
	}
	switch s["type"] {
	case "integer":
		n, ok := v.(float64)
		if !ok || n != float64(int64(n)) {
			return &schemaError{"not an integer"}
		}
	case "number":
		if _, ok := v.(float64); !ok {
			return &schemaError{"not a number"}
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return &schemaError{"not a boolean"}
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return &schemaError{"not a string"}
		}
		if p, ok := s["pattern"].(string); ok && !regexp.MustCompile(p).MatchString(str) {
			return &schemaError{"does not match " + p}
		}
	default:
	}
	if enum, ok := s["enum"].([]any); ok && !slices.Contains(enum, v) {
		return &schemaError{"not in the enum"}
	}
	if n, ok := v.(float64); ok {
		if lo, ok := s["minimum"].(float64); ok && n < lo {
			return &schemaError{"below the minimum"}
		}
		if hi, ok := s["maximum"].(float64); ok && n > hi {
			return &schemaError{"above the maximum"}
		}
	}
	return nil
}

type schemaError struct{ msg string }

func (e *schemaError) Error() string { return e.msg }

// TestImpactClasses pins every x-ruralz-impact annotation: the M0 markers,
// the classes proposed by 02 R-6 (Gateway, Upstream, Consumer and
// AIProvider fields), overridable as a security change (02 req 65), and
// security on every secret field and on the oauth2 tokenUrl, the secret's
// destination (06 rule 90).
func TestImpactClasses(t *testing.T) {
	want := map[string]string{
		"AICredentials.apiKey":                  "security",
		"AIModelSpec.candidates":                "ai",
		"AIProviderSpec.baseUrl":                "ai,security",
		"AIProviderSpec.credentials":            "ai,security",
		"APIKey.secretRef":                      "security",
		"AuthMTLSConfig.caCertificate":          "security",
		"AuthMTLSConfig.crl":                    "security",
		"AuthUpstreamOAuth2Config.clientSecret": "security",
		"AuthUpstreamOAuth2Config.tokenUrl":     "security",
		"Certificate.certificate":               "security",
		"Certificate.privateKey":                "security",
		"ConsumerSpec.credentials":              "security",
		"ConsumerSpec.quotas":                   "traffic",
		"ConsumerSpec.tags":                     "metadata",
		"ConsumerSpec.tier":                     "traffic",
		"GatewaySpec.admin":                     "security",
		"GatewaySpec.limits":                    "traffic",
		"GatewaySpec.listeners":                 "routing",
		"GatewaySpec.stateStore":                "traffic",
		"GatewaySpec.telemetry":                 "metadata",
		"GatewaySpec.trustedProxies":            "security",
		"Listener.proxyProtocol":                "security",
		"Listener.tls":                          "security",
		"OTLP.tls":                              "security",
		"PluginSpec.capabilities":               "plugin,security",
		"PluginSpec.image":                      "plugin,security",
		"PolicySpec.overridable":                "security",
		"RouteSpec.timeout":                     "traffic",
		"StateStore.url":                        "security",
		"StateStoreCache.url":                   "security",
		"UpstreamSpec.ai":                       "ai",
		"UpstreamSpec.circuitBreaker":           "traffic",
		"UpstreamSpec.healthCheck":              "traffic",
		"UpstreamSpec.loadBalancing":            "traffic",
		"UpstreamSpec.retries":                  "traffic",
		"UpstreamSpec.timeout":                  "traffic",
		"UpstreamSpec.tls":                      "security",
		"UpstreamTLS.caCertificate":             "security",
		"UpstreamTLS.clientCertificate":         "security",
		"UpstreamTLS.clientKey":                 "security",
	}
	got := map[string]string{}
	for k, v := range propertyKeyword(t, committed(t, rendered), "x-ruralz-impact") {
		var classes []string
		for _, c := range v.([]any) {
			classes = append(classes, c.(string))
		}
		got[k] = strings.Join(classes, ",")
	}
	if !maps.Equal(got, want) {
		for _, k := range slices.Sorted(maps.Keys(want)) {
			if got[k] != want[k] {
				t.Errorf("impact of %s = %q, want %q", k, got[k], want[k])
			}
		}
		for _, k := range slices.Sorted(maps.Keys(got)) {
			if _, ok := want[k]; !ok {
				t.Errorf("unexpected impact on %s: %s", k, got[k])
			}
		}
	}
	// Policy attachment impact is derived from the referenced type (02 req
	// 64), so spec.policies carries no marker (02 R-6).
	for _, k := range []string{"GatewaySpec.policies", "RouteSpec.policies", "UpstreamSpec.policies", "RouteSpec.excludePolicies"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not carry an impact marker", k)
		}
	}
}

// TestSecretFieldsCarrySecurity checks 06 rule 90 on the committed schema:
// every x-ruralz-secret property lists the security impact class.
func TestSecretFieldsCarrySecurity(t *testing.T) {
	doc := committed(t, rendered)
	impact := propertyKeyword(t, doc, "x-ruralz-impact")
	secrets := propertyKeyword(t, doc, "x-ruralz-secret")
	if len(secrets) < 12 {
		t.Fatalf("found %d secret fields, want at least 12", len(secrets))
	}
	for k := range secrets {
		classes, _ := impact[k].([]any)
		if !slices.Contains(classes, any("security")) {
			t.Errorf("secret field %s lacks the security impact class", k)
		}
	}
}

// TestSchemaLevelZero checks 02 req 75: the served schema level of
// ruralz/v1alpha1 is the maximum x-ruralz-since, 0 in M1, so no field
// carries the keyword (fields added before 0.1.0 are level 0,
// OQ-traffic-management-and-resilience-5).
func TestSchemaLevelZero(t *testing.T) {
	for _, v := range []view{authoring, rendered} {
		out, err := generate(realInput(t), v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(out), "x-ruralz-since") {
			t.Errorf("%s view carries x-ruralz-since; M1 is schema level 0", v)
		}
		if strings.Contains(string(out), `"deprecated"`) {
			t.Errorf("%s view marks a deprecated field; the M1 lifecycle table is empty (02 req 76)", v)
		}
	}
}

// TestRefsResolve checks that every $ref of both views names a definition.
func TestRefsResolve(t *testing.T) {
	for _, v := range []view{authoring, rendered} {
		doc := committed(t, v)
		defs := definitions(t, doc)
		var walk func(n any, path string)
		walk = func(n any, path string) {
			switch x := n.(type) {
			case map[string]any:
				if r, ok := x["$ref"].(string); ok {
					if _, ok := defs[strings.TrimPrefix(r, defsPrefix)]; !ok || !strings.HasPrefix(r, defsPrefix) {
						t.Errorf("%s view: %s: unresolved $ref %s", v, path, r)
					}
				}
				for k, c := range x {
					walk(c, path+"/"+k)
				}
			case []any:
				for _, c := range x {
					walk(c, path)
				}
			default:
			}
		}
		walk(doc, "#")
	}
}

// TestM1Fields checks the shape of the fields M1 adds (WP-28 scope).
func TestM1Fields(t *testing.T) {
	s := committed(t, rendered)
	cases := []struct {
		name string
		path []string
		want string
	}{
		// OQ-scalability-and-distributed-state-3 (a), R-19: the cache connection.
		{"stateStore.cache", []string{"$defs", "StateStore", "properties", "cache", "$ref"}, `"#/$defs/StateStoreCache"`},
		{"cache url required", []string{"$defs", "StateStoreCache", "required"}, `["url"]`},
		{"cache url secret", []string{"$defs", "StateStoreCache", "properties", "url", "x-ruralz-secret"}, `true`},
		{"cache closed", []string{"$defs", "StateStoreCache", "additionalProperties"}, `false`},
		// OQ-observability-2 (a): otlp.tls with the UpstreamTLS shape.
		{"otlp.tls", []string{"$defs", "OTLP", "properties", "tls", "$ref"}, `"#/$defs/UpstreamTLS"`},
		// 09 req 17: the endpoint pattern.
		{"otlp.endpoint", []string{"$defs", "OTLP", "properties", "endpoint", "pattern"}, `"^https?://[^/?#@\\s]+/?$"`},
		// OQ-traffic-management-and-resilience-5 (a): breaker guards.
		{"minimumLegs", []string{"$defs", "CircuitBreaker", "properties", "minimumLegs", "minimum"}, `1`},
		{"failureRatio type", []string{"$defs", "CircuitBreaker", "properties", "failureRatio", "type"}, `"number"`},
		{"failureRatio max", []string{"$defs", "CircuitBreaker", "properties", "failureRatio", "maximum"}, `1`},
		{"halfOpenSuccesses", []string{"$defs", "CircuitBreaker", "properties", "halfOpenSuccesses", "minimum"}, `1`},
		// OQ-scalability-and-distributed-state-11 (a): localOnly.
		{"localOnly", []string{"$defs", "RateLimitConfig", "properties", "localOnly", "type"}, `"boolean"`},
		// 07 J 74: validation.json-schema config.schema, inline draft 2020-12.
		{"schema ref", []string{"$defs", "ValidationJSONSchemaConfig", "properties", "schema", "$ref"}, `"#/$defs/JSONSchemaDocument"`},
		{"schema required", []string{"$defs", "ValidationJSONSchemaConfig", "required"}, `["schema"]`},
		{"schema object or boolean", []string{"$defs", "JSONSchemaDocument", "type"}, `["object","boolean"]`},
		// 07 req 20: headers add and remove.
		{"request add", []string{"$defs", "HeaderRequestOps", "properties", "add", "x-ruralz-list"}, `{"type":"atomic"}`},
		{"request add items", []string{"$defs", "HeaderRequestOps", "properties", "add", "items"}, `{"$ref":"#/$defs/HeaderRequestSet"}`},
		{"request remove", []string{"$defs", "HeaderRequestOps", "properties", "remove", "x-ruralz-list"}, `{"type":"set"}`},
		{"request remove items", []string{"$defs", "HeaderRequestOps", "properties", "remove", "items"}, `{"type":"string"}`},
		{"response add", []string{"$defs", "HeaderResponseOps", "properties", "add", "items"}, `{"$ref":"#/$defs/HeaderResponseSet"}`},
		{"response remove", []string{"$defs", "HeaderResponseOps", "properties", "remove", "x-ruralz-list"}, `{"type":"set"}`},
		{"set stays a map", []string{"$defs", "HeaderRequestOps", "properties", "set", "x-ruralz-list"}, `{"key":"name","type":"map"}`},
		// 07 risk 3: replace without path rewrites the raw body.
		{"replace required", []string{"$defs", "Replace", "required"}, `["pattern"]`},
	}
	for _, c := range cases {
		got, err := json.Marshal(dig(t, s, c.path...))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != c.want {
			t.Errorf("%s: %v = %s, want %s", c.name, c.path, got, c.want)
		}
	}
}

// TestPolicyConfigClosure checks OQ-configuration-model-19 (a) as applied in
// M1: the configs whose fields are registered in full are closed (headers
// and validation.json-schema join them), and every config with
// feature-authored fields still unregistered stays open.
func TestPolicyConfigClosure(t *testing.T) {
	defs := definitions(t, committed(t, rendered))
	open := map[string]bool{
		"AuthJWTConfig":              true, // R-32: issuer fields registered later
		"AuthAPIKeyConfig":           true, // config.query later
		"AuthBasicConfig":            false,
		"AuthMTLSConfig":             false,
		"AuthzCELConfig":             true,
		"AuthzOPAConfig":             true,
		"AuthzCedarConfig":           true,
		"AuthzIPConfig":              false,
		"AuthzGeoIPConfig":           false,
		"RateLimitConfig":            true, // responseHeaders and cost later
		"QuotaConfig":                true,
		"ValidationJSONSchemaConfig": false,
		"CORSConfig":                 true,
		"CacheConfig":                true,
		"HeadersConfig":              false,
		"HeaderRequestOps":           false,
		"HeaderResponseOps":          false,
		"HeaderRequestSet":           false,
		"HeaderResponseSet":          false,
		"TransformRequestConfig":     false,
		"TransformResponseConfig":    false,
		"AuthUpstreamOAuth2Config":   true,
		"AuthUpstreamSigV4Config":    false,
		"AITokenBudgetConfig":        true,
		"AISemanticCacheConfig":      true,
		"AIGuardrailConfig":          true,
	}
	for _, name := range slices.Sorted(maps.Keys(open)) {
		d, ok := defs[name].(map[string]any)
		if !ok {
			t.Errorf("no definition %s", name)
			continue
		}
		_, closed := d["additionalProperties"]
		if closed == open[name] {
			t.Errorf("%s: open = %v, want %v", name, !closed, open[name])
		}
	}
}

// TestAuthoringSubstitution checks OQ-configuration-model-20 (a): in the
// authoring view a ${VAR} expression is offered wherever substitution is
// allowed and the value's constraints would reject it, including strings
// constrained by an enum or a pattern; never in ref, CEL or secret fields,
// the whole subtree of a secret value, or the envelope (RZ-CFG-011). The
// rendered view never offers it.
func TestAuthoringSubstitution(t *testing.T) {
	a, r := committed(t, authoring), committed(t, rendered)
	offers := func(n any) bool {
		m, _ := n.(map[string]any)
		branches, _ := m["anyOf"].([]any)
		for _, b := range branches {
			if b.(map[string]any)["$ref"] == defsPrefix+substDefName {
				return true
			}
		}
		return false
	}
	cases := []struct {
		path []string
		want bool
	}{
		{[]string{"ListenerTLS", "properties", "minVersion"}, true},             // enum
		{[]string{"PolicySpec", "properties", "failureMode"}, true},             // enum
		{[]string{"StateStore", "properties", "timeout"}, true},                 // Duration pattern
		{[]string{"OTLP", "properties", "endpoint"}, true},                      // field pattern
		{[]string{"APIKey", "properties", "hash"}, true},                        // field pattern
		{[]string{"PluginSpec", "properties", "phases", "items"}, true},         // enum list items
		{[]string{"CircuitBreaker", "properties", "failureRatio"}, true},        // number
		{[]string{"Limits", "properties", "maxRequestBodyBytes"}, true},         // ByteSize
		{[]string{"Listener", "properties", "name"}, false},                     // unconstrained string
		{[]string{"SecretRef", "properties", "provider"}, false},                // inside a secret value
		{[]string{"StateStoreCache", "properties", "url"}, false},               // secret
		{[]string{"ObjectMeta", "properties", "name"}, false},                   // envelope
		{[]string{"RouteUpstream", "properties", "name"}, false},                // ref
		{[]string{"RouteMatch", "properties", "when"}, false},                   // CEL
		{[]string{"ValidationJSONSchemaConfig", "properties", "schema"}, false}, // whole document
		// The config dispatch discriminator permits substitution (only the
		// envelope, ref, secret and CEL fields forbid it), so the authoring
		// view accepts type: ${T}; such a Policy matches no if/then branch
		// and its config is checked in the rendered view only, after
		// substitution (TestSubstitutedPolicyType).
		{[]string{"PolicySpec", "properties", "type"}, true},
	}
	for _, c := range cases {
		path := append([]string{"$defs"}, c.path...)
		if got := offers(dig(t, a, path...)); got != c.want {
			t.Errorf("authoring %v offers substitution = %v, want %v", c.path, got, c.want)
		}
		if offers(dig(t, r, path...)) {
			t.Errorf("rendered %v offers substitution", c.path)
		}
	}
}

// TestFixtureSubstitution runs the substitution rules on the Good fixture.
func TestFixtureSubstitution(t *testing.T) {
	info, err := parseSources([]string{"fixture_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[Good]()}}, authoring)
	if err != nil {
		t.Fatal(err)
	}
	props := dig(t, decode(t, out), "$defs", "GoodSpec", "properties").(map[string]any)
	want := map[string]bool{"a": true, "b": true, "mode": true, "ratio": true, "path": true, "wait": true, "note": false, "target": false, "schema": false}
	for name, offered := range want {
		_, got := props[name].(map[string]any)["anyOf"]
		if got != offered {
			t.Errorf("GoodSpec.%s anyOf = %v, want %v", name, got, offered)
		}
	}
	if d := props["wait"].(map[string]any)["default"]; d != "1m30s" {
		t.Errorf("duration default = %v, want the canonical 1m30s", d)
	}
}

// TestSubstitutedPolicyType pins how a Policy whose spec.type is a ${VAR}
// expression meets the config dispatch (OQ-configuration-model-20 (a)): no
// registered Policy type matches the Substitution pattern, so in the
// authoring view such a Policy selects no if/then branch and its config is
// not checked there; the rendered view offers no Substitution, so after
// substitution the type must be a registered one and its config dispatches.
func TestSubstitutedPolicyType(t *testing.T) {
	subst := regexp.MustCompile(substitutionPattern)
	for _, v := range []view{authoring, rendered} {
		doc := committed(t, v)
		enum, _ := dig(t, doc, "$defs", "PolicyType", "enum").([]any)
		var dispatched []any
		allOf, _ := dig(t, doc, "$defs", "PolicySpec", "allOf").([]any)
		for _, b := range allOf {
			if _, ok := b.(map[string]any)["if"]; ok {
				dispatched = append(dispatched, dig(t, b, "if", "properties", "type", "const"))
			}
		}
		if len(enum) == 0 || !slices.Equal(dispatched, enum) {
			t.Fatalf("view %d: dispatch on %v, want every Policy type %v", v, dispatched, enum)
		}
		for _, typ := range enum {
			if s, _ := typ.(string); subst.MatchString(s) {
				t.Errorf("Policy type %q matches the Substitution pattern", s)
			}
		}
	}
	if typ, err := json.Marshal(dig(t, committed(t, rendered), "$defs", "PolicySpec", "properties", "type")); err != nil || !strings.Contains(string(typ), `"$ref":"#/$defs/PolicyType"`) || strings.Contains(string(typ), "anyOf") {
		t.Errorf("rendered PolicySpec.type = %s, %v; want the plain PolicyType reference", typ, err)
	}
}

// TestImpactKeyword covers the x-ruralz-impact marker syntax.
func TestImpactKeyword(t *testing.T) {
	cases := []struct {
		in   string
		want []string
		err  string
	}{
		{"security", []string{"security"}, ""},
		{"ai,security", []string{"ai", "security"}, ""},
		{"metadata,plugin,routing,traffic", []string{"metadata", "plugin", "routing", "traffic"}, ""},
		{"security,ai", nil, "ascending"},
		{"ai,ai", nil, "ascending"},
		{"cost", nil, "unknown impact class"},
		{"", nil, "unknown impact class"},
	}
	for _, c := range cases {
		got, err := impactKeyword(c.in)
		if c.err != "" {
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("impactKeyword(%q) err = %v, want %q", c.in, err, c.err)
			}
			continue
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("impactKeyword(%q) = %v, %v; want %v", c.in, got, err, c.want)
		}
	}
}

// TestCheckDefault covers the default-against-constraint checks.
func TestCheckDefault(t *testing.T) {
	g := &generator{defs: map[string]schema{
		"Code": {"type": "string", "pattern": "^[A-Z]{2}$"},
	}}
	cases := []struct {
		name  string
		value schema
		dv    any
		err   string
	}{
		{"int in range", schema{"minimum": int64(1), "maximum": int64(3)}, int64(2), ""},
		{"int below", schema{"minimum": int64(1)}, int64(0), "minimum"},
		{"int above", schema{"maximum": int64(3)}, int64(4), "maximum"},
		{"float in range", schema{"minimum": int64(0), "maximum": int64(1)}, 0.5, ""},
		{"float above", schema{"maximum": 0.5}, 0.75, "maximum"},
		{"string pattern", schema{"pattern": "^/"}, "/x", ""},
		{"string pattern miss", schema{"pattern": "^/"}, "x", "pattern"},
		{"bad pattern", schema{"pattern": "("}, "x", "pattern"},
		{"min length", schema{"minLength": 2}, "a", "minLength"},
		{"max length counts characters", schema{"maxLength": 2}, "éé", ""},
		{"max length", schema{"maxLength": 2}, "abc", "maxLength"},
		{"ref pattern", schema{"$ref": defsPrefix + "Code"}, "NO", ""},
		{"ref pattern miss", schema{"$ref": defsPrefix + "Code"}, "no", "pattern"},
		{"unknown ref", schema{"$ref": defsPrefix + "Other"}, "x", ""},
		{"bool", schema{"minimum": int64(1)}, true, ""},
	}
	for _, c := range cases {
		err := g.checkDefault(c.value, c.dv)
		if c.err == "" && err != nil {
			t.Errorf("%s: %v", c.name, err)
		}
		if c.err != "" && (err == nil || !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s: err = %v, want %q", c.name, err, c.err)
		}
	}
}
