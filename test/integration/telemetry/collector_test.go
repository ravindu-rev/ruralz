// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"testing"
	"time"

	"google.golang.org/grpc/codes"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// failing is the degraded reason of a collector failing past one push
// interval.
const failing = "telemetry_export_failing"

// collectorNode starts a Node exporting every trace and every access
// record to sink, routing /work to a mock Upstream.
func collectorNode(t *testing.T, top *topology.Topology, sink *otlpsink.Sink) *topology.Node {
	t.Helper()
	mock, err := top.StartMockUpstream(mockup.Config{LogSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	return startNode(t, top, topology.NodeOptions{Bundle: []byte(gatewayDoc(otlpBlock(sink.URL(), "")) +
		upstreamDoc("u", mock.Addr()) + routeDoc("work", "/work", "u", ""))})
}

// load offers rate requests per second to /work for d of real time and
// fails the test on any outcome but 200: telemetry never fails a request.
func load(t *testing.T, top *topology.Topology, n *topology.Node, rate float64, d time.Duration) *loadgen.Result {
	t.Helper()
	target, err := loadgen.NewHTTP(loadgen.HTTPConfig{URL: n.URL("web") + "/work", Connections: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.Close() }()
	res, err := loadgen.Run(t.Context(), target, loadgen.Config{Rate: rate, Duration: d})
	if err != nil {
		t.Fatal(err)
	}
	m := res.Measured
	if m.Outcomes[loadgen.Success] != m.Offered {
		t.Fatalf("%d of %d requests succeeded; outcomes %v, statuses %v, codes %v",
			m.Outcomes[loadgen.Success], m.Offered, m.Outcomes, m.Statuses, m.Codes)
	}
	return res
}

// degraded returns ruralz_node_degraded_info for reason from a fresh scrape.
func degraded(t *testing.T, top *topology.Topology, n *topology.Node, reason string) float64 {
	t.Helper()
	return value(t, scrape(t, top, n), catalog.NodeDegradedInfo, "reason", reason)
}

// raiseFailing moves the clock in 1 s steps, sending a request each step
// so every signal keeps failing, until telemetry_export_failing is raised;
// it fails the test past two minutes of the clock.
func raiseFailing(t *testing.T, top *topology.Topology, n *topology.Node) {
	t.Helper()
	for range 120 {
		requests(t, top, n, "/work", 1)
		if degraded(t, top, n, failing) == 1 {
			return
		}
	}
	t.Fatalf("%s not raised within two minutes of the clock", failing)
}

// clearFailing moves the clock in 1 s steps, sending a request each step,
// until every failing signal has exported again and the reason clears; the
// collector's own reconnection runs in real time, so the wait is bounded
// in real time.
func clearFailing(t *testing.T, top *topology.Topology, n *topology.Node) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for degraded(t, top, n, failing) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not clear once the collector accepted again", failing)
		}
		requests(t, top, n, "/work", 1)
		time.Sleep(50 * time.Millisecond)
	}
}

// drops returns the OTLP-side loss counters of a scrape: spans by reason
// and log records by stream and reason.
type drops struct {
	spansFull, spansError                float64
	accessFull, accessError, processLost float64
}

func dropsOf(t *testing.T, set promtext.Set) drops {
	t.Helper()
	return drops{
		spansFull:   value(t, set, catalog.TelemetrySpansDroppedTotal, "reason", "queue_full"),
		spansError:  value(t, set, catalog.TelemetrySpansDroppedTotal, "reason", "export_error"),
		accessFull:  value(t, set, catalog.TelemetryLogsDroppedTotal, "stream", "access", "sink", "otlp", "reason", "queue_full"),
		accessError: value(t, set, catalog.TelemetryLogsDroppedTotal, "stream", "access", "sink", "otlp", "reason", "export_error"),
		processLost: value(t, set, catalog.TelemetryLogsDroppedTotal, "stream", "process", "sink", "otlp", "reason", "export_error") +
			value(t, set, catalog.TelemetryLogsDroppedTotal, "stream", "process", "sink", "otlp", "reason", "queue_full"),
	}
}

// TestCollectorModes drives one Node through the collector modes of
// internal/testkit/otlpsink in turn: a collector that rejects, one that
// stalls and one that is down each raise telemetry_export_failing past one
// push interval and clear it once the collector accepts again, rejected
// and timed-out batches count export_error, a stall that outlasts the
// queues counts queue_full, and every request stays unaffected.
func TestCollectorModes(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	n := collectorNode(t, top, sink)
	requests(t, top, n, "/work", 3)
	pushMetrics(t, top, sink)
	if degraded(t, top, n, failing) != 0 {
		t.Fatalf("%s raised with the collector accepting", failing)
	}

	t.Run("Reject", func(t *testing.T) {
		before := dropsOf(t, scrape(t, top, n))
		if err := sink.SetMode(otlpsink.Reject{Code: codes.Unavailable, Message: "overloaded"}); err != nil {
			t.Fatal(err)
		}
		raiseFailing(t, top, n)
		after := dropsOf(t, scrape(t, top, n))
		if after.spansError <= before.spansError || after.accessError <= before.accessError {
			t.Errorf("export_error after rejections: spans %v -> %v, access records %v -> %v; want both counted",
				before.spansError, after.spansError, before.accessError, after.accessError)
		}
		if err := sink.SetMode(otlpsink.Accept{}); err != nil {
			t.Fatal(err)
		}
		clearFailing(t, top, n)
	})

	t.Run("Stall", func(t *testing.T) {
		before := dropsOf(t, scrape(t, top, n))
		if err := sink.SetMode(otlpsink.Stall{}); err != nil {
			t.Fatal(err)
		}
		// The first batch of each signal stalls on the collector until its
		// 10 s bound on the clock, which moves 5 s while the load runs: the
		// trace rate cap admits 1,000 sampled traces per second of the
		// clock, so 6,000 traces of three spans and 12,000 access records
		// overflow both queues of 8,192.
		if err := advancing(t.Context(), top, 500*time.Millisecond, time.Second, func() error {
			load(t, top, n, 1200, 10*time.Second)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		mid := dropsOf(t, scrape(t, top, n))
		if mid.spansFull <= before.spansFull || mid.accessFull <= before.accessFull {
			t.Errorf("queue_full during the stall: spans %v -> %v, access records %v -> %v; want both counted",
				before.spansFull, mid.spansFull, before.accessFull, mid.accessFull)
		}
		raiseFailing(t, top, n)
		after := dropsOf(t, scrape(t, top, n))
		if after.spansError <= before.spansError || after.accessError <= before.accessError {
			t.Errorf("export_error of stalled batches: spans %v -> %v, access records %v -> %v; want both counted",
				before.spansError, after.spansError, before.accessError, after.accessError)
		}
		if err := sink.SetMode(otlpsink.Accept{}); err != nil {
			t.Fatal(err)
		}
		clearFailing(t, top, n)
	})

	t.Run("Down", func(t *testing.T) {
		before := dropsOf(t, scrape(t, top, n))
		if err := sink.SetMode(otlpsink.Down{}); err != nil {
			t.Fatal(err)
		}
		raiseFailing(t, top, n)
		after := dropsOf(t, scrape(t, top, n))
		if after.spansError <= before.spansError || after.accessError <= before.accessError {
			t.Errorf("export_error with the collector down: spans %v -> %v, access records %v -> %v; want both counted",
				before.spansError, after.spansError, before.accessError, after.accessError)
		}
		if err := sink.SetMode(otlpsink.Accept{}); err != nil {
			t.Fatal(err)
		}
		clearFailing(t, top, n)
	})
}

// TestMetricsPushEveryFifteenSeconds checks the metric push interval in
// controlled time: once the first push is seen, the next comes after 15 s
// of the clock and not before, and so does the one after.
func TestMetricsPushEveryFifteenSeconds(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{})
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	collectorNode(t, top, sink)
	const step = 100 * time.Millisecond
	pushes := func() int64 { return sink.Stats().MetricRequests }
	// The first push is due 15 s after the activation that built the
	// pipeline, at the clock reading the Node became ready at: find it in
	// 100 ms steps from 1 s before.
	if err := top.Advance(t.Context(), metricPush-time.Second); err != nil {
		t.Fatal(err)
	}
	for i := 0; pushes() == 0; i++ {
		if i > int(2*time.Second/step) {
			t.Fatal("no metric push within 16 s of the clock")
		}
		if err := top.Advance(t.Context(), step); err != nil {
			t.Fatal(err)
		}
		waitQuiet(sink)
	}
	for k := int64(2); k <= 3; k++ {
		if err := top.Advance(t.Context(), metricPush-step); err != nil {
			t.Fatal(err)
		}
		waitQuiet(sink)
		if got := pushes(); got != k-1 {
			t.Fatalf("%d pushes %v before push %d is due, want %d", got, step, k, k-1)
		}
		if err := top.Advance(t.Context(), step); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, "the next push 15 s after the last", func() bool { return pushes() == k })
	}
}

// waitQuiet gives an export a timer just fired the real time to reach the
// sink: it returns once the sink's request count has not changed for
// 100 ms of real time, or after 5 s.
func waitQuiet(sink *otlpsink.Sink) {
	last := sink.Stats()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
		cur := sink.Stats()
		if cur.MetricRequests == last.MetricRequests && cur.TraceRequests == last.TraceRequests && cur.LogRequests == last.LogRequests {
			return
		}
		last = cur
	}
}

// References of the https listener's certificate and key.
const (
	webCert = "/etc/ruralz/tls/web.crt"
	webKey  = "/etc/ruralz/tls/web.key"
)

// tlsGatewayDoc is a Gateway named edge with one https listener web on
// 8443 serving the certificate webCert, admin on 9901, and the given
// telemetry block.
func tlsGatewayDoc(telemetry string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n" +
		"  admin:\n    port: 9901\n" +
		"  listeners:\n    - name: web\n      protocol: https\n      port: 8443\n      tls:\n        certificates:\n" +
		"          - name: web\n            certificate:\n              secretRef:\n                provider: file\n                name: " + webCert + "\n" +
		"            privateKey:\n              secretRef:\n                provider: file\n                name: " + webKey + "\n" +
		"  telemetry:\n" + telemetry
}

// TestCleartextEndpointRaisesHop checks that an http collector endpoint
// is a reported cleartext telemetry hop, raising cleartext_hop, and that a
// Hot Reload to an https one clears it.
func TestCleartextEndpointRaisesHop(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{Transport: topology.Transport{CleartextOTLP: true}})
	h2c, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	serverTLS, err := top.ServerTLS()
	if err != nil {
		t.Fatal(err)
	}
	secure, err := top.StartOTLPSink(otlpsink.Config{TLS: serverTLS})
	if err != nil {
		t.Fatal(err)
	}
	// Telemetry is the Node's only hop that can be cleartext: its one
	// listener serves TLS, the admin port too, and no Upstream or State
	// Store is declared.
	certPEM, keyPEM, err := top.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	secrets := map[string][]byte{"file:" + webCert: certPEM, "file:" + webKey: keyPEM}
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(tlsGatewayDoc(otlpBlock(h2c.URL(), ""))), Secrets: secrets})
	set := scrape(t, top, n)
	if v := value(t, set, catalog.SecurityCleartextHops, "hop", "telemetry"); v != 1 {
		t.Errorf("cleartext telemetry hops = %v with an http endpoint, want 1", v)
	}
	if v := value(t, set, catalog.NodeDegradedInfo, "reason", "cleartext_hop"); v != 1 {
		t.Errorf("cleartext_hop = %v with an http endpoint, want 1", v)
	}
	pushMetrics(t, top, h2c)
	if reqs := h2c.Requests(); reqs[len(reqs)-1].TLS != nil {
		t.Error("the http endpoint was exported to over TLS")
	}

	reload(t, n, tlsGatewayDoc(otlpBlock(secure.URL(), "")))
	set = scrape(t, top, n)
	if v := value(t, set, catalog.SecurityCleartextHops, "hop", "telemetry"); v != 0 {
		t.Errorf("cleartext telemetry hops = %v with an https endpoint, want 0", v)
	}
	if v := value(t, set, catalog.NodeDegradedInfo, "reason", "cleartext_hop"); v != 0 {
		t.Errorf("cleartext_hop = %v with an https endpoint, want 0", v)
	}
	pushMetrics(t, top, secure)
	if reqs := secure.Requests(); reqs[len(reqs)-1].TLS == nil {
		t.Error("the https endpoint was exported to without TLS")
	}
}
