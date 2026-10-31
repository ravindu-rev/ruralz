// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"context"
	"errors"
	"net"
	"strconv"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// errNoSlots reports that no node answered CLUSTER SLOTS with a map.
var errNoSlots = errors.New("redis: no cluster node answered CLUSTER SLOTS")

// refreshSlots reads the cluster's slot map with CLUSTER SLOTS from a
// known primary, else any node the cluster client knows, and publishes
// it: each slot range's primary becomes a shard, slots no range covers
// stay with the unmapped shard. The maintenance goroutine calls it.
func (d *Driver) refreshSlots(ctx context.Context, c rueidis.Client) error {
	nodes := c.Nodes()
	var order []string
	if m := d.slots.Load(); m != nil {
		for _, sh := range m.shards[1:] {
			if _, ok := nodes[sh.addr]; ok {
				order = append(order, sh.addr)
			}
		}
	}
	for addr := range nodes {
		order = append(order, addr)
	}
	err := errNoSlots
	for _, addr := range order {
		node := nodes[addr]
		actx, cancel := context.WithTimeout(ctx, d.opts.AdminTimeout)
		msg, rerr := node.Do(actx, node.B().ClusterSlots().Build()).ToMessage()
		cancel()
		if rerr != nil {
			err = rerr
			continue
		}
		ranges, perr := parseSlots(msg, addr)
		if perr != nil || len(ranges) == 0 {
			err = errors.Join(errNoSlots, perr)
			continue
		}
		d.publishSlots(ranges)
		return nil
	}
	return err
}

// slotRange is one CLUSTER SLOTS entry: slots first to last served by the
// primary at addr.
type slotRange struct {
	first, last int64
	addr        string
}

// parseSlots reads a CLUSTER SLOTS reply from the node at from: each
// entry's start, end and primary endpoint, an empty endpoint meaning
// from's host and "?" an unknown one, which is skipped.
func parseSlots(msg rueidis.RedisMessage, from string) ([]slotRange, error) {
	entries, err := msg.ToArray()
	if err != nil {
		return nil, err
	}
	fromHost, _, _ := net.SplitHostPort(from)
	var out []slotRange
	for i := range entries {
		v, err := entries[i].ToArray()
		if err != nil || len(v) < 3 {
			return nil, errors.New("redis: malformed CLUSTER SLOTS entry")
		}
		first, err1 := v[0].AsInt64()
		last, err2 := v[1].AsInt64()
		primary, err3 := v[2].ToArray()
		if err1 != nil || err2 != nil || err3 != nil || len(primary) < 2 || first < 0 || last >= keys.Slots || first > last {
			return nil, errors.New("redis: malformed CLUSTER SLOTS entry")
		}
		host, err := primary[0].ToString()
		if err != nil {
			return nil, errors.New("redis: malformed CLUSTER SLOTS endpoint")
		}
		port, err := primary[1].AsInt64()
		if err != nil {
			return nil, errors.New("redis: malformed CLUSTER SLOTS port")
		}
		switch host {
		case "?":
			continue
		case "":
			host = fromHost
		}
		out = append(out, slotRange{first: first, last: last, addr: net.JoinHostPort(host, strconv.FormatInt(port, 10))})
	}
	return out, nil
}

// publishSlots installs the slot map of ranges.
func (d *Driver) publishSlots(ranges []slotRange) {
	m := &slotMap{shards: []*shard{d.unmapped}, at: d.clk.Now()}
	index := map[string]uint16{}
	for _, r := range ranges {
		i, ok := index[r.addr]
		if !ok {
			i = uint16(len(m.shards)) //nolint:gosec // G115: at most MaxShards + 1 shards
			index[r.addr] = i
			m.shards = append(m.shards, d.shardAt(r.addr))
		}
		for s := r.first; s <= r.last; s++ {
			m.owner[s] = i
		}
	}
	m.full = true
	for _, o := range m.owner {
		if o == 0 {
			m.full = false
			break
		}
	}
	d.slots.Store(m)
}
