// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// codeSchema is the code of a Route whose upstreams no forwarder can
// serve: no entry, or one Upstream named twice. The configuration model
// rules both out (a Route has one or more Upstreams; upstreams is a map
// keyed by name), so validation normally refuses them first.
const codeSchema = "RZ-CFG-005"

// errUnknownUpstream reports an entry naming an Upstream the snapshot's
// Upstream layer does not hold.
var errUnknownUpstream = errors.New("forward: the snapshot holds no such Upstream")

// upstreamLeg runs one leg to an Upstream; *upstream.Upstream implements it.
type upstreamLeg interface {
	Leg(ctx context.Context, o *snapshot.Outbound, t upstream.Target) (*snapshot.UpstreamResponse, error)
}

// Forwarder is the snapshot.Forwarder of one Route's plain upstreams: one
// leg per request to an entry picked by weight, picking again among the
// entries not tried while legs are refused before any attempt. It is
// immutable and safe for concurrent use; New builds it.
type Forwarder struct {
	split *balance.Split
	// legs are the entries' Upstreams, by entry index.
	legs []upstreamLeg
	src  rand.Source
}

var _ snapshot.Forwarder = (*Forwarder)(nil)

// New returns the Forwarder of the Route named route, whose
// Route.spec.upstreams entries are ups, each resolved in set, the Upstream
// layer of the Route's snapshot. An absent weight is 1. src draws the
// weighted picks and must be safe for concurrent use; nil means the
// math/rand/v2 runtime generator. A Route without entries, or naming one
// Upstream twice, is RZ-CFG-005; an entry naming an Upstream set does not
// hold is an error, since the snapshot compiler compiles every Upstream a
// Route names.
func New(route string, ups []v1alpha1.RouteUpstream, set *upstream.Set, src rand.Source) (*Forwarder, error) {
	if len(ups) == 0 {
		return nil, errcode.Errorf(codeSchema, "Route %q: upstreams has no entry", route)
	}
	legs := make([]upstreamLeg, len(ups))
	weights := make([]uint32, len(ups))
	seen := make(map[string]bool, len(ups))
	for i, e := range ups {
		if seen[e.Name] {
			return nil, errcode.Errorf(codeSchema, "Route %q: upstreams names Upstream %q twice", route, e.Name)
		}
		seen[e.Name] = true
		var u *upstream.Upstream
		ok := false
		if set != nil {
			u, ok = set.Upstream(e.Name)
		}
		if !ok {
			return nil, fmt.Errorf("%w: Route %q names %q", errUnknownUpstream, route, e.Name)
		}
		legs[i] = u
		weights[i] = 1
		if e.Weight != nil {
			weights[i] = uint32(max(*e.Weight, 0))
		}
	}
	return newForwarder(legs, weights, src), nil
}

// newForwarder returns the Forwarder over legs, by entry, with the
// entries' weights.
func newForwarder(legs []upstreamLeg, weights []uint32, src rand.Source) *Forwarder {
	return &Forwarder{split: balance.NewSplit(weights), legs: legs, src: src}
}

// Forward runs the request's leg (snapshot.Forwarder): one entry picked by
// weight, then, while its leg is refused before any attempt (503
// RZ-UP-005, RZ-UP-006 or RZ-UP-008), another picked by weight among the
// entries not tried, each entry at most once; when none is left, the last
// refusal answers. It returns what the last leg returned: a response, the
// Upstream's or a leg Filter's, or a leg error carrying its RZ code
// (*upstream.LegError).
func (f *Forwarder) Forward(ctx context.Context, o *snapshot.Outbound) (*snapshot.UpstreamResponse, error) {
	// New refuses a Route without entries, so the first pick never fails.
	i, _ := f.split.Pick(f.src)
	var tried balance.Tried
	for {
		resp, err := f.legs[i].Leg(ctx, o, upstream.Target{})
		if err == nil || !refused(err) {
			return resp, err
		}
		tried.Add(i)
		next, ok := f.split.PickExcept(f.src, &tried)
		if !ok {
			return nil, err
		}
		i = next
	}
}

// refused reports whether err ends a leg refused before any attempt was
// sent: an upstream.LegError without attempts whose cause is an open
// breaker (503 RZ-UP-005), a full bulkhead and pending queue (503
// RZ-UP-006) or an empty Endpoint set (503 RZ-UP-008). A refused retry
// attempt is not one: its leg sent an attempt and ends with that attempt's
// outcome, and the Route deadline (RZ-RT-007) is no refusal either.
func refused(err error) bool {
	le := legError(err)
	if le == nil || le.Attempts > 0 {
		return false
	}
	return errors.Is(le.Err, resilience.ErrBreakerOpen) ||
		errors.Is(le.Err, resilience.ErrBulkheadFull) ||
		errors.Is(le.Err, resilience.ErrNoEndpoint)
}

// legError returns the *upstream.LegError in err's chain, or nil.
func legError(err error) *upstream.LegError {
	for err != nil {
		if le, ok := err.(*upstream.LegError); ok { //nolint:errorlint // walks the chain by hand: errors.As would allocate on every refusal.
			return le
		}
		err = errors.Unwrap(err)
	}
	return nil
}
