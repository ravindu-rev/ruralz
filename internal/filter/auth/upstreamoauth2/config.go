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
	// a config.timeout that is not positive, or a field its grantType does
	// not take or misses.
	CodeConfig = "RZ-CFG-005"
	// CodeUnresolved is a credential secret missing from the Revision's
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

// DefaultSubjectTokenType is the default config.subjectTokenType of
// token-exchange, a JWT (RFC 8693 section 3).
const DefaultSubjectTokenType = "urn:ietf:params:oauth:token-type:jwt" //nolint:gosec // G101: an RFC 8693 token type URI, not a credential

// grant is a config.grantType.
type grant = v1alpha1.OAuth2GrantType

// The grant types (pkg/config/v1alpha1).
const (
	grantClientCredentials = v1alpha1.OAuth2GrantClientCredentials
	grantJWTBearer         = v1alpha1.OAuth2GrantJWTBearer
	grantMetadataServer    = v1alpha1.OAuth2GrantMetadataServer
	grantTokenExchange     = v1alpha1.OAuth2GrantTokenExchange
)

// Configuration errors; their messages carry no secret.
var (
	errTokenURL = errors.New("config.tokenUrl is not an absolute https URL")
	// errTimeout: a zero config.timeout would end every fetch before it
	// starts, so a non-positive timeout is RZ-CFG-005.
	errTimeout = errors.New("config.timeout must be positive")
	errGrant   = errors.New("config.grantType is not a grant type this release serves")
)

// settings are the fetch parameters of one Policy config. They are
// comparable: with the Policy name they key a Registry entry, so a config
// change gets a new token.
type settings struct {
	// grant is config.grantType.
	grant grant
	// endpoint is config.tokenUrl as written and origin its
	// "https://host:port", the only destination of clientSecret and
	// subjectToken; both "" for metadata-server, which calls the metadata
	// server's fixed address.
	endpoint, origin string
	// clientID is config.clientId (client-credentials).
	clientID string
	// ref is the credential secret: config.clientSecret
	// (client-credentials), config.serviceAccountKey (jwt-bearer) or
	// config.subjectToken (token-exchange); zero for metadata-server.
	ref secret.Ref
	// scope is the scope parameter: config.scopes in order, without
	// duplicates or empty ones, joined by spaces; "" sends none.
	scope string
	// audience is config.audience: an ID token for it instead of an
	// access token.
	audience string
	// subjectTokenType, provider and serviceAccount are
	// config.subjectTokenType, config.workloadIdentityProvider and
	// config.serviceAccount (token-exchange).
	subjectTokenType, provider, serviceAccount string
	// timeout bounds one fetch.
	timeout time.Duration
}

// grantOf returns config.grantType, client-credentials when absent.
func grantOf(cfg *v1alpha1.AuthUpstreamOAuth2Config) grant {
	if cfg.GrantType == nil {
		return grantClientCredentials
	}
	return *cfg.GrantType
}

// subjectTokenTypeOf returns config.subjectTokenType, its default when
// absent.
func subjectTokenTypeOf(cfg *v1alpha1.AuthUpstreamOAuth2Config) string {
	if cfg.SubjectTokenType == nil {
		return DefaultSubjectTokenType
	}
	return *cfg.SubjectTokenType
}

// refOf returns the reference of an optional secret field; zero for none.
func refOf(v *v1alpha1.SecretValue) secret.Ref {
	if v == nil {
		return secret.Ref{}
	}
	return secret.RefOf(v.SecretRef)
}

// settingsOf validates cfg and derives its settings: the rules of Check
// (configErrors), so the Factory refuses on a Node exactly what the
// offline check reports.
func settingsOf(cfg *v1alpha1.AuthUpstreamOAuth2Config) (settings, error) {
	if errs := configErrors(cfg); len(errs) > 0 {
		e := errs[0]
		return settings{}, errcode.Wrap(e.code, &ruleError{msg: messageOf(e), rule: e.err})
	}
	timeout := DefaultTimeout
	if cfg.Timeout != nil {
		timeout = time.Duration(*cfg.Timeout)
	}
	s := settings{
		grant:    grantOf(cfg),
		scope:    scopeOf(cfg.Scopes),
		audience: cfg.Audience,
		timeout:  timeout,
	}
	switch s.grant {
	case grantClientCredentials:
		s.clientID, s.ref = deref(cfg.ClientID), refOf(cfg.ClientSecret)
	case grantJWTBearer:
		s.ref = refOf(cfg.ServiceAccountKey)
	case grantTokenExchange:
		s.ref = refOf(cfg.SubjectToken)
		s.subjectTokenType, s.provider, s.serviceAccount = subjectTokenTypeOf(cfg), cfg.WorkloadIdentityProvider, cfg.ServiceAccount
	case grantMetadataServer:
	default:
	}
	if s.grant != grantMetadataServer {
		u, _ := parseTokenURL(deref(cfg.TokenURL))
		s.endpoint, s.origin = deref(cfg.TokenURL), egress.Origin(u)
	}
	return s, nil
}

// deref returns *p, "" for nil.
func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
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

// ruleError is the Factory's refusal of a configError: its text is the
// diagnostic message Check reports, and it unwraps to the rule.
type ruleError struct {
	msg  string
	rule error
}

func (e *ruleError) Error() string { return e.msg }
func (e *ruleError) Unwrap() error { return e.rule }

// configError is one rule a config breaks, at config.<field>, under the
// config's grant type.
type configError struct {
	code, field string
	grant       grant
	err         error
}

// The rules configErrors states; their messages name fields, never
// values.
var (
	errMissing     = errors.New("is required by grantType")
	errNotTaken    = errors.New("is not taken by grantType")
	errWithScopes  = errors.New("is not used beside config.audience, which requests an ID token")
	errIDTokenOnly = errors.New("requests an ID token, which grantType token-exchange obtains only by impersonating config.serviceAccount")
)

// grantFields returns the config fields each grant takes besides scopes
// and timeout, which every grant takes, and which of them it requires
// (docs/architecture/02-configuration-model.md "Registered from feature
// documents"): audience, an ID token, is a Google source's, which
// client-credentials has no way to request.
func grantFields(g grant) (takes, requires []string) {
	switch g {
	case grantClientCredentials:
		return []string{"tokenUrl", "clientId", "clientSecret"}, []string{"tokenUrl", "clientId", "clientSecret"}
	case grantJWTBearer:
		return []string{"tokenUrl", "serviceAccountKey", "audience"}, []string{"tokenUrl", "serviceAccountKey"}
	case grantMetadataServer:
		return []string{"audience"}, nil
	case grantTokenExchange:
		return []string{"tokenUrl", "subjectToken", "subjectTokenType", "workloadIdentityProvider", "serviceAccount", "audience"},
			[]string{"tokenUrl", "subjectToken", "workloadIdentityProvider"}
	default:
		return nil, nil
	}
}

// presentFields returns the grant-specific fields cfg holds, in field
// order: tokenUrl and clientId whenever written, an empty value included,
// as before grantType existed; every other field when not empty; and
// config.subjectTokenType only with a value other than its default, which
// stage G materializes into every config.
func presentFields(cfg *v1alpha1.AuthUpstreamOAuth2Config) []string {
	var out []string
	add := func(name string, present bool) {
		if present {
			out = append(out, name)
		}
	}
	add("tokenUrl", cfg.TokenURL != nil)
	add("clientId", cfg.ClientID != nil)
	add("clientSecret", cfg.ClientSecret != nil)
	add("serviceAccountKey", cfg.ServiceAccountKey != nil)
	add("subjectToken", cfg.SubjectToken != nil)
	add("subjectTokenType", cfg.SubjectTokenType != nil && *cfg.SubjectTokenType != DefaultSubjectTokenType)
	add("workloadIdentityProvider", cfg.WorkloadIdentityProvider != "")
	add("serviceAccount", cfg.ServiceAccount != "")
	add("audience", cfg.Audience != "")
	return out
}

// configErrors returns every rule cfg breaks: a grantType this release
// does not serve; each field the grantType does not take, then each it
// misses, in field order (RZ-CFG-005); a tokenUrl that is not an absolute
// https URL (RZ-CFG-037); audience beside scopes, or under token-exchange
// without serviceAccount (RZ-CFG-005); a timeout that is not positive
// (RZ-CFG-005).
func configErrors(cfg *v1alpha1.AuthUpstreamOAuth2Config) []configError {
	var out []configError
	g := grantOf(cfg)
	takes, requires := grantFields(g)
	switch g {
	case grantClientCredentials, grantJWTBearer, grantMetadataServer, grantTokenExchange:
	default:
		return []configError{{CodeConfig, "grantType", g, errGrant}}
	}
	present := presentFields(cfg)
	for _, f := range present {
		if !slices.Contains(takes, f) {
			out = append(out, configError{CodeConfig, f, g, errNotTaken})
		}
	}
	for _, f := range requires {
		if !slices.Contains(present, f) {
			out = append(out, configError{CodeConfig, f, g, errMissing})
		}
	}
	if cfg.TokenURL != nil && slices.Contains(takes, "tokenUrl") {
		if _, err := parseTokenURL(*cfg.TokenURL); err != nil {
			out = append(out, configError{CodeNotHTTPS, "tokenUrl", g, err})
		}
	}
	if cfg.Audience != "" && slices.Contains(takes, "audience") {
		if scopeOf(cfg.Scopes) != "" {
			out = append(out, configError{CodeConfig, "scopes", g, errWithScopes})
		}
		if g == grantTokenExchange && cfg.ServiceAccount == "" {
			out = append(out, configError{CodeConfig, "audience", g, errIDTokenOnly})
		}
	}
	if cfg.Timeout != nil && *cfg.Timeout <= 0 {
		out = append(out, configError{CodeConfig, "timeout", g, errTimeout})
	}
	return out
}

var _ hub.PolicyCheck = Check

// Check is the offline check of an auth.upstream-oauth2 Policy (a
// hub.PolicyCheck run at stage H by every binary), reporting each rule of
// configErrors at its spec.config field: RZ-CFG-037 for a tokenUrl that is
// not an absolute https URL, RZ-CFG-005 for every other rule, exactly as
// the Factory refuses them on a Node. A missing field is reported at its
// path, which loc maps to the nearest authored ancestor. It reads no file
// or network. A resource whose Config is not a decoded
// AuthUpstreamOAuth2Config yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthUpstreamOAuth2Config)
	if !ok || cfg == nil {
		return
	}
	for _, e := range configErrors(cfg) {
		p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field(e.field)}
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     e.code,
			Severity: diag.SeverityError,
			Location: l,
			Resource: &diag.ResourceID{Kind: string(r.Kind), Name: r.Name},
			Path:     p,
			Message:  messageOf(e),
		})
	}
}

// messageOf returns the diagnostic message of e: a field the grant type
// misses or does not take names both.
func messageOf(e configError) string {
	switch {
	case errors.Is(e.err, errMissing), errors.Is(e.err, errNotTaken):
		return "config." + e.field + " " + e.err.Error() + " " + string(e.grant)
	case errors.Is(e.err, errWithScopes), errors.Is(e.err, errIDTokenOnly):
		return "config." + e.field + " " + e.err.Error()
	default:
		return e.err.Error()
	}
}
