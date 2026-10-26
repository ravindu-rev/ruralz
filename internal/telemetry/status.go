// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/telemetry/aggregate"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// hopSource is the source of cleartext_hop raised while any hop is above 0.
const hopSource = "cleartext_hops"

// nodeStatus implements emit.NodeStatus: ruralz_node_degraded_info is 1
// for a reason while at least one source holds it, and
// ruralz_security_cleartext_hops counts cleartext hops per kind, with
// cleartext_hop raised while any is above 0
// (docs/architecture/10-observability.md "Degraded states"). Raise and
// clear transitions log at WARN and INFO. It is safe for concurrent use;
// callers are off the request path.
type nodeStatus struct {
	log *slog.Logger

	mu       sync.Mutex
	sources  [catalog.NumReasons]map[string]bool
	degraded [catalog.NumReasons]emit.Gauge
	hops     [catalog.NumHops]int
	hopGauge [catalog.NumHops]emit.Gauge
}

// Compile-time check of the contract.
var _ emit.NodeStatus = (*nodeStatus)(nil)

// newNodeStatus resolves the gauges of every reason and hop; they exist at
// 0 from the start.
func newNodeStatus(reg *aggregate.Registry, log *slog.Logger) (*nodeStatus, error) {
	s := &nodeStatus{log: log}
	for r := catalog.Reason(1); r < catalog.NumReasons; r++ {
		g, err := reg.Gauge(catalog.NodeDegradedInfo, r.String())
		if err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		s.degraded[r] = g
		s.sources[r] = map[string]bool{}
	}
	for h := range catalog.NumHops {
		g, err := reg.Gauge(catalog.SecurityCleartextHops, h.String())
		if err != nil {
			return nil, fmt.Errorf("telemetry: %w", err)
		}
		s.hopGauge[h] = g
	}
	return s, nil
}

// SetDegraded raises or clears reason r for source.
func (s *nodeStatus) SetDegraded(r catalog.Reason, source string, on bool) {
	if r == 0 || r >= catalog.NumReasons {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.setLocked(r, source, on)
}

// setLocked is SetDegraded with s.mu held.
func (s *nodeStatus) setLocked(r catalog.Reason, source string, on bool) {
	held := len(s.sources[r]) > 0
	if on {
		s.sources[r][source] = true
	} else {
		delete(s.sources[r], source)
	}
	switch now := len(s.sources[r]) > 0; {
	case now && !held:
		s.degraded[r].Set(1)
		s.log.Warn("degraded state raised", slog.String("reason", r.String()), slog.String("holder", source))
	case !now && held:
		s.degraded[r].Set(0)
		s.log.Info("degraded state cleared", slog.String("reason", r.String()), slog.String("holder", source))
	}
}

// SetCleartextHops sets the count of cleartext hops of kind h.
func (s *nodeStatus) SetCleartextHops(h catalog.Hop, n int) {
	if h >= catalog.NumHops {
		return
	}
	n = max(n, 0)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hops[h] = n
	s.hopGauge[h].Set(int64(n))
	total := 0
	for _, c := range s.hops {
		total += c
	}
	s.setLocked(catalog.ReasonCleartextHop, hopSource, total > 0)
}
