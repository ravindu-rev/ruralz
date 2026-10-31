// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handover

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
)

// Handover timeline, target values of
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md "In-place
// handover" and "Handover protocol".
const (
	// GateTimeout bounds the gate from the new process's start: one not
	// accepted in time exits 1.
	GateTimeout = 60 * time.Second
	// RetryInterval is how often a new process retries while the holder's
	// socket is missing or unanswered, checking first whether the holder
	// exited, and how often a lock or MAINPID= attempt is retried.
	RetryInterval = time.Second
	// ListeningTimeout is how long the holder waits for listening after
	// accepted before it aborts the handover.
	ListeningTimeout = 10 * time.Second
	// Linger is how long the holder keeps accepting after it steered every
	// group to the new process: handshakes begun before the steering and
	// one SYN-ACK retransmission still land.
	Linger = listener.Linger
	// UsageInterval is how often the holder reports its usage after
	// accepted.
	UsageInterval = time.Second
)

// maxConns bounds the connections the holder's socket serves at once: the
// handover in progress and one connection being refused, which a new
// process makes right before it sends ready. A connection beyond it is
// closed at once; its new process retries after RetryInterval.
const maxConns = 2

// Errors.
var (
	// ErrConfig reports an unusable configuration.
	ErrConfig = errors.New("handover: invalid configuration")
	// ErrNotHeld reports a holder started while the process does not hold
	// the data-directory lock.
	ErrNotHeld = errors.New("handover: the process does not hold the data-directory lock")
	// ErrReleased reports a holder started after its Drain released the
	// lock.
	ErrReleased = errors.New("handover: the data-directory lock was released")
	// ErrSocketPath reports a data directory whose handover.sock path is
	// longer than a Unix socket address holds.
	ErrSocketPath = errors.New("handover: socket path too long for a Unix socket")
	// ErrPeerUser reports a peer that runs as another user than this
	// process's effective user (SO_PEERCRED).
	ErrPeerUser = errors.New("handover: peer runs as another user")
	// ErrRefused reports a refused handover; the error is a *RefusedError
	// naming the reason.
	ErrRefused = errors.New("handover: refused")
	// ErrTimeout reports a new process not accepted within GateTimeout.
	ErrTimeout = errors.New("handover: not accepted within the gate")
	// ErrLost reports a handover whose holder dropped the connection
	// after accepted and kept the lock.
	ErrLost = errors.New("handover: the holder kept the lock after the connection ended")
)

// RefusedError is a refused message with its reason.
type RefusedError struct {
	// Reason is the refused message's reason, such as ReasonBusy.
	Reason string
}

// Error returns the reason.
func (e *RefusedError) Error() string { return fmt.Sprintf("handover: refused: %s", e.Reason) }

// Unwrap returns ErrRefused, which every refusal matches.
func (e *RefusedError) Unwrap() error { return ErrRefused }

// DrainStart is the process's Drain as a new process watches it
// (drain.Drainer).
type DrainStart interface {
	// Started returns a channel closed when the Drain starts.
	Started() <-chan struct{}
}

// Drainer is the holder's Drain as the handover drives it (drain.Drainer).
type Drainer interface {
	DrainStart
	// Hold defers stop signals and failures from accepted until the
	// linger's end; false, holding nothing, once a Drain started.
	Hold() bool
	// Handover starts the handover's Drain at the linger's end and ends
	// the Hold.
	Handover()
	// Resume ends the Hold without a handover, starting a Drain a stop
	// signal or a failure asked for meanwhile.
	Resume()
}

// Revisions are the holder's Revisions a ready message is decided
// against.
type Revisions struct {
	// Active is the active Revision; zero when none is active.
	Active revision.Digest
	// Candidate is the Revision of the candidate the holder last wrote
	// under lkg/; zero when it wrote none.
	Candidate revision.Digest
}

// Activations is the holder's activation loader (internal/gateway/reload)
// as the handover holds it: from accepted until the holder's Drain the
// holder activates nothing, so a source change waits for the new process.
type Activations interface {
	// Hold waits until no activation runs, its candidate write included,
	// then holds every later activation off until Release, and returns
	// the Revisions. It returns ctx's error, holding nothing, when ctx
	// ends first.
	Hold(ctx context.Context) (Revisions, error)
	// Release ends a Hold: activations run again, a pending one included.
	Release()
}

// Pinner pins a process's reuseport groups to its own sockets
// (listener.Set, listener.AdminPort).
type Pinner interface {
	// Pin pins every group to the process's own socket; off Linux it
	// returns errors.ErrUnsupported.
	Pin() error
}

// Sockets are listening sockets of the holder that the handover steers:
// the client listeners (listener.Set) and the admin port
// (listener.AdminPort).
type Sockets interface {
	Pinner
	// WaitLingers waits until no socket a Hot Reload replaced lingers.
	WaitLingers(ctx context.Context) error
	// SteerToSuccessor steers every group to the socket the new process
	// added; off Linux it returns errors.ErrUnsupported.
	SteerToSuccessor() error
	// StopAccepting closes the sockets; their servers keep serving the
	// connections they hold.
	StopAccepting()
}

// Admin is the holder's admin server (admin.Server): during the linger it
// moves keep-alive health checkers to the new process.
type Admin interface {
	// DisableKeepAlives closes idle admin connections and answers later
	// requests with Connection: close.
	DisableKeepAlives()
	// EnableKeepAlives undoes DisableKeepAlives.
	EnableKeepAlives()
}

// Bound are the sockets a new process bound before the gate: the client
// listeners' pending Plan and the admin port, bound with SO_REUSEPORT and
// not listening.
type Bound interface {
	// Listen makes every bound socket listen and serve, still pinned to
	// the holder: the Plan's Commit, the admin port's Listen and its
	// server's Serve.
	Listen(ctx context.Context) error
	// Close closes every socket, bound or listening, when the new process
	// exits without serving.
	Close() error
}

// Notifier tells the service manager the process's new main PID
// (sdnotify.Notifier): MAINPID=, READY=1 and STATUS=.
type Notifier interface {
	// MainPID sends MAINPID=pid with READY=1 and the current STATUS=, and
	// opens the held Notifier; a failed send leaves it held.
	MainPID(ctx context.Context, pid int) error
}

// Ceiling is a Node-wide ceiling that the other process's usage lowers
// (admission.ConnLimiter, admission.Units, body.Budget).
type Ceiling interface {
	// SetExternal lowers the ceiling by n; 0 restores it.
	SetExternal(n int64)
}

// Outcome is how a new process's handover ended.
type Outcome uint8

// Outcomes.
const (
	// OutcomeHolder: the process took the data-directory lock, pinned its
	// groups, wrote holder.json and sent MAINPID=; it serves as the lock
	// holder. In file mode it now reads RURALZ_CONFIG and activates it
	// when its digest differs from the active one.
	OutcomeHolder Outcome = iota + 1
	// OutcomeRefused: refused, not accepted within the gate, or lost
	// before its listening reached the holder. The process closed its
	// sockets and exits 1 (setting.ExitRuntime) holding no listening
	// socket; the holder keeps serving.
	OutcomeRefused
	// OutcomeLost: the holder dropped the connection after the new
	// process's listening and kept the lock. Connections may have been
	// steered to the new process, so it Drains on the failure timeline
	// (drain.Drainer.Fail) and exits 1.
	OutcomeLost
	// OutcomeStopped: the process's Drain started first, on a stop signal
	// or a failed part, and decides the exit; a process not yet listening
	// closed its sockets, so its Drain skips to the flush.
	OutcomeStopped
)

// String names the outcome in logs.
func (o Outcome) String() string {
	switch o {
	case OutcomeHolder:
		return "holder"
	case OutcomeRefused:
		return "refused"
	case OutcomeLost:
		return "lost"
	case OutcomeStopped:
		return "stopped"
	default:
		return "none"
	}
}

// checkSocketPath refuses a handover.sock path a Unix socket address
// cannot hold.
func checkSocketPath(path string) error {
	if len(path) > maxSocketPath {
		return fmt.Errorf("%w: %s is %d bytes, over %d", ErrSocketPath, path, len(path), maxSocketPath)
	}
	return nil
}

// isUnsupported reports an error of a platform without steering.
func isUnsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }
