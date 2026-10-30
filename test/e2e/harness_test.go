// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Bounds of the suite's real-time waits.
const (
	// cliTimeout bounds one ruralz command that does not stream.
	cliTimeout = 2 * time.Minute
	// waitTimeout bounds a condition the suite waits for: an activation,
	// a log line, a request the harness expects to reach a mock.
	waitTimeout = 60 * time.Second
	// pollInterval spaces the suite's condition polls; it never decides
	// an outcome.
	pollInterval = 20 * time.Millisecond
)

// revisionPipeline is the configuration pipeline the harness computes
// Revision and test digests with, as ruralzd computes them: the offline
// Policy checks and the CEL compiler (docs/engineering/03-testing-and-
// quality-strategy.md "Process harness", Nodes).
var revisionPipeline = sync.OnceValues(func() (*pipeline.Pipeline, error) {
	celc, err := cel.New()
	if err != nil {
		return nil, err
	}
	return pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc})
})

// revisionDigest returns the Revision digest of a one-file Bundle as a
// file-mode Node loads it: a rendered Bundle substitutes nothing, so no
// variable is set.
func revisionDigest(rendered []byte) (string, error) {
	p, err := revisionPipeline()
	if err != nil {
		return "", err
	}
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
			fmt.Fprintf(&b, "\n%s %s: %s", d.Code, d.Path, d.Message)
		}
		return "", fmt.Errorf("the Bundle renders no Revision:%s", b.String())
	}
	return res.Validated.Revision.Digest.String(), nil
}

// requireTestClock skips t unless the ruralzd under test is the
// controllable-clock build: the end-to-end suite asserts correctness in
// controlled time (docs/engineering/03-testing-and-quality-strategy.md
// "Hermetic rule scope"), and a release build that RURALZ_TEST_BIN_DIR
// names runs in real time.
func requireTestClock(t testing.TB) {
	t.Helper()
	if !built.TestClock {
		t.Skip("the ruralzd under test is not the controllable-clock build (ruralz_testclock)")
	}
}

// newTopology returns a process harness topology over the built ruralzd
// for t, with c's other settings.
func newTopology(t testing.TB, c topology.Config) *topology.Topology {
	t.Helper()
	requireTestClock(t)
	c.Binary, c.TestClock, c.Digest = built.Ruralzd, built.TestClock, revisionDigest
	return topology.New(t, c)
}

// advancing runs wait while it moves the topology's clock step every
// pollInterval of real time, so the Nodes' timers, such as the source
// poll, a refresh or the span processor's batch interval, fire while the
// suite waits.
func advancing(ctx context.Context, top *topology.Topology, step time.Duration, wait func() error) error {
	return advancingEvery(ctx, top, step, pollInterval, wait)
}

// advancingEvery runs wait while it moves the topology's clock step every
// every of real time.
func advancingEvery(ctx context.Context, top *topology.Topology, step, every time.Duration, wait func() error) error {
	stop, done := make(chan struct{}), make(chan struct{})
	var advErr error
	go func() {
		defer close(done)
		tick := time.NewTicker(every)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := top.Advance(ctx, step); err != nil {
					advErr = err
					return
				}
			}
		}
	}()
	err := wait()
	close(stop)
	<-done
	if err == nil && advErr != nil && !errors.Is(advErr, context.Canceled) {
		err = advErr
	}
	return err
}

// waitFor polls cond every pollInterval until it holds, failing t after
// waitTimeout of real time.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", waitTimeout, what)
		}
		time.Sleep(pollInterval)
	}
}

// writeFile writes data to name under dir, creating its directories, and
// returns its path.
func writeFile(t testing.TB, dir, name string, data []byte) string {
	t.Helper()
	p := filepath.Join(dir, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// readFile reads a file the suite or a process under test wrote.
func readFile(t testing.TB, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: a file under the test's own directories
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// caFile writes the harness's CA, which issues the admin and mock
// certificates, to a file for --ca-file and returns its path.
func caFile(t testing.TB, top *topology.Topology) string {
	t.Helper()
	return writeFile(t, t.TempDir(), "ca.pem", top.CA().CertPEM)
}

// cliOptions adjust one ruralz invocation.
type cliOptions struct {
	// dir is the working directory; default a fresh temporary directory.
	dir string
	// env is added to the base environment.
	env []string
	// stdin is the standard input; default none.
	stdin io.Reader
	// extraFiles are inherited as file descriptors 3 and up.
	extraFiles []*os.File
	// timeout bounds the command; default cliTimeout.
	timeout time.Duration
}

// cliRun is the result of one ruralz invocation.
type cliRun struct {
	args           []string
	code           int
	stdout, stderr string
}

// String describes the invocation for a failure message.
func (r cliRun) String() string {
	return fmt.Sprintf("ruralz %s = exit %d\n--- stdout ---\n%s\n--- stderr ---\n%s", strings.Join(r.args, " "), r.code, r.stdout, r.stderr)
}

// cliEnv returns the environment of a ruralz process: nothing inherited
// but PATH, a private HOME and TMPDIR, and env, so a render without --env
// substitutes only defaults and the variables a test sets.
func cliEnv(t testing.TB, env []string) []string {
	t.Helper()
	tmp := t.TempDir()
	base := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + tmp, "TMPDIR=" + tmp}
	return append(base, env...)
}

// startCLI starts the built ruralz with args, its output captured, and
// returns the process; the process ends with the test at the latest.
func startCLI(t testing.TB, o cliOptions, args ...string) *proc.Process {
	t.Helper()
	dir := o.dir
	if dir == "" {
		dir = t.TempDir()
	}
	p, err := proc.Start(t.Context(), proc.Spec{
		Name: "ruralz", Path: built.Ruralz, Args: args, Env: cliEnv(t, o.env), Dir: dir,
		Stdin: o.stdin, ExtraFiles: o.extraFiles,
	})
	if err != nil {
		t.Fatalf("start ruralz %s: %v", strings.Join(args, " "), err)
	}
	t.Cleanup(func() {
		if p.State() == nil {
			_ = p.Kill()
			<-p.Done()
		}
	})
	return p
}

// finish waits for a ruralz process to exit, within timeout, and returns
// its result; its output goes to top's leak scan when top is not nil.
func finish(t testing.TB, top *topology.Topology, p *proc.Process, args []string, timeout time.Duration) cliRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	code, err := p.Wait(ctx)
	stdout, stderr := p.Output()
	if top != nil {
		top.Capture("cli", stdout)
		top.Capture("cli", stderr)
	}
	r := cliRun{args: args, code: code, stdout: string(stdout), stderr: string(stderr)}
	if err != nil {
		t.Fatalf("%v: %s", err, r)
	}
	return r
}

// cli runs the built ruralz with args to its end and returns its exit
// code and output, which go to top's leak scan when top is not nil.
func cli(t testing.TB, top *topology.Topology, o cliOptions, args ...string) cliRun {
	t.Helper()
	timeout := o.timeout
	if timeout == 0 {
		timeout = cliTimeout
	}
	return finish(t, top, startCLI(t, o, args...), args, timeout)
}

// lines splits output into lines without their newlines.
func lines(s string) []string {
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// revRE matches a display Revision and digestRE a full digest.
var (
	revRE    = regexp.MustCompile(`^rev-[0-9a-f]{12}$`)
	digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// displayRevision returns the rev-<12 hex> form of a full digest.
func displayRevision(t testing.TB, digest string) string {
	t.Helper()
	d := topology.DisplayRevision(digest)
	if d == "" {
		t.Fatalf("%q is not a sha256:<64 hex> digest", digest)
	}
	return d
}

// childTimeout bounds a test that inNetNS re-executes in a network
// namespace.
const childTimeout = 15 * time.Minute

// inNetNS reports whether t runs inside the network namespace its test
// needs: a fresh namespace holding only a loopback interface, brought up,
// with sysctls written there. In the parent it re-executes the test binary
// for t alone inside such a namespace, with the binaries TestMain took
// (RURALZ_TEST_BIN_DIR), waits for it and returns false; t then passes,
// fails or skips as the child did. Creating a namespace needs root (or
// CAP_SYS_ADMIN), which stage 10 has; without it t skips with the reason.
func inNetNS(t *testing.T, sysctls map[string]string) bool {
	t.Helper()
	if proc.NetNSChild(t) {
		return true
	}
	requireTestClock(t)
	var out bytes.Buffer
	_, err := proc.RunInNetNS(t.Context(), proc.NetNSOptions{
		TestName: t.Name(), Sysctls: sysctls,
		Env:    []string{binaries.EnvBinDir + "=" + builtDir},
		Args:   []string{"-test.timeout=" + childTimeout.String()},
		Output: &out,
	})
	if testing.Verbose() {
		t.Logf("the network namespace child's output:\n%s", out.Bytes())
	}
	switch {
	case errors.Is(err, proc.ErrChildSkipped):
		t.Skip(err)
	case errors.Is(err, syscall.EPERM):
		t.Skipf("creating a network namespace needs root or CAP_SYS_ADMIN, which stage 10 grants: %v", err)
	case err != nil:
		t.Fatal(err)
	}
	return false
}

// clientFor returns an HTTP client for requests to top's Nodes and mocks:
// it trusts the harness's CAs, with serverName as the TLS server name
// when set, follows no redirect, captures every response body it reads
// for the teardown leak scan and, with source set, dials from that
// loopback address, so a key on source.ip sees a client of its own.
func clientFor(t testing.TB, top *topology.Topology, source, serverName string) *http.Client {
	t.Helper()
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	if source != "" {
		ip := net.ParseIP(source)
		if ip == nil || !ip.IsLoopback() {
			t.Fatalf("source %q is not a loopback address", source)
		}
		dialer.LocalAddr = &net.TCPAddr{IP: ip}
	}
	tlsc := top.ClientTLS()
	tlsc.ServerName = serverName
	base := &http.Transport{TLSClientConfig: tlsc, ForceAttemptHTTP2: true, DialContext: dialer.DialContext}
	t.Cleanup(base.CloseIdleConnections)
	return &http.Client{
		Transport:     capturingTransport{base: base, top: top},
		Timeout:       waitTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// capturingTransport sends requests through base and hands every response
// body it reads to top's leak scan when the body is closed.
type capturingTransport struct {
	base http.RoundTripper
	top  *topology.Topology
}

func (c capturingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &capturedBody{rc: resp.Body, top: c.top, source: "client-" + r.URL.Host}
	return resp, nil
}

// capturedBody records what is read from a response body, at Close.
type capturedBody struct {
	rc     io.ReadCloser
	top    *topology.Topology
	source string
	buf    bytes.Buffer
	once   sync.Once
}

func (b *capturedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf.Write(p[:n])
	return n, err
}

func (b *capturedBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.top.Capture(b.source, b.buf.Bytes()) })
	return err
}

// firstDifference describes where two texts first differ.
func firstDifference(a, b string) string {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			lo := max(0, i-40)
			return fmt.Sprintf("at byte %d: %q vs %q", i, a[lo:min(len(a), i+40)], b[lo:min(len(b), i+40)])
		}
	}
	if len(a) != len(b) {
		k := min(len(a), len(b))
		return fmt.Sprintf("one text ends at byte %d: %q vs %q", k, a[k:], b[k:])
	}
	return "equal"
}

// jsonLines splits NDJSON output into lines, skipping empty ones.
func jsonLines(b []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, l)
		}
	}
	return out
}
