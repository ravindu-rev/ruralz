// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"errors"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// RZ-UP codes of 05 req 40 and the Route timeout code of 05 req 26, as
// registered in internal/errcode.
const (
	// CodeConnect: connect failed or dial timed out, no retry (502).
	CodeConnect = "RZ-UP-001"
	// CodeTLS: TLS handshake or verification failed or timed out, no
	// retry (502).
	CodeTLS = "RZ-UP-002"
	// CodeTimeout: deadline expired, or the final attempt timed out (504).
	CodeTimeout = "RZ-UP-003"
	// CodeReset: reset or protocol error before a response, no retry (502).
	CodeReset = "RZ-UP-004"
	// CodeBreakerOpen: circuit breaker open, or half-open with its probe in
	// flight (503).
	CodeBreakerOpen = "RZ-UP-005"
	// CodeBulkheadFull: in-flight ceiling and pending queue full (503).
	CodeBulkheadFull = "RZ-UP-006"
	// CodeRetriesFailed: retries ran and the last attempt got no response
	// (502).
	CodeRetriesFailed = "RZ-UP-007"
	// CodeNoEndpoint: Endpoint set empty (503).
	CodeNoEndpoint = "RZ-UP-008"
	// CodeAfterCommit: Upstream failed after commit; the stream ended with
	// an error (no status).
	CodeAfterCommit = "RZ-UP-009"
	// CodeResponseTooLarge: buffered plain upstreams response over its cap
	// (502).
	CodeResponseTooLarge = "RZ-UP-010"
	// CodeRouteTimeout: Route timeout expired before any Upstream attempt
	// (504, area 4).
	CodeRouteTimeout = "RZ-RT-007"
)

// CodeInfo is one row of the code table of 05 req 40.
type CodeInfo struct {
	// Code is the RZ code.
	Code string
	// Status is the HTTP status; 0 for RZ-UP-009, which has none.
	Status int
	// Meaning is the registered meaning.
	Meaning string
}

// Codes returns the table of 05 req 40 in code order: the codes this
// package selects and their statuses. A test holds it equal to the
// registry in internal/errcode.
func Codes() []CodeInfo {
	return []CodeInfo{
		{CodeConnect, 502, "Connect failed or dial timed out, no retry"},
		{CodeTLS, 502, "TLS handshake or verification failed or timed out, no retry"},
		{CodeTimeout, 504, "Deadline expired, or the final attempt timed out"},
		{CodeReset, 502, "Reset or protocol error before a response, no retry"},
		{CodeBreakerOpen, 503, "Circuit breaker open, or half-open with its probe in flight"},
		{CodeBulkheadFull, 503, "In-flight ceiling and pending queue full"},
		{CodeRetriesFailed, 502, "Retries ran and the last attempt got no response"},
		{CodeNoEndpoint, 503, "Endpoint set empty"},
		{CodeAfterCommit, 0, "Upstream failed after commit; the stream ended with an error"},
		{CodeResponseTooLarge, 502, "Buffered plain `upstreams` response over its cap"},
	}
}

// Status returns the HTTP status of a code this package selects, without
// the registry lookup errcode.Status makes per call (the request path
// resolves statuses from constants); 0 for RZ-UP-009 and unknown codes.
func Status(code string) int {
	switch code {
	case CodeConnect, CodeTLS, CodeReset, CodeRetriesFailed, CodeResponseTooLarge:
		return 502
	case CodeBreakerOpen, CodeBulkheadFull, CodeNoEndpoint:
		return 503
	case CodeTimeout, CodeRouteTimeout:
		return 504
	default:
		return 0
	}
}

// Sentinel causes of the gate codes; the errors a gate returns match them
// with errors.Is and carry the code for errcode.CodeOf.
var (
	// ErrBreakerOpen is the cause of RZ-UP-005.
	ErrBreakerOpen = errors.New("resilience: circuit breaker open")
	// ErrBulkheadFull is the cause of RZ-UP-006.
	ErrBulkheadFull = errors.New("resilience: bulkhead full")
	// ErrNoEndpoint is the cause of RZ-UP-008.
	ErrNoEndpoint = errors.New("resilience: no Endpoint")
)

// Gate is a check of 05 req 11 steps a and e that refuses a leg (Leg.Begin)
// or one of its attempts (Leg.StartAttempt for the breaker, and the
// Upstream core's Endpoint set and bulkhead checks before
// Leg.AbandonAttempt).
type Gate uint8

// Gates.
const (
	// GateNone means no gate refused the leg.
	GateNone Gate = iota
	// GateBreaker is an open breaker, or a half-open one with its probe in
	// flight (for an attempt: whose probe is another leg): RZ-UP-005.
	GateBreaker
	// GateBulkhead is a full bulkhead with a full waiter queue: RZ-UP-006.
	GateBulkhead
	// GateNoEndpoint is an empty Endpoint set (or all-zero weights of a
	// Route's upstreams): RZ-UP-008.
	GateNoEndpoint
)

// Code returns the gate's code, "" for GateNone.
func (g Gate) Code() string {
	switch g {
	case GateBreaker:
		return CodeBreakerOpen
	case GateBulkhead:
		return CodeBulkheadFull
	case GateNoEndpoint:
		return CodeNoEndpoint
	default:
		return ""
	}
}

// Err returns the gate's error: an *errcode.Error with the gate's code
// wrapping its sentinel; nil for GateNone.
func (g Gate) Err() error {
	switch g {
	case GateBreaker:
		return errcode.Wrap(CodeBreakerOpen, ErrBreakerOpen)
	case GateBulkhead:
		return errcode.Wrap(CodeBulkheadFull, ErrBulkheadFull)
	case GateNoEndpoint:
		return errcode.Wrap(CodeNoEndpoint, ErrNoEndpoint)
	default:
		return nil
	}
}

// Outcome is how one leg ended, the input of code selection (05 req 40).
type Outcome struct {
	// Responded is true when the last attempt that ran got response
	// headers: the leg returns that response unchanged (05 req 35).
	Responded bool
	// Kind is the last attempt's error kind when Responded is false.
	Kind Kind
	// Attempts is the number of attempts that ran; more than 1 means a
	// retry ran. A retry that could not start (breaker open, bulkhead
	// full, no Endpoint) is not an attempt: the leg ends with the previous
	// attempt's outcome (05 req 35).
	Attempts int
	// Expired is true when the leg or Route deadline expired during the
	// leg, for example during a backoff.
	Expired bool
	// Gate is the gate that refused the leg's last attempt that could not
	// start (StartAttempt or AbandonAttempt); it decides the code only when
	// no attempt ran (SelectCode).
	Gate Gate
}

// SelectCode returns the code of a leg that ended without a response,
// taking the first match of 05 req 40:
//
//  1. no attempt ran and a gate refused the leg: the gate's code
//     (RZ-UP-005, RZ-UP-006, RZ-UP-008);
//  2. the leg or Route deadline expired, or the final attempt hit its
//     deadline: RZ-UP-003;
//  3. a retry ran and the last attempt ended in connect, tls or reset:
//     RZ-UP-007;
//  4. otherwise the last kind: RZ-UP-001 connect, RZ-UP-002 tls,
//     RZ-UP-004 reset.
//
// It returns "" when the leg returns a response. An outcome with neither a
// response nor an error kind (which the attempt loop never produces) is
// treated as a protocol error, RZ-UP-004.
func SelectCode(o Outcome) string {
	switch {
	case o.Responded:
		return ""
	case o.Attempts <= 0 && o.Gate != GateNone:
		return o.Gate.Code()
	case o.Expired || o.Kind == KindTimeout:
		return CodeTimeout
	case o.Attempts > 1 && (o.Kind == KindConnect || o.Kind == KindTLS || o.Kind == KindReset):
		return CodeRetriesFailed
	case o.Kind == KindConnect:
		return CodeConnect
	case o.Kind == KindTLS:
		return CodeTLS
	default:
		return CodeReset
	}
}

// Err returns the leg error of o: nil when the leg responded, else an
// *errcode.Error with SelectCode(o) whose cause is a *KindError naming the
// last attempt's kind (when it has one) around cause. A gate outcome wraps
// the gate's sentinel instead of cause.
func (o Outcome) Err(cause error) error {
	code := SelectCode(o)
	switch {
	case code == "":
		return nil
	case o.Attempts <= 0 && o.Gate != GateNone:
		return o.Gate.Err()
	case o.Kind != KindNone:
		cause = &KindError{Kind: o.Kind, Err: cause}
	case o.Expired:
		cause = &KindError{Kind: KindTimeout, Err: cause}
	}
	return errcode.Wrap(code, cause)
}

// ExpiryCode returns the code of a Route timeout expiry (05 req 26):
// RZ-RT-007 before any Upstream leg started, RZ-UP-003 during a leg and
// RZ-UP-009 after commit (the stream is reset; recorded in onLog).
func ExpiryCode(legStarted, committed bool) string {
	switch {
	case committed:
		return CodeAfterCommit
	case legStarted:
		return CodeTimeout
	default:
		return CodeRouteTimeout
	}
}
