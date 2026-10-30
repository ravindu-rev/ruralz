// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"bufio"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// Tests for the dependencies of the process harness: the State Store
// through each Node's own proxies, with TLS and an ACL user; the L4
// balancer over the Nodes, with PROXY protocol v2; and the in-process
// mocks.

// ping authenticates as the URL's user, when it names one, and sends PING
// through the URL's host, over TLS for rediss.
func ping(t *testing.T, topo *Topology, u *url.URL) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a hang guard only
	defer cancel()
	var c net.Conn
	var err error
	if u.Scheme == "rediss" {
		c, err = (&tls.Dialer{Config: topo.ClientTLS()}).DialContext(ctx, "tcp", u.Host)
	} else {
		c, err = (&net.Dialer{}).DialContext(ctx, "tcp", u.Host)
	}
	if err != nil {
		t.Fatalf("dial %s: %v", u.Host, err)
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(30 * time.Second)) // a hang guard only
	cmds, answers := "PING\r\n", []string{"+PONG\r\n"}
	if u.User != nil {
		password, _ := u.User.Password()
		cmds = "AUTH " + u.User.Username() + " " + password + "\r\n" + cmds
		answers = append([]string{"+OK\r\n"}, answers...)
	}
	if _, err := io.WriteString(c, cmds); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	for _, want := range answers {
		if line, err := r.ReadString('\n'); err != nil || line != want {
			t.Fatalf("through %s: %q, %v; want %q", u.Host, line, err, want)
		}
	}
}

// Each Node reaches the State Store through its own fault proxy, over TLS
// as an ACL user whose password is a canary; Promote fails the proxies
// over to the replica.
func TestStateStoreThroughPerNodeProxies(t *testing.T) {
	topo := standinTopology(t)
	ss := topo.StartStateStore("main", StateStoreOptions{Replica: true})
	o := nodeOptions(t, topo)
	o.StateStoreURLs = map[string]string{refStoreURL: "main"}
	nodes := addNodes(t, topo, 2, o)
	hosts := map[string]string{}
	for _, n := range nodes {
		v, ok := n.SecretValue(refStoreURL)
		if !ok {
			t.Fatalf("%s has no State Store URL", n.Name())
		}
		u, err := url.Parse(string(v))
		if err != nil {
			t.Fatal(err)
		}
		password, _ := u.User.Password()
		if u.Scheme != "rediss" || u.User.Username() != StateStoreUser || !topo.canaries.carries([]byte(password)) {
			t.Errorf("%s State Store URL %s://%s@..., want rediss as %s with a canary password", n.Name(), u.Scheme, u.User.Username(), StateStoreUser)
		}
		proxies, ok := n.StateStoreProxy("main")
		if !ok || len(proxies) != 1 || proxies[0].Addr() != u.Host {
			t.Fatalf("%s proxies %v, want one at the URL's host %s", n.Name(), proxies, u.Host)
		}
		if other, dup := hosts[u.Host]; dup {
			t.Errorf("%s and %s share the proxy %s", other, n.Name(), u.Host)
		}
		hosts[u.Host] = n.Name()
		ping(t, topo, u)
		if st := proxies[0].Stats(); st.Accepted < 1 {
			t.Errorf("%s proxy accepted %d connections", n.Name(), st.Accepted)
		}
	}
	if err := ss.Promote(t.Context()); err != nil {
		t.Fatal(err)
	}
	primary := ss.Deployment().Primary().ClientAddr()
	for _, n := range nodes {
		proxies, _ := n.StateStoreProxy("main")
		if got := proxies[0].Upstream(); got != primary {
			t.Errorf("%s proxy dials %s after Promote, want the promoted %s", n.Name(), got, primary)
		}
		v, _ := n.SecretValue(refStoreURL)
		u, _ := url.Parse(string(v))
		ping(t, topo, u)
	}
	if pem := ss.CAPEM(); !topo.canaries.carries(pem) || !strings.Contains(string(pem), "BEGIN CERTIFICATE") {
		t.Error("CAPEM carries no canary or no certificate")
	}
}

// With RESPProxy each Node's proxy counts the commands it forwards, so
// the State Store runs without TLS.
func TestStateStoreRESPProxy(t *testing.T) {
	topo := standinTopology(t)
	topo.StartStateStore("counted", StateStoreOptions{RESPProxy: true})
	o := nodeOptions(t, topo)
	o.StateStoreURLs = map[string]string{refStoreURL: "counted"}
	n := addNodes(t, topo, 1, o)[0]
	counters, ok := n.RESPProxy("counted")
	if !ok || len(counters) != 1 {
		t.Fatalf("RESPProxy = %v, %v", counters, ok)
	}
	v, _ := n.SecretValue(refStoreURL)
	u, _ := url.Parse(string(v))
	if u.Scheme != "redis" {
		t.Fatalf("a counted State Store URL has scheme %s, want redis", u.Scheme)
	}
	ping(t, topo, u)
	if got := counters[0].Snapshot().Count("PING"); got != 1 {
		t.Errorf("the counter saw %d PING commands, want 1", got)
	}
}

// With RESPProxy each primary of a process cluster announces a counting
// proxy of its own: the cluster's slot map names the proxies, so commands
// a cluster client sends to the servers directly are counted too. The
// primaries' replicas replicate through the same proxies, so their
// handshakes count there as well; the test counts the PING it sends
// beside them.
func TestClusterRESPProxiesCountEveryCommand(t *testing.T) {
	topo := standinTopology(t)
	ss := topo.StartStateStore("cluster", StateStoreOptions{Kind: redisserver.ProcessCluster, RESPProxy: true})
	proxies := ss.Proxies()
	if len(proxies) != 3 {
		t.Fatalf("Proxies() = %d proxies, want one per primary", len(proxies))
	}
	slots, err := ss.Deployment().Primary().Do(t.Context(), "CLUSTER", "SLOTS")
	if err != nil {
		t.Fatal(err)
	}
	named := map[string]bool{}
	for _, r := range slots.Elems {
		if len(r.Elems) >= 3 && len(r.Elems[2].Elems) >= 2 {
			named[net.JoinHostPort(r.Elems[2].Elems[0].Str, strconv.FormatInt(r.Elems[2].Elems[1].Int, 10))] = true
		}
	}
	for i, p := range proxies {
		if !named[p.Addr()] {
			t.Errorf("the slot map %v does not name the proxy %s of primary %d", slots, p.Addr(), i)
		}
		u := &url.URL{Scheme: "redis", Host: p.Addr(), User: url.UserPassword(StateStoreUser, storePassword(t, ss))}
		before := p.Snapshot()
		ping(t, topo, u)
		if got := p.Snapshot().Sub(before).Count("PING"); got != 1 {
			t.Errorf("the proxy of primary %d counted %d PING commands, want 1", i, got)
		}
	}
	if n := len(topo.StartStateStore("plain", StateStoreOptions{RESPProxy: true}).Proxies()); n != 0 {
		t.Errorf("a counted standalone State Store announces %d proxies, want none", n)
	}
}

// storePassword returns the ACL user's password of the State Store s.
func storePassword(t *testing.T, s *StateStore) string {
	t.Helper()
	u, err := url.Parse(s.URL())
	if err != nil {
		t.Fatal(err)
	}
	p, _ := u.User.Password()
	return p
}

// StateStoreURL returns the URL a Node reaches a State Store through, also
// one no reference of its test render holds.
func TestStateStoreURL(t *testing.T) {
	topo := standinTopology(t)
	topo.StartStateStore("main", StateStoreOptions{})
	topo.StartStateStore("other", StateStoreOptions{})
	o := nodeOptions(t, topo)
	o.StateStoreURLs = map[string]string{refStoreURL: "main", "file:/etc/ruralz/unused-url": "other"}
	n := addNodes(t, topo, 1, o)[0]
	v, _ := n.SecretValue(refStoreURL)
	if got, ok := n.StateStoreURL("main"); !ok || got != string(v) {
		t.Errorf("StateStoreURL(main) = %q, %v; want the referenced secret's value", got, ok)
	}
	other, ok := n.StateStoreURL("other")
	proxies, _ := n.StateStoreProxy("other")
	if u, err := url.Parse(other); !ok || err != nil || len(proxies) != 1 || u.Host != proxies[0].Addr() {
		t.Errorf("StateStoreURL(other) = %q, %v; want the URL through the Node's own proxy", other, ok)
	}
	if _, ok := n.StateStoreURL("none"); ok {
		t.Error("StateStoreURL of a State Store the Node has no proxy to reports one")
	}
}

// The leak scan reads every State Store's keyspace.
func TestLeakScanReadsTheStateStoreKeyspace(t *testing.T) {
	topo := standinTopology(t)
	ss := topo.StartStateStore("main", StateStoreOptions{})
	primary := ss.Deployment().Primary()
	leak := topo.Canary()
	for _, cmd := range [][]string{{"SET", "plain", "v"}, {"HSET", "h", "field", "x-" + leak}, {"RPUSH", "l", "a", "b"}} {
		if _, err := primary.Do(t.Context(), cmd...); err != nil {
			t.Fatal(err)
		}
	}
	if errs := topo.dumpCaptures(t.Context(), []*StateStore{ss}, nil, nil); len(errs) > 0 {
		t.Fatal(errs)
	}
	findings, errs := topo.scan(nil)
	if len(errs) > 0 || len(findings) != 1 || !strings.HasSuffix(findings[0].Source, "statestore-main") {
		t.Fatalf("findings %v, errors %v; want the hash value in the keyspace", findings, errs)
	}
	// Clean up the planted leak so the teardown's own scan passes.
	if _, err := primary.Do(t.Context(), "DEL", "h"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(topo.captures.dir, "statestore-main")); err != nil {
		t.Fatal(err)
	}
}

// The balancer splices to each Node's listener and probes /readyz over
// the admin TLS; a Node added later joins it.
func TestBalancerOverTheNodes(t *testing.T) {
	topo := standinTopology(t)
	nodes := addNodes(t, topo, 2, nodeOptions(t, topo))
	lb, err := topo.StartBalancer(t.Context(), "http", BalancerOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.WaitHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	// A connection per request: the balancer picks a backend per
	// connection.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	pids := map[string]bool{}
	for range 4 {
		resp, _ := get(t, client, "http://"+lb.Addr()+"/")
		pids[resp.Header.Get("X-Standin-Pid")] = true
	}
	for _, n := range nodes {
		if !pids[strconv.Itoa(n.Process().PID())] {
			t.Errorf("no request reached %s through the balancer", n.Name())
		}
	}
	third := addNodes(t, topo, 1, nodeOptions(t, topo))[0]
	if _, ok := lb.Backend(third.Name()); !ok {
		t.Fatalf("%s did not join the balancer", third.Name())
	}
	if err := lb.WaitHealthy(t.Context(), third.Name()); err != nil {
		t.Fatal(err)
	}
	if lb.Listener() != "http" {
		t.Errorf("Listener = %s", lb.Listener())
	}
}

// With ProxyProtocol a Node's proxyProtocol listener sees each client's
// own address: a client on 127.0.0.2 reaches the Node as 127.0.0.2.
func TestBalancerProxyProtocol(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("only Linux answers 127.0.0.2 without an alias")
	}
	topo := standinTopology(t)
	o := nodeOptions(t, topo)
	o.Bundle = []byte(strings.Replace(testBundle, "      port: 8080\n", "      port: 8080\n      proxyProtocol: true\n", 1))
	addNodes(t, topo, 1, o)
	lb, err := topo.StartBalancer(t.Context(), "http", BalancerOptions{ProxyProtocol: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := lb.WaitHealthy(t.Context()); err != nil {
		t.Fatal(err)
	}
	local := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 2)}
	client := &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{LocalAddr: local}).DialContext}}
	resp, _ := get(t, client, "http://"+lb.Addr()+"/")
	host, _, err := net.SplitHostPort(resp.Header.Get("X-Standin-Client"))
	if err != nil || host != "127.0.0.2" {
		t.Errorf("the Node saw the client as %q, want 127.0.0.2", resp.Header.Get("X-Standin-Client"))
	}
}

// The mocks run with the harness's TLS where a Bundle needs it and live
// until the teardown.
func TestMocks(t *testing.T) {
	topo := standinTopology(t)
	up, err := topo.StartMockUpstream(mockup.Config{Name: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if resp, _ := get(t, topo.HTTPClient(), up.URL()+"/"); resp.StatusCode != http.StatusOK {
		t.Errorf("mock Upstream answered %d", resp.StatusCode)
	}
	idp, err := topo.StartIdP(mockidp.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(idp.JWKSURL(), "https://") {
		t.Errorf("JWKS URL %s, want https", idp.JWKSURL())
	}
	if resp, _ := get(t, topo.HTTPClient(), idp.JWKSURL()); resp.StatusCode != http.StatusOK {
		t.Errorf("JWKS answered %d", resp.StatusCode)
	}
	sink, err := topo.StartOTLPSink(otlpsink.Config{})
	if err != nil || !strings.HasPrefix(sink.URL(), "https://") {
		t.Errorf("OTLP sink %v, %v; want an https endpoint", sink, err)
	}
	clear := standinTopology(t, func(c *Config) { c.Transport.CleartextOTLP = true })
	if s, err := clear.StartOTLPSink(otlpsink.Config{}); err != nil || !strings.HasPrefix(s.URL(), "http://") {
		t.Errorf("cleartext OTLP sink %v, %v; want an http endpoint", s, err)
	}
}
