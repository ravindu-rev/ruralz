// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"net"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/netaddr"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// zoneTable is one zone map of the connection (stateStore.zones or
// stateStore.cache.zones) compared with the Node's RURALZ_ZONE: the zone
// label of each mapped server host, in netaddr.CanonicalHost form. A host
// without an entry, and every host of a Node without RURALZ_ZONE, is
// unknown (docs/architecture/11-scalability-and-distributed-state.md
// "State client and RZ-STS error codes"). A table is never modified once
// published.
type zoneTable struct {
	labels map[string]int
}

// newZoneTable returns the table of zones for a Node in zone node.
func newZoneTable(node string, zones statestore.Zones) *zoneTable {
	t := &zoneTable{}
	if node == "" || len(zones) == 0 {
		return t
	}
	t.labels = make(map[string]int, len(zones))
	for host, zone := range zones {
		t.labels[host] = statestore.ZoneLabel(node, zone)
	}
	return t
}

// label returns the zone label of a call to the server at host, a
// netaddr.CanonicalHost form or nil for a shard without a server host
// (the unmapped and overflow shards).
func (t *zoneTable) label(host *string) int {
	if host == nil {
		return emit.StateZoneUnknown
	}
	if l, ok := t.labels[*host]; ok {
		return l
	}
	return emit.StateZoneUnknown
}

// SetZones replaces the connection's zone map: later calls are labeled by
// it, while the driver keeps its connections, breakers and scripts. The
// Manager calls it when a newer snapshot's first call reaches a driver a
// Hot Reload kept (statestore.ZoneSetter).
func (d *Driver) SetZones(z statestore.Zones) { d.zones.Store(newZoneTable(d.zone, z)) }

var _ statestore.ZoneSetter = (*Driver)(nil)

// hostOf returns the netaddr.CanonicalHost form of addr's host, or nil
// for an address without one the zone map can name.
func hostOf(addr string) *string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	c, err := netaddr.CanonicalHost(h)
	if err != nil {
		return nil
	}
	return &c
}

// setHost records addr's host as the shard's server host, the one its
// calls are labeled by; the host stays when addr has none.
func (sh *shard) setHost(addr string) {
	if h := hostOf(addr); h != nil {
		if cur := sh.host.Load(); cur == nil || *cur != *h {
			sh.host.Store(h)
		}
	}
}

// tripZone returns the zone label of tr's round trip, from the servers of
// the shards its commands go to, read before it is sent: the shard's
// primary in the slot map, the standalone endpoint or the primary the
// sentinels named last, so a failover, a slot map change or a sentinel
// answer labels the next call at once. A round trip to several servers,
// a pipelined batch over cluster shards, is cross when any of its servers
// is in another zone, else unknown when any server's zone is unknown,
// else same: its duration is its slowest server's.
func (d *Driver) tripZone(tr *trip) int {
	t := d.zones.Load()
	zone := emit.StateZoneSame
	for _, sp := range tr.specs {
		switch t.label(tr.tickets[sp.ticket].sh.host.Load()) {
		case emit.StateZoneCross:
			return emit.StateZoneCross
		case emit.StateZoneUnknown:
			zone = emit.StateZoneUnknown
		}
	}
	if len(tr.specs) == 0 {
		return emit.StateZoneUnknown
	}
	return zone
}

// followPrimary records the address of the primary the sentinel client
// sends to as the sentinel topology's primary shard's server host. The
// dialer records every primary dial at once; this reading corrects a dial
// to a server whose ROLE check then failed, or a redial of the previous
// primary racing the switch.
func (d *Driver) followPrimary(c rueidis.Client) {
	if d.topo != statestore.TopologySentinel || c == nil {
		return
	}
	for addr := range c.Nodes() {
		d.single.setHost(addr)
	}
}
