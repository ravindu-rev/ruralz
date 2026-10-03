// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Defaults and fixed values (05 req 4 table and reqs 18 to 22; target
// values unless noted).
const (
	// DefaultConsecutiveErrors is healthCheck.passive.consecutiveErrors.
	DefaultConsecutiveErrors = 5
	// DefaultEjectionTime is healthCheck.passive.ejectionTime.
	DefaultEjectionTime = 30 * time.Second
	// MaxEjectionMultiplier caps the ejection count that multiplies
	// ejectionTime (05 req 18).
	MaxEjectionMultiplier = 10
	// MaxEjectionPercent is the share of Endpoints that may be passively
	// ejected at once, rounded down (05 req 19).
	MaxEjectionPercent = 50

	// DefaultProbePath is healthCheck.active.path (proposed).
	DefaultProbePath = "/"
	// DefaultProbeInterval is healthCheck.active.interval (proposed).
	DefaultProbeInterval = 10 * time.Second
	// DefaultProbeTimeout is healthCheck.active.timeout (proposed).
	DefaultProbeTimeout = 2 * time.Second
	// DefaultHealthyThreshold is healthCheck.active.healthyThreshold
	// (proposed).
	DefaultHealthyThreshold = 2
	// DefaultUnhealthyThreshold is healthCheck.active.unhealthyThreshold
	// (proposed).
	DefaultUnhealthyThreshold = 3
	// ProbeJitterPercent spreads probes of one Endpoint over interval ±10%
	// (05 req 20).
	ProbeJitterPercent = 10
	// MaxProbeSlots caps the probes in flight per Node (05 req 20).
	MaxProbeSlots = 256
	// ProbesSkippedPercent is the skipped share over ProbesSkippedWindow
	// above which probes_skipped is raised (05 req 20).
	ProbesSkippedPercent = 10
	// ProbesSkippedWindow is the rolling window of that share.
	ProbesSkippedWindow = time.Minute

	// StallAfter is how long an attempt may go without response headers
	// before it is stalled (05 req 21).
	StallAfter = time.Second
	// SuspectAfter is how long an Endpoint holding a stalled attempt may
	// deliver no response headers before it is suspect (05 req 21).
	SuspectAfter = time.Second
	// StalledSharePercent is the share of maxConnections held by stalled
	// attempts from which every Endpoint holding one is avoided (05 req
	// 21).
	StalledSharePercent = 50
	// DefaultMaxConnections is circuitBreaker.maxConnections (05 req 4).
	DefaultMaxConnections = 1024

	// PingAfter is HTTP2Config.SendPingTimeout for Upstream connections:
	// a connection silent this long is pinged (05 req 22).
	PingAfter = time.Second
	// PingTimeout is HTTP2Config.PingTimeout: an unanswered ping closes
	// the connection and resets its streams (05 req 22).
	PingTimeout = 5 * time.Second

	// FailureWindow is the span of the recent-failure count least-request
	// adds to an Endpoint's load (05 req 13).
	FailureWindow = time.Second
)

// PassivePolicy is healthCheck.passive of one snapshot. Zero fields take
// their defaults.
type PassivePolicy struct {
	// ConsecutiveErrors is the run of attempts matching failureWhen that
	// ejects an Endpoint.
	ConsecutiveErrors int
	// EjectionTime is multiplied by the Endpoint's ejection count.
	EjectionTime time.Duration
}

// WithDefaults returns p with zero or negative fields set to their
// defaults.
func (p PassivePolicy) WithDefaults() PassivePolicy {
	if p.ConsecutiveErrors <= 0 {
		p.ConsecutiveErrors = DefaultConsecutiveErrors
	}
	if p.EjectionTime <= 0 {
		p.EjectionTime = DefaultEjectionTime
	}
	return p
}

// ActivePolicy is healthCheck.active of one snapshot; its presence turns
// probing on. Zero fields take their defaults.
type ActivePolicy struct {
	// Path is the probe request path.
	Path string
	// Interval is the time between probes of one Endpoint.
	Interval time.Duration
	// Timeout bounds one probe.
	Timeout time.Duration
	// HealthyThreshold consecutive successes mark an Endpoint healthy.
	HealthyThreshold int
	// UnhealthyThreshold consecutive failures mark it unhealthy.
	UnhealthyThreshold int
}

// WithDefaults returns p with empty or non-positive fields set to their
// defaults.
func (p ActivePolicy) WithDefaults() ActivePolicy {
	if p.Path == "" {
		p.Path = DefaultProbePath
	}
	if p.Interval <= 0 {
		p.Interval = DefaultProbeInterval
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultProbeTimeout
	}
	if p.HealthyThreshold <= 0 {
		p.HealthyThreshold = DefaultHealthyThreshold
	}
	if p.UnhealthyThreshold <= 0 {
		p.UnhealthyThreshold = DefaultUnhealthyThreshold
	}
	return p
}

// Cause is what ended an attempt, as far as the ejection cap cares.
type Cause uint8

// Causes. The Upstream layer maps its error kinds onto them.
const (
	// CauseNone: response headers arrived.
	CauseNone Cause = iota
	// CauseConnect: a dial error or dial timeout, or a reset caused by an
	// HTTP/2 ping close (05 reqs 19 and 22); it bypasses the ejection cap.
	CauseConnect
	// CauseTimeout: the attempt, leg or Route deadline expired; on a
	// suspect Endpoint it ejects at once, bypassing the cap (05 req 21).
	CauseTimeout
	// CauseOther: any other error (reset, TLS).
	CauseOther
)

// Outcome is the result of one attempt for passive ejection.
type Outcome struct {
	// Failed reports that the attempt matched failureWhen (a failureWhen
	// runtime error counts as a failure, 05 req 32).
	Failed bool
	// Cause classifies an attempt that got no response.
	Cause Cause
	// Passive is the thresholds of the attempt's own snapshot (05 req 2:
	// in-flight attempts keep their snapshot's settings); the zero value
	// means the Tracker's current policy.
	Passive PassivePolicy
	// Stripe is the request's counter stripe.
	Stripe emit.Stripe
}
