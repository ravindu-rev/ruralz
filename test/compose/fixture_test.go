// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/canary"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

// quickstartBody is what the projects' mock Upstream answers the
// quickstart Bundle's Route with, one known line as quickstart.sh's
// Upstream serves.
const quickstartBody = "ruralz compose quickstart\n"

// maxBody bounds the response bodies the suite reads.
const maxBody = 1 << 20

// testPKI is one test's CA: it signs the admin, listener, IdP and
// Collector certificates, and the Nodes and the runner trust it.
type testPKI struct {
	ca *pki.CA
}

func newTestPKI(t *testing.T) testPKI {
	t.Helper()
	ca, err := pki.NewCA(pki.CAOptions{CommonName: "Ruralz Compose Test CA"})
	if err != nil {
		t.Fatal(err)
	}
	return testPKI{ca: ca}
}

// writeLeaf writes a server certificate for hosts and its key as
// dir/tls.crt and dir/tls.key, readable by the containers.
func (p testPKI) writeLeaf(t *testing.T, dir string, hosts ...string) {
	t.Helper()
	leaf, err := p.ca.Server(hosts...)
	if err != nil {
		t.Fatal(err)
	}
	writeShared(t, filepath.Join(dir, "tls.crt"), leaf.ChainPEM)
	writeShared(t, filepath.Join(dir, "tls.key"), leaf.KeyPEM)
}

// client returns an HTTP client trusting the test CA, presenting
// serverName in TLS when it is not empty.
func (p testPKI) client(serverName string) *http.Client {
	tc := &tls.Config{RootCAs: p.ca.Pool(), MinVersion: tls.VersionTLS12, ServerName: serverName}
	return &http.Client{
		Transport: &http.Transport{Proxy: nil, TLSClientConfig: tc, ForceAttemptHTTP2: true},
		Timeout:   requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// secrets are the canaries one test puts where real secrets would be: the
// suite scans what it captured for each before the test ends.
type secrets struct {
	values map[string]canary.Canary
}

func newSecrets(t *testing.T, names ...string) secrets {
	t.Helper()
	s := secrets{values: map[string]canary.Canary{}}
	for _, n := range names {
		c, err := canary.New()
		if err != nil {
			t.Fatal(err)
		}
		s.values[n] = c
	}
	return s
}

// get returns the canary named n.
func (s secrets) get(n string) string { return s.values[n].Value }

// scan fails the test for every canary found in data, which came from
// source.
func (s secrets) scan(t *testing.T, source string, data []byte) {
	t.Helper()
	for name, c := range s.values {
		for _, f := range c.Scan(source, data) {
			t.Errorf("secret %s leaked: %s", name, f)
		}
	}
}

// nodeFixture writes what every ruralzd service mounts: admin/tls for the
// admin port's names and admin/token, the operator token.
func nodeFixture(t *testing.T, run string, p testPKI, token string, adminHosts ...string) {
	t.Helper()
	p.writeLeaf(t, filepath.Join(run, "admin", "tls"), adminHosts...)
	writeShared(t, filepath.Join(run, "admin", "token"), []byte(token))
}

// quickstartFixture writes the quickstart Bundle, unchanged, as bundle/
// and the mock Upstream's answer as mock/routes.json.
func quickstartFixture(t *testing.T, root, run string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(quickstartBundleDir), "ruralz.yaml")) //nolint:gosec // G304: a file of the repository
	if err != nil {
		t.Fatal(err)
	}
	writeShared(t, filepath.Join(run, "bundle", "ruralz.yaml"), data)
	writeRoutes(t, run, mock.Routes{Default: mock.Answer{Body: quickstartBody}})
}

// writeRoutes writes the mock Upstream's answers as mock/routes.json.
func writeRoutes(t *testing.T, run string, r mock.Routes) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	writeShared(t, filepath.Join(run, "mock", "routes.json"), data)
}

// exchange is one HTTP request's outcome.
type exchange struct {
	status int
	header http.Header
	body   []byte
}

// do sends req with client and returns the outcome, or an error.
func do(client *http.Client, req *http.Request) (exchange, error) {
	resp, err := client.Do(req)
	if err != nil {
		return exchange{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return exchange{}, fmt.Errorf("reading the body: %w", err)
	}
	return exchange{status: resp.StatusCode, header: resp.Header, body: body}, nil
}

// get sends a GET with header and fails the test on a transport error.
func get(t *testing.T, client *http.Client, url string, header http.Header) exchange {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	if h := header.Get("Host"); h != "" {
		req.Host = h
	}
	ex, err := do(client, req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	return ex
}

// waitReady polls a Node's /readyz until it answers 200.
func waitReady(t *testing.T, client *http.Client, admin string) {
	t.Helper()
	deadline := time.Now().Add(readyTimeout)
	var last string
	for {
		ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+admin+"/readyz", nil)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		ex, err := do(client, req)
		cancel()
		switch {
		case err != nil:
			last = err.Error()
		case ex.status == http.StatusOK:
			return
		default:
			last = fmt.Sprintf("%d %s", ex.status, strings.TrimSpace(string(ex.body)))
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz at %s not 200 within %v: %s", admin, readyTimeout, last)
		}
		sleepCtx(t.Context())
	}
}

// controlJSON reads a mock's control API path at addr into v.
func controlJSON(t *testing.T, addr, path string, v any) []byte {
	t.Helper()
	ex := get(t, http.DefaultClient, "http://"+addr+path, nil)
	if ex.status != http.StatusOK {
		t.Fatalf("GET %s%s = %d %s", addr, path, ex.status, ex.body)
	}
	if err := json.Unmarshal(ex.body, v); err != nil {
		t.Fatalf("GET %s%s: %v", addr, path, err)
	}
	return ex.body
}

// checkQuickstartAnswer checks the answer of the quickstart Route: the
// mock's known line, and x-quickstart, which only the Route's headers
// Policy adds, so the response passed the Filter Chain.
func checkQuickstartAnswer(t *testing.T, ex exchange) {
	t.Helper()
	if ex.status != http.StatusOK || string(ex.body) != quickstartBody || ex.header.Get("X-Quickstart") != "ruralz" {
		t.Errorf("GET / = %d %q x-quickstart %q, want 200 %q x-quickstart ruralz", ex.status, ex.body, ex.header.Get("X-Quickstart"), quickstartBody)
	}
}
