// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Scenario 7 of docs/engineering/03-testing-and-quality-strategy.md
// "End-to-end tests": a Zero-Downtime Upgrade under load fails zero
// requests on TCP listeners, for clients honoring Connection: close and
// GOAWAY. A test Bundle with one plain Route to a mock Upstream on 8080
// and 8443 serves two Nodes behind the L4 balancer, one per listener,
// under open-loop load of 1,000 requests per second for 60 s (target),
// split evenly over HTTP/1.1 and h2c on 8080 and HTTP/2 and HTTP/1.1 over
// TLS on 8443. 10 s in, one Node hands over to a new process inside a
// network namespace with net.ipv4.tcp_migrate_req=1, the test advancing
// the Node clock through the linger and the Drain. Any outcome but the
// mock's 200 fails the scenario.

// Scenario 7's load (targets).
const (
	upgradeRate     = 1000.0
	upgradeDuration = 60 * time.Second
	upgradeAt       = 10 * time.Second
	// upgradeMockName names the mock Upstream, whose 200s carry it in
	// mockup.HeaderServer.
	upgradeMockName = "backend"
)

// upgradeBundle is the test Bundle: an http listener on 8080 and an https
// one on 8443 with a file certificate, and one plain Route to the mock
// Upstream at address. Its Route timeout lies past the Drain deadline,
// which cuts work first, so a clock the test advances fast through the
// Drain never times out a request still in flight on a busy host.
func upgradeBundle(address string) []byte {
	return []byte(`apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: edge
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: main
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: backend
spec:
  protocol: http
  endpoints:
    - address: "` + address + `"
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: plain
spec:
  match:
    path: {prefix: /}
  upstreams:
    - name: backend
  timeout: 5m
`)
}

// upgradeFailures keeps the requests that ended with anything but the
// mock's 200, the first ones whole.
type upgradeFailures struct {
	mu    sync.Mutex
	count map[string]int
	first []string
}

// record counts s unless it is the mock's 200.
func (f *upgradeFailures) record(load string, s loadgen.Sample) {
	if s.Outcome == loadgen.Success && s.Status == http.StatusOK && s.Header.Get(mockup.HeaderServer) == upgradeMockName {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	key := fmt.Sprintf("%s: %s %d %s", load, s.Outcome, s.Status, s.Code)
	f.count[key]++
	if len(f.first) < 20 {
		f.first = append(f.first, fmt.Sprintf("%s at %v: %v (conn %s -> %s)", key, s.Intended.Format(time.RFC3339Nano), s.Err, s.Conn.Local, s.Conn.Remote))
	}
}

// TestZeroDowntimeUpgradeUnderLoad is scenario 7.
func TestZeroDowntimeUpgradeUnderLoad(t *testing.T) {
	if !inNetNS(t, map[string]string{"net.ipv4.tcp_migrate_req": "1"}) {
		return
	}
	ctx := t.Context()
	top := newTopology(t, topology.Config{})
	mock, err := top.StartMockUpstream(mockup.Config{
		Name: upgradeMockName, Behavior: mockup.Behavior{Body: []byte("ok"), ContentType: "text/plain"},
		LogSize: int(upgradeRate*upgradeDuration.Seconds()) * 2, MaxLoggedBody: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := top.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := top.AddNodes(ctx, 2, topology.NodeOptions{
		Bundle:  upgradeBundle(mock.Addr()),
		Secrets: map[string][]byte{"file:/etc/ruralz/tls/tls.crt": certPEM, "file:/etc/ruralz/tls/tls.key": keyPEM},
	})
	if err != nil {
		t.Fatal(err)
	}
	lbs := map[string]*topology.Balancer{}
	for _, l := range []string{"http", "https"} {
		if lbs[l], err = top.StartBalancer(ctx, l, topology.BalancerOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := lbs[l].WaitHealthy(ctx, nodes[0].Name(), nodes[1].Name()); err != nil {
			t.Fatalf("the %s balancer: %v", l, err)
		}
	}

	failures := &upgradeFailures{count: map[string]int{}}
	loads := []struct {
		name  string
		url   string
		proto loadgen.Protocol
	}{
		{"HTTP/1.1 on 8080", "http://" + lbs["http"].Addr() + "/", loadgen.HTTP1},
		{"h2c on 8080", "http://" + lbs["http"].Addr() + "/", loadgen.H2C},
		{"HTTP/2 on 8443", "https://" + lbs["https"].Addr() + "/", loadgen.H2},
		{"HTTP/1.1 over TLS on 8443", "https://" + lbs["https"].Addr() + "/", loadgen.HTTP1},
	}
	gens := make([]*loadgen.Generator, len(loads))
	for i, l := range loads {
		target, err := loadgen.NewHTTP(loadgen.HTTPConfig{URL: l.url, Protocol: l.proto, TLS: top.ClientTLS()})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = target.Close() })
		name := l.name
		if gens[i], err = loadgen.Start(ctx, target, loadgen.Config{
			Rate: upgradeRate / float64(len(loads)), Duration: upgradeDuration,
			OnResult: func(s loadgen.Sample) { failures.record(name, s) },
		}); err != nil {
			t.Fatal(err)
		}
	}

	// 10 s in: every generator has scheduled its first 10 s of requests.
	due := int64(upgradeRate / float64(len(loads)) * upgradeAt.Seconds())
	waitFor(t, "10 s of load", func() bool {
		for _, g := range gens {
			if g.Progress().Scheduled < due {
				return false
			}
		}
		return true
	})
	upgrade(ctx, t, top, nodes[0])

	for i, g := range gens {
		res, err := g.Wait()
		if err != nil {
			t.Fatalf("%s: %v", loads[i].name, err)
		}
		m := res.Measured
		t.Logf("%s: %d offered, %d sent, %d succeeded", loads[i].name, m.Offered, m.Sent(), m.Count(loadgen.Success))
		if m.Offered == 0 || m.Count(loadgen.Success) != m.Offered {
			t.Errorf("%s: %d of %d requests succeeded", loads[i].name, m.Count(loadgen.Success), m.Offered)
		}
	}
	// The new process took its share of the load after the handover: the
	// requests its own counters show, through its admin port.
	resp, err := nodes[0].AdminAs(ctx, "/metrics", topology.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	set, err := promtext.ParseBytes(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	served := set.Sum("ruralz_http_requests_total", promtext.L("route", "plain"))
	t.Logf("%s's new process served %.0f requests", nodes[0].Name(), served)
	if served == 0 {
		t.Errorf("%s's new process served no request after the handover", nodes[0].Name())
	}
	failures.mu.Lock()
	defer failures.mu.Unlock()
	for k, n := range failures.count {
		t.Errorf("%d requests ended %s", n, k)
	}
	for _, f := range failures.first {
		t.Log(f)
	}
}

// upgrade hands the Node n over to a new process of the same binary, as
// an in-place Zero-Downtime Upgrade does, moving the topology's clock a
// Drain step at a time while the processes hand over, the old one lingers
// and drains, until the old process exited 0 and the new one holds the
// lock and serves n's Revision.
func upgrade(ctx context.Context, t *testing.T, top *topology.Topology, n *topology.Node) {
	t.Helper()
	old := n.Process()
	successor, err := n.Handover(ctx, topology.HandoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%s hands over from pid %d to pid %d under load", n.Name(), old.PID(), successor.PID())
	paced := func(wait func() error) error {
		return advancingEvery(ctx, top, topology.DrainStep, topology.DrainStepWait, wait)
	}
	if err := paced(func() error { return n.WaitHolder(ctx, successor) }); err != nil {
		t.Fatalf("the handover: %v", err)
	}
	var code int
	if err := paced(func() error {
		wctx, cancel := context.WithTimeout(ctx, waitTimeout)
		defer cancel()
		var err error
		code, err = old.Wait(wctx)
		return err
	}); err != nil {
		t.Fatalf("the old process did not end its Drain: %v", err)
	}
	if code != 0 {
		t.Errorf("the old process exited %d after the handover, want 0", code)
	}
	if err := n.WaitRevision(ctx, n.TestDigest()); err != nil {
		t.Errorf("the new process: %v", err)
	}
	if p := n.Process(); p != successor || successor.PID() == old.PID() {
		t.Errorf("%s is served by %v, want the successor", n.Name(), p)
	}
}
