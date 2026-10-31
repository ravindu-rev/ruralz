// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// Environment variables the service manager sets (sd_notify(3),
// sd_watchdog_enabled(3)).
const (
	// EnvNotifySocket names the notification socket.
	EnvNotifySocket = "NOTIFY_SOCKET"
	// EnvWatchdogUsec is the watchdog interval in microseconds.
	EnvWatchdogUsec = "WATCHDOG_USEC"
	// EnvWatchdogPID is the process the watchdog interval is for.
	EnvWatchdogPID = "WATCHDOG_PID"
)

// MaxMessage bounds one notification datagram: systemd ignores a larger
// one (NOTIFY_BUFFER_MAX, PIPE_BUF = 4096 bytes, in src/core/manager.c), so
// a long STATUS= value is cut to fit.
const MaxMessage = 4096

// truncated ends a STATUS= value cut to fit MaxMessage.
const truncated = "…"

// ErrSocket reports a NOTIFY_SOCKET that is neither an absolute path nor
// an abstract socket name.
var ErrSocket = errors.New("sdnotify: NOTIFY_SOCKET is not an absolute path or an abstract socket name")

// Options configure a Notifier.
type Options struct {
	// Socket is the NOTIFY_SOCKET value: an absolute path or an abstract
	// socket name starting with "@"; empty disables every notification.
	Socket string
	// Held starts the Notifier held: a handover successor sends nothing
	// until MainPID, once it holds the data-directory lock.
	Held bool
	// Readiness returns the current /readyz state
	// (readiness.Tracker.Readyz), from which every STATUS= is formed; nil
	// sends no STATUS=.
	Readiness func() adminapi.Readyz
}

// gate says which messages a Notifier sends.
type gate uint8

const (
	// gateOpen sends every message.
	gateOpen gate = iota
	// gateHeld sends nothing until MainPID.
	gateHeld
	// gateMuted sends nothing, for good.
	gateMuted
)

// Notifier sends sd_notify messages to the service manager. Its methods
// are safe for concurrent use; they send one message at a time, in call
// order, each within its context. A send that fails returns the error and
// changes nothing, so the next SyncStatus sends the status again.
type Notifier struct {
	socket    string
	readiness func() adminapi.Readyz
	// dial connects to the socket (dialSocket).
	dial func(ctx context.Context, socket string) (net.Conn, error)

	mu     sync.Mutex
	gate   gate
	status string // the last STATUS= value the service manager received
	sent   bool   // whether a STATUS= value was received at all
}

// New returns a Notifier for the socket o names, held when o.Held.
func New(o Options) (*Notifier, error) {
	if o.Socket != "" && !strings.HasPrefix(o.Socket, "/") && !strings.HasPrefix(o.Socket, "@") {
		return nil, fmt.Errorf("%w: %q", ErrSocket, o.Socket)
	}
	n := &Notifier{socket: o.Socket, readiness: o.Readiness, dial: dialSocket}
	if o.Held {
		n.gate = gateHeld
	}
	return n, nil
}

// Enabled reports whether a service manager listens: NOTIFY_SOCKET was
// set.
func (n *Notifier) Enabled() bool { return n.socket != "" }

// Ready sends READY=1 with the current STATUS=. A process that took the
// data-directory lock at start sends it after its first boot attempt,
// ready or not, so a start never waits on usable configuration.
func (n *Notifier) Ready(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.gate != gateOpen {
		return nil
	}
	status, ok := n.currentLocked()
	return n.sendLocked(ctx, status, ok, "READY=1")
}

// MainPID sends MAINPID=pid, READY=1 and the current STATUS=, then opens a
// held Notifier: a handover successor that has taken the lock becomes the
// unit's main process, and its STATUS= replaces the predecessor's. A send
// that fails leaves the Notifier held, so the caller can retry. A muted
// Notifier sends nothing.
func (n *Notifier) MainPID(ctx context.Context, pid int) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.gate == gateMuted {
		return nil
	}
	status, ok := n.currentLocked()
	if err := n.sendLocked(ctx, status, ok, "MAINPID="+strconv.Itoa(pid), "READY=1"); err != nil {
		return err
	}
	n.gate = gateOpen
	return nil
}

// SyncStatus sends the current STATUS= when it differs from the last one
// the service manager received: STATUS= follows every readiness change.
func (n *Notifier) SyncStatus(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.gate != gateOpen {
		return nil
	}
	status, ok := n.currentLocked()
	if !ok || (n.sent && status == n.status) {
		return nil
	}
	return n.sendLocked(ctx, status, true)
}

// Stopping sends STOPPING=1 with STATUS=draining, at the start of a Drain
// a stop signal started; no other Drain sends it.
func (n *Notifier) Stopping(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.gate != gateOpen {
		return nil
	}
	return n.sendLocked(ctx, StatusDraining, true, "STOPPING=1")
}

// Watchdog sends WATCHDOG=1.
func (n *Notifier) Watchdog(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.gate != gateOpen {
		return nil
	}
	return n.sendLocked(ctx, "", false, "WATCHDOG=1")
}

// Mute stops every later message, for good: a process that handed over
// sends nothing from its Drain's start, since the unit keeps serving
// through the successor.
func (n *Notifier) Mute() {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.gate = gateMuted
}

// currentLocked returns the STATUS= value of the current readiness, or
// false without a readiness source.
func (n *Notifier) currentLocked() (string, bool) {
	if n.readiness == nil {
		return "", false
	}
	return StatusOf(n.readiness()), true
}

// sendLocked sends the assignments and, with withStatus, STATUS=status
// last, cut so the datagram fits MaxMessage; on success it records the
// status as received.
func (n *Notifier) sendLocked(ctx context.Context, status string, withStatus bool, assignments ...string) error {
	if n.socket == "" {
		return nil
	}
	msg := message(status, withStatus, assignments...)
	if err := n.send(ctx, msg); err != nil {
		return err
	}
	if withStatus {
		n.status, n.sent = status, true
	}
	return nil
}

// message joins the assignments and STATUS=status with newlines, cutting
// status at a rune boundary, with a trailing "…", so the message fits
// MaxMessage.
func message(status string, withStatus bool, assignments ...string) string {
	msg := strings.Join(assignments, "\n")
	if !withStatus {
		return msg
	}
	if msg != "" {
		msg += "\n"
	}
	msg += "STATUS="
	if len(msg)+len(status) <= MaxMessage {
		return msg + status
	}
	room := max(MaxMessage-len(msg)-len(truncated), 0)
	for room > 0 && !utf8.RuneStart(status[room]) {
		room--
	}
	return msg + status[:room] + truncated
}

// send writes msg as one datagram to the socket within ctx: the write
// deadline moves into the past when ctx ends, so a full queue in the
// service manager never blocks the caller beyond ctx.
func (n *Notifier) send(ctx context.Context, msg string) error {
	c, err := n.dial(ctx, n.socket)
	if err != nil {
		return fmt.Errorf("sdnotify: %w", err)
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.SetWriteDeadline(time.Unix(1, 0)) })
	defer stop()
	if _, err := c.Write([]byte(msg)); err != nil {
		return fmt.Errorf("sdnotify: %w", err)
	}
	return nil
}

// dialSocket connects a datagram socket to the notification socket.
func dialSocket(ctx context.Context, socket string) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unixgram", socket)
}
