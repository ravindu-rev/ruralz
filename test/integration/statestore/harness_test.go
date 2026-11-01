// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package statestore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/cel"
	"github.com/ravindu-rev/ruralz/internal/config/loader"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/subst"
	"github.com/ravindu-rev/ruralz/internal/filter/builtin/checks"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/promtext"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// Timing bounds of the suite. They bound real-time waits for processes
// and conditions, never an outcome.
const (
	// waitTimeout bounds a condition poll, such as a command reaching the
	// State Store or a metric moving.
	waitTimeout = 20 * time.Second
	// pollEvery spaces the condition polls.
	pollEvery = 20 * time.Millisecond
	// requestTimeout bounds one client request.
	requestTimeout = 20 * time.Second
)

// Secret references of the test Bundles, as the harness keys them
// (topology.NodeOptions.StateStoreURLs, Node.SecretPath).
const (
	urlRef        = "file:/etc/ruralz/state-store-url"
	cacheURLRef   = "file:/etc/ruralz/cache-url"
	otherURLRef   = "file:/etc/ruralz/other-url"
	clusterURLRef = "file:/etc/ruralz/cluster-url"
)

// apiKey is the key the test Consumer presents; apiKeyHash its SHA-256,
// as Consumer apiKeys hold it.
const (
	apiKey     = "rz-statestore-suite-key-0123456789abcdef"                                //nolint:gosec // G101: the suite's test Consumer key, not a credential
	apiKeyHash = "sha256:83d842e9b5cf0fc8a91e413aff032705c0aa4b7e4afd6483174b4f89ce7a136a" //nolint:gosec // G101: the test key's hash
)

// epoch is the clock the digest pipeline renders with; a file-mode Node
// renders without time-dependent values, so any fixed time does.
func epoch() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) }

// revisionDigest returns the topology's digest function: the Revision
// digest the configuration pipeline computes for a one-file Bundle, as a
// file-mode Node loads it without ${VAR} values.
func revisionDigest(t *testing.T) func([]byte) (string, error) {
	t.Helper()
	celc, err := cel.New()
	if err != nil {
		t.Fatal(err)
	}
	p, err := pipeline.New(pipeline.Options{Checks: checks.Table(), CEL: celc, Now: epoch})
	if err != nil {
		t.Fatal(err)
	}
	return func(rendered []byte) (string, error) {
		res, err := p.Run(context.Background(), pipeline.Request{
			Source:    loader.Source{FS: fstest.MapFS{"ruralz.yaml": &fstest.MapFile{Data: rendered}}, Path: "ruralz.yaml"},
			Variables: subst.Map{},
		})
		if err != nil {
			return "", err
		}
		if res.Validated == nil {
			var b strings.Builder
			for _, d := range res.Diagnostics {
				fmt.Fprintf(&b, "\n%s %s: %s", d.Code, d.Path, d.Message)
			}
			return "", fmt.Errorf("the Bundle renders no Revision:%s", b.String())
		}
		return res.Validated.Revision.Digest.String(), nil
	}
}

// allKinds returns every redisserver kind: the process kinds run wherever
// a redis-server is found and the container kinds when
// RURALZ_TEST_STATESTORE selects them; redisserver.Start skips or fails
// the rest as the Integration tests section says.
func allKinds() []redisserver.Kind {
	return slices.Concat(redisserver.FlavorProcess.Kinds(), redisserver.FlavorContainer.Kinds())
}

// topologyName returns the stateStore.topology of k.
func topologyName(k redisserver.Kind) string {
	switch {
	case strings.HasSuffix(string(k), "-cluster"):
		return "cluster"
	case strings.HasSuffix(string(k), "-sentinel"):
		return "sentinel"
	default:
		return "standalone"
	}
}

// counted reports whether a counting RESP proxy carries every command
// the Nodes send a deployment of k: a standalone kind through each Node's
// proxy, a process cluster through the proxies its primaries announce. A
// sentinel names its primary and a container cluster its members by
// address, which no proxy fronts; their commands are counted by INFO
// commandstats deltas instead.
func counted(k redisserver.Kind) bool {
	switch topologyName(k) {
	case "standalone":
		return true
	case "cluster":
		return k.Flavor() == redisserver.FlavorProcess
	default:
		return false
	}
}

// env is one test's topology: a Node, the State Stores it uses and a mock
// Upstream.
type env struct {
	t      *testing.T
	top    *topology.Topology
	kind   redisserver.Kind
	stores map[string]*topology.StateStore
	up     *mockup.Server
	node   *topology.Node
	client *http.Client
}

// envOptions configure an env.
type envOptions struct {
	// kind is the main State Store's kind; default redisserver.Process.
	kind redisserver.Kind
	// stores names extra process State Stores, started standalone beside
	// "main", each with a counting proxy.
	stores []string
	// noStore runs without any State Store (the memory driver).
	noStore bool
	// transport turns off TLS or credentials on the harness's hops.
	transport topology.Transport
	// tlsStore runs the main process State Store with TLS, without a
	// counting proxy.
	tlsStore bool
	// replica adds a replica to a standalone process main State Store.
	replica bool
	// config holds extra redis-server directives of process State Stores.
	config []string
	// upstream is the mock Upstream's default Behavior.
	upstream mockup.Behavior
}

// newEnv starts the topology of a test: the State Stores, the mock
// Upstream and the topology itself; Nodes start with start.
func newEnv(t *testing.T, o envOptions) *env {
	t.Helper()
	set := ruralzd(t)
	if o.kind == "" {
		o.kind = redisserver.Process
	}
	top := topology.New(t, topology.Config{
		Binary: set.Ruralzd, TestClock: set.TestClock, Digest: revisionDigest(t), Transport: o.transport,
	})
	e := &env{t: t, top: top, kind: o.kind, stores: map[string]*topology.StateStore{}}
	if !o.noStore {
		e.stores["main"] = top.StartStateStore("main", topology.StateStoreOptions{
			Kind: o.kind, RESPProxy: counted(o.kind) && !o.tlsStore, Replica: o.replica, Config: o.config,
		})
		for _, name := range o.stores {
			e.stores[name] = top.StartStateStore(name, topology.StateStoreOptions{RESPProxy: true, Config: o.config})
		}
	}
	up, err := top.StartMockUpstream(mockup.Config{Name: "backend", Behavior: o.upstream, LogSize: 1 << 14})
	if err != nil {
		t.Fatal(err)
	}
	e.up = up
	e.client = top.HTTPClient()
	return e
}

// start starts the env's one Node on bundle and waits until it is ready.
func (e *env) start(bundle []byte, urls map[string]string, extra ...func(*topology.NodeOptions)) *topology.Node {
	e.t.Helper()
	o := topology.NodeOptions{Bundle: bundle, StateStoreURLs: urls}
	for _, f := range extra {
		f(&o)
	}
	nodes, err := e.top.AddNodes(e.t.Context(), 1, o)
	if err != nil {
		e.t.Fatal(err)
	}
	e.node = nodes[0]
	return e.node
}

// mainURLs maps the main State Store URL reference to "main".
func mainURLs() map[string]string { return map[string]string{urlRef: "main"} }

// advance moves the Node clock by d.
func (e *env) advance(d time.Duration) {
	e.t.Helper()
	if err := e.top.Advance(e.t.Context(), d); err != nil {
		e.t.Fatalf("advance the Node clock by %v: %v", d, err)
	}
}

// reload swaps the Node's test render for bundle and waits until it is
// active: the watcher sees the swap at its next 1 s poll.
func (e *env) reload(bundle []byte) {
	e.t.Helper()
	r, err := e.node.Reload(e.t.Context(), bundle)
	if err != nil {
		e.t.Fatal(err)
	}
	e.advancing(func() error { return e.node.WaitRevision(e.t.Context(), r.TestDigest) })
}

// advancing runs wait while it moves the Node clock 500 ms every 20 ms of
// real time, so the Node's timers, such as the source poll, fire while
// the harness waits.
func (e *env) advancing(wait func() error) {
	e.t.Helper()
	ctx := e.t.Context()
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(pollEvery)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				if err := e.top.Advance(ctx, 500*time.Millisecond); err != nil {
					return
				}
			}
		}
	}()
	err := wait()
	close(stop)
	<-done
	if err != nil {
		e.t.Fatal(err)
	}
}

// response is one client request's outcome.
type response struct {
	status int
	header http.Header
	body   []byte
	// code is the problem document's code, "" for another body.
	code string
}

// String sums up a response for test messages.
func (r response) String() string {
	if r.code != "" {
		return strconv.Itoa(r.status) + " " + r.code
	}
	return strconv.Itoa(r.status)
}

// get sends GET path to the Node's web listener with the header pairs
// given.
func (e *env) get(path string, header ...string) response {
	e.t.Helper()
	return e.do(http.MethodGet, path, nil, header...)
}

// do sends one request to the Node's web listener.
func (e *env) do(method, path string, body []byte, header ...string) response {
	e.t.Helper()
	r, err := e.send(e.client, method, path, body, header...)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return r
}

// send sends one request to the Node's web listener with client, for at
// most requestTimeout or the client's longer Timeout.
func (e *env) send(client *http.Client, method, path string, body []byte, header ...string) (response, error) {
	ctx, cancel := context.WithTimeout(e.t.Context(), max(requestTimeout, client.Timeout))
	defer cancel()
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.node.URL("web")+path, rd)
	if err != nil {
		return response{}, err
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := client.Do(req)
	if err != nil {
		return response{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return response{}, err
	}
	r := response{status: resp.StatusCode, header: resp.Header, body: b}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		var p struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(b, &p) == nil {
			r.code = p.Code
		}
	}
	return r, nil
}

// warmUps is how many requests a warm-up sends: the State client opens
// each of a shard's two pipelined connections at its first use and picks
// one at random per command, so 30 requests on one key leave one unopened
// with a probability of 2^-29.
const warmUps = 30

// warmUp sends warmUps GET requests to path with the header pairs, each
// answered 200, so the shard of their key holds both connections before a
// phase counts its commands or a fault meets them.
func (e *env) warmUp(path string, header ...string) {
	e.t.Helper()
	for range warmUps {
		expect(e.t, "a warm-up request", e.get(path, header...), http.StatusOK, "")
	}
}

// expect fails the test unless r has status and, when code is set, that
// problem code.
func expect(t *testing.T, what string, r response, status int, code string) {
	t.Helper()
	if r.status != status || r.code != code {
		t.Fatalf("%s: got %v (%s), want %d %s", what, r, bytes.TrimSpace(r.body), status, code)
	}
}

// metricsReuse is how long a Node reuses one metrics collection for later
// scrapes, 1 s (target) of Node time
// (docs/architecture/10-observability.md "Observability principles").
const metricsReuse = time.Second

// metrics scrapes the Node's /metrics with the metrics token, after moving
// the Node clock past the collection a scrape may reuse.
func (e *env) metrics() promtext.Set {
	e.t.Helper()
	e.advance(metricsReuse)
	resp, err := e.node.AdminAs(e.t.Context(), "/metrics", topology.Metrics)
	if err != nil {
		e.t.Fatal(err)
	}
	if resp.Status != http.StatusOK {
		e.t.Fatalf("GET /metrics = %d", resp.Status)
	}
	set, err := promtext.ParseBytes(resp.Body)
	if err != nil {
		e.t.Fatal(err)
	}
	return set
}

// degraded reports whether the Node raises the degraded reason.
func (e *env) degraded(reason string) bool {
	e.t.Helper()
	v, err := e.metrics().Value("ruralz_node_degraded_info", promtext.L("reason", reason))
	return err == nil && v == 1
}

// waitFor polls cond until it holds, failing the test after waitTimeout
// of real time.
func (e *env) waitFor(what string, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		select {
		case <-e.t.Context().Done():
			e.t.Fatalf("waiting for %s: %v", what, e.t.Context().Err())
		case <-time.After(pollEvery):
		}
	}
}

// snapshots returns the Node's /debug/snapshots document.
func (e *env) snapshots() adminapi.Snapshots {
	e.t.Helper()
	resp, err := e.node.Admin(e.t.Context(), "/debug/snapshots")
	if err != nil {
		e.t.Fatal(err)
	}
	var doc adminapi.Snapshots
	if err := json.Unmarshal(resp.Body, &doc); err != nil {
		e.t.Fatalf("/debug/snapshots %q: %v", resp.Body, err)
	}
	return doc
}

// logRecord is one JSON line of the Node's process log.
type logRecord map[string]any

// msg returns the record's message.
func (r logRecord) msg() string {
	s, _ := r["msg"].(string)
	return s
}

// logs returns the Node's process log records so far, every process of
// it included.
func (e *env) logs() []logRecord {
	e.t.Helper()
	files, err := filepath.Glob(filepath.Join(e.node.LogDir(), "*.out"))
	if err != nil {
		e.t.Fatal(err)
	}
	var out []logRecord
	for _, f := range files {
		b, err := os.ReadFile(f) //nolint:gosec // G304: a log file the harness wrote under the Node's directory
		if err != nil {
			e.t.Fatal(err)
		}
		sc := bufio.NewScanner(bytes.NewReader(b))
		sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
		for sc.Scan() {
			var r logRecord
			if json.Unmarshal(sc.Bytes(), &r) == nil {
				out = append(out, r)
			}
		}
	}
	return out
}

// logCount returns how many of the Node's log records carry msg.
func (e *env) logCount(msg string) int {
	e.t.Helper()
	n := 0
	for _, r := range e.logs() {
		if r.msg() == msg {
			n++
		}
	}
	return n
}
