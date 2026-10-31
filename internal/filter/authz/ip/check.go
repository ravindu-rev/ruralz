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
// at stage H by every binary), so validation refuses what Build refuses:
// config.allow and config.deny both empty is RZ-CFG-005 at spec.config,
// and each entry of either list that is neither a CIDR nor an IP address
// is RZ-CFG-005 at its set item, spec.config.allow[item=<entry>]. The
// schema requires one of the two members but admits both written as empty
// lists. A resource whose Config is not a decoded AuthzIPConfig yields
// nothing.
func Check(r *hub.Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic)) {
	if r == nil || add == nil {
		return
	}
	cfg, ok := r.Config.(*v1alpha1.AuthzIPConfig)
	if !ok || cfg == nil {
		return
	}
	locate := func(p diag.Path) diag.Location {
		if loc == nil {
			return diag.Location{}
		}
		return loc(p)
	}
	if len(cfg.Allow) == 0 && len(cfg.Deny) == 0 {
		p := diag.Path{diag.Field("spec"), diag.Field("config")}
		add(diag.Diagnostic{
			Code:     CodeConfig,
			Severity: diag.SeverityError,
			Location: locate(p),
			Resource: r.ResourceID(),
			Path:     p,
			Message:  "config.allow and config.deny are both empty",
			Hint:     "list the CIDRs or addresses to allow or to deny",
		})
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
			add(diag.Diagnostic{
				Code:     CodeConfig,
				Severity: diag.SeverityError,
				Location: locate(p),
				Resource: r.ResourceID(),
				Path:     p,
				Message:  "not a CIDR or an IP address",
			})
		}
	}
}
