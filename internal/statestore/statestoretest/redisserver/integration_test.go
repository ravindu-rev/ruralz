// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package redisserver

import (
	"errors"
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
// (no redis-server, no Docker, a cluster container off Linux), its test
// fails when RURALZ_TEST_STATESTORE selects the kind's flavor and skips
// otherwise; a container kind also skips unless the variable is
// container.

// TestSelectedKinds starts every kind RURALZ_TEST_STATESTORE selects and
// checks that it serves at its URL: each primary answers, holds a key,
// and a cluster serves every slot.
func TestSelectedKinds(t *testing.T) {
	for _, k := range Kinds(t) {
		t.Run(string(k), func(t *testing.T) {
			d := Start(t, k, Options{})
			u, err := url.Parse(d.URL())
			if err != nil {
				t.Fatal(err)
			}
			nodes := d.Nodes()
			if u.Scheme != "redis" || u.Host != nodes[0].ClientAddr() || len(u.Query()["addr"]) != len(nodes)-1 {
				t.Errorf("URL %q for %d nodes", d.URL(), len(nodes))
			}
			for _, n := range nodes {
				if v := mustDo(t, n, "PING"); v.Str != "PONG" {
					t.Fatalf("%s: PING = %v", n.Name(), v)
				}
			}
			if d.Topology() == statestore.TopologyStandalone {
				mustDo(t, nodes[0], "SET", "ruralz:k", "v")
				if v := mustDo(t, nodes[0], "GET", "ruralz:k"); v.Str != "v" {
					t.Fatalf("GET = %v", v)
				}
				return
			}
			slotsServed := 0
			for _, r := range mustDo(t, nodes[0], "CLUSTER", "SLOTS").Elems {
				slotsServed += int(r.Elems[1].Int-r.Elems[0].Int) + 1
			}
			if slotsServed != slots {
				t.Errorf("CLUSTER SLOTS serves %d slots, want %d", slotsServed, slots)
			}
		})
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
	mustDo(t, d.Primary(), "SET", "k", "v")
	mustDo(t, d.Primary(), "WAIT", "1", "5000")
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

// TestProcessFailoverLive checks the scripted failover on a real
// redis-server: data written to the primary reaches the replica, the
// primary is killed, the replica promoted takes writes, and a restart of
// the old primary comes back empty on its port.
func TestProcessFailoverLive(t *testing.T) {
	d := Start(t, Process, Options{Replica: true})
	primary, replica := d.Primary(), d.Replica()
	mustDo(t, primary, "SET", "k", "v")
	mustDo(t, primary, "WAIT", "1", "5000")
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

// TestProcessClusterBehindProxies checks Options.Announce on a real
// cluster: each primary announces a counting RESP proxy, so CLUSTER
// SLOTS names the proxies and commands sent through them are counted.
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
	mustDo(t, through, "PING")
	if s := proxies[0].Snapshot(); s.Count("PING") != 1 {
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
