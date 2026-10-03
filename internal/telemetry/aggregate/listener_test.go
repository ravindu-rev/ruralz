// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// Recording through the emit helpers that live beside the contract
// (GatewayTimer and OriginOf) into this package's listener families.

// emit.GatewayTimer.Observe records the result into the listener's
// histogram, or counts a clock anomaly in
// ruralz_http_gateway_duration_skipped_total (spec 09 req 54).
func TestGatewayTimerObserve_Req54(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, b := admitBind(t, r, emit.Shape{Listeners: []string{"l"}})
	defer b.Release()
	h := pl.Listener("l").GatewayDuration
	skipped := r.Node().GatewayDurationSkipped
	var g emit.GatewayTimer
	g.Reset(epoch())
	g.Enter(epoch().Add(time.Millisecond))
	g.Leave(epoch().Add(3 * time.Millisecond))
	if d, ok := g.Observe(epoch().Add(3100*time.Microsecond), 0, h, skipped); !ok || d != 1100*time.Microsecond {
		t.Errorf("Observe = %v, %v", d, ok)
	}
	g.Reset(epoch())
	g.Leave(epoch())
	if _, ok := g.Observe(epoch().Add(time.Millisecond), 0, h, skipped); ok {
		t.Error("anomaly recorded")
	}
	pts := collectNow(t, r, clk)
	if p := mustGet(t, pts, catalog.HTTPGatewayDurationSeconds+`{listener="l"}`); p.count != 1 || !approx(p.sum, 0.0011) {
		t.Errorf("gateway duration = %+v", p)
	}
	if got := mustGet(t, pts, catalog.HTTPGatewayDurationSkippedTotal+`{reason="clock_anomaly"}`).value; got != 1 {
		t.Errorf("skipped = %v", got)
	}
}

// The origin emit.OriginOf classifies indexes the origin label of
// ruralz_http_listener_requests_total (spec 09 req 44).
func TestOriginLabel_Req44(t *testing.T) {
	r, clk := newTestRegistry(t, nil)
	pl, b := admitBind(t, r, emit.Shape{Listeners: []string{"public"}})
	defer b.Release()
	lr := pl.Listener("public").Requests
	lr.Inc(0, emit.ProtoHTTP1, 200, emit.OriginOf("", true))
	lr.Inc(0, emit.ProtoHTTP1, 502, emit.OriginOf("RZ-UP-001", false))
	lr.Inc(0, emit.ProtoHTTP1, 503, emit.OriginOf("RZ-STS-001", false))
	pts := collectNow(t, r, clk)
	for _, tt := range []struct{ class, origin string }{{"2xx", "upstream"}, {"5xx", "dependency"}, {"5xx", "node"}} {
		key := fmt.Sprintf(`%s{listener="public",origin=%q,protocol="http1",status_class=%q}`, catalog.HTTPListenerRequestsTotal, tt.origin, tt.class)
		if got := mustGet(t, pts, key).value; got != 1 {
			t.Errorf("%s = %v, want 1", key, got)
		}
	}
	// The catalog enumeration order is the emit index order.
	var origins []string
	for _, l := range catalog.Families() {
		if l.Name != catalog.HTTPListenerRequestsTotal {
			continue
		}
		for _, lab := range l.Labels {
			if lab.Name == "origin" {
				origins = lab.Values
			}
		}
	}
	if strings.Join(origins, ",") != "upstream,node,dependency" || emit.OriginUpstream != 0 || emit.OriginNode != 1 || emit.OriginDependency != 2 {
		t.Errorf("origin values %v do not match the emit indexes", origins)
	}
}
