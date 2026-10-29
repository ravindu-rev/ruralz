// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"os"
	"strings"
)

// Classify maps a request error to its Outcome: Timeout for ErrTimeout and
// deadlines, Canceled for a canceled context, Refused, Reset, TLSError,
// Closed for a connection closed early, Other for the rest; nil is
// Success. Errors that net/http does not expose as types, such as HTTP/2
// stream resets and lost connections, are matched by their text.
func Classify(err error) Outcome {
	switch {
	case err == nil:
		return Success
	case errors.Is(err, ErrTimeout), errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return Timeout
	case errors.Is(err, context.Canceled):
		return Canceled
	case isRefused(err):
		return Refused
	case isReset(err):
		return Reset
	case isTLS(err):
		return TLSError
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, net.ErrClosed):
		return Closed
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return Timeout
	}
	return classifyText(err.Error())
}

func isTLS(err error) bool {
	var (
		rh  tls.RecordHeaderError
		ae  tls.AlertError
		cv  *tls.CertificateVerificationError
		ua  x509.UnknownAuthorityError
		he  x509.HostnameError
		inv x509.CertificateInvalidError
	)
	return errors.As(err, &rh) || errors.As(err, &ae) || errors.As(err, &cv) ||
		errors.As(err, &ua) || errors.As(err, &he) || errors.As(err, &inv)
}

// classifyText matches the messages of failures net/http reports as plain
// errors.
func classifyText(msg string) Outcome {
	m := strings.ToLower(msg)
	switch {
	case strings.Contains(m, "connection refused"), strings.Contains(m, "actively refused"):
		return Refused
	case strings.Contains(m, "connection reset"), strings.Contains(m, "forcibly closed"),
		strings.Contains(m, "broken pipe"), strings.Contains(m, "stream error"),
		strings.Contains(m, "connection was aborted"):
		return Reset
	case strings.Contains(m, "tls:"), strings.Contains(m, "x509:"):
		return TLSError
	case strings.Contains(m, "timeout"), strings.Contains(m, "deadline exceeded"):
		return Timeout
	case strings.Contains(m, "eof"), strings.Contains(m, "server closed"),
		strings.Contains(m, "connection lost"), strings.Contains(m, "goaway"),
		strings.Contains(m, "use of closed network connection"):
		return Closed
	}
	return Other
}
