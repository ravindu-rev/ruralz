// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package apikey

import (
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// headerCases are config.header values with whether they can carry keys:
// an RFC 9110 token of at most 256 bytes that the Node does not manage,
// or absent or empty for the default x-api-key.
func headerCases() []struct {
	header *string
	ok     bool
} {
	return []struct {
		header *string
		ok     bool
	}{
		{nil, true},
		{ptr(""), true},
		{ptr("x-api-key"), true},
		{ptr("X-Partner-Key"), true},
		{ptr("authorization"), true},
		{ptr("x-forwarded-for"), true},
		{ptr(strings.Repeat("k", 256)), true},
		{ptr(strings.Repeat("k", 257)), false},
		{ptr("x api key"), false},
		{ptr("x-api-key:"), false},
		{ptr("x-äpi-key"), false},
		{ptr(":authority"), false},
		{ptr("Host"), false},
		{ptr("content-length"), false},
		{ptr("Expect"), false},
		{ptr("traceparent"), false},
		{ptr("tracestate"), false},
		{ptr("Connection"), false},
		{ptr("TE"), false},
		{ptr("transfer-encoding"), false},
		{ptr("Proxy-Authorization"), false},
	}
}

// TestCheckHeader checks the offline rule every binary runs: a
// config.header that is not an RFC 9110 field name, or that names a field
// the Node manages (the rule of the headers Policy), is RZ-CFG-005 at
// spec.config.header, located through loc; an absent or empty header is
// the default x-api-key and passes.
func TestCheckHeader(t *testing.T) {
	for _, tt := range headerCases() {
		r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: &v1alpha1.AuthAPIKeyConfig{Header: tt.header}}
		var got diag.List
		var asked []string
		loc := func(p diag.Path) diag.Location {
			asked = append(asked, p.String())
			return diag.Location{File: "policies/keys.yaml", Line: 5, Column: 11}
		}
		Check(r, loc, func(d diag.Diagnostic) { got = append(got, d) })
		name := "<absent>"
		if tt.header != nil {
			name = *tt.header
		}
		switch {
		case tt.ok && len(got) != 0:
			t.Errorf("header %q: unexpected %s", name, got[0].AppendText(nil))
		case !tt.ok && (len(got) != 1 || got[0].Code != "RZ-CFG-005" || got[0].Path.String() != "spec.config.header" ||
			len(asked) != 1 || asked[0] != "spec.config.header"):
			t.Errorf("header %q: diagnostics %v, locations %v; want one RZ-CFG-005 at spec.config.header", name, got, asked)
		}
	}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: &v1alpha1.AuthAPIKeyConfig{Header: ptr("Host")}}
	var got diag.List
	Check(r, func(diag.Path) diag.Location { return diag.Location{File: "policies/keys.yaml", Line: 5, Column: 11} },
		func(d diag.Diagnostic) { got = append(got, d) })
	want := "policies/keys.yaml:5:11 error RZ-CFG-005 Policy/partner-keys spec.config.header: header host is managed by the Node (host)"
	if len(got) != 1 || string(got[0].AppendText(nil)) != want {
		t.Fatalf("Check = %v, want %s", got, want)
	}
}

// TestCheckIgnoresOtherResources checks that Check yields nothing for a
// resource without a decoded auth.api-key config, and accepts a nil
// location mapper.
func TestCheckIgnoresOtherResources(t *testing.T) {
	add := func(d diag.Diagnostic) { t.Errorf("unexpected diagnostic %v", d) }
	Check(nil, nil, add)
	Check(&hub.Resource{Config: &v1alpha1.AuthJWTConfig{}}, nil, add)
	Check(&hub.Resource{Config: (*v1alpha1.AuthAPIKeyConfig)(nil)}, nil, add)
	Check(&hub.Resource{}, nil, nil)
	n := 0
	bad := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: policy}, Config: &v1alpha1.AuthAPIKeyConfig{Header: ptr("x api key")}}
	Check(bad, nil, func(d diag.Diagnostic) {
		n++
		if d.Code != "RZ-CFG-005" || d.Location != (diag.Location{}) {
			t.Errorf("diagnostic %+v", d)
		}
	})
	if n != 1 {
		t.Fatalf("diagnostics = %d, want 1", n)
	}
}

// TestBuildRejectsHeader checks that Build applies Check's rule: a
// config.header that cannot carry keys is RZ-CFG-005, rejected before the
// credential index is acquired, so no secretRef watch is left behind.
func TestBuildRejectsHeader(t *testing.T) {
	for _, tt := range headerCases() {
		st := newStore(map[secret.Ref]string{refPrevious: keyOld})
		flt, err := NewFactory(nil).Build(t.Context(), buildEnv(&v1alpha1.AuthAPIKeyConfig{Header: tt.header}, st, consumer("acme", "", held(refPrevious))))
		code, _ := errcode.CodeOf(err)
		switch {
		case tt.ok && err != nil:
			t.Errorf("header %v: Build = %v", tt.header, err)
		case !tt.ok && (code != "RZ-CFG-005" || st.live() != 0):
			t.Errorf("header %v: Build = %v, %d watches; want RZ-CFG-005 and none", tt.header, err, st.live())
		}
		if f, ok := flt.(*Filter); ok {
			_ = f.Close()
		}
	}
}

// ptr returns a pointer to s.
func ptr(s string) *string { return &s }
