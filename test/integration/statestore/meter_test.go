// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/respproxy"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// setupCommand reports a command a State client sends outside the
// request path, which the round trip does not count: connection setup and
// authentication (HELLO, AUTH, CLIENT, SELECT and a sentinel client's
// ROLE check), script loads, slot map reads, memory readings, keepalives
// and sentinel queries
// (docs/engineering/03-testing-and-quality-strategy.md "Integration
// tests").
func setupCommand(name string) bool {
	first, _, _ := strings.Cut(name, " ")
	switch first {
	case "INFO", "PING", "SCRIPT", "CLUSTER", "HELLO", "AUTH", "CLIENT", "SELECT", "ROLE", "SENTINEL", "COMMAND", "READONLY":
		return true
	}
	return false
}

// foreignCommand reports a command the other clients of a primary send
// it, which no State client sends: a replica's replication handshake and
// acknowledgments, one REPLCONF ACK a second, and a sentinel's hello
// messages, one PUBLISH every 2 s, on the channel it subscribes to. INFO
// commandstats counts them beside the Node's commands, and so do the
// counting proxies a process cluster's primaries announce, since each
// primary's replica replicates through its proxy.
func foreignCommand(name string) bool {
	first, _, _ := strings.Cut(name, " ")
	switch first {
	case "REPLCONF", "PSYNC", "SYNC", "PUBLISH", "SUBSCRIBE":
		return true
	}
	return false
}

// meter counts the commands the Node of a test sends one State Store:
// through the counting RESP proxies that carry every command of a
// deployment the counted function admits, else by INFO commandstats and
// errorstats deltas on its primaries, read per serial phase, either way
// less the commands their other clients send them.
type meter struct {
	e     *env
	store *topology.StateStore
	// proxies are the counting proxies; none means commandstats.
	proxies []*respproxy.Proxy
}

// meter returns the meter of the State Store store, read through the
// Node's own counting proxy or the proxies a process cluster announces.
func (e *env) meter(store string) *meter {
	e.t.Helper()
	s := e.stores[store]
	m := &meter{e: e, store: s}
	kind := s.Deployment().Kind()
	switch {
	case !counted(kind):
	case topologyName(kind) == "cluster":
		m.proxies = s.Proxies()
		if len(m.proxies) == 0 {
			e.t.Fatalf("State Store %s announces no counting proxies", store)
		}
	default:
		ps, ok := e.node.RESPProxy(store)
		if !ok {
			e.t.Fatalf("the Node reaches State Store %s through no counting proxy", store)
		}
		m.proxies = ps
	}
	return m
}

// proxied reports whether the meter reads counting proxies, which also
// give round trips and the command log.
func (m *meter) proxied() bool { return len(m.proxies) > 0 }

// reading is one meter reading.
type reading struct {
	byName map[string]int64
	errors map[string]int64
	// seqs is each proxy's last command Seq.
	seqs []uint64
}

// read reads the meter.
func (m *meter) read() reading {
	m.e.t.Helper()
	r := reading{byName: map[string]int64{}, errors: map[string]int64{}}
	if m.proxied() {
		for _, p := range m.proxies {
			s := p.Snapshot()
			for k, v := range s.ByName {
				r.byName[k] += v
			}
			for k, v := range s.Errors {
				r.errors[k] += v
			}
			r.seqs = append(r.seqs, s.Seq)
		}
		return r
	}
	for _, srv := range m.store.Deployment().Nodes() {
		info, err := srv.Info(m.e.t.Context(), "commandstats")
		if err != nil {
			m.e.t.Fatalf("INFO commandstats on %s: %v", srv.Name(), err)
		}
		for k, v := range info {
			if name, ok := cutStat(k); ok {
				r.byName[name] += statField(v, "calls")
			}
		}
		errs, err := srv.Info(m.e.t.Context(), "errorstats")
		if err != nil {
			continue // a server without the errorstats section counts none
		}
		for k, v := range errs {
			if code, ok := strings.CutPrefix(k, "errorstat_"); ok {
				r.errors[code] += statField(v, "count")
			}
		}
	}
	return r
}

// cutStat returns the command name of an INFO commandstats field,
// "cmdstat_script|load" read as "SCRIPT LOAD", the form counting proxies
// log.
func cutStat(field string) (string, bool) {
	name, ok := strings.CutPrefix(field, "cmdstat_")
	if !ok {
		return "", false
	}
	return strings.ToUpper(strings.ReplaceAll(name, "|", " ")), true
}

// statField returns the integer field name of an INFO stat line such as
// "calls=3,usec=21,usec_per_call=7.00".
func statField(line, name string) int64 {
	for f := range strings.SplitSeq(line, ",") {
		if v, ok := strings.CutPrefix(f, name+"="); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// delta is what a State Store received between two readings.
type delta struct {
	// requestPath counts the request-path commands by name; all counts
	// every command.
	requestPath, all map[string]int64
	errors           map[string]int64
	// trips is the number of round trips of the request-path commands, -1
	// without counting proxies; log holds those commands.
	trips int
	log   []logged
	// scripted reports INFO commandstats counts, which include the
	// commands the scripts call.
	scripted bool
}

// logged is one command a counting proxy logged.
type logged struct {
	// proxy is the index of the proxy that logged it.
	proxy int
	respproxy.Command
}

// String sums the command up for test messages.
func (l logged) String() string {
	return fmt.Sprintf("proxy %d conn %d trip %d %s", l.proxy, l.Conn, l.RoundTrip, l.Name)
}

// scriptCommand reports a command a script of the ruralz_v1.lua library
// calls, which INFO commandstats counts beside the EVALSHA that ran it.
func scriptCommand(name string) bool {
	switch name {
	case "DECRBY", "DEL", "EXISTS", "GET", "HDEL", "HEXISTS", "HGET", "HSET", "INCRBY", "PEXPIRE", "PEXPIREAT", "PTTL", "SET", "TIME":
		return true
	}
	return false
}

// since returns what the State Store received since earlier.
func (m *meter) since(earlier reading) delta {
	m.e.t.Helper()
	now := m.read()
	d := delta{requestPath: map[string]int64{}, all: map[string]int64{}, errors: map[string]int64{}, trips: -1, scripted: !m.proxied()}
	for k, v := range now.byName {
		if foreignCommand(k) {
			continue
		}
		if n := v - earlier.byName[k]; n != 0 {
			d.all[k] = n
			if !setupCommand(k) {
				d.requestPath[k] = n
			}
		}
	}
	for k, v := range now.errors {
		if n := v - earlier.errors[k]; n != 0 {
			d.errors[k] = n
		}
	}
	if !m.proxied() {
		return d
	}
	for i, p := range m.proxies {
		var seq uint64
		if i < len(earlier.seqs) {
			seq = earlier.seqs[i]
		}
		for _, c := range p.LogSince(seq) {
			if !setupCommand(c.Name) && !foreignCommand(c.Name) {
				d.log = append(d.log, logged{proxy: i, Command: c})
			}
		}
	}
	d.trips = trips(d.log)
	return d
}

// reloaded reports whether a primary ran a SCRIPT LOAD the Node sent
// since earlier. A counting proxy logs a command when it reads it, before
// the server runs it, so a load counts once the proxy read its reply,
// which is no error: a call the Node then sends on another connection
// could otherwise reach the server first. INFO commandstats counts a
// command once it ran.
func (m *meter) reloaded(earlier reading) bool {
	m.e.t.Helper()
	if !m.proxied() {
		return m.since(earlier).all["SCRIPT LOAD"] >= 1
	}
	for i, p := range m.proxies {
		var seq uint64
		if i < len(earlier.seqs) {
			seq = earlier.seqs[i]
		}
		for _, c := range p.LogSince(seq) {
			if c.Name == "SCRIPT LOAD" && c.Reply.Kind != 0 && c.Reply.Err == "" {
				return true
			}
		}
	}
	return false
}

// trips counts the round trips of the commands of log named one of names,
// every command when names is empty: commands pipelined on one connection
// of one proxy share a round trip, and each proxy fronts a server of its
// own.
func trips(log []logged, names ...string) int {
	type trip struct {
		proxy int
		trip  uint64
	}
	seen := map[trip]bool{}
	for _, c := range log {
		if len(names) == 0 || slices.Contains(names, c.Name) {
			seen[trip{c.proxy, c.RoundTrip}] = true
		}
	}
	return len(seen)
}

// servers counts the proxies, and so the servers, the commands of log
// named one of names reached.
func servers(log []logged, names ...string) int {
	seen := map[int]bool{}
	for _, c := range log {
		if slices.Contains(names, c.Name) {
			seen[c.proxy] = true
		}
	}
	return len(seen)
}

// phase runs fn and returns what the State Store received meanwhile.
func (m *meter) phase(fn func()) delta {
	m.e.t.Helper()
	before := m.read()
	fn()
	return m.since(before)
}

// commands returns the request-path command names and counts, sorted, for
// test messages.
func (d delta) commands() string {
	keys := slices.Sorted(maps.Keys(d.requestPath))
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k + "=" + strconv.FormatInt(d.requestPath[k], 10))
	}
	return "[" + b.String() + "]"
}

// total returns the number of request-path commands.
func (d delta) total() int64 {
	var n int64
	for _, v := range d.requestPath {
		n += v
	}
	return n
}

// expectCommands fails the test unless d's request-path commands are
// exactly want and, with counting proxies, they took wantTrips round trips
// (wantTrips < 0 skips that check). INFO commandstats also counts the
// commands a script calls, so a phase that ran a script compares the
// EVALSHA calls and the commands no script calls.
func expectCommands(t *testing.T, what string, d delta, want map[string]int64, wantTrips int) {
	t.Helper()
	got := d.requestPath
	if d.scripted && (d.requestPath["EVALSHA"] > 0 || want["EVALSHA"] > 0) {
		got, want = withoutScriptCommands(got), withoutScriptCommands(want)
	}
	if !maps.Equal(got, want) {
		t.Errorf("%s: the Node sent %s on the request path, want %s", what, delta{requestPath: got}.commands(), delta{requestPath: want}.commands())
	}
	if wantTrips >= 0 && d.trips >= 0 && d.trips != wantTrips {
		t.Errorf("%s: the commands took %d round trips, want %d: %v", what, d.trips, wantTrips, d.log)
	}
}

// withoutScriptCommands returns counts without the commands scripts call.
func withoutScriptCommands(counts map[string]int64) map[string]int64 {
	out := map[string]int64{}
	for k, v := range counts {
		if !scriptCommand(k) {
			out[k] = v
		}
	}
	return out
}

// TestMeterLeavesOutOtherClients checks, on the sentinel kinds, whose
// commands INFO commandstats counts, and on the process cluster, whose
// replicas replicate through the counting proxies the primaries announce,
// that the meter leaves out what the primaries' other clients send them:
// once a replica or sentinel has sent a command, a phase without any Node
// counts no request-path command and logs none.
func TestMeterLeavesOutOtherClients(t *testing.T) {
	for _, k := range []redisserver.Kind{redisserver.ProcessSentinel, redisserver.Redis8Sentinel, redisserver.Valkey9Sentinel, redisserver.ProcessCluster} {
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k})
			m := e.meter("main")
			before := m.read()
			// The replica acknowledges the stream every second and the
			// sentinel says hello every 2 s, in real time.
			e.waitFor("a command from the primary's replica or sentinel", func() bool {
				for name, n := range m.read().byName {
					if foreignCommand(name) && n > before.byName[name] {
						return true
					}
				}
				return false
			})
			if d := m.since(before); d.total() != 0 || len(d.log) != 0 {
				t.Errorf("a phase without a Node counted %s on the request path and logged %v, want none", d.commands(), d.log)
			}
		})
	}
}
