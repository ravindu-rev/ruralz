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
	// CodeNewer is content from a newer producer: an unknown format,
	// member, kind or field, or a field above the served schema level.
	CodeNewer = "RZ-CFG-024"
	// CodeMismatch is content that does not hash to its digest or is not
	// in ruralz.canonical.v1 form.
	CodeMismatch = "RZ-CFG-027"
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
// carries CodeNewer or CodeMismatch: one diagnostic per problem, each
// with that code and, when known, its position in the content, resource
// and key-aware path. Err is an underlying cause, such as the jsonval scan
// error of invalid JSON, or nil.
type Findings struct {
	// Diagnostics holds at least one finding.
	Diagnostics diag.List
	// Err is the underlying cause, or nil.
	Err error
}

// Error returns the first finding's message, its resource and path, and
// how many more there are.
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
	if n := len(f.Diagnostics) - 1; n > 0 {
		b.WriteString(" (and ")
		b.WriteString(strconv.Itoa(n))
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
// which carries that code.
func fail(code string, cause error, ds ...diag.Diagnostic) error {
	return errcode.Wrap(code, &Findings{Diagnostics: ds, Err: cause})
}

// finding returns an error diagnostic of code.
func finding(code string, loc diag.Location, res *diag.ResourceID, path diag.Path, msg string) diag.Diagnostic {
	return diag.Diagnostic{Code: code, Severity: diag.SeverityError, Location: loc, Resource: res, Path: path, Message: msg}
}

// notCanonical is the message prefix of every form finding.
const notCanonical = "content is not in " + Format + " form"
