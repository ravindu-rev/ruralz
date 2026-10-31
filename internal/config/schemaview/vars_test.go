// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// afterSubstitution parses src, runs the real stage E (subst.Apply) with
// vars on it, as the pipeline does before stage F, and validates the
// result with v. Stage E must report no error, and must have recorded the
// variables of at least one scalar (tree.Node.Vars).
func afterSubstitution(t *testing.T, v *View, src string, vars map[string]string) diag.List {
	t.Helper()
	idx, err := schemaidx.NewSet(v.Index())
	if err != nil {
		t.Fatal(err)
	}
	files := &tree.FileTable{}
	res := mustResource(t, files, "f.json", src)
	res.APIVersion = v.APIVersion()
	if ds := subst.New(subst.Options{Schemas: idx, Files: files, Variables: subst.Map(vars)}).Apply(res); ds.HasErrors() {
		t.Fatalf("subst.Apply() = %s", texts(ds))
	}
	if !hasVars(res.Root) {
		t.Fatal("subst.Apply() recorded no variables")
	}
	return v.Validate(res, files)
}

// hasVars reports whether a node at or below n records variables.
func hasVars(n *tree.Node) bool {
	if n == nil {
		return false
	}
	if len(n.Vars) > 0 {
		return true
	}
	for _, m := range n.Members {
		if hasVars(m.Value) {
			return true
		}
	}
	for _, it := range n.Items {
		if hasVars(it) {
			return true
		}
	}
	return false
}

// thing wraps a synthetic Thing spec.
func thing(spec string) string {
	return `{"apiVersion": "test/v1", "kind": "Thing", "metadata": {"name": "a"}, "spec": ` + spec + `}`
}

// listener is a Gateway whose one listener has the given members after
// its name.
func listener(members string) string {
	return envelope("Gateway", `{"listeners": [{"name": "http"`+members+`}]}`)
}

// TestSubstitutedVariablesNamed checks that every finding about a value
// that stage E substituted variables into names them, for each keyword
// and wording a substituted scalar can fail, with real substitution
// output: re-typed values that do not fit (integer, number, boolean, an
// anyOf none of whose branches admits the string), values that fit their
// type but fail a keyword (enum, const, pattern, length, bounds,
// multipleOf, oneOf, not, an anyOf branch), the syntax texts (Duration,
// ByteSize, Decimal) and field syntaxes (OTLP endpoint, APIKey and
// BasicCredential hashes, Pricing.currency, CountryCode), partial
// substitution, defaults and more variables than one message names: each
// fails schema validation as RZ-CFG-005 with a message naming the
// variable, which stage F reads from tree.Node.Vars.
func TestSubstitutedVariablesNamed(t *testing.T) {
	rendered, synthetic := testView(t), syntheticView(t)
	for _, tc := range []struct {
		name string
		v    *View
		src  string
		vars map[string]string
		want []finding
	}{
		// Re-typed values that do not fit stay strings.
		{
			"integer", rendered, listener(`, "port": "${HTTP_PORT}", "protocol": "http"`),
			map[string]string{"HTTP_PORT": "eighty"},
			[]finding{{CodeSchema, "spec.listeners[name=http].port", `must be an integer, not a string; variable "HTTP_PORT" was substituted into the value`, ""}},
		},
		{
			"number", rendered, envelope("AIModel", `{"strategy": "fallback", "candidates": [{"name": "a", "provider": "p", "model": "m"}], "cache": {"semantic": {"similarityThreshold": "${RATIO}"}}}`),
			map[string]string{"RATIO": "half"},
			[]finding{{CodeSchema, "spec.cache.semantic.similarityThreshold", `must be a number, not a string; variable "RATIO" was substituted into the value`, ""}},
		},
		{
			"boolean", rendered, listener(`, "port": 80, "protocol": "http", "http3": "${H3}"`),
			map[string]string{"H3": "yes"},
			[]finding{{CodeSchema, "spec.listeners[name=http].http3", `must be a boolean, not a string; variable "H3" was substituted into the value`, ""}},
		},
		{
			"anyOf type", synthetic, thing(`{"toggle": "${TOGGLE}"}`),
			map[string]string{"TOGGLE": "yes"},
			[]finding{{CodeSchema, "spec.toggle", `must be a boolean or an integer, not a string; variable "TOGGLE" was substituted into the value`, ""}},
		},
		// Re-typed values that fit their type but not a keyword.
		{
			"maximum", rendered, listener(`, "port": "${HTTP_PORT}", "protocol": "http"`),
			map[string]string{"HTTP_PORT": "99999"},
			[]finding{{CodeSchema, "spec.listeners[name=http].port", `must be at most 65535; variable "HTTP_PORT" was substituted into the value`, ""}},
		},
		{
			"minimum", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u", "weight": "${WEIGHT}"}]}`),
			map[string]string{"WEIGHT": "-1"},
			[]finding{{CodeSchema, "spec.upstreams[name=u].weight", `must be at least 0; variable "WEIGHT" was substituted into the value`, ""}},
		},
		{
			"number minimum", synthetic, thing(`{"ratio": "${RATIO}"}`),
			map[string]string{"RATIO": "0.25"},
			[]finding{{CodeSchema, "spec.ratio", `must be at least 0.5; variable "RATIO" was substituted into the value`, ""}},
		},
		{
			"exclusiveMinimum", synthetic, thing(`{"count": "${COUNT}"}`),
			map[string]string{"COUNT": "0"},
			[]finding{{CodeSchema, "spec.count", `must be greater than 0; variable "COUNT" was substituted into the value`, ""}},
		},
		{
			"exclusiveMaximum", synthetic, thing(`{"count": "${COUNT}"}`),
			map[string]string{"COUNT": "100"},
			[]finding{{CodeSchema, "spec.count", `must be less than 100; variable "COUNT" was substituted into the value`, ""}},
		},
		{
			"multipleOf", synthetic, thing(`{"count": "${COUNT}"}`),
			map[string]string{"COUNT": "7"},
			[]finding{{CodeSchema, "spec.count", `must be a multiple of 5; variable "COUNT" was substituted into the value`, ""}},
		},
		{
			"oneOf more than one", synthetic, thing(`{"overlap": "${OVERLAP}"}`),
			map[string]string{"OVERLAP": "5"},
			[]finding{{CodeSchema, "spec.overlap", `matches more than one alternative; exactly one is allowed; variable "OVERLAP" was substituted into the value`, ""}},
		},
		// Substituted strings that fail a keyword.
		{
			"enum", rendered, listener(`, "port": 80, "protocol": "${PROTO}"`),
			map[string]string{"PROTO": "htp"},
			[]finding{{CodeSchema, "spec.listeners[name=http].protocol", `must be one of "http", "https"; variable "PROTO" was substituted into the value`, `did you mean "http"?`}},
		},
		{
			"const", synthetic, thing(`{"level": "${LEVEL}"}`),
			map[string]string{"LEVEL": "low"},
			[]finding{{CodeSchema, "spec.level", `must be "high"; variable "LEVEL" was substituted into the value`, ""}},
		},
		{
			"pattern", synthetic, thing(`{"code": "${CODE}"}`),
			map[string]string{"CODE": "ABC"},
			[]finding{{CodeSchema, "spec.code", `must match the pattern ^[a-z]+$; variable "CODE" was substituted into the value`, ""}},
		},
		{
			"minLength", rendered, envelope("Consumer", `{"credentials": {"jwt": [{"issuer": "i", "subject": "${SUBJECT}"}]}}`),
			map[string]string{"SUBJECT": ""},
			[]finding{{CodeSchema, "spec.credentials.jwt[issuer=i].subject", `must not be empty; variable "SUBJECT" was substituted into the value`, ""}},
		},
		{
			"maxLength", synthetic, thing(`{"anyNum": "${NUM}"}`),
			map[string]string{"NUM": "abcd"},
			[]finding{{CodeSchema, "spec.anyNum", `must be at most 3 characters long; variable "NUM" was substituted into the value`, ""}},
		},
		{
			"not", synthetic, thing(`{"notString": "${VALUE}"}`),
			map[string]string{"VALUE": "s"},
			[]finding{{CodeSchema, "spec.notString", `matches a form that is not allowed here; variable "VALUE" was substituted into the value`, ""}},
		},
		// Syntax texts of scalar definitions.
		{
			"Duration", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${TIMEOUT}"}`),
			map[string]string{"TIMEOUT": "5 seconds"},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variable "TIMEOUT" was substituted into the value`, ""}},
		},
		{
			"ByteSize pattern", rendered, gateway(`, "limits": {"maxRequestBodyBytes": "${SIZE}"}`),
			map[string]string{"SIZE": "10MB"},
			[]finding{{CodeSchema, "spec.limits.maxRequestBodyBytes", `must be an integer or quantity such as 10Mi; variable "SIZE" was substituted into the value`, ""}},
		},
		{
			"ByteSize minimum", rendered, gateway(`, "limits": {"maxRequestBodyBytes": "${SIZE}"}`),
			map[string]string{"SIZE": "-1"},
			[]finding{{CodeSchema, "spec.limits.maxRequestBodyBytes", `must be an integer or quantity such as 10Mi; variable "SIZE" was substituted into the value`, ""}},
		},
		{
			"Decimal", rendered, envelope("AIProvider", `{"dialect": "openai", "pricing": {"currency": "EUR", "version": "1", "models": [{"model": "m", "inputPerMillionTokens": "${PRICE}"}]}}`),
			map[string]string{"PRICE": "1,50"},
			[]finding{{CodeSchema, "spec.pricing.models[model=m].inputPerMillionTokens", `must be a decimal number written as a string, such as "1.50"; variable "PRICE" was substituted into the value`, ""}},
		},
		// Field syntaxes.
		{
			"OTLP endpoint", rendered, gateway(`, "telemetry": {"otlp": {"endpoint": "${OTLP_ENDPOINT}"}}`),
			map[string]string{"OTLP_ENDPOINT": "http://collector:4317/v1"},
			[]finding{{CodeSchema, "spec.telemetry.otlp.endpoint", `must be an http or https URL with a host and an optional port, no path other than /, and no user information, query or fragment; variable "OTLP_ENDPOINT" was substituted into the value`, ""}},
		},
		{
			"APIKey hash", rendered, envelope("Consumer", `{"credentials": {"apiKeys": [{"name": "k", "hash": "${KEY_HASH}"}]}}`),
			map[string]string{"KEY_HASH": "md5:abc"},
			[]finding{{CodeSchema, "spec.credentials.apiKeys[name=k].hash", `must be sha256: followed by 64 lower-case hexadecimal digits; variable "KEY_HASH" was substituted into the value`, ""}},
		},
		{
			"BasicCredential hash", rendered, envelope("Consumer", `{"credentials": {"basic": [{"username": "u", "hash": "${USER_HASH}"}]}}`),
			map[string]string{"USER_HASH": "plain"},
			[]finding{{CodeSchema, "spec.credentials.basic[username=u].hash", `must be pbkdf2-sha256:<salt>:<key> with a 22-character base64url salt and a 43-character base64url key; variable "USER_HASH" was substituted into the value`, ""}},
		},
		{
			"Pricing currency", rendered, envelope("AIProvider", `{"dialect": "openai", "pricing": {"currency": "${CURRENCY}", "version": "1"}}`),
			map[string]string{"CURRENCY": "eur"},
			[]finding{{CodeSchema, "spec.pricing.currency", `must be an upper-case ISO 4217 currency code such as EUR; variable "CURRENCY" was substituted into the value`, ""}},
		},
		{
			"CountryCode", rendered, policy("authz.geoip", `{"allow": ["${COUNTRY}"]}`),
			map[string]string{"COUNTRY": "de"},
			[]finding{{CodeSchema, "spec.config.allow[item=de]", `must be an upper-case ISO 3166-1 alpha-2 country code such as DE; variable "COUNTRY" was substituted into the value`, ""}},
		},
		// How the variables are named.
		{
			"partial substitution", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${TIMEOUT}s"}`),
			map[string]string{"TIMEOUT": "5 "},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variable "TIMEOUT" was substituted into the value`, ""}},
		},
		{
			"default value", rendered, listener(`, "port": "${HTTP_PORT:-eighty}", "protocol": "http"`), nil,
			[]finding{{CodeSchema, "spec.listeners[name=http].port", `must be an integer, not a string; variable "HTTP_PORT" was substituted into the value`, ""}},
		},
		{
			"two variables", rendered, gateway(`, "telemetry": {"otlp": {"endpoint": "http://${HOST}/${PATH}"}}`),
			map[string]string{"HOST": "collector", "PATH": "v1"},
			[]finding{{CodeSchema, "spec.telemetry.otlp.endpoint", `must be an http or https URL with a host and an optional port, no path other than /, and no user information, query or fragment; variables "HOST" and "PATH" were substituted into the value`, ""}},
		},
		{
			"one variable twice", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${T}${T}"}`),
			map[string]string{"T": "x"},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variable "T" was substituted into the value`, ""}},
		},
		{
			"more variables than named", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${A}${B}${C}${D}${E}${F}"}`),
			map[string]string{"A": "a", "B": "b", "C": "c", "D": "d", "E": "e", "F": "f"},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variables "A", "B", "C", "D" and 2 more were substituted into the value`, ""}},
		},
		{
			"four variables all named", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${A}${B}${C}${D}"}`),
			map[string]string{"A": "a", "B": "b", "C": "c", "D": "d"},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variables "A", "B", "C" and "D" were substituted into the value`, ""}},
		},
		{
			"five variables", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timeout": "${A}${B}${C}${D}${E}"}`),
			map[string]string{"A": "a", "B": "b", "C": "c", "D": "d", "E": "e"},
			[]finding{{CodeSchema, "spec.timeout", `must be a Go duration such as 50ms; variables "A", "B", "C", "D" and 1 more were substituted into the value`, ""}},
		},
		// Keyed-list and set uniqueness of substituted keys and elements.
		{
			"duplicate keys both substituted", rendered, envelope("Gateway", `{"listeners": [{"name": "${A}", "port": 80, "protocol": "http"}, {"name": "${B}", "port": 81, "protocol": "http"}]}`),
			map[string]string{"A": "web", "B": "web"},
			[]finding{{CodeSchema, "spec.listeners[name=web]", `duplicate entry: name "web" is already used by an earlier entry; variable "B" was substituted into the key; variable "A" was substituted into the earlier key`, ""}},
		},
		{
			"duplicate key substituted", rendered, envelope("Gateway", `{"listeners": [{"name": "web", "port": 80, "protocol": "http"}, {"name": "${B}", "port": 81, "protocol": "http"}]}`),
			map[string]string{"B": "web"},
			[]finding{{CodeSchema, "spec.listeners[name=web]", `duplicate entry: name "web" is already used by an earlier entry; variable "B" was substituted into the key`, ""}},
		},
		{
			"earlier key substituted", rendered, envelope("Gateway", `{"listeners": [{"name": "${A}", "port": 80, "protocol": "http"}, {"name": "web", "port": 81, "protocol": "http"}]}`),
			map[string]string{"A": "web"},
			[]finding{{CodeSchema, "spec.listeners[name=web]", `duplicate entry: name "web" is already used by an earlier entry; variable "A" was substituted into the earlier key`, ""}},
		},
		{
			"duplicate scalar elements", rendered, gateway(`, "trustedProxies": ["${P1}", "${P2}"]`),
			map[string]string{"P1": "10.0.0.0/8", "P2": "10.0.0.0/8"},
			[]finding{{CodeSchema, "spec.trustedProxies[item=10.0.0.0/8]", `duplicate element "10.0.0.0/8" in a set; variable "P2" was substituted into the element; variable "P1" was substituted into the earlier element`, ""}},
		},
		{
			"duplicate object elements", rendered, policy("auth.mtls", `{"caCertificate": {"secretRef": {"provider": "env", "name": "CA"}}, "subjects": [{"subject": "${S1}"}, {"subject": "CN=${S2}"}, {"subject": "${S1}"}]}`),
			map[string]string{"S1": "CN=a", "S2": "a"},
			[]finding{
				{CodeSchema, `spec.config.subjects[item={"subject":"CN=a"}]`, `duplicate element {"subject":"CN=a"} in a set; variable "S2" was substituted into the element; variable "S1" was substituted into the earlier element`, ""},
				{CodeSchema, `spec.config.subjects[item={"subject":"CN=a"}]`, `duplicate element {"subject":"CN=a"} in a set; variable "S1" was substituted into the element; variable "S1" was substituted into the earlier element`, ""},
			},
		},
		{
			"duplicate elements with a variable in two scalars", synthetic, thing(`{"set": [{"a": "${X}", "b": ["${X}", "${Y}"]}, {"a": "1", "b": ["1", "2"]}]}`),
			map[string]string{"X": "1", "Y": "2"},
			[]finding{{CodeSchema, `spec.set[item={"a":"1","b":["1","2"]}]`, `duplicate element {"a":"1","b":["1","2"]} in a set; variables "X" and "Y" were substituted into the earlier element`, ""}},
		},
		{
			"synthetic duplicate keys", synthetic, thing(`{"keyed": [{"name": "${K1}"}, {"name": "${K2}"}]}`),
			map[string]string{"K1": "x", "K2": "x"},
			[]finding{{CodeSchema, "spec.keyed[name=x]", `duplicate entry: name "x" is already used by an earlier entry; variable "K2" was substituted into the key; variable "K1" was substituted into the earlier key`, ""}},
		},
		// Outside the class: findings about keys and members name no
		// variable, even when the member's value was substituted.
		{
			"reserved key with a substituted value", rendered, `{"apiVersion": "ruralz/v1alpha1", "kind": "Route", "metadata": {"name": "x", "labels": {"ruralz.io/team": "${TEAM}"}}, "spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`,
			map[string]string{"TEAM": "a"},
			[]finding{{CodeSchema, `metadata.labels["ruralz.io/team"]`, `label key "ruralz.io/team" uses the reserved prefix ruralz.io/`, ""}},
		},
		{
			"unknown member with a substituted value", rendered, envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "timout": "${TIMEOUT}"}`),
			map[string]string{"TIMEOUT": "1s"},
			[]finding{{CodeUnknownField, "spec.timout", `unknown field "timout"`, `did you mean "timeout"?`}},
		},
		{
			"required beside a substituted value", rendered, envelope("Gateway", `{"listeners": [{"name": "http", "port": "${HTTP_PORT}"}]}`),
			map[string]string{"HTTP_PORT": "80"},
			[]finding{{CodeSchema, "spec.listeners[name=http]", "missing required field protocol", ""}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := findings(afterSubstitution(t, tc.v, tc.src, tc.vars))
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

// TestSubstitutedVariablesOnNodes covers the members of the class that
// stage E cannot produce from a string, with tree.Node.Vars set on the
// node: a boolean or a number at an anyOf none of whose branches admits it
// (alternatives and emitType), and a variable name longer than a message
// quotes.
func TestSubstitutedVariablesOnNodes(t *testing.T) {
	long := strings.Repeat("V", maxQuoted+10)
	for _, tc := range []struct {
		name string
		v    *View
		src  string
		// at is the path of the node Vars is set on.
		at   diag.Path
		vars []string
		want finding
	}{
		{
			"IntOrString anyOf type", testView(t),
			envelope("Upstream", `{"protocol": "http", "discovery": {"type": "dns", "service": "a.example", "port": true}}`),
			diag.Path{diag.Field("spec"), diag.Field("discovery"), diag.Field("port")},
			[]string{"PORT"},
			finding{CodeSchema, "spec.discovery.port", `must be an integer or a string, not a boolean; variable "PORT" was substituted into the value`, "quote the value to make it a string"},
		},
		{
			"anyOf branch type", syntheticView(t), thing(`{"anyNum": 1.5}`),
			diag.Path{diag.Field("spec"), diag.Field("anyNum")},
			[]string{"NUM"},
			finding{CodeSchema, "spec.anyNum", `must be an integer or a string, not a number; variable "NUM" was substituted into the value`, "quote the value to make it a string"},
		},
		{
			"long variable name", syntheticView(t), thing(`{"count": 7}`),
			diag.Path{diag.Field("spec"), diag.Field("count")},
			[]string{long},
			finding{CodeSchema, "spec.count", `must be a multiple of 5; variable ` + quote(long) + ` was substituted into the value`, ""},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "f.json", tc.src)
			n, ok := res.Root.At(tc.at)
			if !ok {
				t.Fatalf("no node at %s", tc.at)
			}
			n.Vars = tc.vars
			got := findings(tc.v.Validate(res, files))
			if len(got) != 1 || got[0] != tc.want {
				t.Errorf("Validate() = %+v, want %+v", got, tc.want)
			}
		})
	}
	if got := quote(long); !strings.HasSuffix(got, `..."`) {
		t.Errorf("quote(%d-byte name) = %s, want it shortened", len(long), got)
	}
}

// TestSubstitutedDuplicateQuoted pins how a duplicate keyed-list key or
// set element that stage E substituted is worded: the message quotes the
// key or element as its path shows it (the path holds it anyway), and the
// clause about the variables names them and never contains a value. So a
// finding about a substituted key or element does contain the substituted
// value, in its path and its message.
func TestSubstitutedDuplicateQuoted(t *testing.T) {
	v := testView(t)
	const secret = "hunter2-s3cr3t"
	for _, tc := range []struct {
		name, src, path, msg string
	}{
		{
			"set element", envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http"}], "trustedProxies": ["${DB_PASSWORD}", "${DB_PASSWORD}"]}`),
			"spec.trustedProxies[item=" + secret + "]",
			`duplicate element "` + secret + `" in a set; variable "DB_PASSWORD" was substituted into the element; variable "DB_PASSWORD" was substituted into the earlier element`,
		},
		{
			"keyed-list key", envelope("Gateway", `{"listeners": [{"name": "${DB_PASSWORD}", "port": 1, "protocol": "http"}, {"name": "${DB_PASSWORD}", "port": 2, "protocol": "http"}]}`),
			"spec.listeners[name=" + secret + "]",
			`duplicate entry: name "` + secret + `" is already used by an earlier entry; variable "DB_PASSWORD" was substituted into the key; variable "DB_PASSWORD" was substituted into the earlier key`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := afterSubstitution(t, v, tc.src, map[string]string{"DB_PASSWORD": secret})
			if len(got) != 1 || got[0].Path.String() != tc.path || got[0].Message != tc.msg {
				t.Fatalf("Validate() = %s, want %s: %s", texts(got), tc.path, tc.msg)
			}
			_, clause, _ := strings.Cut(got[0].Message, ";")
			if strings.Contains(clause, secret) {
				t.Errorf("variables clause %q contains the value", clause)
			}
		})
	}
}

// TestSubstitutedClause words the variables clause.
func TestSubstitutedClause(t *testing.T) {
	for _, tc := range []struct {
		vars []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"A"}, `; variable "A" was substituted into the value`},
		{[]string{"A", "B"}, `; variables "A" and "B" were substituted into the value`},
		{[]string{"A", "B", "C"}, `; variables "A", "B" and "C" were substituted into the value`},
		{[]string{"A", "B", "C", "D"}, `; variables "A", "B", "C" and "D" were substituted into the value`},
		{[]string{"A", "B", "C", "D", "E"}, `; variables "A", "B", "C", "D" and 1 more were substituted into the value`},
		{[]string{"A", "B", "C", "D", "E", "F", "G"}, `; variables "A", "B", "C", "D" and 3 more were substituted into the value`},
	} {
		if got := substituted(tc.vars, "the value"); got != tc.want {
			t.Errorf("substituted(%q) = %q, want %q", tc.vars, got, tc.want)
		}
	}
}
