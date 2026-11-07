// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"errors"
	"fmt"
)

// RZ codes the Control Store raises (docs/architecture/04-control-plane-and-gitops.md
// "API summary").
const (
	// CodeNoLeader is no quorum, leader or lease: a write, a leader-only
	// operation or a leadership transfer that found no other caught-up
	// voter.
	CodeNoLeader = "RZ-CP-004"
	// CodeContentQuorum is a content object that did not reach a replica
	// quorum before the Raft entry that would reference it.
	CodeContentQuorum = "RZ-CP-013"
	// CodeBackup is a backup or restore that failed.
	CodeBackup = "RZ-CP-015"
	// CodeRemoveRefused is a voter removal that names the leader or would
	// leave no majority of the new voter set in contact with the leader.
	CodeRemoveRefused = "RZ-CP-019"
	// CodeFinalize is a refused finalize, or a status report from a binary
	// newer than the Control Store version allows.
	CodeFinalize = "RZ-CP-021"
)

// Error is a Control Store failure that carries a registered RZ-CP code.
// errors.Is matches two Errors with the same code, so a detailed error
// matches its sentinel (ErrNoLeader and the others).
type Error struct {
	code string
	msg  string
	err  error
	// leader is the current leader's peer address when a leader-only
	// operation reached a follower; empty when none is known.
	leader string
}

// Sentinel errors, one per code.
var (
	// ErrNoLeader is RZ-CP-004.
	ErrNoLeader = &Error{code: CodeNoLeader, msg: "no quorum or leader"}
	// ErrContentQuorum is RZ-CP-013.
	ErrContentQuorum = &Error{code: CodeContentQuorum, msg: "content quorum write failed"}
	// ErrBackup is RZ-CP-015.
	ErrBackup = &Error{code: CodeBackup, msg: "backup or restore failed"}
	// ErrRemoveRefused is RZ-CP-019.
	ErrRemoveRefused = &Error{code: CodeRemoveRefused, msg: "voter removal refused"}
	// ErrFinalize is RZ-CP-021.
	ErrFinalize = &Error{code: CodeFinalize, msg: "finalize refused"}
)

// Uncoded errors: programming errors or replica states that never reach a
// client as an RZ code.
var (
	// ErrHalted reports a replica that met a Control Store version above
	// its binary's, an unknown entry type, a skipped minor or a rejected
	// status report, and stopped before serving
	// (docs/engineering/04-release-versioning-and-compatibility.md
	// "Ruralz Control upgrades" rule 4).
	ErrHalted = errors.New("controlstore: replica halted")
	// ErrClosed reports a call on a store that was shut down.
	ErrClosed = errors.New("controlstore: store closed")
	// ErrEntryTooLarge reports a proposal whose encoded entry exceeds
	// MaxEntryBytes; large data goes to the content store instead.
	ErrEntryTooLarge = errors.New("controlstore: entry exceeds 1 MiB")
	// ErrRejected reports a committed entry the state machine rejected;
	// the Rejection says why. A fence rejection carries the stream's
	// current fence, so the leader re-reads and re-signs at the next
	// sequence (ADR-0006 "Consensus").
	ErrRejected = errors.New("controlstore: entry rejected")
	// ErrNotFound reports an absent record, Revision or content object.
	ErrNotFound = errors.New("controlstore: not found")
	// ErrInvalid reports a malformed proposal or object, refused before
	// it is proposed.
	ErrInvalid = errors.New("controlstore: invalid")
)

// Errorf returns an Error with code's meaning, a detail and an optional
// wrapped cause (the last %w argument).
func Errorf(sentinel *Error, format string, args ...any) *Error {
	e := fmt.Errorf(format, args...)
	return &Error{code: sentinel.code, msg: e.Error(), err: errors.Unwrap(e)}
}

// NotLeader returns RZ-CP-004 for a leader-only operation that reached a
// follower, naming the leader's peer address when one is known, so the
// caller forwards it there.
func NotLeader(leader string) *Error {
	msg := "not the leader"
	if leader == "" {
		msg = "no leader"
	}
	return &Error{code: CodeNoLeader, msg: msg, leader: leader}
}

// Error returns "<code>: <message>".
func (e *Error) Error() string {
	if e.err != nil && e.msg == "" {
		return e.code + ": " + e.err.Error()
	}
	return e.code + ": " + e.msg
}

// Code returns the registered RZ-CP code.
func (e *Error) Code() string { return e.code }

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.err }

// Leader returns the leader's peer address of a not-the-leader error, or
// "".
func (e *Error) Leader() string { return e.leader }

// Is matches any Error with the same code.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return t.code == e.code
}

// CodeOf returns the RZ code an error carries, or "".
func CodeOf(err error) string {
	var e *Error
	if errors.As(err, &e) {
		return e.code
	}
	return ""
}
