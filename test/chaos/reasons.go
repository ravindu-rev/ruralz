// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// DegradedMetric is the degraded-state gauge
// (docs/architecture/10-observability.md "Degraded states").
const DegradedMetric = catalog.NodeDegradedInfo

// Reasons returns the degraded reasons a Node raised in its last
// successful sample before real instant t, sorted; ok is false when no
// sample precedes t.
func (s *Sampler) Reasons(node string, t time.Time) (reasons []string, ok bool) {
	smps := s.Samples(node)
	for i := len(smps) - 1; i >= 0; i-- {
		smp := smps[i]
		if !smp.Real.Before(t) || smp.Err != nil || smp.values == nil {
			continue
		}
		s.mu.Lock()
		for id, v := range smp.values {
			if sr := s.series[id]; sr.Name == DegradedMetric && v > 0 {
				reasons = append(reasons, sr.Labels["reason"])
			}
		}
		s.mu.Unlock()
		slices.Sort(reasons)
		return reasons, true
	}
	return nil, false
}

// ReasonTrace returns a Node's degraded gauge for reason at every
// successful sample.
func (s *Sampler) ReasonTrace(node, reason string) []Point {
	return s.Trace(node, DegradedMetric, promtext.L("reason", reason))
}

// NewReasons returns, per Node of nodes, the degraded reasons raised at
// real instant end but not at real instant before: the reasons a fault
// raised that did not clear.
func (s *Sampler) NewReasons(nodes []string, before, end time.Time) map[string][]string {
	out := map[string][]string{}
	for _, n := range nodes {
		was, _ := s.Reasons(n, before)
		now, ok := s.Reasons(n, end)
		if !ok {
			continue
		}
		for _, x := range now {
			if !slices.Contains(was, x) {
				out[n] = append(out[n], x)
			}
		}
	}
	return out
}
