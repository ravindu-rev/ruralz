// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package geoip holds the offline check of the authz.geoip Policy type
// (docs/architecture/08-security-and-identity.md "IP filtering and
// GeoIP"). The schema admits the type, so its config rule applies in every
// binary from M1 (docs/architecture/02-configuration-model.md "Semantic
// validation"), while its Filter, which allows or denies by country from a
// local MaxMind-format database, is Planned (M2) and a Node refuses the
// type as unserved (RZ-CFG-040). Until the Filter ships here, the package
// holds only Check (docs/engineering/02-repository-layout-and-conventions.md
// "Package layers"). It reads no file and opens no connection.
package geoip

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package checks.
const Type = v1alpha1.PolicyTypeAuthzGeoIP

// CodeConfig is RZ-CFG-005: config.allow and config.deny both empty.
const CodeConfig = "RZ-CFG-005"

var _ hub.PolicyCheck = Check

// Check is the offline check of an authz.geoip Policy (a hub.PolicyCheck
// run at stage H by every binary): config.allow and config.deny both
// empty is RZ-CFG-005 at spec.config, since a Policy that lists no
// country would admit every request. The schema requires one of the two
// members and refuses a code that is not two upper-case letters, but
// admits both written as empty lists, which this check reports. A
// resource whose Config is not a decoded AuthzGeoIPConfig yields nothing.
// A nil loc places the finding at no location.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthzGeoIPConfig)
	if !ok || cfg == nil || len(cfg.Allow) > 0 || len(cfg.Deny) > 0 {
		return
	}
	p := diag.Path{diag.Field("spec"), diag.Field("config")}
	var l diag.Location
	if loc != nil {
		l = loc(p)
	}
	add(diag.Diagnostic{
		Code:     CodeConfig,
		Severity: diag.SeverityError,
		Location: l,
		Resource: r.ResourceID(),
		Path:     p,
		Message:  "config.allow and config.deny are both empty",
		Hint:     "list the ISO 3166-1 alpha-2 country codes to allow or to deny",
	})
}
