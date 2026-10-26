// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package balance

import (
	"fmt"
	"strconv"
	"time"
)

// Sizes and limits of the balancer structures (all targets in
// docs/architecture/09-traffic-management-and-resilience.md).
const (
	// MaxScheduleSlots caps a round-robin schedule.
	MaxScheduleSlots = 65536
	// SlotsPerEndpoint is the nominal schedule length per Endpoint.
	SlotsPerEndpoint = 64
	// SlotBytes is the size of one schedule slot, a uint32 Endpoint index.
	SlotBytes = 4
	// MaxRingNodes caps a ring-hash ring.
	MaxRingNodes = 65536
	// VirtualNodeBytes is the size of one ring virtual node: a uint64
	// position, a uint32 Endpoint index and padding.
	VirtualNodeBytes = 16
	// TableBytes is the size of one cumulative weight entry of the random
	// and least-request tables.
	TableBytes = 8
	// MinVirtualNodes is the lowest planned virtual nodes per Endpoint
	// before rings fall back to weighted random.
	MinVirtualNodes = 64
	// MaxVirtualNodes is the highest planned virtual nodes per
	// Endpoint.
	MaxVirtualNodes = 1024
	// DefaultBudget is the balancer memory budget per Node.
	DefaultBudget int64 = 256 << 20
	// CopyOnWriteReserve is kept free for structures being rebuilt while
	// their predecessors still serve.
	CopyOnWriteReserve int64 = 16 << 20
	// RebuildInterval is the shortest time between two rebuilds of one
	// Upstream's structure.
	RebuildInterval = 10 * time.Second
	// MaxConcurrentBuilds bounds the builds running at a time per
	// Node.
	MaxConcurrentBuilds = 8
	// RejectionDraws is how many weighted draws random and least-request
	// make before scanning the eligible Endpoints linearly.
	RejectionDraws = 8
	// FailureWindow is the least-request failure window.
	FailureWindow = time.Second
)

// Algorithm is a loadBalancing.algorithm value.
type Algorithm uint8

// The four M1 algorithms. The zero value is LeastRequest, the
// default.
const (
	// LeastRequest picks the lower score of two weighted random candidates,
	// the score being (in-flight attempts + failures of the last second)
	// per unit of weight.
	LeastRequest Algorithm = iota
	// RoundRobin walks a precomputed smooth weighted schedule.
	RoundRobin
	// RingHash maps a key onto a consistent hash ring.
	RingHash
	// Random draws a weighted random Endpoint.
	Random
)

// String returns the configuration spelling of a.
func (a Algorithm) String() string {
	switch a {
	case LeastRequest:
		return "least-request"
	case RoundRobin:
		return "round-robin"
	case RingHash:
		return "ring-hash"
	case Random:
		return "random"
	}
	return "Algorithm(" + strconv.Itoa(int(a)) + ")"
}

// Valid reports whether a is one of the four algorithms.
func (a Algorithm) Valid() bool { return a <= Random }

// ParseAlgorithm maps a loadBalancing.algorithm value to an Algorithm. The
// empty string is the default, least-request.
func ParseAlgorithm(s string) (Algorithm, error) {
	switch s {
	case "", "least-request":
		return LeastRequest, nil
	case "round-robin":
		return RoundRobin, nil
	case "ring-hash":
		return RingHash, nil
	case "random":
		return Random, nil
	}
	return 0, fmt.Errorf("balance: unknown load-balancing algorithm %q", s)
}
