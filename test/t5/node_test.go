// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Real-time bounds of the waits on a Node (targets of
// docs/operations/02-zero-downtime-upgrades-and-hot-reload.md).
const (
	// readyBound bounds the wait for /readyz to answer 200, as the
	// process harness's 30 s.
	readyBound = 30 * time.Second
	// exitAfterReleaseBound bounds a handed-over process's exit after it
	// released the lock: the handover Drain's 20 s deadline plus the 5 s
	// flush, 28 s after steering, which precedes the release by the 3 s
	// linger (Drain timeline defaults).
	exitAfterReleaseBound = 28 * time.Second
	// pollEvery spaces condition polls; it never decides an outcome.
	pollEvery = 50 * time.Millisecond
)

// adminToken is the admin operator token the Nodes under test read: 22 or
// more RFC 6750 b64token characters.
const adminToken = "t5-admin-token-0123456789abcdef"

// invalidBundle is a ruralz.yaml that does not parse, so a Node has no
// Revision to boot from it.
const invalidBundle = "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata: [unclosed\n"

// bundle returns the one-file Bundle under test: listener http on web, the
// admin port on adminPort and Route t5 sending / through the headers
// Policy t5-header to the Upstream at upstream, which a Node reaches with
// RURALZ_FETCH_ALLOW=127.0.0.1.
func bundle(upstream string, web, adminPort int) []byte {
	return []byte(`apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: t5
spec:
  admin:
    port: ` + strconv.Itoa(adminPort) + `
  listeners:
    - name: http
      protocol: http
      port: ` + strconv.Itoa(web) + `
---
apiVersion: ruralz/v1alpha1
kind: Upstream
metadata:
  name: t5
spec:
  protocol: http
  endpoints:
    - address: "` + upstream + `"
---
apiVersion: ruralz/v1alpha1
kind: Policy
metadata:
  name: t5-header
spec:
  type: headers
  config:
    response:
      set: [{name: ` + strings.ToLower(policyHeader) + `, value: ` + policyValue + `}]
---
apiVersion: ruralz/v1alpha1
kind: Route
metadata:
  name: t5
spec:
  match:
    path: {prefix: /}
  policies:
    - name: t5-header
  upstreams:
    - name: t5
`)
}

// writeAtomic replaces the file at path with data, atomically: a hidden
// temporary file beside it, which the Bundle loader never reads, renamed
// over it.
func writeAtomic(path string, data []byte, mode os.FileMode) error {
	tmp := filepath.Join(filepath.Dir(path), ".t5-"+filepath.Base(path))
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	if err := os.Chmod(tmp, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// freePort returns a TCP port free on 127.0.0.1 when it returns.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	return ln.Addr().(*net.TCPAddr).Port
}

// adminClient sends one request per connection, so it holds no
// connection a Drain waits for.
func adminClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true}, Timeout: 10 * time.Second}
}

// adminGet sends GET path to the admin port at base, with the operator
// token when auth is set, and returns the status and body.
func adminGet(ctx context.Context, base, path string, auth bool) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return 0, nil, err
	}
	if auth {
		req.Header.Set("Authorization", "Bearer "+adminToken)
	}
	res, err := adminClient().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	return res.StatusCode, b, err
}

// readyz returns the /readyz status and body of the admin port at base.
func readyz(ctx context.Context, base string) (int, adminapi.Readyz, error) {
	code, b, err := adminGet(ctx, base, admin.PathReadyz, false)
	if err != nil {
		return 0, adminapi.Readyz{}, err
	}
	var v adminapi.Readyz
	if err := json.Unmarshal(b, &v); err != nil {
		return code, v, fmt.Errorf("/readyz body %q: %w", b, err)
	}
	return code, v, nil
}

// snapshots returns /debug/snapshots of the admin port at base.
func snapshots(ctx context.Context, base string) (adminapi.Snapshots, error) {
	var v adminapi.Snapshots
	code, b, err := adminGet(ctx, base, admin.PathSnapshots, true)
	if err != nil {
		return v, err
	}
	if code != http.StatusOK {
		return v, fmt.Errorf("GET /debug/snapshots = %d: %s", code, b)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return v, fmt.Errorf("/debug/snapshots body %q: %w", b, err)
	}
	return v, nil
}

// get sends one GET / to the listener at base and checks the response.
func get(ctx context.Context, base string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/", nil)
	if err != nil {
		return err
	}
	res, err := adminClient().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	return checkResponse(res.StatusCode, res.Proto, res.Header, b, loadgen.HTTP1)
}

// waitFor polls cond every pollEvery until it holds, within bound of real
// time, and returns the last reason it gave otherwise.
func waitFor(ctx context.Context, bound time.Duration, cond func() (bool, string)) error {
	ctx, cancel := context.WithTimeout(ctx, bound)
	defer cancel()
	tick := time.NewTicker(pollEvery)
	defer tick.Stop()
	for {
		ok, why := cond()
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("not within %v: %s", bound, why)
		case <-tick.C:
		}
	}
}

// waitReady waits until /readyz of the admin port at base answers 200 and
// returns its body.
func waitReady(ctx context.Context, base string) (adminapi.Readyz, error) {
	var last adminapi.Readyz
	err := waitFor(ctx, readyBound, func() (bool, string) {
		code, v, err := readyz(ctx, base)
		last = v
		if err != nil {
			return false, err.Error()
		}
		return code == http.StatusOK, fmt.Sprintf("/readyz %d %+v", code, v)
	})
	return last, err
}

// holderPID returns the pid member of the holder.json in dir.
func holderPID(dir string) (int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "holder.json")) //nolint:gosec // G304: the data directory under test
	if err != nil {
		return 0, err
	}
	var v struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return 0, fmt.Errorf("holder.json: %w", err)
	}
	return v.PID, nil
}

// nodeDir is the directory of a ruralzd the helper checks run outside
// systemd: a Bundle directory routing to a mock Upstream, a data
// directory, an admin token and the free ports its Bundle names.
type nodeDir struct {
	dir, config, data, token string
	web, admin               int
}

// newNodeDir writes a nodeDir whose Bundle routes to upstream.
func newNodeDir(t *testing.T, upstream string) *nodeDir {
	t.Helper()
	dir := t.TempDir()
	d := &nodeDir{
		dir: dir, config: filepath.Join(dir, "bundle"), data: filepath.Join(dir, "data"),
		token: filepath.Join(dir, "admin-token"), web: freePort(t), admin: freePort(t),
	}
	if err := os.Mkdir(d.config, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(d.token, []byte(adminToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.config, "ruralz.yaml"), bundle(upstream, d.web, d.admin), 0o600); err != nil {
		t.Fatal(err)
	}
	return d
}

// env returns the environment the Node and the helper run with, as a unit
// passes its settings.
func (d *nodeDir) env() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		setting.EnvConfig + "=" + d.config,
		setting.EnvDataDir + "=" + d.data,
		setting.EnvAdminTokenFile + "=" + d.token,
		setting.EnvBindLoopback + "=true",
		setting.EnvFetchAllow + "=127.0.0.1",
		setting.EnvLogLevel + "=debug",
	}
}

// adminURL returns the admin port's base URL.
func (d *nodeDir) adminURL() string { return "http://127.0.0.1:" + strconv.Itoa(d.admin) }

// webURL returns the listener's base URL.
func (d *nodeDir) webURL() string { return "http://127.0.0.1:" + strconv.Itoa(d.web) }

// nodeProc is one ruralzd the helper checks started.
type nodeProc struct {
	cmd  *exec.Cmd
	log  string
	done chan struct{}
	err  error
}

// startNode starts bin with env, its output in log; t's end kills it.
func startNode(t *testing.T, bin string, env []string, log string) *nodeProc {
	t.Helper()
	out, err := os.Create(log) //nolint:gosec // G304: a log file in the test's directory
	if err != nil {
		t.Fatal(err)
	}
	// The test's context ends just before its cleanup, which ends the
	// process too.
	cmd := exec.CommandContext(t.Context(), bin) //nolint:gosec // G204: the ruralzd under test
	cmd.Env, cmd.Stdout, cmd.Stderr = env, out, out
	if err := cmd.Start(); err != nil {
		_ = out.Close()
		t.Fatal(err)
	}
	p := &nodeProc{cmd: cmd, log: log, done: make(chan struct{})}
	go func() {
		defer close(p.done)
		p.err = cmd.Wait()
		_ = out.Close()
	}()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-p.done
	})
	return p
}

// pid returns the process ID.
func (p *nodeProc) pid() int { return p.cmd.Process.Pid }

// exited reports whether the process has exited.
func (p *nodeProc) exited() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

// waitExit waits up to exitAfterReleaseBound for the process to exit and
// returns its exit status, or an error when it still runs.
func (p *nodeProc) waitExit() (int, error) {
	select {
	case <-p.done:
	case <-time.After(exitAfterReleaseBound):
		return 0, fmt.Errorf("process %d still runs after %v", p.pid(), exitAfterReleaseBound)
	}
	var ee *exec.ExitError
	switch {
	case p.err == nil:
		return 0, nil
	case errors.As(p.err, &ee):
		return ee.ExitCode(), nil
	}
	return 0, p.err
}

// signal sends sig to the process.
func (p *nodeProc) signal(sig syscall.Signal) { _ = p.cmd.Process.Signal(sig) }

// output returns the process's log.
func (p *nodeProc) output() string {
	b, err := os.ReadFile(p.log)
	if err != nil {
		return err.Error()
	}
	return string(b)
}
