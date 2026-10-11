// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Labels of the render --effective table (02 req 51).
const (
	// LegClient is the Leg of client-leg rows.
	LegClient = "client"
	// None is the Phase and Leg of removed Policies.
	None = "none"
)

// Row is one row of ruralz bundle render --effective --route (02 req 51
// and 52). The JSON members are in the order of the JSON output.
type Row struct {
	// Phase is the Phase, such as "onRequestHeaders", or "none" for a
	// removed Policy.
	Phase string `json:"phase"`
	// Leg is "client", the Upstream name of an upstream leg, or "none".
	Leg string `json:"leg"`
	// Policy is the Policy name.
	Policy string `json:"policy"`
	// Type is the Policy type.
	Type string `json:"type"`
	// FilterClass is the Filter class.
	FilterClass string `json:"filterClass"`
	// Slot is the effective slot.
	Slot string `json:"slot"`
	// From is "Gateway", "Route" or "Upstream <name>".
	From string `json:"from"`
	// Reason says why the Policy is there or gone (reason templates of 02
	// req 51, pinned by golden files).
	Reason string `json:"reason"`
}

// Rows returns the effective table of c: Phases in execution order; within
// a Phase the client leg, then the legs in leg order, each in execution
// order; then the removed Gateway Policies with Phase and Leg "none",
// replaced before excluded, each in Gateway list order (02 req 51). b
// supplies the type and Filter class of removed Policies; with a nil b, or
// a Policy b lacks, they are empty.
func Rows(c *hub.Chain, b *hub.Bundle) []Row {
	if c == nil {
		return nil
	}
	var rows []Row
	for p := range phase.Count {
		for _, e := range c.Client[p] {
			rows = append(rows, entryRow(p, LegClient, e))
		}
		for _, leg := range c.Legs {
			for _, e := range leg.Phases[p] {
				rows = append(rows, entryRow(p, leg.Upstream, e))
			}
		}
	}
	removed := slices.Clone(c.Removed)
	slices.SortStableFunc(removed, func(a, b hub.Removed) int { return cmp.Compare(a.Reason, b.Reason) })
	var reg *registry.Registry
	for _, rm := range removed {
		row := Row{Phase: None, Leg: None, Policy: rm.Policy, Slot: rm.Slot, From: phase.ScopeGateway.String()}
		if b != nil {
			if p, ok := b.Policy(rm.Policy); ok {
				if reg == nil {
					reg = registry.New()
				}
				row.Type, row.FilterClass = string(p.Spec.Type), removedClass(reg, p)
			}
		}
		switch rm.Reason {
		case hub.Replaced:
			row.Reason = "replaced in slot " + rm.Slot + " by " + rm.By
		case hub.Excluded:
			row.Reason = "excluded by Route"
		default:
		}
		rows = append(rows, row)
	}
	return rows
}

// removedClass returns the Filter class of a removed Policy: its
// materialized spec.filterClass, else the registry's effective class.
func removedClass(reg *registry.Registry, p *v1alpha1.Policy) string {
	if p.Spec.FilterClass != nil {
		return string(*p.Spec.FilterClass)
	}
	if e, ok := reg.Lookup(p.Spec.Type); ok {
		return e.EffectiveClass(p).String()
	}
	return ""
}

// entryRow returns the row of e in Phase p on leg.
func entryRow(p phase.Phase, leg string, e hub.Entry) Row {
	row := Row{
		Phase: p.String(), Leg: leg, Policy: e.Policy, Type: string(e.Type), FilterClass: e.Class.String(),
		Slot: e.Slot, From: e.Scope.String(),
	}
	notOverridable := ""
	if !e.Overridable {
		notOverridable = ", not overridable"
	}
	switch e.Scope {
	case phase.ScopeUpstream:
		row.From = e.Scope.String() + " " + leg
		row.Reason = "leg " + leg + " only"
	case phase.ScopeGateway:
		if e.Slot == e.Policy {
			row.Reason = "own slot" + notOverridable
		} else {
			row.Reason = "slot " + e.Slot + ", inherited" + notOverridable
		}
	case phase.ScopeRoute:
		switch {
		case e.Replaces != "":
			row.Reason = "slot " + e.Slot + ", replaces " + e.Replaces
		case e.Slot == e.Policy:
			row.Reason = "own slot"
		default:
			row.Reason = "slot " + e.Slot
		}
	default:
	}
	return row
}
