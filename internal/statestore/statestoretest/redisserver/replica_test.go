// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// replicaFake is a fresh cluster node for attach tests: it answers MEET,
// refuses REPLICATE with "Unknown node" until it has been asked
// learnAfter times, then reports a synced replica.
type replicaFake struct {
	mu         sync.Mutex
	asks       int
	learnAfter int
	replicaOf  string
}

func (r *replicaFake) handle(_ *session, args []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case args[0] == "INFO":
		if r.replicaOf == "" {
			return bulk("# Replication\r\nrole:master\r\n")
		}
		return bulk("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n")
	case args[0] != "CLUSTER":
		return "-ERR unexpected\r\n"
	case args[1] == "REPLICATE":
		if r.asks++; r.asks <= r.learnAfter {
			return "-ERR Unknown node " + args[2] + "\r\n"
		}
		r.replicaOf = args[2]
		return "+OK\r\n"
	case args[1] == "INFO":
		return bulk("cluster_state:ok\r\ncluster_slots_ok:16384\r\ncluster_known_nodes:6\r\n")
	}
	return "+OK\r\n"
}

// slotsReply is the CLUSTER SLOTS reply of a three-shard cluster whose
// shards list replicas replicas each.
func slotsReply(replicas int) string {
	out := "*3\r\n"
	for _, rg := range slotRanges(3) {
		out += fmt.Sprintf("*%d\r\n:%d\r\n:%d\r\n", 3+replicas, rg[0], rg[1])
		for range 1 + replicas {
			out += "*2\r\n" + bulk("127.0.0.1") + ":0\r\n"
		}
	}
	return out
}

// TestAttachReplicas checks how a replica joins a formed cluster: a MEET
// to the first primary at its bus port, then CLUSTER REPLICATE with its
// own primary's node ID, retried while the replica does not know that
// node yet, then the sync and whole-cluster waits, and a wait until every
// primary's slot map lists one replica per shard.
func TestAttachReplicas(t *testing.T) {
	var primaries []clusterNode
	var mu sync.Mutex
	slotPolls := make([]int, 3) // CLUSTER SLOTS polls per primary
	for i := range 3 {
		id := strings.Repeat(strconv.Itoa(i+1), 40)
		f := startFake(t, nil, func(_ *session, args []string) string {
			switch {
			case args[0] == "CLUSTER" && args[1] == "MYID":
				return bulk(id)
			case args[0] == "CLUSTER" && args[1] == "INFO":
				return bulk("cluster_state:ok\r\ncluster_slots_ok:16384\r\ncluster_known_nodes:6\r\n")
			case args[0] == "CLUSTER" && args[1] == "SLOTS":
				mu.Lock()
				defer mu.Unlock()
				// The replicas' roles reach this primary after two polls.
				if slotPolls[i]++; slotPolls[i] <= 2 {
					return slotsReply(0)
				}
				return slotsReply(1)
			}
			return "-ERR unexpected\r\n"
		})
		primaries = append(primaries, clusterNode{srv: f.server("primary-"+strconv.Itoa(i), access{scheme: "redis"}), host: "10.0.0.1", port: 7000 + i, busPort: 17000 + i})
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		for i, n := range slotPolls {
			if n < 3 {
				t.Errorf("primary %d: CLUSTER SLOTS polled %d times, want until it listed the replicas", i, n)
			}
		}
	}()
	fakes := []*replicaFake{{learnAfter: 2}, {}, {learnAfter: 1}}
	var replicaServers []*fakeServer
	var replicas []clusterNode
	for i, rf := range fakes {
		f := startFake(t, nil, rf.handle)
		replicaServers = append(replicaServers, f)
		replicas = append(replicas, clusterNode{srv: f.server("replica-"+strconv.Itoa(i), access{scheme: "redis"}), host: "10.0.0.2", port: 7100 + i, busPort: 17100 + i})
	}
	if err := attachReplicas(t.Context(), primaries, replicas); err != nil {
		t.Fatal(err)
	}
	for i, f := range replicaServers {
		cmds := f.commands()
		if len(cmds) == 0 || !slices.Equal(cmds[0], []string{"CLUSTER", "MEET", "10.0.0.1", "7000", "17000"}) {
			t.Errorf("replica %d: first command %q, want a MEET to the first primary's bus port", i, cmds)
		}
		want := strings.Repeat(strconv.Itoa(i+1), 40)
		fakes[i].mu.Lock()
		got, asks := fakes[i].replicaOf, fakes[i].asks
		fakes[i].mu.Unlock()
		if got != want || asks != fakes[i].learnAfter+1 {
			t.Errorf("replica %d replicates %q after %d asks, want %q after %d", i, got, asks, want, fakes[i].learnAfter+1)
		}
	}
}

// TestAttachReplicasRefused checks that a replica that never learns its
// primary ends the attach with the last refusal once the context ends.
func TestAttachReplicasRefused(t *testing.T) {
	p := startFake(t, nil, func(*session, []string) string { return bulk(strings.Repeat("a", 40)) })
	r := startFake(t, nil, (&replicaFake{learnAfter: 1 << 30}).handle)
	ctx, cancel := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancel()
	err := attachReplicas(ctx,
		[]clusterNode{{srv: p.server("primary", access{scheme: "redis"}), host: "10.0.0.1", port: 7000, busPort: 17000}},
		[]clusterNode{{srv: r.server("replica", access{scheme: "redis"}), host: "10.0.0.2", port: 7001, busPort: 17001}})
	if err == nil || !strings.Contains(err.Error(), "Unknown node") {
		t.Fatalf("attachReplicas = %v, want the refusal", err)
	}
}

// failoverCluster is a cluster deployment of in-process fakes for
// ClusterFailover tests: the replica of shard 1 takes over on CLUSTER
// FAILOVER TAKEOVER, and the other primaries map the shard to it after a
// few polls of CLUSTER SLOTS.
type failoverCluster struct {
	mu       sync.Mutex
	promoted bool
	polls    int
	took     [][]string
}

func (c *failoverCluster) deployment(t *testing.T) *Deployment {
	t.Helper()
	ranges := slotRanges(3)
	d := &Deployment{kind: ProcessCluster, acc: access{scheme: "redis"}}
	replica := startFake(t, nil, func(_ *session, args []string) string {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch {
		case args[0] == "CLUSTER" && args[1] == "FAILOVER":
			c.took = append(c.took, args)
			c.promoted = true
			return "+OK\r\n"
		case args[0] == "INFO" && c.promoted:
			return bulk("# Replication\r\nrole:master\r\n")
		case args[0] == "INFO":
			return bulk("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n")
		}
		return "-ERR unexpected\r\n"
	})
	replicaAddr := replica.addr()
	for i := range 3 {
		var self string
		f := startFake(t, nil, func(_ *session, args []string) string {
			c.mu.Lock()
			defer c.mu.Unlock()
			if args[0] != "CLUSTER" || args[1] != "SLOTS" {
				return "-ERR unexpected\r\n"
			}
			c.polls++
			out := "*3\r\n"
			for j, r := range ranges {
				owner := self
				if j == 1 && c.promoted && c.polls > 4 {
					owner = replicaAddr
				}
				host, port, _ := net.SplitHostPort(owner)
				out += fmt.Sprintf("*3\r\n:%d\r\n:%d\r\n*2\r\n%s:%s\r\n", r[0], r[1], bulk(host), port)
			}
			return out
		})
		c.mu.Lock()
		self = f.addr()
		c.mu.Unlock()
		d.nodes = append(d.nodes, f.server("primary-"+strconv.Itoa(i), d.acc))
	}
	for i := range 3 {
		s := &Server{name: "replica-" + strconv.Itoa(i), addr: "127.0.0.1:1", client: "127.0.0.1:1", acc: d.acc}
		if i == 1 {
			s = replica.server("replica-1", d.acc)
		}
		d.shardReplicas = append(d.shardReplicas, s)
	}
	return d
}

// TestClusterFailover checks the cluster failover: CLUSTER FAILOVER
// TAKEOVER on the shard's replica, a wait until it reports role master
// and every other primary maps the shard's first slot to it, then Nodes
// and Replicas swapped for that shard alone.
func TestClusterFailover(t *testing.T) {
	c := &failoverCluster{}
	d := c.deployment(t)
	before, replicasBefore := d.Nodes(), d.Replicas()
	if err := d.ClusterFailover(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	took, polls := c.took, c.polls
	c.mu.Unlock()
	if !slices.EqualFunc(took, [][]string{{"CLUSTER", "FAILOVER", "TAKEOVER"}}, slices.Equal) {
		t.Errorf("the replica got %q, want one CLUSTER FAILOVER TAKEOVER", took)
	}
	if polls < 5 {
		t.Errorf("CLUSTER SLOTS polled %d times, want until the other primaries mapped the shard to the replica", polls)
	}
	after, replicasAfter := d.Nodes(), d.Replicas()
	if after[1] != replicasBefore[1] || replicasAfter[1] != before[1] {
		t.Errorf("shard 1: primary %s, replica %s; want them swapped", after[1].Name(), replicasAfter[1].Name())
	}
	if after[0] != before[0] || after[2] != before[2] || replicasAfter[0] != replicasBefore[0] {
		t.Error("ClusterFailover changed another shard")
	}
	if err := d.Promote(t.Context()); !errors.Is(err, errClusterPromote) {
		t.Errorf("Promote on a cluster = %v, want errClusterPromote", err)
	}
	if err := d.ClusterFailover(t.Context(), 3); err == nil {
		t.Error("ClusterFailover of shard 3 of 3 succeeded")
	}
}

// TestClusterFailoverNeedsCluster checks that ClusterFailover refuses a
// deployment without shard replicas, whose Replicas is the standalone
// replica.
func TestClusterFailoverNeedsCluster(t *testing.T) {
	d := startHelper(t, Process, Options{Replica: true})
	if err := d.ClusterFailover(t.Context(), 0); !errors.Is(err, errNotCluster) {
		t.Fatalf("ClusterFailover = %v, want errNotCluster", err)
	}
	if got := d.Replicas(); len(got) != 1 || got[0] != d.Replica() {
		t.Errorf("Replicas = %v, want the standalone replica", got)
	}
}

// TestProcessClusterReplicas checks the process cluster's replicas: one
// per primary, cluster-enabled on its own port and bus port, holding the
// replica link credentials as every cluster node does, synced as a
// replica, and announcing its own address, so the URL keeps naming the
// three primaries as seeds.
func TestProcessClusterReplicas(t *testing.T) {
	d := startHelper(t, ProcessCluster, Options{Users: []User{{Name: "ruralz", Password: "pw"}}})
	primaries, replicas := d.Nodes(), d.Replicas()
	if len(primaries) != 3 || len(replicas) != 3 || d.Replica() != nil {
		t.Fatalf("%d primaries, %d replicas, Replica %v; want 3 each and no standalone replica", len(primaries), len(replicas), d.Replica())
	}
	ports := map[string]bool{}
	for i, r := range append(slices.Clone(primaries), replicas...) {
		for _, name := range []string{"port", "cluster-port"} {
			p := configArg(t, r, name)
			if ports[p] {
				t.Errorf("server %d: %s %s reused", i, name, p)
			}
			ports[p] = true
		}
		if got := configArg(t, r, "masteruser"); got != "ruralz" {
			t.Errorf("%s: masteruser %q, want the first user", r.Name(), got)
		}
		if got := configArg(t, r, "cluster-enabled"); got != "yes" {
			t.Errorf("%s: cluster-enabled %q", r.Name(), got)
		}
	}
	for i, r := range replicas {
		if role := roleOf(t, r); role != "slave" {
			t.Errorf("replica %d: role %q", i, role)
		}
		if r.ClientAddr() != r.Addr() {
			t.Errorf("replica %d announces %s, want its own address %s", i, r.ClientAddr(), r.Addr())
		}
		if strings.Contains(d.URL(), r.Addr()) {
			t.Errorf("URL %q names replica %d", d.URL(), i)
		}
	}
	if err := (Options{Replica: true}).validate(ProcessCluster); err == nil {
		t.Error("Options.Replica on a cluster kind passed validation")
	}
}

// TestSentinelAwaitFailover checks AwaitFailover, which follows the
// failover a sentinel starts on its own: it sends the sentinel only
// reads, polling until the sentinel names the replica and the replica
// reports role master, then swaps Primary and Replica and records the
// roles a restart keeps.
func TestSentinelAwaitFailover(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	replica := startFake(t, nil, func(*session, []string) string {
		mu.Lock()
		defer mu.Unlock()
		if polls > 3 {
			return bulk("# Replication\r\nrole:master\r\n")
		}
		return bulk("# Replication\r\nrole:slave\r\n")
	})
	primary := startFake(t, nil, func(*session, []string) string { return "+OK\r\n" })
	replicaAddr, primaryAddr := replica.addr(), primary.addr()
	var sentinelCmds [][]string
	sentinel := startFake(t, nil, func(_ *session, args []string) string {
		mu.Lock()
		defer mu.Unlock()
		sentinelCmds = append(sentinelCmds, args)
		polls++
		addr := primaryAddr
		if polls > 2 {
			addr = replicaAddr
		}
		host, port, _ := net.SplitHostPort(addr)
		return "*2\r\n" + bulk(host) + bulk(port)
	})
	acc := access{scheme: "redis"}
	p, r, s := primary.server("primary", acc), replica.server("replica", acc), sentinel.server("sentinel", acc)
	s.node.sentinel = &sentinelConfig{masterSet: MasterSet, primary: primaryAddr}
	d := &Deployment{kind: ProcessSentinel, acc: acc, nodes: []*Server{p}, replica: r, sentinels: []*Server{s}, masterSet: MasterSet}
	if err := d.AwaitFailover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Primary() != r || d.Replica() != p {
		t.Fatalf("after AwaitFailover: primary %s, replica %s; want them swapped", d.Primary().Name(), d.Replica().Name())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, c := range sentinelCmds {
		if c[0] != "SENTINEL" || c[1] != "GET-MASTER-ADDR-BY-NAME" {
			t.Errorf("AwaitFailover sent the sentinel %q, want only reads", c)
		}
	}
	if p.node.primary != replicaAddr || s.node.sentinel.primary != replicaAddr {
		t.Errorf("a restart would make %s follow %q and the sentinel monitor %q, want %s", p.Name(), p.node.primary, s.node.sentinel.primary, replicaAddr)
	}
}
