// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package body buffers request and response bodies against the Node buffer
// budget (spec 04 group G; docs/architecture/02-configuration-model.md
// "Body buffering and limits"; docs/architecture/03-data-plane.md
// "Streaming"):
//
//   - Budget is limits.maxBufferedBytes, one per Node, carried across Hot
//     Reloads, reserved in 32 KiB increments; 25% is a stream share that
//     gates cannot take (spec 04 req 47). The gauge records each change on
//     the holder's request stripe.
//   - Account charges one holder's bytes (decoded values, rewritten bodies)
//     to the budget, with the decoded-value cap of DecodedFactor times the
//     raw limit (spec 07 reqs 69-71, architecture R-63).
//   - ReadGate reads a whole body within its limit (a gate); Tee copies a
//     streaming response for the Response Cache and stops quietly past its
//     limit or the budget (spec 04 req 46); Limited cuts a streamed body at
//     its limit (spec 04 req 37). Gates and tees grow their buffers as
//     bytes arrive and reserve the increments covering each new capacity
//     before allocating it, so the budget bounds the memory they hold
//     ("all of it is reserved from maxBufferedBytes").
//   - Request implements snapshot.RequestBody over the gate or the client
//     stream (spec 05 reqs 31 and 54, architecture R-41).
//
// Failures are the Filter SPI sentinels ErrBudget and ErrTooLarge; Code maps
// them to the RZ code of the side they happened on.
package body

import (
	"errors"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
)

// Sizes (target values of spec 04 req 47 and spec 07 reqs 15 and 73).
const (
	// Increment is the reservation step: bytes are reserved from the budget
	// in 32 KiB increments as they arrive.
	Increment = 32 << 10
	// DefaultMaxBufferedBytes is the default limits.maxBufferedBytes.
	DefaultMaxBufferedBytes = 512 << 20
	// DecodedFactor bounds a decoded value: at most filter.DecodedLimitFactor
	// (4) times the raw limit its body arrived under; beyond it is
	// oversized (architecture R-63).
	DecodedFactor = filter.DecodedLimitFactor
	// PoolMax is the largest buffer returned to a pool; larger ones are left
	// to the garbage collector, so a pool never pins large bodies. Gates
	// and tees pool arrays of exactly one or two increments (PoolMax).
	PoolMax = 64 << 10
)

// Sentinel errors. They are the Filter SPI's, so a Filter that receives
// one from the data plane (Message.SetBody, Exchange.Reserve) returns it
// unchanged and the executor maps it under either failureMode.
var (
	// ErrBudget: the buffer budget cannot cover a reservation; 503
	// RZ-RT-004 on every side (spec 04 req 47, spec 07 req 15).
	ErrBudget = filter.ErrBudget
	// ErrTooLarge: a body, or a decoded value past DecodedFactor times its
	// raw limit, is over its limit; the code depends on the Side.
	ErrTooLarge = filter.ErrTooLarge
)

// Side says whose body failed, which selects the RZ code of ErrTooLarge.
type Side uint8

// Sides.
const (
	// SideRequest is the client request body: 413 RZ-RT-003.
	SideRequest Side = iota
	// SideResponse is a buffered plain upstreams response: 502 RZ-UP-010.
	SideResponse
	// SideStep is a composition step body: 502 RZ-RT-015 (the step fails
	// under its optional rule).
	SideStep
)

// RZ codes of body failures (spec 04 req 47, spec 05 reqs 30 and 49).
const (
	CodeRequestTooLarge  = "RZ-RT-003"
	CodeBudget           = "RZ-RT-004"
	CodeResponseTooLarge = "RZ-UP-010"
	CodeStepTooLarge     = "RZ-RT-015"
)

// Code returns the RZ code of a body failure on side: CodeBudget for
// ErrBudget on any side, and for ErrTooLarge CodeRequestTooLarge,
// CodeResponseTooLarge or CodeStepTooLarge. Any other error, nil included,
// has no body code and returns "".
func Code(err error, side Side) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrBudget):
		return CodeBudget
	case errors.Is(err, ErrTooLarge):
		switch side {
		case SideResponse:
			return CodeResponseTooLarge
		case SideStep:
			return CodeStepTooLarge
		default:
			return CodeRequestTooLarge
		}
	}
	return ""
}

// Wrap returns err carrying its Code for side (errcode.Wrap), or err
// unchanged when it has no body code.
func Wrap(err error, side Side) error {
	if c := Code(err, side); c != "" {
		return errcode.Wrap(c, err)
	}
	return err
}

// DecodedCap returns the largest decoded size a body that arrived under
// rawLimit may reach (DecodedFactor times rawLimit, saturating); it is the
// budget argument of expr.Compiler.DecodeJSON.
func DecodedCap(rawLimit int64) int64 {
	if rawLimit <= 0 {
		return 0
	}
	if rawLimit > maxInt64/DecodedFactor {
		return maxInt64
	}
	return rawLimit * DecodedFactor
}

// CheckDeclared rejects a declared Content-Length over limit before any
// byte is read (spec 04 req 34: 413 RZ-RT-003 at once, no 100 Continue).
// declared < 0 means unknown and passes.
func CheckDeclared(declared, limit int64) error {
	if declared > max(0, limit) {
		return ErrTooLarge
	}
	return nil
}

const maxInt64 = 1<<63 - 1

// increments returns the increments covering n bytes.
func increments(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n-1)/Increment + 1
}
