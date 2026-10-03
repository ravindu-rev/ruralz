// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Provider produces one Upstream's Endpoint set for the Upstream layer.
// *Source implements it for static endpoints and discovery.type dns;
// kubernetes discovery (M2, spec 05 section 8) adds an implementation fed
// by EndpointSlices behind the same methods, so the Upstream runtime needs
// no change.
type Provider interface {
	// Current returns the published Set; it never blocks and never
	// returns nil.
	Current() *Set
	// Run keeps the Set up to date until ctx ends, calling onChange (when
	// non-nil) after every change of the Endpoints.
	Run(ctx context.Context, onChange func(*Set))
	// SetMetrics switches to a newly activated snapshot's Upstream handles.
	SetMetrics(m *emit.UpstreamMetrics)
	// Close releases the degraded reasons the Provider holds.
	Close()
}
