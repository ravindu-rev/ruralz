// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"slices"
	"strings"
	"testing"
)

// TestOptionsValidate checks the refused Options: anything on a
// container kind, a replica outside a standalone deployment, Announce on
// a sentinel kind, unusable users and multi-line directives.
func TestOptionsValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		k    Kind
		o    Options
		ok   bool
	}{
		{"plain process", Process, Options{}, true},
		{"everything on a process", Process, Options{TLS: true, Replica: true, Users: []User{{Name: "a", Password: "b"}}, Config: []string{"maxmemory 1mb"}}, true},
		{"plain container", Dragonfly, Options{}, true},
		{"TLS on a container", Redis8, Options{TLS: true}, false},
		{"users on a container", Valkey9, Options{Users: []User{{Name: "a", Password: "b"}}}, false},
		{"announce on a container", Redis8Cluster, Options{Announce: func(int, string) string { return "" }}, false},
		{"config on a container", Dragonfly, Options{Config: []string{"maxmemory 1mb"}}, false},
		{"replica on a cluster", ProcessCluster, Options{Replica: true}, false},
		{"everything a sentinel kind takes", ProcessSentinel, Options{TLS: true, Users: []User{{Name: "a", Password: "b"}}, Config: []string{"maxmemory 1mb"}}, true},
		{"replica on a sentinel kind", ProcessSentinel, Options{Replica: true}, false},
		{"announce on a sentinel kind", ProcessSentinel, Options{Announce: func(int, string) string { return "" }}, false},
		{"TLS on a sentinel container", Redis8Sentinel, Options{TLS: true}, false},
		{"plain sentinel container", Redis8Sentinel, Options{}, true},
		{"users on a Valkey sentinel container", Valkey9Sentinel, Options{Users: []User{{Name: "a", Password: "b"}}}, false},
		{"plain Valkey sentinel container", Valkey9Sentinel, Options{}, true},
		{"unnamed user", Process, Options{Users: []User{{Password: "b"}}}, false},
		{"user name with a space", Process, Options{Users: []User{{Name: "a b", Password: "b"}}}, false},
		{"user without password", Process, Options{Users: []User{{Name: "a"}}}, false},
		{"user twice", Process, Options{Users: []User{{Name: "a", Password: "b"}, {Name: "a", Password: "c"}}}, false},
		{"directive over two lines", Process, Options{Config: []string{"maxmemory 1mb\nsave 1 1"}}, false},
	} {
		if err := tc.o.validate(tc.k); (err == nil) != tc.ok {
			t.Errorf("%s: validate = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
}

func joined(ds []directive) []string {
	out := make([]string, len(ds))
	for i, d := range ds {
		out[i] = strings.Join(d, " ")
	}
	return out
}

// TestNodeDirectives checks the configuration of each node shape:
// loopback, no persistence, TLS only on its port without client
// certificates, ACL users with the default user off, a replica's link,
// and a cluster node's bus port and announced address.
func TestNodeDirectives(t *testing.T) {
	files := &tlsFiles{ca: "/d/ca.crt", cert: "/d/server.crt", key: "/d/server.key"}
	users := []User{{Name: "ruralz", Password: "pw"}}
	for _, tc := range []struct {
		name string
		n    nodeConfig
		want []string
	}{
		{"standalone", nodeConfig{port: 7000, dir: "/d"}, []string{"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 7000"}},
		{"TLS", nodeConfig{port: 7000, dir: "/d", tls: files}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 0", "tls-port 7000", "tls-cert-file /d/server.crt",
			"tls-key-file /d/server.key", "tls-ca-cert-file /d/ca.crt", "tls-auth-clients no", "tls-replication yes",
		}},
		{"users and replica", nodeConfig{port: 7001, dir: "/d", users: users, primary: "127.0.0.1:7000"}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 7001", "user ruralz reset on >pw ~* &* +@all",
			"user default off", "replicaof 127.0.0.1 7000", "masteruser ruralz", "masterauth pw",
		}},
		{"sentinel primary with users", nodeConfig{port: 7000, dir: "/d", users: users, linkAuth: true}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 7000", "user ruralz reset on >pw ~* &* +@all",
			"user default off", "masteruser ruralz", "masterauth pw",
		}},
		{"sentinel primary without users", nodeConfig{port: 7000, dir: "/d", linkAuth: true}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 7000",
		}},
		{"sentinel", nodeConfig{port: 26379, dir: "/d", sentinel: &sentinelConfig{masterSet: "ruralz", primary: "127.0.0.1:7000"}}, []string{
			"bind 127.0.0.1", "dir /d", "port 26379", "sentinel monitor ruralz 127.0.0.1 7000 1",
			"sentinel down-after-milliseconds ruralz 5000", "sentinel failover-timeout ruralz 10000",
		}},
		{"TLS sentinel with users", nodeConfig{port: 26379, dir: "/d", tls: files, users: users, sentinel: &sentinelConfig{masterSet: "ruralz", primary: "127.0.0.1:7000"}}, []string{
			"bind 127.0.0.1", "dir /d", "port 0", "tls-port 26379", "tls-cert-file /d/server.crt",
			"tls-key-file /d/server.key", "tls-ca-cert-file /d/ca.crt", "tls-auth-clients no", "tls-replication yes",
			"user ruralz reset on >pw ~* &* +@all", "user default off",
			"sentinel monitor ruralz 127.0.0.1 7000 1", "sentinel down-after-milliseconds ruralz 5000",
			"sentinel failover-timeout ruralz 10000", "sentinel auth-user ruralz ruralz", "sentinel auth-pass ruralz pw",
		}},
		{"announced cluster node", nodeConfig{port: 7000, busPort: 17000, dir: "/d", announce: "127.0.0.2:8000"}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 7000", "cluster-enabled yes", "cluster-config-file nodes.conf",
			"cluster-port 17000", "cluster-announce-ip 127.0.0.2", "cluster-announce-port 8000",
		}},
		{"announced TLS cluster node", nodeConfig{port: 7000, busPort: 17000, dir: "/d", tls: files, announce: "127.0.0.2:8000"}, []string{
			"bind 127.0.0.1", "dir /d", "save ", "appendonly no", "repl-diskless-sync-delay 0", "port 0", "tls-port 7000", "tls-cert-file /d/server.crt",
			"tls-key-file /d/server.key", "tls-ca-cert-file /d/ca.crt", "tls-auth-clients no", "tls-replication yes",
			"cluster-enabled yes", "cluster-config-file nodes.conf", "cluster-port 17000", "tls-cluster yes",
			"cluster-announce-ip 127.0.0.2", "cluster-announce-port 8000", "cluster-announce-tls-port 8000",
		}},
	} {
		ds, err := tc.n.directives()
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := joined(ds); !slices.Equal(got, tc.want) {
			t.Errorf("%s: directives\n%q\nwant\n%q", tc.name, got, tc.want)
		}
	}
	for _, n := range []nodeConfig{{primary: "nowhere"}, {busPort: 1, announce: "nowhere"}, {sentinel: &sentinelConfig{primary: "nowhere"}}} {
		if _, err := n.directives(); err == nil {
			t.Errorf("directives(%+v): want an error for the bad address", n)
		}
	}
}

// TestContainerSentinelConfig checks the sentinel container's
// configuration: its port, protected mode off for the test process, and
// the monitor of the primary's container IP with the sentinel timings.
func TestContainerSentinelConfig(t *testing.T) {
	conf, err := parseConf(string(containerSentinelConfig("172.17.0.2")))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, d := range conf {
		got = append(got, strings.Join(d, " "))
	}
	want := []string{
		"port 26379", "protected-mode no", "sentinel monitor ruralz 172.17.0.2 6379 1",
		"sentinel down-after-milliseconds ruralz 5000", "sentinel failover-timeout ruralz 10000",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sentinel container configuration\n%q\nwant\n%q", got, want)
	}
}

// TestUserDirectives checks that the default user is off unless listed,
// and that custom rules replace the full-access default.
func TestUserDirectives(t *testing.T) {
	if got := userDirectives(nil); got != nil {
		t.Errorf("no users = %q, want no directive", got)
	}
	got := joined(userDirectives([]User{{Name: "default", Password: "d"}, {Name: "ro", Password: "r", Rules: []string{"~*", "+@read"}}}))
	if want := []string{"user default reset on >d ~* &* +@all", "user ro reset on >r ~* +@read"}; !slices.Equal(got, want) {
		t.Errorf("userDirectives = %q, want %q", got, want)
	}
}

// TestRenderRoundTrip checks that every argument, whatever its bytes,
// reaches the server as written: what render writes, Redis's argument
// splitter (splitArgs) reads back exactly.
func TestRenderRoundTrip(t *testing.T) {
	args := []string{
		"", "plain", "with space", `dq"uote`, "sq'uote", `back\slash`, "new\nline", "tab\there", "cr\rhere",
		"bell\a", "\x00nul", "del\x7f", "héllo", `C:\Users\Test User\redis`, ">p w\"\\x41",
	}
	ds := []directive{append(directive{"user"}, args...), {"save", ""}, {"dir", `C:\tmp\a b`}}
	text := string(render(ds, []string{"maxmemory 64mb"}))
	conf, err := parseConf(text)
	if err != nil {
		t.Fatalf("parse %q: %v", text, err)
	}
	if len(conf) != 4 {
		t.Fatalf("%q: %d lines, want 4", text, len(conf))
	}
	for i, d := range ds {
		if !slices.Equal(conf[i], d) {
			t.Errorf("line %d read back as %q, want %q", i, conf[i], d)
		}
	}
	if !slices.Equal(conf[3], []string{"maxmemory", "64mb"}) {
		t.Errorf("extra line read back as %q", conf[3])
	}
	if quote("plain-word_1.2:3@x/y") != "plain-word_1.2:3@x/y" {
		t.Errorf("a plain word was quoted: %s", quote("plain-word_1.2:3@x/y"))
	}
}
