// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"errors"
	"strconv"
)

// Codes the engine answers (docs/architecture/03-data-plane.md "RZ-RT
// registry"; docs/architecture/09-traffic-management-and-resilience.md
// "Error codes this document owns"). A leg's own codes (RZ-UP-001 to
// RZ-UP-008, RZ-RT-007) and RZ-RT-005 (admission.ErrFull) pass through.
const (
	// CodeNoStep is 404: a composition ran no step.
	CodeNoStep = "RZ-RT-010"
	// CodeStepFailed is 502: a step failed on a CEL runtime error, a
	// computed path request hardening refuses, a body over maxBodyBytes, a
	// body the merge rules cannot take or a recovered fault; also a merged
	// body over limits.maxResponseBodyBytes.
	CodeStepFailed = "RZ-RT-015"
	// CodeBudget is 503: the buffer budget cannot cover a step body, a
	// decoded value or the merged body.
	CodeBudget = "RZ-RT-004"
	// CodeStepStatus is 502: a merged or non-final step got a non-2xx
	// response from its Upstream.
	CodeStepStatus = "RZ-UP-011"
	// CodeAfterCommit ends a streamed final step whose body passes its
	// maxBodyBytes after commit; it has no status.
	CodeAfterCommit = "RZ-UP-009"
)

// Error is the failure of a composition request: the coded error that
// decided it, with the last step in list order whose leg ran, which the
// access log's upstream, endpoint and attempts describe
// (docs/architecture/03-data-plane.md "Merge rules"). errcode.CodeOf finds
// its code.
type Error struct {
	// Step is the step whose failure decided the request; "" when the
	// composition itself failed: no step ran (RZ-RT-010), no in-flight
	// unit was left (RZ-RT-005) or the merged body passed its cap.
	Step string
	// Upstream, Endpoint and Attempts describe the last step in list
	// order whose leg ran; empty when none did.
	Upstream string
	Endpoint string
	Attempts int
	// Err is the coded error.
	Err error
}

// Error returns "composition: [step <name>: ]<coded error>".
func (e *Error) Error() string {
	if e.Step == "" {
		return "composition: " + e.Err.Error()
	}
	return "composition: step " + strconv.Quote(e.Step) + ": " + e.Err.Error()
}

// Unwrap returns the coded error.
func (e *Error) Unwrap() error { return e.Err }

// Causes of the engine's own failures; the errors carrying them name the
// step, never request data.
var (
	// errSibling is the cause with which a failed step cancels its
	// siblings in an aggregate.
	errSibling = errors.New("composition: a sibling step failed")
	// errPanic is the cause of a recovered step fault.
	errPanic = errors.New("composition: step panicked")
)
