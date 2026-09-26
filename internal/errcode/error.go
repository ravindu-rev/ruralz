// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package errcode

import (
	"errors"
	"fmt"
)

// Error is an error that carries a registered RZ code. Every layer wraps
// client-facing failures in it (directly or through %w), so the code
// survives to the problem writer, the diagnostics and the metrics.
type Error struct {
	// Code is a registered RZ-<AREA>-<NNN> code.
	Code string
	// Err is the cause; it is logged and never sent to a client.
	Err error
}

// Error returns "<code>: <cause>".
func (e *Error) Error() string {
	if e.Err == nil {
		return e.Code
	}
	return e.Code + ": " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Wrap annotates err with code. A nil err still yields an error, so callers
// can report a code without a cause.
func Wrap(code string, err error) error { return &Error{Code: code, Err: err} }

// Errorf returns an Error whose cause is fmt.Errorf(format, args...).
func Errorf(code, format string, args ...any) error {
	return &Error{Code: code, Err: fmt.Errorf(format, args...)}
}

// CodeOf returns the code of the outermost Error in err's chain.
func CodeOf(err error) (string, bool) {
	var e *Error
	if errors.As(err, &e) {
		return e.Code, true
	}
	return "", false
}

// Status returns the registered HTTP status of code, or 0 when the code
// has none or is unknown. Callers on the request path resolve statuses at
// snapshot compile time, because Lookup builds the registry per call.
func Status(code string) int {
	c, ok := Lookup(code)
	if !ok {
		return 0
	}
	return c.Status
}
