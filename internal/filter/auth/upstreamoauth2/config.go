// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstreamoauth2

import (
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/egress"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeAuthUpstreamOAuth2

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Factory cannot use: the wrong config type,
	// or a config.timeout that is not positive.
	CodeConfig = "RZ-CFG-005"
	// CodeUnresolved is a config.clientSecret missing from the Revision's
	// resolved secret table.
	CodeUnresolved = "RZ-CFG-026"
	// CodeNotHTTPS is a config.tokenUrl that is not an https URL.
	CodeNotHTTPS = "RZ-CFG-037"
	// CodeUpstreamAuth is an upstream credential that was not obtained: the
	// code of every CannotDecide the Filter returns (401).
	CodeUpstreamAuth = "RZ-AUTH-020"
)

// DefaultTimeout is the default config.timeout: one token fetch is bounded
// by 2 s (target).
const DefaultTimeout = 2 * time.Second

// Configuration errors; their messages carry no secret.
var (
	errTokenURL = errors.New("config.tokenUrl is not an absolute https URL")
	// errTimeout: a zero config.timeout would end every fetch before it
	// starts, so a non-positive timeout is RZ-CFG-005.
	errTimeout = errors.New("config.timeout must be positive")
)

// settings are the fetch parameters of one Policy config. They are
// comparable: with the Policy name they key a Registry entry, so a config
// change gets a new token.
type settings struct {
	// endpoint is config.tokenUrl as written; origin its
	// "https://host:port", the clientSecret's only destination.
	endpoint, origin string
	// clientID is config.clientId.
	clientID string
	// ref is the config.clientSecret reference.
	ref secret.Ref
	// scope is the scope parameter: config.scopes in order, without
	// duplicates or empty ones, joined by spaces; "" sends none.
	scope string
	// timeout bounds one fetch.
	timeout time.Duration
}

// settingsOf validates cfg and derives its settings: a tokenUrl that is
// not an https URL is RZ-CFG-037 and a timeout that is not positive
// RZ-CFG-005.
func settingsOf(cfg *v1alpha1.AuthUpstreamOAuth2Config) (settings, error) {
	u, err := parseTokenURL(cfg.TokenURL)
	if err != nil {
		return settings{}, errcode.Wrap(CodeNotHTTPS, err)
	}
	timeout := DefaultTimeout
	if cfg.Timeout != nil {
		timeout = time.Duration(*cfg.Timeout)
	}
	if timeout <= 0 {
		return settings{}, errcode.Wrap(CodeConfig, errTimeout)
	}
	return settings{
		endpoint: cfg.TokenURL,
		origin:   egress.Origin(u),
		clientID: cfg.ClientID,
		ref:      secret.RefOf(cfg.ClientSecret.SecretRef),
		scope:    scopeOf(cfg.Scopes),
		timeout:  timeout,
	}, nil
}

// parseTokenURL parses a tokenUrl and accepts only an absolute https URL
// with a host (Security and identity, "Transport security": tokenUrl MUST
// be https).
func parseTokenURL(s string) (*url.URL, error) {
	u, err := url.Parse(s)
	if err != nil || !strings.EqualFold(u.Scheme, "https") || u.Hostname() == "" {
		return nil, errTokenURL
	}
	return u, nil
}

// scopeOf joins scopes into the scope parameter (RFC 6749 section 3.3):
// in order, without duplicates or empty strings, space-separated.
func scopeOf(scopes []string) string {
	kept := make([]string, 0, len(scopes))
	for _, s := range scopes {
		if s != "" && !slices.Contains(kept, s) {
			kept = append(kept, s)
		}
	}
	return strings.Join(kept, " ")
}

var _ hub.PolicyCheck = Check

// Check is the offline check of an auth.upstream-oauth2 Policy (a
// hub.PolicyCheck run at stage H by every binary): a config.tokenUrl that
// is not an absolute https URL is RZ-CFG-037 at spec.config.tokenUrl, and
// a config.timeout that is not positive RZ-CFG-005 at spec.config.timeout,
// exactly as the Factory refuses them on a Node. It reads no file or
// network. A resource whose Config is not a decoded
// AuthUpstreamOAuth2Config yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthUpstreamOAuth2Config)
	if !ok || cfg == nil {
		return
	}
	report := func(code, field string, err error) {
		p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field(field)}
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     code,
			Severity: diag.SeverityError,
			Location: l,
			Resource: &diag.ResourceID{Kind: string(r.Kind), Name: r.Name},
			Path:     p,
			Message:  err.Error(),
		})
	}
	if _, err := parseTokenURL(cfg.TokenURL); err != nil {
		report(CodeNotHTTPS, "tokenUrl", err)
	}
	if cfg.Timeout != nil && *cfg.Timeout <= 0 {
		report(CodeConfig, "timeout", errTimeout)
	}
}
