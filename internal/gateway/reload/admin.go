// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// AdminServer serves admin connections on a listener (admin.Server).
type AdminServer interface {
	Serve(ln net.Listener) error
}

// AdminSocketConfig configures an AdminSocket.
type AdminSocketConfig struct {
	// Server serves every admin listener; required.
	Server AdminServer
	// Host is the host the admin port binds: empty for every interface,
	// an IP literal otherwise.
	Host string
	// Steer is set in the process holding the data-directory lock: each
	// admin port is probe-bound, listens at once and is pinned (ADR-0015).
	// A handover successor leaves it unset until Pin.
	Steer bool
	// Fail receives a Serve that fails outside shutdown, a fatal part of
	// the Node: the supervisor Drains on the failure timeline
	// (drain.Drainer.Fail). Nil only logs it.
	Fail func(error)
	// Logger receives the socket's records; nil discards them.
	Logger *slog.Logger
}

// AdminSocket is the admin listener's socket across activations
// (docs/architecture/03-data-plane.md "Admin endpoints"): Bind binds the
// boot port before the first activation; as a Part, an activation whose
// Revision moves Gateway.spec.admin.port binds the new port before the
// swap (RZ-CFG-039 when it cannot) and, right after it, serves the new
// listener and closes the old one. It is the admin port's
// handover.Sockets and Pinner whichever port it serves, and a handover
// successor's Listener and Stopper. Its methods are safe for concurrent
// use.
type AdminSocket struct {
	cfg AdminSocketConfig
	log *slog.Logger

	mu      sync.Mutex
	steer   bool
	cur     *listener.AdminPort
	port    int
	serving bool // cur's listener is served
	stopped bool
	wg      sync.WaitGroup // Serve goroutines
}

// NewAdminSocket returns an AdminSocket bound to nothing yet.
func NewAdminSocket(cfg AdminSocketConfig) (*AdminSocket, error) {
	if cfg.Server == nil {
		return nil, fmt.Errorf("%w: AdminSocketConfig.Server is required", ErrConfig)
	}
	a := &AdminSocket{cfg: cfg, log: cfg.Logger, steer: cfg.Steer}
	if a.log == nil {
		a.log = slog.New(slog.DiscardHandler)
	}
	return a, nil
}

// Bind binds the admin port before the first activation: the admin.port
// of the Bundle the Node boots when its Gateway parses, else Last-Known-
// Good's, else 9901. The lock holder's socket listens and is served at
// once, so /healthz and /readyz answer during the boot; a handover
// successor's waits for Listen. A port that cannot be bound is
// RZ-CFG-039, and a boot that cannot bind its admin port exits 1.
func (a *AdminSocket) Bind(ctx context.Context, port int) error {
	a.mu.Lock()
	steer := a.steer
	a.mu.Unlock()
	np, err := listener.BindAdminPort(ctx, listener.AdminPortOptions{Host: a.cfg.Host, Port: port, Steer: steer, Logger: a.log})
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		_ = np.Close()
		return listener.ErrStopped
	}
	old := a.cur
	a.cur, a.port, a.serving = np, port, false
	var lerr error
	if steer {
		lerr = a.listenLocked()
	}
	a.mu.Unlock()
	if old != nil {
		old.StopAccepting()
	}
	return lerr
}

// Listen makes the current socket listen and serves it, unless it
// already is: a handover successor's, once its handover is accepted.
func (a *AdminSocket) Listen(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listenLocked()
}

// listenLocked listens on and serves the current socket.
func (a *AdminSocket) listenLocked() error {
	if a.cur == nil || a.serving || a.stopped {
		return nil
	}
	ln, err := a.cur.Listen()
	if err != nil {
		return errcode.Wrap(codeBind, fmt.Errorf("admin port %d: %w", a.port, err))
	}
	a.serveLocked(ln)
	return nil
}

// serveLocked serves ln on a goroutine Close waits for.
func (a *AdminSocket) serveLocked(ln net.Listener) {
	a.serving = true
	a.wg.Go(func() {
		err := a.cfg.Server.Serve(ln)
		if err == nil || errors.Is(err, http.ErrServerClosed) || errors.Is(err, net.ErrClosed) {
			return
		}
		a.log.Error("admin server stopped serving", catalog.KeyError, err.Error())
		if a.cfg.Fail != nil {
			a.cfg.Fail(err)
		}
	})
}

// Prepare binds the Revision's admin port when it differs from the one
// served, before the swap: probe-bound, listening and pinned in the lock
// holder, bound only in a handover successor. RZ-CFG-039 rejects a
// Revision whose admin port cannot be bound.
func (a *AdminSocket) Prepare(ctx context.Context, next Compiled) (Switch, error) {
	port := next.Snapshot().Gateway.AdminPort
	a.mu.Lock()
	same := a.cur != nil && a.port == port
	steer, stopped := a.steer, a.stopped
	a.mu.Unlock()
	if same || stopped {
		return nopSwitch{}, nil
	}
	np, err := listener.BindAdminPort(ctx, listener.AdminPortOptions{Host: a.cfg.Host, Port: port, Steer: steer, Logger: a.log})
	if err != nil {
		return nil, err
	}
	return &adminSwitch{a: a, np: np, port: port}, nil
}

// adminSwitch moves the admin listener to a new port.
type adminSwitch struct {
	a    *AdminSocket
	np   *listener.AdminPort
	port int
}

// Commit serves the new socket and closes the old one; the old one keeps
// serving when the new one cannot listen.
func (s *adminSwitch) Commit(ctx context.Context) error {
	a := s.a
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		_ = s.np.Close()
		return nil
	}
	ln, err := s.np.Listen()
	if err != nil {
		a.mu.Unlock()
		_ = s.np.Close()
		return errcode.Wrap(codeBind, fmt.Errorf("admin port %d: %w", s.port, err))
	}
	old, oldPort := a.cur, a.port
	a.cur, a.port = s.np, s.port
	a.serveLocked(ln)
	a.mu.Unlock()
	if old != nil {
		old.StopAccepting()
	}
	a.log.InfoContext(ctx, "admin port moved", "from", strconv.Itoa(oldPort), "to", strconv.Itoa(s.port))
	return nil
}

// Abort closes the new socket.
func (s *adminSwitch) Abort() { _ = s.np.Close() }

// Pin pins the admin port's reuseport group to this process's socket and
// makes every later bind the lock holder's (handover.Pinner).
func (a *AdminSocket) Pin() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = true
	if a.cur == nil {
		return nil
	}
	if err := a.cur.Pin(); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

// SteerToSuccessor steers the admin port's group to the socket a handover
// successor added (handover.Sockets).
func (a *AdminSocket) SteerToSuccessor() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = false
	if a.cur == nil {
		return nil
	}
	if err := a.cur.SteerToSuccessor(); err != nil {
		return fmt.Errorf("reload: %w", err)
	}
	return nil
}

// WaitLingers returns nil: no admin socket lingers, a moved port being
// another socket (handover.Sockets).
func (a *AdminSocket) WaitLingers(context.Context) error { return nil }

// StopAccepting closes the current socket; the admin server keeps serving
// the connections it holds, and later activations bind nothing
// (handover.Sockets).
func (a *AdminSocket) StopAccepting() {
	a.mu.Lock()
	a.stopped = true
	cur := a.cur
	a.mu.Unlock()
	if cur != nil {
		cur.StopAccepting()
	}
}

// Close stops accepting and waits for every Serve goroutine.
func (a *AdminSocket) Close() error {
	a.StopAccepting()
	a.wg.Wait()
	return nil
}

// Addr returns the current socket's address, nil before Bind.
func (a *AdminSocket) Addr() net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cur == nil {
		return nil
	}
	return a.cur.Addr()
}

// Port returns the configured admin port, 0 before Bind.
func (a *AdminSocket) Port() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.port
}
