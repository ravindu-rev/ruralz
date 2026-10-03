// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// observe applies one attempt's outcome to Endpoint e at now, an instant of
// t (Tracker.at) (05 reqs 13, 18, 19 and 21); suspect is whether e was
// suspect when the attempt ended.
// It reports whether the outcome ejected e.
func (t *Tracker) observe(e *endpoint, now int64, o Outcome, suspect bool) bool {
	if o.Failed {
		e.failures.add(now)
	}
	pol := o.Passive
	if pol == (PassivePolicy{}) {
		pol = *t.passive.Load()
	}
	pol = pol.WithDefaults()
	// An attempt timeout on a suspect Endpoint ejects it at once, over the
	// cap (05 req 21).
	if o.Cause == CauseTimeout && suspect {
		return t.eject(e, now, pol, true, o.Stripe)
	}
	if !o.Failed {
		if e.consecutive.Load() != 0 {
			e.consecutive.Store(0)
		}
		return false
	}
	// Failures of attempts that end while the Endpoint is ejected do not
	// count toward its next ejection.
	if e.ejectedUntil.Load() > now {
		return false
	}
	if int(e.consecutive.Add(1)) < pol.ConsecutiveErrors {
		return false
	}
	// Connect errors (dial errors, dial timeouts, HTTP/2 ping closes)
	// bypass the cap (05 reqs 19, 22).
	return t.eject(e, now, pol, o.Cause == CauseConnect, o.Stripe)
}

// eject passively ejects e at now unless it is already ejected, it left
// the set, the Tracker is closed, or (without bypass) the ejection would
// exceed floor(MaxEjectionPercent × E) passively ejected Endpoints (05 req
// 19). Ejections are serialized by t.mu, so the cap holds under any
// concurrency. The run of failures that triggered a skipped ejection is
// kept, so the next failure tries again.
func (t *Tracker) eject(e *endpoint, now int64, pol PassivePolicy, bypass bool, s emit.Stripe) bool {
	t.mu.Lock()
	if t.closed || e.removed.Load() || e.ejectedUntil.Load() > now {
		t.mu.Unlock()
		return false
	}
	eps := t.tab.Load().eps
	if !bypass {
		ejected := 0
		for _, x := range eps {
			if x.ejectedUntil.Load() > now {
				ejected++
			}
		}
		if ejected+1 > len(eps)*MaxEjectionPercent/100 {
			t.mu.Unlock()
			return false
		}
	}
	n := min(decayed(e, now, pol.EjectionTime)+1, MaxEjectionMultiplier)
	until := now + int64(n)*int64(pol.EjectionTime)
	e.ejections = n
	e.decayFrom = until
	e.ejectedUntil.Store(until)
	e.consecutive.Store(0)
	if m := t.metrics.Load(); m != nil && m.Ejections[emit.EjectPassive] != nil {
		m.Ejections[emit.EjectPassive].Add(s, 1)
	}
	changed := t.recountLocked(now)
	t.mu.Unlock()
	t.report(changed)
	return true
}

// decayed returns e's ejection count at now: the count of its last
// ejection minus one per ejectionTime spent not ejected since that
// ejection ended (05 req 18; proposed, spec 05 section 9 item 10). t.mu is
// held.
func decayed(e *endpoint, now int64, ejectionTime time.Duration) int {
	if e.ejections == 0 || now <= e.decayFrom {
		return e.ejections
	}
	if ejectionTime <= 0 {
		ejectionTime = DefaultEjectionTime
	}
	steps := (now - e.decayFrom) / int64(ejectionTime)
	if steps >= int64(e.ejections) {
		return 0
	}
	return e.ejections - int(steps)
}
