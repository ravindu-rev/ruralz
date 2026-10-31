// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && linux

package security

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/problem"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// waitTimeout bounds every real-time condition wait of the suite: what a
// Node does once a timer fired, or what a mock records, happens within
// milliseconds, so the bound only guards against a hang.
const waitTimeout = 30 * time.Second

// Resources of the rendered Bundles the cases build. Each is one YAML
// document; bundle joins them.

// bundle joins resources into one rendered Bundle.
func bundle(resources ...string) []byte {
	return []byte(strings.Join(resources, "---\n"))
}

// gatewayDoc is the Gateway edge with the spec members given as YAML
// lines indented by two spaces under spec.
func gatewayDoc(spec string) string {
	return "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: edge\nspec:\n" + spec
}

// webListener is a cleartext listener named web.
const webListener = "  listeners:\n    - name: web\n      protocol: http\n      port: 8080\n"

// upstreamDoc is the Upstream name with one static Endpoint addr and the
// extra spec lines.
func upstreamDoc(name, addr, extra string) string {
	return fmt.Sprintf("apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: %s\nspec:\n  protocol: http\n  endpoints:\n    - address: %q\n%s", name, addr, extra)
}

// policyDoc is the Policy name of type typ with the config lines indented
// by four spaces under config; an empty config is {}.
func policyDoc(name, typ, config string) string {
	if config == "" {
		return fmt.Sprintf("apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata:\n  name: %s\nspec:\n  type: %s\n  config: {}\n", name, typ)
	}
	return fmt.Sprintf("apiVersion: ruralz/v1alpha1\nkind: Policy\nmetadata:\n  name: %s\nspec:\n  type: %s\n  config:\n%s", name, typ, config)
}

// routeDoc is the Route name for the path prefix, with the Policies
// attached in order, to the Upstream upstream.
func routeDoc(name, prefix, upstream string, policies ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "apiVersion: ruralz/v1alpha1\nkind: Route\nmetadata:\n  name: %s\nspec:\n  match:\n    path:\n      prefix: %q\n", name, prefix)
	if len(policies) > 0 {
		b.WriteString("  policies:\n")
		for _, p := range policies {
			fmt.Fprintf(&b, "    - name: %s\n", p)
		}
	}
	fmt.Fprintf(&b, "  upstreams:\n    - name: %s\n", upstream)
	return b.String()
}

// fileRef is a file secretRef as a flow mapping.
func fileRef(name string) string {
	return fmt.Sprintf("{secretRef: {provider: file, name: %q}}", name)
}

// startMock starts a mock Upstream answering 200 with body, its request
// log sized for the case.
func startMock(t *testing.T, top *topology.Topology, c mockup.Config) *mockup.Server {
	t.Helper()
	if c.Behavior.Body == nil && c.Behavior.BodySize == 0 && !c.Behavior.Echo {
		c.Behavior.Body = []byte("upstream says hello")
		c.Behavior.ContentType = "text/plain"
	}
	m, err := top.StartMockUpstream(c)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// reply is one answer from a Node.
type reply struct {
	status int
	header http.Header
	body   []byte
	// code is the problem document's RZ code, "" for any other body.
	code string
}

func (r reply) String() string {
	if r.code != "" {
		return fmt.Sprintf("%d %s", r.status, r.code)
	}
	return fmt.Sprintf("%d %q", r.status, r.body)
}

// send runs req on c and reads the answer; a transport error fails t.
func send(t *testing.T, c *http.Client, req *http.Request) reply {
	t.Helper()
	r, err := trySend(c, req)
	if err != nil {
		t.Fatalf("%s %s: %v", req.Method, req.URL, err)
	}
	return r
}

// trySend runs req on c and reads the answer.
func trySend(c *http.Client, req *http.Request) (reply, error) {
	resp, err := c.Do(req)
	if err != nil {
		return reply{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return reply{}, err
	}
	r := reply{status: resp.StatusCode, header: resp.Header, body: body}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), problem.ContentType) {
		if doc, err := problem.Decode(body); err == nil {
			r.code = doc.Code
		}
	}
	return r, nil
}

// get sends GET url with the header lines given as name, value pairs.
func get(t *testing.T, c *http.Client, url string, header ...string) reply {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Add(header[i], header[i+1])
	}
	return send(t, c, req)
}

// bearer returns the Authorization header pair of a bearer token.
func bearer(token string) []string { return []string{"Authorization", "Bearer " + token} }

// wantCode fails t unless r is a problem document with status and code.
func wantCode(t *testing.T, what string, r reply, status int, code string) {
	t.Helper()
	if r.status != status || r.code != code {
		t.Fatalf("%s: got %v, want %d %s", what, r, status, code)
	}
}

// wantOK fails t unless r is a 200 from the mock Upstream.
func wantOK(t *testing.T, what string, r reply) {
	t.Helper()
	if r.status != http.StatusOK || r.code != "" {
		t.Fatalf("%s: got %v, want 200 from the Upstream", what, r)
	}
}

// advance moves the topology's clock forward by d.
func advance(t *testing.T, top *topology.Topology, d time.Duration) {
	t.Helper()
	if err := top.Advance(t.Context(), d); err != nil {
		t.Fatalf("advance %v: %v", d, err)
	}
}

// waitFor polls cond until it holds, failing t after waitTimeout of real
// time; cond returns whether it holds and the state to report.
func waitFor(t *testing.T, what string, cond func() (bool, string)) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), waitTimeout)
	defer cancel()
	tick := time.NewTicker(topology.PollInterval)
	defer tick.Stop()
	last := ""
	for {
		ok, state := cond()
		if ok {
			return
		}
		last = state
		select {
		case <-ctx.Done():
			t.Fatalf("%s: not within %v; last: %s", what, waitTimeout, last)
		case <-tick.C:
		}
	}
}

// reuseWindow is how long a Node serves one metrics collection to every
// scrape (1 s target, docs/architecture/10-observability.md): on the
// controllable clock a scrape shows new values only once the clock moved
// past it.
const reuseWindow = time.Second

// freshCounter moves the clock past the metrics reuse window, then
// returns counter's value.
func freshCounter(t *testing.T, top *topology.Topology, n *topology.Node, name string, labels ...string) float64 {
	t.Helper()
	advance(t, top, reuseWindow)
	return counter(t, n, name, labels...)
}

// logRecords returns the Node's process log records with message msg
// whose members match the name, value pairs, in order.
func logRecords(n *topology.Node, msg string, members ...string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(nodeLogs(n), "\n") {
		if !strings.Contains(line, msg) {
			continue
		}
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) != nil || rec["msg"] != msg {
			continue
		}
		match := true
		for i := 0; i+1 < len(members); i += 2 {
			if fmt.Sprint(rec[members[i]]) != members[i+1] {
				match = false
			}
		}
		if match {
			out = append(out, rec)
		}
	}
	return out
}

// scrape returns the Node's /metrics, read with the metrics token. The
// Node reuses one collection for every scrape within reuseWindow of its
// clock (freshCounter).
func scrape(t *testing.T, n *topology.Node) promtext.Set {
	t.Helper()
	resp, err := n.AdminAs(t.Context(), "/metrics", topology.Metrics)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		t.Fatalf("GET /metrics = %d %q", resp.Status, resp.Body)
	}
	set, err := promtext.ParseBytes(resp.Body)
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	return set
}

// counter returns the sum of the samples of name matching the labels.
func counter(t *testing.T, n *topology.Node, name string, labels ...string) float64 {
	t.Helper()
	return scrape(t, n).Sum(name, promtext.L(labels...))
}

// degraded reports whether the Node last logged the degraded reason as
// raised rather than cleared; the process log shows each change at once,
// where a scrape shows it only past the metrics reuse window.
func degraded(n *topology.Node, reason string) bool {
	raised := len(logRecords(n, "degraded state raised", "reason", reason))
	cleared := len(logRecords(n, "degraded state cleared", "reason", reason))
	return raised > cleared
}

// waitDegraded waits until the Node's degraded reason is on or off.
func waitDegraded(t *testing.T, n *topology.Node, reason string, on bool) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s raised %v", reason, on), func() (bool, string) {
		return degraded(n, reason) == on, fmt.Sprintf("raised %v", !on)
	})
}

// waitCounter waits until counter name with the labels reaches want,
// moving the clock past the metrics reuse window before each scrape.
func waitCounter(t *testing.T, top *topology.Topology, n *topology.Node, want float64, name string, labels ...string) {
	t.Helper()
	waitFor(t, fmt.Sprintf("%s%v at %v", name, labels, want), func() (bool, string) {
		got := freshCounter(t, top, n, name, labels...)
		return got == want, fmt.Sprintf("%v", got)
	})
}

// reload swaps the Node's test render for rendered and sends SIGHUP, so
// the Node loads it at once.
func reload(t *testing.T, n *topology.Node, rendered []byte) *topology.Render {
	t.Helper()
	r, err := n.Reload(t.Context(), rendered)
	if err != nil {
		t.Fatal(err)
	}
	if err := n.Signal(t.Context(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	return r
}

// capturing returns a client over tr that follows no redirect and records
// every response body it reads for the topology's leak scan, as the
// topology's own HTTPClient does.
func capturing(top *topology.Topology, tr http.RoundTripper) *http.Client {
	return &http.Client{
		Transport:     captureTransport{base: tr, top: top},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// captureTransport hands each response body it reads to the topology.
type captureTransport struct {
	base http.RoundTripper
	top  *topology.Topology
}

func (c captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := c.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	resp.Body = &capturedBody{rc: resp.Body, top: c.top, source: "client-" + r.URL.Host}
	return resp, nil
}

// capturedBody records what is read from a response body, at Close.
type capturedBody struct {
	rc     io.ReadCloser
	top    *topology.Topology
	source string
	buf    bytes.Buffer
	once   sync.Once
}

func (b *capturedBody) Read(p []byte) (int, error) {
	n, err := b.rc.Read(p)
	b.buf.Write(p[:n])
	return n, err
}

func (b *capturedBody) Close() error {
	err := b.rc.Close()
	b.once.Do(func() { b.top.Capture(b.source, b.buf.Bytes()) })
	return err
}

// nodeLogs returns what the Node's processes wrote to standard output,
// its process log.
func nodeLogs(n *topology.Node) string {
	var b strings.Builder
	for _, p := range n.Processes() {
		out, _ := p.Output()
		b.Write(out)
	}
	return b.String()
}
