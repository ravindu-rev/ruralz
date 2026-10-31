// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"net/http"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/telemetry/tracing"
)

// RequestID returns the requestId function of every admin problem
// document (docs/architecture/03-data-plane.md "Admin endpoints"): the
// trace ID of the request's valid traceparent, read by the data plane's
// parser, else a new trace ID that tracer draws as it does for a
// data-plane request without one. The new ID comes from a decision
// without a traceparent at ratio 0, which never samples, so admin requests
// never take from the data plane's sampling caps. The wiring passes the
// one function to adminauth.Options.RequestID, Config.RequestID and
// tap.Config.RequestID.
func RequestID(tracer emit.Tracer) func(*http.Request) string {
	return func(r *http.Request) string {
		// One traceparent field line, as the data plane accepts.
		if vs := r.Header[tracing.HeaderTraceparent]; len(vs) == 1 {
			if id, _, _, ok := tracing.ParseTraceparent(vs[0]); ok {
				return tracing.FormatTraceID(id)
			}
		}
		var d emit.Decision
		tracer.Decide(nil, 0, &d)
		return tracing.FormatTraceID(d.TraceID)
	}
}
