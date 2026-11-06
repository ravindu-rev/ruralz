// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// realBound bounds each wait of these tests in real time: the Node runs on
// a fake process clock the tests move, while its sockets are real.
const realBound = 30 * time.Second

// fixture is the directory of a test Node: its one-file Bundle, data
// directory and admin token file, and the ports its Bundle names.
type fixture struct {
	bundle, data, token string
	adminPort, webPort  int
	upstream            string
}

// newFixture writes a Bundle routing / to upstream.
func newFixture(t *testing.T, upstream string) fixture {
	t.Helper()
	dir := t.TempDir()
	f := fixture{
		bundle: filepath.Join(dir, "ruralz.yaml"), data: filepath.Join(dir, "data"), token: filepath.Join(dir, "admin.token"),
		adminPort: proc.FreePort(t), webPort: proc.FreePort(t), upstream: upstream,
	}
	writeFile(t, f.token, testToken+"\n")
	f.write(t, "/")
	return f
}

// write replaces the Bundle, atomically, with one routing prefix to the
// upstream.
func (f fixture) write(t *testing.T, prefix string) {
	t.Helper()
	tmp := f.bundle + ".tmp"
	writeFile(t, tmp, "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n  admin:\n    port: "+strconv.Itoa(f.adminPort)+
		"\n  listeners:\n    - name: web\n      protocol: http\n      port: "+strconv.Itoa(f.webPort)+"\n"+
		"---\napiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: orders\nspec:\n  protocol: http\n  endpoints:\n    - address: \""+f.upstream+"\"\n"+
		"---\napiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: orders\nspec:\n  match:\n    path:\n      prefix: "+prefix+"\n  upstreams:\n    - name: orders\n")
	if err := os.Rename(tmp, f.bundle); err != nil {
		t.Fatal(err)
	}
}

// env returns the settings of a Node on f.
func (f fixture) env(extra map[string]string) map[string]string {
	env := map[string]string{
		setting.EnvConfig: f.bundle, setting.EnvDataDir: f.data, setting.EnvAdminTokenFile: f.token,
		setting.EnvBindLoopback: "true", setting.EnvFetchAllow: "127.0.0.0/8", setting.EnvLogLevel: "debug",
	}
	for k, v := range extra {
		env[k] = v
	}
	return env
}

// testNode is one in-process Node on a fake process clock.
type testNode struct {
	t      *testing.T
	clk    *clocktest.Fake
	logs   *syncBuffer
	stderr *syncBuffer
	cancel context.CancelFunc
	exited chan struct{}
	code   int
	once   sync.Once
}

// startNode runs a Node with env in the background; the test's cleanup
// stops it.
func startNode(t *testing.T, env map[string]string) *testNode {
	t.Helper()
	n := &testNode{t: t, clk: clocktest.New(epoch()), logs: &syncBuffer{}, stderr: &syncBuffer{}, exited: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.WithoutCancel(t.Context()))
	n.cancel = cancel
	p := testProcess(n.clk, env)
	go func() {
		defer close(n.exited)
		n.code = run(ctx, nil, n.logs, n.stderr, p)
	}()
	t.Cleanup(func() { n.stop() })
	return n
}

// eventually polls cond, moving the process clock 100 ms between polls,
// until it holds; the Node exiting first fails the test.
func (n *testNode) eventually(what string, cond func() bool) {
	n.t.Helper()
	deadline := time.Now().Add(realBound)
	for !cond() {
		if time.Now().After(deadline) {
			n.t.Fatalf("%s: not within %v\nlogs:\n%s\nstderr:\n%s", what, realBound, n.logs.String(), n.stderr.String())
		}
		select {
		case <-n.exited:
			n.t.Fatalf("%s: the Node exited %d\nlogs:\n%s\nstderr:\n%s", what, n.code, n.logs.String(), n.stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		n.clk.Advance(100 * time.Millisecond)
	}
}

// wait moves the process clock until Run returns, and returns its code.
func (n *testNode) wait() int {
	deadline := time.Now().Add(realBound)
	for {
		select {
		case <-n.exited:
			return n.code
		case <-time.After(5 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			n.t.Errorf("the Node did not exit within %v\nlogs:\n%s", realBound, n.logs.String())
			return -1
		}
		n.clk.Advance(250 * time.Millisecond)
	}
}

// stop ends Run's context, a stop signal, moves the clock through the
// Drain and returns the exit code.
func (n *testNode) stop() int {
	n.once.Do(func() {
		n.cancel()
		n.code = n.wait()
	})
	return n.code
}

// client sends one request per connection, so no idle connection holds a
// Drain.
func client() *http.Client {
	return &http.Client{Transport: &http.Transport{DisableKeepAlives: true, Proxy: nil}, Timeout: 10 * time.Second}
}

// get sends GET url, with the admin token when token is set, and returns
// the status, the Ruralz-Version header and the body; 0 when it fails.
func get(ctx context.Context, url string, token bool) (int, string, string) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err.Error()
	}
	if token {
		req.Header.Set("Authorization", "Bearer "+testToken)
	}
	resp, err := client().Do(req)
	if err != nil {
		return 0, "", err.Error()
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return resp.StatusCode, resp.Header.Get(admin.HeaderVersion), string(b)
}

// notes records the datagrams a service manager's notification socket
// receives.
type notes struct {
	mu   sync.Mutex
	msgs []string
}

// has reports whether some datagram holds the assignment want.
func (ns *notes) has(want string) bool {
	return ns.count(want) > 0
}

// count returns how many datagrams hold the assignment want.
func (ns *notes) count(want string) int {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	n := 0
	for _, m := range ns.msgs {
		for _, line := range strings.Split(m, "\n") {
			if line == want {
				n++
				break
			}
		}
	}
	return n
}

// all returns the datagrams in the order they arrived, which is the order
// they were sent.
func (ns *notes) all() []string {
	ns.mu.Lock()
	defer ns.mu.Unlock()
	return append([]string(nil), ns.msgs...)
}

// records returns the attributes of every process log record whose message
// is msg, in the order they were logged.
func records(logs, msg string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(logs, "\n") {
		var r map[string]any
		if json.Unmarshal([]byte(line), &r) == nil && r["msg"] == msg {
			out = append(out, r)
		}
	}
	return out
}

// notifySocket serves a notification socket and returns its path.
func notifySocket(t *testing.T) (string, *notes) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notify.sock")
	conn, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	ns := &notes{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			k, err := conn.Read(buf)
			if err != nil {
				return
			}
			ns.mu.Lock()
			ns.msgs = append(ns.msgs, string(buf[:k]))
			ns.mu.Unlock()
		}
	}()
	t.Cleanup(func() {
		_ = conn.Close()
		<-done
	})
	return path, ns
}

// upstreamServer answers every request with "orders:" and its path.
func upstreamServer(t *testing.T) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "orders:"+r.URL.Path) //nolint:gosec // G705: a plain-text test answer.
	}))
	t.Cleanup(up.Close)
	return up
}

// TestRunRouteDeadlineOnTheProcessClock checks that the Route deadline runs
// on the process clock, as the Upstream layer that enforces it does: a
// process clock an hour ahead of the wall clock, as the controllable-clock
// build's is once a test has moved it, still serves the Route instead of
// refusing every request with RZ-RT-007.
func TestRunRouteDeadlineOnTheProcessClock(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	n := startNode(t, f.env(nil))
	ctx := t.Context()
	adminURL := "http://127.0.0.1:" + strconv.Itoa(f.adminPort)
	n.eventually("/readyz ready", func() bool { code, _, _ := get(ctx, adminURL+admin.PathReadyz, false); return code == http.StatusOK })
	n.clk.Set(time.Now().Add(time.Hour))
	if code, _, body := get(ctx, "http://127.0.0.1:"+strconv.Itoa(f.webPort)+"/a", false); code != http.StatusOK || body != "orders:/a" {
		t.Errorf("GET /a with the process clock an hour ahead = %d %q, want 200 %q", code, body, "orders:/a")
	}
}

func TestRunServesReloadsAndDrains(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	sock, ns := notifySocket(t)
	n := startNode(t, f.env(map[string]string{
		"NOTIFY_SOCKET": sock, "WATCHDOG_USEC": "4000000", "WATCHDOG_PID": strconv.Itoa(testPID),
	}))
	ctx := t.Context()
	adminURL := "http://127.0.0.1:" + strconv.Itoa(f.adminPort)
	web := "http://127.0.0.1:" + strconv.Itoa(f.webPort)

	n.eventually("/readyz ready", func() bool { code, _, _ := get(ctx, adminURL+admin.PathReadyz, false); return code == http.StatusOK })
	n.eventually("READY=1", func() bool { return ns.has("READY=1") })
	if !strings.Contains(n.logs.String(), `"msg":"admin port bound"`) || !strings.Contains(n.logs.String(), `"from":"source"`) {
		t.Errorf("the admin port was not bound from the source:\n%s", n.logs.String())
	}

	// The admin server: /healthz is open and carries no version; an
	// authenticated response carries Ruralz-Version; a missing token is
	// refused.
	if code, version, _ := get(ctx, adminURL+admin.PathHealthz, false); code != http.StatusOK || version != "" {
		t.Errorf("GET /healthz = %d, version %q; want 200 and none", code, version)
	}
	if code, version, body := get(ctx, adminURL+admin.PathConfigDump, true); code != http.StatusOK || version != buildinfo.Get().Version {
		t.Errorf("GET /config/dump = %d, version %q, want 200 and %q\n%s", code, version, buildinfo.Get().Version, body)
	}
	for _, path := range []string{admin.PathSnapshots, admin.PathUpstreams, admin.PathMetrics} {
		if code, version, body := get(ctx, adminURL+path, true); code != http.StatusOK || version == "" {
			t.Errorf("GET %s = %d, version %q\n%s", path, code, version, body)
		}
	}
	if code, _, _ := get(ctx, adminURL+admin.PathConfigDump, false); code != http.StatusUnauthorized {
		t.Errorf("GET /config/dump without a token = %d, want 401", code)
	}

	// The data plane serves the Revision.
	if code, _, body := get(ctx, web+"/a", false); code != http.StatusOK || body != "orders:/a" {
		t.Errorf("GET /a = %d %q, want 200 %q", code, body, "orders:/a")
	}

	// The watchdog sends WATCHDOG=1 while every probed part answers.
	n.eventually("WATCHDOG=1", func() bool { return ns.has("WATCHDOG=1") })

	// A changed source Hot Reloads: the Route now matches /v2 only.
	f.write(t, "/v2")
	n.eventually("the reloaded Revision", func() bool {
		code, _, _ := get(ctx, web+"/a", false)
		return code == http.StatusNotFound
	})
	if code, _, body := get(ctx, web+"/v2/b", false); code != http.StatusOK || body != "orders:/v2/b" {
		t.Errorf("GET /v2/b = %d %q after the reload", code, body)
	}

	if code := n.stop(); code != ExitOK {
		t.Errorf("Run() = %d after a stop signal, want %d\n%s", code, ExitOK, n.logs.String())
	}
	if !ns.has("STOPPING=1") {
		t.Errorf("no STOPPING=1 among %q", ns.msgs)
	}
	// The data directory, identity/ and lkg/ are created with mode 0700.
	for _, d := range []string{f.data, filepath.Join(f.data, "identity"), filepath.Join(f.data, "lkg")} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0o700 {
			t.Errorf("os.Stat(%q) = %v, %v; want a directory of mode 0700", d, fi, err)
		}
	}
}

func TestRunBootsLastKnownGood(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	ctx := t.Context()
	adminURL := "http://127.0.0.1:" + strconv.Itoa(f.adminPort)
	first := startNode(t, f.env(nil))
	first.eventually("/readyz ready", func() bool { code, _, _ := get(ctx, adminURL+admin.PathReadyz, false); return code == http.StatusOK })
	// Readiness reports the Revision before its Last-Known-Good write, and a
	// Drain started first skips that write: wait for /debug/snapshots to
	// name it.
	first.eventually("Last-Known-Good written", func() bool {
		code, _, body := get(ctx, adminURL+admin.PathSnapshots, true)
		var v struct {
			LastKnownGood *string `json:"lastKnownGood"`
		}
		return code == http.StatusOK && json.Unmarshal([]byte(body), &v) == nil && v.LastKnownGood != nil
	})
	if code := first.stop(); code != ExitOK {
		t.Fatalf("Run() = %d, want %d", code, ExitOK)
	}

	// The source no longer parses: the restarted Node binds Last-Known-Good's
	// admin port and serves its Revision.
	writeFile(t, f.bundle, "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: [unclosed\n")
	second := startNode(t, f.env(nil))
	second.eventually("/readyz ready on Last-Known-Good", func() bool {
		code, _, _ := get(ctx, adminURL+admin.PathReadyz, false)
		return code == http.StatusOK
	})
	if !strings.Contains(second.logs.String(), `"from":"last_known_good"`) {
		t.Errorf("the admin port was not bound from Last-Known-Good:\n%s", second.logs.String())
	}
	if code, _, body := get(ctx, "http://127.0.0.1:"+strconv.Itoa(f.webPort)+"/c", false); code != http.StatusOK || body != "orders:/c" {
		t.Errorf("GET /c = %d %q on Last-Known-Good", code, body)
	}
	if code := second.stop(); code != ExitOK {
		t.Errorf("Run() = %d, want %d", code, ExitOK)
	}
}

func TestRunAdminPortBusyExitsRuntime(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	// A socket without SO_REUSEPORT holds the admin port.
	var lc net.ListenConfig
	busy, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(f.adminPort))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = busy.Close() }()
	n := startNode(t, f.env(nil))
	if code := n.wait(); code != ExitRuntime {
		t.Errorf("Run() = %d with the admin port taken, want %d\n%s", code, ExitRuntime, n.logs.String())
	}
	if !strings.Contains(n.logs.String(), `"msg":"admin port cannot be bound"`) {
		t.Errorf("no error record for the admin port:\n%s", n.logs.String())
	}
}

// handOver moves both process clocks until the holder exits, having handed
// over to succ, a second process on its locked data directory: once succ
// booted the holder's candidate it takes the lock and the holder drains
// with exit 0. The successor exiting first, or no handover within
// realBound, fails the test.
func handOver(t *testing.T, holder, succ *testNode) {
	t.Helper()
	deadline := time.Now().Add(realBound)
	for done := false; !done; {
		select {
		case <-holder.exited:
			done = true
		case <-succ.exited:
			t.Fatalf("the successor exited %d\nsuccessor:\n%s\nholder:\n%s", succ.code, succ.logs.String(), holder.logs.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no handover within %v\nsuccessor:\n%s\nholder:\n%s", realBound, succ.logs.String(), holder.logs.String())
		}
		holder.clk.Advance(100 * time.Millisecond)
		succ.clk.Advance(100 * time.Millisecond)
	}
	if holder.code != ExitOK {
		t.Errorf("the holder's Run() = %d after the handover, want %d\n%s", holder.code, ExitOK, holder.logs.String())
	}
}

func TestRunHandsOverToSuccessor(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	ctx := t.Context()
	adminURL := "http://127.0.0.1:" + strconv.Itoa(f.adminPort)
	ready := func() bool { code, _, _ := get(ctx, adminURL+admin.PathReadyz, false); return code == http.StatusOK }
	holder := startNode(t, f.env(nil))
	holder.eventually("/readyz ready", ready)

	// A second process on the locked data directory is a handover
	// successor: once it booted the holder's candidate it takes the lock
	// and the holder drains with exit 0.
	succ := startNode(t, f.env(nil))
	handOver(t, holder, succ)
	succ.eventually("the successor ready", ready)
	if code, _, body := get(ctx, "http://127.0.0.1:"+strconv.Itoa(f.webPort)+"/d", false); code != http.StatusOK || body != "orders:/d" {
		t.Errorf("GET /d = %d %q on the successor", code, body)
	}
	if code := succ.stop(); code != ExitOK {
		t.Errorf("the successor's Run() = %d, want %d", code, ExitOK)
	}
}

// watchdogEnv returns the service manager's variables of a Node: its
// notification socket and, with usec set, a watchdog interval for the test
// Nodes' PID, as systemd gives the ExecStart= process.
func watchdogEnv(socket, usec string) map[string]string {
	env := map[string]string{"NOTIFY_SOCKET": socket}
	if usec != "" {
		env["WATCHDOG_USEC"], env["WATCHDOG_PID"] = usec, strconv.Itoa(testPID)
	}
	return env
}

// startHolderNode starts a lock holder on f with the service manager's
// variables env and waits until it is ready.
func startHolderNode(t *testing.T, f fixture, env map[string]string) *testNode {
	t.Helper()
	adminURL := "http://127.0.0.1:" + strconv.Itoa(f.adminPort)
	holder := startNode(t, f.env(env))
	holder.eventually("/readyz ready", func() bool {
		code, _, _ := get(t.Context(), adminURL+admin.PathReadyz, false)
		return code == http.StatusOK
	})
	return holder
}

// checkSuccessorWatchdog checks a successor that took the lock and sent a
// WATCHDOG=1: its first notification is MAINPID=, READY=1 and STATUS=, so
// no WATCHDOG=1 precedes the lock; it started one watchdog, with interval
// from from, and only after it took the lock. The process log's worker
// writes records in the order they were logged, after the notifications
// went out.
func checkSuccessorWatchdog(t *testing.T, succ *testNode, ns *notes, interval, from string) {
	t.Helper()
	if msgs := ns.all(); len(msgs) == 0 || !strings.HasPrefix(msgs[0], "MAINPID="+strconv.Itoa(testPID)+"\nREADY=1\nSTATUS=") {
		t.Errorf("the successor's notifications %q, want MAINPID=, READY=1 and STATUS= first", msgs)
	}
	succ.eventually("the successor's watchdog record", func() bool {
		return len(records(succ.logs.String(), "watchdog enabled")) > 0
	})
	logs := succ.logs.String()
	recs := records(logs, "watchdog enabled")
	if len(recs) != 1 || recs[0]["interval"] != interval || recs[0]["from"] != from {
		t.Errorf("the successor's watchdog records %v, want one with interval %s from %s\n%s", recs, interval, from, logs)
	}
	took := strings.Index(logs, `"msg":"handover took the lock"`)
	if started := strings.Index(logs, `"msg":"watchdog enabled"`); took < 0 || started < took {
		t.Errorf("the successor's watchdog started at log offset %d, the lock taken at %d: want it after the lock\n%s", started, took, logs)
	}
}

// TestRunSuccessorRunsTheHolderWatchdog checks the watchdog of a handover
// successor that the service manager gives no WATCHDOG_USEC, as systemd
// gives none to a child of the ExecReload= helper: the holder's accepted
// carries the holder's interval, and the successor, once it took the lock
// and sent MAINPID=, READY=1 and STATUS=, sends WATCHDOG=1 with it, the
// first as soon as its parts ran and then every half interval; it sends
// nothing before.
func TestRunSuccessorRunsTheHolderWatchdog(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	holderSock, holderNotes := notifySocket(t)
	holder := startHolderNode(t, f, watchdogEnv(holderSock, "4000000"))
	holder.eventually("the holder's WATCHDOG=1", func() bool { return holderNotes.has("WATCHDOG=1") })

	succSock, succNotes := notifySocket(t)
	succ := startNode(t, f.env(watchdogEnv(succSock, "")))
	handOver(t, holder, succ)
	succ.eventually("the successor's first WATCHDOG=1", func() bool { return succNotes.has("WATCHDOG=1") })
	checkSuccessorWatchdog(t, succ, succNotes, "4s", watchdogFromHolder)
	succ.eventually("two more WATCHDOG=1, every 2 s", func() bool { return succNotes.count("WATCHDOG=1") >= 3 })
	if code := succ.stop(); code != ExitOK {
		t.Errorf("the successor's Run() = %d, want %d", code, ExitOK)
	}
}

// TestRunSuccessorKeepsItsOwnWatchdog checks a handover successor whose
// environment enables a watchdog: it runs it with its own interval, not
// the holder's, and also only once it took the lock and sent MAINPID=.
func TestRunSuccessorKeepsItsOwnWatchdog(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	holderSock, _ := notifySocket(t)
	holder := startHolderNode(t, f, watchdogEnv(holderSock, "4000000"))

	succSock, succNotes := notifySocket(t)
	succ := startNode(t, f.env(watchdogEnv(succSock, "6000000")))
	handOver(t, holder, succ)
	succ.eventually("the successor's first WATCHDOG=1", func() bool { return succNotes.has("WATCHDOG=1") })
	checkSuccessorWatchdog(t, succ, succNotes, "6s", watchdogFromEnvironment)
	if code := succ.stop(); code != ExitOK {
		t.Errorf("the successor's Run() = %d, want %d", code, ExitOK)
	}
}

// TestRunSuccessorWithoutAHolderWatchdog checks a holder that runs no
// watchdog: its accepted carries no interval, as an older holder's does
// not, and a successor whose environment enables none runs no watchdog.
func TestRunSuccessorWithoutAHolderWatchdog(t *testing.T) {
	up := upstreamServer(t)
	f := newFixture(t, up.Listener.Addr().String())
	holderSock, _ := notifySocket(t)
	holder := startHolderNode(t, f, watchdogEnv(holderSock, ""))

	succSock, succNotes := notifySocket(t)
	succ := startNode(t, f.env(watchdogEnv(succSock, "")))
	handOver(t, holder, succ)
	succ.eventually("the successor's MAINPID=", func() bool { return succNotes.has("MAINPID=" + strconv.Itoa(testPID)) })
	// The successor decides its watchdog before it serves the handover
	// socket in its turn, and the process log's worker writes records in
	// order.
	succ.eventually("the successor's handover socket", func() bool {
		return len(records(succ.logs.String(), "handover socket serving")) == 1
	})
	if recs := records(succ.logs.String(), "watchdog enabled"); len(recs) != 0 {
		t.Errorf("the successor started a watchdog %v without an interval from its environment or holder", recs)
	}
	if n := succNotes.count("WATCHDOG=1"); n != 0 {
		t.Errorf("the successor's notifications %q, want no WATCHDOG=1", succNotes.all())
	}
	if code := succ.stop(); code != ExitOK {
		t.Errorf("the successor's Run() = %d, want %d", code, ExitOK)
	}
}
