// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

// Package protocol is the protocol conformance suite
// (docs/engineering/03-testing-and-quality-strategy.md "Conformance
// suites"): the M1 HTTP contract of Ruralz Gateway, checked over real
// sockets against a built ruralzd through its listeners. It is versioned
// with the release: the suite at a tag states that release's contract, and
// each case names the document rule it checks.
//
// # The Node under test
//
// TestMain builds ruralzd once through internal/testkit/binaries, as the
// controllable-clock build (the build tag ruralz_testclock), or takes the
// build RURALZ_TEST_BIN_DIR names. Every test runs its Nodes on the
// process harness, internal/testkit/topology: a private test render, admin
// TLS, and one clock the test advances. Against a release build in
// RURALZ_TEST_BIN_DIR the topology runs in real time, and the cases that
// need the controllable clock skip with a reason (needsTestClock).
//
// # Cases
//
// Wire rules: one case per rule of docs/architecture/03-data-plane.md
// "Listeners and protocols" and "Error response format",
// docs/architecture/08-security-and-identity.md "Request hardening" and
// docs/architecture/07-multi-protocol.md "HTTP intermediary behavior", on
// every client protocol: HTTP/1.1, HTTP/1.1 over TLS, h2c and HTTP/2 over
// TLS. A case whose behavior crosses the hop (framing, trailers,
// hop-by-hop fields, 1xx) runs on every pairing with an http Upstream over
// HTTP/1.1, HTTP/1.1 over TLS and HTTP/2 over TLS.
//
// Phase mapping: the HTTP column of docs/architecture/07-multi-protocol.md
// "Filter Chain applicability per protocol", read from the ruralz.filter
// spans an OTLP sink receives and the access record onLog writes.
//
// HTTP/2 frames: Ruralz-written cases on the golang.org/x/net/http2
// Framer for the HTTP/2 rows of docs/architecture/03-data-plane.md
// "Bounded resources" and the settings the Node advertises, and the one
// GOAWAY of a Drain with the rest of its listener shutdown.
//
// Timeouts: ReadHeaderTimeout, IdleTimeout, the body read and write
// deadlines and the pre-routing write deadline, in real time and in
// parallel, each expiry asserted between its value and its value plus 2 s:
// the Go runtime enforces socket deadlines on the real clock in every
// build.
//
// The clients are the suite's own: raw HTTP/1.x connections and an HTTP/2
// client on the Framer, so a case can send what net/http's client never
// would and see every interim response, trailer field and frame. The
// Upstreams are in-process net/http servers (origin) that record what
// crossed the hop.
package protocol

import (
	"context"
	"debug/buildinfo"
	"errors"
	"fmt"
	"go/version"
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
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// node is the ruralzd under test, set once by TestMain before any test
// runs and only read afterwards.
var node binaries.Set

// TestMain builds ruralzd once for the whole suite: the controllable-clock
// build of this commit, or the build RURALZ_TEST_BIN_DIR names.
func TestMain(m *testing.M) {
	os.Exit(runSuite(m))
}

// runSuite builds the Node binary into a temporary directory that lives as
// long as the tests and returns their exit code.
func runSuite(m *testing.M) int {
	dir, err := os.MkdirTemp("", "rz-protocol-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "protocol: %v\n", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "protocol: %v\n", err)
		return 1
	}
	node, err = binaries.Build(context.Background(), root, dir, binaries.BuildOptions{
		TestClock: true, Commands: []string{binaries.Ruralzd},
	})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "protocol: %v\n", err)
		return 1
	}
	return m.Run()
}

// needsTestClock skips a case that holds a Node timer still or moves it
// on command, against a ruralzd that runs on the real clock.
func needsTestClock(t testing.TB) {
	t.Helper()
	if !node.TestClock {
		t.Skipf("needs the controllable-clock ruralzd build; %s names a build on the real clock", binaries.EnvBinDir)
	}
}

// strictHTTP2ContentLength reports whether the Node under test runs the
// HTTP/2 server of Go 1.27's net/http, which resets the stream of a
// request whose content-length field lines differ, or that declares
// content on a stream that ends with its HEADERS; the Go 1.26 floor's
// server lets both through to the handler, whose framing check answers
// them (docs/architecture/07-multi-protocol.md "HTTP intermediary
// behavior"). It reads the Go release from the binary's build
// information.
func strictHTTP2ContentLength(t testing.TB) bool {
	t.Helper()
	bi, err := buildinfo.ReadFile(node.Ruralzd)
	if err != nil {
		t.Fatalf("build information of %s: %v", node.Ruralzd, err)
	}
	// A build with experiments reports them after the release, such as
	// "go1.26.8 X:jsonv2".
	release, _, _ := strings.Cut(bi.GoVersion, " ")
	return version.Compare(release, "go1.27") >= 0
}

// newTopology returns the process harness of t over the Node under test.
func newTopology(t testing.TB) *topology.Topology {
	t.Helper()
	return topology.New(t, topology.Config{Binary: node.Ruralzd, TestClock: node.TestClock, Digest: revisionDigest(t)})
}

// pipelineEpoch is the fixed clock of the digest pipeline's yield timers,
// which never decide a digest.
func pipelineEpoch() time.Time { return time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) }

// revisionDigest returns the topology's digest function: the Revision
// digest the configuration pipeline computes for a one-file Bundle, as a
// file-mode Node loads it without ${VAR} values.
func revisionDigest(t testing.TB) func([]byte) (string, error) {
	t.Helper()
	celc, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: pipelineEpoch})
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
			return "", fmt.Errorf("the Bundle renders no Revision: %v", res.Diagnostics)
		}
		return res.Validated.Revision.Digest.String(), nil
	}
}

// Clock steps of advancing.
const (
	// advanceStep is how far advancing moves the Node clock at a time.
	advanceStep = 500 * time.Millisecond
	// advanceEvery is the real time between two steps.
	advanceEvery = 20 * time.Millisecond
)

// advancing runs wait while it moves the topology's clock advanceStep
// every advanceEvery of real time, so Node timers such as a retry backoff
// or the span batch interval fire while the test waits. A topology on the
// real clock only runs wait.
func advancing(ctx context.Context, top *topology.Topology, wait func() error) error {
	if !top.TestClock() {
		return wait()
	}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(advanceEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-tick.C:
				if err := top.Advance(ctx, advanceStep); err != nil && !errors.Is(err, context.Canceled) {
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

// guard returns a context that ends a hung exchange: a hang guard only,
// never what a case asserts.
func guard(t testing.TB, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), d)
	t.Cleanup(cancel)
	return ctx
}

// exchangeGuard bounds one request and response of a case.
const exchangeGuard = 30 * time.Second
