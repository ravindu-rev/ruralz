// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"errors"
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Format is the format identifier of the serialization a Revision digest
// covers. A change to the serialization needs a new identifier.
const Format = "ruralz.canonical.v1"

// Codes raised by Decode, Verify and ReadDump, each registered in
// internal/errcode.
const (
	// CodeNewer is content from a newer producer: a format other than
	// ruralz.canonical.v1, an unknown member, kind or field, or a field
	// above the served schema level.
	CodeNewer = "RZ-CFG-024"
	// CodeMismatch is content that does not hash to its digest or is not
	// in ruralz.canonical.v1 form.
	CodeMismatch = "RZ-CFG-027"
	// CodeLimit is content past a limit of the decoder: nesting deeper
	// than DecoderOptions.MaxDepth, more resources than MaxResources or
	// more values than MaxValues, the size and depth limits of
	// RZ-CFG-001. It is also the code of the CodeTooMany diagnostic.
	CodeLimit = "RZ-CFG-001"
)

// Sentinel errors without an RZ code: an encoder input the pipeline should
// never produce (an internal error) and a /config/dump body that is not a
// dump document.
var (
	// ErrEncode reports a tree that has no ruralz.canonical.v1 form: a
	// null, invalid UTF-8, a number outside the RFC 8259 grammar or the
	// double range, duplicate member names, a kind that never appears in
	// the format, or an unsupported schema level.
	ErrEncode = errors.New("canonical: cannot encode")
	// ErrNotDump reports a body that is not a /config/dump document: not
	// a JSON object, or a content, digest, revision or lastKnownGood member
	// missing or malformed.
	ErrNotDump = errors.New("canonical: not a /config/dump document")
	// ErrDumpTooLarge reports a /config/dump body over the reader's limit.
	ErrDumpTooLarge = errors.New("canonical: /config/dump document over its limit")
	// ErrNoIndex reports a Decoder built without a schema index.
	ErrNoIndex = errors.New("canonical: decoder has no schema index")
)

// Findings is the cause of a Decode, Verify or ReadDump error that
// carries CodeNewer, CodeMismatch or CodeLimit: one diagnostic per
// problem, each with that code and, when known, its position in the
// content, resource and key-aware path, in content order. A CodeLimit
// error has one finding, at the point the content passes the limit. A
// decode reports at most DecoderOptions.MaxDiagnostics findings: the
// first MaxDiagnostics of the code it fails with, in content order, and,
// when it met more, Truncated is set and the last diagnostic is one
// CodeTooMany (RZ-CFG-001) "too many diagnostics; stopped after N"
// without a position, the only diagnostic with another code. Newer
// findings past the cap stop the parse there; form findings past it are
// dropped while the parse goes on looking for a newer finding (see the
// package documentation). Err is an underlying cause, such as the jsonval
// scan error of invalid JSON or of nesting too deep, or nil.
type Findings struct {
	// Diagnostics holds at least one finding.
	Diagnostics diag.List
	// Truncated reports that the decode met more findings than its cap.
	Truncated bool
	// Err is the underlying cause, or nil.
	Err error
}

// Error returns the first finding's message, its resource and path, how
// many more there are and, when the decode stopped at its cap, that it
// did.
func (f *Findings) Error() string {
	if len(f.Diagnostics) == 0 {
		if f.Err != nil {
			return f.Err.Error()
		}
		return "canonical: no findings"
	}
	d := f.Diagnostics[0]
	var b strings.Builder
	if d.Resource != nil {
		b.WriteString(d.Resource.Kind)
		b.WriteByte('/')
		b.WriteString(d.Resource.Name)
		b.WriteByte(' ')
	}
	if len(d.Path) > 0 {
		b.WriteString(d.Path.String())
		b.WriteString(": ")
	}
	b.WriteString(d.Message)
	more := len(f.Diagnostics) - 1
	if f.Truncated {
		more--
	}
	switch {
	case f.Truncated:
		b.WriteString(" (and ")
		b.WriteString(strconv.Itoa(more))
		b.WriteString(" more; too many diagnostics, stopped after ")
		b.WriteString(strconv.Itoa(more + 1))
		b.WriteByte(')')
	case more > 0:
		b.WriteString(" (and ")
		b.WriteString(strconv.Itoa(more))
		b.WriteString(" more)")
	}
	return b.String()
}

// Unwrap returns the underlying cause.
func (f *Findings) Unwrap() error { return f.Err }

// DiagnosticsOf returns the diagnostics of a Decode, Verify or ReadDump
// error: its Findings when it has them, otherwise one diagnostic with the
// error's RZ code and message (nil for an error without a code).
func DiagnosticsOf(err error) diag.List {
	var f *Findings
	if errors.As(err, &f) && len(f.Diagnostics) > 0 {
		return f.Diagnostics
	}
	code, ok := errcode.CodeOf(err)
	if !ok {
		return nil
	}
	return diag.List{{Code: code, Severity: diag.SeverityError, Message: err.Error()}}
}

// fail returns the errcode error for findings of code, every one of
// which carries that code, in content order.
func fail(code string, cause error, ds ...diag.Diagnostic) error {
	return errcode.Wrap(code, &Findings{Diagnostics: ds, Err: cause})
}

// finding returns an error diagnostic of code.
func finding(code string, loc diag.Location, res *diag.ResourceID, path diag.Path, msg string) diag.Diagnostic {
	return diag.Diagnostic{Code: code, Severity: diag.SeverityError, Location: loc, Resource: res, Path: path, Message: msg}
}

// notCanonical is the message prefix of every form finding.
const notCanonical = "content is not in " + Format + " form"
