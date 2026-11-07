// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// checkCase is one config of the configuration tests.
type checkCase struct {
	name string
	cfg  *v1alpha1.AuthJWTSignConfig
	// paths are the spec.config paths Check reports, nil for none; schema
	// marks a finding only Build reports, the schema stating it.
	paths  []string
	schema bool
}

// checkCases are the configs the offline check and Build judge alike.
func checkCases() []checkCase {
	rs := v1alpha1.JWTSignAlgorithmRS256
	return []checkCase{
		{name: "one path", cfg: pathsConfig(rs, "", "token")},
		{name: "paths with stars and escapes", cfg: pathsConfig(rs, "k", "items.*.claims", `a\.b.c`, "l.0")},
		{name: "claims at a nested target", cfg: claimsConfig(rs, `{"exp": 1}`, "session.token")},
		{name: "a path with an empty segment", cfg: pathsConfig(rs, "", "token", "a..b"), paths: []string{"spec.config.paths[1]"}},
		{name: "a path with a bare star", cfg: pathsConfig(rs, "", "a*"), paths: []string{"spec.config.paths[0]"}},
		{name: "a path with a bad escape", cfg: pathsConfig(rs, "", `a\b`), paths: []string{"spec.config.paths[0]"}},
		{name: "target beside paths", cfg: func() *v1alpha1.AuthJWTSignConfig {
			c := pathsConfig(rs, "", "token")
			c.Target = "out"
			return c
		}(), paths: []string{"spec.config.target"}},
		{name: "claims without target", cfg: claimsConfig(rs, `{"exp": 1}`, ""), paths: []string{"spec.config.claims"}},
		{name: "a target that is not a dot path", cfg: claimsConfig(rs, `{"exp": 1}`, "a."), paths: []string{"spec.config.target"}},
		{name: "neither mode", cfg: &v1alpha1.AuthJWTSignConfig{Algorithm: rs, SigningKey: v1alpha1.SecretValue{SecretRef: signingKeyRef()}}, schema: true},
		{name: "both modes", cfg: func() *v1alpha1.AuthJWTSignConfig {
			c := claimsConfig(rs, `{"exp": 1}`, "token")
			c.Paths = []string{"x"}
			return c
		}(), paths: []string{"spec.config.target"}, schema: true},
		{name: "an unknown algorithm", cfg: pathsConfig("HS256", "", "token"), schema: true},
	}
}

// TestCheck: Check reports every finding the schema does not state, at
// its field, with RZ-CFG-005 and the resource; a schema finding it leaves
// to stage F.
func TestCheck(t *testing.T) {
	for _, c := range checkCases() {
		t.Run(c.name, func(t *testing.T) {
			r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindPolicy, Name: "signer"}, Config: c.cfg}
			var got []diag.Diagnostic
			Check(r, func(diag.Path) diag.Location { return diag.Location{File: "policies.yaml", Line: 3} }, func(d diag.Diagnostic) { got = append(got, d) })
			var paths []string
			for _, d := range got {
				if d.Code != CodeConfig || d.Severity != diag.SeverityError || d.Resource == nil || d.Resource.Name != "signer" || d.File != "policies.yaml" {
					t.Errorf("finding %+v", d)
				}
				paths = append(paths, d.Path.String())
			}
			if !slices.Equal(paths, c.paths) {
				t.Errorf("Check paths = %v, want %v", paths, c.paths)
			}
		})
	}
	// Another config type and nil arguments yield nothing.
	Check(&hub.Resource{Config: &v1alpha1.AuthMTLSConfig{}}, nil, func(diag.Diagnostic) { t.Error("finding on another type") })
	Check(nil, nil, func(diag.Diagnostic) { t.Error("finding on a nil resource") })
	Check(&hub.Resource{Config: pathsConfig("RS256", "", "a..b")}, nil, nil)
}

// TestBuildRefusesWhatCheckReports: Build refuses each config with a
// finding, Check's and the schema's alike, with RZ-CFG-005 naming the
// first one's field, before it reads the signing key.
func TestBuildRefusesWhatCheckReports(t *testing.T) {
	for _, c := range checkCases() {
		t.Run(c.name, func(t *testing.T) {
			st := newStore(pkcs8(t, keys(t).rsa))
			_, err := Factory{}.Build(t.Context(), buildEnv(t, c.cfg, st))
			refused := len(c.paths) > 0 || c.schema
			if !refused {
				if err != nil {
					t.Fatalf("Build = %v, want a Filter", err)
				}
				return
			}
			if code, _ := errcode.CodeOf(err); code != CodeConfig {
				t.Fatalf("Build = %v, want %s", err, CodeConfig)
			}
			if len(c.paths) > 0 && !c.schema && !strings.Contains(err.Error(), strings.TrimPrefix(c.paths[0], "spec.")) {
				t.Errorf("Build = %v, want the field %s", err, c.paths[0])
			}
			if st.live() != 0 {
				t.Error("a refused config left a watch running")
			}
		})
	}
}
