// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package celtypes

import (
	"errors"
	"fmt"

	"cel.dev/cel-go/common/types"
	"cel.dev/cel-go/common/types/ref"
)

// Sentinel errors. cel-go wraps them in its error values (types.Err unwraps),
// so errors.Is classifies an evaluation error; none carries request data.
var (
	// ErrNull is a field selection on null (a nullable variable that is
	// absent, 03 req 18).
	ErrNull = errors.New("celtypes: field selection on null")
	// ErrBody is a body that is not available or did not decode (03 req 25).
	ErrBody = errors.New("celtypes: body not available")
	// ErrNoSuchKey is a missing map key or unknown field.
	ErrNoSuchKey = errors.New("celtypes: no such key")
	// ErrNotJSON is a value that has no JSON form: NaN, an infinity, invalid
	// UTF-8 in a nested string, a map key that is not a string, int, uint or
	// bool, duplicate keys after conversion, an object or a type (03 req 41).
	ErrNotJSON = errors.New("celtypes: value has no JSON form")
	// ErrUnsupported is a native Go value outside the JSON tree types or an
	// unsupported conversion.
	ErrUnsupported = errors.New("celtypes: unsupported value")
)

// errVal returns a CEL error value wrapping err.
func errVal(err error) ref.Val { return types.WrapErr(err) }

// nullSelect is the error of selecting field of typeName on null.
func nullSelect(typeName, field string) error {
	return fmt.Errorf("%w: %s.%s", ErrNull, typeName, field)
}

// noSuchField is the error of selecting an undeclared field by a dynamic
// key; the key is left out because it may come from request data.
func noSuchField(typeName string) error {
	return fmt.Errorf("%w: undeclared field of %s", ErrNoSuchKey, typeName)
}

// errBodyUnavailable is the error of reading a body that is not available.
func errBodyUnavailable() error {
	return fmt.Errorf("%w: no decoded body", ErrBody)
}
