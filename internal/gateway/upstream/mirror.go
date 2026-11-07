// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/balance"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
	"github.com/ravindu-rev/ruralz/internal/routematch"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// errMirrorExpired is the error of a mirror leg whose Route deadline
// passed before it was sent.
var errMirrorExpired = errors.New("upstream: the Route deadline passed before the mirror leg was sent")

var _ snapshot.MirrorLegs = (*Set)(nil)

// MirrorKey implements snapshot.MirrorLegs: the ring-hash key of a mirror
// leg to the named Upstream, its loadBalancing.hashKey over vars, the
// request's Base variables, while they are live; the zero key for an
// Upstream of another algorithm or one the snapshot does not hold. A
// runtime error picks a weighted random Endpoint, as it does for a leg,
// and counts in ruralz_upstream_cel_errors_total{field="hashKey"}.
func (s *Set) MirrorKey(upstream string, vars *expr.Vars, stripe emit.Stripe) snapshot.MirrorKey {
	u, ok := s.ups[upstream]
	if !ok || u.alg != balance.RingHash || u.hashKey == nil {
		return snapshot.MirrorKey{}
	}
	k, err := u.hashKey.EvalString(vars)
	if err != nil {
		u.metrics.CELErrors[emit.CELHashKey].Add(stripe, 1)
		return snapshot.MirrorKey{Random: true}
	}
	return snapshot.MirrorKey{Key: balance.HashKey(k)}
}

// Mirror implements snapshot.MirrorLegs: one mirror leg to the named
// Upstream (Upstream.mirror); an Upstream the snapshot does not hold is
// filter.ErrNotAvailable.
func (s *Set) Mirror(ctx context.Context, upstream string, c *snapshot.MirrorCopy) error {
	u, ok := s.ups[upstream]
	if !ok {
		return unknownUpstream(upstream, filter.ErrNotAvailable)
	}
	return u.mirror(ctx, c)
}

// mirror sends one mirror leg to u on the caller's goroutine
// (docs/architecture/09-traffic-management-and-resilience.md "Mirroring"):
// c's request, as the leg's first attempt sent it, to an Endpoint u's
// balancer picks, with u's connection pools, health tracking, breaker and
// bulkhead, and none of u's Policies and no retry. Its bounds are those of
// a leg's only attempt: the leg deadline (the Route deadline, or u's
// timeout from now when sooner) and retries.perTryTimeout within it, to
// the response headers. It takes a bulkhead slot only when one is free and
// no attempt waits (Bulkhead.TryAcquire), so a mirror never queues or
// takes the pending room of u's own attempts, and it counts in no retry
// budget. The response body is read up to resilience.RetryDrainBytes,
// within ctx, and discarded.
//
// The attempt counts as any attempt of u does: in
// ruralz_upstream_attempts_total and its duration histogram, through
// failureWhen in passive ejection and in the breaker, its connection in
// ruralz_upstream_pool_connections. It starts no span and leaves the
// trace context fields as the attempt it copies sent them, so the mirror
// Upstream's spans join the request's trace under that attempt. An attempt
// whose ctx ended without a deadline, a Drain or a snapshot retirement
// ending it, is no Upstream failure.
//
// It returns nil once response headers arrived, whatever their status,
// else the gate's error (RZ-UP-008, RZ-UP-005, RZ-UP-006), an expired
// Route deadline or the attempt's error.
func (u *Upstream) mirror(ctx context.Context, c *snapshot.MirrorCopy) error {
	rt, m := u.rt, u.rt.m
	set := u.bal.bal.Load()
	tab, _ := set.Payload().(*table)
	if tab == nil || set.Len() == 0 {
		return resilience.GateNoEndpoint.Err()
	}
	now := m.clk.Now()
	legDeadline := resilience.LegDeadline(c.Deadline, now, u.res.Timeout)
	if !legDeadline.IsZero() && !now.Before(legDeadline) {
		return errMirrorExpired
	}
	br := rt.res.Breaker()
	permit, ok := br.AllowLease(&u.res.Breaker, legDeadline)
	if !ok {
		return resilience.GateBreaker.Err()
	}
	result := resilience.LegNotCounted
	defer func() { br.Record(&u.res.Breaker, permit, result) }()
	if !rt.res.Bulkhead().TryAcquire(&u.res.Bulkhead) {
		return resilience.GateBulkhead.Err()
	}
	defer rt.res.Bulkhead().Release()
	view := attemptView{tab: tab, hv: rt.tracker.View(now), maxConns: u.res.Bulkhead.MaxConnections}
	var (
		ch  balance.Choice
		err error
	)
	if c.Key.Random {
		ch, err = set.SelectRandom(m.rand, &view)
	} else {
		ch, err = set.Select(c.Key.Key, m.rand, &view)
	}
	if err != nil {
		return resilience.GateNoEndpoint.Err()
	}
	ep := tab.eps[ch.Index].st
	perTry := resilience.PerTryTimeout(u.res.Retry.PerTryTimeout, legDeadline, now, 0)
	deadline := resilience.AttemptDeadline(legDeadline, now, perTry)
	cause := resilience.ErrAttemptTimeout
	if !legDeadline.IsZero() && deadline.Equal(legDeadline) {
		cause = resilience.ErrLegTimeout
	}
	actx, dl := resilience.WithDeadline(ctx, m.clk, deadline, cause)
	defer dl.Cancel()
	n := ep.net.Load()
	r := c.Request
	r.URL.Scheme, r.URL.Host, r.Host = u.scheme, n.authority, ""
	// GotConn runs on this goroutine, during RoundTrip; a transport retry
	// on another connection moves the count.
	var conn *trackedConn
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		tc := trackedOf(info.Conn)
		if tc == nil || !tc.begin() {
			return
		}
		if conn != nil {
			conn.end()
		}
		conn = tc
	}}
	r = r.WithContext(httptrace.WithClientTrace(actx, trace))
	ep.inflight.Add(1)
	defer ep.inflight.Add(-1)
	pl := rt.poolFor(ep, u.tls)
	defer pl.done()
	var ha health.Attempt
	start := m.clk.Now()
	tracked := false
	if j, ok := view.health(ch.Index); ok {
		tracked = view.hv.Begin(&ha, j, start)
	}
	resp, err := pl.send.RoundTrip(r)
	defer func() {
		if conn != nil {
			conn.end()
		}
	}()
	vars := mirrorVars(r, ep.identity, u.name)
	if err != nil {
		kind, hcause := classify(actx, err)
		if resilience.Canceled(actx) {
			if tracked {
				ha.Cancel()
			}
			return fmt.Errorf("upstream: mirror leg canceled: %w", err)
		}
		vars.Error = &expr.AttemptError{Kind: kind.String()}
		failed := u.mirrorFailureWhen(vars, c.Stripe)
		if tracked {
			ha.End(m.clk.Now(), health.Outcome{Failed: failed, Cause: hcause, Passive: u.passive, Stripe: c.Stripe})
		}
		u.metrics.Attempts.Inc(c.Stripe, 0, int(kind))
		u.metrics.AttemptDuration.Record(c.Stripe, uint64(max(m.clk.Now().Sub(start), 0)))
		o := resilience.Outcome{Kind: kind, Attempts: 1}
		ejected, total := 0, 0
		if kind == resilience.KindConnect {
			healthy, all := rt.tracker.Healthy(m.clk.Now())
			ejected, total = all-healthy, all
		}
		result = resilience.BreakerResult(failed, o, ejected, total)
		return fmt.Errorf("upstream: mirror leg: %w", err)
	}
	// Headers arrived: the attempt and leg deadlines stop applying, and the
	// body is read under ctx alone.
	dl.Stop()
	if tracked {
		ha.Headers(m.clk.Now())
	}
	vars.Response = &expr.Response{Status: resp.StatusCode, Header: resp.Header}
	failed := u.mirrorFailureWhen(vars, c.Stripe)
	if tracked {
		ha.End(m.clk.Now(), health.Outcome{Failed: failed, Passive: u.passive, Stripe: c.Stripe})
	}
	u.metrics.Attempts.Inc(c.Stripe, resp.StatusCode, emit.ErrNone)
	result = resilience.BreakerResult(failed, resilience.Outcome{Responded: true, Attempts: 1}, 0, 0)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, resilience.RetryDrainBytes))
	_ = resp.Body.Close()
	u.metrics.AttemptDuration.Record(c.Stripe, uint64(max(m.clk.Now().Sub(start), 0)))
	return nil
}

// mirrorFailureWhen evaluates circuitBreaker.failureWhen for a mirror leg;
// a runtime error counts as a failure and in
// ruralz_upstream_cel_errors_total.
func (u *Upstream) mirrorFailureWhen(v *expr.Vars, stripe emit.Stripe) bool {
	failed, err := resilience.EvalFailureWhen(u.res.Breaker.FailureWhen, v)
	if err != nil {
		u.metrics.CELErrors[emit.CELFailureWhen].Add(stripe, 1)
	}
	return failed
}

// mirrorVars returns the activation of a mirror leg's failureWhen: the
// leg's outgoing request (no body), as the Upstream fields read the
// attempt's request, and the upstream, its name and the Endpoint picked.
func mirrorVars(r *http.Request, endpoint, name string) *expr.Vars {
	req := &expr.Request{Method: r.Method, Scheme: r.URL.Scheme, Host: routematch.NormalizeHost(r.URL.Host), Path: routematch.RequestPath(r.URL), Header: r.Header}
	req.SetRawQuery(r.URL.RawQuery)
	return &expr.Vars{Request: req, Upstream: &expr.Upstream{Name: name, Endpoint: endpoint}}
}
