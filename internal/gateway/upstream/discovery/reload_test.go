// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// reload is a Hot Reload that replaces the Source of the Upstream "orders"
// (static host old.internal) with one for new.internal, compiled with
// Options.Previous while the Revision is not active yet (no handles).
type reload struct {
	old   *harness
	nw    *Source
	gauge *gauge // the Upstream's one discovery_stale series
}

func newReload(t *testing.T, oldStale bool) *reload {
	t.Helper()
	old := newHarness(t, Spec{Static: []StaticEndpoint{{Address: "old.internal:80", Weight: 1}}}, nil)
	old.res.setHost("old.internal", "10.0.0.1")
	old.src.Refresh(t.Context())
	if oldStale {
		old.res.failHost("old.internal", servfail("old.internal"))
		old.src.Refresh(t.Context())
	}
	if old.stale(t) != oldStale {
		t.Fatalf("old Source stale %v, want %v", !oldStale, oldStale)
	}
	nw, err := New(Spec{Static: []StaticEndpoint{{Address: "new.internal:80", Weight: 1}}}, Options{
		Upstream: "orders",
		Clock:    old.clk,
		Resolver: old.res,
		Rand:     newRand(2),
		Status:   old.status,
		Previous: old.src.Current(),
	})
	if err != nil {
		t.Fatal(err)
	}
	r := &reload{old: old, nw: nw, gauge: old.degrade[emit.UpDegradedDiscoveryStale]}
	if nw.holder == old.src.holder {
		t.Fatalf("both Sources hold the reason as %q", nw.holder)
	}
	if want := boolGauge(oldStale); r.gauge.v.Load() != want {
		t.Fatal("New wrote the Upstream gauge")
	}
	return r
}

// state returns the Node gauge and the Upstream gauge of discovery_stale.
func (r *reload) state() (node, upstream bool) {
	return r.old.status.node(catalog.ReasonDiscoveryStale), r.gauge.v.Load() == 1
}

func boolGauge(on bool) int64 {
	if on {
		return 1
	}
	return 0
}

// TestReloadKeepsStale covers 05 req 7 across a Hot Reload that replaces a
// Source during a DNS outage: the old and the new Source hold the Node
// reason under their own holders, the Upstream gauge belongs to the
// activated Source, and neither the replaced Source's Close nor its later
// refreshes clear discovery_stale while the new Source still fails. Both
// gauges clear with the new Source's first good answer.
func TestReloadKeepsStale(t *testing.T) {
	for _, tc := range []struct {
		name       string
		closeFirst bool // the old Source closes before the new one activates
	}{
		{"activate then close", false},
		{"close then activate", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newReload(t, true)
			r.old.res.failHost("new.internal", servfail("new.internal"))
			if set, _ := r.nw.Refresh(t.Context()); !set.Status.Stale {
				t.Fatal("compile-time refresh during the outage not stale")
			}
			if !r.old.status.held(catalog.ReasonDiscoveryStale, r.nw.holder) {
				t.Fatal("new Source does not hold the Node reason")
			}
			if tc.closeFirst {
				r.old.src.Close()
				if node, up := r.state(); !node || !up {
					t.Fatalf("old Close cleared discovery_stale: node %v upstream %v", node, up)
				}
				r.nw.SetMetrics(r.old.metrics)
			} else {
				r.nw.SetMetrics(r.old.metrics)
				r.old.src.Close()
			}
			if node, up := r.state(); !node || !up {
				t.Fatalf("after the reload: node %v upstream %v", node, up)
			}
			// More failures of the live Source keep both raised.
			for range 3 {
				r.nw.Refresh(t.Context())
			}
			if node, up := r.state(); !node || !up {
				t.Fatalf("live Source failing: node %v upstream %v", node, up)
			}
			r.old.res.setHost("new.internal", "10.0.0.2")
			if set, _ := r.nw.Refresh(t.Context()); set.Status.Stale {
				t.Fatal("good answer still stale")
			}
			if node, up := r.state(); node || up {
				t.Fatalf("good answer: node %v upstream %v", node, up)
			}
		})
	}
}

// TestReloadSupersedes: once the new Source is activated, the replaced
// one, still running until it closes, no longer writes the Upstream gauge,
// so its recovery cannot clear discovery_stale the live Source raised; it
// still releases its own Node reason.
func TestReloadSupersedes(t *testing.T) {
	r := newReload(t, true)
	r.old.res.failHost("new.internal", servfail("new.internal"))
	r.nw.Refresh(t.Context())
	r.nw.SetMetrics(r.old.metrics)
	r.old.res.setHost("old.internal", "10.0.0.1")
	if set, _ := r.old.src.Refresh(t.Context()); set.Status.Stale {
		t.Fatal("old Source did not recover")
	}
	if r.old.status.held(catalog.ReasonDiscoveryStale, r.old.src.holder) {
		t.Fatal("recovered old Source still holds the Node reason")
	}
	if node, up := r.state(); !node || !up {
		t.Fatalf("superseded Source cleared discovery_stale: node %v upstream %v", node, up)
	}
	// A later failure of the superseded Source raises only its own Node
	// reason; the gauge stays with the live Source.
	r.old.res.failHost("old.internal", servfail("old.internal"))
	r.old.src.Refresh(t.Context())
	r.old.res.setHost("new.internal", "10.0.0.2")
	r.nw.Refresh(t.Context())
	if node, up := r.state(); !node || up {
		t.Fatalf("live Source recovered, old still failing: node %v upstream %v", node, up)
	}
	r.old.src.Close()
	if node, up := r.state(); node || up {
		t.Fatalf("after the old Close: node %v upstream %v", node, up)
	}
}

// TestReloadRefused: the Source of a refused Revision, which failed its
// compile-time refresh, never writes the active snapshot's gauge, and its
// Close releases the Node reason it raised.
func TestReloadRefused(t *testing.T) {
	r := newReload(t, false)
	r.old.res.failHost("new.internal", servfail("new.internal"))
	r.nw.Refresh(t.Context())
	if node, up := r.state(); !node || up {
		t.Fatalf("compile-time failure: node %v upstream %v", node, up)
	}
	r.nw.Close()
	if node, up := r.state(); node || up {
		t.Fatalf("refused Source closed: node %v upstream %v", node, up)
	}
	// The active Source still owns the series.
	r.old.res.failHost("old.internal", servfail("old.internal"))
	r.old.src.Refresh(t.Context())
	if node, up := r.state(); !node || !up {
		t.Fatalf("active Source failing: node %v upstream %v", node, up)
	}
}
