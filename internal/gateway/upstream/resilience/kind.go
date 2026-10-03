// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package resilience

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
)

// Kind is the error kind of one attempt (05 req 39): the error.kind CEL
// variable, the error label of ruralz_upstream_attempts_total and the input
// of code selection. The values follow the order of that label (none,
// connect, timeout, reset, tls), so int(k) is the attempt-error index of
// internal/telemetry/emit (ErrNone to ErrTLS).
type Kind uint8

// Error kinds. KindNone means the attempt got response headers.
const (
	KindNone Kind = iota
	KindConnect
	KindTimeout
	KindReset
	KindTLS
	// NumKinds is the number of kinds.
	NumKinds = int(KindTLS) + 1
)

// String returns the kind as CEL and metric labels spell it: "none",
// "connect", "timeout", "reset" or "tls".
func (k Kind) String() string {
	switch k {
	case KindConnect:
		return "connect"
	case KindTimeout:
		return "timeout"
	case KindReset:
		return "reset"
	case KindTLS:
		return "tls"
	case KindNone:
		return "none"
	default:
		return "none"
	}
}

// ParseKind returns the kind spelled s.
func ParseKind(s string) (Kind, bool) {
	for _, k := range Kinds() {
		if k.String() == s {
			return k, true
		}
	}
	return KindNone, false
}

// Kinds returns every kind in value order, which is the order of the error
// label values of ruralz_upstream_attempts_total (none, connect, timeout,
// reset, tls), so the label sets can exist at 0 from start (05 req 94).
func Kinds() []Kind {
	return []Kind{KindNone, KindConnect, KindTimeout, KindReset, KindTLS}
}

// Stage is how far an attempt got before its error, as the Upstream core
// learns it from net/http/httptrace (StageTrace): a failure during the dial
// is always connect and one during the TLS handshake always tls, unless an
// attempt, leg or Route context ended first.
type Stage uint8

// Attempt stages, in the order an attempt passes them.
const (
	// StageUnknown classifies by the error alone.
	StageUnknown Stage = iota
	// StageDial is before the connection was established (name resolution
	// included).
	StageDial
	// StageTLS is during the TLS handshake.
	StageTLS
	// StageExchange is on an established connection.
	StageExchange
)

// Classify returns the kind of an attempt error (05 req 39); ctx is the
// attempt context, which the leg and Route contexts bound. See
// ClassifyStage.
func Classify(ctx context.Context, err error) Kind {
	return ClassifyStage(ctx, err, StageUnknown)
}

// ClassifyStage returns the kind of an attempt error given how far the
// attempt got (05 req 39), in this order:
//
//   - nil: KindNone;
//   - ctx ended (attempt, leg or Route deadline, or cancellation): timeout.
//     Only these contexts yield timeout (05 req 24), so a dial timeout
//     stays connect and a handshake timeout tls;
//   - a dial error (*net.OpError with Op "dial": refused, unreachable, dial
//     timeout, the egress guard's refusal in Dialer.Control) or a
//     *net.DNSError of a static host: connect;
//   - a TLS error (handshake failure, certificate verification, a sent or
//     received alert, a non-TLS record, the net/http handshake timeout,
//     http.ErrSchemeMismatch): tls;
//   - any other error at StageDial: connect; at StageTLS: tls;
//   - otherwise reset: connection reset, EOF or unexpected EOF before
//     response headers, HTTP/2 stream reset or connection loss (ping
//     timeout), protocol errors.
//
// A context error in err without ctx having ended (a context of the
// caller's own) is timeout too. HTTP/2 stream and connection errors are
// unexported in net/http, so they are never matched by name: they fall to
// reset, as req 39 requires (05 section 9 item 20).
//
// A ctx canceled without a deadline expiry (the client went away, or a
// sibling composition step canceled the shared context) also reads
// timeout here, because req 39 has no other kind for it, but it is no
// Upstream failure: the Upstream core checks Canceled first and then skips
// failureWhen, passive ejection and the retry decision, and ends the leg
// with LegNotCounted.
func ClassifyStage(ctx context.Context, err error, stage Stage) Kind {
	if err == nil {
		return KindNone
	}
	if ctx != nil && ctx.Err() != nil {
		return KindTimeout
	}
	switch {
	case isDialError(err):
		return KindConnect
	case isTLSError(err):
		return KindTLS
	}
	switch stage {
	case StageDial:
		return KindConnect
	case StageTLS:
		return KindTLS
	case StageUnknown, StageExchange:
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return KindTimeout
	}
	return KindReset
}

// Expired reports whether ctx ended by a deadline expiry, the only ends
// that make error kind timeout an Upstream failure (05 reqs 24 and 39):
// the attempt or leg deadline of a Deadline (cause ErrAttemptTimeout or
// ErrLegTimeout) or a context deadline such as the Route timeout (error or
// cause context.DeadlineExceeded). It is false while ctx is live.
func Expired(ctx context.Context) bool {
	if ctx == nil || ctx.Err() == nil {
		return false
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return true
	}
	cause := context.Cause(ctx)
	return errors.Is(cause, ErrAttemptTimeout) || errors.Is(cause, ErrLegTimeout) || errors.Is(cause, context.DeadlineExceeded)
}

// Canceled reports whether ctx ended without a deadline expiry: the client
// went away, a sibling composition step canceled the shared context, or
// Deadline.Cancel ran. An attempt that ends this way is not counted against
// the Upstream (see ClassifyStage). It is false while ctx is live.
func Canceled(ctx context.Context) bool {
	return ctx != nil && ctx.Err() != nil && !Expired(ctx)
}

// isDialError reports a failure to establish the connection.
func isDialError(err error) bool {
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "dial" {
		return true
	}
	var dns *net.DNSError
	return errors.As(err, &dns)
}

// handshakeTimeoutText is the text of net/http's unexported TLS handshake
// timeout error (Transport.TLSHandshakeTimeout).
const handshakeTimeoutText = "TLS handshake timeout"

// isTLSError reports a TLS handshake, verification or alert error.
func isTLSError(err error) bool {
	var (
		rec  tls.RecordHeaderError
		alrt tls.AlertError
		ver  *tls.CertificateVerificationError
		ech  *tls.ECHRejectionError
		ua   x509.UnknownAuthorityError
		host x509.HostnameError
		inv  x509.CertificateInvalidError
		op   *net.OpError
	)
	switch {
	case errors.As(err, &rec), errors.As(err, &alrt), errors.As(err, &ver), errors.As(err, &ech),
		errors.As(err, &ua), errors.As(err, &host), errors.As(err, &inv),
		errors.Is(err, http.ErrSchemeMismatch):
		return true
	case errors.As(err, &op) && (op.Op == "remote error" || op.Op == "local error"):
		// crypto/tls reports received and sent alerts this way.
		return true
	}
	return anyInChain(err, 0, func(e error) bool {
		msg := e.Error()
		return strings.HasPrefix(msg, "tls: ") || strings.Contains(msg, handshakeTimeoutText)
	})
}

// maxChainDepth bounds the error-chain walk of anyInChain.
const maxChainDepth = 32

// anyInChain reports whether f holds for err or an error it wraps
// (single and multiple Unwrap), walking at most maxChainDepth levels.
func anyInChain(err error, depth int, f func(error) bool) bool {
	if err == nil || depth > maxChainDepth {
		return false
	}
	if f(err) {
		return true
	}
	switch u := err.(type) { //nolint:errorlint // walking the chain by hand, one level per call.
	case interface{ Unwrap() error }:
		return anyInChain(u.Unwrap(), depth+1, f)
	case interface{ Unwrap() []error }:
		for _, e := range u.Unwrap() {
			if anyInChain(e, depth+1, f) {
				return true
			}
		}
	}
	return false
}

// StageTrace records how far one attempt got, for ClassifyStage. The
// Upstream core adds ClientTrace's hooks to the attempt's request context
// (httptrace.WithClientTrace, merged with its own hooks) and reads Stage
// when RoundTrip fails. The stage only moves forward. A StageTrace may be
// pooled: Reset clears the stage and keeps the hooks. Its methods are safe
// for concurrent use (net/http calls the hooks from its dial goroutines).
type StageTrace struct {
	stage atomic.Uint32
	trace *httptrace.ClientTrace
}

// Stage returns the furthest stage reached.
func (s *StageTrace) Stage() Stage {
	return Stage(s.stage.Load()) //nolint:gosec // G115: only Stage values are stored.
}

// Reset returns the trace to StageUnknown for the next attempt.
func (s *StageTrace) Reset() { s.stage.Store(uint32(StageUnknown)) }

// advance moves the stage forward to st.
func (s *StageTrace) advance(st Stage) {
	for {
		cur := s.stage.Load()
		if cur >= uint32(st) || s.stage.CompareAndSwap(cur, uint32(st)) {
			return
		}
	}
}

// ClientTrace returns the hooks that advance s: name resolution and
// connect move it to StageDial, the TLS handshake to StageTLS and an
// obtained connection (new or pooled) to StageExchange. The hooks are
// built once per StageTrace; call ClientTrace from one goroutine before
// the attempt starts.
func (s *StageTrace) ClientTrace() *httptrace.ClientTrace {
	if s.trace == nil {
		s.trace = &httptrace.ClientTrace{
			DNSStart:          func(httptrace.DNSStartInfo) { s.advance(StageDial) },
			ConnectStart:      func(string, string) { s.advance(StageDial) },
			TLSHandshakeStart: func() { s.advance(StageTLS) },
			GotConn:           func(httptrace.GotConnInfo) { s.advance(StageExchange) },
		}
	}
	return s.trace
}

// KindError is an attempt or leg error annotated with its kind; the
// Upstream core puts the kind of RZ-UP-007 into the span, the log and the
// access log from it (05 req 40).
type KindError struct {
	// Kind is the error kind.
	Kind Kind
	// Err is the cause; it may be nil.
	Err error
}

// Error returns "upstream <kind> error: <cause>".
func (e *KindError) Error() string {
	if e.Err == nil {
		return "upstream " + e.Kind.String() + " error"
	}
	return "upstream " + e.Kind.String() + " error: " + e.Err.Error()
}

// Unwrap returns the cause.
func (e *KindError) Unwrap() error { return e.Err }

// KindOf returns the kind of the outermost KindError in err's chain, or
// KindNone.
func KindOf(err error) Kind {
	var ke *KindError
	if errors.As(err, &ke) {
		return ke.Kind
	}
	return KindNone
}
