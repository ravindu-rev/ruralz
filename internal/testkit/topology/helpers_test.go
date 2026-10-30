// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package topology

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/proc"
)

// testBundle is a rendered Bundle with an http and an https listener and
// file, env and keyed secret references, the shapes the test render
// rewrites.
const testBundle = `apiVersion: ruralz/v1alpha1
kind: Gateway
metadata:
  name: gateway
spec:
  listeners:
    - name: http
      protocol: http
      port: 8080
    - name: https
      protocol: https
      port: 8443
      tls:
        certificates:
          - name: main
            certificate: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.crt}}
            privateKey: {secretRef: {provider: file, name: /etc/ruralz/tls/tls.key}}
  admin:
    port: 9901
  stateStore:
    driver: redis
    url: {secretRef: {provider: env, name: RURALZ_STATE_STORE_URL}}
    tls:
      caCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/creds.json, key: ca}}
      clientCertificate: {secretRef: {provider: file, name: /etc/ruralz/redis/creds.json, key: cert}}
      clientKey: {secretRef: {provider: file, name: /etc/ruralz/redis/creds.json, key: key}}
`

// References of testBundle.
const (
	refCert     = "file:/etc/ruralz/tls/tls.crt"
	refKey      = "file:/etc/ruralz/tls/tls.key"
	refStoreURL = "env:RURALZ_STATE_STORE_URL"
	refCredsCA  = "file:/etc/ruralz/redis/creds.json#ca"
	refCredsKey = "file:/etc/ruralz/redis/creds.json#key"
)

// standinCfg is a topology Config of stand-in Nodes on the controllable
// clock.
func standinCfg(t *testing.T) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{
		Binary:    exe,
		TestClock: true,
		Digest:    standinDigest,
		Env:       []string{standinEnv + "=1", standinClockEnv + "=1"},
	}
}

// standinTopology returns a topology of stand-in Nodes; mod adjusts the
// Config first.
func standinTopology(t *testing.T, mod ...func(*Config)) *Topology {
	t.Helper()
	c := standinCfg(t)
	for _, m := range mod {
		m(&c)
	}
	return New(t, c)
}

// nodeOptions serves testBundle with a loopback certificate for the https
// listener.
func nodeOptions(t *testing.T, topo *Topology) NodeOptions {
	t.Helper()
	certPEM, keyPEM, err := topo.ServerCertificate()
	if err != nil {
		t.Fatal(err)
	}
	return NodeOptions{Bundle: []byte(testBundle), Secrets: map[string][]byte{refCert: certPEM, refKey: keyPEM}}
}

// addNodes adds n Nodes or fails the test.
func addNodes(t *testing.T, topo *Topology, n int, o NodeOptions) []*Node {
	t.Helper()
	nodes, err := topo.AddNodes(t.Context(), n, o)
	if err != nil {
		t.Fatalf("AddNodes(%d) = %v", n, err)
	}
	return nodes
}

// get sends a GET through client and returns the response with its body.
func get(t *testing.T, client *http.Client, url string) (*http.Response, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a hang guard only
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s body: %v", url, err)
	}
	return resp, string(body)
}

// clockReading is the time a stand-in's clock showed when it answered.
func clockReading(t *testing.T, resp *http.Response) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339Nano, resp.Header.Get("X-Standin-Now"))
	if err != nil {
		t.Fatalf("X-Standin-Now %q: %v", resp.Header.Get("X-Standin-Now"), err)
	}
	return at
}

// format writes a time as the clock answers it.
func format(at time.Time) string { return at.UTC().Format(clock.CommandTimeLayout) }

// waitLine waits for a line of the Node's process output matching expr.
func waitLine(t *testing.T, n *Node, expr string) []string {
	t.Helper()
	return waitOutput(t, n.Process(), expr)
}

// waitOutput waits for a line of p's output matching expr.
func waitOutput(t *testing.T, p *proc.Process, expr string) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second) // a hang guard only
	defer cancel()
	m, err := p.WaitOutput(ctx, regexp.MustCompile(expr))
	if err != nil {
		t.Fatalf("%s: %v", p.Name(), err)
	}
	return m
}

// recordingTB keeps the errors a topology reports instead of failing the
// test, so a test checks the teardown's leak scan and failure evidence.
type recordingTB struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recordingTB) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recordingTB) Failed() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.errors) > 0 || r.TB.Failed()
}

// recorded returns the errors reported, joined by newlines.
func (r *recordingTB) recorded() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.errors, "\n")
}
