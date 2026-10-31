// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cel

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Check is the offline check of an authz.cel Policy (a hub.PolicyCheck run
// at stage H by every binary): an empty config.rule is RZ-CFG-005 at
// spec.config.rule, so validation refuses what Build refuses. The schema
// refuses an empty rule first (minLength 1, stage F); Check is the
// backstop for a config that reaches stage H another way, because the CEL
// compiler treats an empty source as absent and stage J, which compiles
// the rule itself (RZ-CFG-014, RZ-CFG-015), would report nothing for it.
// A resource whose Config is not a decoded AuthzCELConfig yields nothing;
// the schema reports a missing rule.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthzCELConfig)
	if !ok || cfg == nil || cfg.Rule != "" {
		return
	}
	p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field("rule")}
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
		Message:  "rule is empty",
		Hint:     `write the CEL condition that allows a request, such as consumer != null`,
	})
}
