// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"fmt"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// finding is one rule an auth.jwt config breaks.
type finding struct {
	// code is RZ-CFG-005, or RZ-CFG-037 for a jwksUrl.
	code string
	// path is the key-aware path under spec.config.
	path diag.Path
	// msg is the message Check reports.
	msg string
	// issuer and cause are the issuer and error of a jwksUrl finding,
	// which Build wraps.
	issuer string
	cause  error
}

// err returns the error Build refuses the config with: the code and
// "auth.jwt: <msg>", or for a jwksUrl "auth.jwt: issuer <issuer>: <cause>".
func (f finding) err() error {
	if f.code == CodeHTTPS {
		return errcode.Wrap(CodeHTTPS, fmt.Errorf("auth.jwt: issuer %q: %w", f.issuer, f.cause))
	}
	return errcode.Errorf(f.code, "auth.jwt: %s", f.msg)
}

// findings returns every rule cfg breaks beyond the schema, in issuer
// order, each entry's in rule order: at least one issuer; a non-empty
// issuer, named once, with at least one audience (RZ-CFG-005); an https
// jwksUrl with a host (RZ-CFG-037). Build refuses the first and Check
// reports them all, so an offline run reports what activation refuses.
func findings(cfg *v1alpha1.AuthJWTConfig) []finding {
	issuers := diag.Path{diag.Field("issuers")}
	if len(cfg.Issuers) == 0 {
		return []finding{{code: codeSchema, path: issuers, msg: "config.issuers is empty"}}
	}
	var out []finding
	seen := make(map[string]bool, len(cfg.Issuers))
	for _, is := range cfg.Issuers {
		entry := issuers.Append(diag.Keyed("issuer", is.Issuer))
		if is.Issuer == "" {
			out = append(out, finding{code: codeSchema, path: entry.Append(diag.Field("issuer")), msg: "config.issuers has an empty issuer"})
		} else if seen[is.Issuer] {
			out = append(out, finding{code: codeSchema, path: entry, msg: "issuer " + strconv.Quote(is.Issuer) + " is listed twice"})
		}
		seen[is.Issuer] = true
		if len(is.Audiences) == 0 {
			out = append(out, finding{code: codeSchema, path: entry.Append(diag.Field("audiences")), msg: "issuer " + strconv.Quote(is.Issuer) + " has no audiences"})
		}
		if err := checkJWKSURL(is.JWKSURL); err != nil {
			out = append(out, finding{code: CodeHTTPS, path: entry.Append(diag.Field("jwksUrl")), msg: err.Error(), issuer: is.Issuer, cause: err})
		}
	}
	return out
}

// Check is the offline check of an auth.jwt Policy (a hub.PolicyCheck run
// at stage H by every binary): every finding Build refuses, at its path
// under spec.config. An issuer that is empty, named twice or without
// audiences is RZ-CFG-005; a jwksUrl that is not an absolute https URL
// with a host is RZ-CFG-037 at spec.config.issuers[issuer=<issuer>].jwksUrl.
// It reads no file or network. A resource whose Config is not a decoded
// AuthJWTConfig yields nothing; the schema reports missing and malformed
// members.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthJWTConfig)
	if !ok || cfg == nil {
		return
	}
	for _, f := range findings(cfg) {
		p := diag.Path{diag.Field("spec"), diag.Field("config")}.Append(f.path...)
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     f.code,
			Severity: diag.SeverityError,
			Location: l,
			Resource: r.ResourceID(),
			Path:     p,
			Message:  f.msg,
		})
	}
}
