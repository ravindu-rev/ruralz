// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package raft

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
	"github.com/ravindu-rev/ruralz/internal/controlstore"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

// testStart is the start of the store clocks of these tests; certificates
// are valid around it.
var testStart = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

// waitBound bounds every real-time wait: raft elects and replicates on
// the real clock, so the tests poll conditions under this bound.
const waitBound = 30 * time.Second

// testPKI is the disjoint peer CA and Node CA of TB-11.
type testPKI struct {
	peer, node *pki.CA
}

// validity is the certificate lifetime of these tests: around both the
// store clock and the real clock.
func validity() (time.Time, time.Time) {
	return time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2100, 1, 1, 0, 0, 0, 0, time.UTC)
}

// newPKI returns the two CAs.
func newPKI(t *testing.T) *testPKI {
	t.Helper()
	nb, na := validity()
	peer, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Test Peer CA", NotBefore: nb, NotAfter: na})
	if err != nil {
		t.Fatal(err)
	}
	node, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Test Node CA", NotBefore: nb, NotAfter: na})
	if err != nil {
		t.Fatal(err)
	}
	return &testPKI{peer: peer, node: node}
}

// leaf issues a certificate from ca for 127.0.0.1 with the given URI SANs.
func leaf(t *testing.T, ca *pki.CA, uris ...string) tls.Certificate {
	t.Helper()
	nb, na := validity()
	l, err := ca.Issue(pki.LeafOptions{CommonName: "peer", IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, URIs: uris, NotBefore: nb, NotAfter: na})
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.TLSCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// replicaCert issues a replica peer certificate.
func (p *testPKI) replicaCert(t *testing.T, id controlstore.ServerID) tls.Certificate {
	t.Helper()
	u, err := controlstore.PeerURI(controlstore.RoleReplica, id)
	if err != nil {
		t.Fatal(err)
	}
	return leaf(t, p.peer, u.String())
}

// source returns the TLSSource of a replica.
func (p *testPKI) source(t *testing.T, id controlstore.ServerID) StaticTLS {
	t.Helper()
	return StaticTLS{Cert: p.replicaCert(t, id), Pool: p.peer.Pool()}
}

// listen binds a loopback listener, on port when non-zero.
func listen(t *testing.T, port string) net.Listener {
	t.Helper()
	if port == "" {
		port = "0"
	}
	var ln net.Listener
	var err error
	deadline := time.Now().Add(waitBound)
	for {
		ln, err = (&net.ListenConfig{}).Listen(t.Context(), "tcp", net.JoinHostPort("127.0.0.1", port))
		if err == nil || port == "0" || time.Now().After(deadline) {
			break
		}
		// A restarted replica rebinds its port once the old listener's
		// close completes.
		pollPause()
	}
	if err != nil {
		t.Fatal(err)
	}
	return ln
}

// pollPause is the pause between two polls of a condition.
func pollPause() {
	t := time.NewTimer(5 * time.Millisecond)
	defer t.Stop()
	<-t.C
}

// waitFor polls cond until it holds or waitBound passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitBound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		pollPause()
	}
}

// replica is one test replica.
type replica struct {
	t     *testing.T
	id    controlstore.ServerID
	dir   string
	port  string
	addr  string
	cfg   Config
	store *Store
}

// cluster is a set of replicas sharing a PKI and a store clock.
type cluster struct {
	t        *testing.T
	pki      *testPKI
	clk      clock.Clock
	fake     *clocktest.Fake
	binary   string
	mu       sync.Mutex
	replicas map[controlstore.ServerID]*replica
	tweak    func(*Config)
}

// newCluster returns an empty cluster on a fake store clock.
func newCluster(t *testing.T) *cluster {
	t.Helper()
	fake := clocktest.New(testStart)
	return &cluster{t: t, pki: newPKI(t), clk: fake, fake: fake, binary: "0.2.0", replicas: map[controlstore.ServerID]*replica{}}
}

// config returns a replica's configuration with fast raft timing.
func (c *cluster) config(r *replica, bootstrap bool, peers []string) Config {
	cfg := Config{
		DataDir: r.dir, ServerID: r.id, Address: r.addr, TLS: c.pki.source(c.t, r.id), Bootstrap: bootstrap, Peers: peers,
		BinaryVersion: c.binary, Clock: c.clk, Logger: slog.New(slog.DiscardHandler),
		HeartbeatTimeout: 100 * time.Millisecond, ElectionTimeout: 100 * time.Millisecond,
		LeaderLeaseTimeout: 100 * time.Millisecond, CommitTimeout: 5 * time.Millisecond,
	}
	if c.tweak != nil {
		c.tweak(&cfg)
	}
	return cfg
}

// bootstrap opens r1, the replica that bootstraps the cluster, on a fresh
// port.
func (c *cluster) bootstrap() *replica {
	c.t.Helper()
	return c.startOn("r1", listen(c.t, ""), true)
}

// startOn opens a new replica on a bound listener.
func (c *cluster) startOn(id controlstore.ServerID, ln net.Listener, bootstrap bool, peers ...string) *replica {
	c.t.Helper()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	r := &replica{t: c.t, id: id, dir: filepath.Join(c.t.TempDir(), string(id)), port: port, addr: ln.Addr().String()}
	r.cfg = c.config(r, bootstrap, peers)
	r.cfg.Listener = ln
	s, err := Open(r.cfg)
	if err != nil {
		c.t.Fatalf("Open %s: %v", id, err)
	}
	r.store = s
	c.mu.Lock()
	c.replicas[id] = r
	c.mu.Unlock()
	c.t.Cleanup(func() { r.stop() })
	return r
}

// join adds a replica the way ruralz control join does: the leader
// records it as a pending member, it starts unbootstrapped and reports to
// the leader, which adds it as a non-voter on that first connection and
// promotes it once it reached the commit index. Each poll moves the store
// clock one report interval, so the replicas report.
func (c *cluster) join(id controlstore.ServerID) *replica {
	c.t.Helper()
	l := c.leader()
	ln := listen(c.t, "")
	c.propose(controlstore.Proposal{Ops: []controlstore.Op{controlstore.AddPending{Member: controlstore.PendingMember{ServerID: id, Address: ln.Addr().String()}}}})
	r := c.startOn(id, ln, false, l.addr)
	c.untilVoter(id)
	return r
}

// untilVoter advances the report clock until id is a committed voter.
func (c *cluster) untilVoter(id controlstore.ServerID) {
	c.t.Helper()
	waitFor(c.t, "server "+string(id)+" to become a voter", func() bool {
		c.fake.Advance(ReportInterval)
		voter := false
		l := c.leader()
		_ = l.store.View(func(v controlstore.View) {
			for _, m := range v.Members() {
				if m.ServerID == id && m.Voter {
					voter = true
				}
			}
		})
		return voter
	})
}

// three returns a cluster of three voters r1, r2 and r3.
func three(t *testing.T) (*cluster, []*replica) {
	t.Helper()
	c := newCluster(t)
	r1 := c.bootstrap()
	c.leader()
	r2 := c.join("r2")
	r3 := c.join("r3")
	return c, []*replica{r1, r2, r3}
}

// others returns the replicas of rs other than x.
func others(rs []*replica, x *replica) []*replica {
	var out []*replica
	for _, r := range rs {
		if r != x {
			out = append(out, r)
		}
	}
	return out
}

// stop shuts the replica down.
func (r *replica) stop() {
	if r.store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitBound)
	defer cancel()
	_ = r.store.Shutdown(ctx)
	r.store = nil
}

// restart reopens the replica on its directory and port.
func (r *replica) restart() error {
	r.t.Helper()
	r.stop()
	cfg := r.cfg
	cfg.Listener = listen(r.t, r.port)
	s, err := Open(cfg)
	if err != nil {
		return err
	}
	r.store = s
	return nil
}

// leader waits for a replica whose leadership passed its Barrier.
func (c *cluster) leader() *replica {
	c.t.Helper()
	var found *replica
	waitFor(c.t, "a leader", func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, r := range c.replicas {
			if r.store != nil && r.store.leadership() != nil {
				found = r
				return true
			}
		}
		return false
	})
	return found
}

// propose proposes on the current leader, retrying while leadership moves.
func (c *cluster) propose(p controlstore.Proposal) controlstore.Result {
	c.t.Helper()
	var res controlstore.Result
	var err error
	waitFor(c.t, "a committed proposal", func() bool {
		l := c.leader()
		ctx, cancel := context.WithTimeout(context.Background(), waitBound)
		defer cancel()
		res, err = l.store.Propose(ctx, p)
		return err == nil || !errors.Is(err, controlstore.ErrNoLeader)
	})
	if err != nil {
		c.t.Fatalf("Propose: %v", err)
	}
	return res
}

// applied waits until every running replica applied index.
func (c *cluster) applied(index uint64, rs ...*replica) {
	c.t.Helper()
	for _, r := range rs {
		waitFor(c.t, "replica "+string(r.id)+" to apply", func() bool { return r.store.machine.Index() >= index })
	}
}

// ctx returns a bounded context.
func boundedCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitBound)
	t.Cleanup(cancel)
	return ctx
}
