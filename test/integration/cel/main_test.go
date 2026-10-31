// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package cel

import (
	"context"
	"errors"
	"fmt"
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
	dir, err := os.MkdirTemp("", "rz-cel-bin-")
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

// reuseWindow is how long the Node reuses one metric collection, on its
// clock (docs/architecture/10-observability.md "Observability
// principles"); a scrape that must see every change moves the clock past
// it first.
const reuseWindow = time.Second

// scrape returns the Node's /metrics after moving the topology's clock
// past the collection reuse window, so it shows every record made before
// the call.
func scrape(t *testing.T, top *topology.Topology, n *topology.Node) promtext.Set {
	t.Helper()
	if err := top.Advance(t.Context(), reuseWindow+time.Millisecond); err != nil {
		t.Fatal(err)
	}
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
