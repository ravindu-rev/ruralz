// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path"
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

// launcher starts the redis-server processes of one test: on this
// machine, or on a Host.
type launcher struct {
	tb   testing.TB
	path string   // the redis-server binary of this machine
	args []string // arguments ahead of the configuration file
	env  []string // the complete environment of the processes
	// host, when set, runs the servers instead of this machine (StartOn).
	host Host
	// ports returns a free port of the servers' address.
	ports func(context.Context) (int, error)
	// held are the ports of the servers this launcher started.
	held map[int]bool
}

// ip returns the address the servers bind and announce, and the test and
// the clients dial: loopback, or the Host's.
func (l *launcher) ip() string {
	if l.host != nil {
		return l.host.Addr()
	}
	return loopback
}

// join joins the elements of a server file's path: slash-separated on a
// Host, whose servers run on a Linux lab host, and in this machine's form
// otherwise.
func (l *launcher) join(elem ...string) string {
	if l.host != nil {
		return path.Join(elem...)
	}
	return filepath.Join(elem...)
}

// baseDir returns the deployment's directory: the test's temporary
// directory, or a fresh directory on the Host, which the end of the test
// removes after the servers stopped.
func (l *launcher) baseDir(ctx context.Context) (string, error) {
	if l.host == nil {
		return l.tb.TempDir(), nil
	}
	dir, err := l.host.MkdirTemp(ctx, "", "redisserver-")
	if err != nil {
		return "", fmt.Errorf("redisserver: %w", err)
	}
	// Registered before any server's cleanup, so it runs after them all.
	l.tb.Cleanup(func() {
		if err := l.host.RemoveAll(context.WithoutCancel(ctx), dir); err != nil {
			l.tb.Errorf("redisserver: remove %s: %v", dir, err)
		}
	})
	return dir, nil
}

// nodeDir creates a fresh directory for the server name under base.
func (l *launcher) nodeDir(ctx context.Context, base, name string) (string, error) {
	if l.host == nil {
		dir, err := os.MkdirTemp(base, name+"-")
		if err != nil {
			return "", fmt.Errorf("redisserver: %w", err)
		}
		return dir, nil
	}
	dir, err := l.host.MkdirTemp(ctx, base, name+"-")
	if err != nil {
		return "", fmt.Errorf("redisserver: %w", err)
	}
	return dir, nil
}

// writeFile writes a file of the deployment, mode 0600.
func (l *launcher) writeFile(ctx context.Context, name string, data []byte) error {
	if l.host == nil {
		if err := os.WriteFile(name, data, 0o600); err != nil {
			return fmt.Errorf("redisserver: %w", err)
		}
		return nil
	}
	if err := l.host.WriteFile(ctx, name, data); err != nil {
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

// freePort returns a loopback port that was free a moment ago.
func freePort(ctx context.Context) (int, error) {
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", loopback+":0")
	if err != nil {
		return 0, fmt.Errorf("redisserver: free port: %w", err)
	}
	defer func() { _ = ln.Close() }()
	a, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("redisserver: free port: address %v", ln.Addr())
	}
	return a.Port, nil
}

// writeTLS writes a fresh CA and a certificate for localhost, 127.0.0.1,
// ::1 and, on a Host, its address and names into base. The certificate
// serves both ends: a replica and a cluster node present it as a client
// too, and the cluster bus requires client certificates. caFile is the
// CA on this machine, for Deployment.CAFile: base's own on this machine,
// a copy in the test's temporary directory for a Host.
func (l *launcher) writeTLS(ctx context.Context, base string) (ca *pki.CA, files *tlsFiles, caFile string, err error) {
	ca, err = pki.NewCA(pki.CAOptions{CommonName: "Ruralz Test State Store CA"})
	if err != nil {
		return nil, nil, "", fmt.Errorf("redisserver: %w", err)
	}
	names := []string{"localhost"}
	ips := []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	if l.host != nil {
		// startOn checked that the address parses.
		ips = append(ips, net.ParseIP(l.host.Addr()))
		names = append(names, l.host.Names()...)
	}
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
	caFile = files.ca
	if l.host != nil {
		caFile = filepath.Join(l.tb.TempDir(), "ca.crt")
		if err := ca.WriteFile(caFile); err != nil {
			return nil, nil, "", fmt.Errorf("redisserver: %w", err)
		}
	}
	return ca, files, caFile, nil
}

// startProcess starts a process-flavor deployment.
func startProcess(l *launcher, k Kind, o Options) (*Deployment, error) {
	ctx, cancel := context.WithTimeout(l.tb.Context(), startTimeout)
	defer cancel()
	base, err := l.baseDir(ctx)
	if err != nil {
		return nil, err
	}
	d := &Deployment{kind: k, acc: access{scheme: "redis"}}
	cfg := nodeConfig{bind: l.ip(), users: o.Users, extra: o.Config}
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
		// Every cluster node holds the replica link credentials: a
		// replica needs them, and a primary once a failover demotes it.
		cfg.linkAuth = true
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
		// One replica per primary, announcing its own address.
		var replicas []clusterNode
		for i := range n {
			s, err := l.startNode(ctx, base, fmt.Sprintf("%s-%d-replica", k, i), cfg, d.acc, true, nil)
			if err != nil {
				return nil, err
			}
			d.shardReplicas = append(d.shardReplicas, s)
			replicas = append(replicas, clusterNode{srv: s, host: l.ip(), port: s.node.port, busPort: s.node.busPort})
		}
		if err := attachReplicas(ctx, nodes, replicas); err != nil {
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
		cfg.base = base
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

// serverProc is a running redis-server: a process of this machine, or
// one a Host started.
type serverProc interface {
	// kill sends SIGKILL and waits until the process exited.
	kill(ctx context.Context) error
	// signal sends sig.
	signal(sig os.Signal) error
	// output returns what the process wrote, as far as it is kept.
	output() string
	// end kills the process if it still runs and waits for it, with the
	// test.
	end()
}

// localProc is a redis-server process of this machine.
type localProc struct{ p *proc.Process }

func (lp localProc) kill(ctx context.Context) error {
	if err := lp.p.Kill(); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	select {
	case <-lp.p.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (lp localProc) signal(sig os.Signal) error {
	if err := lp.p.Signal(sig); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	return nil
}

func (lp localProc) output() string {
	out, errOut := lp.p.Output()
	return string(out) + string(errOut)
}

func (lp localProc) end() {
	_ = lp.p.Kill()
	<-lp.p.Done()
}

// hostProc is a redis-server a Host started.
type hostProc struct{ p HostProcess }

func (hp hostProc) kill(ctx context.Context) error {
	if err := hp.p.Kill(ctx); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	return nil
}

func (hp hostProc) signal(sig os.Signal) error {
	if err := hp.p.Signal(sig); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	return nil
}

func (hp hostProc) output() string { return hp.p.Output() }

func (hp hostProc) end() { _ = hp.p.Close() }

// start writes the configuration, starts redis-server (with --sentinel
// for a sentinel, which rewrites its configuration file as it runs) and
// waits until it logs its readiness line and answers PING, on this
// machine or a Host.
func (s *Server) start(ctx context.Context) error {
	s.mu.Lock()
	cfg := s.node
	if cfg.sentinel != nil {
		sc := *cfg.sentinel
		cfg.sentinel = &sc
	}
	old := s.proc
	s.mu.Unlock()
	if old != nil {
		// The process a Restart killed: ending it also releases what holds
		// it, such as a Host's session.
		old.end()
	}
	ds, err := cfg.directives()
	if err != nil {
		return err
	}
	conf := s.l.join(cfg.dir, "redis.conf")
	if err := s.l.writeFile(ctx, conf, render(ds, cfg.extra)); err != nil {
		return err
	}
	var p serverProc
	if s.l.host != nil {
		hs := HostServer{Name: s.name, Dir: cfg.base, Port: cfg.port, Args: []string{conf}, Ready: readyServer}
		if cfg.sentinel != nil {
			hs.Args, hs.Ready = append(hs.Args, "--sentinel"), readySentinel
		}
		hp, err := s.l.host.Start(ctx, hs)
		if err != nil {
			if strings.Contains(err.Error(), portInUse) {
				return fmt.Errorf("%w: %s", errPortInUse, s.name)
			}
			return fmt.Errorf("redisserver: %s did not start: %w", s.name, err)
		}
		p = hostProc{hp}
	} else {
		lp, err := s.startLocal(ctx, cfg, conf)
		if err != nil {
			return err
		}
		p = lp
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
func (s *Server) startLocal(ctx context.Context, cfg nodeConfig, conf string) (localProc, error) {
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
		return localProc{}, fmt.Errorf("redisserver: %w", err)
	}
	lp := localProc{p}
	if _, err := p.WaitOutput(ctx, ready); err != nil {
		out := lp.output()
		lp.end()
		if strings.Contains(out, portInUse) {
			return localProc{}, fmt.Errorf("%w: %s", errPortInUse, s.name)
		}
		return localProc{}, fmt.Errorf("redisserver: %s did not start: %w\n%s", s.name, err, tail(out))
	}
	return lp, nil
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
