// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/drain"
	"github.com/ravindu-rev/ruralz/internal/gateway/handover"
	"github.com/ravindu-rev/ruralz/internal/gateway/lkg"
	"github.com/ravindu-rev/ruralz/internal/gateway/sdnotify"
	"github.com/ravindu-rev/ruralz/internal/gateway/source"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Where the admin port bound before the first activation came from
// (docs/architecture/03-data-plane.md "Admin endpoints").
const (
	portFromSource    = "source"
	portFromCandidate = "candidate"
	portFromLKG       = "last_known_good"
	portFromDefault   = "default"
)

// serve runs the Node: the Drain first, so a stop signal during the boot
// drains at once, then the boot, then the wait for the Drain's end. It
// returns the Drain's exit code once every goroutine the Node started
// has ended.
func (n *node) serve(ctx context.Context) int {
	defer n.close(ctx)
	done := make(chan int, 1)
	go func() { done <- n.drainer.Run(context.WithoutCancel(ctx)) }()
	// The end of ctx counts as a stop signal.
	stopCtx := context.AfterFunc(ctx, n.drainer.Signal)
	defer stopCtx()
	if n.p.signals {
		stopSignals := drain.Notify(n.drainer)
		defer stopSignals()
		stopHUP := source.NotifySIGHUP(n.watcher)
		defer stopHUP()
	}
	n.boot(ctx)
	code := <-done
	n.shutdown(ctx)
	return code
}

// boot starts the background parts, binds the admin port, starts the
// activation loop and, once the first boot attempt ended, sends READY=1
// (a lock holder) or runs the handover gate (a successor). A boot that
// cannot bind its admin port fails the Node: exit 1.
func (n *node) boot(ctx context.Context) {
	n.loops.goRun("retirer", n.retirer.Run)
	n.loops.goRun("upstream manager", n.ups.Run)
	n.loops.goRun("secret resolver", n.res.Run)
	for _, c := range n.filters.Components() {
		n.components.goRun(c.Name(), c.Run)
	}
	port, from := n.bootAdminPort(ctx)
	if err := n.adminSock.Bind(ctx, port); err != nil {
		n.log.ErrorContext(ctx, "admin port cannot be bound", "port", port, "from", from, catalog.KeyError, err.Error())
		n.failed(fmt.Errorf("admin port %d: %w", port, err))
		return
	}
	n.log.InfoContext(ctx, "admin port bound", "port", port, "from", from, "addresses", addrList(n.adminSock.Addrs()))
	if !n.successor {
		n.startHolder(ctx)
	}
	n.loops.goRun("activation loop", n.loader.Run)
	if r := n.startWatchdog(ctx); r != nil {
		n.failed(r)
	}
	n.loops.goFunc("boot", func(ctx context.Context) {
		select {
		case <-n.loader.Booted():
		case <-n.drainer.Started():
			return
		case <-ctx.Done():
			return
		}
		if n.successor {
			n.handover(ctx)
			return
		}
		n.ready(ctx)
	})
}

// ready sends READY=1 with the current STATUS= once the lock holder's first
// boot attempt ended, ready or not, so a start never waits on usable
// configuration; a Drain that started first sends no READY=1.
func (n *node) ready(ctx context.Context) {
	select {
	case <-n.drainer.Started():
		return
	default:
	}
	if err := n.notifier.Ready(ctx); err != nil {
		n.log.WarnContext(ctx, "READY=1 not delivered", catalog.KeyError, err.Error())
	}
}

// bootAdminPort returns the admin port to bind before the first activation
// and where it came from: the admin.port of the Bundle the Node boots when
// its Gateway parses (the source for a lock holder, the holder's candidate
// for a handover successor, or the source when the holder has none), else
// Last-Known-Good's, else 9901.
func (n *node) bootAdminPort(ctx context.Context) (int, string) {
	switch {
	case n.successor:
		e, err := n.store.Candidate()
		if err == nil {
			if p, ok := e.AdminPort(); ok {
				return p, portFromCandidate
			}
		} else if errors.Is(err, lkg.ErrAbsent) {
			if p, ok := n.watcher.AdminPort(ctx); ok {
				return p, portFromSource
			}
		}
	default:
		if p, ok := n.watcher.AdminPort(ctx); ok {
			return p, portFromSource
		}
	}
	if e, err := n.store.LastKnownGood(); err == nil {
		if p, ok := e.AdminPort(); ok {
			return p, portFromLKG
		}
	}
	return admin.DefaultPort, portFromDefault
}

// startHolder serves the handover socket of the process holding the
// data-directory lock, so a new process on the same data directory can
// take over. A socket that cannot serve leaves the Node serving without
// in-place upgrades, logged at error.
func (n *node) startHolder(ctx context.Context) {
	h, err := handover.StartHolder(n.loopCtx, handover.HolderConfig{
		Dir: n.dir, Lock: n.lock, Activations: n.loader, Drain: n.drainer,
		Sockets: []handover.Sockets{n.set, n.adminSock}, Admin: n.adminSrv,
		Meter: handover.Meter{Conns: n.conns, Units: n.units, Budget: n.budget},
		Clock: n.clk, Logger: n.rt.Logger(componentHandover),
	})
	if err != nil {
		n.log.ErrorContext(ctx, "handover socket not served: in-place upgrades are off", catalog.KeyError, err.Error())
		return
	}
	n.mu.Lock()
	n.holderSrv = h
	n.mu.Unlock()
}

// handover runs a successor's handover gate once its boot ended: taking
// the lock makes it the holder, which watches the source and serves the
// handover socket in its turn; a refused or lost handover fails the Node,
// exit 1; a Drain that started first decides the exit.
func (n *node) handover(ctx context.Context) {
	rec, err := nodedir.NewHolder(n.nodeID, n.version)
	if err != nil {
		n.log.WarnContext(ctx, "holder.json record incomplete", catalog.KeyError, err.Error())
	}
	succ, err := handover.NewSuccessor(handover.SuccessorConfig{
		Dir: n.dir, Lock: n.lock, Drain: n.drainer, Start: n.p.start, Version: n.version, PID: n.p.pid,
		Active: n.loader.Active, Reload: n.loader.Reload, Bound: n.loader.Bound(),
		Pinners: []handover.Pinner{n.set, n.adminSock}, Record: rec, Notifier: n.notifier,
		Clock: n.clk, Logger: n.rt.Logger(componentHandover),
		Ceilings: handover.Ceilings{Conns: n.conns, Units: n.units, Budget: n.budget},
	})
	if err != nil {
		n.failed(fmt.Errorf("handover: %w", err))
		return
	}
	n.mu.Lock()
	n.succ = succ
	n.mu.Unlock()
	out, err := succ.Run(ctx)
	switch out {
	case handover.OutcomeHolder:
		n.loader.TookLock()
		n.startHolder(ctx)
		if err != nil {
			n.failed(err)
		}
	case handover.OutcomeRefused, handover.OutcomeLost:
		n.failed(fmt.Errorf("handover %s: %w", out, err))
	default:
		// The Drain started first and decides the exit.
	}
}

// startWatchdog starts the watchdog when the service manager asks for one:
// WATCHDOG=1 every half interval while the admin server, the retirer and
// the activation loop each answered its probe within the interval.
func (n *node) startWatchdog(ctx context.Context) error {
	iv := n.interval.env.Watchdog
	if iv <= 0 {
		return nil
	}
	wd, err := sdnotify.NewWatchdog(sdnotify.WatchdogOptions{
		Notifier: n.notifier, Interval: iv, Clock: n.clk, Logger: n.rt.Logger(componentWatchdog),
		Parts: []sdnotify.Part{
			{Name: "admin server", Probe: n.adminProbe()},
			{Name: "retirer", Probe: n.retirer.Ping},
			{Name: "activation loop", Probe: n.loader.Probe},
		},
	})
	if err != nil {
		return fmt.Errorf("watchdog: %w", err)
	}
	n.log.InfoContext(ctx, "watchdog enabled", "interval", iv.String())
	n.loops.goRun("watchdog", wd.Run)
	return nil
}

// shutdown stops what the Drain left running: the background loops, the
// handover socket and a successor's follower, each awaited within the
// Drain's flush bound.
func (n *node) shutdown(ctx context.Context) {
	n.stop()
	n.stopComps()
	n.mu.Lock()
	h, succ := n.holderSrv, n.succ
	n.mu.Unlock()
	if h != nil {
		_ = h.Close()
	}
	if succ != nil {
		_ = succ.Close()
	}
	wctx, cancel := n.bounded(context.WithoutCancel(ctx), drain.FlushBound)
	defer cancel()
	for name, s := range map[string]*supervisor{"background parts": n.loops, "filter components": n.components} {
		if err := s.wait(wctx); err != nil {
			n.log.WarnContext(ctx, "goroutines still running at exit", "parts", name, catalog.KeyError, err.Error())
		}
	}
}

// bounded returns a context that ends with parent or d later on the
// process clock.
func (n *node) bounded(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	t := n.clk.AfterFunc(d, cancel)
	return ctx, func() {
		t.Stop()
		cancel()
	}
}

// addrList returns the addresses as strings, for a log record.
func addrList(as []net.Addr) []string {
	out := make([]string, len(as))
	for i, a := range as {
		out[i] = a.String()
	}
	return out
}
