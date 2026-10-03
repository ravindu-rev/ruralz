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

// Exit codes (spec 10 req 18). They never change once released.
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
	// ExitInterrupted is an invocation ended by SIGINT or SIGTERM.
	ExitInterrupted = 130
)

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
// "Run '<program> <path> --help' for usage." (spec 10 req 7).
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
// on the first SIGINT or SIGTERM (spec 10 req 21).
type Interrupted struct {
	// Signal is the signal received.
	Signal os.Signal
}

// Error names the signal.
func (i Interrupted) Error() string { return "interrupted by " + signalName(i.Signal) }

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
// summary on stderr (spec 10 req 23).
type ProblemDocument interface {
	error
	// ProblemJSON returns the problem document exactly as received.
	ProblemJSON() []byte
}

// Code maps a command's result to the exit code (spec 10 reqs 18, 21): a
// context canceled with cause Interrupted is 130 whatever err is; nil is
// 0; an ExitError anywhere in err's chain gives its code; an Interrupted
// in the chain is 130; anything else is 2.
func Code(ctx context.Context, err error) int {
	if ctx != nil && interrupted(ctx) {
		return ExitInterrupted
	}
	if err == nil {
		return ExitOK
	}
	if ee, ok := errors.AsType[*ExitError](err); ok {
		return ee.Code
	}
	if _, ok := errors.AsType[Interrupted](err); ok {
		return ExitInterrupted
	}
	return ExitNoResult
}

// interrupted reports whether ctx was canceled by SignalContext.
func interrupted(ctx context.Context) bool {
	if ctx.Err() == nil {
		return false
	}
	_, ok := errors.AsType[Interrupted](context.Cause(ctx))
	return ok
}
