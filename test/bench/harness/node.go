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
	if err := os.RemoveAll(data); err != nil {
		return nil, err
	}
	for _, d := range []string{data, p.SecretRoot} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	n := &Node{p: p, done: make(chan struct{}), errb: &tail{max: 64 << 10}, client: &http.Client{Timeout: 2 * time.Minute}}
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
	n.env = append(env, s.Env...)
	argv := p.Runner(RoleGateway).Argv([]string{s.Binary}, g.CPUs)
	n.cmd = exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // the harness starts the binary it built
	n.cmd.Env = n.env
	n.cmd.Stdout, n.cmd.Stderr = io.Discard, n.errb
	n.cmd.Cancel = func() error { return n.cmd.Process.Signal(syscall.SIGTERM) }
	n.cmd.WaitDelay = nodeStopTimeout
	if err := n.cmd.Start(); err != nil {
		return nil, fmt.Errorf("harness: start ruralzd: %w", err)
	}
	go func() { // owned by the Node: Stop waits for done
		n.err = n.cmd.Wait()
		close(n.done)
	}()
	n.pid = n.cmd.Process.Pid
	n.adminURL = "http://127.0.0.1:" + strconv.Itoa(p.adminPort())
	if err := n.waitReady(ctx); err != nil {
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

// waitReady polls /readyz until it answers 200.
func (n *Node) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(nodeReadyTimeout)
	var last string
	for time.Now().Before(deadline) {
		select {
		case <-n.done:
			return fmt.Errorf("harness: ruralzd exited before ready (%v): %s", n.err, n.errb.String())
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
				return nil
			}
			last = fmt.Sprintf("%d %s", resp.StatusCode, b)
		} else {
			last = err.Error()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return fmt.Errorf("harness: ruralzd not ready within %v; last /readyz: %s; stderr: %s", nodeReadyTimeout, last, n.errb.String())
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
	if n.Exited() {
		return nil
	}
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
	var exit *exec.ExitError
	if n.err != nil && !(errors.As(n.err, &exit) && exit.ExitCode() == 0) && !errors.Is(n.err, exec.ErrWaitDelay) {
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
