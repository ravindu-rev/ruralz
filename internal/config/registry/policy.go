// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package registry

import (
	"fmt"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Defaults are the registry defaults of one Policy, materialized into
// every Policy.spec that lacks them like schema defaults (01 req 36, 02 req
// 12, foundation pack section 8.1).
type Defaults struct {
	// Slot is the registry slot, or the Policy's metadata.name for types
	// whose slot is its name.
	Slot string
	// FailureMode is the registry failureMode default.
	FailureMode v1alpha1.FailureMode
	// FilterClass is the registry Filter class; custom for plugin.
	FilterClass v1alpha1.FilterClass
}

// DefaultSlot returns the slot a Policy named name gets when spec.slot is
// absent: the registry slot, or name for types whose slot is SlotName.
func (e Entry) DefaultSlot(name string) string {
	if e.Slot == SlotName {
		return name
	}
	return e.Slot
}

// Defaults returns the registry defaults of a Policy of type e named name.
func (e Entry) Defaults(name string) Defaults {
	return Defaults{Slot: e.DefaultSlot(name), FailureMode: e.DefaultFailureMode, FilterClass: e.Class.V1alpha1()}
}

// EffectiveSlot returns spec.slot when set, else the registry slot, else
// the Policy's metadata.name (01 req 40).
func (e Entry) EffectiveSlot(p *v1alpha1.Policy) string {
	if p.Spec.Slot != "" {
		return p.Spec.Slot
	}
	return e.DefaultSlot(p.Metadata.Name)
}

// EffectiveClass returns the Filter class p runs in: for plugin its
// spec.filterClass (custom when absent or not a Filter class, which
// CheckFilterClass reports), for every other type the registry class
// whatever spec.filterClass says (a differing value is CheckFilterClass's
// RZ-CFG-005).
func (e Entry) EffectiveClass(p *v1alpha1.Policy) phase.Class {
	if e.ClassFromSpec && p.Spec.FilterClass != nil {
		if c, ok := phase.ParseClass(*p.Spec.FilterClass); ok {
			return c
		}
	}
	return e.Class
}

// EffectiveFailureMode returns spec.failureMode when set, else the
// registry default (foundation pack section 8.10).
func (e Entry) EffectiveFailureMode(p *v1alpha1.Policy) v1alpha1.FailureMode {
	if p.Spec.FailureMode != nil {
		return *p.Spec.FailureMode
	}
	return e.DefaultFailureMode
}

// ClosedOnlyFor reports whether failureMode open is RZ-CFG-029 for p:
// auth.* (including auth.upstream-*), authz.*, and a plugin Policy whose
// effective class is auth or authz (02 req 37, foundation pack section
// 8.10).
func (e Entry) ClosedOnlyFor(p *v1alpha1.Policy) bool {
	if e.ClosedOnly {
		return true
	}
	if !e.ClassFromSpec {
		return false
	}
	c := e.EffectiveClass(p)
	return c == phase.ClassAuth || c == phase.ClassAuthz
}

// AllowsFailureMode reports whether p may use failure mode m: closed
// always, open unless p is closed only (ClosedOnlyFor). Values other than
// open and closed are the schema's (RZ-CFG-005) and are not allowed.
func (e Entry) AllowsFailureMode(p *v1alpha1.Policy, m v1alpha1.FailureMode) bool {
	switch m {
	case v1alpha1.FailureModeClosed:
		return true
	case v1alpha1.FailureModeOpen:
		return !e.ClosedOnlyFor(p)
	default:
		return false
	}
}

// Materialize writes the registry defaults p lacks into p.Spec (slot,
// failureMode, filterClass) and reports whether it wrote any; authored
// values are kept, so a second call writes nothing (01 req 36, 02 req 12).
// An empty default slot (a type whose slot is its name, on a Policy
// without metadata.name, which stage D rejects) is not written. A Policy
// of an unregistered type is left alone. Stage G materializes the same
// values into the positioned tree from Entry.Defaults; this form serves
// typed callers.
func (r *Registry) Materialize(p *v1alpha1.Policy) bool {
	e, ok := r.Lookup(p.Spec.Type)
	if !ok {
		return false
	}
	d := e.Defaults(p.Metadata.Name)
	changed := false
	if p.Spec.Slot == "" && d.Slot != "" {
		p.Spec.Slot = d.Slot
		changed = true
	}
	if p.Spec.FailureMode == nil {
		m := d.FailureMode
		p.Spec.FailureMode = &m
		changed = true
	}
	if p.Spec.FilterClass == nil {
		c := d.FilterClass
		p.Spec.FilterClass = &c
		changed = true
	}
	return changed
}

// policyResource names p in a diagnostic.
func policyResource(p *v1alpha1.Policy) *diag.ResourceID {
	return &diag.ResourceID{Kind: string(v1alpha1.KindPolicy), Name: p.Metadata.Name}
}

// specPath returns the path spec.<field> of a Policy resource.
func specPath(field string) diag.Path {
	return diag.Path{diag.Field("spec"), diag.Field(field)}
}

// newDiagnostic returns an error diagnostic about Policy p at spec.<field>.
// The caller adds the source Location.
func newDiagnostic(code string, p *v1alpha1.Policy, field, msg string) diag.Diagnostic {
	return diag.Diagnostic{
		Code: code, Severity: diag.SeverityError, Resource: policyResource(p),
		Path: specPath(field), Message: msg,
	}
}

// CheckFilterClass reports an authored spec.filterClass the registry
// rejects, as RZ-CFG-005 at spec.filterClass (02 req 38, architecture
// R-7): on a type other than plugin, any value other than the registry
// class (restating it is accepted); on plugin, a value that is not one of
// the nine Filter classes. An absent filterClass is always accepted. This
// is the only filterClass rule: R-7 materializes filterClass on every
// Policy, so it overrides the "filterClass only for plugin" reading of 01
// J 39 and note 9, and stages G and H must not reject a non-plugin
// Policy's filterClass that restates the registry class. The diagnostic
// carries Resource, Path and Message; the caller adds the source
// Location. An unregistered type yields no finding: the schema's
// PolicyType enum reports it (RZ-CFG-005 at spec.type).
func (r *Registry) CheckFilterClass(p *v1alpha1.Policy) (diag.Diagnostic, bool) {
	e, ok := r.Lookup(p.Spec.Type)
	if !ok || p.Spec.FilterClass == nil {
		return diag.Diagnostic{}, false
	}
	authored := *p.Spec.FilterClass
	if e.ClassFromSpec {
		if _, ok := phase.ParseClass(authored); ok {
			return diag.Diagnostic{}, false
		}
		return newDiagnostic(CodeFilterClass, p, "filterClass",
			fmt.Sprintf("filterClass %q is not a Filter class", authored)), true
	}
	if authored == e.Class.V1alpha1() {
		return diag.Diagnostic{}, false
	}
	return newDiagnostic(CodeFilterClass, p, "filterClass",
		fmt.Sprintf("filterClass %q is not allowed for Policy type %q: its registry class is %q, and only a plugin Policy chooses its class",
			authored, e.Type, e.Class.V1alpha1())), true
}

// CheckFailureMode reports failureMode open on a closed-only Policy as
// RZ-CFG-029 at spec.failureMode with the message of 02 req 37. Stage I
// raises it once per Policy, attached or not (R-46). An absent failureMode
// takes the registry default, which is always allowed. The caller adds
// the source Location. An unregistered type yields no finding.
func (r *Registry) CheckFailureMode(p *v1alpha1.Policy) (diag.Diagnostic, bool) {
	e, ok := r.Lookup(p.Spec.Type)
	if !ok || p.Spec.FailureMode == nil {
		return diag.Diagnostic{}, false
	}
	if *p.Spec.FailureMode != v1alpha1.FailureModeOpen || !e.ClosedOnlyFor(p) {
		return diag.Diagnostic{}, false
	}
	return newDiagnostic(CodeFailureMode, p, "failureMode",
		fmt.Sprintf("failureMode open is not allowed for Policy type %q (closed only)", e.Type)), true
}

// CheckServed reports a Policy whose type this release does not serve as
// RZ-CFG-040 at spec.type (architecture section 0 item 8, R-7): plugin,
// authz.opa, authz.cedar, authz.geoip, auth.upstream-sigv4 and ai.* in
// M1. The caller adds the source Location. An unregistered type yields no
// finding: the schema's PolicyType enum reports it.
func (r *Registry) CheckServed(p *v1alpha1.Policy) (diag.Diagnostic, bool) {
	e, ok := r.Lookup(p.Spec.Type)
	if !ok || e.Served {
		return diag.Diagnostic{}, false
	}
	return newDiagnostic(CodeUnserved, p, "type",
		fmt.Sprintf("Policy type %q is not served by this release (Planned (%s))", e.Type, e.Planned)), true
}
