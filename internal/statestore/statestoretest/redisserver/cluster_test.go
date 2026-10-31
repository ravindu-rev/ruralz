// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// TestSlotRanges checks the split of the 16384 hash slots: redis-cli's
// cut points for three primaries, and contiguous, covering, non-empty
// ranges for any count.
func TestSlotRanges(t *testing.T) {
	if got, want := slotRanges(3), [][2]int{{0, 5460}, {5461, 10922}, {10923, 16383}}; !slices.Equal(got, want) {
		t.Errorf("slotRanges(3) = %v, want %v", got, want)
	}
	for n := 1; n <= 7; n++ {
		r := slotRanges(n)
		next := 0
		for _, rg := range r {
			if rg[0] != next || rg[1] < rg[0] {
				t.Fatalf("slotRanges(%d) = %v: gap, overlap or empty range", n, r)
			}
			next = rg[1] + 1
		}
		if next != slots {
			t.Errorf("slotRanges(%d) = %v, want every slot", n, r)
		}
	}
}

// clusterFake is a primary for bootstrap tests: it records the CLUSTER
// commands and reports the cluster whole after okAfter CLUSTER INFO polls.
type clusterFake struct {
	mu      sync.Mutex
	polls   int
	okAfter int
	failOn  string // a CLUSTER subcommand answered with an error
}

func (c *clusterFake) handle(_ *session, args []string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if args[0] != "CLUSTER" {
		return "-ERR unexpected\r\n"
	}
	switch args[1] {
	case c.failOn:
		return "-ERR refused\r\n"
	case "INFO":
		c.polls++
		if c.okAfter > 0 && c.polls >= c.okAfter {
			return bulk("cluster_state:ok\r\ncluster_slots_ok:16384\r\ncluster_known_nodes:3\r\n")
		}
		return bulk("cluster_state:fail\r\ncluster_slots_ok:5461\r\ncluster_known_nodes:1\r\n")
	}
	return "+OK\r\n"
}

func startClusterFakes(t *testing.T, fakes []*clusterFake) ([]*fakeServer, []clusterNode) {
	t.Helper()
	var servers []*fakeServer
	var nodes []clusterNode
	for i, cf := range fakes {
		f := startFake(t, nil, cf.handle)
		_, port, _ := net.SplitHostPort(f.addr())
		p, _ := strconv.Atoi(port)
		servers = append(servers, f)
		nodes = append(nodes, clusterNode{srv: f.server("node-"+strconv.Itoa(i), access{scheme: "redis"}), host: "10.0.0." + strconv.Itoa(i+1), port: p, busPort: 17000 + i})
	}
	return servers, nodes
}

// TestBootstrapCluster checks the formation of a cluster: distinct config
// epochs and one slot range per primary, a MEET from the first primary
// to each other one at its bus port, then polls until every primary
// reports the cluster whole.
func TestBootstrapCluster(t *testing.T) {
	fakes := []*clusterFake{{okAfter: 3}, {okAfter: 1}, {okAfter: 2}}
	servers, nodes := startClusterFakes(t, fakes)
	if err := bootstrapCluster(t.Context(), nodes); err != nil {
		t.Fatal(err)
	}
	ranges := slotRanges(3)
	for i, f := range servers {
		got := f.commands()
		var setup [][]string
		for _, c := range got {
			if c[1] != "INFO" {
				setup = append(setup, c)
			}
		}
		want := [][]string{
			{"CLUSTER", "SET-CONFIG-EPOCH", strconv.Itoa(i + 1)},
			{"CLUSTER", "ADDSLOTSRANGE", strconv.Itoa(ranges[i][0]), strconv.Itoa(ranges[i][1])},
		}
		if i == 0 {
			for _, n := range nodes[1:] {
				want = append(want, []string{"CLUSTER", "MEET", n.host, strconv.Itoa(n.port), strconv.Itoa(n.busPort)})
			}
		}
		if !slices.EqualFunc(setup, want, slices.Equal) {
			t.Errorf("node %d got %q, want %q", i, setup, want)
		}
	}
	fakes[0].mu.Lock()
	defer fakes[0].mu.Unlock()
	if fakes[0].polls < 3 {
		t.Errorf("node 0 polled %d times, want until it reported ok", fakes[0].polls)
	}
}

// TestBootstrapClusterRefused checks that a refused setup command stops
// the bootstrap with the server's error, before any MEET.
func TestBootstrapClusterRefused(t *testing.T) {
	servers, nodes := startClusterFakes(t, []*clusterFake{{okAfter: 1}, {okAfter: 1, failOn: "ADDSLOTSRANGE"}, {okAfter: 1}})
	if err := bootstrapCluster(t.Context(), nodes); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Errorf("bootstrap with a refused ADDSLOTSRANGE = %v", err)
	}
	for _, c := range servers[0].commands() {
		if c[1] == "MEET" {
			t.Errorf("MEET %q sent after a refused setup", c)
		}
	}
}

// TestWaitUntilReportsLastState checks that a wait ended by its context
// names what it awaited and the last answer it got.
func TestWaitUntilReportsLastState(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	calls := 0
	err := waitUntil(ctx, "the cluster to form", func(context.Context) error {
		calls++
		cancel()
		return errors.New(`cluster_state "fail"`)
	})
	if err == nil || !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), `the cluster to form`) || !strings.Contains(err.Error(), `cluster_state "fail"`) || calls != 1 {
		t.Errorf("waitUntil = %v after %d calls", err, calls)
	}
}

// TestWaitReplica checks that the replica wait ends once the link to the
// primary is up.
func TestWaitReplica(t *testing.T) {
	var mu sync.Mutex
	polls := 0
	f := startFake(t, nil, func(*session, []string) string {
		mu.Lock()
		defer mu.Unlock()
		if polls++; polls < 3 {
			return bulk("# Replication\r\nrole:slave\r\nmaster_link_status:down\r\n")
		}
		return bulk("# Replication\r\nrole:slave\r\nmaster_link_status:up\r\n")
	})
	if err := waitReplica(t.Context(), f.server("replica", access{scheme: "redis"})); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if polls != 3 {
		t.Errorf("polled %d times, want 3", polls)
	}
}
