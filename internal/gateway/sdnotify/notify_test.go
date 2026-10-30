// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

const rev = "rev-162af81f5de4"

// openNotifier returns an open Notifier on s whose readiness the test
// sets.
func openNotifier(t *testing.T, s *sink, held bool) (*Notifier, *readiness) {
	t.Helper()
	r := &readiness{v: notReady(adminapi.ReadyReason{Reason: adminapi.ReasonNoRevision})}
	n, err := New(Options{Socket: s.path, Held: held, Readiness: r.get})
	if err != nil {
		t.Fatal(err)
	}
	return n, r
}

// TestNewSocketForms checks NOTIFY_SOCKET: an absolute path or an abstract
// name starting with "@" is used, empty disables the Notifier, and any
// other form is ErrSocket.
func TestNewSocketForms(t *testing.T) {
	for _, tt := range []struct {
		socket  string
		enabled bool
		err     error
	}{
		{socket: "/run/systemd/notify", enabled: true},
		{socket: "@/org/freedesktop/systemd1/notify", enabled: true},
		{socket: ""},
		{socket: "run/notify", err: ErrSocket},
		{socket: "vsock:2:1234", err: ErrSocket},
	} {
		n, err := New(Options{Socket: tt.socket})
		if !errors.Is(err, tt.err) {
			t.Errorf("New(%q) error = %v, want %v", tt.socket, err, tt.err)
			continue
		}
		if err == nil && n.Enabled() != tt.enabled {
			t.Errorf("New(%q).Enabled() = %t, want %t", tt.socket, n.Enabled(), tt.enabled)
		}
	}
}

// TestDisabledNotifierSendsNothing checks that without NOTIFY_SOCKET every
// message is a no-op that succeeds.
func TestDisabledNotifierSendsNothing(t *testing.T) {
	n, err := New(Options{Readiness: func() adminapi.Readyz { return readyAt(rev) }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	for name, f := range map[string]func() error{
		"Ready":      func() error { return n.Ready(ctx) },
		"MainPID":    func() error { return n.MainPID(ctx, 42) },
		"SyncStatus": func() error { return n.SyncStatus(ctx) },
		"Stopping":   func() error { return n.Stopping(ctx) },
		"Watchdog":   func() error { return n.Watchdog(ctx) },
	} {
		if err := f(); err != nil {
			t.Errorf("%s without a socket = %v, want nil", name, err)
		}
	}
}

// TestReadySendsReadyWithStatus checks READY=1 after the first boot
// attempt, ready or not, with STATUS= naming the readiness reasons.
func TestReadySendsReadyWithStatus(t *testing.T) {
	s := newSink(t)
	n, r := openNotifier(t, s, false)
	r.set(notReady(
		adminapi.ReadyReason{Reason: adminapi.ReasonNoRevision},
		adminapi.ReadyReason{Reason: adminapi.ReasonListenersUnbound, Code: "RZ-CFG-039", Detail: "web"},
	))
	if err := n.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := "READY=1\nSTATUS=not ready: no_revision; listeners_unbound RZ-CFG-039 web"
	if got := s.next(t); got != want {
		t.Errorf("Ready sent %q, want %q", got, want)
	}
}

// TestReadyWithoutReadinessSource checks that a Notifier without a
// readiness source sends READY=1 alone.
func TestReadyWithoutReadinessSource(t *testing.T) {
	s := newSink(t)
	n, err := New(Options{Socket: s.path})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Ready(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "READY=1" {
		t.Errorf("Ready sent %q, want READY=1", got)
	}
	if err := n.SyncStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.nothingSince(t)
}

// TestSyncStatusFollowsReadinessChanges checks that STATUS= follows every
// readiness change and is not repeated while readiness stays the same,
// READY=1's own STATUS= included.
func TestSyncStatusFollowsReadinessChanges(t *testing.T) {
	s := newSink(t)
	n, r := openNotifier(t, s, false)
	ctx := t.Context()
	if err := n.Ready(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "READY=1\nSTATUS=not ready: no_revision" {
		t.Fatalf("Ready sent %q", got)
	}
	if err := n.SyncStatus(ctx); err != nil {
		t.Fatal(err)
	}
	s.nothingSince(t)

	r.set(readyAt(rev))
	if err := n.SyncStatus(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "STATUS=ready "+rev {
		t.Errorf("after the Revision activated: %q, want STATUS=ready %s", got, rev)
	}
	if err := n.SyncStatus(ctx); err != nil {
		t.Fatal(err)
	}
	s.nothingSince(t)
}

// TestStoppingSendsDraining checks the start of a Drain a stop signal
// started: STOPPING=1 with STATUS=draining, which a later SyncStatus of
// the draining readiness does not repeat.
func TestStoppingSendsDraining(t *testing.T) {
	s := newSink(t)
	n, r := openNotifier(t, s, false)
	if err := n.Stopping(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "STOPPING=1\nSTATUS=draining" {
		t.Errorf("Stopping sent %q, want STOPPING=1 and STATUS=draining", got)
	}
	r.set(notReady(adminapi.ReadyReason{Reason: adminapi.ReasonDraining}))
	if err := n.SyncStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.nothingSince(t)
}

// TestWatchdogSendsPing checks WATCHDOG=1.
func TestWatchdogSendsPing(t *testing.T) {
	s := newSink(t)
	n, _ := openNotifier(t, s, false)
	if err := n.Watchdog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "WATCHDOG=1" {
		t.Errorf("Watchdog sent %q, want WATCHDOG=1", got)
	}
}

// TestHeldNotifierWaitsForMainPID checks a handover successor: it sends
// nothing, not even READY=1, STOPPING=1 or a watchdog ping, until it takes
// the lock; MainPID then sends MAINPID=, READY=1 and its STATUS=, and the
// Notifier is open.
func TestHeldNotifierWaitsForMainPID(t *testing.T) {
	s := newSink(t)
	n, r := openNotifier(t, s, true)
	r.set(readyAt(rev))
	ctx := t.Context()
	for _, f := range []func(context.Context) error{n.Ready, n.SyncStatus, n.Stopping, n.Watchdog} {
		if err := f(ctx); err != nil {
			t.Fatal(err)
		}
	}
	s.nothingSince(t)

	if err := n.MainPID(ctx, 4242); err != nil {
		t.Fatal(err)
	}
	if got, want := s.next(t), "MAINPID=4242\nREADY=1\nSTATUS=ready "+rev; got != want {
		t.Errorf("MainPID sent %q, want %q", got, want)
	}
	if err := n.Watchdog(ctx); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "WATCHDOG=1" {
		t.Errorf("after MainPID: %q, want WATCHDOG=1", got)
	}
}

// TestFailedMainPIDKeepsHeld checks that a MAINPID= the service manager
// did not receive leaves the successor's Notifier held: nothing else is
// sent until a retried MainPID succeeds.
func TestFailedMainPIDKeepsHeld(t *testing.T) {
	path := filepath.Join(socketDir(t), "notify")
	r := &readiness{v: readyAt(rev)}
	n, err := New(Options{Socket: path, Held: true, Readiness: r.get})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.MainPID(t.Context(), 31); err == nil {
		t.Fatal("MainPID with no socket listening = nil, want an error")
	}
	s := listenSink(t, path)
	if err := n.Watchdog(t.Context()); err != nil {
		t.Fatal(err)
	}
	s.nothingSince(t)
	if err := n.MainPID(t.Context(), 31); err != nil {
		t.Fatal(err)
	}
	if got, want := s.next(t), "MAINPID=31\nREADY=1\nSTATUS=ready "+rev; got != want {
		t.Errorf("retried MainPID sent %q, want %q", got, want)
	}
}

// TestMutedNotifierSendsNothing checks a process that handed over: once
// muted, held or open, it sends no message at all, MainPID included.
func TestMutedNotifierSendsNothing(t *testing.T) {
	for _, held := range []bool{false, true} {
		s := newSink(t)
		n, _ := openNotifier(t, s, held)
		n.Mute()
		ctx := t.Context()
		for _, f := range []func(context.Context) error{n.Ready, n.SyncStatus, n.Stopping, n.Watchdog} {
			if err := f(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if err := n.MainPID(ctx, 7); err != nil {
			t.Fatal(err)
		}
		s.nothingSince(t)
	}
}

// TestAbstractSocket checks a NOTIFY_SOCKET naming an abstract socket
// ("@" for the leading NUL byte), as systemd may set on Linux.
func TestAbstractSocket(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("abstract sockets are Linux only")
	}
	s := listenSink(t, "@"+filepath.Join(socketDir(t), "abstract"))
	n, _ := openNotifier(t, s, false)
	if err := n.Watchdog(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "WATCHDOG=1" {
		t.Errorf("abstract socket got %q, want WATCHDOG=1", got)
	}
}

// TestFailedSendIsRetried checks that a STATUS= the service manager did
// not receive is sent again by the next SyncStatus.
func TestFailedSendIsRetried(t *testing.T) {
	path := filepath.Join(socketDir(t), "notify")
	r := &readiness{v: readyAt(rev)}
	n, err := New(Options{Socket: path, Readiness: r.get})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.SyncStatus(t.Context()); err == nil {
		t.Fatal("SyncStatus with no socket listening = nil, want an error")
	}
	s := listenSink(t, path)
	if err := n.SyncStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "STATUS=ready "+rev {
		t.Errorf("after the failed send: %q, want the status again", got)
	}
}

// TestSendWithEndedContextFails checks that a send whose context already
// ended fails at once and records nothing.
func TestSendWithEndedContextFails(t *testing.T) {
	s := newSink(t)
	n, _ := openNotifier(t, s, false)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := n.SyncStatus(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("SyncStatus with an ended context = %v, want context.Canceled", err)
	}
	if err := n.SyncStatus(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "STATUS=not ready: no_revision" {
		t.Errorf("after the failed send: %q, want the status sent", got)
	}
}

// TestMessageFitsMaxMessage checks that a STATUS= too long for one
// datagram is cut at a rune boundary, with "…", so the message fits
// MaxMessage, and that one that fits is kept whole.
func TestMessageFitsMaxMessage(t *testing.T) {
	long := strings.Repeat("é", MaxMessage)
	msg := message(long, true, "READY=1")
	if len(msg) > MaxMessage || !utf8.ValidString(msg) || !strings.HasPrefix(msg, "READY=1\nSTATUS=éé") || !strings.HasSuffix(msg, "…") {
		t.Errorf("message of a %d-byte status: %d bytes, valid UTF-8 %t, ends %q", len(long), len(msg), utf8.ValidString(msg), msg[max(len(msg)-8, 0):])
	}
	exact := strings.Repeat("a", MaxMessage-len("STATUS="))
	if got := message(exact, true); got != "STATUS="+exact {
		t.Errorf("a status that fits exactly was cut to %d bytes", len(got))
	}

	s := newSink(t)
	r := &readiness{v: notReady(adminapi.ReadyReason{Reason: adminapi.ReasonSecretsUnresolved, Code: "RZ-CFG-026", Detail: long})}
	n, err := New(Options{Socket: s.path, Readiness: r.get})
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Ready(t.Context()); err != nil {
		t.Fatalf("Ready with a long status: %v", err)
	}
	if got := s.next(t); len(got) > MaxMessage || !strings.HasPrefix(got, "READY=1\nSTATUS=not ready: secrets_unresolved RZ-CFG-026 é") {
		t.Errorf("long status sent as %d bytes starting %q", len(got), got[:min(len(got), 60)])
	}
}
