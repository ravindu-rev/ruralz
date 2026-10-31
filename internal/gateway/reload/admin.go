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
	// Hosts are the hosts the admin port binds, one socket per host, each
	// an IP literal: none binds every interface with one socket, and
	// RURALZ_BIND_LOOPBACK passes listener.LoopbackHosts, so the admin
	// listener binds the addresses every client listener binds
	// (docs/architecture/03-data-plane.md "Listeners and protocols").
	Hosts []string
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

// AdminSocket is the admin listener's sockets across activations
// (docs/architecture/03-data-plane.md "Admin endpoints"), one per host of
// AdminSocketConfig.Hosts, each its own reuseport group: Bind binds the
// boot port before the first activation; as a Part, an activation whose
// Revision moves Gateway.spec.admin.port binds the new port before the
// swap (RZ-CFG-039 when it cannot) and, right after it, serves the new
// listeners and closes the old ones. It is the admin port's
// handover.Sockets and Pinner whichever port it serves, and a handover
// successor's Listener and Stopper. Its methods are safe for concurrent
// use.
type AdminSocket struct {
	cfg AdminSocketConfig
	log *slog.Logger

	mu      sync.Mutex
	steer   bool
	cur     []*listener.AdminPort // one per host, in host order
	port    int
	serving bool // cur's listeners are served
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

// bind binds the admin port once per host (listener.BindAdminPorts).
func (a *AdminSocket) bind(ctx context.Context, port int, steer bool) ([]*listener.AdminPort, error) {
	return listener.BindAdminPorts(ctx, a.cfg.Hosts, listener.AdminPortOptions{Port: port, Steer: steer, Logger: a.log})
}

// stopAll stops every socket of ps.
func stopAll(ps []*listener.AdminPort) {
	for _, p := range ps {
		p.StopAccepting()
	}
}

// Bind binds the admin port before the first activation: the admin.port
// of the Bundle the Node boots when its Gateway parses, else Last-Known-
// Good's, else 9901. The lock holder's sockets listen and are served at
// once, so /healthz and /readyz answer during the boot; a handover
// successor's wait for Listen. A port that cannot be bound is
// RZ-CFG-039, and a boot that cannot bind its admin port exits 1.
func (a *AdminSocket) Bind(ctx context.Context, port int) error {
	a.mu.Lock()
	steer := a.steer
	a.mu.Unlock()
	np, err := a.bind(ctx, port, steer)
	if err != nil {
		return err
	}
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		stopAll(np)
		return listener.ErrStopped
	}
	old := a.cur
	a.cur, a.port, a.serving = np, port, false
	var lerr error
	if steer {
		lerr = a.listenLocked()
	}
	a.mu.Unlock()
	stopAll(old)
	return lerr
}

// Listen makes the current sockets listen and serves them, unless they
// already are: a handover successor's, once its handover is accepted.
func (a *AdminSocket) Listen(context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.listenLocked()
}

// listenLocked listens on and serves every current socket.
func (a *AdminSocket) listenLocked() error {
	if len(a.cur) == 0 || a.serving || a.stopped {
		return nil
	}
	lns, err := listenAll(a.cur)
	if err != nil {
		return errcode.Wrap(codeBind, fmt.Errorf("admin port %d: %w", a.port, err))
	}
	a.serveLocked(lns)
	return nil
}

// listenAll makes every socket of ps listen and returns their listeners;
// on error none is returned.
func listenAll(ps []*listener.AdminPort) ([]net.Listener, error) {
	lns := make([]net.Listener, 0, len(ps))
	for _, p := range ps {
		ln, err := p.Listen()
		if err != nil {
			return nil, err
		}
		lns = append(lns, ln)
	}
	return lns, nil
}

// serveLocked serves each listener of lns on a goroutine Close waits for.
func (a *AdminSocket) serveLocked(lns []net.Listener) {
	a.serving = true
	for _, ln := range lns {
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
}

// Prepare binds the Revision's admin port when it differs from the one
// served, before the swap: probe-bound, listening and pinned in the lock
// holder, bound only in a handover successor. RZ-CFG-039 rejects a
// Revision whose admin port cannot be bound.
func (a *AdminSocket) Prepare(ctx context.Context, next Compiled) (Switch, error) {
	port := next.Snapshot().Gateway.AdminPort
	a.mu.Lock()
	same := len(a.cur) > 0 && a.port == port
	steer, stopped := a.steer, a.stopped
	a.mu.Unlock()
	if same || stopped {
		return nopSwitch{}, nil
	}
	np, err := a.bind(ctx, port, steer)
	if err != nil {
		return nil, err
	}
	return &adminSwitch{a: a, np: np, port: port}, nil
}

// adminSwitch moves the admin listener to a new port.
type adminSwitch struct {
	a    *AdminSocket
	np   []*listener.AdminPort
	port int
}

// Commit serves the new sockets and closes the old ones; the old ones keep
// serving when a new one cannot listen.
func (s *adminSwitch) Commit(ctx context.Context) error {
	a := s.a
	a.mu.Lock()
	if a.stopped {
		a.mu.Unlock()
		stopAll(s.np)
		return nil
	}
	lns, err := listenAll(s.np)
	if err != nil {
		a.mu.Unlock()
		stopAll(s.np)
		return errcode.Wrap(codeBind, fmt.Errorf("admin port %d: %w", s.port, err))
	}
	old, oldPort := a.cur, a.port
	a.cur, a.port = s.np, s.port
	a.serveLocked(lns)
	a.mu.Unlock()
	stopAll(old)
	a.log.InfoContext(ctx, "admin port moved", "from", strconv.Itoa(oldPort), "to", strconv.Itoa(s.port))
	return nil
}

// Abort closes the new sockets.
func (s *adminSwitch) Abort() { stopAll(s.np) }

// Pin pins each admin port's reuseport group to this process's socket and
// makes every later bind the lock holder's (handover.Pinner).
func (a *AdminSocket) Pin() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = true
	var errs []error
	for _, p := range a.cur {
		if err := p.Pin(); err != nil {
			errs = append(errs, fmt.Errorf("reload: %w", err))
		}
	}
	return errors.Join(errs...)
}

// SteerToSuccessor steers each admin port's group to the socket a
// handover successor added (handover.Sockets).
func (a *AdminSocket) SteerToSuccessor() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.steer = false
	var errs []error
	for _, p := range a.cur {
		if err := p.SteerToSuccessor(); err != nil {
			errs = append(errs, fmt.Errorf("reload: %w", err))
		}
	}
	return errors.Join(errs...)
}

// WaitLingers returns nil: no admin socket lingers, a moved port being
// other sockets (handover.Sockets).
func (a *AdminSocket) WaitLingers(context.Context) error { return nil }

// StopAccepting closes the current sockets; the admin server keeps
// serving the connections it holds, and later activations bind nothing
// (handover.Sockets).
func (a *AdminSocket) StopAccepting() {
	a.mu.Lock()
	a.stopped = true
	cur := a.cur
	a.mu.Unlock()
	stopAll(cur)
}

// Close stops accepting and waits for every Serve goroutine.
func (a *AdminSocket) Close() error {
	a.StopAccepting()
	a.wg.Wait()
	return nil
}

// Addr returns the address of the current socket of the first host, nil
// before Bind.
func (a *AdminSocket) Addr() net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cur) == 0 {
		return nil
	}
	return a.cur[0].Addr()
}

// Addrs returns the addresses of the current sockets, one per host in
// AdminSocketConfig.Hosts order, nil before Bind.
func (a *AdminSocket) Addrs() []net.Addr {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.cur) == 0 {
		return nil
	}
	out := make([]net.Addr, len(a.cur))
	for i, p := range a.cur {
		out[i] = p.Addr()
	}
	return out
}

// Port returns the configured admin port, 0 before Bind.
func (a *AdminSocket) Port() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.port
}
