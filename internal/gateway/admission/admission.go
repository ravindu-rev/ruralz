// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package admission bounds what a Node admits before any routing work: the
// in-flight units every request (and every parallel composition step and
// stream pump) holds, the pre-routing responses written at once, and the
// client connections open across all client listeners (spec 04 reqs 11, 19
// and 20; docs/architecture/03-data-plane.md "Bounded resources"). During
// an in-place handover each ceiling is lowered by the usage the other
// process reports (spec 04 req 67; "In-place handover" in
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md).
//
// Every ceiling is fixed in M1 (OQ-data-plane-6 (a)): no adaptive limiter.
// The request path uses only atomics; the connection limiter blocks the
// accept loop (never a request goroutine) behind a mutex.
package admission

import (
	"errors"
	"time"

	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// Ceilings (all target values of docs/architecture/03-data-plane.md
// "Bounded resources").
const (
	// DefaultUnits is the Node in-flight unit ceiling (spec 04 req 19).
	DefaultUnits = 20_000
	// DefaultPreRouting is the number of pre-routing responses written at
	// once (spec 04 req 20).
	DefaultPreRouting = 2_000
	// DefaultConnections is the Node-wide client connection ceiling across
	// all client listeners (spec 04 req 11).
	DefaultConnections = 20_000
	// AdminConnections is the admin listener's own connection ceiling; the
	// admin listener is not counted against DefaultConnections (spec 04
	// req 11).
	AdminConnections = 256
	// PreRoutingWriteTimeout is the write deadline of a pre-routing
	// response (spec 04 req 20).
	PreRoutingWriteTimeout = 5 * time.Second
)

// CodeFull is the RZ code of a request rejected because the in-flight
// ceiling is full: 503, written at once and never queued (spec 04 req 19).
const CodeFull = "RZ-RT-005"

// ErrFull reports a full in-flight ceiling; errcode.CodeOf returns CodeFull.
// Callers that take units for parallel composition steps or stream pumps
// return it (spec 05 req 43).
var ErrFull = errcode.Wrap(CodeFull, errors.New("admission: in-flight ceiling is full"))

// effective returns limit lowered by external, never below zero.
func effective(limit, external int64) int64 {
	return max(0, limit-max(0, external))
}
