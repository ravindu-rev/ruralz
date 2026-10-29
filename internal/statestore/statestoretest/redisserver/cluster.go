// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"
)

// slots is the number of Redis Cluster hash slots.
const slots = 16384

// pollInterval spaces the polls of a server's state.
const pollInterval = 20 * time.Millisecond

// slotRanges splits the hash slots among n primaries into contiguous
// ranges, cut where redis-cli --cluster create cuts them: for three,
// 0-5460, 5461-10922 and 10923-16383.
func slotRanges(n int) [][2]int {
	per := float64(slots) / float64(n)
	out := make([][2]int, n)
	first, cursor := 0, 0.0
	for i := range n {
		last := int(math.Round(cursor + per - 1))
		if last > slots-1 || i == n-1 {
			last = slots - 1
		}
		last = max(last, first)
		out[i] = [2]int{first, last}
		first = last + 1
		cursor += per
	}
	return out
}

// clusterNode is a primary to join into a cluster, with the address the
// other primaries reach it at.
type clusterNode struct {
	srv           *Server
	host          string
	port, busPort int
}

// bootstrapCluster forms a cluster of fresh primaries: distinct config
// epochs, a slot range each, a MEET from the first to the others, then a
// wait until every primary sees the cluster whole.
func bootstrapCluster(ctx context.Context, nodes []clusterNode) error {
	ranges := slotRanges(len(nodes))
	for i, n := range nodes {
		if _, err := n.srv.Do(ctx, "CLUSTER", "SET-CONFIG-EPOCH", strconv.Itoa(i+1)); err != nil {
			return err
		}
		if _, err := n.srv.Do(ctx, "CLUSTER", "ADDSLOTSRANGE", strconv.Itoa(ranges[i][0]), strconv.Itoa(ranges[i][1])); err != nil {
			return err
		}
	}
	for _, n := range nodes[1:] {
		if _, err := nodes[0].srv.Do(ctx, "CLUSTER", "MEET", n.host, strconv.Itoa(n.port), strconv.Itoa(n.busPort)); err != nil {
			return err
		}
	}
	known := strconv.Itoa(len(nodes))
	return waitUntil(ctx, "the cluster to form", func(ctx context.Context) error {
		for _, n := range nodes {
			v, err := n.srv.Do(ctx, "CLUSTER", "INFO")
			if err != nil {
				return err
			}
			info := parseInfo(v.Str)
			if info["cluster_state"] != "ok" || info["cluster_known_nodes"] != known || info["cluster_slots_ok"] != strconv.Itoa(slots) {
				return fmt.Errorf("%s: cluster_state %q, %q known nodes, %q slots ok", n.srv.name,
					info["cluster_state"], info["cluster_known_nodes"], info["cluster_slots_ok"])
			}
		}
		return nil
	})
}

// waitReplica waits until replica r has synced with its primary.
func waitReplica(ctx context.Context, r *Server) error {
	return waitUntil(ctx, r.name+" to sync", func(ctx context.Context) error {
		info, err := r.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if st := info["master_link_status"]; st != "up" {
			return fmt.Errorf("master_link_status %q", st)
		}
		return nil
	})
}

// masterAddr returns the primary sentinel s names for set ("host:port").
func masterAddr(ctx context.Context, s *Server, set string) (string, error) {
	v, err := s.Do(ctx, "SENTINEL", "GET-MASTER-ADDR-BY-NAME", set)
	if err != nil {
		return "", err
	}
	if len(v.Elems) != 2 {
		return "", fmt.Errorf("%s names no primary for %s: %v", s.name, set, v)
	}
	return net.JoinHostPort(v.Elems[0].Str, v.Elems[1].Str), nil
}

// waitSentinel waits until sentinel s names primary for set and lists
// replica as a healthy replica, linked to its primary, so that SENTINEL
// FAILOVER can promote it.
func waitSentinel(ctx context.Context, s *Server, set string, primary, replica *Server) error {
	return waitUntil(ctx, s.name+" to monitor "+primary.name+" and "+replica.name, func(ctx context.Context) error {
		got, err := masterAddr(ctx, s, set)
		if err != nil {
			return err
		}
		if got != primary.addr {
			return fmt.Errorf("%s names %s, want %s", s.name, got, primary.addr)
		}
		v, err := s.Do(ctx, "SENTINEL", "REPLICAS", set)
		if err != nil {
			return err
		}
		for _, r := range v.Elems {
			f := fields(r)
			if net.JoinHostPort(f["ip"], f["port"]) == replica.addr && f["flags"] == "slave" && f["master-link-status"] == "ok" {
				return nil
			}
		}
		return fmt.Errorf("%s lists no healthy replica %s: %v", s.name, replica.addr, v)
	})
}

// fields returns the fields of a flat key and value array (RESP2), or of
// a map (RESP3).
func fields(v Value) map[string]string {
	out := map[string]string{}
	for i := 0; i+1 < len(v.Elems); i += 2 {
		out[v.Elems[i].Str] = v.Elems[i+1].Str
	}
	return out
}

// waitServing waits until s answers PING, then checks its version floor.
func waitServing(ctx context.Context, s *Server, k Kind) error {
	if err := waitUntil(ctx, s.name+" to answer", func(ctx context.Context) error {
		_, err := s.Do(ctx, "PING")
		return err
	}); err != nil {
		return err
	}
	info, err := s.Info(ctx, "server")
	if err != nil {
		return err
	}
	return checkVersion(k, info)
}

// waitUntil polls cond until it returns nil or ctx ends; the error then
// names what was awaited and cond's last error.
func waitUntil(ctx context.Context, what string, cond func(context.Context) error) error {
	for {
		err := cond(ctx)
		if err == nil {
			return nil
		}
		t := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			t.Stop()
			return fmt.Errorf("redisserver: waiting for %s: %w (last: %w)", what, ctx.Err(), err)
		case <-t.C:
		}
	}
}
