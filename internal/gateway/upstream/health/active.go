// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// RecordProbe applies one active check result for the Endpoint with the
// given identity at now (05 req 20): HealthyThreshold consecutive
// successes mark an unhealthy Endpoint healthy, UnhealthyThreshold
// consecutive failures mark a healthy one unhealthy and count
// ruralz_upstream_ejections_total{reason="active"}. Active removals ignore
// the passive ejection cap (05 req 19). It does nothing without an active
// policy or for an unknown identity, and reports whether the verdict
// changed. The Prober calls it; tests and the Upstream layer may too.
func (t *Tracker) RecordProbe(identity string, ok bool, now time.Time) bool {
	i, found := t.Index(identity)
	if !found {
		return false
	}
	return t.recordProbe(t.tab.Load().eps[i], ok, t.at(now))
}

// recordProbe applies a probe result to e under the current active policy;
// now is an instant of t (Tracker.at).
func (t *Tracker) recordProbe(e *endpoint, ok bool, now int64) bool {
	t.mu.Lock()
	act := t.active.Load()
	if t.closed || act == nil || e.removed.Load() {
		t.mu.Unlock()
		return false
	}
	flipped := false
	if ok {
		e.probeFail = 0
		e.probeOK = min(e.probeOK+1, act.HealthyThreshold)
		if e.unhealthy.Load() && e.probeOK >= act.HealthyThreshold {
			e.unhealthy.Store(false)
			flipped = true
		}
	} else {
		e.probeOK = 0
		e.probeFail = min(e.probeFail+1, act.UnhealthyThreshold)
		if !e.unhealthy.Load() && e.probeFail >= act.UnhealthyThreshold {
			e.unhealthy.Store(true)
			flipped = true
			if m := t.metrics.Load(); m != nil && m.Ejections[emit.EjectActive] != nil {
				m.Ejections[emit.EjectActive].Add(0, 1)
			}
		}
	}
	changed := t.recountLocked(now)
	t.mu.Unlock()
	t.report(changed)
	return flipped
}
