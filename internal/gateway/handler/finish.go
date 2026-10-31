// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package handler

import (
	"context"
	"crypto/tls"
	"log/slog"
	"net"
	"net/netip"
	"time"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/tap"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// upstreamClock is the Excluder the Upstream layer marks its I/O on: it
// excludes each section from the request's gateway-added time and also
// measures their union on its own, the access record's upstream_duration.
type upstreamClock struct {
	gw *emit.GatewayTimer
	up emit.GatewayTimer
}

// Enter opens an Upstream I/O section on both timers.
func (u *upstreamClock) Enter(now time.Time) {
	u.gw.Enter(now)
	u.up.Enter(now)
}

// Leave closes it on both.
func (u *upstreamClock) Leave(now time.Time) {
	u.gw.Leave(now)
	u.up.Leave(now)
}

// spent returns the union of the Upstream I/O sections of a request that
// ended at end; 0 for a clock anomaly.
func (u *upstreamClock) spent(start, end time.Time) time.Duration {
	d, ok := u.up.Result(end)
	if !ok {
		return 0
	}
	return end.Sub(start) - d
}

// finish ends the request after its response: the outcome Final reports,
// onLog and every Finish (on a context that the client's departure does
// not cancel), the request metrics, the access record, the /tap event and
// the server span (docs/architecture/03-data-plane.md "Phases";
// docs/architecture/10-observability.md "Access logs").
func (st *state) finish(ctx context.Context) {
	h, x := st.h, st.x
	end := h.clk.Now()
	dur := end.Sub(st.start)
	lctx := context.WithoutCancel(ctx)
	status := st.status
	if !st.committed {
		// The client went away before any response.
		status = 0
	}
	x.SetFinal(filter.Final{Status: status, Code: st.code, Committed: st.committed, UpstreamReached: st.reached.Load()}, dur)
	if st.run != nil {
		st.run.Log(lctx)
	}
	if st.gwEnd.IsZero() {
		st.gwEnd = end
	}
	gw, gwOK := x.Timer().Observe(st.gwEnd, st.stripe, st.lm.GatewayDuration, h.node.GatewayDurationSkipped)
	st.count(status, end, dur)
	st.accessRecord(lctx, status, dur, end, gw, gwOK)
	st.publishTap(status, dur)
	st.endSpan(status)
}

// count records the request metrics: ruralz_http_requests_total and
// ruralz_http_request_duration_seconds on the Route (or _unmatched),
// ruralz_http_listener_requests_total with the response's origin,
// ruralz_http_node_responses_total for a Node-generated response with a
// code, and the body sizes. A request whose client went away before any
// response has no status to count.
func (st *state) count(status int, end time.Time, dur time.Duration) {
	h, s := st.h, st.stripe
	rm := st.view.unmatched
	if st.route != nil && st.route.Metrics != nil {
		rm = st.route.Metrics
	}
	if status != 0 {
		rm.Requests.Inc(s, status)
		st.lm.Requests.Inc(s, protoIndex(st.r), status, emit.OriginOf(st.code, st.fromUpstream))
	}
	if d := uint64(max(dur, 0)); st.decision.Sampled {
		rm.Duration.RecordExemplar(s, d, emit.Exemplar{TraceID: st.decision.TraceID, SpanID: st.decision.ServerSpanID, Time: end})
	} else {
		rm.Duration.Record(s, d)
	}
	if st.generated && st.code != "" {
		h.node.NodeResponses.Inc(s, st.code)
	}
	st.lm.RequestBody.Record(s, uint64(max(st.x.RequestBody().BytesRead(), 0)))
	st.lm.ResponseBody.Record(s, uint64(max(st.respBytes, 0)))
}

// accessRecord writes the access record when the snapshot's
// Gateway.spec.telemetry.accessLog.when selects it, over the onLog
// variables (Base, response, upstream, duration); a runtime error writes it
// and counts ruralz_telemetry_access_log_cel_errors_total. The writer
// copies every field at Submit, so the record never references the
// request's memory.
func (st *state) accessRecord(ctx context.Context, status int, dur time.Duration, end time.Time, gw time.Duration, gwOK bool) {
	h, x := st.h, st.x
	if h.alog == nil {
		return
	}
	if when := st.snap.Gateway.AccessLogWhen; when != nil {
		x.Enter(nil, phase.OnLog, nil)
		ok, err := when.EvalBool(ctx, x.Exchange().Vars())
		switch {
		case err != nil:
			h.node.AccessLogCELErrors.Add(st.stripe, 1)
		case !ok:
			return
		}
	}
	rec := h.alog.Acquire()
	x.FillAccess(rec)
	rec.SpanID, rec.Sampled = st.decision.ServerSpanID, st.decision.Sampled
	rec.Revision = st.view.revision
	rec.Protocol = protoLabel(st.r)
	rec.Status, rec.Code = status, st.code
	rec.Duration = dur
	rec.RequestBytes, rec.ResponseBytes = x.RequestBody().BytesRead(), st.respBytes
	rec.GatewayDuration, rec.GatewayDurationSkipped = gw, !gwOK
	rec.UpstreamDuration = st.up.spent(st.start, end)
	rec.ClientAddress = addrText(st.src, st.r.RemoteAddr)
	rec.TLSVersion = tlsVersion(st.r.TLS)
	rec.Upstream, rec.Endpoint, rec.Attempts = st.upstream, st.endpoint, st.attempts
	h.alog.Submit(rec)
}

// publishTap reports the finished exchange to /tap when a subscriber
// listens (tap.Hub.Active is one atomic load).
func (st *state) publishTap(status int, dur time.Duration) {
	h, x := st.h, st.x
	if !h.tap.Active() {
		return
	}
	route := unmatchedRoute
	if st.route != nil {
		route = st.route.Name
	}
	consumer := ""
	if id := x.Exchange().Identity(); id != nil && id.Consumer != nil {
		consumer = id.Consumer.Name
	}
	st.ev = tap.Event{
		Time: st.start, TraceID: st.decision.TraceID, Listener: st.listenerName, Protocol: protoLabel(st.r),
		Route: route, Method: st.r.Method, Host: x.Exchange().Host(), Path: st.path,
		Status: status, Code: st.code, Duration: dur,
		RequestBytes: x.RequestBody().BytesRead(), ResponseBytes: st.respBytes,
		Upstream: st.upstream, Endpoint: st.endpoint, Consumer: consumer,
		RequestHeaders: st.r.Header, ResponseHeaders: st.w.Header(), Credentials: st.view.creds,
	}
	h.tap.Publish(st.stripe, &st.ev)
	st.ev = tap.Event{}
}

// endSpan ends the server span with the Revision, the Consumer and Tier
// when authenticated, the status and the RZ code.
func (st *state) endSpan(status int) {
	s := st.span
	if s == nil || st.spanEnded {
		return
	}
	st.spanEnded = true
	if st.view.revision != "" {
		s.SetAttr(catalog.AttrRevision, slog.StringValue(st.view.revision))
	}
	if id := st.x.Exchange().Identity(); id != nil && id.Consumer != nil {
		s.SetAttr(catalog.AttrConsumer, slog.StringValue(id.Consumer.Name))
		if t := id.Consumer.Tier; t != "" {
			s.SetAttr(catalog.AttrTier, slog.StringValue(t))
		}
	}
	s.End(status, st.code, "")
}

// addrText returns source.ip as the access record writes it, the address
// without a port, "" when unknown; for the connection peer it is the
// address part of r.RemoteAddr, a string net/http already holds.
func addrText(src filter.Source, remote string) string {
	if !src.IP.IsValid() {
		return ""
	}
	if !src.FromHeader {
		if host, _, err := net.SplitHostPort(remote); err == nil {
			if a, err := netip.ParseAddr(host); err == nil && a == src.IP {
				return host
			}
		}
	}
	return src.IP.String()
}

// tlsVersion returns the access record's tls_version: 1.2 or 1.3, as CEL
// source.tlsVersion reads it, "" on cleartext.
func tlsVersion(s *tls.ConnectionState) string {
	if s == nil {
		return ""
	}
	switch s.Version {
	case tls.VersionTLS13:
		return "1.3"
	case tls.VersionTLS12:
		return "1.2"
	default:
		return ""
	}
}
