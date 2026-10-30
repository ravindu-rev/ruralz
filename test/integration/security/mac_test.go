// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"net/http"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The State Store entry MAC (docs/architecture/08-security-and-identity.md
// "Secrets" rule 9, docs/architecture/11-scalability-and-distributed-state.md
// "Response Cache layout"): with RURALZ_STATE_STORE_MAC_KEY_FILE set,
// every Response Cache entry a Node stores carries an HMAC-SHA-256 tag,
// and every stored entry records whether it carries one, so a Cell's
// keyed and key-less Nodes on one State Store never serve each other's
// entries, in either direction.

// refStateStoreURL is the secret holding the State Store URL.
const refStateStoreURL = "file:/etc/ruralz/state-store-url"

// cacheBody is what the mock Upstream answers on the cached Route.
const cacheBody = "cacheable answer"

func TestStateStoreEntryMAC(t *testing.T) {
	top := newTopology(t, topology.Config{})
	// A shard admits no Response Cache store while it reports maxmemory
	// 0; limit keys need noeviction.
	store := top.StartStateStore("main", topology.StateStoreOptions{Config: []string{"maxmemory 64mb", "maxmemory-policy noeviction"}})
	mock := startMock(t, top, mockup.Config{LogSize: 1 << 14, Behavior: mockup.Behavior{
		Body: []byte(cacheBody), ContentType: "text/plain", Header: http.Header{"Cache-Control": {"max-age=600"}},
	}})
	b := bundle(
		gatewayDoc(webListener+"  stateStore:\n    driver: redis\n    url: "+fileRef("/etc/ruralz/state-store-url")+"\n"),
		upstreamDoc("backend", mock.Addr(), ""),
		policyDoc("cache", "cache", ""),
		routeDoc("cached", "/mac", "backend", "cache"),
	)
	urls := map[string]string{refStateStoreURL: "main"}
	keyed, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: b, StateStoreURLs: urls, MACKey: []byte(top.Canary())})
	if err != nil {
		t.Fatal(err)
	}
	keyless, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{Bundle: b, StateStoreURLs: urls})
	if err != nil {
		t.Fatal(err)
	}
	c := top.HTTPClient()

	// fetch sends GET path to n and reports whether the cache served it.
	// Every request names one authority, as clients reaching the Nodes
	// through one load balancer do: the URI an entry is stored under
	// carries the authority's port, and each Node listens on its own.
	fetch := func(n *topology.Node, path string) (hit bool) {
		t.Helper()
		before := mock.Stats().Requests
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, n.URL("web")+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "cache.example"
		r := send(t, c, req)
		if r.status != http.StatusOK || string(r.body) != cacheBody {
			t.Fatalf("%s %s = %v, want 200 %q", n.Name(), path, r, cacheBody)
		}
		hit = r.header.Get("Age") != ""
		if forwarded := mock.Stats().Requests > before; forwarded == hit {
			t.Fatalf("%s %s: Age %q but the Upstream was reached %v", n.Name(), path, r.header.Get("Age"), forwarded)
		}
		return hit
	}
	// cached waits until n serves path from the cache.
	cached := func(n *topology.Node, path string) {
		t.Helper()
		waitFor(t, n.Name()+" serving "+path+" from the cache", func() (bool, string) {
			return fetch(n, path), "a miss"
		})
	}
	// stored returns the stored form of the one entry whose saved
	// request target ends in path.
	stored := func(path string) []byte {
		t.Helper()
		srv := store.Deployment().Primary()
		ks, err := srv.Do(t.Context(), "KEYS", keys.PrefixCache+"*")
		if err != nil {
			t.Fatal(err)
		}
		var found [][]byte
		for _, k := range ks.Elems {
			if strings.HasSuffix(k.Str, keys.SuffixGeneration) || strings.HasSuffix(k.Str, keys.SuffixLease) {
				continue
			}
			h, err := srv.Do(t.Context(), "HGETALL", k.Str)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i+1 < len(h.Elems); i += 2 {
				if strings.HasPrefix(h.Elems[i].Str, keys.FieldEntry) && strings.Contains(h.Elems[i+1].Str, path) {
					found = append(found, []byte(h.Elems[i+1].Str))
				}
			}
		}
		if len(found) != 1 {
			t.Fatalf("%d stored entries for %s, want 1", len(found), path)
		}
		return found[0]
	}

	keyedNode, keylessNode := keyed[0], keyless[0]
	cached(keyedNode, "/mac/keyed")
	if e := stored("/mac/keyed"); e[0] != keys.MarkTagged || len(e) <= 1+keys.TagSize {
		t.Fatalf("keyed Node's entry: mark %d, %d bytes; want mark %d and a tag", e[0], len(e), keys.MarkTagged)
	}
	cached(keylessNode, "/mac/keyless")
	if e := stored("/mac/keyless"); e[0] != keys.MarkUntagged {
		t.Fatalf("key-less Node's entry: mark %d, want %d", e[0], keys.MarkUntagged)
	}
	// Each Node reads the other's entry as a miss and serves the
	// Upstream's answer, byte for byte.
	if fetch(keylessNode, "/mac/keyed") {
		t.Fatal("the key-less Node served the keyed Node's entry")
	}
	if fetch(keyedNode, "/mac/keyless") {
		t.Fatal("the keyed Node served the key-less Node's entry")
	}
	for _, n := range []*topology.Node{keyedNode, keylessNode} {
		if got := freshCounter(t, top, n, "ruralz_cache_requests_total", "result", "hit"); got < 1 {
			t.Errorf("%s counted %v hits, want its own entry's", n.Name(), got)
		}
	}
	// Each refusal is a rate-limited warning, written off the request
	// path.
	waitFor(t, "the key-less Node's warning for the tagged entry", func() (bool, string) {
		return len(logRecords(keylessNode, "state store cache entry carries a MAC tag but this Node has no MAC key; served as a miss")) > 0, "none logged"
	})
	waitFor(t, "the keyed Node's warning for the untagged entry", func() (bool, string) {
		return len(logRecords(keyedNode, "state store cache entry failed its MAC check; served as a miss")) > 0, "none logged"
	})
}
