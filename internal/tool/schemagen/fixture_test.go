// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import "github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"

// Kind lists the fixture resource kinds.
type Kind string

// Fixture kinds.
const (
	KindGood           Kind = "Good"
	KindOptionalBool   Kind = "OptionalBool"
	KindUnmarkedList   Kind = "UnmarkedList"
	KindBadRef         Kind = "BadRef"
	KindUnmarkedSecret Kind = "UnmarkedSecret"
	KindBadOneOf       Kind = "BadOneOf"
	KindNoOmitempty    Kind = "NoOmitempty"
	KindValueDefault   Kind = "ValueDefault"
	KindBadEnumDefault Kind = "BadEnumDefault"
	// Kinds of the M1 generator rules.
	KindSecretNoImpact  Kind = "SecretNoImpact"
	KindRequiredDefault Kind = "RequiredDefault"
	KindRangeDefault    Kind = "RangeDefault"
	KindPatternDefault  Kind = "PatternDefault"
	KindImpactOrder     Kind = "ImpactOrder"
	KindImpactUnknown   Kind = "ImpactUnknown"
	KindImpactTwice     Kind = "ImpactTwice"
	KindLengthDefault   Kind = "LengthDefault"
	KindMissingTag      Kind = "MissingTag"
	KindDashTag         Kind = "DashTag"
)

// Meta is the fixture metadata.
type Meta struct {
	// Name names the resource.
	// +ruralz:required
	Name string `json:"name"`
}

// Mode is a fixture enum.
type Mode string

// Modes.
const (
	ModeA Mode = "a"
	ModeB Mode = "b"
)

// Good is a valid fixture resource.
type Good struct {
	Metadata Meta     `json:"metadata"`
	Spec     GoodSpec `json:"spec"`
}

// GoodSpec uses every rule correctly.
// +ruralz:exactlyOneOf=a,b
type GoodSpec struct {
	// A is optional.
	A *bool `json:"a,omitempty"`
	// B is optional.
	// +ruralz:default=3
	// +ruralz:minimum=1
	// +ruralz:maximum=3
	B *int32 `json:"b,omitempty"`
	// Items is a set.
	// +ruralz:list=set
	Items []string `json:"items,omitempty"`
	// Mode has a default.
	// +ruralz:default=b
	// +ruralz:impact=routing,traffic
	Mode *Mode `json:"mode,omitempty"`
	// Ratio has a fractional default within its range.
	// +ruralz:default=0.25
	// +ruralz:minimum=0
	// +ruralz:maximum=0.5
	Ratio *float64 `json:"ratio,omitempty"`
	// Path has a default matching its pattern and length.
	// +ruralz:default=/x
	// +ruralz:pattern=^/
	// +ruralz:minLength=1
	// +ruralz:maxLength=8
	Path *string `json:"path,omitempty"`
	// Wait has a duration default.
	// +ruralz:default=90s
	Wait *v1alpha1.Duration `json:"wait,omitempty"`
	// Schema is an inline JSON Schema; it needs no list marker.
	Schema v1alpha1.JSONSchemaDocument `json:"schema,omitempty"`
	// Note is an unconstrained string.
	Note string `json:"note,omitempty"`
	// Target is a reference, never substitutable.
	// +ruralz:ref=Good
	Target string `json:"target,omitempty"`
}

// OptionalBool breaks the pointer rule.
type OptionalBool struct {
	Metadata Meta             `json:"metadata"`
	Spec     OptionalBoolSpec `json:"spec"`
}

// OptionalBoolSpec has a non-pointer optional bool.
type OptionalBoolSpec struct {
	// On is optional.
	On bool `json:"on,omitempty"`
}

// UnmarkedList breaks the list rule.
type UnmarkedList struct {
	Metadata Meta             `json:"metadata"`
	Spec     UnmarkedListSpec `json:"spec"`
}

// UnmarkedListSpec has a list without a list type.
type UnmarkedListSpec struct {
	// Items is a list.
	Items []string `json:"items,omitempty"`
}

// BadRef references an unknown kind.
type BadRef struct {
	Metadata Meta       `json:"metadata"`
	Spec     BadRefSpec `json:"spec"`
}

// BadRefSpec has a bad reference.
type BadRefSpec struct {
	// Target references a kind that does not exist.
	// +ruralz:ref=Nothing
	Target string `json:"target,omitempty"`
}

// UnmarkedSecret has a secret without the marker.
type UnmarkedSecret struct {
	Metadata Meta               `json:"metadata"`
	Spec     UnmarkedSecretSpec `json:"spec"`
}

// UnmarkedSecretSpec has an unmarked SecretValue.
type UnmarkedSecretSpec struct {
	// Key is secret.
	Key *v1alpha1.SecretValue `json:"key,omitempty"`
}

// BadOneOf names an unknown field.
type BadOneOf struct {
	Metadata Meta         `json:"metadata"`
	Spec     BadOneOfSpec `json:"spec"`
}

// BadOneOfSpec names a missing field.
// +ruralz:exactlyOneOf=a,missing
type BadOneOfSpec struct {
	// A is optional.
	A string `json:"a,omitempty"`
}

// NoOmitempty has an optional field without omitempty.
type NoOmitempty struct {
	Metadata Meta            `json:"metadata"`
	Spec     NoOmitemptySpec `json:"spec"`
}

// NoOmitemptySpec lacks omitempty.
type NoOmitemptySpec struct {
	// A is optional.
	A string `json:"a"`
}

// ValueDefault has a default on a value field.
type ValueDefault struct {
	Metadata Meta             `json:"metadata"`
	Spec     ValueDefaultSpec `json:"spec"`
}

// ValueDefaultSpec has a default on a non-pointer.
type ValueDefaultSpec struct {
	// A is required with a default.
	// +ruralz:required
	// +ruralz:default=x
	A string `json:"a"`
}

// BadEnumDefault has a default outside its enum.
type BadEnumDefault struct {
	Metadata Meta               `json:"metadata"`
	Spec     BadEnumDefaultSpec `json:"spec"`
}

// BadEnumDefaultSpec has an invalid enum default.
type BadEnumDefaultSpec struct {
	// Mode has a bad default.
	// +ruralz:default=c
	Mode *Mode `json:"mode,omitempty"`
}

// SecretNoImpact has a secret field without the security impact class.
type SecretNoImpact struct {
	Metadata Meta               `json:"metadata"`
	Spec     SecretNoImpactSpec `json:"spec"`
}

// SecretNoImpactSpec marks a secret but not its impact.
type SecretNoImpactSpec struct {
	// Key is secret.
	// +ruralz:secret
	// +ruralz:impact=traffic
	Key *v1alpha1.SecretValue `json:"key,omitempty"`
}

// RequiredDefault has a default on a required field.
type RequiredDefault struct {
	Metadata Meta                `json:"metadata"`
	Spec     RequiredDefaultSpec `json:"spec"`
}

// RequiredDefaultSpec has a required pointer with a default.
type RequiredDefaultSpec struct {
	// A is required and defaulted.
	// +ruralz:required
	// +ruralz:default=1
	A *int32 `json:"a"`
}

// RangeDefault has a default outside its maximum.
type RangeDefault struct {
	Metadata Meta             `json:"metadata"`
	Spec     RangeDefaultSpec `json:"spec"`
}

// RangeDefaultSpec has an out-of-range default.
type RangeDefaultSpec struct {
	// Ratio is 0 to 1.
	// +ruralz:default=1.5
	// +ruralz:minimum=0
	// +ruralz:maximum=1
	Ratio *float64 `json:"ratio,omitempty"`
	// Count is at least 1.
	// +ruralz:default=0
	// +ruralz:minimum=1
	Count *int32 `json:"count,omitempty"`
}

// PatternDefault has a default outside its pattern.
type PatternDefault struct {
	Metadata Meta               `json:"metadata"`
	Spec     PatternDefaultSpec `json:"spec"`
}

// PatternDefaultSpec has a default that fails its pattern.
type PatternDefaultSpec struct {
	// Path starts with a slash.
	// +ruralz:default=x
	// +ruralz:pattern=^/
	Path *string `json:"path,omitempty"`
}

// LengthDefault has a default longer than its maxLength.
type LengthDefault struct {
	Metadata Meta              `json:"metadata"`
	Spec     LengthDefaultSpec `json:"spec"`
}

// LengthDefaultSpec has a default that is too long.
type LengthDefaultSpec struct {
	// Code is at most two characters.
	// +ruralz:default=abc
	// +ruralz:maxLength=2
	Code *string `json:"code,omitempty"`
}

// ImpactOrder lists impact classes out of order.
type ImpactOrder struct {
	Metadata Meta            `json:"metadata"`
	Spec     ImpactOrderSpec `json:"spec"`
}

// ImpactOrderSpec has unsorted impact classes.
type ImpactOrderSpec struct {
	// A changes traffic and routing.
	// +ruralz:impact=traffic,routing
	A string `json:"a,omitempty"`
}

// ImpactUnknown names an unknown impact class.
type ImpactUnknown struct {
	Metadata Meta              `json:"metadata"`
	Spec     ImpactUnknownSpec `json:"spec"`
}

// ImpactUnknownSpec has an unknown impact class.
type ImpactUnknownSpec struct {
	// A has a bad class.
	// +ruralz:impact=cost
	A string `json:"a,omitempty"`
}

// ImpactTwice gives two impact markers.
type ImpactTwice struct {
	Metadata Meta            `json:"metadata"`
	Spec     ImpactTwiceSpec `json:"spec"`
}

// ImpactTwiceSpec has two impact markers on one field.
type ImpactTwiceSpec struct {
	// A has two markers.
	// +ruralz:impact=routing
	// +ruralz:impact=traffic
	A string `json:"a,omitempty"`
}

// MissingTag has a field without a json tag.
type MissingTag struct {
	Metadata Meta           `json:"metadata"`
	Spec     MissingTagSpec `json:"spec"`
}

// MissingTagSpec lacks a json tag.
type MissingTagSpec struct {
	// A has no tag.
	A string
}

// DashTag has a field skipped by encoding/json.
type DashTag struct {
	Metadata Meta        `json:"metadata"`
	Spec     DashTagSpec `json:"spec"`
}

// DashTagSpec has a field tagged "-".
type DashTagSpec struct {
	// A is skipped.
	A string `json:"-"`
}
