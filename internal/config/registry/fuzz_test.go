// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"errors"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// fuzzConfig builds a config of e's type whose list and body fields are
// set by the bits of flags.
func fuzzConfig(e Entry, flags uint16, rule string) any {
	one := func(bit uint) bool { return flags&(1<<bit) != 0 }
	switch e.Type {
	case v1alpha1.PolicyTypeHeaders:
		c := &v1alpha1.HeadersConfig{}
		if one(0) {
			c.Request = &v1alpha1.HeaderRequestOps{}
			if one(1) {
				c.Request.Set = []v1alpha1.HeaderRequestSet{{Name: "x"}}
			}
		}
		if one(2) {
			c.Response = &v1alpha1.HeaderResponseOps{}
			if one(3) {
				c.Response.Set = []v1alpha1.HeaderResponseSet{{Name: "y"}}
			}
		}
		return c
	case v1alpha1.PolicyTypeTransformRequest:
		c := &v1alpha1.TransformRequestConfig{}
		if one(4) {
			c.Body = rule
		}
		if one(5) {
			c.Remove = []v1alpha1.TransformRequestRemove{{Target: v1alpha1.TransformRequestTargetHeader, Name: "x"}}
		}
		return c
	case v1alpha1.PolicyTypeTransformResponse:
		c := &v1alpha1.TransformResponseConfig{}
		if one(4) {
			c.Body = rule
		}
		if one(6) {
			c.Replace = []v1alpha1.Replace{{Pattern: "a"}}
		}
		return c
	case v1alpha1.PolicyTypeAuthzCEL:
		return &v1alpha1.AuthzCELConfig{Rule: rule}
	default:
		if one(7) {
			return nil
		}
		return e.NewConfig()
	}
}

// oracleFunc adapts a function to BodyOracle.
type oracleFunc func(string) (bool, error)

func (f oracleFunc) ReferencesRequestBody(src string) (bool, error) { return f(src) }

// FuzzPhases checks Phase selection invariants over every type, scope,
// config shape and Plugin Phase list (02 req 13, 13a and 14; 07 req 2):
// no panic; a disallowed scope or Plugin Phase is exactly RZ-CFG-020;
// otherwise the result stays within the declared Phases of the scope's leg
// (the Plugin's Phases for plugin); authz.cel runs in exactly one Phase;
// selection is deterministic.
func FuzzPhases(f *testing.F) {
	f.Add(uint8(14), uint8(2), uint16(0b1011), uint16(0), "request.body")
	f.Add(uint8(4), uint8(1), uint16(0), uint16(0), "request.body.x")
	f.Add(uint8(22), uint8(3), uint16(0), uint16(1<<phase.OnResponse|1<<phase.OnUpstreamRequest), "")
	f.Add(uint8(13), uint8(1), uint16(1<<7), uint16(0), "")
	f.Add(uint8(15), uint8(3), uint16(0b110000), uint16(0), "body")
	reg := New()
	entries := reg.Entries()
	body := oracleFunc(func(src string) (bool, error) {
		if src == "error" {
			return false, errors.New("check failed")
		}
		return len(src) > 4 && src[:5] == "reque", nil
	})
	f.Fuzz(func(t *testing.T, typeIdx, scopeIdx uint8, flags, pluginBits uint16, rule string) {
		e := entries[int(typeIdx)%len(entries)]
		scope := phase.Scope(scopeIdx % 4)
		var pluginPhases []v1alpha1.Phase
		for p := phase.OnRequestHeaders; p < phase.Count; p++ {
			if pluginBits&(1<<p) != 0 {
				pluginPhases = append(pluginPhases, p.V1alpha1())
			}
		}
		if pluginBits&(1<<15) != 0 {
			pluginPhases = append(pluginPhases, "onStream")
		}
		p := policy("pol", e.Type)
		p.Spec.Plugin = "plg"
		a := Attachment{Policy: p, Config: fuzzConfig(e, flags, rule), Scope: scope, PluginPhases: pluginPhases}
		got, err := reg.Phases(a, body)
		again, err2 := reg.Phases(a, body)
		if got != again || (err == nil) != (err2 == nil) {
			t.Fatalf("not deterministic: %v %v, then %v %v", got, err, again, err2)
		}
		if scope == phase.ScopeNone {
			if err == nil {
				t.Fatal("zero scope accepted")
			}
			return
		}
		var se *ScopeError
		isScope := errors.As(err, &se)
		if isScope {
			if code, _ := errcode.CodeOf(err); code != CodeScope || se.Error() == "" {
				t.Fatalf("scope error without %s: %v", CodeScope, err)
			}
		} else if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !e.Scopes.Has(scope) {
			if !isScope {
				t.Fatalf("%s at %s: want RZ-CFG-020, got %v", e.Type, scope, err)
			}
			return
		}
		onLeg := phase.Phase.ClientLeg
		declared := e.Phases
		if scope == phase.ScopeUpstream {
			onLeg, declared = phase.Phase.UpstreamLeg, e.UpstreamPhases
		}
		if e.PhasesFromPlugin {
			wantBad := false
			var want phase.Set
			for _, v := range pluginPhases {
				pp, ok := phase.Parse(v)
				if !ok || !onLeg(pp) {
					wantBad = true
				}
				if ok {
					want = want.Add(pp)
				}
			}
			if wantBad != isScope {
				t.Fatalf("plugin %v at %s: scope error %v, want %v", pluginPhases, scope, isScope, wantBad)
			}
			if !wantBad && got != want {
				t.Fatalf("plugin Phases = %v, want %v", got.Phases(), want.Phases())
			}
			return
		}
		if isScope {
			t.Fatalf("%s at allowed scope %s: %v", e.Type, scope, err)
		}
		if got&^declared != 0 {
			t.Fatalf("%s at %s: Phases %v outside declared %v", e.Type, scope, got.Phases(), declared.Phases())
		}
		for _, pp := range got.Phases() {
			if !onLeg(pp) {
				t.Fatalf("%s at %s: Phase %s not on the leg", e.Type, scope, pp)
			}
		}
		if e.Type == v1alpha1.PolicyTypeAuthzCEL && len(got.Phases()) != 1 {
			t.Fatalf("authz.cel runs in %v, want exactly one Phase", got.Phases())
		}
	})
}

// BenchmarkPhases measures Phase selection for a headers attachment, the
// per-attachment cost stage I pays on every Hot Reload (PB-7 budget of the
// configuration pipeline).
func BenchmarkPhases(b *testing.B) {
	reg := New()
	a := Attachment{
		Policy: policy("hdr", v1alpha1.PolicyTypeHeaders),
		Config: &v1alpha1.HeadersConfig{Response: &v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{{Name: "x"}}}},
		Scope:  phase.ScopeRoute,
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := reg.Phases(a, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLookup measures one registry lookup.
func BenchmarkLookup(b *testing.B) {
	reg := New()
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := reg.Lookup(v1alpha1.PolicyTypeTransformResponse); !ok {
			b.Fatal("missing")
		}
	}
}
