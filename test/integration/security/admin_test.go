// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"crypto/tls"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The admin port (docs/architecture/08-security-and-identity.md "Admin
// ports"): every path but /healthz and /readyz needs a credential on
// every interface, loopback included: the operator token for every path,
// the metrics token for /metrics, or a client certificate chaining to the
// TLS directory's ca.crt with the clientAuth usage, which admits /metrics
// and, while an operator token is set, the operator paths. Failures are
// 401 problem documents, RZ-AUTH-001 without a credential and RZ-AUTH-002
// for an invalid one, with WWW-Authenticate: Bearer realm="ruralz-admin".
// Token files are re-read on change: a rotation needs no restart, an
// invalid file keeps the last valid token, and removing one revokes its
// token until a valid file returns. /tap and /config/dump use is logged
// with path, peer and credential kind, never the token. A bearer token
// from a non-loopback peer without TLS is refused (netns_test.go).

// adminChallenge is the WWW-Authenticate value of every admin 401.
const adminChallenge = `Bearer realm="ruralz-admin"`

// adminClient returns a client of the Node's admin port trusting the
// harness's CA, presenting leaf when it is not nil, its bodies captured
// for the leak scan.
func adminClient(t *testing.T, top *topology.Topology, leaf *pki.Leaf) *http.Client {
	t.Helper()
	cfg := &tls.Config{RootCAs: top.CA().Pool(), MinVersion: tls.VersionTLS12}
	if leaf != nil {
		cert, err := leaf.TLSCertificate()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Certificates = []tls.Certificate{cert}
	}
	tr := &http.Transport{TLSClientConfig: cfg, DisableKeepAlives: true}
	t.Cleanup(tr.CloseIdleConnections)
	return capturing(top, tr)
}

// wantAdminRefused fails t unless r is the admin 401 with code and the
// Bearer challenge.
func wantAdminRefused(t *testing.T, what string, r reply, code string) {
	t.Helper()
	wantCode(t, what, r, http.StatusUnauthorized, code)
	if got := r.header.Get("WWW-Authenticate"); got != adminChallenge {
		t.Fatalf("%s: WWW-Authenticate %q, want %q", what, got, adminChallenge)
	}
}

func TestAdminPortCredentials(t *testing.T) {
	top := newTopology(t, topology.Config{})
	mock := startMock(t, top, mockup.Config{})
	nodes, err := top.AddNodes(t.Context(), 1, topology.NodeOptions{
		Bundle: bundle(gatewayDoc(webListener), upstreamDoc("backend", mock.Addr(), ""), routeDoc("r", "/", "backend")),
	})
	if err != nil {
		t.Fatal(err)
	}
	n := nodes[0]
	admin := n.AdminURL()
	c := adminClient(t, top, nil)
	operatorPaths := []string{"/config/dump", "/debug/snapshots", "/debug/upstreams", "/debug/pprof/", "/tap"}

	t.Run("open paths", func(t *testing.T) {
		for _, p := range []string{"/healthz", "/readyz"} {
			if r := get(t, c, admin+p); r.status != http.StatusOK {
				t.Errorf("GET %s without a credential = %v, want 200", p, r)
			}
		}
	})
	t.Run("missing and invalid credentials", func(t *testing.T) {
		for _, p := range append([]string{"/metrics"}, operatorPaths...) {
			wantAdminRefused(t, p+" without a credential", get(t, c, admin+p), "RZ-AUTH-001")
			wantAdminRefused(t, p+" with a wrong token", get(t, c, admin+p, bearer(top.Canary())...), "RZ-AUTH-002")
		}
	})
	t.Run("metrics token", func(t *testing.T) {
		if r := get(t, c, admin+"/metrics", bearer(n.MetricsToken())...); r.status != http.StatusOK {
			t.Fatalf("/metrics with the metrics token = %v, want 200", r)
		}
		for _, p := range operatorPaths {
			wantAdminRefused(t, p+" with the metrics token", get(t, c, admin+p, bearer(n.MetricsToken())...), "RZ-AUTH-002")
		}
	})
	t.Run("client certificates", func(t *testing.T) {
		leaf := issue(t, top.CA(), pki.LeafOptions{CommonName: "operator-tool", Client: true})
		cc := adminClient(t, top, leaf)
		for _, p := range []string{"/metrics", "/config/dump"} {
			if r := get(t, cc, admin+p); r.status != http.StatusOK {
				t.Fatalf("GET %s with an admitted client certificate = %v, want 200", p, r)
			}
		}
		other := issue(t, newCA(t, "Ruralz unknown admin CA"), pki.LeafOptions{CommonName: "intruder", Client: true})
		if r, err := trySend(adminClient(t, top, other), mustRequest(t, admin+"/metrics")); err == nil && r.status == http.StatusOK {
			t.Fatalf("a client certificate of another CA got %v", r)
		}
		serverOnly := issue(t, top.CA(), pki.LeafOptions{CommonName: "no-client-usage", Server: true})
		if r, err := trySend(adminClient(t, top, serverOnly), mustRequest(t, admin+"/metrics")); err == nil {
			t.Fatalf("a client certificate without clientAuth got %v, want the handshake refused", r)
		}
	})
	t.Run("audit records", func(t *testing.T) {
		if r := get(t, c, admin+"/config/dump", bearer(n.OperatorToken())...); r.status != http.StatusOK {
			t.Fatalf("/config/dump = %v", r)
		}
		// The process log writes records off the request path.
		var recs []map[string]any
		waitFor(t, "the admin access record of /config/dump", func() (bool, string) {
			recs = logRecords(n, "admin access", "path", "/config/dump", "credential", "operator")
			return len(recs) > 0, "none logged"
		})
		if peer, _ := recs[len(recs)-1]["peer"].(string); !strings.HasPrefix(peer, "127.0.0.1:") {
			t.Errorf("admin access record peer %q, want the loopback client", peer)
		}
	})
	t.Run("token file rotation", func(t *testing.T) {
		old := n.OperatorToken()
		rotated := top.Canary()
		replaceFile(t, n.TokenFile(), []byte(rotated+"\n"))
		if r := get(t, c, admin+"/config/dump", bearer(rotated)...); r.status != http.StatusOK {
			t.Fatalf("the rotated token = %v, want 200", r)
		}
		wantAdminRefused(t, "the token before the rotation", get(t, c, admin+"/config/dump", bearer(old)...), "RZ-AUTH-002")
		// An invalid file keeps the last valid token.
		replaceFile(t, n.TokenFile(), []byte("short\n"))
		if r := get(t, c, admin+"/config/dump", bearer(rotated)...); r.status != http.StatusOK {
			t.Fatalf("the last valid token after an invalid file = %v, want 200", r)
		}
		// Removing the file revokes the token until a valid file returns.
		if err := os.Remove(n.TokenFile()); err != nil {
			t.Fatal(err)
		}
		wantAdminRefused(t, "the token of a removed file", get(t, c, admin+"/config/dump", bearer(rotated)...), "RZ-AUTH-002")
		// The harness's own token comes back, for its teardown.
		replaceFile(t, n.TokenFile(), []byte(old+"\n"))
		if r := get(t, c, admin+"/config/dump", bearer(old)...); r.status != http.StatusOK {
			t.Fatalf("the token of the restored file = %v, want 200", r)
		}
	})
}

// mustRequest returns GET url.
func mustRequest(t *testing.T, url string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	return req
}
