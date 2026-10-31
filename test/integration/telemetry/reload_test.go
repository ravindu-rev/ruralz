// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"net/http"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// metricPush is the OTLP metric push interval (target): advancing the
// clock by it makes the Node push once.
const metricPush = 15 * time.Second

// retireBound is the time within which a swapped OTLP connection closes
// (target), the telemetry flush bound of the Drain timeline.
const retireBound = 5 * time.Second

// otlpCA is the reference of the collector CA a Bundle names in
// telemetry.otlp.tls.
const otlpCA = "/etc/ruralz/otel/ca.crt"

// reload swaps the Node's test render for bundle, signals SIGHUP and
// waits until the Node serves it.
func reload(t *testing.T, n *topology.Node, bundle string) {
	t.Helper()
	r, err := n.Reload(t.Context(), []byte(bundle))
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Signal(t.Context(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	if err := n.WaitRevision(t.Context(), r.TestDigest); err != nil {
		t.Fatal(err)
	}
}

// requests sends count GETs to path on the Node's web listener, each
// expected to answer 200.
func requests(t *testing.T, top *topology.Topology, n *topology.Node, path string, count int) {
	t.Helper()
	client := top.HTTPClient()
	for range count {
		code, err := get(t.Context(), client, n.URL("web")+path, nil)
		if err != nil || code != http.StatusOK {
			t.Fatalf("GET %s = %d, %v; want 200", path, code, err)
		}
	}
}

// pushMetrics advances the clock one push interval and waits until the
// sink has recorded one more metric export.
func pushMetrics(t *testing.T, top *topology.Topology, sink *otlpsink.Sink) {
	t.Helper()
	before := sink.Stats().MetricRequests
	if err := top.Advance(t.Context(), metricPush); err != nil {
		t.Fatal(err)
	}
	eventually(t, "a metric push after one interval", func() bool {
		return sink.Stats().MetricRequests > before
	})
}

// TestHotReloadKeepsCountersAndConnection checks a Hot Reload that leaves
// the telemetry settings alone: the counters carry over without a reset,
// on /metrics and over OTLP, and the OTLP connection is kept.
func TestHotReloadKeepsCountersAndConnection(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	gw := gatewayDoc(otlpBlock(sink.URL(), ""))
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(gw + upstreamDoc(mock.Addr()) + routeDoc("orders", "/orders", ""))})
	requests(t, top, n, "/orders", 5)
	pushMetrics(t, top, sink)
	if v := value(t, scrape(t, top, n), catalog.HTTPRequestsTotal, "route", "orders", "status_class", "2xx"); v != 5 {
		t.Fatalf("requests before the reload = %v, want 5", v)
	}
	conns := sink.Stats().Connections

	reload(t, n, gw+upstreamDoc(mock.Addr())+routeDoc("orders", "/orders", "")+routeDoc("items", "/items", ""))
	requests(t, top, n, "/orders", 3)
	requests(t, top, n, "/items", 2)
	set := scrape(t, top, n)
	if v := value(t, set, catalog.HTTPRequestsTotal, "route", "orders", "status_class", "2xx"); v != 8 {
		t.Errorf("requests after the reload = %v, want 8: the counter carries over", v)
	}
	if v := value(t, set, catalog.HTTPRequestsTotal, "route", "items", "status_class", "2xx"); v != 2 {
		t.Errorf("requests of the added Route = %v, want 2", v)
	}
	pushMetrics(t, top, sink)
	if v := exportedSum(t, sink, catalog.HTTPRequestsTotal, "route", "orders", "status_class", "2xx"); v != 8 {
		t.Errorf("exported %s{route=orders} = %v, want 8: the OTLP counter carries over", catalog.HTTPRequestsTotal, v)
	}
	st := sink.Stats()
	if st.Connections != conns || st.OpenConnections != 1 {
		t.Errorf("collector connections %d (open %d) after the reload, want the %d before, still open", st.Connections, st.OpenConnections, conns)
	}
	var first uint64
	for _, r := range sink.Requests() {
		if first == 0 {
			first = r.Conn
		}
		if r.Conn != first {
			t.Fatalf("a %v export used connection %d, want the one connection %d", r.Signal, r.Conn, first)
		}
	}
}

// exportedSum returns the value of the newest exported data point of the
// counter name whose attributes hold labels.
func exportedSum(t *testing.T, sink *otlpsink.Sink, name string, labels ...string) float64 {
	t.Helper()
	m, ok := sink.LastMetric(name)
	if !ok {
		t.Fatalf("%s never exported", name)
	}
	for _, dp := range m.Proto.GetSum().GetDataPoints() {
		match := true
		for i := 0; i+1 < len(labels); i += 2 {
			v, _ := otlpsink.Attr(dp.GetAttributes(), labels[i])
			if otlpsink.ValueString(v) != labels[i+1] {
				match = false
				break
			}
		}
		if match {
			return dp.GetAsDouble() + float64(dp.GetAsInt())
		}
	}
	t.Fatalf("%s%v not in the newest export", name, labels)
	return 0
}

// TestEndpointChangeSwapsWithinFiveSeconds checks a Hot Reload that
// changes otlp.endpoint: the new collector receives the exports at once,
// and the old connection, whose final export the old collector holds,
// closes when 5 s of the clock have passed, not before.
func TestEndpointChangeSwapsWithinFiveSeconds(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{})
	if err != nil {
		t.Fatal(err)
	}
	a, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rest := upstreamDoc(mock.Addr()) + routeDoc("orders", "/orders", "")
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(gatewayDoc(otlpBlock(a.URL(), "")) + rest)})
	requests(t, top, n, "/orders", 2)
	pushMetrics(t, top, a)
	if a.Stats().OpenConnections != 1 {
		t.Fatalf("collector A: %+v, want one open connection", a.Stats())
	}
	// A holds every export from now on, the retirement's final one too.
	if err := a.SetMode(otlpsink.Stall{}); err != nil {
		t.Fatal(err)
	}
	reload(t, n, gatewayDoc(otlpBlock(b.URL(), ""))+rest)
	// The activation swapped the pipelines at this clock reading: nothing
	// moved the clock since.
	activated := top.Now()
	requests(t, top, n, "/orders", 2)
	eventually(t, "the old pipeline's final export reaching A", func() bool { return a.Stats().Stalled > 0 })
	// The new requests' spans leave for B within the 1 s batch interval.
	if err := top.Advance(t.Context(), time.Second+time.Millisecond); err != nil {
		t.Fatal(err)
	}
	eventually(t, "spans at collector B", func() bool { return len(b.Spans()) > 0 })
	if a.Stats().OpenConnections != 1 {
		t.Fatalf("collector A closed 1 s after the swap: %+v", a.Stats())
	}
	if err := top.Advance(t.Context(), activated.Add(retireBound).Sub(top.Now())-10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if a.Stats().OpenConnections != 1 {
		t.Fatalf("collector A closed before 5 s of the clock: %+v", a.Stats())
	}
	if err := top.Advance(t.Context(), 20*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the old connection closing at the 5 s bound", func() bool { return a.Stats().OpenConnections == 0 })
	if b.Stats().OpenConnections != 1 {
		t.Errorf("collector B: %+v, want one open connection", b.Stats())
	}
}

// TestTLSChangeSwapsConnection checks a Hot Reload that changes
// telemetry.otlp.tls alone: the Node closes the old connection without
// the clock moving, then exports to the same collector over a new TLS
// connection.
func TestTLSChangeSwapsConnection(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	rest := upstreamDoc(mock.Addr()) + routeDoc("orders", "/orders", "")
	n := startNode(t, top, topology.NodeOptions{
		Bundle:  []byte(gatewayDoc(otlpBlock(sink.URL(), "")) + rest),
		Secrets: map[string][]byte{"file:" + otlpCA: top.WithCanary(top.CA().CertPEM)},
	})
	pushMetrics(t, top, sink)
	if st := sink.Stats(); st.Connections != 1 || st.OpenConnections != 1 {
		t.Fatalf("collector: %+v, want one connection", st)
	}
	before := len(sink.Requests())
	tls := "      tls:\n        caCertificate:\n          secretRef:\n            provider: file\n            name: " + otlpCA + "\n"
	reload(t, n, gatewayDoc(otlpBlock(sink.URL(), tls))+rest)
	eventually(t, "the old connection closing", func() bool { return sink.Stats().OpenConnections == 0 })
	requests(t, top, n, "/orders", 1)
	pushMetrics(t, top, sink)
	if st := sink.Stats(); st.Connections != 2 || st.OpenConnections != 1 {
		t.Errorf("collector after the push: %+v, want a second connection, open", st)
	}
	reqs := sink.Requests()
	last := reqs[len(reqs)-1]
	if last.Conn == reqs[before-1].Conn || last.TLS == nil {
		t.Errorf("the push after the reload used connection %d (TLS %v), want a new TLS connection after %d", last.Conn, last.TLS != nil, reqs[before-1].Conn)
	}
}
