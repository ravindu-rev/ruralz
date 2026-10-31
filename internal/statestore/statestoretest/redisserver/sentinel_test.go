// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"errors"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// confLines returns the configuration file a server of the process
// flavor was started with, one directive per element.
func confLines(t *testing.T, s *Server) []string {
	t.Helper()
	s.mu.Lock()
	dir := s.node.dir
	s.mu.Unlock()
	text, err := os.ReadFile(filepath.Join(dir, "redis.conf")) //nolint:gosec // G304: the file the launcher wrote.
	if err != nil {
		t.Fatal(err)
	}
	conf, err := parseConf(string(text))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(conf))
	for i, d := range conf {
		out[i] = strings.Join(d, " ")
	}
	return out
}

// roleOf returns the role INFO replication reports for s.
func roleOf(t *testing.T, s *Server) string {
	t.Helper()
	info, err := s.Info(t.Context(), "replication")
	if err != nil {
		t.Fatal(err)
	}
	return info["role"]
}

// primaryOf returns the primary sentinel s names for MasterSet.
func primaryOf(t *testing.T, s *Server) string {
	t.Helper()
	v := mustDo(t, s, "SENTINEL", "GET-MASTER-ADDR-BY-NAME", MasterSet)
	if len(v.Elems) != 2 {
		t.Fatalf("GET-MASTER-ADDR-BY-NAME = %v", v)
	}
	return net.JoinHostPort(v.Elems[0].Str, v.Elems[1].Str)
}

// TestProcessSentinel checks the sentinel kind of the process flavor with
// users: a primary that holds the replica link credentials too, its
// synced replica, and a sentinel started with --sentinel, monitoring the
// primary as MasterSet and reaching it as the first user; the URL names
// the sentinel and the master set; Failover makes the sentinel promote
// the replica, after which Primary and Replica swap, across a restart of
// the sentinel too; Promote is refused.
func TestProcessSentinel(t *testing.T) {
	d := startHelper(t, ProcessSentinel, Options{Users: []User{{Name: "ruralz", Password: "pw"}}})
	primary, replica, sentinels := d.Primary(), d.Replica(), d.Sentinels()
	if d.Topology() != statestore.TopologySentinel || replica == nil || len(sentinels) != 1 || len(d.Nodes()) != 1 || d.MasterSet() != MasterSet {
		t.Fatalf("deployment %v: replica %v, %d sentinels, %d nodes, master set %q", d.Topology(), replica, len(sentinels), len(d.Nodes()), d.MasterSet())
	}
	s := sentinels[0]
	if want := "redis://ruralz:pw@" + s.ClientAddr() + "?master_set=" + MasterSet; d.URL() != want {
		t.Errorf("URL %q, want %q", d.URL(), want)
	}
	if got := primaryOf(t, s); got != primary.Addr() {
		t.Errorf("the sentinel names %s, want the primary %s", got, primary.Addr())
	}
	_, pport, _ := net.SplitHostPort(primary.Addr())
	conf := confLines(t, s)
	for _, want := range []string{
		"sentinel monitor " + MasterSet + " 127.0.0.1 " + pport + " 1",
		"sentinel auth-user " + MasterSet + " ruralz", "sentinel auth-pass " + MasterSet + " pw",
		"user ruralz reset on >pw ~* &* +@all", "user default off",
	} {
		if !slices.Contains(conf, want) {
			t.Errorf("sentinel configuration lacks %q:\n%q", want, conf)
		}
	}
	if slices.ContainsFunc(conf, func(l string) bool { return strings.HasPrefix(l, "save") || strings.HasPrefix(l, "maxmemory") }) {
		t.Errorf("the sentinel took a server directive: %q", conf)
	}
	if got := configArg(t, primary, "masteruser"); got != "ruralz" {
		t.Errorf("primary masteruser = %q, want the first user, for its link once demoted", got)
	}
	if got := configArg(t, replica, "replicaof"); got != "127.0.0.1 "+pport {
		t.Errorf("replicaof = %q, want the primary", got)
	}

	if err := d.Failover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Primary() != replica || d.Replica() != primary {
		t.Fatalf("after Failover: primary %s, replica %v; want them swapped", d.Primary().Name(), d.Replica())
	}
	if got := primaryOf(t, s); got != replica.Addr() {
		t.Errorf("after Failover the sentinel names %s, want %s", got, replica.Addr())
	}
	if role := roleOf(t, replica); role != "master" {
		t.Errorf("promoted role %q", role)
	}
	if role := roleOf(t, primary); role != "slave" {
		t.Errorf("demoted role %q", role)
	}
	if err := d.Promote(t.Context()); !errors.Is(err, errSentinelPromote) {
		t.Errorf("Promote = %v, want errSentinelPromote", err)
	}
	if err := s.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := primaryOf(t, s); got != replica.Addr() {
		t.Errorf("a restarted sentinel names %s, want the promoted %s", got, replica.Addr())
	}
}

// TestProcessSentinelTLS checks a TLS sentinel deployment: a rediss://
// URL naming the sentinel, its TLS listener and TLS links to its servers,
// over which Failover runs.
func TestProcessSentinelTLS(t *testing.T) {
	d := startHelper(t, ProcessSentinel, Options{TLS: true, Config: []string{"maxmemory 1mb"}})
	s := d.Sentinels()[0]
	u, err := url.Parse(d.URL())
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "rediss" || u.Host != s.ClientAddr() || u.Query().Get("master_set") != MasterSet {
		t.Errorf("URL %q", d.URL())
	}
	_, sport, _ := net.SplitHostPort(s.Addr())
	conf := confLines(t, s)
	for _, want := range []string{"port 0", "tls-port " + sport, "tls-replication yes", "tls-auth-clients no"} {
		if !slices.Contains(conf, want) {
			t.Errorf("sentinel configuration lacks %q:\n%q", want, conf)
		}
	}
	if got := configArg(t, d.Primary(), "maxmemory"); got != "1mb" {
		t.Errorf("primary maxmemory = %q, want Options.Config on the servers", got)
	}
	replica := d.Replica()
	if err := d.Failover(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := primaryOf(t, s); got != replica.Addr() {
		t.Errorf("after Failover over TLS the sentinel names %s, want %s", got, replica.Addr())
	}
}

// TestFailoverNeedsSentinel checks that Failover refuses a deployment
// without a sentinel.
func TestFailoverNeedsSentinel(t *testing.T) {
	d := startHelper(t, Process, Options{Replica: true})
	if err := d.Failover(t.Context()); !errors.Is(err, errNotSentinel) {
		t.Fatalf("Failover = %v, want errNotSentinel", err)
	}
	if d.Sentinels() != nil || d.MasterSet() != "" {
		t.Errorf("standalone: sentinels %v, master set %q", d.Sentinels(), d.MasterSet())
	}
	if _, port, _ := net.SplitHostPort(d.Primary().Addr()); !strings.Contains(d.URL(), port) || strings.Contains(d.URL(), "master_set") {
		t.Errorf("standalone URL %q", d.URL())
	}
}
