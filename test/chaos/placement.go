// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package chaos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/test/bench/harness"
	"github.com/ravindu-rev/ruralz/test/bench/labhost/lab"
)

// TargetGOMAXPROCS is each Node's GOMAXPROCS at the target profile.
const TargetGOMAXPROCS = 1

// Placement places a run's dependencies on the hosts of a benchmark
// harness inventory, as the target profile runs on the four RH-1 hosts
// (docs/engineering/03-testing-and-quality-strategy.md "Scale profiles";
// docs/architecture/12-performance-budgets-and-benchmarking.md "Reference
// hardware"): the Nodes, the L4 balancers and the proxies on the gateway
// host, the harness's own, each Node with GOMAXPROCS 1 in a scope on the
// gateway cpuset they share; the mock Upstreams on the upstream host; the
// State Store's servers on the State Store host, from the pinned Valkey 9
// image unless the inventory names a server executable; and the load on
// the load host, which keeps every request's record there until the run's
// load has ended (loadSession). Every service on another host is
// test/bench/labhost, started over ssh as the inventory's dedicated user
// in a systemd-run scope on its host's cpuset when the inventory asks for
// scopes, and as a child process of the harness on the loopback
// inventory, which runs the same services on one host.
type Placement struct {
	// Profile is the inventory.
	Profile *harness.Profile
	// bins are labhost's paths on the hosts that run it.
	bins map[harness.Role]string
	// gateway and store are the IP addresses of the gateway and State
	// Store hosts; mockHost the host the mocks listen on.
	gateway, store, mockHost string
	// owner is the State Store host's "uid:gid", which a container of the
	// pinned image runs as; "" with a server executable.
	owner string
	// work is the harness's local working directory.
	work string
}

// placedRoles are the hosts a placement runs labhost on.
func placedRoles() []harness.Role {
	return []harness.Role{harness.RoleLoad, harness.RoleUpstream, harness.RoleStateStore}
}

// NewPlacement builds labhost from the repository at root into work,
// copies it to the inventory's load, upstream and State Store hosts and
// resolves their addresses.
func NewPlacement(ctx context.Context, p *harness.Profile, root, work string) (*Placement, error) {
	pl := &Placement{Profile: p, bins: map[harness.Role]string{}, work: work}
	local := filepath.Join(work, "labhost")
	if err := harness.BuildLabhost(ctx, root, local); err != nil {
		return nil, err
	}
	for _, role := range placedRoles() {
		dst := path.Join(p.Hosts[role].Dir, "bin", "labhost")
		if err := p.Runner(role).Put(ctx, local, dst, 0o700); err != nil {
			return nil, fmt.Errorf("chaos: placement: %w", err)
		}
		pl.bins[role] = dst
	}
	var err error
	if pl.gateway, err = resolveIP(ctx, p.Hosts[harness.RoleGateway].Address); err != nil {
		return nil, err
	}
	if pl.store, err = resolveIP(ctx, p.Hosts[harness.RoleStateStore].Address); err != nil {
		return nil, err
	}
	host, _, err := net.SplitHostPort(p.Services.Mock)
	if err != nil {
		return nil, fmt.Errorf("chaos: placement: services.mock %q: %w", p.Services.Mock, err)
	}
	pl.mockHost = host
	if p.Services.StateStoreServer == "" {
		if pl.owner, err = harness.Owner(ctx, p.Runner(harness.RoleStateStore)); err != nil {
			return nil, err
		}
	}
	return pl, nil
}

// resolveIP returns the IPv4 address, else the IP address, a lab host's
// name resolves to, the address the other hosts reach it at.
func resolveIP(ctx context.Context, host string) (string, error) {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String(), nil
	}
	ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return "", fmt.Errorf("chaos: placement: resolve %s: %w", host, err)
	}
	if len(ips) == 0 {
		return "", fmt.Errorf("chaos: placement: %s resolves to no address", host)
	}
	for _, ip := range ips {
		if ip.IP.To4() != nil {
			return ip.IP.String(), nil
		}
	}
	return ips[0].IP.String(), nil
}

// NodeCPUs returns the cpuset the Nodes share: the gateway host's when the
// inventory asks for scopes, else none.
func (pl *Placement) NodeCPUs() string {
	if !pl.Profile.Scope {
		return ""
	}
	return pl.Profile.Hosts[harness.RoleGateway].CPUs
}

// BalancerListen returns the address the L4 balancers listen on: the
// gateway host's, which the load host reaches.
func (pl *Placement) BalancerListen() string { return net.JoinHostPort(pl.gateway, "0") }

// StartMocks starts n mock Upstreams on the upstream host's measured
// cpuset, named backend-1 to backend-n as the chaos mocks are and keeping
// no request log, as benchmark runs keep none, and returns their
// addresses; they stop when t ends.
func (pl *Placement) StartMocks(ctx context.Context, t testing.TB, n int) ([]string, error) {
	r := pl.Profile.Runner(harness.RoleUpstream)
	cpus := pl.Profile.Hosts[harness.RoleUpstream].CPUs
	var addrs []string
	for i := range n {
		name := "backend-" + strconv.Itoa(i+1)
		argv := []string{
			pl.bins[harness.RoleUpstream], "mock", "-listen", net.JoinHostPort(pl.mockHost, "0"),
			"-body-size", "0", "-name", name,
		}
		p, err := r.Start(context.WithoutCancel(ctx), name, argv, cpus)
		if err != nil {
			return nil, err
		}
		t.Cleanup(func() { stopService(t, p, name) })
		addrs = append(addrs, p.Ready.Addr)
	}
	return addrs, nil
}

// stopService stops a labhost service at the end of t, logging the end of
// its standard error when it fails or t failed.
func stopService(t testing.TB, p *harness.Proc, name string) {
	if _, err := p.Stop(); err != nil {
		t.Errorf("chaos: stop %s: %v", name, err)
		return
	}
	if t.Failed() {
		t.Logf("chaos: %s's standard error:\n%s", name, p.Stderr())
	}
}

// StateStoreHost returns the State Store host as a redisserver.Host.
func (pl *Placement) StateStoreHost() redisserver.Host {
	h := &storeHost{pl: pl, r: pl.Profile.Runner(harness.RoleStateStore)}
	if net.ParseIP(pl.Profile.Hosts[harness.RoleStateStore].Address) == nil {
		h.names = []string{pl.Profile.Hosts[harness.RoleStateStore].Address}
	}
	return h
}

// storeHost runs a State Store's servers on the State Store host, each
// under labhost run in a scope on the host's measured cpuset: the
// inventory's server executable, or the pinned Valkey 9 image with host
// networking as the inventory's user, its deployment directory shared,
// supervised as the benchmark harness supervises Valkey there.
type storeHost struct {
	pl    *Placement
	r     *harness.Runner
	names []string
}

func (h *storeHost) Addr() string    { return h.pl.store }
func (h *storeHost) Names() []string { return h.names }

// Port asks labhost on the host for a port free on its address.
func (h *storeHost) Port(ctx context.Context) (int, error) {
	out, err := h.r.Output(ctx, []string{h.pl.bins[harness.RoleStateStore], "port", "-listen", net.JoinHostPort(h.pl.store, "0")}, nil)
	if err != nil {
		return 0, err
	}
	var doc struct {
		Port int `json:"port"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return 0, fmt.Errorf("chaos: labhost port answered %q: %w", out, err)
	}
	if doc.Port < 1 || doc.Port > 65535 {
		return 0, fmt.Errorf("chaos: labhost port answered %q, no port", out)
	}
	return doc.Port, nil
}

// MkdirTemp creates a directory on the host, under the host's working
// directory's chaos directory when dir is empty.
func (h *storeHost) MkdirTemp(ctx context.Context, dir, pattern string) (string, error) {
	if dir == "" {
		dir = path.Join(h.pl.Profile.Hosts[harness.RoleStateStore].Dir, "chaos")
		if _, err := h.r.Output(ctx, []string{"mkdir", "-p", dir}, nil); err != nil {
			return "", err
		}
	}
	out, err := h.r.Output(ctx, []string{"mktemp", "-d", "-p", dir, pattern + "XXXXXXXX"}, nil)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WriteFile copies data to the host through a file of the harness's
// working directory, mode 0600.
func (h *storeHost) WriteFile(ctx context.Context, name string, data []byte) error {
	f, err := os.CreateTemp(h.pl.work, "store-file-")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return h.r.Put(ctx, f.Name(), name, 0o600)
}

// RemoveAll removes dir on the host.
func (h *storeHost) RemoveAll(ctx context.Context, dir string) error {
	_, err := h.r.Output(ctx, []string{"rm", "-rf", "--", dir}, nil)
	return err
}

// Start starts one server under labhost run, ready once it wrote its
// readiness line and its port accepts connections.
func (h *storeHost) Start(ctx context.Context, s redisserver.HostServer) (redisserver.HostProcess, error) {
	container := ""
	if h.pl.Profile.Services.StateStoreServer == "" {
		suffix := make([]byte, 6)
		if _, err := rand.Read(suffix); err != nil {
			return nil, err
		}
		container = "ruralz-chaos-" + hex.EncodeToString(suffix) + "-" + s.Name
	}
	// The server outlives ctx, which bounds the start alone; labhost bounds
	// its wait for the readiness line, and the server's Close ends it.
	proc, err := h.r.Start(context.WithoutCancel(ctx), s.Name, h.pl.serverArgv(s, container), h.pl.Profile.Hosts[harness.RoleStateStore].CPUs)
	if err != nil {
		return nil, err
	}
	return &storeProc{p: proc}, nil
}

// serverArgv returns the command line of labhost run supervising the
// server s on the State Store host, ready once it wrote s.Ready and its
// port accepts connections: the inventory's server executable, or with
// container set the pinned image's container of that name, which takes
// its signals through docker kill.
func (pl *Placement) serverArgv(s redisserver.HostServer, container string) []string {
	argv := []string{
		pl.bins[harness.RoleStateStore], "run", "-ready", net.JoinHostPort(pl.store, strconv.Itoa(s.Port)), "-ready-line", s.Ready,
	}
	if container == "" {
		return append(append(argv, "--", pl.Profile.Services.StateStoreServer), s.Args...)
	}
	argv = append(argv, "-container", container, "--")
	return append(append(argv, dockerServer(pl.Profile, pl.owner, container, s.Dir)...), s.Args...)
}

// dockerServer returns the command that runs the pinned Valkey 9 image as
// one server of a deployment: host networking, as owner, the deployment
// directory dir shared at its own path, OpenFiles, and the State Store
// host's measured cpuset, since a container joins neither the scope nor
// the limits of the docker client labhost starts (harness.DockerValkey).
func dockerServer(p *harness.Profile, owner, container, dir string) []string {
	argv := []string{
		"docker", "run", "--rm", "--name", container, "--network", "host", "--user", owner,
		"--ulimit", fmt.Sprintf("nofile=%d:%d", harness.OpenFiles, harness.OpenFiles), "-v", dir + ":" + dir,
	}
	if c := p.Hosts[harness.RoleStateStore].CPUs; c != "" {
		argv = append(argv, "--cpuset-cpus", c)
	}
	return append(argv, redisserver.ImageValkey9, "valkey-server")
}

// storeProc is a server labhost run supervises on the State Store host.
type storeProc struct{ p *harness.Proc }

// Signal delivers sig through labhost, to the container's process when the
// server runs in one.
func (s *storeProc) Signal(sig os.Signal) error {
	name, ok := lab.SignalName(sig)
	if !ok {
		return fmt.Errorf("chaos: labhost delivers no signal %v", sig)
	}
	_, err := s.p.Do(lab.Request{Op: lab.OpSignal, Name: name})
	return err
}

// Kill delivers SIGKILL and waits until labhost saw the server exit.
func (s *storeProc) Kill(context.Context) error {
	if _, err := s.p.Do(lab.Request{Op: lab.OpSignal, Name: "SIGKILL"}); err != nil {
		return err
	}
	_, err := s.p.Do(lab.Request{Op: lab.OpWait})
	return err
}

// Close ends the labhost session, which stops the server if it runs.
func (s *storeProc) Close() error {
	_, err := s.p.Stop()
	return err
}

// Output returns the end of labhost's standard error, which carries the
// server's output.
func (s *storeProc) Output() string { return s.p.Stderr() }
