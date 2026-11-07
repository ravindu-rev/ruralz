// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package check is the offline check of the authz.geoip Policy type
// (docs/architecture/08-security-and-identity.md "IP filtering and
// GeoIP"; docs/architecture/02-configuration-model.md "Semantic
// validation"): the config rule the schema cannot state, which stage H
// runs in every binary through internal/filter/builtin/checks.Table(), and
// which the authz.geoip Filter's Build refuses from the same rule. It lives
// beside the Filter rather than in it because the Filter links
// github.com/oschwald/maxminddb-golang, which only ruralzd links, while
// checks.Table() links into all three binaries
// (docs/engineering/02-repository-layout-and-conventions.md "Package
// layers"). It imports the standard library and configuration contracts
// only, reads no file and opens no connection.
package check

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package checks.
const Type = v1alpha1.PolicyTypeAuthzGeoIP

// CodeConfig is RZ-CFG-005: config.allow and config.deny both empty.
const CodeConfig = "RZ-CFG-005"

// MessageEmpty is the message of the finding, which the Factory's
// refusal repeats.
const MessageEmpty = "config.allow and config.deny are both empty"

var _ hub.PolicyCheck = Check

// Empty reports whether cfg lists no country in either list, absent or
// written as an empty list: the rule Check reports and the authz.geoip
// Factory refuses, since a Policy that lists no country would admit every
// request. A nil cfg lists none.
func Empty(cfg *v1alpha1.AuthzGeoIPConfig) bool {
	return cfg == nil || len(cfg.Allow) == 0 && len(cfg.Deny) == 0
}

// Check is the offline check of an authz.geoip Policy (a hub.PolicyCheck
// run at stage H by every binary): config.allow and config.deny both
// empty (Empty) is RZ-CFG-005 at spec.config. The schema requires one of
// the two members and refuses a code that is not two upper-case letters,
// but admits both written as empty lists, which this check reports. A
// resource whose Config is not a decoded AuthzGeoIPConfig yields nothing.
// A nil loc places the finding at no location.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthzGeoIPConfig)
	if !ok || cfg == nil || !Empty(cfg) {
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
		Message:  MessageEmpty,
		Hint:     "list the ISO 3166-1 alpha-2 country codes to allow or to deny",
	})
}
