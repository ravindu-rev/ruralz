// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sdnotify

import (
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// waitFor polls cond with a real-time bound; it waits only for goroutines
// the test or the package started, never for fake time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// recv waits for one value from ch with a real-time bound.
func recv[T any](t *testing.T, what string, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

// socketDir returns a directory whose socket paths fit sun_path (about
// 100 bytes): the test's temporary directory when short enough.
func socketDir(t *testing.T) string {
	t.Helper()
	if dir := t.TempDir(); len(dir) < 80 {
		return dir
	}
	dir, err := os.MkdirTemp("", "sdn")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// sink is a notification socket the test reads, standing in for systemd.
type sink struct {
	path string
	conn *net.UnixConn
}

// listenSink listens on a datagram socket at path, skipping the test where
// datagram Unix sockets do not exist.
func listenSink(t *testing.T, path string) *sink {
	t.Helper()
	var lc net.ListenConfig
	pc, err := lc.ListenPacket(t.Context(), "unixgram", path)
	if err != nil {
		t.Skipf("no datagram Unix socket at %s: %v", path, err)
	}
	t.Cleanup(func() { _ = pc.Close() })
	c, ok := pc.(*net.UnixConn)
	if !ok {
		t.Fatalf("unixgram listener is a %T", pc)
	}
	return &sink{path: path, conn: c}
}

// newSink listens on a datagram socket in a fresh directory.
func newSink(t *testing.T) *sink {
	t.Helper()
	return listenSink(t, filepath.Join(socketDir(t), "notify"))
}

// next reads one notification, waiting at most 10 s of real time.
func (s *sink) next(t *testing.T) string {
	t.Helper()
	if err := s.conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 2*MaxMessage)
	n, err := s.conn.Read(b)
	if err != nil {
		t.Fatalf("no notification: %v", err)
	}
	return string(b[:n])
}

// nothingSince checks that no notification arrived before a marker the
// test sends now: datagrams reach one socket in the order they are sent.
func (s *sink) nothingSince(t *testing.T) {
	t.Helper()
	c, err := dialSocket(t.Context(), s.path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Write([]byte("MARKER=1"))
	_ = c.Close()
	if err != nil {
		t.Fatal(err)
	}
	if got := s.next(t); got != "MARKER=1" {
		t.Fatalf("unexpected notification %q", got)
	}
}

// readiness is a readiness source the test sets.
type readiness struct {
	mu sync.Mutex
	v  adminapi.Readyz
}

func (r *readiness) set(v adminapi.Readyz) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.v = v
}

func (r *readiness) get() adminapi.Readyz {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.v
}

// Readiness states.

func readyAt(rev string) adminapi.Readyz {
	return adminapi.Readyz{Status: "ready", Revision: rev}
}

func notReady(reasons ...adminapi.ReadyReason) adminapi.Readyz {
	return adminapi.Readyz{Status: "not_ready", Reasons: reasons}
}

// captureHandler is a slog.Handler keeping every record's level, message
// and attributes.
type captureHandler struct {
	mu   sync.Mutex
	recs []string
}

func (*captureHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *captureHandler) Handle(_ context.Context, r slog.Record) error {
	var b strings.Builder
	b.WriteString(r.Level.String() + " " + r.Message)
	r.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recs = append(h.recs, b.String())
	return nil
}

func (h *captureHandler) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *captureHandler) WithGroup(string) slog.Handler      { return h }

// count returns how many records contain every part.
func (h *captureHandler) count(parts ...string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, r := range h.recs {
		all := true
		for _, p := range parts {
			all = all && strings.Contains(r, p)
		}
		if all {
			n++
		}
	}
	return n
}
