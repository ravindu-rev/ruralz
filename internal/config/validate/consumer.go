// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// consumer checks the Consumer rule the schema cannot state: a quota
// window of at least 1s in whole seconds (RZ-CFG-005, configuration model
// "Registered from feature documents";
// docs/architecture/09-traffic-management-and-resilience.md "Quotas").
// Exactly one of window and calendar is the schema's, stage F.
func (u *unit) consumer() {
	c, ok := hub.Object[v1alpha1.Consumer](u.r)
	if !ok || c == nil {
		return
	}
	for _, q := range c.Spec.Quotas {
		u.w.charge(1)
		if q.Window == nil {
			continue
		}
		if w := time.Duration(*q.Window); w < time.Second || w%time.Second != 0 {
			u.report(CodeSchema, specPath(diag.Field("quotas"), diag.Keyed("name", q.Name), diag.Field("window")),
				"quota "+quote(q.Name)+" window "+w.String()+" is not at least 1s in whole seconds",
				"use a whole number of seconds, or calendar: month", nil)
		}
	}
}
