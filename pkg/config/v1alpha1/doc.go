// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package v1alpha1 holds the Go types of the ruralz/v1alpha1 configuration
// resources: the envelope, the ten kinds and the config of every Policy type.
//
// These types are the single source of the published JSON Schema. Run
// `go generate ./pkg/config/v1alpha1` to regenerate the authoring and rendered
// views under api/schema/ruralz/v1alpha1; the generated schema, not these
// types, is the contract every loading path validates against
// (docs/engineering/02-repository-layout-and-conventions.md, Schema
// generation from Go types).
//
// Doc comments become schema descriptions. Lines that start with "+ruralz:"
// are markers: they are stripped from the description and emit schema
// keywords. Optional booleans and numbers, and optional fields whose default
// is not the Go zero value, are pointers so that an explicit false or 0
// survives decoding.
package v1alpha1

//go:generate go run ../../../internal/tool/schemagen -out ../../../api/schema/ruralz/v1alpha1
