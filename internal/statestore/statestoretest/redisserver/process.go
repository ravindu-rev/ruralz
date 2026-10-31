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

// launcher starts the redis-server processes of one test.
type launcher struct {
	tb   testing.TB
	path string   // the redis-server binary
	args []string // arguments ahead of the configuration file
	env  []string // the complete environment of the processes
	// ports returns a free loopback port.
	ports func(context.Context) (int, error)
	// held are the ports of the servers this launcher started.
	held map[int]bool
}

// port returns a free loopback port that no server of this launcher holds
// and that is not avoid. ports can return a port it returned before,
// because a port is free again once its probe closes.
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
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
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

// writeTLS writes a fresh CA and a certificate for localhost, 127.0.0.1
// and ::1 into dir. The certificate serves both ends: a replica and a
// cluster node present it as a client too, and the cluster bus requires
// client certificates.
func writeTLS(dir string) (*pki.CA, *tlsFiles, error) {
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Test State Store CA"})
	if err != nil {
		return nil, nil, fmt.Errorf("redisserver: %w", err)
	}
	leaf, err := ca.Issue(pki.LeafOptions{
		CommonName:  "localhost",
		DNSNames:    []string{"localhost"},
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("redisserver: %w", err)
	}
	cert, key, err := leaf.WriteFiles(dir, "server")
	if err != nil {
		return nil, nil, fmt.Errorf("redisserver: %w", err)
	}
	caFile := filepath.Join(dir, "ca.crt")
	if err := ca.WriteFile(caFile); err != nil {
		return nil, nil, fmt.Errorf("redisserver: %w", err)
	}
	return ca, &tlsFiles{ca: caFile, cert: cert, key: key}, nil
}

// startProcess starts a process-flavor deployment.
func startProcess(l *launcher, k Kind, o Options) (*Deployment, error) {
	ctx, cancel := context.WithTimeout(l.tb.Context(), startTimeout)
	defer cancel()
	base := l.tb.TempDir()
	d := &Deployment{kind: k, acc: access{scheme: "redis"}}
	cfg := nodeConfig{users: o.Users, extra: o.Config}
	if o.TLS {
		ca, files, err := writeTLS(base)
		if err != nil {
			return nil, err
		}
		client, err := pki.ClientTLS(ca, nil)
		if err != nil {
			return nil, fmt.Errorf("redisserver: %w", err)
		}
		d.acc.scheme, d.acc.dial.tls, d.caFile, cfg.tls = "rediss", client, files.ca, files
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
		nodes = append(nodes, clusterNode{srv: s, host: "127.0.0.1", port: s.node.port, busPort: s.node.busPort})
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
			replicas = append(replicas, clusterNode{srv: s, host: "127.0.0.1", port: s.node.port, busPort: s.node.busPort})
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
		if cfg.dir, err = os.MkdirTemp(base, name+"-"); err != nil {
			return nil, fmt.Errorf("redisserver: %w", err)
		}
		addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.port))
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

// start writes the configuration, starts redis-server (with --sentinel
// for a sentinel, which rewrites its configuration file as it runs) and
// waits until it is ready and answers PING.
func (s *Server) start(ctx context.Context) error {
	s.mu.Lock()
	cfg := s.node
	if cfg.sentinel != nil {
		sc := *cfg.sentinel
		cfg.sentinel = &sc
	}
	s.mu.Unlock()
	ds, err := cfg.directives()
	if err != nil {
		return err
	}
	conf := filepath.Join(cfg.dir, "redis.conf")
	if err := os.WriteFile(conf, render(ds, cfg.extra), 0o600); err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	args := append(slices.Clone(s.l.args), conf)
	ready := regexp.MustCompile(`Ready to accept connections`)
	if cfg.sentinel != nil {
		args = append(args, "--sentinel")
		ready = regexp.MustCompile(`Sentinel ID is`)
	}
	// The process outlives ctx, which bounds the start alone; cleanup
	// ends it with the test.
	p, err := proc.Start(context.WithoutCancel(ctx), proc.Spec{
		Name: s.name, Path: s.l.path, Args: args,
		Env: s.l.env, Dir: cfg.dir, RingBytes: outputBytes,
	})
	if err != nil {
		return fmt.Errorf("redisserver: %w", err)
	}
	s.mu.Lock()
	s.proc = p
	s.mu.Unlock()
	_, err = p.WaitOutput(ctx, ready)
	if err == nil {
		_, err = s.Do(ctx, "PING")
	}
	if err == nil {
		return nil
	}
	_ = p.Kill()
	<-p.Done()
	out := s.Output()
	if strings.Contains(out, "Address already in use") {
		return fmt.Errorf("%w: %s", errPortInUse, s.name)
	}
	return fmt.Errorf("redisserver: %s did not start: %w\n%s", s.name, err, tail(out))
}

// cleanup ends the process with the test and shows its output when the
// test failed.
func (s *Server) cleanup() {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	_ = p.Kill()
	<-p.Done()
	if s.l.tb.Failed() {
		s.l.tb.Logf("redisserver: %s output:\n%s", s.name, tail(s.Output()))
	}
}

// tail returns the end of out.
func tail(out string) string {
	if len(out) > tailBytes {
		return "..." + out[len(out)-tailBytes:]
	}
	return out
}
