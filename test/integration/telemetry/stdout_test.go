// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package telemetry

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Blocked stdout run (docs/engineering/03-testing-and-quality-strategy.md
// "Integration tests", telemetry; targets).
const (
	// blockedFor is how long the Node's stdout goes unread under load.
	blockedFor = 60 * time.Second
	// unblockedFor is the unblocked run the blocked one is compared with.
	unblockedFor = 20 * time.Second
	// loadRate is the offered load of both runs, in requests per second.
	loadRate = 200
	// exitBound is the Drain's exit bound after a stop signal.
	exitBound = 30 * time.Second
)

// noisyBundle is a Bundle whose every request writes an access record and,
// at the debug level, a process record: the Route's open headers Policy
// has a when that fails at runtime, which skips the Policy and logs why.
// otlp is the telemetry block.
func noisyBundle(telemetry, upstream string) string {
	return gatewayDoc(telemetry) + upstreamDoc(upstream) +
		"---\napiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata:\n  name: noisy\nspec:\n  type: headers\n  failureMode: open\n" +
		"  when: 'request.headers[\"x-absent\"] == \"y\"'\n  config:\n    response:\n      set:\n        - name: x-noisy\n          value: \"1\"\n" +
		"---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: work\nspec:\n  match:\n    path:\n      prefix: /work\n" +
		"  upstreams:\n    - name: u\n  policies:\n    - name: noisy\n"
}

// logsOnly is a telemetry block exporting the logs and metrics to
// endpoint and sampling no trace.
func logsOnly(endpoint string) string {
	return "    otlp:\n      endpoint: " + endpoint + "\n    traceSampling: 0\n"
}

// gatewayHistogram returns ruralz_http_gateway_duration_seconds of the web
// listener.
func gatewayHistogram(t *testing.T, set promtext.Set) promtext.Histogram {
	t.Helper()
	h, err := set.Histogram(catalog.HTTPGatewayDurationSeconds, promtext.L("listener", "web"))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// histogramDelta returns the observations after made since before.
func histogramDelta(before, after promtext.Histogram) promtext.Histogram {
	prev := map[float64]float64{}
	for _, b := range before.Buckets {
		prev[b.UpperBound] = b.Count
	}
	d := promtext.Histogram{Count: after.Count - before.Count, Sum: after.Sum - before.Sum}
	for _, b := range after.Buckets {
		d.Buckets = append(d.Buckets, promtext.Bucket{UpperBound: b.UpperBound, Count: b.Count - prev[b.UpperBound]})
	}
	return d
}

// accessRecords counts the access records the collector received.
func accessRecords(sink *otlpsink.Sink) int {
	return len(sink.FindLogs(func(l otlpsink.LogRecord) bool { return l.EventName() == "ruralz.access" }))
}

// TestBlockedStdout leaves the Node's stdout unread for 60 s under load:
// every request completes, both log streams count their stdout losses
// with sink stdout and reason queue_full, OTLP log export goes on with no
// otlp loss, the run records its gateway-added p99 against an unblocked
// run, and output resumes once stdout is read again.
func TestBlockedStdout(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{Env: []string{"RURALZ_LOG_LEVEL=debug"}})
	sink, err := top.StartOTLPSink(otlpsink.Config{})
	if err != nil {
		t.Fatal(err)
	}
	mock, err := top.StartMockUpstream(mockup.Config{LogSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(noisyBundle(logsOnly(sink.URL()), mock.Addr()))})
	// The clock follows real time while load runs, so the batch interval
	// and the metric push go on as in production.
	run := func(d time.Duration) {
		if err := advancing(t.Context(), top, 100*time.Millisecond, 100*time.Millisecond, func() error {
			load(t, n, loadRate, d)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	start := scrape(t, top, n)
	run(unblockedFor)
	unblocked := scrape(t, top, n)
	p99Unblocked := histogramDelta(gatewayHistogram(t, start), gatewayHistogram(t, unblocked)).Quantile(0.99)

	unblock := n.Process().BlockStdout()
	t.Cleanup(unblock)
	exported := accessRecords(sink)
	run(blockedFor)
	blocked := scrape(t, top, n)
	p99Blocked := histogramDelta(gatewayHistogram(t, unblocked), gatewayHistogram(t, blocked)).Quantile(0.99)
	t.Logf("gateway-added p99: %v unblocked, %v with stdout blocked for %v at %d requests/s",
		seconds(p99Unblocked), seconds(p99Blocked), blockedFor, loadRate)

	for _, stream := range []string{"access", "process"} {
		v := promtext.Delta(unblocked, blocked, catalog.TelemetryLogsDroppedTotal,
			promtext.L("stream", stream, "sink", "stdout", "reason", "queue_full"))
		if v <= 0 {
			t.Errorf("%s records dropped from stdout with queue_full = %v while it was blocked, want some", stream, v)
		}
		for _, reason := range []string{"queue_full", "export_error"} {
			if v := value(t, blocked, catalog.TelemetryLogsDroppedTotal, "stream", stream, "sink", "otlp", "reason", reason); v != 0 {
				t.Errorf("%s records the OTLP sink lost with %s = %v, want none", stream, reason, v)
			}
		}
	}
	served := int(promtext.Delta(unblocked, blocked, catalog.HTTPRequestsTotal, promtext.L("route", "work", "status_class", "2xx")))
	if served < int(loadRate*blockedFor.Seconds()) {
		t.Fatalf("%d requests counted during the blocked run, want %d", served, int(loadRate*blockedFor.Seconds()))
	}
	// Every access record of the blocked run reaches the collector.
	eventually(t, "the blocked run's access records at the collector", func() bool {
		if err := top.Advance(t.Context(), 100*time.Millisecond); err != nil {
			t.Fatal(err)
		}
		return accessRecords(sink)-exported >= served
	})

	unblock()
	// A record that reaches the stdout sink while the blocked run's backlog
	// still fills its queue is dropped (queue_full), as designed, so
	// requests go on until the access record of one is written.
	marker := "/work/resumed-" + strconv.FormatInt(int64(served), 10)
	resumed := regexp.MustCompile(regexp.QuoteMeta(`"path":"` + marker + `"`))
	eventually(t, "stdout to resume once read again", func() bool {
		requests(t, top, n, marker, 1)
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		_, err := n.Process().WaitOutput(ctx, resumed)
		return err == nil
	})
}

// seconds formats a histogram value in seconds as a duration.
func seconds(v float64) time.Duration {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return -1
	}
	return time.Duration(v * float64(time.Second))
}

// exited reports whether p's process has exited: reaped, or a zombie the
// reaper has not collected yet, as when its stdout is still held.
func exited(p *proc.Process) bool {
	if p.State() != nil {
		return true
	}
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(p.PID()) + "/stat")
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	if err != nil {
		return false
	}
	// The state follows the parenthesized command name.
	i := bytes.LastIndexByte(stat, ')')
	return i >= 0 && i+2 < len(stat) && stat[i+2] == 'Z'
}

// waitDraining waits, in real time with the clock still, until the Node's
// /readyz answers 503 draining: the Drain has started, and its timeline
// runs from the clock's reading now.
func waitDraining(t *testing.T, n *topology.Node) {
	t.Helper()
	eventually(t, "/readyz 503 draining after SIGTERM", func() bool {
		doc, status, err := n.Readyz(t.Context())
		if err != nil || status != http.StatusServiceUnavailable {
			return false
		}
		for _, r := range doc.Reasons {
			if r.Reason == adminapi.ReasonDraining {
				return true
			}
		}
		return false
	})
}

// TestSIGTERMWhileStdoutBlocked sends SIGTERM to a Node whose stdout
// nobody reads, with lines waiting in its stdout queues: the flush gives
// up on stdout at its bound, so the process exits with exit 0 by the time
// the clock reaches the 30 s exit bound after the Drain started.
func TestSIGTERMWhileStdoutBlocked(t *testing.T) {
	requireClock(t)
	top := newTopology(t, topology.Config{Env: []string{"RURALZ_LOG_LEVEL=debug"}})
	mock, err := top.StartMockUpstream(mockup.Config{LogSize: -1})
	if err != nil {
		t.Fatal(err)
	}
	n := startNode(t, top, topology.NodeOptions{Bundle: []byte(noisyBundle("", mock.Addr()))})
	p := n.Process()
	unblock := p.BlockStdout()
	t.Cleanup(unblock)
	// Enough lines to fill the pipe and the Node's stdout queues.
	load(t, n, 400, 5*time.Second)
	if err := n.Signal(t.Context(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitDraining(t, n)
	const step = 500 * time.Millisecond
	var advanced time.Duration
	for advanced < exitBound+2*step && !exited(p) {
		if err := top.Advance(t.Context(), step); err != nil {
			t.Fatal(err)
		}
		advanced += step
		// The Node acts on each step before the next; an exit due at
		// this reading shows within the wait.
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) && !exited(p) {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if !exited(p) || advanced > exitBound {
		t.Fatalf("the Node exited = %v after %v of the clock since SIGTERM with stdout blocked, want an exit within the %v exit bound",
			exited(p), advanced, exitBound)
	}
	t.Logf("exited %v of the clock after SIGTERM with stdout blocked", advanced)
	unblock()
	code, err := p.Wait(t.Context())
	if err != nil || code != 0 {
		t.Errorf("exit = %d, %v; want 0", code, err)
	}
}
