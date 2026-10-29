// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jwt

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Check is the offline check of an auth.jwt Policy (a hub.PolicyCheck run
// at stage H by every binary): each config.issuers[].jwksUrl that is not
// an absolute https URL with a host is RZ-CFG-037 at
// spec.config.issuers[issuer=<issuer>].jwksUrl. It reads no file or
// network. A resource whose Config is not a decoded AuthJWTConfig yields
// nothing; the schema reports missing and malformed members.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthJWTConfig)
	if !ok || cfg == nil {
		return
	}
	for _, is := range cfg.Issuers {
		err := checkJWKSURL(is.JWKSURL)
		if err == nil {
			continue
		}
		p := diag.Path{
			diag.Field("spec"), diag.Field("config"), diag.Field("issuers"),
			diag.Keyed("issuer", is.Issuer), diag.Field("jwksUrl"),
		}
		var l diag.Location
		if loc != nil {
			l = loc(p)
		}
		add(diag.Diagnostic{
			Code:     CodeHTTPS,
			Severity: diag.SeverityError,
			Location: l,
			Resource: r.ResourceID(),
			Path:     p,
			Message:  err.Error(),
		})
	}
}
