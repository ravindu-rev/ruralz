// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"net/http"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clientaddr"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/redact"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// defaultAPIKeyHeader is auth.api-key's config.header default.
const defaultAPIKeyHeader = "x-api-key"

// snapView is what the handler derives once per snapshot and reads on
// every request pinned to it: the compiled trusted proxies, the Revision's
// display form, the /tap credential set and the _unmatched Route handles.
// It is immutable.
type snapView struct {
	snap      *snapshot.Snapshot
	trusted   *clientaddr.Trusted
	revision  string
	creds     *redact.Set
	unmatched *emit.RouteMetrics
}

// viewOf returns the view of s: the cached one, else a new one, cached
// only when s is the published snapshot, so a request still pinned to a
// retired snapshot never evicts the active snapshot's view.
func (h *Handler) viewOf(s *snapshot.Snapshot) *snapView {
	if v := h.view.Load(); v != nil && v.snap == s {
		return v
	}
	v := &snapView{snap: s, trusted: clientaddr.New(s.Gateway.TrustedProxies), creds: credentialSet(s.Validated), unmatched: &h.noop.route}
	if !s.Revision.Digest.IsZero() {
		v.revision = s.Revision.Digest.Short()
	}
	if s.Metrics != nil {
		if rm := s.Metrics.Route(unmatchedRoute); rm != nil {
			v.unmatched = rm
		}
	}
	if h.snaps.Active() == s {
		h.view.Store(v)
	}
	return v
}

// listenerOf returns the snapshot's listener named name, nil when it has
// none (a request on a listener the Revision removed).
func listenerOf(s *snapshot.Snapshot, name string) *snapshot.Listener {
	for _, l := range s.Listeners {
		if l.Name == name {
			return l
		}
	}
	return nil
}

// credentialSet returns the Revision's credential-header set
// (internal/redact): the fixed credential headers and every auth.api-key
// config.header. The headers upstream-auth Filters set travel only on leg
// requests, which /tap never shows; auth.upstream-oauth2 sets
// Authorization, a fixed one.
func credentialSet(v *hub.Validated) *redact.Set {
	if v == nil || v.Bundle == nil {
		return redact.Default()
	}
	var names []string
	for _, p := range v.Bundle.Policies() {
		if p.Spec.Type != v1alpha1.PolicyTypeAuthAPIKey {
			continue
		}
		name := defaultAPIKeyHeader
		if r, ok := v.Bundle.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: p.Metadata.Name}); ok {
			if c, ok := r.Config.(*v1alpha1.AuthAPIKeyConfig); ok && c != nil && c.Header != nil && *c.Header != "" {
				name = *c.Header
			}
		}
		names = append(names, name)
	}
	return redact.New(names...)
}

// dateCache formats the Node's Date field once per second.
type dateCache struct{ cur atomic.Pointer[dateEntry] }

// dateEntry is the Date field value of one second; v is shared by every
// response of that second and never written.
type dateEntry struct {
	sec int64
	v   []string
}

// get returns the Date field value of now.
func (d *dateCache) get(now time.Time) []string {
	s := now.Unix()
	if e := d.cur.Load(); e != nil && e.sec == s {
		return e.v
	}
	e := &dateEntry{sec: s, v: []string{now.UTC().Format(http.TimeFormat)}}
	d.cur.Store(e)
	return e.v
}

// noopHandles are metric handles that record nothing, for a request whose
// snapshot holds no handles (tests) and a handler without Node metrics.
type noopHandles struct {
	listener emit.ListenerMetrics
	route    emit.RouteMetrics
	node     emit.NodeMetrics
}

// init fills every handle.
func (n *noopHandles) init() {
	c, g, hi := noopCounter{}, noopGauge{}, noopHistogram{}
	n.listener = emit.ListenerMetrics{
		Requests: noopListenerRequests{}, GatewayDuration: hi, RequestBody: hi, ResponseBody: hi,
		Active: g, TLSHandshake: hi,
	}
	for p := range n.listener.OpenConns {
		n.listener.OpenConns[p] = g
		for r := range n.listener.Conns[p] {
			n.listener.Conns[p][r] = c
		}
	}
	n.route = emit.RouteMetrics{Requests: noopStatus{}, Duration: hi}
	for i := range n.route.Cache {
		n.route.Cache[i] = c
	}
	for i := range n.route.Revalidations {
		n.route.Revalidations[i] = c
	}
	n.node = emit.NodeMetrics{
		NodeResponses: noopCodes{}, GatewayDurationSkipped: c, BufferedBytes: g, TapEventsDropped: c,
		AccessLogCELErrors: c,
	}
}

type (
	noopCounter          struct{}
	noopGauge            struct{}
	noopHistogram        struct{}
	noopStatus           struct{}
	noopCodes            struct{}
	noopListenerRequests struct{}
)

func (noopCounter) Add(emit.Stripe, uint64)                             {}
func (noopGauge) Add(emit.Stripe, int64)                                {}
func (noopGauge) Set(int64)                                             {}
func (noopHistogram) Record(emit.Stripe, uint64)                        {}
func (noopHistogram) RecordExemplar(emit.Stripe, uint64, emit.Exemplar) {}
func (noopStatus) Inc(emit.Stripe, int)                                 {}
func (noopCodes) Inc(emit.Stripe, string)                               {}
func (noopListenerRequests) Inc(emit.Stripe, int, int, int)             {}
