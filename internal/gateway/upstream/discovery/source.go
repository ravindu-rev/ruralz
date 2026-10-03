// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"math/rand/v2"
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Refresh timing and bounds (05 reqs 5 to 7; target values unless noted).
const (
	// RefreshInterval is the period between good answers (05 reqs 5, 6).
	RefreshInterval = 30 * time.Second
	// RefreshJitter is the ±10% spread around RefreshInterval (05 req 6):
	// the next refresh after a good answer is due 27 s to 33 s later.
	RefreshJitter = RefreshInterval / 10
	// BackoffMin and BackoffMax bound the full-jitter backoff after a
	// failed refresh (05 req 7): the n-th consecutive failure waits a
	// uniform delay from BackoffMin to min(BackoffMax, BackoffMin × 2^n).
	BackoffMin = time.Second
	// BackoffMax is the longest delay after a failure.
	BackoffMax = 60 * time.Second
	// NotFoundLimit is the number of consecutive refreshes that must repeat
	// NXDOMAIN or an empty answer before the set empties (05 req 7).
	NotFoundLimit = 3
	// DefaultLookupTimeout bounds one DNS lookup (proposed: the resolver's
	// own retries stay within it).
	DefaultLookupTimeout = 5 * time.Second
	// MaxConcurrentLookups bounds the parallel host lookups of one refresh.
	MaxConcurrentLookups = 8
)

// errLookupTimeout is the cancel cause of a lookup that ran out of time.
var errLookupTimeout = errors.New("discovery: lookup timed out")

// Options configures a Source.
type Options struct {
	// Upstream is the Upstream's metadata.name. The Source holds the
	// discovery_stale Node reason under a source of its own that starts
	// with it, so the Sources of one Upstream that coexist across a Hot
	// Reload raise and clear the reason independently.
	Upstream string
	// Clock schedules refreshes and lookup timeouts; nil means clock.Real().
	Clock clock.Clock
	// Resolver performs lookups; nil means NewResolver().
	Resolver Resolver
	// Rand draws jitter; nil means the math/rand/v2 global source. It is
	// only used under the Source's refresh lock.
	Rand rand.Source
	// Status receives the discovery_stale Node degraded reason; nil
	// disables it.
	Status emit.NodeStatus
	// Metrics are Upstream handles the Source reports discovery_stale to
	// from the start: those of the active snapshot, when the Source serves
	// it already. A Source compiled for a Revision that is not active yet
	// leaves it nil and gets its handles from SetMetrics at activation, so
	// the Source of a refused Revision never writes the active snapshot's
	// gauge. New itself writes no gauge.
	Metrics *emit.UpstreamMetrics
	// LookupTimeout bounds each lookup; 0 means DefaultLookupTimeout.
	LookupTimeout time.Duration
	// Previous is the current Set of the Source this one replaces after a
	// Hot Reload changed the Upstream's source. Host names both sources
	// resolve start from its last good addresses, and an unchanged DNS
	// source starts from its Endpoints, so a reload during a DNS outage
	// keeps the last good answer (05 req 5). Activating this Source
	// (SetMetrics) also stops the Source that published Previous from
	// writing the Upstream gauge, whose one series this Source then owns.
	Previous *Set
}

// Source owns the Endpoint set of one Upstream. It is safe for concurrent
// use: Current never blocks, Refresh calls are serialized, and SetMetrics
// and Close never wait for a lookup in flight.
type Source struct {
	spec          Spec
	key           string
	holder        string // the discovery_stale source on the Node gauge
	clk           clock.Clock
	base          time.Time // clk reading at New; due is measured from it
	res           Resolver
	rnd           rand.Source
	status        emit.NodeStatus
	lookupTimeout time.Duration
	static        []staticEntry
	dynamic       bool

	cur atomic.Pointer[Set]

	// refresh serializes Refresh and guards the refresh state below; it is
	// held across lookups.
	refresh     sync.Mutex
	lastGood    map[string][]netip.Addr
	notFound    int
	failures    int
	version     uint64
	lastRefresh time.Time
	lastErr     error

	// mu guards the degraded state, its reporting and the schedule; it is
	// never held across a lookup, so activation (SetMetrics) and teardown
	// (Close) do not wait for DNS.
	mu      sync.Mutex
	metrics *emit.UpstreamMetrics // nil: the Source writes no Upstream gauge
	prev    *Source               // the Source that published Options.Previous, until activation
	stale   bool
	closed  bool
	// scheduled is set once a refresh completed; due is when the next one
	// is, in nanoseconds of clk.Since(base), so Run follows the delay the
	// last Refresh returned whoever called it.
	scheduled bool
	due       int64
}

// New returns a Source for spec. It performs no lookup and writes no
// gauge: the Upstream layer calls Refresh at compile time (05 req 5) and
// then runs Run, which waits for the delay that Refresh returned before
// its own first lookup.
func New(spec Spec, o Options) (*Source, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	s := &Source{
		spec:          spec,
		key:           spec.key(),
		clk:           o.Clock,
		res:           o.Resolver,
		rnd:           o.Rand,
		status:        o.Status,
		lookupTimeout: o.LookupTimeout,
		lastGood:      map[string][]netip.Addr{},
	}
	if s.clk == nil {
		s.clk = clock.Real()
	}
	s.base = s.clk.Now()
	// The address of s tells coexisting Sources of one Upstream apart for
	// as long as both live, without package state.
	s.holder = fmt.Sprintf("%s#%p", o.Upstream, s)
	if s.res == nil {
		s.res = NewResolver()
	}
	if s.rnd == nil {
		s.rnd = globalSource{}
	}
	if s.lookupTimeout <= 0 {
		s.lookupTimeout = DefaultLookupTimeout
	}
	s.metrics = o.Metrics
	if o.Previous != nil && o.Previous.src != s {
		s.prev = o.Previous.src
	}
	s.dynamic = spec.DNS != nil
	for _, e := range spec.Static {
		se, err := parseStatic(e)
		if err != nil {
			return nil, err
		}
		s.static = append(s.static, se)
		s.dynamic = s.dynamic || se.host != ""
	}
	first := &Set{Status: Status{Type: spec.Type()}, key: s.key, src: s}
	s.seed(o.Previous)
	switch {
	case spec.DNS != nil && o.Previous != nil && o.Previous.key == s.key:
		first.Endpoints = o.Previous.Endpoints
	case spec.DNS == nil:
		first.Endpoints = normalize(s.staticEndpoints())
	}
	if len(first.Endpoints) > 0 {
		s.version = 1
		first.Version = 1
	}
	s.cur.Store(first)
	return s, nil
}

// seed records the last good addresses of every named Endpoint of prev.
func (s *Source) seed(prev *Set) {
	if prev == nil {
		return
	}
	for i := range prev.Endpoints {
		e := &prev.Endpoints[i]
		if e.Host == "" || len(e.Addrs) == 0 {
			continue
		}
		addrs := s.lastGood[e.Host]
		for _, ap := range e.Addrs {
			if a := ap.Addr(); !slices.Contains(addrs, a) {
				addrs = append(addrs, a)
			}
		}
		s.lastGood[e.Host] = addrs
	}
}

// Current returns the published Set; it never blocks and never returns nil.
func (s *Source) Current() *Set { return s.cur.Load() }

// Spec returns the source's configuration.
func (s *Source) Spec() Spec { return s.spec }

// Dynamic reports whether the source has names to resolve; a source of
// IP literals only never needs a refresh.
func (s *Source) Dynamic() bool { return s.dynamic }

// SetMetrics switches the Upstream handles to those of a newly activated
// snapshot and sets its discovery_stale gauge to the current state
// (enumerated label values exist at 0 from the start, 05 req 94). The
// first call also stops the Source this one replaces (Options.Previous)
// from writing the gauge: the Upstream has one series, which the
// activated Source owns. A closed Source ignores the call.
func (s *Source) SetMetrics(m *emit.UpstreamMetrics) {
	s.mu.Lock()
	prev := s.prev
	s.prev = nil
	s.mu.Unlock()
	// The replaced Source stops first, so none of its reports can land
	// after the state this one sets below.
	if prev != nil {
		prev.supersede()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.metrics = m
	s.reportGauge(s.stale)
}

// supersede stops s from writing the Upstream gauge: the Source that
// replaced it was activated and owns the series now. s keeps holding its
// own Node reason until it recovers or closes.
func (s *Source) supersede() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = nil
}

// Close clears the discovery_stale Node reason this Source holds; it never
// waits for a refresh in flight, whose outcome no longer raises the
// reason. It leaves the Upstream gauge alone: the series belongs to the
// Source that replaced this one (its SetMetrics sets it), or ends with the
// snapshot's label sets (Binding.Release). The owner stops Run (by
// canceling its context) before or after; later Refresh calls return the
// current Set without lookups.
func (s *Source) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	s.closed = true
	if s.stale && s.status != nil {
		s.status.SetDegraded(catalog.ReasonDiscoveryStale, s.holder, false)
	}
	s.stale = false
	s.metrics = nil
	s.prev = nil
}

// isClosed reports whether Close ran.
func (s *Source) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// Run refreshes until ctx ends, calling onChange (when non-nil) on the
// caller's goroutine after every refresh that changed the Endpoints; it
// returns when ctx ends. Each refresh waits for the delay the previous one
// returned, including a Refresh the caller ran before Run (the compile-time
// refresh, 05 req 5), so Run never repeats a lookup early; with no refresh
// yet, Run refreshes at once. A source without names to resolve, or a
// closed one, just waits for ctx.
func (s *Source) Run(ctx context.Context, onChange func(*Set)) {
	seen := s.Current().Version
	for ctx.Err() == nil {
		wait, ok := s.untilDue()
		if !ok {
			<-ctx.Done()
			return
		}
		if wait > 0 {
			// Sleep fails only when ctx ends, which ends the loop. A Refresh
			// during the sleep may have moved the due time: check again.
			if s.clk.Sleep(ctx, wait) != nil {
				return
			}
			continue
		}
		set, _ := s.Refresh(ctx)
		if ctx.Err() != nil {
			return
		}
		if onChange != nil && set.Version != seen {
			seen = set.Version
			onChange(set)
		}
	}
}

// untilDue returns the time left until the next refresh is due (0 or less:
// now), and false when none ever is: nothing to resolve, or closed.
func (s *Source) untilDue() (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dynamic || s.closed {
		return 0, false
	}
	if !s.scheduled {
		return 0, true
	}
	return time.Duration(s.due - s.elapsed()), true
}

// elapsed returns the time since New on the Source's clock (monotonic on
// clock.Real), so the schedule ignores wall-clock steps.
func (s *Source) elapsed() int64 { return int64(s.clk.Since(s.base)) }

// answer is the outcome of one refresh's lookups.
type answer struct {
	endpoints []Endpoint
	publish   bool  // endpoints replace the current ones
	err       error // the refresh failed (the set may still be published: partial)
	notFound  bool  // err is NXDOMAIN or an empty answer for the whole source
}

// Refresh resolves the source once, publishes the resulting Set and
// returns it with the delay until the next refresh is due: RefreshInterval
// ±RefreshJitter after a good answer, the backoff after a failure and
// while an accepted NXDOMAIN or empty answer keeps the set empty, 0 when
// the source has nothing to resolve or is closed. A failure keeps the last
// good Endpoints and raises discovery_stale; a good answer clears it (05
// req 7). When ctx ends during the lookups nothing changes and the delay
// is 0. Run waits for the returned delay before its next refresh.
func (s *Source) Refresh(ctx context.Context) (*Set, time.Duration) {
	s.refresh.Lock()
	defer s.refresh.Unlock()
	if !s.dynamic || s.isClosed() {
		return s.cur.Load(), 0
	}
	var a answer
	switch {
	case s.spec.DNS != nil && s.spec.DNS.PortName != "":
		a = s.resolveSRV(ctx)
	case s.spec.DNS != nil:
		a = s.resolveIP(ctx)
	default:
		a = s.resolveStatic(ctx)
	}
	if ctx.Err() != nil {
		return s.cur.Load(), 0
	}
	return s.apply(a)
}

// apply publishes the outcome a and returns the next delay (0 once the
// Source is closed); s.refresh is held.
func (s *Source) apply(a answer) (*Set, time.Duration) {
	cur := s.cur.Load()
	now := s.clk.Now()
	accepted := false
	if a.notFound {
		s.notFound++
		if s.notFound >= NotFoundLimit {
			// The repeated answer is accepted: the set empties (05 req 7).
			a = answer{publish: true}
			accepted = true
		}
	} else {
		s.notFound = 0
	}
	eps := cur.Endpoints
	if a.publish {
		eps = normalize(a.endpoints)
		if !sameEndpoints(eps, cur.Endpoints) {
			s.version++
		} else {
			eps = cur.Endpoints
		}
	}
	var next time.Duration
	if a.err != nil {
		s.failures++
		s.lastErr = a.err
		next = s.backoff(s.failures)
	} else {
		s.failures = 0
		s.lastErr = nil
		s.lastRefresh = now
		next = s.interval()
		if accepted {
			// The name may come back any moment: while the accepted answer
			// keeps the set empty, retries stay on the failure backoff, its
			// ceiling still growing with the streak (05 req 7), and the 30 s
			// schedule resumes after the next non-empty answer.
			next = s.backoff(s.notFound)
		}
	}
	set := &Set{
		Endpoints: eps,
		Version:   s.version,
		Status: Status{
			Type:        s.spec.Type(),
			LastRefresh: s.lastRefresh,
			Stale:       a.err != nil,
			Failures:    s.failures,
			Err:         s.lastErr,
		},
		key: s.key,
		src: s,
	}
	s.cur.Store(set)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return set, 0
	}
	s.setStaleLocked(a.err != nil)
	s.scheduled = true
	s.due = s.elapsed() + int64(next)
	return set, next
}

// resolveStatic re-resolves the static host names; a failed name keeps its
// last good addresses (05 req 5). Any failure marks the refresh failed.
func (s *Source) resolveStatic(ctx context.Context) answer {
	var hosts []string
	for _, e := range s.static {
		if e.host != "" && !slices.Contains(hosts, e.host) {
			hosts = append(hosts, e.host)
		}
	}
	results := s.lookupHosts(ctx, hosts)
	var firstErr error
	good := make(map[string][]netip.Addr, len(hosts))
	for i, h := range hosts {
		r := results[i]
		if r.err == nil {
			good[h] = r.addrs
			continue
		}
		if firstErr == nil {
			firstErr = r.err
		}
		if last, ok := s.lastGood[h]; ok {
			good[h] = last
		}
	}
	s.lastGood = good
	return answer{endpoints: s.staticEndpoints(), publish: true, err: firstErr}
}

// staticEndpoints builds the static Endpoints from the last good addresses.
func (s *Source) staticEndpoints() []Endpoint {
	out := make([]Endpoint, 0, len(s.static))
	for _, e := range s.static {
		ep := Endpoint{Identity: e.identity, Host: e.host, Port: e.port, Weight: e.weight}
		if e.host == "" {
			ep.Addrs = []netip.AddrPort{netip.AddrPortFrom(e.addr, e.port)}
		} else {
			ep.Addrs = withPort(s.lastGood[e.host], e.port)
		}
		out = append(out, ep)
	}
	return out
}

// resolveIP looks up A and AAAA records of the service: each IP is one
// Endpoint ip:port of weight 1 (05 req 6).
func (s *Source) resolveIP(ctx context.Context) answer {
	d := s.spec.DNS
	r := s.lookupHost(ctx, d.Service)
	if r.err != nil {
		return answer{err: r.err, notFound: notFound(r.err)}
	}
	eps := make([]Endpoint, 0, len(r.addrs))
	for _, a := range r.addrs {
		ap := netip.AddrPortFrom(a, d.Port)
		eps = append(eps, Endpoint{
			Identity: ap.String(),
			Host:     trimDot(d.Service),
			Port:     d.Port,
			Addrs:    []netip.AddrPort{ap},
			Weight:   1,
		})
	}
	return answer{endpoints: eps, publish: true}
}

// resolveSRV looks up _<port>._tcp.<service>, keeps the lowest-priority
// group with the weight rule of 05 req 6, and resolves each target like a
// static host name. A target that fails keeps its last good addresses, or
// is left out when it has none; the refresh then counts as failed but
// still publishes the other targets. When no target has an address the
// whole refresh fails and the last set stays.
func (s *Source) resolveSRV(ctx context.Context) answer {
	d := s.spec.DNS
	lctx, cancel := s.lookupContext(ctx)
	_, records, err := s.res.LookupSRV(lctx, d.PortName, "tcp", d.Service)
	cancel()
	if err != nil && malformedOnly(err, records) {
		// The resolver dropped records with invalid target names and
		// returned the others: a good answer, whose dropped records are
		// ignored like every other unusable record (selectSRV).
		err = nil
	}
	if err == nil && len(records) == 0 {
		err = errEmptyAnswer
	}
	if err != nil {
		err = fmt.Errorf("discovery: SRV _%s._tcp.%s: %w", d.PortName, d.Service, err)
		return answer{err: err, notFound: notFound(err)}
	}
	targets := selectSRV(records)
	if len(targets) == 0 {
		err = fmt.Errorf("discovery: SRV _%s._tcp.%s: no usable target: %w", d.PortName, d.Service, errEmptyAnswer)
		return answer{err: err, notFound: true}
	}
	var hosts []string
	for _, t := range targets {
		if !slices.Contains(hosts, t.host) {
			hosts = append(hosts, t.host)
		}
	}
	results := s.lookupHosts(ctx, hosts)
	var firstErr error
	good := make(map[string][]netip.Addr, len(hosts))
	for i, h := range hosts {
		r := results[i]
		if r.err == nil {
			good[h] = r.addrs
			continue
		}
		if firstErr == nil {
			firstErr = r.err
		}
		if last, ok := s.lastGood[h]; ok {
			good[h] = last
		}
	}
	eps := make([]Endpoint, 0, len(targets))
	for _, t := range targets {
		addrs, ok := good[t.host]
		if !ok {
			continue
		}
		eps = append(eps, Endpoint{
			Identity: t.identity,
			Host:     t.host,
			Port:     t.port,
			Addrs:    withPort(addrs, t.port),
			Weight:   t.weight,
		})
	}
	if len(eps) == 0 {
		return answer{err: firstErr}
	}
	s.lastGood = good
	return answer{endpoints: eps, publish: true, err: firstErr}
}

// hostResult is the answer for one host name.
type hostResult struct {
	addrs []netip.Addr
	err   error
}

// lookupHosts resolves hosts with at most MaxConcurrentLookups lookups in
// flight, joining them before it returns; results follow hosts' order.
func (s *Source) lookupHosts(ctx context.Context, hosts []string) []hostResult {
	results := make([]hostResult, len(hosts))
	sem := make(chan struct{}, MaxConcurrentLookups)
	var wg sync.WaitGroup
	for i, h := range hosts {
		if a, err := netip.ParseAddr(h); err == nil {
			results[i] = hostResult{addrs: []netip.Addr{a.Unmap()}}
			continue
		}
		sem <- struct{}{}
		wg.Go(func() {
			defer func() { <-sem }()
			results[i] = s.lookupHost(ctx, h)
		})
	}
	wg.Wait()
	return results
}

// lookupHost resolves one name; an empty answer is errEmptyAnswer.
func (s *Source) lookupHost(ctx context.Context, host string) hostResult {
	lctx, cancel := s.lookupContext(ctx)
	answer, err := s.res.LookupIPAddr(lctx, host)
	cancel()
	if err == nil {
		if addrs := ipsOf(answer); len(addrs) > 0 {
			return hostResult{addrs: addrs}
		}
		err = errEmptyAnswer
	}
	return hostResult{err: fmt.Errorf("discovery: resolve %s: %w", host, err)}
}

// lookupContext bounds one lookup by the lookup timeout on the Source's
// clock.
func (s *Source) lookupContext(ctx context.Context) (context.Context, func()) {
	lctx, cancel := context.WithCancelCause(ctx)
	t := s.clk.AfterFunc(s.lookupTimeout, func() { cancel(errLookupTimeout) })
	return lctx, func() {
		t.Stop()
		cancel(nil)
	}
}

// interval returns RefreshInterval ± RefreshJitter, uniformly (05 req 6).
func (s *Source) interval() time.Duration {
	return RefreshInterval - RefreshJitter + uniform(s.rnd, 2*RefreshJitter)
}

// backoff returns the delay after the n-th consecutive failure: uniform
// from BackoffMin to min(BackoffMax, BackoffMin × 2^n) (05 req 7).
func (s *Source) backoff(n int) time.Duration {
	return Backoff(s.rnd, n)
}

// Backoff returns the full-jitter delay after the n-th (n ≥ 1) consecutive
// failed refresh: uniform from BackoffMin to min(BackoffMax, BackoffMin ×
// 2^n), so every delay lies in [1 s, 60 s] (05 req 7).
func Backoff(src rand.Source, n int) time.Duration {
	ceiling := BackoffMax
	if n < 6 {
		ceiling = min(BackoffMax, BackoffMin<<max(n, 1))
	}
	return BackoffMin + uniform(src, ceiling-BackoffMin)
}

// uniform returns a duration uniformly drawn from [0, d].
func uniform(src rand.Source, d time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	hi, _ := bits.Mul64(src.Uint64(), uint64(d)+1)
	return time.Duration(hi) //nolint:gosec // G115: hi ≤ d, a positive Duration.
}

// setStaleLocked raises or clears discovery_stale on the Node gauge,
// under this Source's own holder, and on the Upstream gauge when the
// Source has handles, whenever its state changes; s.mu is held, so
// reports never reorder.
func (s *Source) setStaleLocked(on bool) {
	if on == s.stale {
		return
	}
	s.stale = on
	if s.status != nil {
		s.status.SetDegraded(catalog.ReasonDiscoveryStale, s.holder, on)
	}
	s.reportGauge(on)
}

// reportGauge sets ruralz_upstream_degraded_info{reason="discovery_stale"}
// when the Source has handles; s.mu is held.
func (s *Source) reportGauge(on bool) {
	m := s.metrics
	if m == nil || m.Degraded[emit.UpDegradedDiscoveryStale] == nil {
		return
	}
	var v int64
	if on {
		v = 1
	}
	m.Degraded[emit.UpDegradedDiscoveryStale].Set(v)
}

func trimDot(n string) string {
	if len(n) > 1 && n[len(n)-1] == '.' {
		return n[:len(n)-1]
	}
	return n
}

// globalSource draws from the math/rand/v2 global generator, which is safe
// for concurrent use.
type globalSource struct{}

// Uint64 implements rand.Source.
func (globalSource) Uint64() uint64 {
	return rand.Uint64() //nolint:gosec // G404: refresh jitter needs a uniform draw, not an unpredictable one.
}
