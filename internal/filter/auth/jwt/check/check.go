// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package check is the offline check of the auth.jwt Policy type
// (docs/architecture/02-configuration-model.md "Semantic validation"):
// the config rules the schema cannot state, which stage H runs in every
// binary through internal/filter/builtin/checks.Table(), and which the
// auth.jwt Filter's Build refuses from the same rule set. It lives beside
// the Filter rather than in it because the Filter links jwx, a module
// cmd/ruralz must not link, while checks.Table() links into all three
// binaries (docs/engineering/02-repository-layout-and-conventions.md
// "Package layers"). It imports the standard library and configuration
// contracts only, reads no file and opens no connection.
package check

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Codes of the findings, each registered in internal/errcode.
const (
	// CodeHTTPS is an issuers[].jwksUrl that is not an absolute https URL
	// with a host.
	CodeHTTPS = "RZ-CFG-037"
	// CodeSchema is any other config rule: no issuers, or an issuer that is
	// empty, named twice or without audiences.
	CodeSchema = "RZ-CFG-005"
)

var _ hub.PolicyCheck = Check

// Finding is one rule an auth.jwt config breaks.
type Finding struct {
	// Code is CodeSchema, or CodeHTTPS for a jwksUrl.
	Code string
	// Path is the key-aware path under spec.config.
	Path diag.Path
	// Message is the message Check reports.
	Message string
	// Issuer and Cause are the issuer and the error of a jwksUrl finding,
	// which Err wraps.
	Issuer string
	Cause  error
}

// Err returns the error the auth.jwt Factory refuses the config with: the
// code and "auth.jwt: <message>", or for a jwksUrl "auth.jwt: issuer
// <issuer>: <cause>".
func (f Finding) Err() error {
	if f.Code == CodeHTTPS && f.Cause != nil {
		return errcode.Wrap(CodeHTTPS, fmt.Errorf("auth.jwt: issuer %q: %w", f.Issuer, f.Cause))
	}
	return errcode.Errorf(f.Code, "auth.jwt: %s", f.Message)
}

// Findings returns every rule cfg breaks beyond the schema, in issuer
// order and, within an issuer, in rule order: at least one issuer; a
// non-empty issuer, named once, with at least one audience (CodeSchema);
// a jwksUrl that is an absolute https URL with a host (CodeHTTPS, TB-10).
// The Factory refuses the first and Check reports them all, so an offline
// run reports what activation refuses. A nil cfg has no findings.
func Findings(cfg *v1alpha1.AuthJWTConfig) []Finding {
	if cfg == nil {
		return nil
	}
	issuers := diag.Path{diag.Field("issuers")}
	if len(cfg.Issuers) == 0 {
		return []Finding{{Code: CodeSchema, Path: issuers, Message: "config.issuers is empty"}}
	}
	var out []Finding
	seen := make(map[string]bool, len(cfg.Issuers))
	for _, is := range cfg.Issuers {
		entry := issuers.Append(diag.Keyed("issuer", is.Issuer))
		if is.Issuer == "" {
			out = append(out, Finding{Code: CodeSchema, Path: entry.Append(diag.Field("issuer")), Message: "config.issuers has an empty issuer"})
		} else if seen[is.Issuer] {
			out = append(out, Finding{Code: CodeSchema, Path: entry, Message: "issuer " + strconv.Quote(is.Issuer) + " is listed twice"})
		}
		seen[is.Issuer] = true
		if len(is.Audiences) == 0 {
			out = append(out, Finding{Code: CodeSchema, Path: entry.Append(diag.Field("audiences")), Message: "issuer " + strconv.Quote(is.Issuer) + " has no audiences"})
		}
		if err := checkJWKSURL(is.JWKSURL); err != nil {
			out = append(out, Finding{Code: CodeHTTPS, Path: entry.Append(diag.Field("jwksUrl")), Message: err.Error(), Issuer: is.Issuer, Cause: err})
		}
	}
	return out
}

// Check is the offline check of an auth.jwt Policy (a hub.PolicyCheck run
// at stage H by every binary): every finding the Factory refuses, at its
// path under spec.config. An issuer that is empty, named twice or without
// audiences is RZ-CFG-005; a jwksUrl that is not an absolute https URL
// with a host is RZ-CFG-037 at spec.config.issuers[issuer=<issuer>].jwksUrl.
// A resource whose Config is not a decoded AuthJWTConfig yields nothing;
// the schema reports missing and malformed members. A nil loc places
// findings at no location.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthJWTConfig)
	if !ok || cfg == nil {
		return
	}
	for _, f := range Findings(cfg) {
		p := diag.Path{diag.Field("spec"), diag.Field("config")}.Append(f.Path...)
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     f.Code,
			Severity: diag.SeverityError,
			Location: l,
			Resource: r.ResourceID(),
			Path:     p,
			Message:  f.Message,
		})
	}
}

// checkJWKSURL returns an error unless s is an absolute https URL with a
// host (TB-10); the scheme compares ignoring case.
func checkJWKSURL(s string) error {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return errors.New("jwksUrl is not a URL")
	case u.Scheme != "https" || u.Opaque != "":
		return errors.New("jwksUrl is not an https URL")
	case u.Hostname() == "":
		return errors.New("jwksUrl has no host")
	}
	return nil
}
