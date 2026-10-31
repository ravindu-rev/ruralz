// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
)

// Node settings of every benchmark run (PBB "Scenarios",
// "Reproducibility", "GOGC and GOMEMLIMIT").
const (
	// BenchZone is RURALZ_ZONE: each profile's overlay maps Valkey's host
	// to lab-a and S5x's fault proxy's to lab-b.
	BenchZone = "lab-a"
	// AdminPort is the scenario Bundles' admin port.
	AdminPort = 9901
	// nodeReadyTimeout bounds the wait for /readyz.
	nodeReadyTimeout = 2 * time.Minute
	// nodeStopTimeout bounds the drain after SIGTERM.
	nodeStopTimeout = 2 * time.Minute
	// drainGrace is how long the first SIGTERM's Drain runs before the
	// second jumps it to its deadline.
	drainGrace = time.Second
)

// NodeSpec starts one ruralzd on the gateway host.
type NodeSpec struct {
	// Binary is the release build of ruralzd.
	Binary string
	// Rendered is the rendered Bundle the Node loads.
	Rendered string
	// StateStoreURL is RURALZ_STATE_STORE_URL; empty without a State
	// Store.
	StateStoreURL string
	// Env is added last: a GOMEMLIMIT run sets GOMEMLIMIT.
	Env []string
	// KeepData keeps RURALZ_DATA_DIR from the Node before, so R1's cold
	// start from Last-Known-Good boots what the previous Node activated.
	KeepData bool
	// ScopeProperties are further properties of the Node's scope, such as
	// the GOMEMLIMIT run's MemoryMax; the loopback profile has no scopes.
	ScopeProperties []string
	// NoRevision starts a Node whose source holds no Revision yet, such as
	// an empty directory R1 activates its first Revision into: StartNode
	// then waits for /healthz instead of /readyz.
	NoRevision bool
}

// Node is a running ruralzd.
type Node struct {
	p        *Profile
	cmd      *exec.Cmd
	pid      int
	done     chan struct{}
	err      error
	errb     *tail
	token    string
	metrics  string
	adminURL string
	client   *http.Client
	cgroup   string
	env      []string
	data     string
	// started is just before the process started, ready when /readyz
	// first answered 200: R1's cold start.
	started, ready time.Time
	// peak is the scope's memory.peak, open since the last ResetPeak.
	peak *os.File
}

// randomToken returns a fresh admin token.
func randomToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// StartNode starts ruralzd in a scope on the gateway cpuset with the
// benchmark settings and waits until /readyz answers 200. The loopback
// profile binds loopback only and allows fetches from loopback addresses.
func StartNode(ctx context.Context, p *Profile, s NodeSpec) (*Node, error) {
	g := p.Hosts[RoleGateway]
	dir := filepath.Join(g.Dir, "node")
	data := filepath.Join(dir, "data")
	if !s.KeepData {
		if err := os.RemoveAll(data); err != nil {
			return nil, err
		}
	}
	for _, d := range []string{data, p.SecretRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	n := &Node{p: p, data: data, done: make(chan struct{}), errb: &tail{max: 64 << 10}, client: &http.Client{Timeout: 2 * time.Minute}}
	var err error
	if n.token, err = randomToken(); err != nil {
		return nil, err
	}
	if n.metrics, err = randomToken(); err != nil {
		return nil, err
	}
	tokenFile, metricsFile := filepath.Join(dir, "operator.token"), filepath.Join(dir, "metrics.token")
	for _, f := range []struct{ path, v string }{{tokenFile, n.token}, {metricsFile, n.metrics}} {
		if err := os.WriteFile(f.path, []byte(f.v+"\n"), 0o600); err != nil {
			return nil, err
		}
	}
	env := []string{
		"RURALZ_CONFIG=" + s.Rendered,
		"RURALZ_DATA_DIR=" + data,
		"RURALZ_SECRET_ROOT=" + p.SecretRoot,
		"RURALZ_ADMIN_TOKEN_FILE=" + tokenFile,
		"RURALZ_ADMIN_METRICS_TOKEN_FILE=" + metricsFile,
		"RURALZ_ZONE=" + BenchZone,
		"SSL_CERT_FILE=" + filepath.Join(p.TLSDir, FileCA),
		"GOGC=100",
		"PATH=" + os.Getenv("PATH"),
	}
	if p.GOMAXPROCS > 0 {
		env = append(env, "GOMAXPROCS="+strconv.Itoa(p.GOMAXPROCS))
	}
	if p.Name == ProfileLoopback {
		env = append(env, "RURALZ_BIND_LOOPBACK=true", "RURALZ_FETCH_ALLOW=127.0.0.0/8,::1")
	}
	if s.StateStoreURL != "" {
		env = append(env, "RURALZ_STATE_STORE_URL="+s.StateStoreURL)
	}
	env = append(env, s.Env...)
	n.env = env
	argv := p.Runner(RoleGateway).Argv([]string{s.Binary}, g.CPUs, s.ScopeProperties...)
	n.cmd = exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the harness starts the binary it built
	n.cmd.Env = n.env
	n.cmd.Stdout, n.cmd.Stderr = io.Discard, n.errb
	n.cmd.Cancel = func() error { return n.cmd.Process.Signal(syscall.SIGTERM) }
	n.cmd.WaitDelay = nodeStopTimeout
	n.started = time.Now()
	if err := n.cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness: start ruralzd: %w", err)
	}
	go func() { // owned by the Node: Stop waits for done
		n.err = n.cmd.Wait()
		close(n.done)
	}()
	n.pid = n.cmd.Process.Pid
	n.adminURL = "http://127.0.0.1:" + strconv.Itoa(p.adminPort())
	wait := n.waitReady
	if s.NoRevision {
		wait = n.waitHealthy
	}
	if err := wait(ctx); err != nil {
		_ = n.Stop(context.WithoutCancel(ctx))
		return nil, err
	}
	if p.Scope {
		// systemd-run --scope executes ruralzd in place, so the started
		// process is ruralzd and its cgroup is the scope.
		n.cgroup = cgroupOf("/proc", n.pid)
	}
	return n, nil
}

// readyPoll is the /readyz poll interval: fine enough to time R1's cold
// starts, whose smallest budget is 200 ms (target).
const readyPoll = 5 * time.Millisecond

// waitReady polls /readyz until it answers 200 and records when.
func (n *Node) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(nodeReadyTimeout)
	var last string
	for time.Now().Before(deadline) {
		select {
		case <-n.done:
			return n.exitError(" before ready")
		default:
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.adminURL+"/readyz", nil)
		if err != nil {
			return err
		}
		resp, err := n.client.Do(req)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				n.ready = time.Now()
				return nil
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
	return fmt.Errorf("harness: ruralzd not ready within %v; last /readyz: %s; stderr: %s", nodeReadyTimeout, last, n.errb.String())
}

// waitHealthy polls /healthz until it answers 200.
func (n *Node) waitHealthy(ctx context.Context) error {
	deadline := time.Now().Add(nodeReadyTimeout)
	for time.Now().Before(deadline) {
		select {
		case <-n.done:
			return n.exitError(" before it answered /healthz")
		default:
		}
		if code, _, err := n.open(ctx, "/healthz"); err == nil && code == http.StatusOK {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(readyPoll):
		}
	}
	return fmt.Errorf("harness: ruralzd did not answer /healthz within %v: %s", nodeReadyTimeout, n.errb.String())
}

// open issues an unauthenticated admin GET (/healthz, /readyz) and returns
// its status and body.
func (n *Node) open(ctx context.Context, path string) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.adminURL+path, nil)
	if err != nil {
		return 0, nil, err
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, b, err
}

// WaitRevision polls /readyz until it answers 200 naming rev
// (rev-<12 hex>), within limit, and returns when it first did: R1's
// activation of a replaced source.
func (n *Node) WaitRevision(ctx context.Context, rev string, limit time.Duration) (time.Time, error) {
	deadline := time.Now().Add(limit)
	var last string
	for time.Now().Before(deadline) {
		if n.Exited() {
			return time.Time{}, n.exitError("")
		}
		code, b, err := n.open(ctx, "/readyz")
		if err == nil && code == http.StatusOK {
			var doc adminapi.Readyz
			if json.Unmarshal(b, &doc) == nil && doc.Revision == rev {
				return time.Now(), nil
			}
		}
		last = fmt.Sprintf("%d %s %v", code, b, err)
		select {
		case <-ctx.Done():
			return time.Time{}, ctx.Err()
		case <-time.After(readyPoll):
		}
	}
	return time.Time{}, fmt.Errorf("harness: ruralzd did not serve %s within %v; last /readyz: %s", rev, limit, last)
}

// HeapAlloc returns the Node's live heap after a forced collection: the
// HeapAlloc total of /debug/pprof/heap?gc=1&debug=1 (PBB "Startup and
// reload time by configuration size", snapshot live heap).
func (n *Node) HeapAlloc(ctx context.Context) (int64, error) {
	b, err := n.admin(ctx, "/debug/pprof/heap?gc=1&debug=1", n.token)
	if err != nil {
		return 0, err
	}
	return ParseHeapAlloc(b)
}

// ParseHeapAlloc reads the "# HeapAlloc = N" line of a debug=1 heap
// profile.
func ParseHeapAlloc(b []byte) (int64, error) {
	for line := range strings.SplitSeq(string(b), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "# HeapAlloc = "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return 0, fmt.Errorf("harness: heap profile HeapAlloc %q: %w", v, err)
			}
			return n, nil
		}
	}
	return 0, errors.New("harness: the heap profile has no HeapAlloc total")
}

// ResetPeak starts a new peak memory measurement: it resets the scope's
// cgroup v2 memory.peak, which tracks the peak from the reset for reads
// through the file descriptor that wrote it, so the Node keeps that
// descriptor until the next reset; without a scope it resets the process's
// VmHWM through /proc/<pid>/clear_refs (PBB "Startup and reload time by
// configuration size", peak configuration memory).
func (n *Node) ResetPeak() error {
	if n.cgroup == "" {
		return os.WriteFile(filepath.Join("/proc", strconv.Itoa(n.pid), "clear_refs"), []byte("5\n"), 0o600)
	}
	if n.peak != nil {
		_ = n.peak.Close()
		n.peak = nil
	}
	f, err := os.OpenFile(filepath.Join("/sys/fs/cgroup", n.cgroup, "memory.peak"), os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString("reset\n"); err != nil {
		_ = f.Close()
		return err
	}
	n.peak = f
	return nil
}

// Peak returns the peak memory since ResetPeak: the scope's memory.peak,
// or the process's VmHWM.
func (n *Node) Peak() (int64, error) {
	if n.cgroup != "" {
		if n.peak == nil {
			return 0, errors.New("harness: Peak before ResetPeak")
		}
		b := make([]byte, 64)
		k, err := n.peak.ReadAt(b, 0)
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		return strconv.ParseInt(strings.TrimSpace(string(b[:k])), 10, 64)
	}
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(n.pid), "status"))
	if err != nil {
		return 0, err
	}
	return ParseVMHWM(b)
}

// ParseVMHWM reads VmHWM, in bytes, from /proc/<pid>/status.
func ParseVMHWM(b []byte) (int64, error) {
	for line := range strings.SplitSeq(string(b), "\n") {
		v, ok := strings.CutPrefix(line, "VmHWM:")
		if !ok {
			continue
		}
		f := strings.Fields(v)
		if len(f) != 2 || f[1] != "kB" {
			return 0, fmt.Errorf("harness: VmHWM %q", v)
		}
		kb, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			return 0, err
		}
		return kb << 10, nil
	}
	return 0, errors.New("harness: no VmHWM in the process status")
}

// PID returns the Node's process ID.
func (n *Node) PID() int { return n.pid }

// Exited reports whether the Node has exited.
func (n *Node) Exited() bool {
	select {
	case <-n.done:
		return true
	default:
		return false
	}
}

// exitError reports that the Node exited, when (such as " before ready")
// naming the wait it ended, with its wait error wrapped and its standard
// error; a Node that exited 0 has no wait error to wrap.
func (n *Node) exitError(when string) error {
	if n.err == nil {
		return fmt.Errorf("harness: ruralzd exited%s (exit status 0): %s", when, n.errb.String())
	}
	return fmt.Errorf("harness: ruralzd exited%s (%w): %s", when, n.err, n.errb.String())
}

// admin issues an authenticated admin GET.
func (n *Node) admin(ctx context.Context, path, token string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.adminURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := n.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("harness: admin %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("harness: admin %s: %d %s", path, resp.StatusCode, bytes.TrimSpace(b[:min(len(b), 512)]))
	}
	return b, nil
}

// Admin issues an admin GET of path with the operator token, such as R1's
// heap totals after a forced collection (/debug/pprof/heap?gc=1).
func (n *Node) Admin(ctx context.Context, path string) ([]byte, error) {
	return n.admin(ctx, path, n.token)
}

// Signal sends sig to the Node: SIGHUP loads its source at once, a Hot
// Reload of a replaced file.
func (n *Node) Signal(sig os.Signal) error {
	if n.Exited() {
		return n.exitError("")
	}
	return n.cmd.Process.Signal(sig)
}

// Started returns when the Node process started; Ready when /readyz first
// answered 200.
func (n *Node) Started() time.Time { return n.started }

// Ready returns when /readyz first answered 200.
func (n *Node) Ready() time.Time { return n.ready }

// DataDir returns the Node's RURALZ_DATA_DIR.
func (n *Node) DataDir() string { return n.data }

// Scrape reads the Node's metrics.
func (n *Node) Scrape(ctx context.Context) (promtext.Set, error) {
	b, err := n.admin(ctx, "/metrics", n.metrics)
	if err != nil {
		return promtext.Set{}, err
	}
	return promtext.ParseBytes(b)
}

// CPUProfile takes a CPU profile of seconds from /debug/pprof/profile.
func (n *Node) CPUProfile(ctx context.Context, seconds int) ([]byte, error) {
	return n.admin(ctx, "/debug/pprof/profile?seconds="+strconv.Itoa(seconds), n.token)
}

// Revision returns the Node's active Revision (rev-<12 hex>) from /readyz.
func (n *Node) Revision(ctx context.Context) (string, error) {
	b, err := n.admin(ctx, "/readyz", n.token)
	if err != nil {
		return "", err
	}
	var doc adminapi.Readyz
	if err := json.Unmarshal(b, &doc); err != nil {
		return "", fmt.Errorf("harness: /readyz: %w", err)
	}
	return doc.Revision, nil
}

// Usage samples the Node's CPU time and RSS.
func (n *Node) Usage() (proc.Usage, error) { return proc.SampleProc("/proc", n.pid) }

// RSS returns the Node's resident memory: anonymous plus mapped file
// memory of its cgroup v2 scope (memory.stat anon + file_mapped), which
// counts what the process itself keeps; VmRSS without a scope.
func (n *Node) RSS() (int64, error) {
	if n.cgroup != "" {
		if v, err := CgroupRSS(filepath.Join("/sys/fs/cgroup", n.cgroup, "memory.stat")); err == nil {
			return v, nil
		}
	}
	u, err := n.Usage()
	if err != nil {
		return 0, err
	}
	return u.RSSBytes, nil
}

// Stop sends SIGTERM and waits for the drain, then SIGKILL.
func (n *Node) Stop(ctx context.Context) error {
	if n.peak != nil {
		_ = n.peak.Close()
		n.peak = nil
	}
	if n.Exited() {
		return nil
	}
	// A run's measurement is over when its Node stops, so the Drain's
	// 10 s jitter window only delays the next run: a second SIGTERM after
	// drainGrace jumps the Drain to its deadline (docs/architecture/03-data-plane.md
	// "Sources, Last-Known-Good and Drain").
	_ = n.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-n.done:
	case <-time.After(drainGrace):
		_ = n.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-n.done:
		case <-time.After(nodeStopTimeout):
			_ = n.cmd.Process.Kill()
			<-n.done
		case <-ctx.Done():
			_ = n.cmd.Process.Kill()
			<-n.done
		}
	case <-ctx.Done():
		_ = n.cmd.Process.Kill()
		<-n.done
	}
	var exit *exec.ExitError
	if n.err != nil && (!errors.As(n.err, &exit) || exit.ExitCode() != 0) && !errors.Is(n.err, exec.ErrWaitDelay) {
		if errors.As(n.err, &exit) && !exit.Exited() {
			return nil // ended by the signal
		}
		return fmt.Errorf("harness: ruralzd: %w: %s", n.err, n.errb.String())
	}
	return nil
}

// Stderr returns the end of the Node's standard error.
func (n *Node) Stderr() string { return n.errb.String() }

// cgroupOf returns the cgroup v2 path of pid ("" when unknown).
func cgroupOf(procRoot string, pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "cgroup")) //nolint:gosec // G304: procfs
	if err != nil {
		return ""
	}
	for line := range strings.SplitSeq(string(b), "\n") {
		if rest, ok := strings.CutPrefix(line, "0::"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// CgroupRSS reads anon plus file_mapped from a cgroup v2 memory.stat file.
func CgroupRSS(path string) (int64, error) {
	f, err := os.Open(path) //nolint:gosec // G304: a cgroup file of the Node's scope
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	var anon, mapped int64
	found := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), " ")
		if !ok {
			continue
		}
		x, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			continue
		}
		switch k {
		case "anon":
			anon, found = x, found+1
		case "file_mapped":
			mapped, found = x, found+1
		}
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	if found < 2 {
		return 0, fmt.Errorf("harness: %s has no anon and file_mapped", path)
	}
	return anon + mapped, nil
}
