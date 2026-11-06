// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Fixture kinds of the cel marker.
const (
	KindCELGood         Kind = "CELGood"
	KindCELMinLength    Kind = "CELMinLength"
	KindCELEmptyDefault Kind = "CELEmptyDefault"
)

// CELGood holds CEL fields.
type CELGood struct {
	Metadata Meta        `json:"metadata"`
	Spec     CELGoodSpec `json:"spec"`
}

// CELGoodSpec has an optional and a defaulted CEL field.
type CELGoodSpec struct {
	// When gates the resource.
	// +ruralz:cel=request:bool
	When string `json:"when,omitempty"`
	// Cost has a default.
	// +ruralz:default="1"
	// +ruralz:cel=request:int
	Cost *string `json:"cost,omitempty"`
}

// CELMinLength restates the minLength of a CEL field.
type CELMinLength struct {
	Metadata Meta             `json:"metadata"`
	Spec     CELMinLengthSpec `json:"spec"`
}

// CELMinLengthSpec has a CEL field with its own minLength marker.
type CELMinLengthSpec struct {
	// Rule is an expression.
	// +ruralz:minLength=1
	// +ruralz:cel=request:bool
	Rule string `json:"rule,omitempty"`
}

// CELEmptyDefault defaults a CEL field to the empty string.
type CELEmptyDefault struct {
	Metadata Meta                `json:"metadata"`
	Spec     CELEmptyDefaultSpec `json:"spec"`
}

// CELEmptyDefaultSpec has an empty default expression.
type CELEmptyDefaultSpec struct {
	// Key defaults to no expression.
	// +ruralz:default=""
	// +ruralz:cel=request:string
	Key *string `json:"key,omitempty"`
}

// celSources parses the fixtures of fixture_test.go and this file.
func celSources(t *testing.T) sourceInfo {
	t.Helper()
	info, err := parseSources([]string{"fixture_test.go", "cel_test.go"})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestCELMarkerEmitsMinLength checks the cel marker: beside x-ruralz-cel
// it emits minLength 1, in both views and with or without a default, so
// an empty expression fails stage F (docs/architecture/02-configuration-model.md
// "Allowed places").
func TestCELMarkerEmitsMinLength(t *testing.T) {
	info := celSources(t)
	want := map[string]string{
		"when": `{"description":"When gates the resource.","minLength":1,"type":"string","x-ruralz-cel":{"result":"bool","variables":["request"]}}`,
		"cost": `{"default":"1","description":"Cost has a default.","minLength":1,"type":"string","x-ruralz-cel":{"result":"int","variables":["request"]}}`,
	}
	for _, v := range []view{authoring, rendered} {
		out, err := generate(input{source: info, resources: []reflect.Type{reflect.TypeFor[CELGood]()}}, v)
		if err != nil {
			t.Fatal(err)
		}
		s := decode(t, out)
		for field, w := range want {
			got, err := json.Marshal(dig(t, s, "$defs", "CELGoodSpec", "properties", field))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != w {
				t.Errorf("view %v: %s = %s, want %s", v, field, got, w)
			}
		}
	}
}

// TestCELMarkerRefusals checks what the cel marker refuses: a minLength
// marker of its own, which would restate or weaken the rule, and a default
// that is the empty expression.
func TestCELMarkerRefusals(t *testing.T) {
	info := celSources(t)
	for _, c := range []struct {
		typ  reflect.Type
		want string
	}{
		{reflect.TypeFor[CELMinLength](), "a +ruralz:cel field takes no +ruralz:minLength"},
		{reflect.TypeFor[CELEmptyDefault](), "shorter than minLength 1"},
	} {
		_, err := generate(input{source: info, resources: []reflect.Type{c.typ}}, rendered)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want %q", c.typ.Name(), err, c.want)
		}
	}
}

// TestCELFieldsAreNonEmpty checks every x-ruralz-cel field of both
// committed views: each carries minLength 1, so no CEL field accepts an
// empty expression that a Node's Build would refuse or read as absent.
func TestCELFieldsAreNonEmpty(t *testing.T) {
	for _, v := range []view{authoring, rendered} {
		doc := committed(t, v)
		minLength := propertyKeyword(t, doc, "minLength")
		cel := propertyKeyword(t, doc, "x-ruralz-cel")
		if len(cel) == 0 {
			t.Fatalf("view %v: no x-ruralz-cel field", v)
		}
		for _, field := range slices.Sorted(maps.Keys(cel)) {
			if n, ok := minLength[field].(float64); !ok || n != 1 {
				t.Errorf("view %v: %s has minLength %v, want 1", v, field, minLength[field])
			}
		}
	}
}

// TestEmptyExpressionIsRefused checks the committed schema at stage F: an
// empty expression in a CEL field of each kind and of each Policy config
// that holds one is RZ-CFG-005 at the field, and the same field with an
// expression validates. A ring-hash Upstream with an empty hashKey and a
// step with an empty pathExpression are among them: the Node would read
// either as missing and refuse the Upstream or the step at activation.
func TestEmptyExpressionIsRefused(t *testing.T) {
	views := embeddedViews(t)
	const (
		upstream = `"protocol": "http", "endpoints": [{"address": "a.svc:80"}]`
		match    = `"match": {"path": {"prefix": "/r"}}`
		listener = `"listeners": [{"name": "http", "port": 8080, "protocol": "http"}]`
	)
	policy := func(typ, config string) string { return `"type": "` + typ + `", "config": ` + config }
	tests := []struct {
		name string
		kind v1alpha1.Kind
		// spec is the resource's spec members, EXPR standing for the
		// expression as a JSON string.
		spec, expr, at string
	}{
		{"Gateway accessLog.when", v1alpha1.KindGateway, listener + `, "telemetry": {"accessLog": {"when": EXPR}}`, `response.status >= 500`, "spec.telemetry.accessLog.when"},
		{"Route match.when", v1alpha1.KindRoute, `"match": {"path": {"prefix": "/r"}, "when": EXPR}, "upstreams": [{"name": "u"}]`, `request.method == "GET"`, "spec.match.when"},
		{"step pathExpression", v1alpha1.KindRoute, match + `, "composition": {"mode": "sequential", "steps": [{"name": "a", "upstream": "u", "pathExpression": EXPR}]}`, `"/a"`, "spec.composition.steps[name=a].pathExpression"},
		{"step when", v1alpha1.KindRoute, match + `, "composition": {"mode": "sequential", "steps": [{"name": "a", "upstream": "u", "path": "/a", "when": EXPR}]}`, `true`, "spec.composition.steps[name=a].when"},
		{"ring-hash hashKey", v1alpha1.KindUpstream, upstream + `, "loadBalancing": {"algorithm": "ring-hash", "hashKey": EXPR}`, `source.ip`, "spec.loadBalancing.hashKey"},
		{"retries.retryOn", v1alpha1.KindUpstream, upstream + `, "retries": {"retryOn": EXPR}`, `response.status == 503`, "spec.retries.retryOn"},
		{"circuitBreaker.failureWhen", v1alpha1.KindUpstream, upstream + `, "circuitBreaker": {"failureWhen": EXPR}`, `response.status >= 500`, "spec.circuitBreaker.failureWhen"},
		{"Policy when", v1alpha1.KindPolicy, policy("cors", `{}`) + `, "when": EXPR`, `request.method != "OPTIONS"`, "spec.when"},
		{"authz.cel rule", v1alpha1.KindPolicy, policy("authz.cel", `{"rule": EXPR}`), `consumer != null`, "spec.config.rule"},
		{"ratelimit key", v1alpha1.KindPolicy, policy("ratelimit", `{"key": EXPR, "limits": [{"requests": 1, "window": "1s"}]}`), `consumer.name`, "spec.config.key"},
		{"ratelimit cost", v1alpha1.KindPolicy, policy("ratelimit", `{"cost": EXPR, "limits": [{"requests": 1, "window": "1s"}]}`), `2`, "spec.config.cost"},
		{"ratelimit limits[].when", v1alpha1.KindPolicy, policy("ratelimit", `{"limits": [{"requests": 1, "window": "1s", "when": EXPR}]}`), `true`, "spec.config.limits[0].when"},
		{"quota key", v1alpha1.KindPolicy, policy("quota", `{"consumerQuota": "daily", "key": EXPR}`), `consumer.name`, "spec.config.key"},
		{"quota cost", v1alpha1.KindPolicy, policy("quota", `{"consumerQuota": "daily", "cost": EXPR}`), `2`, "spec.config.cost"},
		{"cache key", v1alpha1.KindPolicy, policy("cache", `{"key": EXPR}`), `consumer.name`, "spec.config.key"},
		{"headers valueExpression", v1alpha1.KindPolicy, policy("headers", `{"request": {"set": [{"name": "x-a", "valueExpression": EXPR}]}}`), `request.method`, "spec.config.request.set[name=x-a].valueExpression"},
		{"transform.request body", v1alpha1.KindPolicy, policy("transform.request", `{"body": EXPR}`), `request.body`, "spec.config.body"},
		{"transform.response valueExpression", v1alpha1.KindPolicy, policy("transform.response", `{"set": [{"target": "header", "name": "x-a", "valueExpression": EXPR}]}`), `"1"`, "spec.config.set[0].valueExpression"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resource := func(expression string) string {
				quoted, err := json.Marshal(expression)
				if err != nil {
					t.Fatal(err)
				}
				return `{"apiVersion": "ruralz/v1alpha1", "kind": "` + string(tt.kind) + `", "metadata": {"name": "r"}, "spec": {` +
					strings.Replace(tt.spec, "EXPR", string(quoted), 1) + `}}`
			}
			if got := validateJSON(t, views, tt.kind, resource(tt.expr)); len(got) != 0 {
				t.Fatalf("an expression: Validate = %q, want no finding", got)
			}
			want := []string{"RZ-CFG-005 " + tt.at}
			if got := validateJSON(t, views, tt.kind, resource("")); strings.Join(got, "|") != strings.Join(want, "|") {
				t.Errorf("an empty expression: Validate = %q, want %q", got, want)
			}
		})
	}
}
