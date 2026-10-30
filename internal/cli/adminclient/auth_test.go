// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package adminclient

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminauth"
	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// Tests of the client against the Node's own admin authentication,
// adminauth behind the admin TLS configuration of tlsconf.Admin
// (docs/architecture/08-security-and-identity.md "Admin ports"): the
// operator token and admitted client certificates pass, failures are 401
// problem documents whose requestId names the CLI's request, and TLS
// failures exit 2 before any request reaches the Node.

// requestID is the Node's requestId rule for the test: the trace ID of a
// valid traceparent.
func requestID(r *http.Request) string {
	id, _ := traceIDOf(r.Header.Get("Traceparent"))
	return id
}

// node is an admin port with an operator token whose /config/dump answers
// dumpBody; reached counts the requests adminauth admitted.
type node struct {
	url     string
	token   string // the operator token file
	reached *atomic.Int32
}

// startNode serves the admin port over TLS from p, or over cleartext
// loopback when p is nil.
func startNode(t *testing.T, p *testPKI) node {
	t.Helper()
	tok := tokenFile(t, testToken)
	settings := adminauth.Settings{TokenFile: tok}
	if p != nil {
		settings.TLSDir = p.adminDir
	}
	auth, err := adminauth.New(settings, adminauth.Options{RequestID: requestID})
	if err != nil {
		t.Fatal(err)
	}
	var reached atomic.Int32
	h := auth.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, dumpBody)
	}))
	var srv *httptest.Server
	if p != nil {
		srv = serveTLS(t, p, h)
	} else {
		srv = httptest.NewServer(h)
		t.Cleanup(srv.Close)
	}
	return node{url: srv.URL, token: tok, reached: &reached}
}

// TestNodeAdmitsCredentials checks that the operator token, over TLS or
// from loopback, and an admitted client certificate reach /config/dump.
func TestNodeAdmitsCredentials(t *testing.T) {
	p := newPKI(t)
	tlsNode := startNode(t, p)
	cleartext := startNode(t, nil)
	cases := map[string]Config{
		"token over TLS":      {URL: tlsNode.url, TokenFile: tlsNode.token, CAFile: p.caFile},
		"client certificate":  {URL: tlsNode.url, CAFile: p.caFile, ClientCert: p.clientCert, ClientKey: p.clientKey},
		"token from loopback": {URL: cleartext.url, TokenFile: cleartext.token},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Timeout = DefaultTimeout
			c, _ := newTestClient(t, cfg)
			body, err := c.ConfigDump(t.Context())
			if err != nil || string(body) != dumpBody {
				t.Errorf("ConfigDump = %q, %v", body, err)
			}
		})
	}
}

// TestNodeRefusesCredentials checks the Node's 401 problem documents as
// the client reports them, their requestId the trace ID the client sent.
func TestNodeRefusesCredentials(t *testing.T) {
	p := newPKI(t)
	n := startNode(t, p)
	cases := map[string]struct {
		cfg  Config
		code string
	}{
		"no credential": {Config{URL: n.url, CAFile: p.caFile}, adminauth.CodeMissing},
		"wrong token":   {Config{URL: n.url, CAFile: p.caFile, TokenFile: tokenFile(t, rotatedToken)}, adminauth.CodeInvalid},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			c.cfg.Timeout = DefaultTimeout
			cl, _ := newTestClient(t, c.cfg)
			_, err := cl.ConfigDump(t.Context())
			pe, ok := errors.AsType[*ProblemError](err)
			if !ok || pe.Status != http.StatusUnauthorized || pe.Doc.Code != c.code || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Fatalf("ConfigDump = %v, want 401 %s", err, c.code)
			}
			// The first traceparent of a test client: trace ID 01 to 10.
			if pe.Doc.RequestID != "0102030405060708090a0b0c0d0e0f10" {
				t.Errorf("requestId %q, want the trace ID the client sent", pe.Doc.RequestID)
			}
			if want := "401 " + c.code + " Unauthorized (requestId 0102030405060708090a0b0c0d0e0f10)"; err.Error() != want {
				t.Errorf("ConfigDump = %q, want %q", err, want)
			}
		})
	}
	if got := n.reached.Load(); got != 0 {
		t.Errorf("%d refused requests reached the handler", got)
	}
}

// TestNodeTLSFailures checks that a server certificate the client cannot
// verify and a client certificate the Node does not admit end the call,
// exit 2, without a problem document and before the handler.
func TestNodeTLSFailures(t *testing.T) {
	p := newPKI(t)
	n := startNode(t, p)
	serverLeaf, err := p.ca.Server("not-a-client.shop.example")
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath, err := serverLeaf.WriteFiles(t.TempDir(), "server-only")
	if err != nil {
		t.Fatal(err)
	}
	other := newPKI(t)
	cases := map[string]Config{
		"server not trusted":                    {URL: n.url, TokenFile: n.token},
		"server of another CA":                  {URL: n.url, TokenFile: n.token, CAFile: other.caFile},
		"client certificate without clientAuth": {URL: n.url, CAFile: p.caFile, ClientCert: certPath, ClientKey: keyPath},
		"client certificate of another CA":      {URL: n.url, CAFile: p.caFile, ClientCert: other.clientCert, ClientKey: other.clientKey},
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			cfg.Timeout = DefaultTimeout
			c, _ := newTestClient(t, cfg)
			_, err := c.ConfigDump(t.Context())
			if err == nil || command.Code(t.Context(), err) != command.ExitNoResult {
				t.Fatalf("ConfigDump = %v, want exit 2", err)
			}
			if _, ok := errors.AsType[*ProblemError](err); ok {
				t.Errorf("ConfigDump = %v, want a TLS failure", err)
			}
		})
	}
	if got := n.reached.Load(); got != 0 {
		t.Errorf("%d requests reached the handler", got)
	}
}
