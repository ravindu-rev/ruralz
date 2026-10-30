// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dev

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/cli/launch"
	"github.com/ravindu-rev/ruralz/internal/clock"
)

// runner is one ruralz dev run: its sources, workspace and child, and the
// loop that renders, launches, watches and stops.
type runner struct {
	sio          command.IO
	clock        clock.Clock
	lo           launch.Options
	ws           *launch.Workspace
	bin          string
	src          string
	environments string
	overrides    string
	renderer     *launch.Renderer
	json         bool
	compact      bool
	colorErr     bool
	readyTimeout time.Duration
	out, errw    *command.LineWriter
	sigs         chan os.Signal

	child  *launch.Child
	admin  *launch.Admin
	active *launch.Render
	// last is the sources as the last render read them.
	last snapshot
	// nsig counts the signals received; first is the first of them.
	nsig  int
	first os.Signal
}

// event is what ended a wait.
type event uint8

const (
	// evTimer is the timer's fire.
	evTimer event = iota
	// evChild is the child's exit.
	evChild
	// evStop is a stop request: a signal, or the end of ctx.
	evStop
)

// run renders until a render is valid, launches ruralzd on it, awaits its
// readiness, then serves until a stop request or the child's exit. The
// private directory is removed once the child has exited.
func (r *runner) run(ctx context.Context) error {
	err := r.runChild(ctx)
	if r.admin != nil {
		r.admin.Close()
	}
	if r.child != nil && !r.child.Exited() {
		// An error after the launch: stop the child before leaving.
		r.shutdown()
	}
	if rmErr := r.ws.Remove(); rmErr != nil {
		_ = r.errw.Line("warning: " + rmErr.Error())
	}
	return err
}

// runChild is run until the child has exited or must be stopped.
func (r *runner) runChild(ctx context.Context) error {
	rend, err := r.firstRender(ctx)
	if err != nil {
		return err
	}
	if !rend.Rewritten {
		if err := launch.CheckPortsFree(ctx, rend); err != nil {
			return err
		}
	}
	if err := r.renderer.Swap(rend); err != nil {
		return err
	}
	if r.admin, err = launch.NewAdmin(r.sio, rend.AdminURL(), r.ws, r.lo); err != nil {
		return err
	}
	if r.child, err = launch.Start(ctx, launch.StartOptions{
		Binary:  r.bin,
		Environ: launch.Environ(r.sio.Environ, r.ws, r.overrides != ""),
		Logs:    r.sio.Stderr,
		Compact: r.compact,
	}, r.lo); err != nil {
		return err
	}
	if err := r.awaitReady(ctx); err != nil {
		return err
	}
	r.active = rend
	if err := r.printReady(rend); err != nil {
		return err
	}
	return r.serve(ctx)
}

// firstRender renders until a render is valid, printing each failure and
// waiting for the sources to change in between. An input that cannot be
// read at the first render exits 2.
func (r *runner) firstRender(ctx context.Context) (*launch.Render, error) {
	r.last = r.snapshot(ctx)
	for attempt := 0; ; attempt++ {
		rend, err := r.renderer.Render(ctx)
		if err == nil {
			r.printWarnings(rend)
			return rend, nil
		}
		if ctx.Err() != nil {
			return nil, r.stop(ctx)
		}
		var f *launch.Failure
		if !errors.As(err, &f) && attempt == 0 {
			return nil, err
		}
		r.printFailure(err)
		if err := r.awaitChange(ctx); err != nil {
			return nil, err
		}
	}
}

// awaitChange polls the sources until a change settles; a stop request
// ends the wait.
func (r *runner) awaitChange(ctx context.Context) error {
	for {
		poll := r.clock.NewTimer(launch.PollInterval)
		if r.wait(ctx, poll) == evStop {
			return r.stop(ctx)
		}
		snap := r.snapshot(ctx)
		for !snap.equal(r.last) {
			settle := r.clock.NewTimer(launch.SettleTime)
			if r.wait(ctx, settle) == evStop {
				return r.stop(ctx)
			}
			again := r.snapshot(ctx)
			if again.equal(snap) {
				r.last = again
				return nil
			}
			snap = again
		}
	}
}

// wait waits for t, the child's exit or a stop request.
func (r *runner) wait(ctx context.Context, t clock.Timer) event {
	var child <-chan struct{}
	if r.child != nil {
		child = r.child.Done()
	}
	select {
	case <-t.C():
		return evTimer
	case <-child:
		t.Stop()
		return evChild
	case s := <-r.sigs:
		t.Stop()
		r.signal(s)
		return evStop
	case <-ctx.Done():
		t.Stop()
		return evStop
	}
}

// signal counts a signal received.
func (r *runner) signal(s os.Signal) {
	r.nsig++
	if r.first == nil {
		r.first = s
	}
}

// awaitReady polls the child's /readyz every 0.5 s (target) until it
// answers 200, and checks its holder.json version once it appears. When
// the ready timeout, counted from the launch, passes first, it prints the
// last answer's reasons and exits 2; a child that exits first exits 2 at
// once.
func (r *runner) awaitReady(ctx context.Context) error {
	deadline := r.clock.Now().Add(r.readyTimeout)
	held := false
	last := "no answer from " + r.admin.URL()
	for {
		switch r.wait(ctx, r.clock.NewTimer(launch.ReadyInterval)) {
		case evStop:
			return r.stop(ctx)
		case evChild:
			return exitedError(r.child, false)
		case evTimer:
		}
		if !held {
			var err error
			if held, err = launch.CheckHolder(r.ws, r.bin, r.lo); err != nil {
				return err
			}
		}
		ans, err := r.admin.Readyz(ctx)
		switch {
		case ctx.Err() != nil:
			return r.stop(ctx)
		case err == nil && ans.Ready:
			if !held {
				if held, err = launch.CheckHolder(r.ws, r.bin, r.lo); err != nil {
					return err
				}
				if !held {
					return command.NoResultf("%s answers /readyz but wrote no holder.json in its data directory", r.bin)
				}
			}
			return nil
		case err == nil:
			last = launch.Reasons(ans)
		default:
			last = err.Error()
		}
		if !r.clock.Now().Before(deadline) {
			return command.NoResultf("ruralzd is not ready after %s: %s", command.FormatDuration(r.readyTimeout), last)
		}
	}
}

// activation is a swap whose test render /readyz has not named yet.
type activation struct {
	rend     *launch.Render
	deadline time.Time
	timer    clock.Timer
}

// serve watches the sources and swaps each new valid test render in,
// reporting its activation or rejection, until a stop request or the
// child's exit. A swap that a newer one replaces before /readyz names it
// reports neither, as ruralzd keeps only the newest pending Revision.
func (r *runner) serve(ctx context.Context) error {
	poll := r.clock.NewTimer(launch.PollInterval)
	var settle clock.Timer
	var settling snapshot
	var act *activation
	for {
		var settleC, actC <-chan time.Time
		if settle != nil {
			settleC = settle.C()
		}
		if act != nil {
			actC = act.timer.C()
		}
		select {
		case <-poll.C():
			if snap := r.snapshot(ctx); settle == nil && !snap.equal(r.last) {
				settling, settle = snap, r.clock.NewTimer(launch.SettleTime)
			}
			poll = r.clock.NewTimer(launch.PollInterval)
		case <-settleC:
			snap := r.snapshot(ctx)
			if !snap.equal(settling) {
				settling, settle = snap, r.clock.NewTimer(launch.SettleTime)
				continue
			}
			settle, r.last = nil, snap
			next, err := r.rerender(ctx, act)
			if err != nil {
				return err
			}
			if next != nil {
				if act != nil {
					act.timer.Stop()
				}
				act = next
			}
		case <-actC:
			done, err := r.checkActivation(ctx, act)
			if err != nil {
				return err
			}
			if done {
				act = nil
			} else {
				act.timer = r.clock.NewTimer(launch.ReadyInterval)
			}
		case <-r.child.Done():
			return exitedError(r.child, true)
		case s := <-r.sigs:
			r.signal(s)
			return r.stop(ctx)
		case <-ctx.Done():
			return r.stop(ctx)
		}
	}
}

// rerender renders the changed sources and swaps a new test render in,
// returning its activation; a failed render prints its diagnostics and
// keeps the active Revision, and a render whose test digest equals the
// newest swapped one only copies its secret files again, so ruralzd's
// file watch rotates them.
func (r *runner) rerender(ctx context.Context, pending *activation) (*activation, error) {
	rend, err := r.renderer.Render(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, r.stop(ctx)
		}
		r.printFailure(err)
		return nil, nil
	}
	r.printWarnings(rend)
	newest := r.active
	if pending != nil {
		newest = pending.rend
	}
	if rend.TestDigest == newest.TestDigest {
		if err := r.renderer.RefreshSecrets(rend); err != nil {
			r.printFailure(err)
		}
		return nil, nil
	}
	if err := r.renderer.Swap(rend); err != nil {
		r.printFailure(err)
		return nil, nil
	}
	return &activation{
		rend: rend, deadline: r.clock.Now().Add(r.readyTimeout),
		timer: r.clock.NewTimer(launch.ReadyInterval),
	}, nil
}

// checkActivation polls /readyz once for act: when its revision names the
// new test render the swap is active; when the ready timeout passes first
// the swap was rejected and the child still serves the active Revision,
// the cause being in its log. It reports whether act is decided.
func (r *runner) checkActivation(ctx context.Context, act *activation) (bool, error) {
	ans, err := r.admin.Readyz(ctx)
	if ctx.Err() != nil {
		return false, r.stop(ctx)
	}
	if err == nil && ans.Revision == act.rend.TestDigest.Short() {
		prev := r.active
		r.active = act.rend
		return true, r.printActive(act.rend, prev)
	}
	if r.clock.Now().Before(act.deadline) {
		return false, nil
	}
	return true, r.printRejected()
}

// stop ends the run on a stop request: it sends the child SIGTERM, a
// second signal a second SIGTERM, and awaits its exit, which the exit
// bound ends with SIGKILL; then it returns the interruption, so the
// command exits 130 after SIGINT and 143 after SIGTERM.
func (r *runner) stop(ctx context.Context) error {
	if r.child != nil {
		r.terminate()
		for !r.child.Exited() {
			select {
			case <-r.child.Done():
			case s := <-r.sigs:
				r.signal(s)
				if r.nsig >= 2 {
					r.terminate()
				}
			}
		}
	}
	return interruptionOf(ctx, r.first)
}

// shutdown stops the child after an error: SIGTERM, a second one on a
// second signal, SIGKILL after the exit bound.
func (r *runner) shutdown() {
	r.terminate()
	for !r.child.Exited() {
		select {
		case <-r.child.Done():
		case s := <-r.sigs:
			r.signal(s)
			r.terminate()
		}
	}
}

// terminate sends the child its next SIGTERM, reporting a failure.
func (r *runner) terminate() {
	if _, err := r.child.Terminate(); err != nil {
		_ = r.errw.Line("ruralz dev run: " + err.Error())
	}
}
