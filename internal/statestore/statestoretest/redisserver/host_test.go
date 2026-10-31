// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// labHost is a Host on this machine standing in for a lab host: its
// working directory a test directory, its servers the fake redis-server,
// started as a lab host's helper starts one, ready once it wrote its
// readiness line and its port accepts a connection. It records what the
// launcher asked of it.
type labHost struct {
	root  string
	names []string
	// ports, when set, replaces the free-port probe.
	ports func(context.Context) (int, error)

	mu      sync.Mutex
	started []HostServer
	procs   []*labHostProc
	written []string
}

func newLabHost(t *testing.T, names ...string) *labHost {
	t.Helper()
	return &labHost{root: t.TempDir(), names: names}
}

func (h *labHost) Addr() string    { return loopback }
func (h *labHost) Names() []string { return h.names }

func (h *labHost) Port(ctx context.Context) (int, error) {
	if h.ports != nil {
		return h.ports(ctx)
	}
	return freePort(ctx)
}

func (h *labHost) MkdirTemp(_ context.Context, dir, pattern string) (string, error) {
	if dir == "" {
		dir = h.root
	}
	return os.MkdirTemp(dir, pattern)
}

func (h *labHost) WriteFile(_ context.Context, name string, data []byte) error {
	h.mu.Lock()
	h.written = append(h.written, name)
	h.mu.Unlock()
	return os.WriteFile(name, data, 0o600)
}

func (h *labHost) RemoveAll(_ context.Context, dir string) error { return os.RemoveAll(dir) }

func (h *labHost) Start(ctx context.Context, s HostServer) (HostProcess, error) {
	h.mu.Lock()
	s.Args = slices.Clone(s.Args)
	h.started = append(h.started, s)
	h.mu.Unlock()
	p, err := proc.Start(context.WithoutCancel(ctx), proc.Spec{
		Name: s.Name, Path: os.Args[0], Args: append([]string{"-test.run=^TestHelperRedisServer$", "-test.count=1"}, s.Args...),
		Env: append(childEnv(), helperEnv+"=1"), RingBytes: outputBytes,
	})
	if err != nil {
		return nil, err
	}
	hp := &labHostProc{p: p}
	h.mu.Lock()
	h.procs = append(h.procs, hp)
	h.mu.Unlock()
	if _, err := p.WaitOutput(ctx, regexp.MustCompile(s.Ready)); err != nil {
		_ = hp.Close()
		return nil, fmt.Errorf("%s did not write %q: %w: %s", s.Name, s.Ready, err, hp.Output())
	}
	addr := net.JoinHostPort(h.Addr(), strconv.Itoa(s.Port))
	for {
		c, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = c.Close()
			return hp, nil
		}
		select {
		case <-p.Done():
			return nil, fmt.Errorf("%s exited before serving on %s: %s", s.Name, addr, hp.Output())
		case <-ctx.Done():
			_ = hp.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (h *labHost) starts() []HostServer {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.started)
}

func (h *labHost) files() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.written)
}

// servers returns the processes the host started, in order.
func (h *labHost) servers() []*labHostProc {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.procs)
}

// labHostProc is a labHost's server; closed reports a Close, which
// releases what the host holds for it.
type labHostProc struct {
	p      *proc.Process
	mu     sync.Mutex
	closed bool
}

func (f *labHostProc) isClosed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

func (f *labHostProc) Signal(sig os.Signal) error { return f.p.Signal(sig) }

func (f *labHostProc) Kill(ctx context.Context) error {
	if err := f.p.Kill(); err != nil {
		return err
	}
	select {
	case <-f.p.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *labHostProc) Close() error {
	_ = f.p.Kill()
	<-f.p.Done()
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	return nil
}

func (f *labHostProc) Output() string {
	out, errOut := f.p.Output()
	return string(out) + string(errOut)
}

// hostCert reads the server certificate a deployment keeps on the host.
func hostCert(t *testing.T, dir string) *x509.Certificate {
	t.Helper()
	b, err := os.ReadFile(path.Join(dir, "server.crt")) //nolint:gosec // G304: the file the launcher wrote on the fake host
	if err != nil {
		t.Fatal(err)
	}
	blk, _ := pem.Decode(b)
	if blk == nil {
		t.Fatalf("server.crt holds no PEM block")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestStartOnHost: a deployment on a Host keeps every file of it, the
// configurations and the TLS files, in a directory on the host, which the
// end of the test removes; each server starts through the host, bound to
// the host's address, with its configuration file; the certificate covers
// the host's address and names beside loopback; the CA the clients trust
// is a copy on the test's own machine; and Kill, Restart and the
// replica's link reach the servers on the host, a Restart releasing the
// killed process's hold on the host.
func TestStartOnHost(t *testing.T) {
	h := newLabHost(t, "statestore.test")
	var dir string
	t.Run("deployment", func(t *testing.T) {
		users := []User{{Name: "ruralz", Password: "pw"}}
		d := StartOn(t, h, Process, Options{TLS: true, Users: users, Replica: true})
		p, r := d.Primary(), d.Replica()
		if host, _, _ := net.SplitHostPort(p.Addr()); host != h.Addr() || r == nil {
			t.Fatalf("primary at %s, replica %v; want both on the host's address", p.Addr(), r)
		}
		starts := h.starts()
		if len(starts) != 2 || starts[0].Name != p.Name() || starts[1].Name != r.Name() {
			t.Fatalf("the host started %+v, want the primary then the replica", starts)
		}
		dir = starts[0].Dir
		for _, s := range starts {
			if s.Dir != dir || !strings.HasPrefix(dir, h.root) || len(s.Args) != 1 || !strings.HasPrefix(s.Args[0], dir+"/") || path.Base(s.Args[0]) != "redis.conf" {
				t.Errorf("start %+v: want the configuration file under the deployment directory %s on the host", s, dir)
			}
		}
		for _, f := range h.files() {
			if !strings.HasPrefix(f, dir+"/") {
				t.Errorf("wrote %s outside the deployment directory %s", f, dir)
			}
		}
		if !slices.Contains(confLines(t, p), "bind "+h.Addr()) {
			t.Errorf("the primary's configuration %q binds no host address", confLines(t, p))
		}
		c := hostCert(t, dir)
		if !slices.Contains(c.DNSNames, "statestore.test") || !slices.ContainsFunc(c.IPAddresses, func(ip net.IP) bool { return ip.Equal(net.ParseIP(h.Addr())) }) {
			t.Errorf("the certificate names %q and %v, want the host's name and address", c.DNSNames, c.IPAddresses)
		}
		if strings.HasPrefix(d.CAFile(), h.root) {
			t.Errorf("CAFile %s is on the host, want a copy on the test's machine", d.CAFile())
		}
		if _, err := os.ReadFile(d.CAFile()); err != nil {
			t.Errorf("CAFile: %v", err)
		}
		if got := configArg(t, r, "replicaof"); got != h.Addr()+" "+strconv.Itoa(p.node.port) {
			t.Errorf("replicaof = %q, want the primary on the host", got)
		}
		mustDo(t, p, "SET", "k", "v")
		if err := p.Kill(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := p.Do(t.Context(), "PING"); err == nil {
			t.Fatal("PING after Kill succeeded")
		}
		if err := p.Kill(t.Context()); err != nil {
			t.Errorf("Kill of a killed server = %v, want nil", err)
		}
		if err := p.Restart(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := mustDo(t, p, "GET", "k"); !got.Null {
			t.Errorf("GET after Restart = %v, want nil", got)
		}
		if got := h.starts(); len(got) != 3 || got[2].Name != p.Name() || got[2].Port != p.node.port {
			t.Errorf("after Restart the host started %+v, want the primary again on its port", got)
		}
		if procs := h.servers(); !procs[0].isClosed() || procs[2].isClosed() {
			t.Error("after Restart the killed primary's process is still held on the host, or the new one is not")
		}
	})
	if dir == "" {
		t.Fatal("no deployment directory")
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the deployment directory after the test: %v, want it removed", err)
	}
}

// TestStartOnHostTopologies: the sentinel and cluster kinds run on a Host
// as on this machine, every server started through it, the sentinel with
// --sentinel and awaited by the sentinel's readiness line, the others by
// the server's, and their URLs name the host's address.
func TestStartOnHostTopologies(t *testing.T) {
	for _, tc := range []struct {
		kind    Kind
		servers int
	}{{ProcessSentinel, 3}, {ProcessCluster, 6}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			h := newLabHost(t)
			d := StartOn(t, h, tc.kind, Options{})
			starts := h.starts()
			if len(starts) != tc.servers {
				t.Fatalf("the host started %d servers, want %d", len(starts), tc.servers)
			}
			sentinels := 0
			for _, s := range starts {
				want := readyServer
				if slices.Contains(s.Args, "--sentinel") {
					sentinels++
					want = readySentinel
				}
				if s.Ready != want {
					t.Errorf("%s awaited by %q, want %q", s.Name, s.Ready, want)
				}
			}
			if want := len(d.Sentinels()); sentinels != want {
				t.Errorf("%d servers started with --sentinel, want %d", sentinels, want)
			}
			if !strings.Contains(d.URL(), "://"+h.Addr()+":") {
				t.Errorf("URL %q, want the host's address", d.URL())
			}
			if tc.kind.Topology() == statestore.TopologyCluster && len(d.Replicas()) != 3 {
				t.Errorf("%d replicas, want one per primary", len(d.Replicas()))
			}
		})
	}
}

// TestStartOnRefuses: a Host runs process kinds only, refuses an Announce
// for a cluster, whose replicas would replicate through it from the host,
// an address that is not an IP address and invalid Options, before
// starting anything.
func TestStartOnRefuses(t *testing.T) {
	announce := func(int, string) string { return "127.0.0.2:7000" }
	for _, tc := range []struct {
		name string
		kind Kind
		o    Options
		host *labHost
		want string
	}{
		{"container kind", Valkey9, Options{}, nil, "not a process kind"},
		{"announced cluster", ProcessCluster, Options{Announce: announce}, nil, "takes no Announce"},
		{"replica of a cluster", ProcessCluster, Options{Replica: true}, nil, "has a replica per primary already"},
		{"host name", Process, Options{}, &labHost{}, "not an IP address"},
	} {
		h := tc.host
		if h == nil {
			h = newLabHost(t)
		}
		var hostIface Host = h
		if tc.host != nil {
			hostIface = namedHost{h}
		}
		if _, err := startOn(t, hostIface, tc.kind, tc.o); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: startOn = %v, want an error naming %q", tc.name, err, tc.want)
		}
		if got := h.starts(); len(got) > 0 {
			t.Errorf("%s: the host started %+v", tc.name, got)
		}
	}
}

// namedHost is a labHost that gives a DNS name for its address.
type namedHost struct{ *labHost }

func (namedHost) Addr() string { return "statestore.test" }

// TestStartOnRetriesTakenPort: a port another process takes on the host
// before the server binds it costs a retry on a fresh port, recognized by
// the output the host's start error carries.
func TestStartOnRetriesTakenPort(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", loopback+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	taken := ln.Addr().(*net.TCPAddr).Port
	h := newLabHost(t)
	calls := 0
	h.ports = func(ctx context.Context) (int, error) {
		if calls++; calls == 1 {
			return taken, nil
		}
		return freePort(ctx)
	}
	d := StartOn(t, h, Process, Options{})
	if _, port, _ := net.SplitHostPort(d.Primary().Addr()); port == strconv.Itoa(taken) || len(h.starts()) != 2 {
		t.Fatalf("primary at %s after %d starts, want a second start on a free port", d.Primary().Addr(), len(h.starts()))
	}
}
