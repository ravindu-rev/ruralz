// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The secret leak and redaction test
// (docs/engineering/03-testing-and-quality-strategy.md "Process harness",
// docs/architecture/08-security-and-identity.md "Secrets" rule 2): one
// Node holding every kind of secret M1 resolves, each a canary, under
// traffic that exercises every credential path. Resolved values never
// appear in a Revision, /config/dump, a log, a metric label or /tap, which
// also redacts credential headers; an auth.api-key or auth.basic
// credential never reaches the Upstream. The harness's teardown scans
// every byte it captured, raw and in every encoding canary.Scan knows,
// and fails the test on a finding: the process logs, every admin response,
// the /tap stream, client responses, the mock Upstream's request log, the
// files under RURALZ_DATA_DIR and the State Store keyspace; this test adds
// explicit checks with plain messages.

// tapStream reads /tap until stopped, keeping its lines.
type tapStream struct {
	mu     sync.Mutex
	raw    bytes.Buffer
	events []adminapi.TapEvent
	done   chan struct{}
	err    error
}

// subscribeTap opens /tap on n with the operator token and reads it until
// ctx ends; the bytes it read go to the leak scan.
func subscribeTap(ctx context.Context, t *testing.T, top *topology.Topology, n *topology.Node) *tapStream {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, n.AdminURL()+"/tap", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+n.OperatorToken())
	tr := &http.Transport{TLSClientConfig: top.ClientTLS()}
	t.Cleanup(tr.CloseIdleConnections)
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		t.Fatalf("GET /tap = %d", resp.StatusCode)
	}
	s := &tapStream{done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer func() { _ = resp.Body.Close() }()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Bytes()
			s.mu.Lock()
			s.raw.Write(line)
			s.raw.WriteByte('\n')
			var ev adminapi.TapEvent
			if json.Unmarshal(line, &ev) == nil && ev.Path != "" {
				s.events = append(s.events, ev)
			}
			s.mu.Unlock()
		}
		s.err = sc.Err()
	}()
	t.Cleanup(func() {
		<-s.done
		top.Capture("tap", s.raw.Bytes())
	})
	return s
}

// event returns the first /tap event for path with status, waiting for it.
func (s *tapStream) event(t *testing.T, path string, status int) adminapi.TapEvent {
	t.Helper()
	var found adminapi.TapEvent
	waitFor(t, fmt.Sprintf("the /tap event of %s %d", path, status), func() (bool, string) {
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, ev := range s.events {
			if ev.Path == path && ev.Status == status {
				found = ev
				return true, ""
			}
		}
		return false, fmt.Sprintf("%d events", len(s.events))
	})
	return found
}

func TestSecretLeaks(t *testing.T) {
	top := newTopology(t, topology.Config{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	top.StartStateStore("main", topology.StateStoreOptions{Config: []string{"maxmemory 64mb", "maxmemory-policy noeviction"}})
	idp, err := top.StartIdP(mockidp.Config{Now: top.Now})
	if err != nil {
		t.Fatal(err)
	}
	// The mock Upstream echoes each request back, so a credential the Node
	// forwarded would return to the client too.
	mock := startMock(t, top, mockup.Config{LogSize: 1 << 14, Behavior: mockup.Behavior{Echo: true, Header: http.Header{"Cache-Control": {"max-age=600"}}}})
	apiKey, oauthSecret, password := top.Canary(), top.Canary(), top.Canary()
	secrets := listenerSecrets(t, top)
	secrets["file:/etc/ruralz/keys/app"] = []byte(apiKey)
	secrets[refOAuthSecret] = []byte(oauthSecret)
	b := bundle(
		gatewayDoc(secureListeners+"  stateStore:\n    driver: redis\n    url: "+fileRef("/etc/ruralz/state-store-url")+"\n"),
		upstreamDoc("backend", mock.Addr(), "  policies:\n    - name: oauth\n"),
		// An entry stored from a leg request carrying the upstream token is
		// never replayed, so the cached Route has its own Upstream.
		upstreamDoc("plain", mock.Addr(), ""),
		oauthPolicy("oauth", idp.TokenURL(), rawFileRef("/etc/ruralz/oauth/client-secret")),
		policyDoc("apikey", "auth.api-key", "    header: x-api-key\n"),
		policyDoc("basic", "auth.basic", ""),
		policyDoc("jwt", "auth.jwt", fmt.Sprintf("    issuers:\n      - issuer: %q\n        jwksUrl: %q\n        audiences: [api]\n", idp.Issuer(), idp.JWKSURL())),
		policyDoc("cache", "cache", ""),
		"apiVersion: ruralz/v1alpha1\nkind: Consumer\nmetadata:\n  name: app\nspec:\n  credentials:\n    apiKeys:\n      - name: main\n        secretRef: "+rawFileRef("/etc/ruralz/keys/app")+"\n"+
			"    basic:\n      - username: alice\n        hash: "+basicHash(t, password)+"\n",
		routeDoc("keyed", "/keyed", "backend", "apikey"),
		routeDoc("cached", "/cached", "plain", "cache"),
		routeDoc("basic", "/basic", "backend", "basic"),
		routeDoc("jwt", "/jwt", "backend", "jwt"),
	)
	nodes, err := top.AddNodes(ctx, 1, topology.NodeOptions{
		Bundle: b, Secrets: secrets, StateStoreURLs: map[string]string{refStateStoreURL: "main"}, MACKey: []byte(top.Canary()),
	})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	tap := subscribeTap(ctx, t, top, n)
	c := top.HTTPClient()
	web, secure := n.URL("web"), n.URL("secure")
	k, _ := idp.Key(mockidp.RS256)
	tok, err := idp.MintWith(k, nil, map[string]any{"aud": "api", "sub": "alice"})
	if err != nil {
		t.Fatal(err)
	}

	wantOK(t, "API key over TLS", get(t, c, secure+"/keyed", "x-api-key", apiKey))
	// A cached answer puts a tagged entry, the echo of the request the
	// Node sent upstream, in the State Store keyspace the scan reads.
	waitFor(t, "the cached answer", func() (bool, string) {
		r := get(t, c, web+"/cached")
		return r.header.Get("Age") != "", r.String()
	})
	wantCode(t, "a wrong API key", get(t, c, web+"/keyed", "x-api-key", top.Canary()), http.StatusUnauthorized, "RZ-AUTH-002")
	wantOK(t, "Basic credential", get(t, c, web+"/basic", basicAuth("alice", password)...))
	wantCode(t, "a wrong Basic password", get(t, c, web+"/basic", basicAuth("alice", top.Canary())...), http.StatusUnauthorized, "RZ-AUTH-002")
	wantOK(t, "JWT", get(t, c, web+"/jwt", bearer(tok)...))

	t.Run("credentials never reach the Upstream", func(t *testing.T) {
		for _, r := range mock.Requests() {
			if v := r.Header.Get("x-api-key"); v != "" {
				t.Errorf("the Upstream got x-api-key %q on %s", v, r.Path)
			}
			if v := r.Header.Get("Authorization"); strings.HasPrefix(v, "Basic ") {
				t.Errorf("the Upstream got a Basic credential on %s", r.Path)
			}
		}
		if reqs := idp.TokenRequests(); len(reqs) == 0 || reqs[0].ClientSecret != oauthSecret {
			t.Errorf("the token endpoint, the client secret's destination, got %+v", reqs)
		}
	})
	t.Run("/tap redacts credential headers", func(t *testing.T) {
		// auth.jwt forwards its credential, which /tap shows redacted;
		// auth.api-key and auth.basic strip theirs before the exchange
		// ends, so /tap has none to show.
		for _, c := range []struct {
			path, header string
			stripped     bool
		}{
			{"/keyed", "x-api-key", true}, {"/basic", "authorization", true}, {"/jwt", "authorization", false},
		} {
			ev := tap.event(t, c.path, http.StatusOK)
			got, present := ev.RequestHeaders[c.header]
			switch {
			case present && (len(got) != 1 || got[0] != "[REDACTED]"):
				t.Errorf("/tap %s %s = %q, want [REDACTED]", c.path, c.header, got)
			case !present && !c.stripped:
				t.Errorf("/tap %s shows no %s, want it redacted", c.path, c.header)
			}
		}
		tap.mu.Lock()
		raw := tap.raw.String()
		tap.mu.Unlock()
		for name, v := range map[string]string{"API key": apiKey, "password": password, "token": tok} {
			if strings.Contains(raw, v) {
				t.Errorf("/tap carries the %s", name)
			}
		}
	})
	t.Run("admin surfaces hold references only", func(t *testing.T) {
		values := map[string]string{"API key": apiKey, "client secret": oauthSecret, "operator token": n.OperatorToken(), "metrics token": n.MetricsToken()}
		for _, p := range []string{"/config/dump", "/debug/snapshots", "/debug/upstreams", "/metrics", "/readyz"} {
			resp, err := n.Admin(t.Context(), p)
			if err != nil || resp.Status != http.StatusOK {
				t.Fatalf("GET %s = %v, %v", p, resp.Status, err)
			}
			for name, v := range values {
				if bytes.Contains(resp.Body, []byte(v)) {
					t.Errorf("%s carries the %s", p, name)
				}
			}
		}
		dump, err := n.Admin(t.Context(), "/config/dump")
		if err != nil {
			t.Fatal(err)
		}
		if path, _ := n.SecretPath("file:/etc/ruralz/keys/app"); !bytes.Contains(dump.Body, []byte(path)) {
			t.Errorf("/config/dump does not name the API key's reference %s", path)
		}
	})
	t.Run("process logs", func(t *testing.T) {
		logs := nodeLogs(n)
		for name, v := range map[string]string{"API key": apiKey, "client secret": oauthSecret, "password": password, "token": tok} {
			if strings.Contains(logs, v) {
				t.Errorf("the process log carries the %s", name)
			}
		}
	})
	cancel()
}
