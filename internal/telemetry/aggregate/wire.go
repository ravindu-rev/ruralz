// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// The wire functions map a family's handles onto the emit metric structs;
// the enumeration index of each array matches the catalog label order
// (emit constants, R-56).

func wireListener(m *emit.ListenerMetrics, name string, hs handleSet) {
	switch name {
	case catalog.HTTPListenerRequestsTotal:
		m.Requests = hs.listenerReq()
	case catalog.HTTPGatewayDurationSeconds:
		m.GatewayDuration = hs.hist(0)
	case catalog.HTTPRequestBodyBytes:
		m.RequestBody = hs.hist(0)
	case catalog.HTTPResponseBodyBytes:
		m.ResponseBody = hs.hist(0)
	case catalog.HTTPActiveRequests:
		m.Active = hs.gauge(0)
	case catalog.ListenerOpenConnections:
		for p := range m.OpenConns {
			m.OpenConns[p] = hs.gauge(p)
		}
	case catalog.ListenerConnectionsTotal:
		for p := range m.Conns {
			for r := range m.Conns[p] {
				m.Conns[p][r] = hs.counter(p*emit.NumConnResults + r)
			}
		}
	case catalog.ListenerTLSHandshakeDurationSeconds:
		m.TLSHandshake = hs.hist(0)
	}
}

func wireRoute(m *emit.RouteMetrics, name string, hs handleSet) {
	switch name {
	case catalog.HTTPRequestsTotal:
		m.Requests = hs.status()
	case catalog.HTTPRequestDurationSeconds:
		m.Duration = hs.hist(0)
	case catalog.CacheRequestsTotal:
		for i := range m.Cache {
			m.Cache[i] = hs.counter(i)
		}
	}
}

func wireUpstream(m *emit.UpstreamMetrics, name string, hs handleSet) {
	switch name {
	case catalog.UpstreamAttemptsTotal:
		m.Attempts = hs.attempts()
	case catalog.UpstreamAttemptDurationSeconds:
		m.AttemptDuration = hs.hist(0)
	case catalog.UpstreamRetriesTotal:
		m.Retries = hs.counter(0)
	case catalog.UpstreamRetryBudgetExhaustedTotal:
		m.RetryBudgetExhausted = hs.counter(0)
	case catalog.UpstreamBreakerStateInfo:
		for i := range m.BreakerState {
			m.BreakerState[i] = hs.gauge(i)
		}
	case catalog.UpstreamEjectionsTotal:
		for i := range m.Ejections {
			m.Ejections[i] = hs.counter(i)
		}
	case catalog.UpstreamHealthyEndpoints:
		m.HealthyEndpoints = hs.gauge(0)
	case catalog.UpstreamProbesSkippedTotal:
		m.ProbesSkipped = hs.counter(0)
	case catalog.UpstreamDegradedInfo:
		for i := range m.Degraded {
			m.Degraded[i] = hs.gauge(i)
		}
	case catalog.UpstreamCELErrorsTotal:
		for i := range m.CELErrors {
			m.CELErrors[i] = hs.counter(i)
		}
	case catalog.UpstreamPoolConnections:
		for i := range m.PoolConnections {
			m.PoolConnections[i] = hs.gauge(i)
		}
	}
}

func wirePolicy(m *emit.PolicyMetrics, name string, ph int8, hs handleSet) {
	switch name {
	case catalog.FilterDurationSeconds:
		m.Duration[ph] = hs.hist(0)
	case catalog.FilterShortCircuitsTotal:
		m.ShortCircuits[ph] = hs.status()
	case catalog.FilterFailuresTotal:
		for k := range m.Failures[ph] {
			m.Failures[ph][k] = hs.counter(k)
		}
	case catalog.AuthDecisionsTotal:
		m.Auth = hs.auth()
	case catalog.AuthJWKSAgeSeconds:
		m.JWKSAge = hs.gauge(0)
	case catalog.AuthUpstreamTokenAgeSeconds:
		m.UpstreamTokenAge = hs.gauge(0)
	case catalog.AuthUpstreamRefreshFailuresTotal:
		m.UpstreamRefreshes = hs.counter(0)
	case catalog.RateLimitDecisionsTotal:
		for i := range m.RateLimit {
			m.RateLimit[i] = hs.counter(i)
		}
	case catalog.RateLimitBucketEvictionsTotal:
		m.BucketEvictions = hs.counter(0)
	case catalog.QuotaDecisionsTotal:
		for i := range m.Quota {
			m.Quota[i] = hs.counter(i)
		}
	case catalog.CacheStoreSkippedTotal:
		for i := range m.StoreSkipped {
			m.StoreSkipped[i] = hs.counter(i)
		}
	}
}
