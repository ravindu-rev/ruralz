// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/problem"
)

// ErrCleartext is the refusal to send an admin token over http to a peer
// that is not a loopback address; nothing was sent to it.
var ErrCleartext = errors.New("refusing to send the admin token in cleartext")

// ErrStreamEnded reports a /tap stream that the Node ended, as at a Drain,
// a shutdown, an admin port move or a missed write deadline: an end of
// stream, a reset or a truncated last line.
var ErrStreamEnded = errors.New("the Node ended the /tap stream")

// DeadlineKind names the deadline a call missed.
type DeadlineKind string

// Deadline kinds.
const (
	// DeadlineConnect is ConnectTimeout: connecting and finishing TLS.
	DeadlineConnect DeadlineKind = "connect"
	// DeadlineHeaders is HeaderTimeout: the established connection to the
	// response headers.
	DeadlineHeaders DeadlineKind = "headers"
	// DeadlineCall is the whole call that does not stream: DefaultTimeout
	// or --timeout.
	DeadlineCall DeadlineKind = "call"
)

// DeadlineError is a call that missed a deadline; it exits 2.
type DeadlineError struct {
	// Kind is the deadline missed.
	Kind DeadlineKind
	// Op is "connect to <host:port>" or "GET <url>".
	Op string
	// Limit is the deadline's length.
	Limit time.Duration
	// Flag names what set a call deadline, such as --timeout.
	Flag string
	// TLS marks a connection deadline that covered a TLS handshake.
	TLS bool
}

// Error names the operation and the deadline it missed.
func (e *DeadlineError) Error() string {
	d := command.FormatDuration(e.Limit)
	switch e.Kind {
	case DeadlineConnect:
		if e.TLS {
			return fmt.Sprintf("%s: no TLS connection within %s", e.Op, d)
		}
		return fmt.Sprintf("%s: no connection within %s", e.Op, d)
	case DeadlineHeaders:
		return fmt.Sprintf("%s: no response headers within %s", e.Op, d)
	default:
		return fmt.Sprintf("%s: not complete within %s (%s)", e.Op, d, e.Flag)
	}
}

// ProblemError is an RFC 9457 problem document a Node answered with; it
// exits 2. Its message is the one line of
// docs/reference/01-cli-and-api-surface.md "Admin API conventions",
// "<status> <code> <title>[: <detail>] (requestId <id>)", and ProblemJSON
// returns the document exactly as received, which command.Execute writes to
// standard output under --output json.
type ProblemError struct {
	// Status is the HTTP status of the answer.
	Status int
	// Doc is the decoded document; unknown members are ignored.
	Doc problem.Document
	// raw is the document as received.
	raw []byte
}

var _ command.ProblemDocument = (*ProblemError)(nil)

// Error returns the problem's one line. A missing code, detail or
// requestId is left out and a missing title is the status text; control
// characters print as U+FFFD, so a document cannot break the line or drive
// a terminal.
func (e *ProblemError) Error() string {
	var b strings.Builder
	b.WriteString(strconv.Itoa(e.Status))
	if e.Doc.Code != "" {
		b.WriteString(" " + clean(e.Doc.Code))
	}
	title := e.Doc.Title
	if title == "" {
		title = http.StatusText(e.Status)
	}
	if title != "" {
		b.WriteString(" " + clean(title))
	}
	if e.Doc.Detail != "" {
		b.WriteString(": " + clean(e.Doc.Detail))
	}
	if e.Doc.RequestID != "" {
		b.WriteString(" (requestId " + clean(e.Doc.RequestID) + ")")
	}
	return b.String()
}

// ProblemJSON returns the problem document exactly as received.
func (e *ProblemError) ProblemJSON() []byte { return e.raw }

// RedirectError is a 3xx answer, which the client never follows; it exits
// 2.
type RedirectError struct {
	// Op is "GET <url>".
	Op string
	// Status is the HTTP status.
	Status int
	// Location is the answer's Location, empty when it had none.
	Location string
}

// Error names the redirect and its Location.
func (e *RedirectError) Error() string {
	if e.Location == "" {
		return fmt.Sprintf("%s: %s without a Location; redirects are not followed", e.Op, statusText(e.Status))
	}
	return fmt.Sprintf("%s: %s with Location %s; redirects are not followed", e.Op, statusText(e.Status), clean(e.Location))
}

// StatusError is an answer that carries neither the endpoint's body nor a
// valid problem document, or a problem document over MaxProblemBytes; it
// exits 2.
type StatusError struct {
	// Op is "GET <url>".
	Op string
	// Status is the HTTP status.
	Status int
	// ContentType is the answer's Content-Type, empty when it had none.
	ContentType string
	// Reason says what is wrong beyond the status, empty for an
	// unexpected status alone.
	Reason string
}

// Error names the status, the content type and the reason.
func (e *StatusError) Error() string {
	s := fmt.Sprintf("%s: unexpected %s", e.Op, statusText(e.Status))
	if e.ContentType != "" {
		s += " (" + clean(e.ContentType) + ")"
	}
	if e.Reason != "" {
		s += ": " + e.Reason
	}
	return s
}

// statusText is "<code> <text>", or the code alone for an unknown status.
func statusText(code int) string {
	if t := http.StatusText(code); t != "" {
		return strconv.Itoa(code) + " " + t
	}
	return strconv.Itoa(code)
}

// clean keeps text a server chose on one printable line: invalid UTF-8
// and control characters become U+FFFD.
func clean(s string) string {
	s = strings.ToValidUTF8(s, "�")
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return '�'
		}
		return r
	}, s)
}

// size formats a byte bound: 256 MiB, 64 KiB, or a byte count.
func size(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n>>20, 10) + " MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n>>10, 10) + " KiB"
	default:
		return strconv.FormatInt(n, 10) + " bytes"
	}
}
