// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"cmp"
	"slices"
	"strconv"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
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
// supplies the type of removed Policies, and r's registry their Filter
// class exactly as Resolve derives it for a live entry
// (registry.Entry.EffectiveClass), so a Policy prints one class whether
// it runs or was removed. With a nil b, or a Policy b lacks or whose
// Object is not a non-nil *v1alpha1.Policy (see Resolve), both are empty;
// a type r's registry does not list leaves the class empty.
//
// Every field of a Row is printable text: a backslash, an invalid UTF-8
// byte and every rune strconv.IsPrint rejects (a newline, a tab, an ESC
// byte) are written as strconv.Quote writes them, without the quotes
// (printable). The text table of 02 req 51 therefore has one line per
// row and five cells per line whatever the authored values, so a slot
// such as "x\nonRequestHeaders\tclient\t..." cannot forge a row of
// render --effective (WP-38 S2 round-4 minor), and the JSON form of 02
// req 52 carries the same text. Every field of a Bundle stage F accepted
// other than spec.slot is already printable, as is a slot of the
// registry or a metadata.name default.
func (r *Resolver) Rows(c *hub.Chain, b *hub.Bundle) []Row {
	if c == nil {
		return nil
	}
	r = r.usable()
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
	for _, rm := range removed {
		row := Row{Phase: None, Leg: None, Policy: rm.Policy, Slot: rm.Slot, From: phase.ScopeGateway.String()}
		if b != nil {
			res, _ := b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: rm.Policy})
			if p, ok := object[v1alpha1.Policy](res); ok {
				row.Type = string(p.Spec.Type)
				if e, ok := r.reg.Lookup(p.Spec.Type); ok {
					row.FilterClass = e.EffectiveClass(p).String()
				}
			}
		}
		switch rm.Reason {
		case hub.Replaced:
			row.Reason = "replaced in slot " + rm.Slot + " by " + rm.By
		case hub.Excluded:
			row.Reason = "excluded by Route"
		default:
		}
		rows = append(rows, row.printable())
	}
	return rows
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
	return row.printable()
}

// printable returns row with every field printable (see Rows): those
// that carry authored text (names, the Policy type of a removed Policy,
// which Rows reads from b, and the slot) are escaped; Phase and
// FilterClass are names of fixed sets.
func (row Row) printable() Row {
	for _, f := range []*string{&row.Leg, &row.Policy, &row.Type, &row.Slot, &row.From, &row.Reason} {
		*f = printable(*f)
	}
	return row
}

// printable returns s with each backslash, each invalid UTF-8 byte and
// each rune strconv.IsPrint rejects written as strconv.Quote writes it,
// without the quotes (\\, \x1b, \n, \t, \u2028), or s itself when it
// holds none: text that renders on one line of one table cell, and that
// tells apart every two values it escapes.
func printable(s string) string {
	for i := 0; i < len(s); {
		if c := s[i]; c < utf8.RuneSelf {
			// ASCII: printable from space to tilde, backslash excepted.
			if c < ' ' || c > '~' || c == '\\' {
				return escapeFrom(s, i)
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if escaped(r, size) {
			return escapeFrom(s, i)
		}
		i += size
	}
	return s
}

// escaped reports whether printable escapes the rune r of size bytes.
func escaped(r rune, size int) bool {
	return r == '\\' || (r == utf8.RuneError && size == 1) || !strconv.IsPrint(r)
}

// escapeFrom is printable for an s whose first rune to escape starts at
// byte i.
func escapeFrom(s string, i int) string {
	out := make([]byte, 0, len(s)+8)
	out = append(out, s[:i]...)
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		if escaped(r, size) {
			q := strconv.Quote(s[i : i+size])
			out = append(out, q[1:len(q)-1]...)
		} else {
			out = append(out, s[i:i+size]...)
		}
		i += size
	}
	return string(out)
}
