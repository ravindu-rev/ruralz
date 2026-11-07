// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwtsign

import (
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthJWTSign

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Policy refuses (package documentation,
	// "Configuration").
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is a config.claims with a syntax, type or unavailable-variable
	// error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is a config.claims whose static cost estimate exceeds the
	// ceiling.
	CodeCost = "RZ-CFG-015"
	// CodeKey is a config.signingKey that is not resolved, or whose value
	// is not a private key fitting config.algorithm.
	CodeKey = "RZ-CFG-026"
)

// Config members, as findings name them.
const (
	fieldPaths     = "paths"
	fieldClaims    = "claims"
	fieldTarget    = "target"
	fieldAlgorithm = "algorithm"
)

// finding is one RZ-CFG-005 finding at a path under spec.config.
type finding struct {
	path    diag.Path
	message string
	// schema marks a finding the JSON Schema reports itself: Build refuses
	// it and Check leaves it out.
	schema bool
}

// findings returns what Build refuses in cfg, in field order: the mode
// (exactly one of paths and claims, a schema rule), each path, then
// target, which claims requires and paths does not take, and the
// algorithm (a schema enum).
func findings(cfg *v1alpha1.AuthJWTSignConfig) []finding {
	var out []finding
	paths, claims := len(cfg.Paths) > 0, cfg.Claims != ""
	if paths == claims {
		out = append(out, finding{message: "set exactly one of config.paths and config.claims", schema: true})
	}
	for i, p := range cfg.Paths {
		if _, err := dotpath.Parse(p); err != nil {
			out = append(out, finding{path: diag.Path{diag.Field(fieldPaths), diag.Index(i)}, message: "not a dot path: " + err.Error()})
		}
	}
	switch {
	case cfg.Target != "" && paths:
		out = append(out, finding{path: diag.Path{diag.Field(fieldTarget)}, message: "config.target is written only with config.claims; config.paths signs the objects in place"})
	case cfg.Target == "" && claims:
		out = append(out, finding{path: diag.Path{diag.Field(fieldClaims)}, message: "config.claims needs config.target, the body path the token is written at"})
	case cfg.Target != "":
		if _, err := dotpath.Parse(cfg.Target); err != nil {
			out = append(out, finding{path: diag.Path{diag.Field(fieldTarget)}, message: "not a dot path: " + err.Error()})
		}
	}
	if familyOf(cfg.Algorithm) == 0 {
		out = append(out, finding{path: diag.Path{diag.Field(fieldAlgorithm)}, message: strconv.Quote(string(cfg.Algorithm)) + " is not RS256, PS256, ES256 or EdDSA", schema: true})
	}
	return out
}

var _ hub.PolicyCheck = Check

// Check is the offline check of an auth.jwt-sign Policy (a hub.PolicyCheck
// run at stage H by every binary): each config.paths entry and
// config.target that is not a dot path, config.target beside config.paths,
// and config.claims without config.target are RZ-CFG-005 at the field. The
// schema states the mode (exactly one of paths and claims), a paths list
// written empty, an empty claims and the algorithm, so Check leaves them
// out. It never resolves the signing key, which only a Node does. A
// resource whose Config is not a decoded AuthJWTSignConfig yields nothing;
// a nil loc places findings at no location.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthJWTSignConfig)
	if !ok || cfg == nil {
		return
	}
	for _, f := range findings(cfg) {
		if f.schema {
			continue
		}
		p := diag.Path{diag.Field("spec"), diag.Field("config")}
		p = append(p, f.path...)
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code: CodeConfig, Severity: diag.SeverityError, Location: l,
			Resource: r.ResourceID(), Path: p, Message: f.message,
		})
	}
}
