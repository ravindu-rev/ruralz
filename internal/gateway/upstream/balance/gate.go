// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"context"
	"fmt"
)

// BuildGate bounds the balancer builds running at a time on a Node (05 req
// 16: at most 8). The Upstream manager shares one across its Balancers and
// passes it to Rebuild.
type BuildGate struct {
	slots chan struct{}
}

// NewBuildGate returns a gate of n slots (n ≤ 0: MaxConcurrentBuilds).
func NewBuildGate(n int) *BuildGate {
	if n <= 0 {
		n = MaxConcurrentBuilds
	}
	return &BuildGate{slots: make(chan struct{}, n)}
}

// Acquire takes a slot, waiting until one frees or ctx ends.
func (g *BuildGate) Acquire(ctx context.Context) error {
	select {
	case g.slots <- struct{}{}:
		return nil
	default:
	}
	select {
	case g.slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("balance: waiting for a build slot: %w", ctx.Err())
	}
}

// TryAcquire takes a slot if one is free.
func (g *BuildGate) TryAcquire() bool {
	select {
	case g.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

// Release returns a slot taken by Acquire or TryAcquire. Each successful
// Acquire or TryAcquire must be paired with exactly one Release by the same
// builder: slots are anonymous, so an unpaired Release frees a slot another
// builder still holds and lets one more build run at a time. On a gate with
// no slot taken, Release does nothing.
func (g *BuildGate) Release() {
	select {
	case <-g.slots:
	default:
	}
}

// InUse returns the slots taken.
func (g *BuildGate) InUse() int { return len(g.slots) }
