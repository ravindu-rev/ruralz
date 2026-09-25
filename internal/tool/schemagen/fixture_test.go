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
	B *int32 `json:"b,omitempty"`
	// Items is a set.
	// +ruralz:list=set
	Items []string `json:"items,omitempty"`
	// Mode has a default.
	// +ruralz:default=b
	Mode *Mode `json:"mode,omitempty"`
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
