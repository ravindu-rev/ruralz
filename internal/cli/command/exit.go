// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
)

// Exit codes. They never change once released.
const (
	// ExitOK is success with nothing to report.
	ExitOK = 0
	// ExitNegative is a negative result: an invalid Bundle, a diff with
	// changes.
	ExitNegative = 1
	// ExitNoResult is no result: a usage error, unreadable input, an admin
	// error, an unsupported platform.
	ExitNoResult = 2
	// ExitWaiting is waiting on a person; reserved for rollout in M2.
	ExitWaiting = 3
	// ExitInterrupted is an invocation ended by SIGINT, 128 plus its
	// number: Ctrl-C.
	ExitInterrupted = 130
	// ExitTerminated is an invocation ended by SIGTERM, 128 plus its
	// number: a process manager or kill stopping the command.
	ExitTerminated = 143
)

// signalExitBase is added to a signal's number for the exit code of an
// invocation the signal ended (docs/reference/01-cli-and-api-surface.md
// "Exit codes").
const signalExitBase = 128

// ExitError ends a command with a specific exit code. Execute prints Err
// as "<program> <path>: <message>" when it is not nil; a nil Err means the
// command already printed its result.
type ExitError struct {
	// Code is the process exit code.
	Code int
	// Err is the cause printed on stderr, or nil.
	Err error
}

// Error returns the cause's message, or "exit status <code>" without one.
func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

// Unwrap returns the cause.
func (e *ExitError) Unwrap() error { return e.Err }

// UsageError is a usage problem; Execute follows its message with
// "Run '<program> <path> --help' for usage.".
type UsageError struct {
	// Err is the problem.
	Err error
}

// Error returns the problem's message.
func (e *UsageError) Error() string { return e.Err.Error() }

// Unwrap returns the problem.
func (e *UsageError) Unwrap() error { return e.Err }

// Exit returns an ExitError with code and nothing more to print.
func Exit(code int) error { return &ExitError{Code: code} }

// Usagef returns a usage error with exit code 2 whose message is
// fmt.Errorf(format, args...).
func Usagef(format string, args ...any) error {
	return &ExitError{Code: ExitNoResult, Err: &UsageError{Err: fmt.Errorf(format, args...)}}
}

// NoResult returns err with exit code 2 (no result). A nil err prints
// nothing.
func NoResult(err error) error { return &ExitError{Code: ExitNoResult, Err: err} }

// NoResultf returns fmt.Errorf(format, args...) with exit code 2.
func NoResultf(format string, args ...any) error {
	return &ExitError{Code: ExitNoResult, Err: fmt.Errorf(format, args...)}
}

// Negative returns exit code 1 with nothing more to print: the command has
// already written its negative result.
func Negative() error { return &ExitError{Code: ExitNegative} }

// Interrupted is the cancellation cause SignalContext gives the context
// on the first SIGINT or SIGTERM.
type Interrupted struct {
	// Signal is the signal received.
	Signal os.Signal
}

// Error names the signal.
func (i Interrupted) Error() string { return "interrupted by " + signalName(i.Signal) }

// Code returns the exit code of an invocation the signal ended: 128 plus
// the signal's number, so ExitInterrupted for SIGINT and ExitTerminated
// for SIGTERM; a signal without a number, such as a nil one, counts as
// SIGINT.
func (i Interrupted) Code() int {
	if s, ok := i.Signal.(syscall.Signal); ok && s > 0 {
		return signalExitBase + int(s)
	}
	return ExitInterrupted
}

// isSignalCode reports whether code is the exit code of an invocation a
// signal ended: ExitInterrupted, ExitTerminated or another 128 plus a
// signal number.
func isSignalCode(code int) bool { return code > signalExitBase && code < signalExitBase+65 }

// signalName returns the conventional name of the two handled signals.
func signalName(s os.Signal) string {
	switch s {
	case nil:
		return "signal"
	case os.Interrupt:
		return "SIGINT"
	case syscall.SIGTERM:
		return "SIGTERM"
	default:
		return s.String()
	}
}

// ProblemDocument is implemented by errors that carry a server's RFC 9457
// problem document (the admin client's problem error). With --output json
// Execute prints the document verbatim to stdout besides the one-line
// summary on stderr.
type ProblemDocument interface {
	error
	// ProblemJSON returns the problem document exactly as received.
	ProblemJSON() []byte
}

// Code maps a command's result to the exit code: a context canceled with
// cause Interrupted exits by that signal whatever err is, 130 for SIGINT
// and 143 for SIGTERM; nil is 0; an ExitError anywhere in err's chain
// gives its code; an Interrupted in the chain exits by its signal;
// anything else is 2.
func Code(ctx context.Context, err error) int {
	if ctx != nil {
		if in, ok := interrupted(ctx); ok {
			return in.Code()
		}
	}
	if err == nil {
		return ExitOK
	}
	if ee, ok := errors.AsType[*ExitError](err); ok {
		return ee.Code
	}
	if in, ok := errors.AsType[Interrupted](err); ok {
		return in.Code()
	}
	return ExitNoResult
}

// interrupted returns the cause of ctx when SignalContext canceled it.
func interrupted(ctx context.Context) (Interrupted, bool) {
	if ctx.Err() == nil {
		return Interrupted{}, false
	}
	return errors.AsType[Interrupted](context.Cause(ctx))
}
