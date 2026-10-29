// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package ip

import (
	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

var _ hub.PolicyCheck = Check

// Config members holding the lists.
const (
	fieldAllow = "allow"
	fieldDeny  = "deny"
)

// Check is the offline check of an authz.ip Policy (a hub.PolicyCheck run
// at stage H by every binary): each entry of config.allow or config.deny
// that is neither a CIDR nor an IP address is RZ-CFG-005 at its set item,
// spec.config.allow[item=<entry>], so validation refuses what Build
// refuses.
// The schema reports two empty lists. A resource whose Config is not a
// decoded AuthzIPConfig yields nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthzIPConfig)
	if !ok || cfg == nil {
		return
	}
	lists := []struct {
		field   string
		entries []string
	}{{fieldAllow, cfg.Allow}, {fieldDeny, cfg.Deny}}
	for _, list := range lists {
		for _, s := range list.entries {
			if _, err := clientaddr.ParsePrefix(s); err == nil {
				continue
			}
			p := diag.Path{diag.Field("spec"), diag.Field("config"), diag.Field(list.field), diag.Item(s)}
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
				Message:  "not a CIDR or an IP address",
			})
		}
	}
}
