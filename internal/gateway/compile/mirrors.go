// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"cmp"
	"slices"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/gateway/redirect"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/mirror"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// mirrorKey identifies one Route mirror across snapshots.
type mirrorKey struct{ route, upstream string }

// inFlightOf returns the in-flight counters of the previous snapshot's
// mirrors, by Route and Upstream, which the mirrors of the same Route and
// Upstream carry over, so each mirror's maxInFlight bounds the Node across
// Hot Reloads (docs/architecture/09-traffic-management-and-resilience.md
// "Mirroring").
func inFlightOf(prev *snapshot.Snapshot) map[mirrorKey]*atomic.Int64 {
	out := map[mirrorKey]*atomic.Int64{}
	if prev == nil {
		return out
	}
	for _, rt := range prev.Routes {
		if rt == nil {
			continue
		}
		for _, m := range rt.Mirrors {
			out[mirrorKey{route: rt.Name, upstream: m.Upstream}] = m.InFlight
		}
	}
	return out
}

// mirrorsOf returns the compiled mirrors of rt, a Route with upstreams, in
// Upstream name order: each with its percent and maxInFlight (the schema
// defaults that stage G materialized, else mirror.DefaultPercent and
// mirror.DefaultMaxInFlight), the in-flight counter of the previous
// snapshot's mirror of the same Route and Upstream or a new one, and its
// ruralz_upstream_mirrors_total handles.
func (b *build) mirrorsOf(rt *v1alpha1.Route, prev map[mirrorKey]*atomic.Int64) []*snapshot.Mirror {
	if len(rt.Spec.Mirrors) == 0 {
		return nil
	}
	ms := slices.SortedFunc(slices.Values(rt.Spec.Mirrors), func(a, c v1alpha1.RouteMirror) int { return cmp.Compare(a.Upstream, c.Upstream) })
	out := make([]*snapshot.Mirror, 0, len(ms))
	for _, m := range ms {
		percent, maxInFlight := int32(mirror.DefaultPercent), int32(mirror.DefaultMaxInFlight)
		if m.Percent != nil {
			percent = min(max(*m.Percent, 0), 100)
		}
		if m.MaxInFlight != nil {
			maxInFlight = max(*m.MaxInFlight, 1)
		}
		c := prev[mirrorKey{route: rt.Metadata.Name, upstream: m.Upstream}]
		if c == nil {
			c = new(atomic.Int64)
		}
		out = append(out, &snapshot.Mirror{
			Upstream: m.Upstream, Percent: uint32(percent), MaxInFlight: int64(maxInFlight), InFlight: c,
			Metrics: b.plan.Mirror(rt.Metadata.Name, m.Upstream),
		})
	}
	return out
}

// redirectOf compiles rt's redirect, its locationExpression at the site
// stage J checked it at.
func (b *build) redirectOf(rt *v1alpha1.Route) (snapshot.Redirector, error) {
	r, err := redirect.Compile(rt, b.builder, pipeline.RouteSite(rt, expr.PlaceRedirectLocation, ""))
	if err != nil {
		return nil, err
	}
	return r, nil
}
