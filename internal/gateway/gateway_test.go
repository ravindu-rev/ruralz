// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/compile"
	"github.com/ravindu-rev/ruralz/internal/gateway/setting"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// testToken is the admin operator token of the test Nodes.
//
//nolint:gosec // G101: a test canary, not a credential.
const testToken = "Zm9vYmFyYmF6cXV4cXV1eGNvcmdlZ3JhdWx0"

// testPID is the process ID the test Nodes take.
const testPID = 4242

// epoch is where the fake process clocks start.
func epoch() time.Time { return time.Date(2026, time.March, 1, 12, 0, 0, 0, time.UTC) }

// syncBuffer is a bytes.Buffer safe for a Node's concurrent writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// lockedSource is a rand.Source safe for the Node's concurrent draws.
type lockedSource struct {
	mu  sync.Mutex
	src rand.Source
}

func (s *lockedSource) Uint64() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.src.Uint64()
}

// testProcess returns the process of a test Node: the fake clock clk, the
// real clock for socket deadlines, the environment env and a seeded
// random source; it registers no signal and leaves the library globals
// alone.
func testProcess(clk clock.Clock, env map[string]string) process {
	return process{
		clock: func() (clock.Clock, error) { return clk, nil },
		wall:  clock.Real(),
		lookupEnv: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		variables: func() subst.Source { return subst.Map{} },
		pid:       testPID,
		fileLimit: func() (uint64, bool) { return 1 << 20, true },
		rand:      &lockedSource{src: rand.NewPCG(1, 2)},
	}
}

// writeFile writes data to path, mode 0600.
func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRunRefusesStart(t *testing.T) {
	dir := t.TempDir()
	bundle := filepath.Join(dir, "ruralz.yaml")
	writeFile(t, bundle, "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec: {}\n")
	notDir := filepath.Join(dir, "file")
	writeFile(t, notDir, "x")
	base := func(kv ...string) map[string]string {
		env := map[string]string{setting.EnvConfig: bundle, setting.EnvDataDir: filepath.Join(t.TempDir(), "data")}
		for i := 0; i+1 < len(kv); i += 2 {
			if kv[i+1] == "" {
				delete(env, kv[i])
				continue
			}
			env[kv[i]] = kv[i+1]
		}
		return env
	}
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want []string
	}{
		{"argument", []string{"--config"}, base(), []string{"accepts no arguments", "usage: ruralzd"}},
		{"no configuration source", nil, base(setting.EnvConfig, ""), []string{setting.EnvConfig}},
		{"invalid log level", nil, base(setting.EnvLogLevel, "loud"), []string{setting.EnvLogLevel}},
		{"invalid bind loopback", nil, base(setting.EnvBindLoopback, "yes"), []string{setting.EnvBindLoopback}},
		{"data directory is a file", nil, base(setting.EnvDataDir, notDir), []string{setting.EnvDataDir}},
		{"admin token file missing", nil, base(setting.EnvAdminTokenFile, filepath.Join(dir, "absent")), []string{"admin credentials"}},
		{"admin token too short", nil, base(setting.EnvAdminTokenFile, writeToken(t, dir, "short")), []string{"admin credentials"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr syncBuffer
			code := run(t.Context(), tt.args, &stdout, &stderr, testProcess(clocktest.New(epoch()), tt.env))
			if code != ExitUsage {
				t.Errorf("run() = %d, want %d; stderr:\n%s", code, ExitUsage, stderr.String())
			}
			for _, w := range tt.want {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr does not name %q:\n%s", w, stderr.String())
				}
			}
		})
	}
}

// writeToken writes an admin token file holding tok and returns its path.
func writeToken(t *testing.T, dir, tok string) string {
	t.Helper()
	p := filepath.Join(dir, "token-"+tok)
	writeFile(t, p, tok+"\n")
	return p
}

func TestRunClockErrors(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code int
		want string
	}{
		{"test build without clock commands", clock.ErrNoCommandDescriptor, ExitUsage, "runs only under the test harness"},
		{"wrapped missing descriptor", fmt.Errorf("start: %w", clock.ErrNoCommandDescriptor), ExitUsage, "runs only under the test harness"},
		{"other clock failure", errors.New("clock socket closed"), ExitRuntime, "process clock: clock socket closed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testProcess(nil, map[string]string{})
			opened := false
			p.clock = func() (clock.Clock, error) { return nil, tt.err }
			p.variables = func() subst.Source { opened = true; return subst.Map{} }
			var stdout, stderr syncBuffer
			if code := run(t.Context(), nil, &stdout, &stderr, p); code != tt.code {
				t.Errorf("run() = %d, want %d", code, tt.code)
			}
			if !strings.Contains(stderr.String(), tt.want) {
				t.Errorf("stderr = %q, want %q", stderr.String(), tt.want)
			}
			if opened {
				t.Error("run read the environment before the process clock")
			}
		})
	}
}

func TestReportSettings(t *testing.T) {
	var b strings.Builder
	reportSettings(&b, errors.Join(errors.New("RURALZ_LOG_LEVEL: bad"), fmt.Errorf("x: %w", setting.ErrUsage)))
	got := b.String()
	if !strings.HasPrefix(got, "ruralzd: RURALZ_LOG_LEVEL: bad\nruralzd: x: ") || !strings.HasSuffix(got, setting.Usage) {
		t.Errorf("reportSettings() wrote %q", got)
	}
	b.Reset()
	reportSettings(&b, errors.New("RURALZ_ZONE: bad"))
	if got := b.String(); got != "ruralzd: RURALZ_ZONE: bad\n" {
		t.Errorf("reportSettings() wrote %q without a usage error, want one line", got)
	}
}

func TestSupervisor(t *testing.T) {
	tests := []struct {
		name     string
		start    func(s *supervisor)
		stop     bool
		wantFail string
	}{
		{"part returns before stop", func(s *supervisor) {
			s.goRun("retirer", func(context.Context) error { return nil })
		}, false, "retirer: returned before the Node stopped it"},
		{"part fails before stop", func(s *supervisor) {
			s.goRun("watchdog", func(context.Context) error { return errors.New("boom") })
		}, false, "watchdog: boom"},
		{"part panics", func(s *supervisor) {
			s.goRun("activation loop", func(context.Context) error { panic("bad state") })
		}, false, "activation loop: panic: bad state"},
		{"part ended by the stop", func(s *supervisor) {
			s.goRun("retirer", func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() })
		}, true, ""},
		{"function returns", func(s *supervisor) {
			s.goFunc("boot", func(context.Context) {})
		}, false, ""},
		{"function panics", func(s *supervisor) {
			s.goFunc("boot", func(context.Context) { panic(errors.New("nil map")) })
		}, false, "boot: panic: nil map"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			var mu sync.Mutex
			var fails []string
			var logs syncBuffer
			s := newSupervisor(ctx, slog.New(slog.NewJSONHandler(&logs, nil)), func(err error) {
				mu.Lock()
				defer mu.Unlock()
				fails = append(fails, err.Error())
			})
			tt.start(s)
			if tt.stop {
				cancel()
			}
			if err := s.wait(t.Context()); err != nil {
				t.Fatal(err)
			}
			cancel()
			mu.Lock()
			defer mu.Unlock()
			switch {
			case tt.wantFail == "" && len(fails) > 0:
				t.Errorf("fail(%q), want none", fails)
			case tt.wantFail != "" && (len(fails) != 1 || fails[0] != tt.wantFail):
				t.Errorf("fail(%q), want %q", fails, tt.wantFail)
			case tt.wantFail != "" && !strings.Contains(logs.String(), `"level":"ERROR"`):
				t.Errorf("no error record:\n%s", logs.String())
			}
		})
	}
}

func TestSupervisorWaitBounded(t *testing.T) {
	s := newSupervisor(t.Context(), slog.New(slog.DiscardHandler), func(error) {})
	release := make(chan struct{})
	s.goFunc("stuck", func(context.Context) { <-release })
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.wait(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("wait() = %v, want context.Canceled", err)
	}
	close(release)
	if err := s.wait(t.Context()); err != nil {
		t.Errorf("wait() = %v after the goroutine ended", err)
	}
}

// TestSuccessorWatchdog checks the interval a handover successor's
// watchdog runs with once it took the lock: its own when its environment
// enables one, even beside its holder's; else the one its holder's
// accepted carried, as for a child of systemd's ExecReload= helper; none
// when neither gives one, as from a holder without a watchdog.
func TestSuccessorWatchdog(t *testing.T) {
	tests := []struct {
		name        string
		own, holder time.Duration
		want        time.Duration
		wantFrom    string
	}{
		{"own environment beside the holder's", 6 * time.Second, 30 * time.Second, 6 * time.Second, watchdogFromEnvironment},
		{"own environment alone", 6 * time.Second, 0, 6 * time.Second, watchdogFromEnvironment},
		{"the holder's for a child of ExecReload=", 0, 30 * time.Second, 30 * time.Second, watchdogFromHolder},
		{"neither", 0, 0, 0, watchdogFromHolder},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, from := successorWatchdog(tt.own, tt.holder)
			if got != tt.want || from != tt.wantFrom {
				t.Errorf("successorWatchdog(%v, %v) = %v, %q; want %v, %q", tt.own, tt.holder, got, from, tt.want, tt.wantFrom)
			}
		})
	}
}

func TestProbeAddr(t *testing.T) {
	tests := []struct{ in, want string }{
		{"0.0.0.0:9901", "127.0.0.1:9901"},
		{"[::]:9901", "127.0.0.1:9901"},
		{"127.0.0.1:9901", "127.0.0.1:9901"},
		{"[::1]:9901", "[::1]:9901"},
		{"10.0.0.7:80", "10.0.0.7:80"},
	}
	for _, tt := range tests {
		a, err := net.ResolveTCPAddr("tcp", tt.in)
		if err != nil {
			t.Fatal(err)
		}
		if got := probeAddr(a); got != tt.want {
			t.Errorf("probeAddr(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestProbeHealthz(t *testing.T) {
	healthz := func(code int) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
				w.WriteHeader(http.StatusTeapot)
				return
			}
			w.WriteHeader(code)
		})
	}
	plain := httptest.NewServer(healthz(http.StatusOK))
	defer plain.Close()
	failing := httptest.NewServer(healthz(http.StatusServiceUnavailable))
	defer failing.Close()
	secure := httptest.NewTLSServer(healthz(http.StatusOK))
	defer secure.Close()
	// httptest's servers share one certificate: the other server presents
	// one of its own.
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	other := httptest.NewUnstartedServer(healthz(http.StatusOK))
	if other.TLS, err = leaf.ServerTLS(nil); err != nil {
		t.Fatal(err)
	}
	other.StartTLS()
	defer other.Close()
	pinned := secure.Certificate().Raw
	tests := []struct {
		name    string
		addr    string
		tls     bool
		pinned  []byte
		wantErr error
		errText string
	}{
		{"plain admin server", plain.Listener.Addr().String(), false, nil, nil, ""},
		{"unhealthy answer", failing.Listener.Addr().String(), false, nil, nil, "answered 503"},
		{"pinned certificate", secure.Listener.Addr().String(), true, pinned, nil, ""},
		{"other certificate", other.Listener.Addr().String(), true, pinned, errNotPinned, ""},
		{"no pinned certificate", secure.Listener.Addr().String(), true, nil, errNotPinned, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := probeHealthz(t.Context(), tt.addr, tt.tls, tt.pinned)
			switch {
			case tt.wantErr != nil:
				if !errors.Is(err, tt.wantErr) {
					t.Errorf("probeHealthz() = %v, want %v", err, tt.wantErr)
				}
			case tt.errText != "":
				if err == nil || !strings.Contains(err.Error(), tt.errText) {
					t.Errorf("probeHealthz() = %v, want %q", err, tt.errText)
				}
			case err != nil:
				t.Errorf("probeHealthz() = %v", err)
			}
		})
	}
}

// compiled is a reload.Compiled holding only a snapshot.
type compiled struct{ snap *snapshot.Snapshot }

func (c compiled) Snapshot() *snapshot.Snapshot { return c.snap }
func (compiled) Secrets() secret.Store          { return nil }
func (compiled) Warnings() []compile.Warning    { return nil }
func (compiled) UpstreamConnections() int       { return 0 }
func (compiled) Activate()                      {}
func (compiled) Discard()                       {}

func TestBudgetPartSetsTotal(t *testing.T) {
	b := body.NewBudget(body.DefaultMaxBufferedBytes, nil)
	p := budgetPart{budget: b}
	snap := &snapshot.Snapshot{}
	snap.Gateway.Limits.MaxBufferedBytes = 64 << 20
	sw, err := p.Prepare(t.Context(), compiled{snap: snap})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Total(); got != body.DefaultMaxBufferedBytes {
		t.Errorf("Prepare changed the total to %d", got)
	}
	sw.Abort()
	if got := b.Total(); got != body.DefaultMaxBufferedBytes {
		t.Errorf("Abort changed the total to %d", got)
	}
	if err := sw.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := b.Total(); got != 64<<20 {
		t.Errorf("Total() = %d after Commit, want %d", got, 64<<20)
	}
}
