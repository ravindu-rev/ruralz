// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// policy checks a Policy: an authored filterClass the registry refuses
// (RZ-CFG-005), a consumerQuota no Consumer defines with the type's unit
// (RZ-CFG-009), and the injected offline check of its type.
func (u *unit) policy() {
	p, ok := hub.Object[v1alpha1.Policy](u.r)
	if !ok || p == nil {
		return
	}
	if d, bad := u.v.reg.CheckFilterClass(p); bad {
		d.Location = u.f.path(d.Path)
		u.out.add(d)
	}
	switch cfg := u.r.Config.(type) {
	case *v1alpha1.QuotaConfig:
		if cfg != nil {
			u.consumerQuota(cfg.ConsumerQuota, v1alpha1.QuotaUnitRequests)
		}
	case *v1alpha1.AITokenBudgetConfig:
		if cfg != nil {
			u.consumerQuota(cfg.ConsumerQuota, v1alpha1.QuotaUnitTokens)
		}
	default:
	}
	if check := u.v.checks[p.Spec.Type]; check != nil && u.r.Config != nil {
		u.w.charge(1)
		check(u.r, u.f.path, func(d diag.Diagnostic) {
			if d.Resource == nil {
				d.Resource = u.r.ResourceID()
			}
			u.out.add(d)
		})
	}
}

// consumerQuota checks that some Consumer defines the quota name with
// the unit want, requests for quota and tokens for ai.token-budget: a
// missing one is RZ-CFG-009, hinted at the same name under the other
// unit, else at the nearest name of this unit.
func (u *unit) consumerQuota(name string, want v1alpha1.QuotaUnit) {
	if _, ok := u.ix.quotas[name][want]; ok {
		return
	}
	other := v1alpha1.QuotaUnitTokens
	if want == v1alpha1.QuotaUnitTokens {
		other = v1alpha1.QuotaUnitRequests
	}
	hint := u.ix.quotaNames[want].hint(name)
	var related []diag.Related
	if d, ok := u.ix.quotas[name][other]; ok {
		hint = "Consumer " + quote(d.consumer) + " defines " + quote(name) + " with unit " + string(other)
		related = []diag.Related{{Location: d.loc, Message: "defined in"}}
	}
	u.report(CodeReference, specPath(diag.Field("config"), diag.Field("consumerQuota")),
		"no Consumer defines quota "+quote(name)+" with unit "+string(want), hint, related)
}
