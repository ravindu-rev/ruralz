// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package gateway

import (
	"context"
	"errors"
	"net/http"
	"os"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// builtNode returns the ruralzd under test: the build RURALZ_TEST_BIN_DIR
// names, else the controllable-clock build of this commit.
func builtNode(t *testing.T) binaries.Set {
	t.Helper()
	if dir := os.Getenv(binaries.EnvBinDir); dir != "" {
		set, err := binaries.FromDir(dir, binaries.Ruralzd)
		if err != nil {
			t.Fatal(err)
		}
		return set
	}
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	set, err := binaries.Build(t.Context(), root, t.TempDir(), binaries.BuildOptions{
		TestClock: true, Commands: []string{binaries.Ruralzd},
	})
	if err != nil {
		t.Fatal(err)
	}
	return set
}

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
			return "", errors.New("the Bundle renders no Revision")
		}
		return res.Validated.Revision.Digest.String(), nil
	}
}

// topologyBundle is a Bundle routing /orders to an Upstream that need not
// answer; the topology rewrites its ports.
func topologyBundle(prefix string) []byte {
	return []byte("apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  admin:\n    port: 9901\n" +
		"  listeners:\n    - name: web\n      protocol: http\n      port: 8080\n" +
		"---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n  endpoints:\n    - address: \"127.0.0.1:9\"\n" +
		"---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: orders\nspec:\n  match:\n    path:\n      prefix: " + prefix + "\n  upstreams:\n    - name: orders\n")
}

// advancing runs wait while it moves the topology's clock 500 ms every 20
// ms of real time, so the Nodes' timers, such as the source poll and the
// handover gate, fire while the harness waits.
func advancing(ctx context.Context, top *topology.Topology, wait func() error) error {
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := top.Advance(ctx, 500*time.Millisecond); err != nil {
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

// TestBuiltNodeUnderTopology drives the built ruralzd through the process
// harness: boot to ready, the authenticated admin answers with
// Ruralz-Version, a Hot Reload of a swapped render, a Zero-Downtime
// Upgrade to a successor on the same directory, and a Drain to exit 0.
func TestBuiltNodeUnderTopology(t *testing.T) {
	set := builtNode(t)
	ctx := t.Context()
	top := topology.New(t, topology.Config{Binary: set.Ruralzd, TestClock: set.TestClock, Digest: revisionDigest(t)})
	nodes, err := top.AddNodes(ctx, 1, topology.NodeOptions{Bundle: topologyBundle("/orders")})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]

	resp, err := n.Admin(ctx, admin.PathConfigDump)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK || resp.Header.Get(admin.HeaderVersion) == "" {
		t.Errorf("GET /config/dump = %d, %s %q", resp.Status, admin.HeaderVersion, resp.Header.Get(admin.HeaderVersion))
	}

	r, err := n.Reload(ctx, topologyBundle("/v2"))
	if err != nil {
		t.Fatal(err)
	}
	if err := advancing(ctx, top, func() error { return n.WaitRevision(ctx, r.TestDigest) }); err != nil {
		t.Fatalf("the swapped render did not activate: %v", err)
	}

	p, err := n.Handover(ctx, topology.HandoverOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := advancing(ctx, top, func() error { return n.WaitHolder(ctx, p) }); err != nil {
		t.Fatalf("the successor did not take the lock: %v", err)
	}
	if err := n.WaitRevision(ctx, r.TestDigest); err != nil {
		t.Fatalf("the successor does not serve the Revision: %v", err)
	}

	code, err := n.Stop(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if code != ExitOK {
		t.Errorf("ruralzd exited %d after SIGTERM, want %d", code, ExitOK)
	}
}
