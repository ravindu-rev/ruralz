// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"strings"
	"unicode"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// STATUS= values and prefixes
// (docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "Handover
// under systemd").
const (
	// StatusDraining is the STATUS= of a draining Node.
	StatusDraining = "draining"
	// statusReady prefixes the active Revision, rev-<12 hex>.
	statusReady = "ready"
	// statusNotReady prefixes the readiness reasons.
	statusNotReady = "not ready: "
	// reasonSeparator joins the readiness reasons.
	reasonSeparator = "; "
)

// StatusOf returns the STATUS= value of a /readyz state: "ready
// rev-<12 hex>" without reasons; "draining" while a draining reason is
// present, whatever else is; else "not ready: " and each reason as
// "<reason> <code> <detail>", empty fields left out, joined by "; ". The
// value is one line of valid UTF-8: invalid bytes become U+FFFD and
// control characters spaces, so a detail never adds an assignment.
func StatusOf(v adminapi.Readyz) string {
	if len(v.Reasons) == 0 {
		return clean(joinFields(statusReady, v.Revision))
	}
	for _, r := range v.Reasons {
		if r.Reason == adminapi.ReasonDraining {
			return StatusDraining
		}
	}
	reasons := make([]string, len(v.Reasons))
	for i, r := range v.Reasons {
		reasons[i] = joinFields(r.Reason, r.Code, r.Detail)
	}
	return clean(statusNotReady + strings.Join(reasons, reasonSeparator))
}

// joinFields joins the non-empty fields with single spaces.
func joinFields(fields ...string) string {
	kept := fields[:0:0]
	for _, f := range fields {
		if f != "" {
			kept = append(kept, f)
		}
	}
	return strings.Join(kept, " ")
}

// clean makes s one line of valid UTF-8.
func clean(s string) string {
	s = strings.ToValidUTF8(s, string(unicode.ReplacementChar))
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
