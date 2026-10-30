// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package upstream

import (
	"context"
	"errors"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/health"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream/resilience"
)

// lostPingText is the error net/http's HTTP/2 transport (Go 1.26 bundle
// and Go 1.27 internal package alike) gives the streams of a connection it
// closed because a ping went unanswered for healthCheck.pingTimeout.
const lostPingText = "http2: client connection lost"

// classify returns the error kind of an attempt that ended with err before
// response headers, and its cause for passive ejection. Only the attempt,
// leg and Route deadlines (an ended attempt context) give timeout; the
// pool's dialer marks dial failures (connect) and handshake failures (tls)
// so the stage decides when the error alone does not
// (resilience.ClassifyStage).
//
// Causes follow docs/architecture/09-traffic-management-and-resilience.md
// "Health checking and outlier detection": connect errors and the HTTP/2
// ping close bypass the ejection cap (health.CauseConnect), a deadline is
// health.CauseTimeout (an ejection at once on a suspect Endpoint),
// anything else health.CauseOther. A ping close is error kind reset: an
// idempotent request retries it, others get 502 RZ-UP-004.
func classify(actx context.Context, err error) (resilience.Kind, health.Cause) {
	stage := resilience.StageUnknown
	var de *dialError
	var he *handshakeError
	switch {
	case errors.As(err, &de):
		stage = resilience.StageDial
	case errors.As(err, &he):
		stage = resilience.StageTLS
	}
	kind := resilience.ClassifyStage(actx, err, stage)
	switch kind {
	case resilience.KindConnect:
		return kind, health.CauseConnect
	case resilience.KindTimeout:
		return kind, health.CauseTimeout
	case resilience.KindReset:
		if lostPing(err) {
			return kind, health.CauseConnect
		}
		return kind, health.CauseOther
	case resilience.KindNone, resilience.KindTLS:
		return kind, health.CauseOther
	default:
		return kind, health.CauseOther
	}
}

// lostPing reports a stream reset by an HTTP/2 ping close. The error is
// unexported in net/http, so its text is the only handle.
func lostPing(err error) bool {
	return err != nil && strings.Contains(err.Error(), lostPingText)
}
