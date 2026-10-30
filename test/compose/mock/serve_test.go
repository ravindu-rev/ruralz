// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mock

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/pki"
)

func TestServeConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		c       serveConfig
		wantErr string
	}{
		{"nothing to serve", serveConfig{control: ":0"}, "at least one of"},
		{"upstream flags without -upstream", serveConfig{dns: ":0", upstreamBody: "x"}, "need -upstream"},
		{"routes and body", serveConfig{upstream: ":0", upstreamRoutes: "r", upstreamBody: "x"}, "not both"},
		{"idp without TLS", serveConfig{idp: ":0", idpCert: "c"}, "-idp-cert and -idp-key"},
		{"idp flags without -idp", serveConfig{dns: ":0", idpIssuer: "https://x"}, "need -idp"},
		{"client without its secret", serveConfig{idp: ":0", idpCert: "c", idpKey: "k", idpClient: "c"}, "go together"},
		{"secret without its client", serveConfig{idp: ":0", idpCert: "c", idpKey: "k", idpClientSecret: "f"}, "go together"},
		{"an upstream", serveConfig{upstream: ":0", upstreamBody: "x", upstreamName: "n"}, ""},
		{"an idp", serveConfig{idp: ":0", idpCert: "c", idpKey: "k", idpClient: "c", idpClientSecret: "f"}, ""},
		{"a sink and its control API", serveConfig{dns: ":0", control: ":0"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.c.validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestUpstreamAnswersAndLogs(t *testing.T) {
	dir := t.TempDir()
	routes := Routes{
		Default: Answer{Status: http.StatusNotFound, Body: "no route\n"},
		Paths: map[string]Answer{
			"/orders/42": {ContentType: "application/json", Body: `{"id":"42"}`, Header: map[string]string{"X-Order": "42"}},
			"/healthz":   {Body: "ok\n"},
		},
	}
	file := writeJSONFile(t, dir, "routes.json", routes)
	p := startTestParts(t, serveConfig{upstream: "127.0.0.1:0", upstreamName: "orders", upstreamRoutes: file, control: "127.0.0.1:0"})
	base := "http://" + p.upstream.Addr()
	tests := []struct {
		name, path, wantType, wantBody string
		wantStatus                     int
	}{
		{"a routed path", "/orders/42", "application/json", `{"id":"42"}`, http.StatusOK},
		{"a routed path ignoring the query", "/healthz?probe=1", "text/plain; charset=utf-8", "ok\n", http.StatusOK},
		{"the default answer", "/elsewhere", "text/plain; charset=utf-8", "no route\n", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, h, body := get(t, http.DefaultClient, base+tt.path, map[string]string{"X-Probe": tt.name})
			if status != tt.wantStatus || h.Get("Content-Type") != tt.wantType || body != tt.wantBody {
				t.Errorf("GET %s = %d %q %q, want %d %q %q", tt.path, status, h.Get("Content-Type"), body, tt.wantStatus, tt.wantType, tt.wantBody)
			}
			if h.Get("X-Mockup-Server") != "orders" {
				t.Errorf("GET %s X-Mockup-Server = %q, want orders", tt.path, h.Get("X-Mockup-Server"))
			}
		})
	}
	if _, h, _ := get(t, http.DefaultClient, base+"/orders/42", nil); h.Get("X-Order") != "42" {
		t.Errorf("routed answer header X-Order = %q, want 42", h.Get("X-Order"))
	}
	var log RequestLog
	controlJSON(t, p, "/requests", &log)
	if len(log.Requests) != len(tests)+1 || log.Evicted != 0 {
		t.Fatalf("GET /requests = %d requests, %d evicted; want %d, 0", len(log.Requests), log.Evicted, len(tests)+1)
	}
	first := log.Requests[0]
	if first.Seq != 1 || first.Method != http.MethodGet || first.Path != "/orders/42" || first.Header.Get("X-Probe") != tests[0].name {
		t.Errorf("first logged request = %+v, want GET /orders/42 with X-Probe %q", first, tests[0].name)
	}
	if second := log.Requests[1]; second.RawQuery != "probe=1" {
		t.Errorf("second logged request query = %q, want probe=1", second.RawQuery)
	}
}

func TestUpstreamDefaultBody(t *testing.T) {
	p := startTestParts(t, serveConfig{upstream: "127.0.0.1:0", upstreamBody: "hello from the mock\n"})
	status, h, body := get(t, http.DefaultClient, "http://"+p.upstream.Addr()+"/", nil)
	if status != http.StatusOK || body != "hello from the mock\n" || h.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("GET / = %d %q %q, want 200 text/plain with the -upstream-body", status, h.Get("Content-Type"), body)
	}
}

func TestUpstreamRoutesFile(t *testing.T) {
	dir := t.TempDir()
	tests := []struct{ name, content, wantErr string }{
		{"an unknown member", `{"default":{},"extra":1}`, "unknown field"},
		{"not JSON", `{`, "unexpected EOF"},
		{"a missing file", "", "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(dir, strings.ReplaceAll(tt.name, " ", "-"))
			if tt.content != "" {
				if err := os.WriteFile(file, []byte(tt.content), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := startParts(t.Context(), serveConfig{upstream: "127.0.0.1:0", upstreamRoutes: file})
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("startParts with %s = %v, want an error containing %q", tt.name, err, tt.wantErr)
			}
		})
	}
}

func TestIdPServesKeyAndClient(t *testing.T) {
	dir := t.TempDir()
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("auth.shop.example", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, err := leaf.WriteFiles(dir, "idp")
	if err != nil {
		t.Fatal(err)
	}
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	signing := filepath.Join(dir, "signing.pem")
	if err := os.WriteFile(signing, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	const secret = "s3cret-client-value" //nolint:gosec // G101: a test fixture, not a credential
	secretFile := filepath.Join(dir, "client-secret")
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	p := startTestParts(t, serveConfig{
		idp: "127.0.0.1:0", idpCert: certFile, idpKey: keyFile, idpIssuer: "https://auth.shop.example",
		idpSigningKey: signing, idpAlg: "ES256", idpKID: "compose-key", idpClient: "gateway-edge",
		idpClientSecret: secretFile, idpAudience: "orders", control: "127.0.0.1:0",
	})
	tc, err := pki.ClientTLS(ca, nil)
	if err != nil {
		t.Fatal(err)
	}
	tc.ServerName = "auth.shop.example"
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: tc}, Timeout: 10 * time.Second}
	defer client.CloseIdleConnections()
	base := "https://" + p.idp.Addr()

	status, _, body := get(t, client, base+mockidp.JWKSPath, nil)
	keys, err := mockidp.ParseJWKS([]byte(body))
	if status != http.StatusOK || err != nil || len(keys) != 1 || keys[0].ID != "compose-key" {
		t.Fatalf("GET JWKS = %d %v %+v, want one key compose-key", status, err, keys)
	}

	token := func(id, sec string) (int, string) {
		form := url.Values{"grant_type": {"client_credentials"}, "scope": {"orders.read"}}
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, base+mockidp.TokenPath, strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.SetBasicAuth(id, sec)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var tr struct {
			AccessToken string `json:"access_token"` //nolint:tagliatelle // the OAuth2 wire name
		}
		_ = json.NewDecoder(resp.Body).Decode(&tr)
		return resp.StatusCode, tr.AccessToken
	}
	if status, _ := token("gateway-edge", "wrong"); status != http.StatusUnauthorized {
		t.Errorf("token with a wrong secret = %d, want 401", status)
	}
	status, access := token("gateway-edge", secret)
	if status != http.StatusOK {
		t.Fatalf("token with the client secret = %d, want 200", status)
	}
	_, claims, err := mockidp.Verify(access, keys)
	if err != nil || claims["iss"] != "https://auth.shop.example" || claims["client_id"] != "gateway-edge" || claims["aud"] != "orders" {
		t.Errorf("issued token claims = %v (%v), want iss https://auth.shop.example, client_id gateway-edge, aud orders", claims, err)
	}

	raw := controlBody(t, p, "/idp")
	if bytes.Contains(raw, []byte(secret)) {
		t.Errorf("GET /idp carries the client secret: %s", raw)
	}
	var log IdPLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatal(err)
	}
	if log.JWKSFetches != 1 || len(log.TokenRequests) != 2 {
		t.Fatalf("GET /idp = %+v, want 1 JWKS fetch and 2 token requests", log)
	}
	if last := log.TokenRequests[1]; last.Status != http.StatusOK || last.ClientID != "gateway-edge" ||
		last.AuthMethod != "client_secret_basic" || last.AccessToken != access {
		t.Errorf("last token request = %+v, want the 200 one with its access token", last)
	}
}

func TestIdPRefusesBadFiles(t *testing.T) {
	dir := t.TempDir()
	notPEM := filepath.Join(dir, "not.pem")
	if err := os.WriteFile(notPEM, []byte("text"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca, err := pki.NewCA(pki.CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Server("idp")
	if err != nil {
		t.Fatal(err)
	}
	certFile, keyFile, err := leaf.WriteFiles(dir, "idp")
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		c       serveConfig
		wantErr string
	}{
		{"a certificate that is no key pair", serveConfig{idp: "127.0.0.1:0", idpCert: notPEM, idpKey: keyFile}, "-idp-cert and -idp-key"},
		{"a signing key that is no PEM", serveConfig{idp: "127.0.0.1:0", idpCert: certFile, idpKey: keyFile, idpSigningKey: notPEM, idpAlg: "ES256"}, "no PKCS #8"},
		{"a signing key of another family", serveConfig{idp: "127.0.0.1:0", idpCert: certFile, idpKey: keyFile, idpSigningKey: keyFile, idpAlg: "RS256"}, "cannot sign RS256"},
		{"a missing client secret", serveConfig{idp: "127.0.0.1:0", idpCert: certFile, idpKey: keyFile, idpClient: "c", idpClientSecret: filepath.Join(dir, "none")}, "no such file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := startParts(t.Context(), tt.c); err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("startParts = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestControlDNSAndMissingParts(t *testing.T) {
	p := startTestParts(t, serveConfig{dns: "127.0.0.1:0", control: "127.0.0.1:0"})
	if _, err := sinkResolver(p.dns.Addr()).LookupHost(t.Context(), "updates.example."); err == nil {
		t.Fatal("a lookup through the sink resolved")
	}
	var log DNSLog
	controlJSON(t, p, "/dns", &log)
	if len(log.Queries) == 0 || log.Queries[0].Name != "updates.example." {
		t.Errorf("GET /dns = %+v, want the updates.example. questions", log)
	}
	for _, path := range []string{"/requests", "/idp"} {
		resp := controlGet(t, p, path)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s without its part = %d, want 404", path, resp.StatusCode)
		}
	}
	var health map[string]string
	controlJSON(t, p, "/healthz", &health)
	if health["status"] != "ok" {
		t.Errorf("GET /healthz = %v, want status ok", health)
	}
	if got := p.describe(); !strings.Contains(got, "dns "+p.dns.Addr()) || !strings.Contains(got, "control "+p.ctlAddr) {
		t.Errorf("describe() = %q, want the sink and control addresses", got)
	}
}

func TestStartPartsClosesOnError(t *testing.T) {
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	// The control address is taken, so startParts fails after starting the
	// sink, and must close it again.
	_, err = startParts(t.Context(), serveConfig{dns: "127.0.0.1:0", control: ln.Addr().String()})
	if err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("startParts on a taken control address = %v, want a control error", err)
	}
}

func TestServeCommand(t *testing.T) {
	t.Run("usage errors", func(t *testing.T) {
		for _, args := range [][]string{{}, {"-unknown"}, {"-dns", ":0", "extra"}, {"-idp", ":0"}} {
			var stderr bytes.Buffer
			if got := serve(t.Context(), args, &stderr); got != ExitUsage {
				t.Errorf("serve(%q) = %d, want %d (%s)", args, got, ExitUsage, stderr.String())
			}
		}
	})
	t.Run("help", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := serve(t.Context(), []string{"-h"}, &stderr); got != ExitOK || !strings.Contains(stderr.String(), "-upstream-routes") {
			t.Errorf("serve -h = %d, %q; want 0 and the flags", got, stderr.String())
		}
	})
	t.Run("a part that cannot start", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := serve(t.Context(), []string{"-upstream", "127.0.0.1:0", "-upstream-routes", filepath.Join(t.TempDir(), "none")}, &stderr); got != ExitFailure {
			t.Errorf("serve with a missing Routes file = %d, want %d (%s)", got, ExitFailure, stderr.String())
		}
	})
	t.Run("serves until its context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		stderr := &syncBuffer{}
		done := make(chan int, 1)
		go func() { done <- serve(ctx, []string{"-dns", "127.0.0.1:0"}, stderr) }()
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(stderr.String(), "serving dns ") {
			if time.Now().After(deadline) {
				t.Fatalf("serve did not report serving: %q", stderr.String())
			}
			time.Sleep(10 * time.Millisecond) // a condition poll
		}
		cancel()
		select {
		case got := <-done:
			if got != ExitOK {
				t.Errorf("serve after its context ended = %d, want %d", got, ExitOK)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("serve did not return after its context ended")
		}
	})
}

func TestMain_Dispatch(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, ExitUsage},
		{"an unknown command", []string{"run"}, ExitUsage},
		{"help", []string{"help"}, ExitOK},
		{"probe usage", []string{"probe"}, ExitUsage},
		{"serve usage", []string{"serve"}, ExitUsage},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if got := Main(t.Context(), tt.args, &stdout, &stderr); got != tt.want {
				t.Errorf("Main(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

// startTestParts starts c's parts, closed with the test.
func startTestParts(t *testing.T, c serveConfig) *parts {
	t.Helper()
	p, err := startParts(context.WithoutCancel(t.Context()), c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("Close = %v", err)
		}
	})
	return p
}

// writeJSONFile writes v as JSON to dir/name and returns the path.
func writeJSONFile(t *testing.T, dir, name string, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// get sends a GET with header and returns the status, header and body.
func get(t *testing.T, client *http.Client, u string, header map[string]string) (int, http.Header, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, u, nil)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, resp.Header, string(body)
}

func controlGet(t *testing.T, p *parts, path string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+p.ctlAddr+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func controlBody(t *testing.T, p *parts, path string) []byte {
	t.Helper()
	resp := controlGet(t, p, path)
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("GET %s = %d %q, want 200 application/json", path, resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func controlJSON(t *testing.T, p *parts, path string, v any) {
	t.Helper()
	if err := json.Unmarshal(controlBody(t, p, path), v); err != nil {
		t.Fatal(err)
	}
}

// syncBuffer is a bytes.Buffer safe for one writer and concurrent
// readers.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
