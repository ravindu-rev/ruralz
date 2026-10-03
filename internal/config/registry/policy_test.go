// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Tests for the per-Policy rules: registry defaults and their
// materialization (01 req 36, 02 req 12 and test 8), the effective slot
// (01 req 40), filterClass (02 req 38, R-7), failureMode (02 req 37, 06
// req 1 and 53, foundation pack section 8.10) and served types (RZ-CFG-040).

func policy(name string, typ v1alpha1.PolicyType) *v1alpha1.Policy {
	return &v1alpha1.Policy{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: v1alpha1.KindPolicy},
		Metadata: v1alpha1.ObjectMeta{Name: name},
		Spec:     v1alpha1.PolicySpec{Type: typ},
	}
}

func ptr[T any](v T) *T { return &v }

// TestDefaults is 02 test 8: for each of the 23 types, absent slot,
// failureMode and filterClass materialize the registry values; explicit
// values are kept; plugin without filterClass gets custom; a second pass
// changes nothing (01 req 36 idempotence).
func TestDefaults(t *testing.T) {
	wantSlot := map[v1alpha1.PolicyType]string{
		v1alpha1.PolicyTypeAuthJWT: "auth", v1alpha1.PolicyTypeAuthAPIKey: "auth",
		v1alpha1.PolicyTypeAuthBasic: "auth", v1alpha1.PolicyTypeAuthMTLS: "auth",
		v1alpha1.PolicyTypeValidationJSONSchema: "validation", v1alpha1.PolicyTypeCORS: "cors",
		v1alpha1.PolicyTypeCache: "cache", v1alpha1.PolicyTypeAuthUpstreamOAuth2: "upstream-auth",
		v1alpha1.PolicyTypeAuthUpstreamSigV4: "upstream-auth", v1alpha1.PolicyTypeAISemanticCache: "semantic-cache",
	}
	wantOpen := map[v1alpha1.PolicyType]bool{
		v1alpha1.PolicyTypeRateLimit: true, v1alpha1.PolicyTypeQuota: true,
		v1alpha1.PolicyTypeCache: true, v1alpha1.PolicyTypeAISemanticCache: true,
	}
	reg := New()
	if n := len(reg.Types()); n != 23 {
		t.Fatalf("registry has %d types, 02 test 8 expects 23", n)
	}
	for _, e := range reg.Entries() {
		t.Run(string(e.Type), func(t *testing.T) {
			slot, ok := wantSlot[e.Type]
			if !ok {
				slot = "my-policy"
			}
			fm := v1alpha1.FailureModeClosed
			if wantOpen[e.Type] {
				fm = v1alpha1.FailureModeOpen
			}
			class := e.Class.V1alpha1()
			if e.Type == v1alpha1.PolicyTypePlugin {
				class = v1alpha1.FilterClassCustom
			}
			w := Defaults{Slot: slot, FailureMode: fm, FilterClass: class}
			if got := e.Defaults("my-policy"); got != w {
				t.Errorf("Defaults = %+v, want %+v", got, w)
			}

			p := policy("my-policy", e.Type)
			if !reg.Materialize(p) {
				t.Fatal("Materialize wrote nothing on a bare Policy")
			}
			if p.Spec.Slot != w.Slot || *p.Spec.FailureMode != w.FailureMode || *p.Spec.FilterClass != w.FilterClass {
				t.Errorf("materialized slot=%q failureMode=%q filterClass=%q, want %+v",
					p.Spec.Slot, *p.Spec.FailureMode, *p.Spec.FilterClass, w)
			}
			if reg.Materialize(p) {
				t.Error("second Materialize wrote again (not idempotent)")
			}
			// The materialized Policy passes the registry's own checks.
			if d, bad := reg.CheckFilterClass(p); bad {
				t.Errorf("materialized filterClass rejected: %s", d.Message)
			}
			if d, bad := reg.CheckFailureMode(p); bad {
				t.Errorf("materialized failureMode rejected: %s", d.Message)
			}

			// Explicit values are kept.
			q := policy("my-policy", e.Type)
			q.Spec.Slot = "explicit"
			q.Spec.FailureMode = ptr(v1alpha1.FailureModeClosed)
			q.Spec.FilterClass = ptr(v1alpha1.FilterClassAuthz)
			if reg.Materialize(q) {
				t.Error("Materialize overwrote authored values")
			}
			if q.Spec.Slot != "explicit" || *q.Spec.FailureMode != v1alpha1.FailureModeClosed || *q.Spec.FilterClass != v1alpha1.FilterClassAuthz {
				t.Errorf("authored values changed: %+v", q.Spec)
			}

			// A nameless Policy (stage D rejects it) gets no empty slot
			// for a type whose slot is its name, and a second call still
			// writes nothing (01 req 36).
			n := policy("", e.Type)
			if !reg.Materialize(n) {
				t.Fatal("Materialize wrote nothing on a nameless Policy")
			}
			if want := e.DefaultSlot(""); n.Spec.Slot != want {
				t.Errorf("nameless slot = %q, want %q", n.Spec.Slot, want)
			}
			if reg.Materialize(n) {
				t.Error("second Materialize of a nameless Policy wrote again")
			}
		})
	}
	if reg.Materialize(policy("x", "rateLimit")) {
		t.Error("Materialize wrote defaults for an unknown type")
	}
}

// TestEffectiveSlot is 01 req 40: spec.slot if set, else the registry
// slot, else metadata.name.
func TestEffectiveSlot(t *testing.T) {
	reg := New()
	tests := []struct {
		typ  v1alpha1.PolicyType
		slot string
		want string
	}{
		{v1alpha1.PolicyTypeAuthJWT, "", "auth"},
		{v1alpha1.PolicyTypeAuthAPIKey, "", "auth"},
		{v1alpha1.PolicyTypeAuthJWT, "partner-auth", "partner-auth"},
		{v1alpha1.PolicyTypeRateLimit, "", "rl"},
		{v1alpha1.PolicyTypeRateLimit, "edge-ratelimit", "edge-ratelimit"},
		{v1alpha1.PolicyTypeHeaders, "", "rl"},
		{v1alpha1.PolicyTypeCache, "", "cache"},
		{v1alpha1.PolicyTypeAISemanticCache, "", "semantic-cache"},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, "", "upstream-auth"},
		{v1alpha1.PolicyTypePlugin, "", "rl"},
	}
	for _, tt := range tests {
		e, _ := reg.Lookup(tt.typ)
		p := policy("rl", tt.typ)
		p.Spec.Slot = tt.slot
		if got := e.EffectiveSlot(p); got != tt.want {
			t.Errorf("%s slot %q: EffectiveSlot = %q, want %q", tt.typ, tt.slot, got, tt.want)
		}
	}
}

// TestCheckFilterClass is 02 req 38 and test 8 with R-7: a type other than
// plugin may only restate its registry class (RZ-CFG-005 otherwise); a
// plugin chooses any of the nine classes.
func TestCheckFilterClass(t *testing.T) {
	reg := New()
	tests := []struct {
		name  string
		typ   v1alpha1.PolicyType
		class *v1alpha1.FilterClass
		bad   bool
		want  phase.Class
	}{
		{"ratelimit absent", v1alpha1.PolicyTypeRateLimit, nil, false, phase.ClassAdmission},
		{"ratelimit restates admission", v1alpha1.PolicyTypeRateLimit, ptr(v1alpha1.FilterClassAdmission), false, phase.ClassAdmission},
		{"ratelimit auth", v1alpha1.PolicyTypeRateLimit, ptr(v1alpha1.FilterClassAuth), true, phase.ClassAdmission},
		{"auth.jwt custom", v1alpha1.PolicyTypeAuthJWT, ptr(v1alpha1.FilterClassCustom), true, phase.ClassAuth},
		{"headers transform", v1alpha1.PolicyTypeHeaders, ptr(v1alpha1.FilterClassTransform), false, phase.ClassTransform},
		{"cors not a class", v1alpha1.PolicyTypeCORS, ptr(v1alpha1.FilterClass("CORS")), true, phase.ClassCORS},
		{"plugin absent", v1alpha1.PolicyTypePlugin, nil, false, phase.ClassCustom},
		{"plugin authz", v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClassAuthz), false, phase.ClassAuthz},
		{"plugin upstream-auth", v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClassUpstreamAuth), false, phase.ClassUpstreamAuth},
		{"plugin not a class", v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClass("plugin")), true, phase.ClassCustom},
		{"unknown type", "rateLimit", ptr(v1alpha1.FilterClassAuth), false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := policy("pol", tt.typ)
			p.Spec.FilterClass = tt.class
			d, bad := reg.CheckFilterClass(p)
			if bad != tt.bad {
				t.Fatalf("CheckFilterClass = %v (%q), want %v", bad, d.Message, tt.bad)
			}
			if bad {
				wantDiag(t, d, CodeFilterClass, "pol", "spec.filterClass")
			}
			if e, ok := reg.Lookup(tt.typ); ok {
				if got := e.EffectiveClass(p); got != tt.want {
					t.Errorf("EffectiveClass = %v, want %v", got, tt.want)
				}
			}
		})
	}
	p := policy("pol", v1alpha1.PolicyTypeRateLimit)
	p.Spec.FilterClass = ptr(v1alpha1.FilterClassAuth)
	d, _ := reg.CheckFilterClass(p)
	if w := `filterClass "auth" is not allowed for Policy type "ratelimit": its registry class is "admission", and only a plugin Policy chooses its class`; d.Message != w {
		t.Errorf("message = %q\nwant      %q", d.Message, w)
	}
}

// wantDiag checks the registry-owned parts of a diagnostic.
func wantDiag(t *testing.T, d diag.Diagnostic, code, name, path string) {
	t.Helper()
	if d.Code != code || d.Severity != diag.SeverityError {
		t.Errorf("code %s severity %s, want %s error", d.Code, d.Severity, code)
	}
	if _, ok := errcode.Lookup(d.Code); !ok {
		t.Errorf("code %s is not registered", d.Code)
	}
	if d.Resource == nil || d.Resource.Kind != "Policy" || d.Resource.Name != name {
		t.Errorf("resource = %+v, want Policy/%s", d.Resource, name)
	}
	if got := d.Path.String(); got != path {
		t.Errorf("path = %q, want %q", got, path)
	}
	if d.Message == "" || strings.ContainsAny(d.Message, "\n\r") {
		t.Errorf("message %q is not one line", d.Message)
	}
}

// TestCheckFailureMode is 02 req 37 and test 16 (RZ-CFG-029): open on
// auth.*, auth.upstream-*, authz.* or a plugin of class auth or authz is
// rejected; ratelimit open and plugin custom open are accepted.
func TestCheckFailureMode(t *testing.T) {
	reg := New()
	tests := []struct {
		typ   v1alpha1.PolicyType
		class *v1alpha1.FilterClass
		mode  *v1alpha1.FailureMode
		bad   bool
	}{
		{v1alpha1.PolicyTypeAuthJWT, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthAPIKey, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthBasic, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthMTLS, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthzCEL, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthzIP, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthzOPA, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthUpstreamOAuth2, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypeAuthUpstreamSigV4, nil, ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClassAuthz), ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClassAuth), ptr(v1alpha1.FailureModeOpen), true},
		{v1alpha1.PolicyTypePlugin, ptr(v1alpha1.FilterClassCustom), ptr(v1alpha1.FailureModeOpen), false},
		{v1alpha1.PolicyTypePlugin, nil, ptr(v1alpha1.FailureModeOpen), false},
		{v1alpha1.PolicyTypeRateLimit, nil, ptr(v1alpha1.FailureModeOpen), false},
		{v1alpha1.PolicyTypeAITokenBudget, nil, ptr(v1alpha1.FailureModeOpen), false},
		{v1alpha1.PolicyTypeCORS, nil, ptr(v1alpha1.FailureModeOpen), false},
		{v1alpha1.PolicyTypeAuthJWT, nil, ptr(v1alpha1.FailureModeClosed), false},
		{v1alpha1.PolicyTypeAuthJWT, nil, nil, false},
		{"rateLimit", nil, ptr(v1alpha1.FailureModeOpen), false},
	}
	for _, tt := range tests {
		p := policy("pol", tt.typ)
		p.Spec.FilterClass = tt.class
		p.Spec.FailureMode = tt.mode
		d, bad := reg.CheckFailureMode(p)
		if bad != tt.bad {
			t.Errorf("%s class %v mode %v: CheckFailureMode = %v, want %v", tt.typ, tt.class, tt.mode, bad, tt.bad)
			continue
		}
		if bad {
			wantDiag(t, d, CodeFailureMode, "pol", "spec.failureMode")
			if w := `failureMode open is not allowed for Policy type "` + string(tt.typ) + `" (closed only)`; d.Message != w {
				t.Errorf("message = %q, want %q", d.Message, w)
			}
		}
		if e, ok := reg.Lookup(tt.typ); ok {
			if tt.mode != nil && e.AllowsFailureMode(p, *tt.mode) == tt.bad {
				t.Errorf("%s: AllowsFailureMode(%s) disagrees with CheckFailureMode", tt.typ, *tt.mode)
			}
			if !e.AllowsFailureMode(p, v1alpha1.FailureModeClosed) {
				t.Errorf("%s: closed not allowed", tt.typ)
			}
			if e.AllowsFailureMode(p, "sometimes") {
				t.Errorf("%s: an unknown failureMode allowed", tt.typ)
			}
		}
	}
}

// TestEffectiveFailureMode: the registry default applies unless the Policy
// sets one (foundation pack section 8.10), including OQ-configuration-
// model-8 (a) quota open and ai.token-budget closed.
func TestEffectiveFailureMode(t *testing.T) {
	reg := New()
	tests := []struct {
		typ  v1alpha1.PolicyType
		mode *v1alpha1.FailureMode
		want v1alpha1.FailureMode
	}{
		{v1alpha1.PolicyTypeQuota, nil, v1alpha1.FailureModeOpen},
		{v1alpha1.PolicyTypeAITokenBudget, nil, v1alpha1.FailureModeClosed},
		{v1alpha1.PolicyTypeQuota, ptr(v1alpha1.FailureModeClosed), v1alpha1.FailureModeClosed},
		{v1alpha1.PolicyTypeCache, nil, v1alpha1.FailureModeOpen},
		{v1alpha1.PolicyTypeHeaders, nil, v1alpha1.FailureModeClosed},
		{v1alpha1.PolicyTypeHeaders, ptr(v1alpha1.FailureModeOpen), v1alpha1.FailureModeOpen},
	}
	for _, tt := range tests {
		e, _ := reg.Lookup(tt.typ)
		p := policy("pol", tt.typ)
		p.Spec.FailureMode = tt.mode
		if got := e.EffectiveFailureMode(p); got != tt.want {
			t.Errorf("%s: EffectiveFailureMode = %q, want %q", tt.typ, got, tt.want)
		}
	}
}

// TestCheckServed: a Policy of an unserved type is RZ-CFG-040 at spec.type
// naming its milestone; served and unknown types yield nothing.
func TestCheckServed(t *testing.T) {
	reg := New()
	for _, e := range reg.Entries() {
		p := policy("pol", e.Type)
		d, bad := reg.CheckServed(p)
		if bad == e.Served {
			t.Errorf("%s: CheckServed = %v, served %v", e.Type, bad, e.Served)
			continue
		}
		if bad {
			wantDiag(t, d, CodeUnserved, "pol", "spec.type")
			if w := `Policy type "` + string(e.Type) + `" is not served by this release (Planned (` + e.Planned + `))`; d.Message != w {
				t.Errorf("message = %q, want %q", d.Message, w)
			}
		}
	}
	if _, bad := reg.CheckServed(policy("pol", "rateLimit")); bad {
		t.Error("CheckServed reported an unknown type; the schema owns it")
	}
	d, _ := reg.CheckServed(policy("geo", v1alpha1.PolicyTypeAuthzGeoIP))
	d.Location = diag.Location{File: "policies/geo.yaml", Line: 4, Column: 9}
	got := string(d.AppendText(nil))
	w := `policies/geo.yaml:4:9 error RZ-CFG-040 Policy/geo spec.type: Policy type "authz.geoip" is not served by this release (Planned (M2))`
	if got != w {
		t.Errorf("text form = %q\nwant        %q", got, w)
	}
}
