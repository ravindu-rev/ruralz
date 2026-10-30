// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// bins is the ruralzd under test, built once by TestMain: the
// controllable-clock build of this commit, or the build
// RURALZ_TEST_BIN_DIR names.
var bins binaries.Set

func TestMain(m *testing.M) { os.Exit(runSuite(m)) }

// runSuite builds the binaries, runs the tests and removes the build.
func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "rz-telemetry-bin-")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	bins, err = binaries.Build(context.Background(), root, dir, binaries.BuildOptions{
		TestClock: true, Commands: []string{binaries.Ruralzd},
	})
	if err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return m.Run()
}

// requireClock skips a test that asserts in controlled time against a
// ruralzd without the controllable clock, such as a release build that
// RURALZ_TEST_BIN_DIR names.
func requireClock(t *testing.T) {
	t.Helper()
	if !bins.TestClock {
		t.Skip("needs the controllable-clock ruralzd build; " + binaries.EnvBinDir + " names a build without it")
	}
}

// epoch is the fixed time of the test's configuration pipeline.
func epoch() time.Time { return time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) }

// revisionDigest returns the topology's digest function: the Revision
// digest the configuration pipeline computes for a one-file Bundle, as a
// file-mode Node loads it without ${VAR} values.
func revisionDigest(t *testing.T) func([]byte) (string, error) {
	t.Helper()
	celc, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	return func(rendered []byte) (string, error) {
		res, err := p.Run(context.Background(), pipeline.Request{
			Source:    loader.Source{FS: fstest.MapFS{"ruralz.yaml": &fstest.MapFile{Data: rendered}}, Path: "ruralz.yaml"},
			Variables: subst.Map{},
		})
		if err != nil {
			return "", err
		}
		if res.Validated == nil {
			var b strings.Builder
			for _, d := range res.Diagnostics {
				b.WriteString(string(d.AppendText(nil)))
				b.WriteByte('\n')
			}
			return "", errors.New("the Bundle renders no Revision:\n" + b.String())
		}
		return res.Validated.Revision.Digest.String(), nil
	}
}

// newTopology returns a topology over the ruralzd under test.
func newTopology(t *testing.T, c topology.Config) *topology.Topology {
	t.Helper()
	c.Binary, c.TestClock, c.Digest = bins.Ruralzd, bins.TestClock, revisionDigest(t)
	return topology.New(t, c)
}

// startNode adds one Node serving o and waits until it is ready.
func startNode(t *testing.T, top *topology.Topology, o topology.NodeOptions) *topology.Node {
	t.Helper()
	nodes, err := top.AddNodes(t.Context(), 1, o)
	if err != nil {
		t.Fatal(err)
	}
	return nodes[0]
}

// reuseWindow is how long the Node reuses one metric collection, on its
// clock (docs/architecture/10-observability.md "Observability
// principles"); a scrape that must see every change moves the clock past
// it first.
const reuseWindow = time.Second

// scrape returns the Node's /metrics after moving the topology's clock
// past the collection reuse window, so it shows every record made before
// the call; on a real-time topology it waits the window out.
func scrape(t *testing.T, top *topology.Topology, n *topology.Node) promtext.Set {
	t.Helper()
	if top.TestClock() {
		if err := top.Advance(t.Context(), reuseWindow+time.Millisecond); err != nil {
			t.Fatal(err)
		}
	} else {
		time.Sleep(reuseWindow + time.Millisecond)
	}
	return scrapeNow(t, n)
}

// scrapeNow returns the Node's /metrics without moving the clock: the
// collection may be one the Node reuses.
func scrapeNow(t *testing.T, n *topology.Node) promtext.Set {
	t.Helper()
	resp, err := n.AdminAs(t.Context(), "/metrics", topology.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /metrics = %d: %s", resp.Status, resp.Body)
	}
	set, err := promtext.ParseBytes(resp.Body)
	if err != nil {
		t.Fatalf("parse /metrics: %v", err)
	}
	return set
}

// value returns the sample of name with labels, failing the test when
// absent.
func value(t *testing.T, set promtext.Set, name string, labels ...string) float64 {
	t.Helper()
	v, err := set.Value(name, promtext.L(labels...))
	if err != nil {
		t.Fatalf("%s%v: %v", name, labels, err)
	}
	return v
}

// advancing runs wait while moving the topology's clock step every
// interval of real time, so the Nodes' timers, such as the source poll,
// the batch intervals and the metric push, fire while the test waits on a
// condition.
func advancing(ctx context.Context, top *topology.Topology, step, interval time.Duration, wait func() error) error {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(interval)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := top.Advance(ctx, step); err != nil {
					return
				}
			}
		}
	}()
	err := wait()
	close(stop)
	<-done
	return err
}

// eventually polls cond every 20 ms of real time until it holds, failing
// the test after timeout.
func eventually(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within %v", what, timeout)
		}
		time.Sleep(topology.PollInterval)
	}
}

// get sends GET url with client and returns the status; a failed status
// comes with its body in the error.
func get(ctx context.Context, client *http.Client, url string, header http.Header) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	for k, vs := range header {
		req.Header[k] = vs
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		return resp.StatusCode, fmt.Errorf("status %d: %s", resp.StatusCode, body)
	}
	return resp.StatusCode, nil
}

// gatewayDoc is a Gateway named edge with one cleartext listener web on
// 8080, admin on 9901, and the given telemetry block (YAML lines indented
// under telemetry:, "" for none).
func gatewayDoc(telemetry string) string {
	doc := "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n" +
		"  admin:\n    port: 9901\n" +
		"  listeners:\n    - name: web\n      protocol: http\n      port: 8080\n"
	if telemetry != "" {
		doc += "  telemetry:\n" + telemetry
	}
	return doc
}

// upstreamDoc is an http Upstream with one static Endpoint.
func upstreamDoc(name, address string) string {
	return "---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: " + name +
		"\nspec:\n  protocol: http\n  endpoints:\n    - address: \"" + address + "\"\n"
}

// routeDoc is a Route matching the path prefix to upstream, with a
// whole-request timeout.
func routeDoc(name, prefix, upstream, timeout string) string {
	doc := "---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: " + name +
		"\nspec:\n  match:\n    path:\n      prefix: " + prefix + "\n  upstreams:\n    - name: " + upstream + "\n"
	if timeout != "" {
		doc += "  timeout: " + timeout + "\n"
	}
	return doc
}

// otlpBlock is a telemetry block exporting to endpoint, sampling every
// trace, with extra lines appended (indented four spaces).
func otlpBlock(endpoint string, extra ...string) string {
	b := "    otlp:\n      endpoint: " + endpoint + "\n"
	for _, e := range extra {
		b += e
	}
	return b + "    traceSampling: 1\n"
}
