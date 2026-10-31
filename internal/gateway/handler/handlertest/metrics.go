// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handlertest

import (
	"slices"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Counter is a readable emit.Counter.
type Counter struct{ n atomic.Int64 }

// Add implements emit.Counter.
func (c *Counter) Add(_ emit.Stripe, n uint64) { c.n.Add(int64(n)) } //nolint:gosec // G115: test counts stay small.

// Value returns the count.
func (c *Counter) Value() int64 { return c.n.Load() }

// Gauge is a readable emit.Gauge.
type Gauge struct{ v atomic.Int64 }

// Add implements emit.Gauge.
func (g *Gauge) Add(_ emit.Stripe, d int64) { g.v.Add(d) }

// Set implements emit.Gauge.
func (g *Gauge) Set(v int64) { g.v.Store(v) }

// Value returns the value.
func (g *Gauge) Value() int64 { return g.v.Load() }

// Histogram is a readable emit.Histogram keeping every value and exemplar.
type Histogram struct {
	mu        sync.Mutex
	values    []uint64
	exemplars []emit.Exemplar
}

// Record implements emit.Histogram.
func (h *Histogram) Record(_ emit.Stripe, v uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.values = append(h.values, v)
}

// RecordExemplar implements emit.Histogram.
func (h *Histogram) RecordExemplar(s emit.Stripe, v uint64, ex emit.Exemplar) {
	h.Record(s, v)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.exemplars = append(h.exemplars, ex)
}

// Values returns the recorded values.
func (h *Histogram) Values() []uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.values)
}

// Exemplars returns the recorded exemplars.
func (h *Histogram) Exemplars() []emit.Exemplar {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.exemplars)
}

// StatusCounter is a readable emit.StatusCounter keeping each status.
type StatusCounter struct {
	mu       sync.Mutex
	statuses []int
}

// Inc implements emit.StatusCounter.
func (c *StatusCounter) Inc(_ emit.Stripe, status int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.statuses = append(c.statuses, status)
}

// Statuses returns the counted statuses in order.
func (c *StatusCounter) Statuses() []int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.statuses)
}

// CodeCounter is a readable emit.CodeCounter keeping each code.
type CodeCounter struct {
	mu    sync.Mutex
	codes []string
}

// Inc implements emit.CodeCounter.
func (c *CodeCounter) Inc(_ emit.Stripe, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.codes = append(c.codes, code)
}

// Codes returns the counted codes in order.
func (c *CodeCounter) Codes() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.codes)
}

// ListenerRequest is one ruralz_http_listener_requests_total increment.
type ListenerRequest struct {
	Protocol, Status, Origin int
}

// ListenerRequests is a readable emit.ListenerRequests.
type ListenerRequests struct {
	mu     sync.Mutex
	events []ListenerRequest
}

// Inc implements emit.ListenerRequests.
func (l *ListenerRequests) Inc(_ emit.Stripe, protocol, status, origin int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, ListenerRequest{Protocol: protocol, Status: status, Origin: origin})
}

// Events returns the increments in order.
func (l *ListenerRequests) Events() []ListenerRequest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.events)
}

// Listener is a listener's readable handles; M is what a snapshot holds.
type Listener struct {
	M            *emit.ListenerMetrics
	Requests     *ListenerRequests
	Gateway      *Histogram
	RequestBody  *Histogram
	ResponseBody *Histogram
	Active       *Gauge
}

// NewListener returns readable listener handles.
func NewListener() *Listener {
	l := &Listener{Requests: &ListenerRequests{}, Gateway: &Histogram{}, RequestBody: &Histogram{}, ResponseBody: &Histogram{}, Active: &Gauge{}}
	l.M = &emit.ListenerMetrics{
		Requests: l.Requests, GatewayDuration: l.Gateway, RequestBody: l.RequestBody, ResponseBody: l.ResponseBody,
		Active: l.Active, TLSHandshake: &Histogram{},
	}
	for p := range l.M.OpenConns {
		l.M.OpenConns[p] = &Gauge{}
		for r := range l.M.Conns[p] {
			l.M.Conns[p][r] = &Counter{}
		}
	}
	return l
}

// RouteHandles are a Route's readable handles; M is what a snapshot holds.
type RouteHandles struct {
	M        *emit.RouteMetrics
	Requests *StatusCounter
	Duration *Histogram
	Cache    [emit.NumCacheResults]*Counter
}

// NewRoute returns readable Route handles.
func NewRoute() *RouteHandles {
	r := &RouteHandles{Requests: &StatusCounter{}, Duration: &Histogram{}}
	r.M = &emit.RouteMetrics{Requests: r.Requests, Duration: r.Duration}
	for i := range r.Cache {
		r.Cache[i] = &Counter{}
		r.M.Cache[i] = r.Cache[i]
	}
	for i := range r.M.Revalidations {
		r.M.Revalidations[i] = &Counter{}
	}
	return r
}

// Node are the Node-wide readable handles; M is what the handler takes.
type Node struct {
	M         *emit.NodeMetrics
	Responses *CodeCounter
	Skipped   *Counter
	CELErrors *Counter
}

// NewNode returns readable Node handles.
func NewNode() *Node {
	n := &Node{Responses: &CodeCounter{}, Skipped: &Counter{}, CELErrors: &Counter{}}
	n.M = &emit.NodeMetrics{
		NodeResponses: n.Responses, GatewayDurationSkipped: n.Skipped, BufferedBytes: &Gauge{},
		TapEventsDropped: &Counter{}, AccessLogCELErrors: n.CELErrors,
	}
	return n
}

// Plan is an emit.Plan over readable handles, created on first use; its
// Route("_unmatched") is the requests no Route matched.
type Plan struct {
	mu        sync.Mutex
	listeners map[string]*Listener
	routes    map[string]*RouteHandles
}

// NewPlan returns an empty Plan.
func NewPlan() *Plan {
	return &Plan{listeners: map[string]*Listener{}, routes: map[string]*RouteHandles{}}
}

// Listener implements emit.Plan.
func (p *Plan) Listener(name string) *emit.ListenerMetrics { return p.ListenerHandles(name).M }

// Route implements emit.Plan.
func (p *Plan) Route(name string) *emit.RouteMetrics { return p.RouteHandles(name).M }

// Upstream implements emit.Plan; no handle is readable.
func (*Plan) Upstream(string) *emit.UpstreamMetrics { return nil }

// Policy implements emit.Plan; no handle is readable.
func (*Plan) Policy(string) *emit.PolicyMetrics { return nil }

// ListenerHandles returns the readable handles of listener name.
func (p *Plan) ListenerHandles(name string) *Listener {
	p.mu.Lock()
	defer p.mu.Unlock()
	l := p.listeners[name]
	if l == nil {
		l = NewListener()
		p.listeners[name] = l
	}
	return l
}

// RouteHandles returns the readable handles of Route name.
func (p *Plan) RouteHandles(name string) *RouteHandles {
	p.mu.Lock()
	defer p.mu.Unlock()
	r := p.routes[name]
	if r == nil {
		r = NewRoute()
		p.routes[name] = r
	}
	return r
}
