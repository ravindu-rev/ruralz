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
	return waitClusterWhole(ctx, "the cluster to form", nodes, len(nodes))
}

// waitClusterWhole waits until every node reports the cluster ok, every
// slot served and at least known nodes: the primaries of a forming
// cluster, or the primaries and their replicas once those joined.
func waitClusterWhole(ctx context.Context, what string, nodes []clusterNode, known int) error {
	return waitUntil(ctx, what, func(ctx context.Context) error {
		for _, n := range nodes {
			v, err := n.srv.Do(ctx, "CLUSTER", "INFO")
			if err != nil {
				return err
			}
			info := parseInfo(v.Str)
			got, _ := strconv.Atoi(info["cluster_known_nodes"])
			if info["cluster_state"] != "ok" || got < known || info["cluster_slots_ok"] != strconv.Itoa(slots) {
				return fmt.Errorf("%s: cluster_state %q, %q known nodes, %q slots ok", n.srv.name,
					info["cluster_state"], info["cluster_known_nodes"], info["cluster_slots_ok"])
			}
		}
		return nil
	})
}

// attachReplicas joins replicas[i], a fresh cluster node, to the formed
// cluster of primaries as the replica of primaries[i]: a MEET from the
// replica to the first primary, then CLUSTER REPLICATE with the
// primary's node ID, retried while the replica has not yet learned that
// node from the cluster bus. It waits until every replica has synced
// with its primary and every node sees the cluster whole.
func attachReplicas(ctx context.Context, primaries, replicas []clusterNode) error {
	ids := make([]string, len(primaries))
	for i, p := range primaries {
		v, err := p.srv.Do(ctx, "CLUSTER", "MYID")
		if err != nil {
			return err
		}
		if v.Str == "" {
			return fmt.Errorf("redisserver: %s answered CLUSTER MYID with %v", p.srv.name, v)
		}
		ids[i] = v.Str
	}
	first := primaries[0]
	for i, r := range replicas {
		if _, err := r.srv.Do(ctx, "CLUSTER", "MEET", first.host, strconv.Itoa(first.port), strconv.Itoa(first.busPort)); err != nil {
			return err
		}
		if err := waitUntil(ctx, r.srv.name+" to replicate "+primaries[i].srv.name, func(ctx context.Context) error {
			_, err := r.srv.Do(ctx, "CLUSTER", "REPLICATE", ids[i])
			return err
		}); err != nil {
			return err
		}
	}
	for _, r := range replicas {
		if err := waitReplica(ctx, r.srv); err != nil {
			return err
		}
	}
	all := append(append([]clusterNode(nil), primaries...), replicas...)
	if err := waitClusterWhole(ctx, "the replicas to join the cluster", all, len(all)); err != nil {
		return err
	}
	// The cluster bus spreads each replica's role; until every primary
	// lists it, a takeover could find its primary unknown there.
	return waitUntil(ctx, "every primary to list the replicas", func(ctx context.Context) error {
		for _, p := range primaries {
			v, err := p.srv.Do(ctx, "CLUSTER", "SLOTS")
			if err != nil {
				return err
			}
			if len(v.Elems) != len(primaries) {
				return fmt.Errorf("%s maps %d shards", p.srv.name, len(v.Elems))
			}
			for _, shard := range v.Elems {
				// Start, end, the primary, then one entry per replica.
				if len(shard.Elems) != 4 {
					return fmt.Errorf("%s lists %d servers for slots %v", p.srv.name, len(shard.Elems)-2, shard.Elems[:min(2, len(shard.Elems))])
				}
			}
		}
		return nil
	})
}

// waitPromoted waits until the cluster serves the slots of a promoted
// replica from it: the replica reports role master, and the slot map of
// each other primary in others names the replica's client address for the
// shard's first slot, as a client refreshing its map from any seed then
// reads it.
func waitPromoted(ctx context.Context, replica *Server, firstSlot int, others []*Server) error {
	return waitUntil(ctx, replica.name+" to serve its shard", func(ctx context.Context) error {
		info, err := replica.Info(ctx, "replication")
		if err != nil {
			return err
		}
		if info["role"] != "master" {
			return fmt.Errorf("%s has role %q", replica.name, info["role"])
		}
		for _, o := range others {
			owner, err := slotOwner(ctx, o, firstSlot)
			if err != nil {
				return err
			}
			if owner != replica.client {
				return fmt.Errorf("%s maps slot %d to %s", o.name, firstSlot, owner)
			}
		}
		return nil
	})
}

// slotOwner returns the address ("host:port") s's CLUSTER SLOTS names for
// the primary of slot.
func slotOwner(ctx context.Context, s *Server, slot int) (string, error) {
	v, err := s.Do(ctx, "CLUSTER", "SLOTS")
	if err != nil {
		return "", err
	}
	for _, r := range v.Elems {
		if len(r.Elems) < 3 || len(r.Elems[2].Elems) < 2 {
			continue
		}
		if int(r.Elems[0].Int) <= slot && slot <= int(r.Elems[1].Int) {
			return net.JoinHostPort(r.Elems[2].Elems[0].Str, strconv.FormatInt(r.Elems[2].Elems[1].Int, 10)), nil
		}
	}
	return "", fmt.Errorf("%s maps no primary for slot %d", s.name, slot)
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
