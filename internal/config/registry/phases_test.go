// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for Phase selection and the scope rule: 02 req 13, 13a, 14 and
// 35.4 with tests 16 (RZ-CFG-020) and 17; 07 req 1 to 3; 06 req 1, 56 and
// 68; 05 req 75 and T 97 (cache at G or U); R-40 (cache Phases).

var (
	gw  = phase.ScopeGateway
	rt  = phase.ScopeRoute
	ups = phase.ScopeUpstream
)

// fakeOracle answers ReferencesRequestBody from a table.
type fakeOracle map[string]error

func (f fakeOracle) ReferencesRequestBody(src string) (bool, error) {
	err, ok := f[src]
	return ok && err == nil, err
}

// decode builds a typed config the way stage G does.
func decode(t *testing.T, typ v1alpha1.PolicyType, js string) any {
	t.Helper()
	c, err := New().NewConfig(typ)
	if err != nil {
		t.Fatal(err)
	}
	if js != "" {
		if err := json.Unmarshal([]byte(js), c); err != nil {
			t.Fatalf("decode %s config %s: %v", typ, js, err)
		}
	}
	return c
}

// phasesOf runs Phase selection for one attachment.
func phasesOf(t *testing.T, typ v1alpha1.PolicyType, scope phase.Scope, cfg any, body BodyOracle) (phase.Set, error) {
	t.Helper()
	return New().Phases(Attachment{Policy: policy("pol", typ), Config: cfg, Scope: scope}, body)
}

// TestPhasesScopeRule is 02 test 16 (RZ-CFG-020 fixtures) with 07 req 3,
// 05 req 75 and T 97, and 06 req 1 and 68: a type at a scope outside its
// registry Scopes fails with a ScopeError carrying CodeScope.
func TestPhasesScopeRule(t *testing.T) {
	tests := []struct {
		typ   v1alpha1.PolicyType
		scope phase.Scope
		msg   string
	}{
		{v1alpha1.PolicyTypeValidationJSONSchema, gw, `Policy type "validation.json-schema" is not allowed at Gateway scope (allowed: Route)`},
		{v1alpha1.PolicyTypeValidationJSONSchema, ups, `Policy type "validation.json-schema" is not allowed at Upstream scope (allowed: Route)`},
		{v1alpha1.PolicyTypeCache, gw, `Policy type "cache" is not allowed at Gateway scope (allowed: Route)`},
		{v1alpha1.PolicyTypeCache, ups, `Policy type "cache" is not allowed at Upstream scope (allowed: Route)`},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, rt, `Policy type "auth.upstream-oauth2" is not allowed at Route scope (allowed: Upstream)`},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, gw, `Policy type "auth.upstream-oauth2" is not allowed at Gateway scope (allowed: Upstream)`},
		{v1alpha1.PolicyTypeAuthJWT, ups, `Policy type "auth.jwt" is not allowed at Upstream scope (allowed: Gateway, Route)`},
		{v1alpha1.PolicyTypeAuthzCEL, ups, `Policy type "authz.cel" is not allowed at Upstream scope (allowed: Gateway, Route)`},
		{v1alpha1.PolicyTypeRateLimit, ups, `Policy type "ratelimit" is not allowed at Upstream scope (allowed: Gateway, Route)`},
		{v1alpha1.PolicyTypeAISemanticCache, gw, `Policy type "ai.semantic-cache" is not allowed at Gateway scope (allowed: Route)`},
	}
	for _, tt := range tests {
		t.Run(string(tt.typ)+"@"+tt.scope.String(), func(t *testing.T) {
			_, err := phasesOf(t, tt.typ, tt.scope, nil, nil)
			wantScopeError(t, err, tt.msg)
		})
	}
}

// wantScopeError checks an RZ-CFG-020 error and its diagnostic.
func wantScopeError(t *testing.T, err error, msg string) {
	t.Helper()
	var se *ScopeError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *ScopeError", err)
	}
	if code, ok := errcode.CodeOf(err); !ok || code != CodeScope {
		t.Errorf("CodeOf = %q, %v; want %s", code, ok, CodeScope)
	}
	if se.Error() != msg {
		t.Errorf("message = %q\nwant      %q", se.Error(), msg)
	}
	path := diag.Path{diag.Field("spec"), diag.Field("policies"), diag.Keyed("name", "pol")}
	d := se.Diagnostic(path)
	if d.Code != CodeScope || d.Severity != diag.SeverityError || d.Message != msg || d.Path.String() != "spec.policies[name=pol]" {
		t.Errorf("Diagnostic = %+v", d)
	}
}

// TestPhasesAllowedScopes: at every allowed scope a fixed-Phase type runs
// in its declared Phases; Gateway and Route use the client-leg Phases,
// Upstream the upstream-leg Phases (02 req 14); cache stays in
// onRequestHeaders and onResponse (R-40).
func TestPhasesAllowedScopes(t *testing.T) {
	selected := map[v1alpha1.PolicyType]bool{
		v1alpha1.PolicyTypeAuthzCEL: true, v1alpha1.PolicyTypeAuthzOPA: true, v1alpha1.PolicyTypeAuthzCedar: true,
		v1alpha1.PolicyTypeHeaders: true, v1alpha1.PolicyTypeTransformRequest: true,
		v1alpha1.PolicyTypeTransformResponse: true, v1alpha1.PolicyTypePlugin: true,
	}
	reg := New()
	for _, e := range reg.Entries() {
		if selected[e.Type] {
			continue
		}
		for _, s := range []phase.Scope{gw, rt, ups} {
			got, err := reg.Phases(Attachment{Policy: policy("pol", e.Type), Config: e.NewConfig(), Scope: s}, nil)
			if !e.Scopes.Has(s) {
				if err == nil {
					t.Errorf("%s at %s: no error", e.Type, s)
				}
				continue
			}
			w := e.Phases
			if s == ups {
				w = e.UpstreamPhases
			}
			if err != nil || got != w {
				t.Errorf("%s at %s: Phases = %v, %v; want %v", e.Type, s, got.Phases(), err, w.Phases())
			}
		}
	}
	got, err := phasesOf(t, v1alpha1.PolicyTypeCache, rt, nil, nil)
	if err != nil || got != phase.Of(phase.OnRequestHeaders, phase.OnResponse) {
		t.Errorf("cache Phases = %v, %v; want onRequestHeaders and onResponse (R-40)", got.Phases(), err)
	}
}

// TestPhasesSelection is 02 test 17 with the 07 req 2 table: headers and
// transform.* run only in the Phases their config uses; an empty config
// subscribes to nothing.
func TestPhasesSelection(t *testing.T) {
	const (
		reqSet  = `{"request":{"set":[{"name":"x-a","value":"1"}]}}`
		respSet = `{"response":{"set":[{"name":"x-b","value":"2"}]}}`
		both    = `{"request":{"set":[{"name":"x-a","value":"1"}]},"response":{"set":[{"name":"x-b","value":"2"}]}}`
	)
	tests := []struct {
		name  string
		typ   v1alpha1.PolicyType
		scope phase.Scope
		cfg   string
		want  phase.Set
	}{
		{"headers response only at G", v1alpha1.PolicyTypeHeaders, gw, respSet, phase.Of(phase.OnResponse)},
		{"headers request only at U", v1alpha1.PolicyTypeHeaders, ups, reqSet, phase.Of(phase.OnUpstreamRequest)},
		{"headers response only at U", v1alpha1.PolicyTypeHeaders, ups, respSet, phase.Of(phase.OnUpstreamResponseHeaders)},
		{"headers both at R", v1alpha1.PolicyTypeHeaders, rt, both, phase.Of(phase.OnRequestHeaders, phase.OnResponse)},
		{"headers both at U", v1alpha1.PolicyTypeHeaders, ups, both, phase.Of(phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders)},
		{"headers request only at R", v1alpha1.PolicyTypeHeaders, rt, reqSet, phase.Of(phase.OnRequestHeaders)},
		{"headers empty config", v1alpha1.PolicyTypeHeaders, rt, `{}`, 0},
		{"headers empty sides", v1alpha1.PolicyTypeHeaders, rt, `{"request":{},"response":{"set":[]}}`, 0},
		{"transform.request at U", v1alpha1.PolicyTypeTransformRequest, ups, `{"set":[{"target":"header","name":"x","valueExpression":"'1'"}]}`, phase.Of(phase.OnUpstreamRequest)},
		{"transform.request body at R", v1alpha1.PolicyTypeTransformRequest, rt, `{"body":"request.body"}`, phase.Of(phase.OnRequestBody)},
		{"transform.request remove at G", v1alpha1.PolicyTypeTransformRequest, gw, `{"remove":[{"target":"query","name":"q"}]}`, phase.Of(phase.OnRequestBody)},
		{"transform.request arrayOps at G", v1alpha1.PolicyTypeTransformRequest, gw, `{"arrayOps":[{"op":"delete","from":"a"}]}`, phase.Of(phase.OnRequestBody)},
		{"transform.request replace at U", v1alpha1.PolicyTypeTransformRequest, ups, `{"replace":[{"pattern":"a"}]}`, phase.Of(phase.OnUpstreamRequest)},
		{"transform.request contentType only", v1alpha1.PolicyTypeTransformRequest, rt, `{"contentType":"application/json"}`, 0},
		{"transform.request empty", v1alpha1.PolicyTypeTransformRequest, ups, `{}`, 0},
		{"transform.response at R", v1alpha1.PolicyTypeTransformResponse, rt, `{"set":[{"target":"body","name":"a","valueExpression":"1"}]}`, phase.Of(phase.OnResponse)},
		{"transform.response body at U", v1alpha1.PolicyTypeTransformResponse, ups, `{"body":"response.body"}`, phase.Of(phase.OnUpstreamResponseBody)},
		{"transform.response empty", v1alpha1.PolicyTypeTransformResponse, gw, `{}`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := phasesOf(t, tt.typ, tt.scope, decode(t, tt.typ, tt.cfg), nil)
			if err != nil || got != tt.want {
				t.Errorf("Phases = %v, %v; want %v", got.Phases(), err, tt.want.Phases())
			}
		})
	}
}

// TestPhasesHeadersAddRemove: add and remove (07 req 2 and 20) count as
// request and response ops like set. R-62 keeps these fields, so a schema
// without them fails here instead of skipping.
func TestPhasesHeadersAddRemove(t *testing.T) {
	tests := []struct {
		cfg   string
		scope phase.Scope
		want  phase.Set
	}{
		{`{"request":{"add":[{"name":"x","value":"1"}]}}`, rt, phase.Of(phase.OnRequestHeaders)},
		{`{"request":{"remove":["x"]}}`, ups, phase.Of(phase.OnUpstreamRequest)},
		{`{"response":{"remove":["server"]}}`, gw, phase.Of(phase.OnResponse)},
		{`{"response":{"add":[{"name":"x","value":""}]}}`, ups, phase.Of(phase.OnUpstreamResponseHeaders)},
	}
	for _, tt := range tests {
		got, err := phasesOf(t, v1alpha1.PolicyTypeHeaders, tt.scope, decode(t, v1alpha1.PolicyTypeHeaders, tt.cfg), nil)
		if err != nil || got != tt.want {
			t.Errorf("%s at %s: Phases = %v, %v; want %v", tt.cfg, tt.scope, got.Phases(), err, tt.want.Phases())
		}
	}
}

// TestPhasesAuthzCEL is 02 req 13a (test 17 "body oracle both ways") and
// 06 req 56: onRequestBody iff the rule references request.body, never
// both; a rule without an oracle is ErrNoBodyOracle, not a guess; authz.opa
// and authz.cedar stay in onRequestHeaders in M1.
func TestPhasesAuthzCEL(t *testing.T) {
	oracle := fakeOracle{
		`request.body.amount < 100`: nil,
		`bad(`:                      errors.New("syntax error"),
	}
	headers, body := phase.Of(phase.OnRequestHeaders), phase.Of(phase.OnRequestBody)
	tests := []struct {
		name    string
		cfg     any
		oracle  BodyOracle
		want    phase.Set
		wantErr error
	}{
		{"body rule", &v1alpha1.AuthzCELConfig{Rule: `request.body.amount < 100`}, oracle, body, nil},
		{"body rule by value", v1alpha1.AuthzCELConfig{Rule: `request.body.amount < 100`}, oracle, body, nil},
		{"header rule", &v1alpha1.AuthzCELConfig{Rule: `"admin" in auth.claims.roles`}, oracle, headers, nil},
		{"rule the oracle cannot check", &v1alpha1.AuthzCELConfig{Rule: `bad(`}, oracle, headers, nil},
		{"body rule without oracle", &v1alpha1.AuthzCELConfig{Rule: `request.body.amount < 100`}, nil, 0, ErrNoBodyOracle},
		{"header rule without oracle", &v1alpha1.AuthzCELConfig{Rule: `"admin" in auth.claims.roles`}, nil, 0, ErrNoBodyOracle},
		{"empty rule", &v1alpha1.AuthzCELConfig{}, oracle, headers, nil},
		{"empty rule without oracle", &v1alpha1.AuthzCELConfig{}, nil, headers, nil},
		{"nil config", nil, oracle, headers, nil},
		{"nil config without oracle", nil, nil, headers, nil},
		{"typed nil config", (*v1alpha1.AuthzCELConfig)(nil), oracle, headers, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, s := range []phase.Scope{gw, rt} {
				got, err := phasesOf(t, v1alpha1.PolicyTypeAuthzCEL, s, tt.cfg, tt.oracle)
				if tt.wantErr != nil {
					if !errors.Is(err, tt.wantErr) || errors.As(err, new(*ScopeError)) || !strings.Contains(err.Error(), `"pol"`) {
						t.Errorf("at %s: err = %v, want %v naming the Policy", s, err, tt.wantErr)
					}
					continue
				}
				if err != nil || got != tt.want {
					t.Errorf("at %s: Phases = %v, %v; want %v", s, got.Phases(), err, tt.want.Phases())
				}
			}
		})
	}
	for _, typ := range []v1alpha1.PolicyType{v1alpha1.PolicyTypeAuthzOPA, v1alpha1.PolicyTypeAuthzCedar} {
		got, err := phasesOf(t, typ, rt, decode(t, typ, `{}`), oracle)
		if err != nil || got != headers {
			t.Errorf("%s: Phases = %v, %v; want onRequestHeaders", typ, got.Phases(), err)
		}
	}
}

// TestPhasesPlugin is 02 req 14 and test 16: a plugin runs in its Plugin's
// Phases; any Phase outside the scope's leg is RZ-CFG-020 naming the
// Plugin and the offending Phases.
func TestPhasesPlugin(t *testing.T) {
	tests := []struct {
		name   string
		phases []v1alpha1.Phase
		scope  phase.Scope
		want   phase.Set
		errMsg string
	}{
		{
			"client Phases at Gateway",
			[]v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnResponse},
			gw,
			phase.Of(phase.OnRequestHeaders, phase.OnResponse), "",
		},
		{
			"onChunk at Route",
			[]v1alpha1.Phase{v1alpha1.PhaseOnRequestBody, v1alpha1.PhaseOnChunk, v1alpha1.PhaseOnLog},
			rt,
			phase.Of(phase.OnRequestBody, phase.OnChunk, phase.OnLog), "",
		},
		{
			"upstream Phases at Upstream",
			[]v1alpha1.Phase{v1alpha1.PhaseOnUpstreamRequest, v1alpha1.PhaseOnUpstreamResponseBody, v1alpha1.PhaseOnChunk},
			ups,
			phase.Of(phase.OnUpstreamRequest, phase.OnUpstreamResponseBody, phase.OnChunk), "",
		},
		{
			"onResponse at Upstream",
			[]v1alpha1.Phase{v1alpha1.PhaseOnUpstreamRequest, v1alpha1.PhaseOnResponse},
			ups, 0,
			`Plugin "geo-block" Phases [onResponse] are not allowed at Upstream scope`,
		},
		{
			"onUpstreamRequest at Route",
			[]v1alpha1.Phase{v1alpha1.PhaseOnRequestHeaders, v1alpha1.PhaseOnUpstreamRequest, v1alpha1.PhaseOnUpstreamResponseHeaders},
			rt, 0,
			`Plugin "geo-block" Phases [onUpstreamRequest, onUpstreamResponseHeaders] are not allowed at Route scope`,
		},
		{
			"unknown Phase",
			[]v1alpha1.Phase{"onStream"},
			gw, 0,
			`Plugin "geo-block" Phases [onStream] are not allowed at Gateway scope`,
		},
		{"no Phases", nil, rt, 0, ""},
	}
	reg := New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := policy("pol", v1alpha1.PolicyTypePlugin)
			p.Spec.Plugin = "geo-block"
			got, err := reg.Phases(Attachment{Policy: p, Config: decode(t, v1alpha1.PolicyTypePlugin, `{"x":1}`), Scope: tt.scope, PluginPhases: tt.phases}, nil)
			if tt.errMsg != "" {
				wantScopeError(t, err, tt.errMsg)
				return
			}
			if err != nil || got != tt.want {
				t.Errorf("Phases = %v, %v; want %v", got.Phases(), err, tt.want.Phases())
			}
		})
	}
}

// TestPhasesErrors: an unknown type, the zero scope and a Config of the
// wrong type are programming errors, not RZ-CFG-020.
func TestPhasesErrors(t *testing.T) {
	reg := New()
	_, err := reg.Phases(Attachment{Policy: policy("pol", "rateLimit"), Scope: rt}, nil)
	if !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v, want ErrUnknownType", err)
	}
	if _, err := reg.Phases(Attachment{Scope: rt}, nil); err == nil || errors.As(err, new(*ScopeError)) {
		t.Errorf("nil Policy: %v", err)
	}
	_, err = reg.Phases(Attachment{Policy: policy("pol", v1alpha1.PolicyTypeCORS)}, nil)
	if err == nil || errors.As(err, new(*ScopeError)) || !strings.Contains(err.Error(), "no attachment scope") {
		t.Errorf("zero scope: %v", err)
	}
	// Every type, fixed-Phase ones included, rejects a Config of another
	// type, before the scope rule; it accepts its own type as a pointer, a
	// typed nil or a value.
	for _, e := range reg.Entries() {
		var wrong any = &v1alpha1.CORSConfig{}
		if e.Type == v1alpha1.PolicyTypeCORS {
			wrong = &v1alpha1.RateLimitConfig{}
		}
		for _, s := range []phase.Scope{gw, rt, ups} {
			_, err := reg.Phases(Attachment{Policy: policy("pol", e.Type), Scope: s, Config: wrong}, fakeOracle{})
			if err == nil || errors.As(err, new(*ScopeError)) || !strings.Contains(err.Error(), reflect.TypeOf(wrong).String()) {
				t.Errorf("%s at %s with %T: %v; want a config type error", e.Type, s, wrong, err)
			}
		}
		own := e.NewConfig()
		for _, cfg := range []any{own, reflect.Zero(reflect.TypeOf(own)).Interface(), reflect.ValueOf(own).Elem().Interface()} {
			_, err := reg.Phases(Attachment{Policy: policy("pol", e.Type), Scope: rt, Config: cfg}, fakeOracle{})
			if err != nil && !errors.As(err, new(*ScopeError)) {
				t.Errorf("%s with its own config %T: %v", e.Type, cfg, err)
			}
		}
	}
	// A transform.request config handed to transform.response is wrong too.
	_, err = reg.Phases(Attachment{
		Policy: policy("pol", v1alpha1.PolicyTypeTransformResponse), Scope: rt,
		Config: &v1alpha1.TransformRequestConfig{Body: "x"},
	}, nil)
	if err == nil {
		t.Error("transform.response accepted a transform.request config")
	}
	// Nil and typed-nil configs read as empty.
	for _, cfg := range []any{nil, (*v1alpha1.HeadersConfig)(nil), (*v1alpha1.TransformRequestConfig)(nil), (*v1alpha1.TransformResponseConfig)(nil)} {
		for _, typ := range []v1alpha1.PolicyType{v1alpha1.PolicyTypeHeaders, v1alpha1.PolicyTypeTransformRequest, v1alpha1.PolicyTypeTransformResponse} {
			if cfg != nil && reflect.TypeOf(cfg) != reflect.TypeOf(decode(t, typ, "")) {
				continue
			}
			got, err := reg.Phases(Attachment{Policy: policy("pol", typ), Scope: rt, Config: cfg}, nil)
			if err != nil || !got.Empty() {
				t.Errorf("%s with %T(nil): %v, %v; want no Phases", typ, cfg, got.Phases(), err)
			}
		}
	}
	// Values of the config type are accepted like pointers.
	got, err := reg.Phases(Attachment{
		Policy: policy("pol", v1alpha1.PolicyTypeTransformResponse), Scope: rt,
		Config: v1alpha1.TransformResponseConfig{Body: "response.body"},
	}, nil)
	if err != nil || got != phase.Of(phase.OnResponse) {
		t.Errorf("value config: %v, %v", got.Phases(), err)
	}
	got, err = reg.Phases(Attachment{
		Policy: policy("pol", v1alpha1.PolicyTypeHeaders), Scope: gw,
		Config: v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{{Name: "x"}}}},
	}, nil)
	if err != nil || got != phase.Of(phase.OnResponse) {
		t.Errorf("value headers config: %v, %v", got.Phases(), err)
	}
}

// TestScopeErrorText covers the message forms and the scope spellings.
func TestScopeErrorText(t *testing.T) {
	tests := []struct {
		err  *ScopeError
		want string
	}{
		{&ScopeError{Type: "cache", Scope: gw, Allowed: phase.Scopes(rt)}, `Policy type "cache" is not allowed at Gateway scope (allowed: Route)`},
		{&ScopeError{Type: "x", Scope: phase.ScopeNone}, `Policy type "x" is not allowed at no scope (allowed: none)`},
		{&ScopeError{Type: "plugin", Scope: rt, Plugin: "p", Phases: []v1alpha1.Phase{"onLog"}}, `Plugin "p" Phases [onLog] are not allowed at Route scope`},
	}
	for _, tt := range tests {
		if got := tt.err.Error(); got != tt.want {
			t.Errorf("Error() = %q, want %q", got, tt.want)
		}
	}
}
