// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// startHelper starts a process deployment of fake redis-servers.
func startHelper(t *testing.T, k Kind, o Options) *Deployment {
	t.Helper()
	d, err := startProcess(helperLauncher(t, nil), k, o)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func mustDo(t *testing.T, s *Server, args ...string) Value {
	t.Helper()
	v, err := s.Do(t.Context(), args...)
	if err != nil {
		t.Fatalf("%s %q: %v", s.Name(), args, err)
	}
	return v
}

// configArg returns the arguments of a directive of a running fake
// redis-server, as it read its configuration file, or "-" when absent.
func configArg(t *testing.T, s *Server, name string) string {
	t.Helper()
	v := mustDo(t, s, "CONFIG", "GET", name)
	if len(v.Elems) != 2 {
		return "-"
	}
	return v.Elems[1].Str
}

// TestProcessStartKillRestart checks the process lifecycle: a server
// ready on a loopback port, its URL, a kill that stops it, and a restart
// on the same port that loses what it held in memory.
func TestProcessStartKillRestart(t *testing.T) {
	d := startHelper(t, Process, Options{})
	s := d.Primary()
	if d.Kind() != Process || d.Topology() != statestore.TopologyStandalone || len(d.Nodes()) != 1 || d.Replica() != nil {
		t.Fatalf("deployment %s %v, %d nodes, replica %v", d.Kind(), d.Topology(), len(d.Nodes()), d.Replica())
	}
	if want := "redis://" + s.Addr(); d.URL() != want || s.URL() != want || s.ClientAddr() != s.Addr() {
		t.Errorf("URL %q, server URL %q, want %q", d.URL(), s.URL(), want)
	}
	if host, _, _ := net.SplitHostPort(s.Addr()); host != "127.0.0.1" {
		t.Errorf("Addr %q, want loopback", s.Addr())
	}
	if got := mustDo(t, s, "PING"); got.Str != "PONG" {
		t.Fatalf("PING = %v", got)
	}
	mustDo(t, s, "SET", "k", "v")
	if got := configArg(t, s, "save"); got != "" {
		t.Errorf("save = %q, want persistence off", got)
	}
	if !strings.Contains(s.Output(), "Ready to accept connections") {
		t.Errorf("Output = %q, want the readiness line", s.Output())
	}

	if err := s.Kill(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Do(t.Context(), "PING"); err == nil {
		t.Fatal("PING after Kill succeeded")
	}
	addr := s.Addr()
	if err := s.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if s.Addr() != addr {
		t.Fatalf("Addr after Restart = %q, want %q", s.Addr(), addr)
	}
	if got := mustDo(t, s, "GET", "k"); !got.Null {
		t.Errorf("GET after Restart = %v, want nil", got)
	}
	if err := s.Restart(t.Context()); err != nil {
		t.Fatalf("Restart of a running server: %v", err)
	}
	mustDo(t, s, "PING")
}

// TestProcessRetriesTakenPort checks that a port another process takes
// before redis-server binds it costs a retry on a fresh port, and that
// the retries are bounded.
func TestProcessRetriesTakenPort(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	taken := ln.Addr().(*net.TCPAddr).Port
	calls := 0
	ports := func(ctx context.Context) (int, error) {
		if calls++; calls == 1 {
			return taken, nil
		}
		return freePort(ctx)
	}
	d, err := startProcess(helperLauncher(t, ports), Process, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, port, _ := net.SplitHostPort(d.Primary().Addr()); port == strconv.Itoa(taken) || calls != 2 {
		t.Fatalf("Addr %s after %d port picks, want a second, free port", d.Primary().Addr(), calls)
	}

	always := func(context.Context) (int, error) { return taken, nil }
	if _, err := startProcess(helperLauncher(t, always), Process, Options{}); !errors.Is(err, errPortInUse) {
		t.Fatalf("start with every port taken = %v, want errPortInUse", err)
	}
}

// TestProcessStartFailureShowsOutput checks that a server that refuses
// its configuration fails the start with its own output.
func TestProcessStartFailureShowsOutput(t *testing.T) {
	_, err := startProcess(helperLauncher(t, nil), Process, Options{Config: []string{"no-such-directive yes"}})
	if err == nil || !strings.Contains(err.Error(), "Bad directive") {
		t.Fatalf("start = %v, want the server's Bad directive output", err)
	}
}

// TestProcessExtraConfig checks that Options.Config lines reach the
// server verbatim.
func TestProcessExtraConfig(t *testing.T) {
	d := startHelper(t, Process, Options{Config: []string{"maxmemory 64mb"}})
	if got := configArg(t, d.Primary(), "maxmemory"); got != "64mb" {
		t.Errorf("maxmemory = %q, want 64mb", got)
	}
}

// TestProcessTLSAndUsers checks Options.TLS and Options.Users: a rediss://
// URL with the first user's escaped credentials, admin connections over
// TLS that authenticate, a client without credentials refused, and a CA
// that verifies the server.
func TestProcessTLSAndUsers(t *testing.T) {
	users := []User{{Name: "ruralz", Password: `p w"\x`}, {Name: "ops", Password: "o"}}
	d := startHelper(t, Process, Options{TLS: true, Users: users})
	s := d.Primary()
	u, err := url.Parse(d.URL())
	if err != nil {
		t.Fatal(err)
	}
	if pw, _ := u.User.Password(); u.Scheme != "rediss" || u.User.Username() != "ruralz" || pw != users[0].Password || u.Host != s.Addr() {
		t.Errorf("URL %q, want rediss:// with the first user's credentials", d.URL())
	}
	if got := mustDo(t, s, "PING"); got.Str != "PONG" {
		t.Fatalf("PING over TLS = %v", got)
	}
	for name, want := range map[string]string{"tls-auth-clients": "no", "port": "0", "tls-port": strconv.Itoa(s.node.port)} {
		if got := configArg(t, s, name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}

	// No credentials: TLS works, the server refuses the command.
	c, err := dialConn(t.Context(), s.Addr(), dialOptions{tls: d.ClientTLS()})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	var re *ReplyError
	if _, err := c.Do(t.Context(), "PING"); !errors.As(err, &re) || re.Code() != "NOAUTH" {
		t.Errorf("PING without AUTH = %v, want NOAUTH", err)
	}
	if _, err := dialConn(t.Context(), s.Addr(), dialOptions{tls: d.ClientTLS(), user: "ops", password: "wrong"}); !errors.As(err, &re) || re.Code() != "WRONGPASS" {
		t.Errorf("AUTH with a wrong password = %v, want WRONGPASS", err)
	}

	pem, err := os.ReadFile(d.CAFile())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		t.Fatalf("CAFile holds %q, want a PEM certificate", pem)
	}
	trusting := &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	if _, err := dialConn(t.Context(), s.Addr(), dialOptions{tls: trusting, user: "ops", password: "o"}); err != nil {
		t.Errorf("TLS trusting CAFile: %v", err)
	}
	other := &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
	if _, err := dialConn(t.Context(), s.Addr(), dialOptions{tls: other}); err == nil {
		t.Error("TLS trusting another CA succeeded")
	}
}

// TestProcessReplicaPromote checks Options.Replica and Promote: a replica
// synced from the primary, promoted to primary, and kept a primary
// across its restarts.
func TestProcessReplicaPromote(t *testing.T) {
	d := startHelper(t, Process, Options{Replica: true, Users: []User{{Name: "ruralz", Password: "pw"}}})
	primary, replica := d.Primary(), d.Replica()
	if replica == nil {
		t.Fatal("no replica")
	}
	if got := configArg(t, replica, "replicaof"); got != "127.0.0.1 "+strconv.Itoa(primary.node.port) {
		t.Errorf("replicaof = %q, want the primary", got)
	}
	if got := configArg(t, replica, "masteruser"); got != "ruralz" {
		t.Errorf("masteruser = %q, want the first user", got)
	}
	if err := d.Promote(t.Context()); err != nil {
		t.Fatal(err)
	}
	if d.Primary() != replica || d.Replica() != nil {
		t.Fatalf("after Promote: primary %s, replica %v; want the former replica alone", d.Primary().Name(), d.Replica())
	}
	if d.URL() != replica.URL() {
		t.Errorf("URL %q, want the promoted server's %q", d.URL(), replica.URL())
	}
	if err := replica.Restart(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := configArg(t, replica, "replicaof"); got != "-" {
		t.Errorf("replicaof after Promote and Restart = %q, want none", got)
	}
	if err := d.Promote(t.Context()); !errors.Is(err, errNoReplica) {
		t.Errorf("second Promote = %v, want errNoReplica", err)
	}
}

// TestProcessClusterAnnounce checks the process cluster: three primaries
// on distinct ports and bus ports, each announcing the address
// Options.Announce returned, which the URL lists as seeds.
func TestProcessClusterAnnounce(t *testing.T) {
	var announced []string
	d := startHelper(t, ProcessCluster, Options{Announce: func(node int, addr string) string {
		a := net.JoinHostPort("127.0.0.2", strconv.Itoa(7000+node))
		announced = append(announced, addr+">"+a)
		return a
	}})
	nodes := d.Nodes()
	if len(nodes) != 3 || d.Topology() != statestore.TopologyCluster {
		t.Fatalf("%d nodes of %v, want a cluster of 3", len(nodes), d.Topology())
	}
	ports := map[string]bool{}
	for i, n := range nodes {
		want := net.JoinHostPort("127.0.0.2", strconv.Itoa(7000+i))
		if n.ClientAddr() != want || announced[i] != n.Addr()+">"+want {
			t.Errorf("node %d: client address %q (announced %q), want %q", i, n.ClientAddr(), announced[i], want)
		}
		for _, name := range []string{"port", "cluster-port"} {
			p := configArg(t, n, name)
			if ports[p] {
				t.Errorf("node %d: %s %s reused", i, name, p)
			}
			ports[p] = true
		}
		if got := configArg(t, n, "cluster-announce-port"); got != strconv.Itoa(7000+i) {
			t.Errorf("node %d: cluster-announce-port %q", i, got)
		}
		if got := configArg(t, n, "cluster-announce-ip"); got != "127.0.0.2" {
			t.Errorf("node %d: cluster-announce-ip %q", i, got)
		}
	}
	if want := "redis://127.0.0.2:7000?addr=127.0.0.2:7001&addr=127.0.0.2:7002"; d.URL() != want {
		t.Errorf("URL = %q, want %q", d.URL(), want)
	}
}

// TestProcessPortsNeverShared checks that the servers of one launch never
// share a port, even when the port source repeats one: every other pick
// returns the first port it gave.
func TestProcessPortsNeverShared(t *testing.T) {
	first, calls := 0, 0
	repeating := func(ctx context.Context) (int, error) {
		calls++
		if first != 0 && calls%2 == 0 {
			return first, nil
		}
		p, err := freePort(ctx)
		if first == 0 {
			first = p
		}
		return p, err
	}
	d, err := startProcess(helperLauncher(t, repeating), ProcessCluster, Options{})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, n := range d.Nodes() {
		for _, name := range []string{"port", "cluster-port"} {
			p := configArg(t, n, name)
			if seen[p] {
				t.Errorf("node %d: %s %s is already in use", i, name, p)
			}
			seen[p] = true
		}
	}
}

// TestProcessOutputOnFailure checks that a failing test shows the output
// of its servers when they stop.
func TestProcessOutputOnFailure(t *testing.T) {
	rt := &recordTB{TB: t}
	l := helperLauncher(t, nil)
	l.tb = rt
	d, err := startProcess(l, Process, Options{})
	if err != nil {
		t.Fatal(err)
	}
	rt.failed = true
	d.Primary().cleanup()
	if !strings.Contains(rt.logs.String(), "Ready to accept connections") {
		t.Errorf("logged %q, want the server output", rt.logs.String())
	}
}

// recordTB is a testing.TB whose failure the test sets and whose logs it
// reads.
type recordTB struct {
	testing.TB
	failed bool
	logs   strings.Builder
}

func (r *recordTB) Failed() bool { return r.failed }

func (r *recordTB) Logf(format string, args ...any) { _, _ = fmt.Fprintf(&r.logs, format, args...) }
