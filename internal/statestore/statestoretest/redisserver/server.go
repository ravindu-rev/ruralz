// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/testcontainers/testcontainers-go"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// Start starts a deployment of kind k for tb; it stops when tb ends.
// When k's flavor cannot run here (no redis-server; no Docker daemon, or
// a cluster or sentinel container on a host other than Linux), Start
// fails tb if EnvStateStore selects that flavor and skips tb with the
// reason if not; a process kind runs wherever a redis-server is found,
// selected or not, and a container kind only when selected. Start also
// fails tb on an invalid EnvRedisServer or EnvStateStore, invalid Options
// or a failed start.
func Start(tb testing.TB, k Kind, o Options) *Deployment {
	tb.Helper()
	d, skip, err := start(tb, realHost(), k, o)
	switch {
	case err != nil:
		tb.Fatalf("redisserver: start %s: %v", k, err)
	case skip != "":
		tb.Skipf("redisserver: %s skipped: %s", k, skip)
	}
	return d
}

// start starts k, or returns why it skips.
func start(tb testing.TB, h host, k Kind, o Options) (*Deployment, string, error) {
	if k.Flavor() == FlavorNone {
		return nil, "", fmt.Errorf("redisserver: unknown kind %q", string(k))
	}
	if err := o.validate(k); err != nil {
		return nil, "", err
	}
	path, skip, err := h.gate(tb.Context(), k)
	if err != nil || skip != "" {
		return nil, skip, err
	}
	var d *Deployment
	if k.Flavor() == FlavorProcess {
		d, err = startProcess(&launcher{tb: tb, path: path, env: childEnv(), ports: freePort}, k, o)
	} else {
		d, err = startContainer(tb, k)
	}
	return d, "", err
}

// access is how clients reach the servers of a deployment.
type access struct {
	scheme string // "redis", or "rediss" with TLS
	user   *User  // the URL's credentials; nil without users
	dial   dialOptions
}

// buildURL returns the State Store URL of addrs in the rueidis form the
// configuration model names: one endpoint for a standalone deployment,
// the first seed then "addr" parameters for a cluster, and for a sentinel
// deployment its sentinels the same way, then master_set.
func buildURL(a access, addrs []string, masterSet string) string {
	u := url.URL{Scheme: a.scheme, Host: addrs[0]}
	if a.user != nil {
		u.User = url.UserPassword(a.user.Name, a.user.Password)
	}
	var q []string
	for _, addr := range addrs[1:] {
		q = append(q, "addr="+addr)
	}
	if masterSet != "" {
		q = append(q, "master_set="+url.QueryEscape(masterSet))
	}
	u.RawQuery = strings.Join(q, "&")
	return u.String()
}

// clusterPrimaries is the number of primaries of the cluster kinds, each
// with one replica.
const clusterPrimaries = 3

// errNotSentinel is returned by Failover outside the sentinel kinds,
// errSentinelPromote by Promote within them, errClusterPromote by Promote
// on a cluster, and errNotCluster by ClusterFailover outside the cluster
// kinds.
var (
	errNotSentinel     = errors.New("redisserver: Failover needs a sentinel kind")
	errSentinelPromote = errors.New("redisserver: a sentinel deployment fails over with Failover, not Promote")
	errClusterPromote  = errors.New("redisserver: a cluster fails over with ClusterFailover, not Promote")
	errNotCluster      = errors.New("redisserver: ClusterFailover needs a cluster kind")
)

// Deployment is a running State Store: one primary (and an optional
// replica), the three primaries of a cluster with one replica each, or a
// primary, its replica and a sentinel monitoring them. Its methods are
// safe for concurrent use.
type Deployment struct {
	kind   Kind
	acc    access
	caFile string

	mu        sync.Mutex
	nodes     []*Server // standalone, sentinel: the primary; cluster: the primaries in slot order
	replica   *Server
	sentinels []*Server
	masterSet string // the sentinel kinds' MasterSet
	// shardReplicas holds a cluster's replicas, shardReplicas[i] that of
	// nodes[i].
	shardReplicas []*Server
}

// Kind returns the deployment's kind.
func (d *Deployment) Kind() Kind { return d.kind }

// Topology returns standalone, cluster or sentinel, for
// stateStore.topology.
func (d *Deployment) Topology() statestore.Topology { return d.kind.Topology() }

// URL returns the stateStore.url of the deployment, with the first
// user's credentials: redis://host:port, rediss:// with TLS, for a
// cluster the seeds as in rediss://host:port?addr=host:port&addr=host:port,
// and for a sentinel deployment its sentinel and the master set, as in
// rediss://host:port?master_set=ruralz. It names the addresses
// Options.Announce returned, if any.
func (d *Deployment) URL() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	servers := d.nodes
	if len(d.sentinels) > 0 {
		servers = d.sentinels
	}
	addrs := make([]string, len(servers))
	for i, n := range servers {
		addrs[i] = n.client
	}
	return buildURL(d.acc, addrs, d.masterSet)
}

// Sentinels returns the sentinels of a sentinel kind; none otherwise.
func (d *Deployment) Sentinels() []*Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Server(nil), d.sentinels...)
}

// MasterSet returns the master set a sentinel kind's sentinels monitor
// (MasterSet); "" otherwise.
func (d *Deployment) MasterSet() string { return d.masterSet }

// Failover makes the sentinel fail the master set over to the replica
// (SENTINEL FAILOVER) and waits until the sentinel names the replica and
// the replica reports role master. Primary then returns it, and Replica
// the former primary, which the sentinel turns into its replica. Only the
// sentinel kinds fail over this way.
func (d *Deployment) Failover(ctx context.Context) error {
	return d.sentinelFailover(ctx, true)
}

// AwaitFailover waits for the failover the sentinel starts on its own
// once the primary stopped answering for sentinelDownAfter, as after a
// SIGKILL of the primary (Server.Kill), until the sentinel names the
// replica and the replica reports role master; Primary and Replica then
// swap as after Failover, and a restart of the former primary makes it
// the new primary's replica. Only the sentinel kinds fail over this way.
func (d *Deployment) AwaitFailover(ctx context.Context) error {
	return d.sentinelFailover(ctx, false)
}

// sentinelFailover runs SENTINEL FAILOVER when force is set, then waits
// for the switch and records the new roles.
func (d *Deployment) sentinelFailover(ctx context.Context, force bool) error {
	d.mu.Lock()
	if len(d.sentinels) == 0 || d.replica == nil {
		d.mu.Unlock()
		return errNotSentinel
	}
	s, primary, replica, set := d.sentinels[0], d.nodes[0], d.replica, d.masterSet
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if force {
		if _, err := s.Do(ctx, "SENTINEL", "FAILOVER", set); err != nil {
			return err
		}
	}
	if err := waitUntil(ctx, s.name+" to promote "+replica.name, func(ctx context.Context) error {
		got, err := masterAddr(ctx, s, set)
		if err != nil {
			return err
		}
		if got != replica.addr {
			return fmt.Errorf("the sentinel names %s", got)
		}
		info, err := replica.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if info["role"] != "master" {
			return fmt.Errorf("%s has role %q", replica.name, info["role"])
		}
		return nil
	}); err != nil {
		return err
	}
	// A restart keeps each server's new role and the sentinel's monitor.
	replica.mu.Lock()
	replica.node.primary, replica.node.linkAuth = "", true
	replica.mu.Unlock()
	primary.mu.Lock()
	primary.node.primary = replica.addr
	primary.mu.Unlock()
	s.mu.Lock()
	if s.node.sentinel != nil {
		s.node.sentinel.primary = replica.addr
	}
	s.mu.Unlock()
	d.mu.Lock()
	d.nodes[0], d.replica = replica, primary
	d.mu.Unlock()
	return nil
}

// Primary returns the standalone primary, or a cluster's first primary.
func (d *Deployment) Primary() *Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.nodes[0]
}

// Nodes returns the primaries: one, or a cluster's three in slot order.
func (d *Deployment) Nodes() []*Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Server(nil), d.nodes...)
}

// Replica returns the replica of Options.Replica or of a sentinel kind;
// nil without one, after Promote, and for a cluster, whose replicas
// Replicas returns.
func (d *Deployment) Replica() *Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.replica
}

// Replicas returns every replica: a cluster's, Replicas()[i] being the
// replica of Nodes()[i], else the one Replica returns, if any.
func (d *Deployment) Replicas() []*Server {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.shardReplicas) > 0 {
		return append([]*Server(nil), d.shardReplicas...)
	}
	if d.replica != nil {
		return []*Server{d.replica}
	}
	return nil
}

// ClusterFailover promotes the replica of a cluster's shard (an index of
// Nodes, in slot order) with CLUSTER FAILOVER TAKEOVER, which needs no
// agreement from the primary or the other primaries: the cluster
// failover of the cluster kinds, run after a SIGKILL of the shard's
// primary (Server.Kill). It waits until the replica reports role master
// and every other primary maps the shard's slots to it. Nodes()[shard]
// then returns the promoted replica, and Replicas()[shard] the former
// primary, which rejoins as its replica when restarted (Server.Restart),
// as its cluster configuration file keeps its node identity. A container
// cluster is promoted the same way, its primary left running.
func (d *Deployment) ClusterFailover(ctx context.Context, shard int) error {
	d.mu.Lock()
	if len(d.shardReplicas) == 0 || shard < 0 || shard >= len(d.nodes) {
		d.mu.Unlock()
		if len(d.shardReplicas) == 0 {
			return errNotCluster
		}
		return fmt.Errorf("redisserver: no shard %d of %d", shard, len(d.nodes))
	}
	primary, replica := d.nodes[shard], d.shardReplicas[shard]
	var others []*Server
	for i, n := range d.nodes {
		if i != shard {
			others = append(others, n)
		}
	}
	d.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, startTimeout)
	defer cancel()
	if _, err := replica.Do(ctx, "CLUSTER", "FAILOVER", "TAKEOVER"); err != nil {
		return err
	}
	if err := waitPromoted(ctx, replica, slotRanges(clusterPrimaries)[shard][0], others); err != nil {
		return err
	}
	d.mu.Lock()
	d.nodes[shard], d.shardReplicas[shard] = replica, primary
	d.mu.Unlock()
	return nil
}

// Promote makes the replica the primary (REPLICAOF NO ONE), the scripted
// failover of the process flavor: Primary returns it from then on, also
// across its restarts, and Replica returns nil. The former primary keeps
// running, unchanged, unless the test killed it; a proxy standing for
// the primary's endpoint is switched by the test (SetUpstream). A
// sentinel deployment fails over with Failover, a cluster with
// ClusterFailover.
func (d *Deployment) Promote(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.sentinels) > 0 {
		return errSentinelPromote
	}
	if len(d.shardReplicas) > 0 {
		return errClusterPromote
	}
	r := d.replica
	if r == nil {
		return errNoReplica
	}
	if _, err := r.Do(ctx, "REPLICAOF", "NO", "ONE"); err != nil {
		return err
	}
	r.mu.Lock()
	r.node.primary = ""
	r.mu.Unlock()
	d.nodes[0], d.replica = r, nil
	return nil
}

// CAFile returns the PEM file of the CA that issued the servers'
// certificates under Options.TLS ("" without TLS): a bundle for
// SSL_CERT_FILE (testkit/pki.Env), so a child process trusts the servers.
func (d *Deployment) CAFile() string { return d.caFile }

// ClientTLS returns a client configuration trusting the servers under
// Options.TLS; nil without TLS.
func (d *Deployment) ClientTLS() *tls.Config {
	if d.acc.dial.tls == nil {
		return nil
	}
	return d.acc.dial.tls.Clone()
}

// Server is one server of a deployment: a redis-server process or a
// container. Its methods are safe for concurrent use.
type Server struct {
	name   string
	addr   string // what the test process dials
	client string // what clients are given (Options.Announce), else addr
	acc    access

	// The process flavor, on this machine or a Host.
	l    *launcher
	mu   sync.Mutex // guards node and proc
	node nodeConfig
	proc serverProc

	// The container flavor.
	ctr testcontainers.Container
}

// Name returns the server's name, such as "redis-server-cluster-1".
func (s *Server) Name() string { return s.name }

// Addr returns the address the test process dials ("host:port").
func (s *Server) Addr() string { return s.addr }

// ClientAddr returns the address clients are given: the one
// Options.Announce returned, else Addr.
func (s *Server) ClientAddr() string { return s.client }

// URL returns a standalone State Store URL of this server alone, with
// the deployment's credentials and scheme.
func (s *Server) URL() string { return buildURL(s.acc, []string{s.client}, "") }

// Dial opens an admin connection to the server, over TLS and
// authenticated as the deployment requires.
func (s *Server) Dial(ctx context.Context) (*Conn, error) { return dialConn(ctx, s.addr, s.acc.dial) }

// Do runs one command on a fresh admin connection.
func (s *Server) Do(ctx context.Context, args ...string) (Value, error) {
	c, err := s.Dial(ctx)
	if err != nil {
		return Value{}, err
	}
	defer func() { _ = c.Close() }()
	return c.Do(ctx, args...)
}

// Info runs INFO section and returns its fields.
func (s *Server) Info(ctx context.Context, section string) (map[string]string, error) {
	v, err := s.Do(ctx, "INFO", section)
	if err != nil {
		return nil, err
	}
	return parseInfo(v.Str), nil
}

// Kill kills a redis-server process (SIGKILL on Linux) and waits for it
// to exit; the process flavor's "primary killed" fault, on this machine
// or a Host. Killing an exited process is not an error. Containers return
// errors.ErrUnsupported.
func (s *Server) Kill(ctx context.Context) error {
	if s.l == nil {
		return fmt.Errorf("redisserver: kill %s: %w", s.name, errors.ErrUnsupported)
	}
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if err := p.kill(ctx); err != nil {
		return fmt.Errorf("redisserver: kill %s: %w", s.name, err)
	}
	return nil
}

// Signal sends sig to a redis-server process, on this machine or a Host,
// such as SIGSTOP and SIGCONT for the frozen State Store fault, whose
// sockets stay open; signaling an exited process fails. Containers return
// errors.ErrUnsupported.
func (s *Server) Signal(sig os.Signal) error {
	if s.l == nil {
		return fmt.Errorf("redisserver: signal %s: %w", s.name, errors.ErrUnsupported)
	}
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	return p.signal(sig)
}

// Restart kills a running redis-server process and starts it again on
// the same port, directory and configuration; data held only in memory
// is gone, as after a real restart. It fails when the port was taken
// meanwhile. Containers return errors.ErrUnsupported.
func (s *Server) Restart(ctx context.Context) error {
	if s.l == nil {
		return fmt.Errorf("redisserver: restart %s: %w", s.name, errors.ErrUnsupported)
	}
	if err := s.Kill(ctx); err != nil {
		return err
	}
	return s.start(ctx)
}

// Output returns what the current redis-server process wrote, as far as
// its bounded rings, or a Host, keep it; "" for a container.
func (s *Server) Output() string {
	s.mu.Lock()
	p := s.proc
	s.mu.Unlock()
	if p == nil {
		return ""
	}
	return p.output()
}
