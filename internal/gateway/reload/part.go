// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/gateway/listener"
)

// Part is a Node part an activation readies before the snapshot swap and
// switches right after it: the client listeners (validation gate step
// 11), the admin port and the telemetry settings.
type Part interface {
	// Prepare readies the part for next without changing what the Node
	// serves. An error rejects the Revision with the error's RZ code, such
	// as RZ-CFG-039 for a port that cannot be bound, or RZ-CFG-045 when it
	// carries none; ctx ends at the compile deadline.
	Prepare(ctx context.Context, next Compiled) (Switch, error)
}

// Switch is one Part's prepared change.
type Switch interface {
	// Commit applies the change right after the swap. An error is logged;
	// one with RZ-CFG-039 reports a listener as unbound on /readyz.
	Commit(ctx context.Context) error
	// Abort releases what Prepare readied when the activation ends before
	// the swap, or when a handover successor's held switches are replaced
	// or closed.
	Abort()
}

// Listener is implemented by a Part whose sockets a handover successor
// makes listen once its handover is accepted, beside committing the
// switches its activations hold (Bound.Listen).
type Listener interface {
	Listen(ctx context.Context) error
}

// Stopper is implemented by a Part whose sockets a handover successor
// closes when it exits without serving (Bound.Close).
type Stopper interface {
	StopAccepting()
}

// nopSwitch changes nothing.
type nopSwitch struct{}

func (nopSwitch) Commit(context.Context) error { return nil }
func (nopSwitch) Abort()                       {}

// ListenerPart returns the client listeners as a Part: Prepare binds
// every added or changed listener without listening (listener.Set.Prepare,
// RZ-CFG-039 on a port that cannot be bound) and Commit listens and
// serves on the new sockets right after the swap (listener.Plan.Commit).
// A handover successor's Bound.Close stops the Set accepting.
func ListenerPart(set *listener.Set) Part { return listenerPart{set: set} }

type listenerPart struct{ set *listener.Set }

func (p listenerPart) Prepare(ctx context.Context, next Compiled) (Switch, error) {
	plan, err := p.set.Prepare(ctx, next.Snapshot(), next.Secrets())
	if err != nil {
		return nil, err
	}
	return planSwitch{plan: plan}, nil
}

func (p listenerPart) StopAccepting() { p.set.StopAccepting() }

// planSwitch commits or aborts a listener Plan.
type planSwitch struct{ plan *listener.Plan }

func (s planSwitch) Commit(context.Context) error { return s.plan.Commit() } //nolint:contextcheck // Plan.Commit shuts replaced Servers down in the background, past the activation.
func (s planSwitch) Abort()                       { s.plan.Abort() }
