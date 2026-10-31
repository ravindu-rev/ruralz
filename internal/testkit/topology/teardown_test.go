// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// Tests for the teardown: the secret leak scan over every byte the
// harness captured, and the evidence of a failed test
// (docs/engineering/03-testing-and-quality-strategy.md "End-to-end tests"
// and "Process harness").

// At teardown a canary in a Node's log, a capture, a client response, a
// mock Upstream's request log or a data directory fails the test, and the
// failure keeps its evidence.
func TestTeardownScansForLeaks(t *testing.T) {
	artifacts := t.TempDir()
	rtb := &recordingTB{TB: t}
	var topo *Topology
	var node *Node
	// Registered before New, so it runs after the teardown.
	t.Cleanup(func() {
		if node == nil {
			return // the test failed before
		}
		got := rtb.recorded()
		for _, source := range []string{
			"node-1/logs/node-1.out: canary (raw)",
			"captures/capture-cli: canary (raw)",
			"node-1/data/leaked: canary (raw)",
			"captures/mock-upstream-1-orders: canary (raw)",
			"captures/client-127.0.0.1_",
		} {
			if !strings.Contains(got, "secret leak: "+source) {
				t.Errorf("the teardown reported no leak in %s:\n%s", source, got)
			}
		}
		// The failed test kept its evidence.
		if _, err := os.Stat(filepath.Join(artifacts, fileName(t.Name()), node.Name(), "logs", node.Name()+".out")); err != nil {
			t.Errorf("no log evidence: %v", err)
		}
	})
	cfg := standinCfg(t)
	cfg.Env = append(cfg.Env, standinLeakEnv+"=1")
	cfg.ArtifactDir = artifacts
	topo = New(rtb, cfg)
	node = addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	waitLine(t, node, `standin: operator token `)
	topo.Capture("cli", []byte("ruralz printed "+node.MetricsToken()))
	if err := os.WriteFile(filepath.Join(node.DataDir(), "leaked"), []byte(topo.Canary()), 0o600); err != nil {
		t.Fatal(err)
	}
	up, err := topo.StartMockUpstream(mockup.Config{Name: "orders", Behavior: mockup.Behavior{Echo: true}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, up.URL()+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Leak", topo.Canary())
	resp, err := topo.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The echo carries the header back: a client response leak, captured
	// as the body is read.
	var body bytes.Buffer
	_, _ = body.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	if !topo.canaries.carries(body.Bytes()) {
		t.Fatalf("the echo %q carries no canary", body.Bytes())
	}
}

// A failed test's evidence holds each process's logs, a listing of each
// RURALZ_DATA_DIR, each Node's /debug/snapshots, the OTLP records and the
// balancer's transition log; a passing test keeps none.
func TestFailureEvidence(t *testing.T) {
	artifacts := t.TempDir()
	rtb := &recordingTB{TB: t}
	var node *Node
	t.Cleanup(func() {
		if node == nil {
			return // the test failed before
		}
		base := filepath.Join(artifacts, fileName(t.Name()))
		nd := filepath.Join(base, node.Name())
		if log, err := os.ReadFile(filepath.Join(nd, "logs", node.Name()+".out")); err != nil || !bytes.Contains(log, []byte("standin: ready")) { //nolint:gosec // G304: evidence the teardown wrote under t.TempDir
			t.Errorf("log evidence %q, %v", log, err)
		}
		if listing, err := os.ReadFile(filepath.Join(nd, "data-dir.txt")); err != nil || !bytes.Contains(listing, []byte("identity/node-id")) { //nolint:gosec // G304: evidence the teardown wrote under t.TempDir
			t.Errorf("data directory listing %q, %v", listing, err)
		}
		var snaps adminapi.Snapshots
		data, err := os.ReadFile(filepath.Join(nd, "debug-snapshots.json")) //nolint:gosec // G304: evidence the teardown wrote under t.TempDir
		if err == nil {
			err = json.Unmarshal(data, &snaps)
		}
		if err != nil || len(snaps.Snapshots) != 1 || snaps.Snapshots[0].Revision != DisplayRevision(node.TestDigest()) {
			t.Errorf("/debug/snapshots evidence %s, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(base, "otlp-1", "summary.txt")); err != nil {
			t.Errorf("OTLP evidence: %v", err)
		}
		if log, err := os.ReadFile(filepath.Join(base, "balancer-http.txt")); err != nil || !bytes.Contains(log, []byte("initial -> healthy")) { //nolint:gosec // G304: evidence the teardown wrote under t.TempDir
			t.Errorf("balancer evidence %q, %v", log, err)
		}
		if got := rtb.recorded(); !strings.Contains(got, "forced failure") || strings.Contains(got, "secret leak") {
			t.Errorf("recorded %q, want the forced failure alone", got)
		}
	})
	cfg := standinCfg(t)
	cfg.ArtifactDir = artifacts
	topo := New(rtb, cfg)
	node = addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if _, err := topo.StartOTLPSink(otlpsink.Config{}); err != nil {
		t.Fatal(err)
	}
	lb, err := topo.StartBalancer(t.Context(), "http", BalancerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.WaitHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	rtb.Errorf("forced failure")

	passing := t.TempDir()
	quiet := &recordingTB{TB: t}
	t.Cleanup(func() {
		if entries, err := os.ReadDir(passing); err != nil || len(entries) != 0 {
			t.Errorf("a passing test kept evidence: %v %v", entries, err)
		}
	})
	qcfg := standinCfg(t)
	qcfg.ArtifactDir = passing
	q := New(quiet, qcfg)
	addNodes(t, q, 1, nodeOptions(t, q))
}

// Without Config.ArtifactDir a failed test's evidence goes under
// RURALZ_TEST_ARTIFACT_DIR as the teardown finds it.
func TestFailureEvidenceUnderTheEnvironmentDirectory(t *testing.T) {
	artifacts := t.TempDir()
	t.Setenv(ArtifactDirEnv, artifacts)
	rtb := &recordingTB{TB: t}
	var node *Node
	// Registered before New, so it runs after the teardown.
	t.Cleanup(func() {
		if node == nil {
			return // the test failed before
		}
		if _, err := os.Stat(filepath.Join(artifacts, fileName(t.Name()), node.Name(), "logs", node.Name()+".out")); err != nil {
			t.Errorf("no evidence under %s: %v", ArtifactDirEnv, err)
		}
	})
	topo := New(rtb, standinCfg(t))
	node = addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	rtb.Errorf("forced failure")
}
