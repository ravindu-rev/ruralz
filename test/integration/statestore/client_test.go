// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The State client behaviors a Node shows
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes").

// limitedBundle returns a Bundle whose Route /limited carries a ratelimit
// Policy of failureMode closed, so a State Store failure shows as 503,
// on the stateStore block given; extra adds documents.
func limitedBundle(upstream, stateStore string, extra ...string) []byte {
	return bundle{
		stateStore: stateStore,
		upstream:   upstream,
		docs:       append([]string{rateLimit("rl", 100000, "1m", "", "closed"), route("limited", "/limited", "rl")}, extra...),
	}.render()
}

// setupSent returns the connection setup commands of d: a redial
// authenticates and loads the scripts again. INFO commandstats also
// counts the AUTH of the admin connections that read it, which carry no
// HELLO, so AUTH counts only through counting proxies.
func setupSent(d delta) map[string]int64 {
	out := map[string]int64{}
	for _, name := range []string{"HELLO", "AUTH", "SCRIPT LOAD", "CLIENT SETINFO", "CLIENT SETNAME"} {
		if n := d.all[name]; n != 0 && (name != "AUTH" || !d.scripted) {
			out[name] = n
		}
	}
	return out
}

// accepted sums the connections the Node's proxies in front of the State
// Store store accepted.
func (e *env) accepted(store string) int64 {
	e.t.Helper()
	ps, ok := e.node.StateStoreProxy(store)
	if !ok {
		e.t.Fatalf("the Node reaches State Store %s through no proxy", store)
	}
	var n int64
	for _, p := range ps {
		n += p.Stats().Accepted
	}
	return n
}

// active sums the open connections of the Node's proxies in front of the
// State Store store.
func (e *env) active(store string) int64 {
	e.t.Helper()
	ps, _ := e.node.StateStoreProxy(store)
	var n int64
	for _, p := range ps {
		n += p.Stats().Active
	}
	return n
}

// TestHotReloadKeepsClient checks, on every kind, that a Hot Reload that
// keeps the State Store configuration keeps the State client: no redial,
// no authentication and no script load follow it, and its connections
// serve the new snapshot's calls.
func TestHotReloadKeepsClient(t *testing.T) {
	for _, k := range allKinds() {
		t.Run(string(k), func(t *testing.T) {
			e := newEnv(t, envOptions{kind: k})
			ss := redisStore(topologyName(k), urlRef)
			e.start(limitedBundle(e.up.Addr(), ss), mainURLs())
			m := e.meter("main")
			e.warmUp("/limited", "x-client", "h")
			before, dialed := m.read(), e.accepted("main")
			e.reload(limitedBundle(e.up.Addr(), ss, route("other", "/other", "rl")))
			for _, path := range []string{"/limited", "/other"} {
				expect(t, path+" after the Hot Reload", e.get(path, "x-client", "h"), http.StatusOK, "")
			}
			d := m.since(before)
			if got := setupSent(d); len(got) != 0 {
				t.Errorf("the Hot Reload redialed: the Node sent %v", got)
			}
			if got := e.accepted("main") - dialed; got != 0 {
				t.Errorf("the Node opened %d connections after the Hot Reload, want none", got)
			}
			expectCommands(t, "the calls after the Hot Reload", d, evalsha(2), 2)
		})
	}
}

// TestChangedURLOrTopologyOpensNewClient checks that a Hot Reload that
// changes the State Store URL, or its topology, opens a new client before
// the swap: the first request after it is served there, without a
// failure, and the old client closes once its snapshot is freed.
func TestChangedURLOrTopologyOpensNewClient(t *testing.T) {
	e := newEnv(t, envOptions{stores: []string{"other"}})
	cluster := e.top.StartStateStore("cluster", topology.StateStoreOptions{Kind: redisserver.ProcessCluster, RESPProxy: true})
	e.stores["cluster"] = cluster
	urls := map[string]string{urlRef: "main", otherURLRef: "other", clusterURLRef: "cluster"}
	e.start(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef)), urls)
	expect(t, "a request on the first State Store", e.get("/limited", "x-client", "u"), http.StatusOK, "")

	other := e.meter("other")
	d := other.phase(func() {
		e.reload(limitedBundle(e.up.Addr(), redisStore("standalone", otherURLRef)))
		expect(t, "the first request after a URL change", e.get("/limited", "x-client", "u"), http.StatusOK, "")
	})
	expectCommands(t, "the request after the URL change", d, evalsha(1), 1)
	if setupSent(d)["SCRIPT LOAD"] == 0 {
		t.Error("the new client loaded no scripts before its first call")
	}
	e.waitFor("the first client to close", func() bool {
		e.advance(time.Second)
		return e.active("main") == 0
	})

	m := e.meter("cluster")
	d = m.phase(func() {
		e.reload(limitedBundle(e.up.Addr(), redisStore("cluster", clusterURLRef)))
		expect(t, "the first request after a topology change", e.get("/limited", "x-client", "u"), http.StatusOK, "")
	})
	expectCommands(t, "the request after the topology change", d, evalsha(1), 1)
	e.waitFor("the standalone client to close", func() bool {
		e.advance(time.Second)
		return e.active("other") == 0
	})
}

// TestChangedTLSBlockOpensNewClient checks that a Hot Reload that adds a
// TLS block to the connection opens a new client, which dials again and
// verifies the server with the block's CA, and that the old client closes.
func TestChangedTLSBlockOpensNewClient(t *testing.T) {
	e := newEnv(t, envOptions{tlsStore: true})
	caRef := "file:/etc/ruralz/state-store-ca.crt"
	ca := e.stores["main"].CAPEM()
	if ca == nil {
		t.Fatal("the TLS State Store has no CA")
	}
	e.start(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef)), mainURLs(), func(o *topology.NodeOptions) {
		o.Secrets = map[string][]byte{caRef: ca}
	})
	expect(t, "a request verified against the system roots", e.get("/limited", "x-client", "t"), http.StatusOK, "")
	dialed := e.accepted("main")
	e.reload(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef, "tls:", "  caCertificate: "+secretRef(caRef))))
	expect(t, "a request verified against the TLS block's CA", e.get("/limited", "x-client", "t"), http.StatusOK, "")
	if e.accepted("main") == dialed {
		t.Error("the TLS block change dialed no connection: the old client serves")
	}
	opened := e.accepted("main") - dialed
	e.waitFor("the old client to close", func() bool {
		e.advance(time.Second)
		return e.active("main") <= opened
	})
}

// TestRotatedFileSecretURLOpensNewClient checks that a rotated value of a
// file secret State Store URL opens a new client at once, without a Hot
// Reload: after the resolver's 2 s poll the calls go to the deployment
// the new value names, which starts on empty counters, and the old
// client closes.
func TestRotatedFileSecretURLOpensNewClient(t *testing.T) {
	e := newEnv(t, envOptions{stores: []string{"other"}})
	e.start(bundle{
		stateStore: redisStore("standalone", urlRef),
		upstream:   e.up.Addr(),
		docs:       []string{rateLimit("rl", 2, "1m", "0", "closed"), route("limited", "/limited", "rl")},
	}.render(), map[string]string{urlRef: "main", otherURLRef: "other"})
	revision := e.node.TestDigest()
	expect(t, "the GCRA admission", e.get("/limited", "x-client", "r"), http.StatusOK, "")
	expect(t, "the GCRA deny", e.get("/limited", "x-client", "r"), http.StatusTooManyRequests, "RZ-RL-002")

	path, ok := e.node.SecretPath(urlRef)
	if !ok {
		t.Fatal("the test render maps no file to the State Store URL")
	}
	next, ok := e.node.StateStoreURL("other")
	if !ok {
		t.Fatal("the Node has no proxy to the other State Store")
	}
	if err := os.WriteFile(path, []byte(next), 0o600); err != nil {
		t.Fatal(err)
	}
	other := e.meter("other")
	d := other.phase(func() {
		// The resolver polls file secrets every 2 s; the deny stays cached
		// until its 30 s retry-after, then the key asks the new client.
		e.advance(2 * time.Second)
		e.waitFor("the new client", func() bool { return e.logCount(msgRotated) == 1 })
		e.advance(29 * time.Second)
		expect(t, "the key on the new deployment's empty counters", e.get("/limited", "x-client", "r"), http.StatusOK, "")
	})
	expectCommands(t, "the request after the rotation", d, evalsha(1), 1)
	if setupSent(d)["SCRIPT LOAD"] == 0 {
		t.Error("the rotated client loaded no scripts before its first call")
	}
	e.waitFor("the old client to close", func() bool {
		e.advance(time.Second)
		return e.active("main") == 0
	})
	if got := e.node.TestDigest(); got != revision {
		t.Errorf("the rotation changed the Revision: %s, then %s", revision, got)
	}
	if doc := e.snapshots(); len(doc.Snapshots) != 1 {
		t.Errorf("the rotation activated a snapshot: %+v", doc.Snapshots)
	}
}

// msgRotated is the Node's log message of a rotated URL secret.
const msgRotated = "state store url rotated: a new driver serves the connection"

// TestCacheConnectionCarriesCacheTraffic checks that stateStore.cache
// carries the Response Cache's traffic: lookups and stores reach the
// cache connection's deployment, the Rate Limit's calls the main one, and
// the Node warns of a Response Cache sharing the limit store only once a
// Revision drops the cache connection.
func TestCacheConnectionCarriesCacheTraffic(t *testing.T) {
	e := newEnv(t, envOptions{stores: []string{"cache"}, config: memoryConfig(), upstream: cacheable()})
	docs := []string{rateLimit("rl", 1000, "1m", "", ""), policy("cache", "cache", "config: {}"), route("mixed", "/mixed", "rl", "cache")}
	split := bundle{stateStore: redisStore("standalone", urlRef, cacheBlock("standalone", cacheURLRef)...), upstream: e.up.Addr(), docs: docs}
	e.start(split.render(), map[string]string{urlRef: "main", cacheURLRef: "cache"})
	main, cache := e.meter("main"), e.meter("cache")
	mainBefore, cacheBefore := main.read(), cache.read()
	expect(t, "a miss", e.get("/mixed/item", "x-client", "c"), http.StatusOK, "")
	e.waitFor("the store", func() bool { return answered(cache.since(cacheBefore).log, "EVALSHA") >= 2 })
	expect(t, "a hit", e.get("/mixed/item", "x-client", "c"), http.StatusOK, "")
	expectCommands(t, "the main connection", main.since(mainBefore), evalsha(2), 2)
	got := cache.since(cacheBefore).requestPath
	if got["EVALSHA"] != 2 || got["HMGET"] != 2 || got["TIME"] != 2 || got["GET"] < 1 {
		t.Errorf("the cache connection got %s, want two lookups and the store's two scripts", delta{requestPath: got}.commands())
	}
	if n := e.logCount(msgShared); n != 0 {
		t.Errorf("the Node warned %d times of a shared limit store with stateStore.cache set", n)
	}
	shared := split
	shared.stateStore = redisStore("standalone", urlRef)
	e.reload(shared.render())
	if n := e.logCount(msgShared); n != 1 {
		t.Errorf("the Node warned %d times of the Response Cache sharing the limit store, want once", n)
	}
}

// msgShared is the Node's warning of a Response Cache sharing the State
// Store that holds limit keys.
const msgShared = "the Response Cache shares the State Store that holds Rate Limit and Quota keys"

// TestReadOnlyAfterDemotion checks a standalone endpoint whose primary a
// failover demoted: the call that meets READONLY fails (RZ-STS-002) and
// closes the shard's connections, whose redials reach the new primary
// behind the endpoint and load the scripts there.
func TestReadOnlyAfterDemotion(t *testing.T) {
	e := newEnv(t, envOptions{replica: true})
	e.start(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef)), mainURLs())
	m := e.meter("main")
	e.warmUp("/limited", "x-client", "d")
	s := e.stores["main"]
	old := s.Deployment().Primary()
	if err := s.Promote(e.t.Context()); err != nil {
		t.Fatal(err)
	}
	promoted := s.Deployment().Primary()
	host, port := splitAddr(t, promoted.Addr())
	if _, err := old.Do(e.t.Context(), "REPLICAOF", host, port); err != nil {
		t.Fatalf("demote the old primary: %v", err)
	}
	before := m.read()
	expect(t, "the call that meets READONLY", e.get("/limited", "x-client", "d"), http.StatusServiceUnavailable, "RZ-STS-002")
	if n := m.since(before).errors["READONLY"]; n != 1 {
		t.Errorf("READONLY replies = %d, want 1", n)
	}
	loads := commandStats(e, promoted)["SCRIPT LOAD"]
	var r response
	e.waitFor("a call on the new primary", func() bool {
		e.advance(time.Second)
		r = e.get("/limited", "x-client", "d")
		return r.status == http.StatusOK
	})
	if got := commandStats(e, promoted); got["SCRIPT LOAD"] <= loads || got["EVALSHA"] == 0 {
		t.Errorf("the new primary got %d script loads and %d EVALSHA calls after the demotion", got["SCRIPT LOAD"]-loads, got["EVALSHA"])
	}
}

// splitAddr splits a "host:port" address.
func splitAddr(t *testing.T, addr string) (string, string) {
	t.Helper()
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i], addr[i+1:]
		}
	}
	t.Fatalf("address %q has no port", addr)
	return "", ""
}
