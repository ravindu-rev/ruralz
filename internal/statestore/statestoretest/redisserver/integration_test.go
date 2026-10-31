// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package redisserver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// Integration tests against real servers: redis-server for the process
// flavor, the images for the container flavor. Where a kind cannot run
// (no redis-server, no Docker, a cluster or sentinel container off
// Linux), its test
// fails when RURALZ_TEST_STATESTORE selects the kind's flavor and skips
// otherwise; a container kind also skips unless the variable is
// container.

// TestSelectedKinds starts every kind RURALZ_TEST_STATESTORE selects and
// checks that it serves at its URL: each primary answers, holds a key, a
// cluster serves every slot, and a sentinel names the primary.
func TestSelectedKinds(t *testing.T) {
	for _, k := range Kinds(t) {
		t.Run(string(k), func(t *testing.T) {
			d := Start(t, k, Options{})
			u, err := url.Parse(d.URL())
			if err != nil {
				t.Fatal(err)
			}
			nodes := d.Nodes()
			if d.Topology() == statestore.TopologySentinel {
				s := d.Sentinels()[0]
				if u.Scheme != "redis" || u.Host != s.ClientAddr() || u.Query().Get("master_set") != MasterSet || len(nodes) != 1 {
					t.Errorf("URL %q for %d nodes", d.URL(), len(nodes))
				}
				v := mustDo(t, s, "SENTINEL", "GET-MASTER-ADDR-BY-NAME", MasterSet)
				if len(v.Elems) != 2 || net.JoinHostPort(v.Elems[0].Str, v.Elems[1].Str) != nodes[0].Addr() {
					t.Errorf("the sentinel names %v, want %s", v, nodes[0].Addr())
				}
			} else if u.Scheme != "redis" || u.Host != nodes[0].ClientAddr() || len(u.Query()["addr"]) != len(nodes)-1 {
				t.Errorf("URL %q for %d nodes", d.URL(), len(nodes))
			}
			for _, n := range nodes {
				if v := mustDo(t, n, "PING"); v.Str != "PONG" {
					t.Fatalf("%s: PING = %v", n.Name(), v)
				}
			}
			if d.Topology() != statestore.TopologyCluster {
				mustDo(t, nodes[0], "SET", "ruralz:k", "v")
				if v := mustDo(t, nodes[0], "GET", "ruralz:k"); v.Str != "v" {
					t.Fatalf("GET = %v", v)
				}
				return
			}
			slotsServed := 0
			for _, r := range mustDo(t, nodes[0], "CLUSTER", "SLOTS").Elems {
				slotsServed += int(r.Elems[1].Int-r.Elems[0].Int) + 1
				// Each shard lists its primary, then its one replica.
				if len(r.Elems) != 4 {
					t.Errorf("CLUSTER SLOTS shard %v lists %d servers, want a primary and one replica", r.Elems[:2], len(r.Elems)-2)
				}
			}
			if slotsServed != slots {
				t.Errorf("CLUSTER SLOTS serves %d slots, want %d", slotsServed, slots)
			}
			for i, r := range d.Replicas() {
				if info := mustInfo(t, r, "replication"); info["role"] != "slave" || info["master_link_status"] != "up" {
					t.Errorf("replica %d: role %q, link %q", i, info["role"], info["master_link_status"])
				}
			}
		})
	}
}

// keyOnShard returns a key whose slot lies in the slot range of shard
// i of a three-primary cluster, asking s with CLUSTER KEYSLOT.
func keyOnShard(t *testing.T, s *Server, i int) string {
	t.Helper()
	r := slotRanges(3)[i]
	for n := 0; ; n++ {
		k := "ruralz:k" + strconv.Itoa(n)
		slot := int(mustDo(t, s, "CLUSTER", "KEYSLOT", k).Int)
		if r[0] <= slot && slot <= r[1] {
			return k
		}
	}
}

// TestProcessClusterFailoverLive checks the cluster failover on real
// servers with TLS and users: a key written on shard 1 and replicated
// (WAIT) survives a SIGKILL of the shard's primary and the promotion of
// its replica with CLUSTER FAILOVER TAKEOVER, which takes writes and
// which the other primaries map the shard to; the killed primary,
// restarted, rejoins as the replica of the promoted one.
func TestProcessClusterFailoverLive(t *testing.T) {
	d := Start(t, ProcessCluster, Options{TLS: true, Users: []User{{Name: "ruralz", Password: "s3cret pw"}}})
	primary, replica := d.Nodes()[1], d.Replicas()[1]
	k := keyOnShard(t, primary, 1)
	replicate(t, primary, k, "v")
	if err := primary.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.ClusterFailover(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	if d.Nodes()[1] != replica || d.Replicas()[1] != primary {
		t.Fatalf("after ClusterFailover: primary %s, replica %s", d.Nodes()[1].Name(), d.Replicas()[1].Name())
	}
	if v := mustDo(t, replica, "GET", k); v.Str != "v" {
		t.Errorf("promoted replica GET %s = %v, want the replicated value", k, v)
	}
	mustDo(t, replica, "SET", k, "v2")
	r := slotRanges(3)[1]
	if owner, err := slotOwner(t.Context(), d.Nodes()[0], r[0]); err != nil || owner != replica.ClientAddr() {
		t.Errorf("shard 0 maps slot %d to %s (%v), want the promoted %s", r[0], owner, err, replica.ClientAddr())
	}
	if err := primary.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(t.Context(), "the former primary to rejoin as a replica", func(ctx context.Context) error {
		info, err := primary.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if info["role"] != "slave" || info["master_link_status"] != "up" {
			return fmt.Errorf("role %q, link %q", info["role"], info["master_link_status"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProcessSentinelAutoFailoverLive checks the failover a sentinel
// starts on its own: after a SIGKILL of the primary, the sentinel
// promotes the replica once the primary is down past its
// down-after-milliseconds, AwaitFailover follows it, and the restarted
// former primary becomes the new primary's replica.
func TestProcessSentinelAutoFailoverLive(t *testing.T) {
	d := Start(t, ProcessSentinel, Options{Users: []User{{Name: "ruralz", Password: "pw"}}})
	primary, replica := d.Primary(), d.Replica()
	replicate(t, primary, "k", "v")
	if err := primary.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.AwaitFailover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Primary() != replica {
		t.Fatalf("after AwaitFailover the primary is %s", d.Primary().Name())
	}
	if v := mustDo(t, replica, "GET", "k"); v.Str != "v" {
		t.Errorf("promoted GET = %v", v)
	}
	if err := primary.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := waitUntil(t.Context(), "the former primary to follow", func(ctx context.Context) error {
		info, err := primary.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if info["role"] != "slave" || info["master_link_status"] != "up" {
			return fmt.Errorf("role %q, link %q", info["role"], info["master_link_status"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProcessTLSAndUsersLive checks TLS and ACL users on a real
// redis-server: the first user authenticates over TLS, a client without
// credentials is refused, and the URL carries rediss:// and the user.
func TestProcessTLSAndUsersLive(t *testing.T) {
	d := Start(t, Process, Options{TLS: true, Users: []User{{Name: "ruralz", Password: "s3cret pw"}}})
	if v := mustDo(t, d.Primary(), "ACL", "WHOAMI"); v.Str != "ruralz" {
		t.Fatalf("ACL WHOAMI = %v", v)
	}
	c, err := dialConn(t.Context(), d.Primary().Addr(), dialOptions{tls: d.ClientTLS()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var re *ReplyError
	if _, err := c.Do(t.Context(), "GET", "x"); !errors.As(err, &re) || re.Code() != "NOAUTH" {
		t.Errorf("GET without AUTH = %v, want NOAUTH", err)
	}
	if u, _ := url.Parse(d.URL()); u.Scheme != "rediss" || u.User.Username() != "ruralz" {
		t.Errorf("URL %q", d.URL())
	}
}

// TestProcessTLSLinksLive checks that TLS reaches every link of a real
// deployment: a replica syncing over TLS with the first user's
// credentials, and a cluster whose bus, which requires client
// certificates, runs over TLS.
func TestProcessTLSLinksLive(t *testing.T) {
	d := Start(t, Process, Options{TLS: true, Replica: true, Users: []User{{Name: "ruralz", Password: "pw"}}})
	replicate(t, d.Primary(), "k", "v")
	if v := mustDo(t, d.Replica(), "GET", "k"); v.Str != "v" {
		t.Fatalf("replica GET over TLS = %v", v)
	}
	c := Start(t, ProcessCluster, Options{TLS: true})
	for _, n := range c.Nodes() {
		if info := parseInfo(mustDo(t, n, "CLUSTER", "INFO").Str); info["cluster_state"] != "ok" {
			t.Errorf("%s: cluster_state %q over a TLS bus", n.Name(), info["cluster_state"])
		}
	}
	if u, _ := url.Parse(c.URL()); u.Scheme != "rediss" || len(u.Query()["addr"]) != 2 {
		t.Errorf("TLS cluster URL %q", c.URL())
	}
}

// replicate sets key to value on the primary s and waits until a replica
// has acknowledged the write. WAIT counts the replicas that acknowledged
// its own connection's writes, so SET and WAIT share one connection: on a
// fresh connection, which has written nothing, WAIT returns at once.
func replicate(t *testing.T, s *Server, key, value string) {
	t.Helper()
	c, err := s.Dial(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Do(t.Context(), "SET", key, value); err != nil {
		t.Fatalf("%s SET %s: %v", s.Name(), key, err)
	}
	v, err := c.Do(t.Context(), "WAIT", "1", "5000")
	if err != nil {
		t.Fatalf("%s WAIT: %v", s.Name(), err)
	}
	if v.Int < 1 {
		t.Fatalf("%s WAIT 1 5000 = %d: no replica acknowledged the SET of %s", s.Name(), v.Int, key)
	}
}

// TestProcessFailoverLive checks the scripted failover on a real
// redis-server: data written to the primary reaches the replica, the
// primary is killed, the replica promoted takes writes, and a restart of
// the old primary comes back empty on its port.
func TestProcessFailoverLive(t *testing.T) {
	d := Start(t, Process, Options{Replica: true})
	primary, replica := d.Primary(), d.Replica()
	replicate(t, primary, "k", "v")
	if v := mustDo(t, replica, "GET", "k"); v.Str != "v" {
		t.Fatalf("replica GET = %v", v)
	}
	if err := primary.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := d.Promote(t.Context()); err != nil {
		t.Fatal(err)
	}
	mustDo(t, d.Primary(), "SET", "k2", "v2")
	if info := mustInfo(t, d.Primary(), "replication"); info["role"] != "master" {
		t.Errorf("promoted role %q", info["role"])
	}
	if err := primary.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if v := mustDo(t, primary, "GET", "k"); !v.Null {
		t.Errorf("restarted primary GET = %v, want nil", v)
	}
}

// TestProcessSentinelFailoverLive checks the sentinel kind on real
// servers, with TLS and users: data written to the primary reaches the
// replica, SENTINEL FAILOVER promotes the replica, which takes writes,
// and the sentinel turns the former primary into its replica.
func TestProcessSentinelFailoverLive(t *testing.T) {
	d := Start(t, ProcessSentinel, Options{TLS: true, Users: []User{{Name: "ruralz", Password: "s3cret pw"}}})
	primary, replica := d.Primary(), d.Replica()
	replicate(t, primary, "k", "v")
	if v := mustDo(t, replica, "GET", "k"); v.Str != "v" {
		t.Fatalf("replica GET = %v", v)
	}
	if v := mustDo(t, d.Sentinels()[0], "ACL", "WHOAMI"); v.Str != "ruralz" {
		t.Errorf("sentinel ACL WHOAMI = %v", v)
	}
	if err := d.Failover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Primary() != replica || d.Replica() != primary {
		t.Fatalf("after Failover: primary %s", d.Primary().Name())
	}
	mustDo(t, replica, "SET", "k2", "v2")
	if err := waitUntil(t.Context(), "the former primary to follow", func(ctx context.Context) error {
		info, err := primary.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if info["role"] != "slave" || info["master_link_status"] != "up" {
			return fmt.Errorf("role %q, link %q", info["role"], info["master_link_status"])
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// TestProcessClusterBehindProxies checks Options.Announce on a real
// cluster: each primary announces a counting RESP proxy, so CLUSTER
// SLOTS names the proxies and commands sent through them are counted.
// The replicas replicate through the announced proxies too, so the test
// counts a command no replication link sends.
func TestProcessClusterBehindProxies(t *testing.T) {
	proxies := map[int]*respproxy.Proxy{} // the last start attempt of each node wins
	d := Start(t, ProcessCluster, Options{Announce: func(node int, addr string) string {
		p, err := respproxy.Start(t.Context(), respproxy.Config{Fault: faultproxy.Config{Upstream: addr}})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		proxies[node] = p
		return p.Addr()
	}})
	nodes := d.Nodes()
	var announced []string
	for _, r := range mustDo(t, nodes[0], "CLUSTER", "SLOTS").Elems {
		announced = append(announced, net.JoinHostPort(r.Elems[2].Elems[0].Str, strconv.FormatInt(r.Elems[2].Elems[1].Int, 10)))
	}
	for _, n := range nodes {
		if !slices.Contains(announced, n.ClientAddr()) {
			t.Errorf("CLUSTER SLOTS names %q, want %s's proxy %s", announced, n.Name(), n.ClientAddr())
		}
	}
	through := &Server{name: "via-proxy", addr: nodes[0].ClientAddr(), client: nodes[0].ClientAddr(), acc: access{scheme: "redis"}}
	mustDo(t, through, "ECHO", "ruralz-probe")
	if s := proxies[0].Snapshot(); s.Count("ECHO") != 1 {
		t.Errorf("proxy of node 0 counted %v", s.ByName)
	}
}

func mustInfo(t *testing.T, s *Server, section string) map[string]string {
	t.Helper()
	info, err := s.Info(t.Context(), section)
	if err != nil {
		t.Fatal(err)
	}
	return info
}
