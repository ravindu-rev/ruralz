// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// TestDegradedReasons checks the State Store degraded states a Node
// reports (docs/architecture/10-observability.md "Degraded states"): a
// cleartext, credential-less loopback State Store evicting any key under
// a ratelimit Policy raises state_store_eviction_policy, once the first
// INFO memory reading arrives, state_store_unauthenticated and
// cleartext_hop, with one state_store cleartext hop; an authenticated TLS
// State Store under noeviction raises none of them and counts no
// state_store hop.
func TestDegradedReasons(t *testing.T) {
	t.Run("cleartext unauthenticated evicting", func(t *testing.T) {
		e := newEnv(t, envOptions{
			transport: topology.Transport{NoStateStoreUser: true},
			config:    []string{"maxmemory 1gb", "maxmemory-policy allkeys-lru"},
		})
		e.start(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef)), mainURLs())
		expect(t, "a request", e.get("/limited", "x-client", "s"), http.StatusOK, "")
		for _, reason := range []string{"state_store_eviction_policy", "state_store_unauthenticated", "cleartext_hop"} {
			e.waitFor(reason, func() bool { return e.degraded(reason) })
		}
		if v, err := e.metrics().Value("ruralz_security_cleartext_hops", promtext.L("hop", "state_store")); err != nil || v != 1 {
			t.Errorf("ruralz_security_cleartext_hops{hop=\"state_store\"} = %v (%v), want 1", v, err)
		}
	})
	t.Run("tls authenticated noeviction", func(t *testing.T) {
		e := newEnv(t, envOptions{tlsStore: true, config: memoryConfig()})
		e.start(limitedBundle(e.up.Addr(), redisStore("standalone", urlRef)), mainURLs())
		expect(t, "a request", e.get("/limited", "x-client", "s"), http.StatusOK, "")
		// Two status polls past the first memory reading.
		for range 2 {
			e.metrics()
		}
		for _, reason := range []string{"state_store_eviction_policy", "state_store_unauthenticated"} {
			if e.degraded(reason) {
				t.Errorf("%s raised for an authenticated noeviction State Store", reason)
			}
		}
		if v, err := e.metrics().Value("ruralz_security_cleartext_hops", promtext.L("hop", "state_store")); err != nil || v != 0 {
			t.Errorf("ruralz_security_cleartext_hops{hop=\"state_store\"} = %v (%v), want 0 over TLS", v, err)
		}
	})
}

// refusedURLs returns the State Store URLs a Node refuses with RZ-CFG-042
// (docs/architecture/11-scalability-and-distributed-state.md "State client
// and RZ-STS error codes"), each carrying the canary c, by case name; the
// tls case goes beside a TLS block.
func refusedURLs(c string) map[string]string {
	cred := "ruralz:" + c + "@127.0.0.1:6379"
	return map[string]string{
		"scheme":                "http://" + cred,
		"unread parameter":      "redis://" + cred + "?unread=1",
		"skip_verify":           "rediss://" + cred + "?skip_verify=true",
		"client_cache":          "redis://" + cred + "?client_cache=1",
		"protocol":              "redis://" + cred + "?protocol=2",
		"max_retries":           "redis://" + cred + "?max_retries=1",
		"parameter twice":       "redis://" + cred + "?dial_timeout=1s&dial_timeout=2s",
		"database twice":        "redis://" + cred + "/1?db=2",
		"tls":                   "redis://" + cred,
		"remote without secret": "redis://10.255.255.1:6379?client_name=" + c,
	}
}

// TestStateStoreURLRefusals checks the RZ-CFG-042 refusals: a Node booting
// on a refused URL stays not ready, and a Hot Reload to one is rejected
// while the active Revision keeps serving; /debug/snapshots reports each
// rejection's code.
func TestStateStoreURLRefusals(t *testing.T) {
	e := newEnv(t, envOptions{})
	secrets, refs := refusalSecrets(e)
	good := limitedBundle(e.up.Addr(), redisStore("standalone", urlRef))
	e.start(good, mainURLs(), func(o *topology.NodeOptions) { o.Secrets = secrets })
	active := e.node.TestDigest()
	for name, ref := range refs {
		var extra []string
		if name == "tls" {
			extra = []string{"tls:", "  caCertificate: " + secretRef(caRef)}
		}
		r, err := e.node.Reload(e.t.Context(), limitedBundle(e.up.Addr(), redisStore("standalone", ref, extra...)))
		if err != nil {
			t.Fatal(err)
		}
		want := topology.DisplayRevision(r.TestDigest)
		e.waitFor(name+" refused", func() bool {
			e.advance(500 * time.Millisecond)
			rej := e.snapshots().LastRejected
			return rej != nil && rej.Digest != nil && topology.DisplayRevision(*rej.Digest) == want
		})
		if rej := e.snapshots().LastRejected; rej.Code != "RZ-CFG-042" {
			t.Errorf("%s: the reload was rejected with %s, want RZ-CFG-042", name, rej.Code)
		}
		if err := e.node.WaitRevision(e.t.Context(), active); err != nil {
			t.Errorf("%s: the active Revision changed: %v", name, err)
		}
		expect(t, name+": a request on the active Revision", e.get("/limited", "x-client", "f"), http.StatusOK, "")
	}

	boot := newEnv(t, envOptions{})
	bootSecrets, bootRefs := refusalSecrets(boot)
	n := boot.start(limitedBundle(boot.up.Addr(), redisStore("standalone", bootRefs["skip_verify"])), mainURLs(), func(o *topology.NodeOptions) {
		o.Secrets, o.NoWait = bootSecrets, true
	})
	boot.waitFor("the boot refusal", func() bool {
		boot.advance(500 * time.Millisecond)
		// The admin port answers once the process bound it.
		resp, err := n.Admin(boot.t.Context(), "/debug/snapshots")
		if err != nil {
			return false
		}
		var doc adminapi.Snapshots
		return json.Unmarshal(resp.Body, &doc) == nil && doc.LastRejected != nil && doc.LastRejected.Code == "RZ-CFG-042"
	})
	if _, status, err := n.Readyz(boot.t.Context()); err != nil || status == http.StatusOK {
		t.Errorf("a Node booting on a refused State Store URL answers /readyz %d (%v), want not ready", status, err)
	}
}

// caRef is the file secret of the CA a refused URL's TLS block names.
const caRef = "file:/etc/ruralz/refused-ca.crt"

// refusalSecrets returns the secrets of e's Node holding the refused URLs,
// each carrying a canary of e's topology, and their references by case
// name.
func refusalSecrets(e *env) (map[string][]byte, map[string]string) {
	secrets := map[string][]byte{caRef: e.top.WithCanary(e.top.CA().CertPEM)}
	refs := map[string]string{}
	i := 0
	for name, u := range refusedURLs(e.top.Canary()) {
		ref := fmt.Sprintf("file:/etc/ruralz/refused-%d", i)
		i++
		refs[name] = ref
		secrets[ref] = []byte(u)
	}
	return secrets, refs
}

// msgFallback is the Node's warning of the memory fallback.
const msgFallback = "no State Store is configured: the memory driver multiplies every Rate Limit and Quota by the Node count"

// TestMemoryFallbackWarning checks the memory fallback: without
// stateStore and RURALZ_STATE_STORE_URL a Node serves its Rate Limits
// from the memory driver, logs one warning, which a Hot Reload does not
// repeat, and raises state_store_memory_fallback; with
// RURALZ_STATE_STORE_URL it calls that State Store instead and does
// neither.
func TestMemoryFallbackWarning(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		e := newEnv(t, envOptions{noStore: true})
		e.start(limitedBundle(e.up.Addr(), ""), nil)
		expect(t, "a request on the memory driver", e.get("/limited", "x-client", "m"), http.StatusOK, "")
		e.waitFor("state_store_memory_fallback", func() bool { return e.degraded("state_store_memory_fallback") })
		e.reload(limitedBundle(e.up.Addr(), "", route("other", "/other", "rl")))
		if n := e.logCount(msgFallback); n != 1 {
			t.Errorf("the memory fallback warning was logged %d times, want once", n)
		}
	})
	t.Run("RURALZ_STATE_STORE_URL", func(t *testing.T) {
		e := newEnv(t, envOptions{tlsStore: true})
		srv := e.stores["main"].Deployment().Primary()
		before := commandStats(e, srv)
		e.start(limitedBundle(e.up.Addr(), ""), nil, func(o *topology.NodeOptions) {
			o.Env = append(o.Env, "RURALZ_STATE_STORE_URL="+e.stores["main"].URL())
		})
		expect(t, "a request on the variable's State Store", e.get("/limited", "x-client", "m"), http.StatusOK, "")
		if got := commandStats(e, srv); got["EVALSHA"] <= before["EVALSHA"] {
			t.Error("the Rate Limit did not call the State Store RURALZ_STATE_STORE_URL names")
		}
		if e.degraded("state_store_memory_fallback") || e.logCount(msgFallback) != 0 {
			t.Error("the Node fell back to memory with RURALZ_STATE_STORE_URL set")
		}
	})
}
