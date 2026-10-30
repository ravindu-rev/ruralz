// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package forward

import (
	"cmp"
	"encoding/json"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// StatusSource reports the Node's Upstream runtimes for /debug/upstreams;
// *upstream.Manager implements it.
type StatusSource interface {
	Status() []upstream.Status
}

var _ StatusSource = (*upstream.Manager)(nil)

// Debug returns the /debug/upstreams body of st, the runtimes
// upstream.Manager.Status reports: one entry per runtime a live snapshot
// holds (upstream.Status.Live), in name order, so none before the first
// activation (docs/architecture/03-data-plane.md "Admin endpoints"). Times
// are RFC 3339 UTC, or nil (null) when unset; a refresh error is its text,
// nil without one; lists are empty, never nil.
func Debug(st []upstream.Status) adminapi.Upstreams {
	out := adminapi.Upstreams{Upstreams: make([]adminapi.UpstreamInfo, 0, len(st))}
	for i := range st {
		if st[i].Live {
			out.Upstreams = append(out.Upstreams, upstreamInfo(&st[i]))
		}
	}
	slices.SortStableFunc(out.Upstreams, func(a, b adminapi.UpstreamInfo) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// upstreamInfo converts the state of one runtime.
func upstreamInfo(s *upstream.Status) adminapi.UpstreamInfo {
	u := adminapi.UpstreamInfo{
		Name: s.Name,
		Source: adminapi.SourceInfo{
			Type:        s.Source.Type,
			LastRefresh: stamp(s.Source.LastRefresh),
			Stale:       s.Source.Stale,
			Failures:    s.Source.Failures,
			Error:       errText(s.Source.Err),
		},
		Breaker: adminapi.BreakerInfo{
			State:               breakerState(s.Breaker.State),
			OpenedAt:            stamp(s.Breaker.OpenedAt),
			OpenUntil:           stamp(s.Breaker.OpenUntil),
			WindowLegs:          s.Breaker.WindowLegs,
			WindowFailures:      s.Breaker.WindowFailures,
			ConsecutiveFailures: s.Breaker.ConsecutiveFailures,
		},
		InFlight:    s.InFlight,
		Pending:     s.Pending,
		RetryBudget: adminapi.RetryBudgetInfo{Originals: s.RetryOriginals, Retries: s.RetryRetries},
		Panic:       s.Panic,
		Balancers:   make([]adminapi.BalancerInfo, 0, len(s.Balancers)),
		Endpoints:   make([]adminapi.EndpointInfo, 0, len(s.Endpoints)),
	}
	for _, b := range s.Balancers {
		u.Balancers = append(u.Balancers, adminapi.BalancerInfo{
			Algorithm:    b.Algorithm.String(),
			Effective:    b.Effective.String(),
			VirtualNodes: b.VirtualNodes,
			Fallback:     b.Fallback,
			Bytes:        b.Bytes,
			Endpoints:    b.Endpoints,
			Built:        stamp(b.Built),
			Stale:        b.Stale,
		})
	}
	for _, e := range s.Endpoints {
		u.Endpoints = append(u.Endpoints, adminapi.EndpointInfo{
			Identity:          e.Identity,
			Weight:            e.Weight,
			Healthy:           e.Healthy,
			Ejected:           e.Ejected,
			EjectedUntil:      stamp(e.EjectedUntil),
			Ejections:         e.Ejections,
			ConsecutiveErrors: e.ConsecutiveErrors,
			Suspect:           e.Suspect,
			Stalled:           e.Stalled,
			Failures1s:        e.Failures1s,
			InFlight:          e.InFlight,
			Addresses:         append(make([]string, 0, len(e.Addresses)), e.Addresses...),
		})
	}
	return u
}

// breakerState returns the body's name of a breaker state.
func breakerState(s resilience.State) string {
	switch s {
	case resilience.StateOpen:
		return adminapi.BreakerOpen
	case resilience.StateHalfOpen:
		return adminapi.BreakerHalfOpen
	default:
		return adminapi.BreakerClosed
	}
}

// stamp formats t in RFC 3339 UTC; nil for the zero time.
func stamp(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := t.UTC().Format(time.RFC3339Nano)
	return &s
}

// errText returns err's text; nil without an error.
func errText(err error) *string {
	if err == nil {
		return nil
	}
	s := err.Error()
	return &s
}

// DebugHandler returns the /debug/upstreams handler: 200 with the Debug
// body of src's runtimes as application/json, never cached, like every
// admin JSON body. The admin server authenticates the request, matches the
// path and refuses methods other than GET before the handler runs.
func DebugHandler(src StatusSource) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, err := json.Marshal(Debug(src.Status()))
		if err != nil {
			// adminapi.Upstreams holds strings, integers, booleans, string
			// pointers and slices of them; encoding cannot fail.
			panic(err)
		}
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}
