// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// AdminPortOptions configure BindAdminPort.
type AdminPortOptions struct {
	// Host is the host the socket binds: empty for every interface, as
	// ruralzd binds; an IP literal otherwise (tests use loopback).
	Host string
	// Port is the admin port, Gateway.spec.admin.port; 0 binds a free port
	// and skips the probe.
	Port int
	// Steer is set in the process holding the data-directory lock: the
	// port is first probe-bound without SO_REUSEPORT, then the socket
	// listens at once and the port's reuseport group is pinned to it
	// (ADR-0015 "Pinning"). A handover successor leaves it unset: the
	// socket is bound without listening until Listen, skipping the probe
	// of the port the holder's admin socket listens on, and Pin pins it
	// once the successor holds the lock.
	Steer bool
	// Logger receives a record when pinning fails; nil discards it.
	Logger *slog.Logger
}

// AdminPort is the socket of the admin listener on Gateway.spec.admin.port:
// the admin server (internal/gateway/admin) serves the listener Listen
// returns, and AdminPort keeps the port's reuseport group as ADR-0015 keeps
// every group of a lock holder, 9901 included: probe-bound before the
// bind, pinned to the holder's socket, steered to a handover successor's
// socket during the handover and re-pinned when the successor is lost
// during the linger. A Hot Reload that moves the admin port binds a new
// AdminPort on the new port and stops the old one. Its methods are safe
// for concurrent use.
type AdminPort struct {
	log *slog.Logger

	mu sync.Mutex
	// sock is a successor's bound socket until Listen; nil after.
	sock *reuseport.Socket
	// tcp is the listening socket, recorded in group.
	tcp     *net.TCPListener
	local   net.Addr
	group   reuseport.Group
	steer   bool
	stopped bool
}

// BindAdminPort binds the admin port with SO_REUSEPORT. With Steer the
// port is probe-bound first, unless it is 0, and the socket listens and is
// pinned at once; without, the socket is bound and does not listen until
// Listen, where the platform can bind without listening (Linux), else it
// listens at once. A port another process listens on, or one that cannot
// be bound, is RZ-CFG-039: a boot that cannot bind its admin port exits 1,
// and an activation that moves the port to one is rejected.
func BindAdminPort(ctx context.Context, o AdminPortOptions) (*AdminPort, error) {
	p := &AdminPort{log: o.Logger, steer: o.Steer}
	if p.log == nil {
		p.log = slog.New(slog.DiscardHandler)
	}
	addr := net.JoinHostPort(o.Host, strconv.Itoa(o.Port))
	if o.Steer && o.Port != 0 {
		if err := reuseport.Probe(ctx, addr); err != nil {
			return nil, bindError(ctx, o.Port, fmt.Errorf("probe bind: %w", err))
		}
	}
	sock, err := reuseport.Bind(ctx, addr)
	switch {
	case errors.Is(err, errors.ErrUnsupported):
		tcp, lerr := reuseport.Listen(ctx, addr)
		if lerr != nil {
			return nil, bindError(ctx, o.Port, lerr)
		}
		p.tcp, p.local = tcp, tcp.Addr()
		p.group.Add(tcp)
		if o.Steer {
			p.pinLocked()
		}
	case err != nil:
		return nil, bindError(ctx, o.Port, err)
	default:
		p.sock, p.local = sock, sock.Addr()
	}
	if o.Steer {
		if _, err := p.Listen(); err != nil {
			p.StopAccepting()
			return nil, bindError(ctx, o.Port, err)
		}
	}
	return p, nil
}

// BindAdminPorts binds the admin port once per host, all on o.Port, with
// BindAdminPort and o's other options: none binds every interface with
// one socket, and RURALZ_BIND_LOOPBACK passes LoopbackHosts, so the admin
// listener binds 127.0.0.1 and, where the host has IPv6, ::1, one socket
// per address, as every client listener does
// (docs/architecture/03-data-plane.md "Listeners and protocols"). On port
// 0 every host after the first binds the port the first one got, binding
// again on another free port when a later host finds it taken. Each
// AdminPort keeps its own reuseport group, since the kernel groups sockets
// by address: the handover pins and steers each. On error none stays
// bound.
func BindAdminPorts(ctx context.Context, hosts []string, o AdminPortOptions) ([]*AdminPort, error) {
	hosts, err := checkHosts(hosts)
	if err != nil {
		return nil, err
	}
	for attempt := 1; ; attempt++ {
		ports, err := bindAdminHosts(ctx, hosts, o)
		if err == nil {
			return ports, nil
		}
		retry := o.Port == 0 && len(hosts) > 1 && attempt < freePortAttempts && errors.Is(err, syscall.EADDRINUSE) && ctx.Err() == nil
		if !retry {
			return nil, err
		}
	}
}

// bindAdminHosts binds one AdminPort per host on o.Port, or on port 0 the
// port the first host got; on error it closes those it bound.
func bindAdminHosts(ctx context.Context, hosts []string, o AdminPortOptions) ([]*AdminPort, error) {
	out := make([]*AdminPort, 0, len(hosts))
	for _, host := range hosts {
		ho := o
		ho.Host = host
		p, err := BindAdminPort(ctx, ho)
		if err != nil {
			for _, q := range out {
				q.StopAccepting()
			}
			return nil, err
		}
		out = append(out, p)
		if o.Port == 0 {
			o.Port = portOfAddr(p.Addr())
		}
	}
	return out, nil
}

// bindError returns ctx's error when ctx ended, else err as RZ-CFG-039.
func bindError(ctx context.Context, port int, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	return errcode.Wrap(codeBind, fmt.Errorf("admin port %d: %w", port, err))
}

// Addr returns the bound address, with the port the kernel chose for
// port 0.
func (p *AdminPort) Addr() net.Addr { return p.local }

// Listen makes the socket listen, when it does not yet, and returns the
// listening socket for the admin server's Serve, which counts its
// connections against the admin ceiling. A successor's socket joins its
// port's group at the end, so the holder's pin keeps every connection
// until the holder steers to it; a socket that listens once Pin was
// called is pinned at once. After StopAccepting it returns ErrStopped.
func (p *AdminPort) Listen() (net.Listener, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return nil, ErrStopped
	}
	if p.tcp == nil {
		tcp, err := p.sock.Listen()
		if err != nil {
			return nil, fmt.Errorf("admin port: %w", err)
		}
		p.tcp, p.sock = tcp, nil
		p.group.Add(tcp)
		if p.steer {
			p.pinLocked()
		}
	}
	return p.tcp, nil
}

// pinLocked pins the group to the listening socket, logging a failure.
func (p *AdminPort) pinLocked() {
	if err := p.group.Pin(p.tcp); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		p.log.Warn("admin port steering failed", catalog.KeyError, err.Error())
	}
}

// Pin pins the port's reuseport group to this process's listening socket:
// the lock holder's pinning. A handover successor calls it once it holds
// the lock, when the previous holder's admin socket is closed, and a
// holder calls it to re-pin when the successor it steered to is lost
// during the linger. A socket that does not listen yet is pinned once it
// listens; off Linux it returns errors.ErrUnsupported.
func (p *AdminPort) Pin() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steer = true
	if p.tcp == nil || p.stopped {
		return nil
	}
	if err := p.group.Pin(p.tcp); err != nil {
		return fmt.Errorf("admin port: %w", err)
	}
	return nil
}

// SteerToSuccessor steers the port's reuseport group to the socket a
// handover successor added after this process's socket (ADR-0015
// "Steering"); the socket keeps accepting until StopAccepting. Off Linux
// it returns errors.ErrUnsupported.
func (p *AdminPort) SteerToSuccessor() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.steer = false
	if p.tcp == nil || p.stopped {
		return nil
	}
	if err := p.group.SteerTo(p.group.Len()); err != nil {
		return fmt.Errorf("admin port: %w", err)
	}
	return nil
}

// WaitLingers returns nil: no admin socket lingers, since a moved admin
// port is another AdminPort on another port. It lets the handover treat
// the admin port like the client listeners.
func (p *AdminPort) WaitLingers(context.Context) error { return nil }

// StopAccepting closes the socket, listening or only bound; the admin
// server keeps serving the connections it holds, and its Serve returns an
// error matching net.ErrClosed, which is no failure. A handover's holder
// calls it at the linger's end, with the client listeners' StopAccepting.
// It is idempotent.
func (p *AdminPort) StopAccepting() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.sock != nil {
		_ = p.sock.Close()
		p.sock = nil
	}
	if p.tcp != nil {
		_ = p.tcp.Close()
		p.group.Remove(p.tcp)
	}
}

// Close closes the socket, as StopAccepting does.
func (p *AdminPort) Close() error {
	p.StopAccepting()
	return nil
}
