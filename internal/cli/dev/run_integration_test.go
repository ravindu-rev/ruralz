// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && unix

package dev

import (
	"bufio"
	"context"
	"crypto/tls"
	debugbuildinfo "debug/buildinfo"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// Tests for ruralz dev run with the real ruralzd: the controllable-clock
// build of internal/testkit/binaries, whose clock the test advances beside
// the CLI's fake clock (docs/engineering/03-testing-and-quality-strategy.md
// "Hermetic rule scope"); the launcher hands it its clock command socket as
// file descriptor 3 (launch.Options.ExtraFiles).

// buildTimeout bounds the build of ruralzd.
const buildTimeout = 10 * time.Minute

// builtRuralzd holds the ruralzd TestMain built, or took from
// RURALZ_TEST_BIN_DIR.
var builtRuralzd binaries.Set

// TestMain builds ruralzd once per test binary as the controllable-clock
// build, and removes it when the tests end.
func TestMain(m *testing.M) { os.Exit(runTests(m)) }

func runTests(m *testing.M) int {
	dir, err := os.MkdirTemp("", "ruralz-dev-bin-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dev: %v\n", err)
		return 2
	}
	defer func() { _ = os.RemoveAll(dir) }()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dev: %v\n", err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), buildTimeout)
	defer cancel()
	builtRuralzd, err = binaries.Build(ctx, root, dir, binaries.BuildOptions{TestClock: true, Commands: []string{binaries.Ruralzd}})
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "dev: %v\n", err)
		return 2
	}
	return m.Run()
}

// binaryVersion returns the build version the linker set in the Go
// binary at path (internal/buildinfo's version), DefaultVersion when the
// build information names none.
func binaryVersion(t *testing.T, path string) string {
	t.Helper()
	info, err := debugbuildinfo.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	const marker = "/internal/buildinfo.version="
	for _, s := range info.Settings {
		if s.Key != "-ldflags" {
			continue
		}
		for _, f := range strings.Fields(s.Value) {
			if _, v, ok := strings.Cut(f, marker); ok {
				return v
			}
		}
	}
	return binaries.DefaultVersion
}

// nodeClock drives the child's clock over its command socket; the
// launcher closes the test's copy of the child's end once the child holds
// it, so the socket ends when the child exits.
type nodeClock struct {
	conn net.Conn
	r    *bufio.Reader
	gone bool
	// started is set once the start's answer was read, before the first
	// advance's.
	started bool
}

// commandSocket returns a connected stream socket pair: the test's end,
// and the child's, which the launcher passes as file descriptor 3. The
// test's end carries the start command first, at the real time of the
// call, which the child reads before its first reading.
func commandSocket(t *testing.T) (*nodeClock, *os.File) {
	t.Helper()
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	f := os.NewFile(uintptr(fds[0]), "clock-test")
	conn, err := net.FileConn(f)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	child := os.NewFile(uintptr(fds[1]), "clock-child")
	t.Cleanup(func() { _ = conn.Close(); _ = child.Close() })
	if _, err := io.WriteString(conn, clock.CommandStart+" "+time.Now().UTC().Format(clock.CommandTimeLayout)+"\n"); err != nil {
		t.Fatal(err)
	}
	return &nodeClock{conn: conn, r: bufio.NewReaderSize(conn, clock.MaxCommandBytes)}, child
}

// advance moves the child's clock by d and waits for its answer, under a
// real-time deadline; a child that has exited answers nothing, which ends
// the clock for good.
func (c *nodeClock) advance(t *testing.T, d time.Duration) {
	t.Helper()
	if c.gone {
		return
	}
	_ = c.conn.SetDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(c.conn, clock.CommandAdvance+" "+d.String()+"\n"); err != nil {
		c.gone = true
		return
	}
	if !c.started {
		line, err := c.r.ReadString('\n')
		if err != nil {
			c.gone = true
			return
		}
		if strings.HasPrefix(line, clock.AnswerError) {
			t.Fatalf("the child's clock refused its start: %s", line)
		}
		c.started = true
	}
	line, err := c.r.ReadString('\n')
	if err != nil {
		c.gone = true
		return
	}
	if strings.HasPrefix(line, clock.AnswerError) {
		t.Fatalf("the child's clock refused advance %v: %s", d, line)
	}
}

// devRun is one ruralz dev run with the real ruralzd.
type devRun struct {
	t     *testing.T
	clk   *clocktest.Fake
	node  *nodeClock
	inv   *invocation
	sigs  chan (chan<- os.Signal)
	tmp   string
	sigCh chan<- os.Signal
}

// startDevRun runs ruralz dev run --ruralzd <built> args... with an empty
// environment; mutate adjusts the options.
func startDevRun(t *testing.T, mutate func(*Options), args ...string) *devRun {
	t.Helper()
	if !builtRuralzd.TestClock {
		t.Skip("the ruralzd under test is not the controllable-clock build")
	}
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	clk := clocktest.New(epoch())
	node, child := commandSocket(t)
	d := &devRun{t: t, clk: clk, node: node, tmp: tmp, sigs: make(chan (chan<- os.Signal), 1)}
	o := testOptions(clk)
	o.Version = binaryVersion(t, builtRuralzd.Ruralzd)
	o.Launch.ExtraFiles = []*os.File{child}
	o.Notify = func(c chan<- os.Signal, _ ...os.Signal) { d.sigs <- c }
	o.StopNotify = func(chan<- os.Signal) {}
	if mutate != nil {
		mutate(&o)
	}
	d.inv = start(t, o, nil, append([]string{"dev", "run", "--ruralzd", builtRuralzd.Ruralzd}, args...)...)
	select {
	case d.sigCh = <-d.sigs:
	case <-time.After(30 * time.Second):
		t.Fatal("dev run registered no signal channel")
	}
	return d
}

// step is how far one drive step moves both clocks.
const step = 100 * time.Millisecond

// drive advances the CLI's clock and the child's in steps until cond
// holds, under a real-time deadline.
func (d *devRun) drive(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s\n--- stdout ---\n%s\n--- stderr ---\n%s", what, d.inv.stdout, d.inv.stderr)
		}
		d.clk.Advance(step)
		d.node.advance(d.t, step)
		time.Sleep(2 * time.Millisecond)
	}
}

// driveNode advances the child's clock alone, 1 s at a time, until cond
// holds, under a real-time deadline, and returns how far it moved it.
func (d *devRun) driveNode(what string, cond func() bool) time.Duration {
	d.t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	var moved time.Duration
	for !cond() {
		if time.Now().After(deadline) {
			d.t.Fatalf("timed out waiting for %s\n--- stdout ---\n%s\n--- stderr ---\n%s", what, d.inv.stdout, d.inv.stderr)
		}
		d.node.advance(d.t, time.Second)
		moved += time.Second
		time.Sleep(10 * time.Millisecond)
	}
	return moved
}

// exited reports whether the command has ended.
func (d *devRun) exited() bool {
	select {
	case code := <-d.inv.done:
		d.inv.done <- code
		return true
	default:
		return false
	}
}

// signal delivers a signal to the command, as the terminal or kill does.
func (d *devRun) signal(s os.Signal) { d.sigCh <- s }

// ready is the readiness block ruralz dev run prints.
type ready struct {
	revision, digest, testDigest, admin, tokenFile string
	listeners                                      map[string]string
	mapping                                        []string
	tap                                            string
}

// readyBlock waits for the readiness block and parses it.
func (d *devRun) readyBlock() ready {
	d.t.Helper()
	d.drive("the readiness block", func() bool { return strings.Contains(d.inv.stdout.String(), "ruralz dev tap --admin ") })
	r := ready{listeners: map[string]string{}}
	for _, l := range lines(d.inv.stdout.String()) {
		key, value, _ := strings.Cut(l, ": ")
		switch {
		case key == "revision":
			r.revision, r.digest, _ = strings.Cut(value, " ")
		case key == "testDigest":
			r.testDigest = value
		case key == "admin" && strings.HasPrefix(value, "http"):
			r.admin = value
		case key == "adminTokenFile":
			r.tokenFile = value
		case strings.HasPrefix(key, "listener "):
			r.listeners[strings.TrimPrefix(key, "listener ")] = value
		case strings.HasPrefix(l, "ruralz dev tap "):
			r.tap = l
		case strings.Contains(value, " -> "):
			r.mapping = append(r.mapping, l)
		}
	}
	return r
}

// upstream starts a mock Upstream on loopback that answers 200 "hello".
func upstream(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// devBundle is a one-file Bundle routing every request to the Upstream at
// address, with a headers Policy setting x-dev to value.
func devBundle(address, value string) string {
	return `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: dev
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: hello
spec:
  protocol: http
  endpoints:
    - address: "` + address + `"
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: mark
spec:
  type: headers
  config:
    response:
      set: [{name: x-dev, value: ` + value + `}]
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: hello
spec:
  match:
    path: {prefix: /}
  policies:
    - name: mark
  upstreams:
    - name: hello
`
}

// get sends GET url and returns the status, the x-dev header and the body.
func get(t *testing.T, client *http.Client, url string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header.Get("X-Dev"), string(body)
}

// TestRunServesAndHotReloads covers the main path with --ephemeral-ports:
// the readiness block (Revision, test digest, admin URL, token file,
// listener, mapping, dev tap line), traffic through the Filter Chain, a
// source edit swapped in and reported active with the same ports, then
// Ctrl-C draining ruralzd: exit 130 and the private directory removed.
func TestRunServesAndHotReloads(t *testing.T) {
	up := upstream(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "bundle.yaml", devBundle(up, "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports")
	r := d.readyBlock()
	if !regexp.MustCompile(`^rev-[0-9a-f]{12}$`).MatchString(r.revision) || !strings.HasPrefix(r.digest, "sha256:") ||
		r.revision[4:] != r.digest[7:19] || !strings.HasPrefix(r.testDigest, "sha256:") || r.testDigest == r.digest {
		t.Errorf("readiness block %+v", r)
	}
	listener := r.listeners["http"]
	if !strings.HasPrefix(listener, "http://127.0.0.1:") || strings.HasSuffix(listener, ":8080") || len(r.mapping) != 2 ||
		!strings.HasPrefix(r.mapping[0], "http: 8080 -> ") || !strings.HasPrefix(r.mapping[1], "admin: 9901 -> ") {
		t.Errorf("listener %q, mapping %q", listener, r.mapping)
	}
	if r.tap != "ruralz dev tap --admin "+r.admin+" --admin-token-file "+r.tokenFile {
		t.Errorf("dev tap line %q", r.tap)
	}
	if !strings.HasPrefix(r.tokenFile, d.tmp) {
		t.Errorf("token file %s outside the private directory under %s", r.tokenFile, d.tmp)
	}
	client := &http.Client{Timeout: 10 * time.Second}
	if code, mark, body := get(t, client, listener+"/"); code != http.StatusOK || mark != "one" || body != "hello" {
		t.Fatalf("GET %s = %d, x-dev %q, %q", listener, code, mark, body)
	}
	if code, _, _ := get(t, client, r.admin+"/readyz"); code != http.StatusOK {
		t.Errorf("GET %s/readyz = %d", r.admin, code)
	}

	writeFile(t, dir, "bundle.yaml", devBundle(up, "two"))
	before := d.inv.stdout.String()
	d.drive("the activation", func() bool { return strings.Contains(strings.TrimPrefix(d.inv.stdout.String(), before), "active rev-") })
	if added := strings.TrimPrefix(d.inv.stdout.String(), before); !regexp.MustCompile(`^active rev-[0-9a-f]{12}\n$`).MatchString(added) {
		t.Errorf("after the edit: %q, want one active line and no mapping", added)
	}
	if code, mark, _ := get(t, client, listener+"/"); code != http.StatusOK || mark != "two" {
		t.Errorf("after the reload: GET = %d, x-dev %q", code, mark)
	}

	client.CloseIdleConnections()
	d.signal(os.Interrupt)
	if took := d.driveNode("the Drain", d.exited); took < 20*time.Second {
		t.Errorf("one SIGTERM ended ruralzd after %v of its clock, before its Drain deadline", took)
	}
	if code := d.inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev run = exit %d, want 130\n%s", code, d.inv.stderr)
	}
	if entries, _ := os.ReadDir(d.tmp); len(entries) != 0 {
		t.Errorf("the private directory stays: %v", entries)
	}
}

// TestRunJSONEvents covers --output json: a ready event, then an active
// event after a reload, each one NDJSON object with every member.
func TestRunJSONEvents(t *testing.T) {
	up := upstream(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "bundle.yaml", devBundle(up, "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports", "--output", "json")
	d.drive("the ready event", func() bool { return strings.Contains(d.inv.stdout.String(), `"event":"ready"`) })
	writeFile(t, dir, "bundle.yaml", devBundle(up, "two"))
	d.drive("the active event", func() bool { return strings.Contains(d.inv.stdout.String(), `"event":"active"`) })
	got := lines(d.inv.stdout.String())
	member := regexp.MustCompile(`^\{"event":"(ready|active)","revision":"rev-[0-9a-f]{12}","digest":"sha256:[0-9a-f]{64}",` +
		`"testDigest":"sha256:[0-9a-f]{64}","admin":"http://127\.0\.0\.1:\d+","adminTokenFile":"[^"]+",` +
		`"listeners":\[\{"name":"http","url":"http://127\.0\.0\.1:\d+"\}\],` +
		`"ports":\[\{"name":"http","port":8080,"testPort":\d+\},\{"name":"admin","port":9901,"testPort":\d+\}\]\}$`)
	if len(got) != 2 || !member.MatchString(got[0]) || !member.MatchString(got[1]) {
		t.Errorf("events:\n%s", d.inv.stdout)
	}
	d.signal(syscall.SIGTERM)
	d.driveNode("the Drain", d.exited)
	if code := d.inv.wait(t); code != command.ExitTerminated {
		t.Errorf("dev run = exit %d, want 143", code)
	}
}

// TestRunRejectedReload: a render ruralzd cannot activate, here one whose
// env secret is unset, keeps the active Revision: once the ready timeout
// passes the command prints that it still serves it.
func TestRunRejectedReload(t *testing.T) {
	up := upstream(t)
	dir := t.TempDir()
	src := writeFile(t, dir, "bundle.yaml", devBundle(up, "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports", "--ready-timeout", "5s")
	r := d.readyBlock()
	broken := strings.Replace(devBundle(up, "one"), "      port: 8080\n",
		"      port: 8080\n  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_UNSET_IN_TEST}}\n", 1)
	writeFile(t, dir, "bundle.yaml", broken)
	before := d.inv.stdout.String()
	d.drive("the rejection", func() bool { return strings.Contains(d.inv.stdout.String(), "rejected, still serving ") })
	if added := strings.TrimPrefix(d.inv.stdout.String(), before); added != "rejected, still serving "+r.revision+"\n" {
		t.Errorf("after the broken edit: %q", added)
	}
	d.signal(os.Interrupt)
	d.signal(os.Interrupt)
	d.driveNode("the Drain", d.exited)
	if code := d.inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev run = exit %d, want 130", code)
	}
}

// TestRunSecretOverrides: a TLS listener whose certificate and key come
// from --secret-overrides serves with them.
func TestRunSecretOverrides(t *testing.T) {
	up := upstream(t)
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writeFile(t, dir, "local/tls.crt", string(leaf.CertPEM))
	writeFile(t, dir, "local/tls.key", string(leaf.KeyPEM))
	overrides := writeFile(t, dir, "overrides.yaml", "file:/etc/ruralz/tls/tls.crt: local/tls.crt\nfile:/etc/ruralz/tls/tls.key: local/tls.key\n")
	bundle := strings.Replace(devBundle(up, "tls"), "    - name: http\n      protocol: http\n      port: 8080\n",
		"    - name: https\n      protocol: https\n      port: 8443\n      tls:\n        certificates:\n          - name: main\n"+
			"            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}\n"+
			"            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}\n", 1)
	src := writeFile(t, dir, "bundle.yaml", bundle)
	d := startDevRun(t, nil, src, "--ephemeral-ports", "--secret-overrides", overrides)
	r := d.readyBlock()
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: ca.Pool(), MinVersion: tls.VersionTLS13}}}
	if code, mark, body := get(t, client, r.listeners["https"]+"/"); code != http.StatusOK || mark != "tls" || body != "hello" {
		t.Fatalf("GET %s = %d, %q, %q", r.listeners["https"], code, mark, body)
	}
	d.signal(os.Interrupt)
	d.signal(os.Interrupt)
	d.driveNode("the Drain", d.exited)
	d.inv.wait(t)
}

// TestRunReadyTimeout: an unresolvable reference keeps /readyz at 503;
// when the ready timeout passes the command prints the reasons, stops
// ruralzd and exits 2.
func TestRunReadyTimeout(t *testing.T) {
	up := upstream(t)
	broken := strings.Replace(devBundle(up, "one"), "      port: 8080\n",
		"      port: 8080\n  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_UNSET_IN_TEST}}\n", 1)
	src := writeFile(t, t.TempDir(), "bundle.yaml", broken)
	d := startDevRun(t, nil, src, "--ephemeral-ports", "--ready-timeout", "3s")
	d.drive("the timeout", func() bool {
		return strings.Contains(d.inv.stderr.String(), "ruralzd is not ready after 3s: ") || d.exited()
	})
	d.driveNode("the stop", d.exited)
	if code := d.inv.wait(t); code != command.ExitNoResult || !strings.Contains(d.inv.stderr.String(), "RZ-CFG-026") {
		t.Errorf("dev run = exit %d\n%s", code, d.inv.stderr)
	}
	if entries, _ := os.ReadDir(d.tmp); len(entries) != 0 {
		t.Errorf("the private directory stays: %v", entries)
	}
}

// TestRunChildExitsOnItsOwn: a ruralzd that exits on its own, here killed
// from outside, ends the command with exit 2 naming its status.
func TestRunChildExitsOnItsOwn(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports")
	r := d.readyBlock()
	h, err := nodedir.ReadHolderAt(filepath.Join(filepath.Dir(r.tokenFile), "data"))
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(h.PID, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if code := d.inv.wait(t); code != command.ExitNoResult || !strings.Contains(d.inv.stderr.String(), "ruralz dev run: ruralzd exited: signal: killed\n") {
		t.Errorf("dev run = exit %d\n%s", code, d.inv.stderr)
	}
}

// TestRunExitBound: a ruralzd that outlives the 30 s exit bound after
// SIGTERM, its clock held still mid-Drain, gets SIGKILL.
func TestRunExitBound(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports")
	d.readyBlock()
	d.signal(os.Interrupt)
	deadline := time.Now().Add(60 * time.Second)
	for !d.exited() && time.Now().Before(deadline) {
		d.clk.Advance(time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	if code := d.inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev run = exit %d, want 130", code)
	}
}

// TestRunVersionMismatch: a ruralzd of another release exits 2 naming
// both versions.
func TestRunVersionMismatch(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, func(o *Options) { o.Version = "9.9.9" }, src, "--ephemeral-ports")
	d.drive("the refusal", d.exited)
	want := "is version " + binaryVersion(t, builtRuralzd.Ruralzd) + ", but this ruralz is 9.9.9"
	if code := d.inv.wait(t); code != command.ExitNoResult || !strings.Contains(d.inv.stderr.String(), want) {
		t.Errorf("dev run = exit %d\n%s", code, d.inv.stderr)
	}
}

// TestRunBusyPort: without --ephemeral-ports a busy port exits 2 before
// the launch, naming the listener.
func TestRunBusyPort(t *testing.T) {
	busy, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	port := strconv.Itoa(busy.Addr().(*net.TCPAddr).Port)
	src := writeFile(t, t.TempDir(), "bundle.yaml", strings.Replace(devBundle(upstream(t), "one"), "port: 8080", "port: "+port, 1))
	d := startDevRun(t, nil, src)
	if code := d.inv.wait(t); code != command.ExitNoResult || !strings.Contains(d.inv.stderr.String(), "listener http: port "+port+" is busy") {
		t.Errorf("dev run = exit %d\n%s", code, d.inv.stderr)
	}
}

// TestRunMemoryStateStore: a Bundle naming a redis State Store runs on
// --memory-state-store without its URL or server.
func TestRunMemoryStateStore(t *testing.T) {
	bundle := strings.Replace(devBundle(upstream(t), "one"), "      port: 8080\n",
		"      port: 8080\n  stateStore:\n    driver: redis\n    url: {secretRef: {provider: env, name: RURALZ_SECRET_UNSET_IN_TEST}}\n", 1)
	src := writeFile(t, t.TempDir(), "bundle.yaml", bundle)
	d := startDevRun(t, nil, src, "--ephemeral-ports", "--memory-state-store")
	r := d.readyBlock()
	if r.testDigest == r.digest {
		t.Error("the memory State Store kept the Revision digest")
	}
	d.signal(os.Interrupt)
	d.signal(os.Interrupt)
	d.driveNode("the Drain", d.exited)
	d.inv.wait(t)
}

// TestRunSecondSignal: a second Ctrl-C sends a second SIGTERM, which jumps
// ruralzd's Drain to its deadline, so it exits long before the deadline
// of one SIGTERM.
func TestRunSecondSignal(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports")
	r := d.readyBlock()
	d.signal(os.Interrupt)
	client := &http.Client{Timeout: 10 * time.Second}
	waitFor(t, "the Drain to start", func() bool {
		code, _, _ := get(t, client, r.admin+"/readyz")
		return code != http.StatusOK
	})
	d.signal(os.Interrupt)
	if took := d.driveNode("the Drain", d.exited); took >= 20*time.Second {
		t.Errorf("two SIGTERMs ended ruralzd after %v of its clock, not at its Drain deadline", took)
	}
	if code := d.inv.wait(t); code != command.ExitInterrupted {
		t.Errorf("dev run = exit %d, want 130", code)
	}
}

// TestRunCompactLogs: on a terminal, without --output json, each JSON log
// line of ruralzd becomes its short form, without the Node and Revision
// IDs.
func TestRunCompactLogs(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, func(o *Options) { o.terminal = func(io.Writer) bool { return true } }, src, "--ephemeral-ports")
	d.readyBlock()
	d.signal(os.Interrupt)
	d.signal(os.Interrupt)
	d.driveNode("the Drain", d.exited)
	d.inv.wait(t)
	activated := regexp.MustCompile(`(?m)^\S+ INFO revision activated component=reload `)
	if logs := d.inv.stderr.String(); !activated.MatchString(logs) || strings.Contains(logs, `"msg":`) ||
		strings.Contains(logs, "node_id=") || strings.Contains(logs, " revision=rev-") {
		t.Errorf("stderr is not in the short form:\n%s", logs)
	}
}

// TestTapRealNode runs ruralz dev tap against the /tap of the ruralzd dev
// run launched, with the token file the readiness block names: an exchange
// prints with its credential header redacted, and with four subscribers
// at the Node's limit a fifth exits 2 with 503 RZ-RT-019.
func TestTapRealNode(t *testing.T) {
	src := writeFile(t, t.TempDir(), "bundle.yaml", devBundle(upstream(t), "one"))
	d := startDevRun(t, nil, src, "--ephemeral-ports")
	r := d.readyBlock()
	tapArgs := []string{"dev", "tap", "--admin", r.admin, "--admin-token-file", r.tokenFile}
	var taps []*invocation
	for range 4 {
		taps = append(taps, start(t, testOptions(clocktest.New(epoch())), nil, tapArgs...))
	}
	client := &http.Client{Timeout: 10 * time.Second}
	line := regexp.MustCompile(`(?m)^\S+ 200 GET 127\.0\.0\.1/orders route=hello upstream=hello [0-9.]+ms\n(  [<>] .*\n)*  > authorization: \[REDACTED\]\n`)
	waitFor(t, "every subscriber to print an exchange", func() bool {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, r.listeners["http"]+"/orders", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer canary-4fQ9x2LmZ7rT1vB8nK3sW6yH")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		for _, tap := range taps {
			if !line.MatchString(tap.stdout.String()) {
				return false
			}
		}
		return true
	})
	fifth := execute(t, testOptions(clocktest.New(epoch())), nil, tapArgs...)
	if fifth.code != command.ExitNoResult || fifth.stdout != "" || !strings.Contains(fifth.stderr, "ruralz dev tap: 503 RZ-RT-019 ") {
		t.Errorf("the fifth dev tap = exit %d, stdout %q, stderr %q; want 2 and 503 RZ-RT-019", fifth.code, fifth.stdout, fifth.stderr)
	}
	for _, tap := range taps {
		if strings.Contains(tap.stdout.String(), "canary-4fQ9") {
			t.Errorf("the token reached ruralz dev tap's output:\n%s", tap.stdout)
		}
		tap.interrupt(os.Interrupt)
		if code := tap.wait(t); code != command.ExitInterrupted {
			t.Errorf("dev tap = exit %d, want 130\n%s", code, tap.stderr)
		}
	}
	client.CloseIdleConnections()
	d.signal(os.Interrupt)
	d.signal(os.Interrupt)
	d.driveNode("the Drain", d.exited)
	d.inv.wait(t)
}
