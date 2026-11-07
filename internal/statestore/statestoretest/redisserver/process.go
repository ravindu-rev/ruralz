// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// Bounds of the process flavor. The timeouts only end a broken test.
const (
	// startTimeout bounds starting a deployment, cluster formation and
	// replica sync included.
	startTimeout = time.Minute
	// portAttempts bounds the retries when another process takes a free
	// port before redis-server binds it.
	portAttempts = 5
	// outputBytes bounds the output kept per stream of a process.
	outputBytes = 1 << 20
	// tailBytes is the output an error or a failed test shows.
	tailBytes = 8 << 10
)

// errPortInUse marks a start that lost its port to another process.
var errPortInUse = errors.New("redisserver: port in use")

// portInUse is what a redis-server that lost its port writes.
const portInUse = "Address already in use"

// loopback is the address the servers of this machine bind.
const loopback = "127.0.0.1"

// launcher starts the redis-server processes of one test.
type launcher struct {
	tb   testing.TB
	path string // the redis-server binary of this machine
	// args are the arguments ahead of the configuration file: the test
	// binary's flags when the tests run it as their fake redis-server.
	args []string
	env  []string // the complete environment of the processes
	// ports returns a free port of the servers' address.
	ports func(context.Context) (int, error)
	// held are the ports of the servers this launcher started.
	held map[int]bool
}

// ip returns the address the servers bind and announce, and the test and
// the clients dial.
func (l *launcher) ip() string { return loopback }

// join joins the elements of a server file's path.
func (l *launcher) join(elem ...string) string { return filepath.Join(elem...) }

// nodeDir creates a fresh directory for the server name under base.
func (l *launcher) nodeDir(_ context.Context, base, name string) (string, error) {
	dir, err := os.MkdirTemp(base, name+"-")
	if err != nil {
		return "", fmt.Errorf("redisserver: %w", err)
	}
	return dir, nil
}

// writeFile writes a file of the deployment, mode 0600.
func (l *launcher) writeFile(_ context.Context, name string, data []byte) error {
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	return nil
}

// port returns a free port that no server of this launcher holds and that
// is not avoid. ports can return a port it returned before, because a
// port is free again once its probe closes.
func (l *launcher) port(ctx context.Context, avoid int) (int, error) {
	for range portAttempts {
		p, err := l.ports(ctx)
		if err != nil {
			return 0, err
		}
		if p != avoid && !l.held[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("redisserver: no unused port in %d picks", portAttempts)
}

// childEnv is the environment of a redis-server process: nothing, but
// SYSTEMROOT where it is set, without which Windows sockets fail.
func childEnv() []string {
	if v := os.Getenv("SYSTEMROOT"); v != "" {
		return []string{"SYSTEMROOT=" + v}
	}
	return []string{}
}

// heldPort returns the ports of the servers this machine runs for tb's
// test: loopback ports held until the test ends (proc.HoldPort), so no
// connection takes a port between its pick and the server's bind.
func heldPort(tb testing.TB) func(context.Context) (int, error) {
	return func(ctx context.Context) (int, error) { return proc.HoldPort(ctx, tb) }
}

// writeTLS writes a fresh CA and a certificate for localhost, 127.0.0.1
// and ::1 into base. The certificate serves both ends: a replica and a
// cluster node present it as a client too, and the cluster bus requires
// client certificates. caFile is the CA file, for Deployment.CAFile.
func (l *launcher) writeTLS(ctx context.Context, base string) (ca *pki.CA, files *tlsFiles, caFile string, err error) {
	ca, err = pki.NewCA(pki.CAOptions{CommonName: "Ruralz Test State Store CA"})
	if err != nil {
		return nil, nil, "", fmt.Errorf("redisserver: %w", err)
	}
	names := []string{"localhost"}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	leaf, err := ca.Issue(pki.LeafOptions{CommonName: "localhost", DNSNames: names, IPAddresses: ips})
	if err != nil {
		return nil, nil, "", fmt.Errorf("redisserver: %w", err)
	}
	files = &tlsFiles{ca: l.join(base, "ca.crt"), cert: l.join(base, "server.crt"), key: l.join(base, "server.key")}
	for name, data := range map[string][]byte{files.ca: ca.CertPEM, files.cert: leaf.ChainPEM, files.key: leaf.KeyPEM} {
		if err := l.writeFile(ctx, name, data); err != nil {
			return nil, nil, "", err
		}
	}
	return ca, files, files.ca, nil
}

// startProcess starts a process-flavor deployment.
func startProcess(l *launcher, k Kind, o Options) (*Deployment, error) {
	ctx, cancel := context.WithTimeout(l.tb.Context(), startTimeout)
	defer cancel()
	// The deployment's files live in the test's temporary directory.
	base := l.tb.TempDir()
	d := &Deployment{kind: k, acc: access{scheme: "redis"}}
	cfg := nodeConfig{users: o.Users, extra: o.Config}
	if o.TLS {
		ca, files, caFile, err := l.writeTLS(ctx, base)
		if err != nil {
			return nil, err
		}
		client, err := pki.ClientTLS(ca, nil)
		if err != nil {
			return nil, fmt.Errorf("redisserver: %w", err)
		}
		d.acc.scheme, d.acc.dial.tls, d.caFile, cfg.tls = "rediss", client, caFile, files
	}
	if len(o.Users) > 0 {
		u := o.Users[0]
		d.acc.user, d.acc.dial.user, d.acc.dial.password = &u, u.Name, u.Password
	}
	if k.Topology() == statestore.TopologySentinel {
		if err := l.startSentinel(ctx, base, d, cfg); err != nil {
			return nil, err
		}
		return d, nil
	}
	cluster := k.Topology() == statestore.TopologyCluster
	n := 1
	if cluster {
		n = clusterPrimaries
	}
	var nodes []clusterNode
	for i := range n {
		announce := func(string) string { return "" }
		if o.Announce != nil {
			announce = func(addr string) string { return o.Announce(i, addr) }
		}
		name := string(k)
		if cluster {
			name = fmt.Sprintf("%s-%d", k, i)
		}
		s, err := l.startNode(ctx, base, name, cfg, d.acc, cluster, announce)
		if err != nil {
			return nil, err
		}
		d.nodes = append(d.nodes, s)
		nodes = append(nodes, clusterNode{srv: s, host: l.ip(), port: s.node.port, busPort: s.node.busPort})
	}
	if cluster {
		if err := bootstrapCluster(ctx, nodes); err != nil {
			return nil, err
		}
	}
	if o.Replica {
		rc := cfg
		rc.primary = d.nodes[0].addr
		r, err := l.startNode(ctx, base, string(k)+"-replica", rc, d.acc, false, nil)
		if err != nil {
			return nil, err
		}
		if err := waitReplica(ctx, r); err != nil {
			return nil, err
		}
		d.replica = r
	}
	return d, nil
}

// startSentinel starts the sentinel kind's servers into d: a primary, its
// replica, synced, and a sentinel monitoring them as MasterSet, and waits
// until the sentinel lists the replica as healthy, so that Failover can
// promote it. The sentinel takes the deployment's TLS and users, but not
// Options.Config, which is the servers'.
func (l *launcher) startSentinel(ctx context.Context, base string, d *Deployment, cfg nodeConfig) error {
	name := string(d.kind)
	pc := cfg
	pc.linkAuth = true
	p, err := l.startNode(ctx, base, name+"-primary", pc, d.acc, false, nil)
	if err != nil {
		return err
	}
	rc := cfg
	rc.primary = p.addr
	r, err := l.startNode(ctx, base, name+"-replica", rc, d.acc, false, nil)
	if err != nil {
		return err
	}
	if err := waitReplica(ctx, r); err != nil {
		return err
	}
	sc := cfg
	sc.extra = nil
	sc.sentinel = &sentinelConfig{masterSet: MasterSet, primary: p.addr}
	s, err := l.startNode(ctx, base, name+"-sentinel", sc, d.acc, false, nil)
	if err != nil {
		return err
	}
	if err := waitSentinel(ctx, s, MasterSet, p, r); err != nil {
		return err
	}
	d.nodes, d.replica, d.sentinels, d.masterSet = []*Server{p}, r, []*Server{s}, MasterSet
	return nil
}

// startNode starts one redis-server on fresh ports, retrying when another
// process takes a port first. announce, when set, returns the address
// clients are given.
func (l *launcher) startNode(ctx context.Context, base, name string, cfg nodeConfig, acc access, cluster bool, announce func(addr string) string) (*Server, error) {
	var last error
	for range portAttempts {
		var err error
		if cfg.port, err = l.port(ctx, 0); err != nil {
			return nil, err
		}
		if cluster {
			if cfg.busPort, err = l.port(ctx, cfg.port); err != nil {
				return nil, err
			}
		}
		if cfg.dir, err = l.nodeDir(ctx, base, name); err != nil {
			return nil, err
		}
		addr := net.JoinHostPort(l.ip(), strconv.Itoa(cfg.port))
		s := &Server{name: name, addr: addr, client: addr, acc: acc, l: l, node: cfg}
		if announce != nil {
			if a := announce(addr); a != "" {
				s.client = a
				if cluster {
					s.node.announce = a
				}
			}
		}
		err = s.start(ctx)
		if err == nil {
			if l.held == nil {
				l.held = map[int]bool{}
			}
			l.held[cfg.port] = true
			if cluster {
				l.held[cfg.busPort] = true
			}
			l.tb.Cleanup(s.cleanup)
			return s, nil
		}
		if !errors.Is(err, errPortInUse) {
			return nil, err
		}
		last = err
	}
	return nil, fmt.Errorf("redisserver: %s: no free port in %d attempts: %w", name, portAttempts, last)
}

// serverProc is a running redis-server process.
type serverProc struct{ p *proc.Process }

// kill sends SIGKILL and waits until the process exited.
func (sp serverProc) kill(ctx context.Context) error {
	if err := sp.p.Kill(); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	select {
	case <-sp.p.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// output returns what the process wrote, as far as its rings keep it.
func (sp serverProc) output() string {
	out, errOut := sp.p.Output()
	return string(out) + string(errOut)
}

// end kills the process if it still runs and waits for it, with the
// test.
func (sp serverProc) end() {
	_ = sp.p.Kill()
	<-sp.p.Done()
}

// start writes the configuration, starts redis-server (with --sentinel
// for a sentinel, which rewrites its configuration file as it runs) and
// waits until it logs its readiness line and answers PING.
func (s *Server) start(ctx context.Context) error {
	cfg := s.node
	ds, err := cfg.directives()
	if err != nil {
		return err
	}
	conf := s.l.join(cfg.dir, "redis.conf")
	if err := s.l.writeFile(ctx, conf, render(ds, cfg.extra)); err != nil {
		return err
	}
	p, err := s.startLocal(ctx, cfg, conf)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
	if _, err := s.Do(ctx, "PING"); err != nil {
		out := p.output()
		p.end()
		return fmt.Errorf("redisserver: %s did not start: %w\n%s", s.name, err, tail(out))
	}
	return nil
}

// The lines a server and a sentinel write once they serve.
const (
	readyServer   = `Ready to accept connections`
	readySentinel = `Sentinel ID is`
)

// startLocal starts redis-server on this machine and waits for its
// readiness line.
func (s *Server) startLocal(ctx context.Context, cfg nodeConfig, conf string) (serverProc, error) {
	args := append(slices.Clone(s.l.args), conf)
	ready := regexp.MustCompile(readyServer)
	if cfg.sentinel != nil {
		args = append(args, "--sentinel")
		ready = regexp.MustCompile(readySentinel)
	}
	// The process outlives ctx, which bounds the start alone; cleanup
	// ends it with the test.
	p, err := proc.Start(context.WithoutCancel(ctx), proc.Spec{
		Name: s.name, Path: s.l.path, Args: args,
		Env: s.l.env, Dir: cfg.dir, RingBytes: outputBytes,
	})
	if err != nil {
		return serverProc{}, fmt.Errorf("redisserver: %w", err)
	}
	sp := serverProc{p}
	if _, err := p.WaitOutput(ctx, ready); err != nil {
		out := sp.output()
		sp.end()
		if strings.Contains(out, portInUse) {
			return serverProc{}, fmt.Errorf("%w: %s", errPortInUse, s.name)
		}
		return serverProc{}, fmt.Errorf("redisserver: %s did not start: %w\n%s", s.name, err, tail(out))
	}
	return sp, nil
}

// cleanup ends the process with the test and shows its output when the
// test failed.
func (s *Server) cleanup() {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	p.end()
	if s.l.tb.Failed() {
		s.l.tb.Logf("redisserver: %s output:\n%s", s.name, tail(p.output()))
	}
}

// tail returns the end of out.
func tail(out string) string {
	if len(out) > tailBytes {
		return "..." + out[len(out)-tailBytes:]
	}
	return out
}
