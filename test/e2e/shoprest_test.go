// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/binaries"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
	"github.com/ravindu-rev/ruralz/internal/testkit/topology"
)

// The REST example Bundle on the process harness (scenario 2 of
// docs/engineering/03-testing-and-quality-strategy.md "End-to-end
// tests"): the harness serves a temporary copy of
// examples/shop-rest-bundle/ with the test-owned e2e Environment and its
// overlay from testdata/shop-rest/, whose variables point the Bundle's DNS
// names, JWKS URL and token URL at the harness's mocks and whose overlay
// turns the Consumer's key hash into a canary secretRef. The example and
// its document blocks stay unchanged.

// Names of the REST example Bundle the suite relies on.
const (
	shopRestExample = "shop-rest-bundle"
	shopRestEnv     = "e2e"
	// shopHost is the Routes' host and the https listener's hostname.
	shopHost = "api.shop.example"
	// shopIssuer is jwt-default's issuer, which the mock IdP signs as.
	shopIssuer = "https://auth.shop.example"
	// shopAudience is jwt-default's audience.
	shopAudience = "shop-api"
	// shopClientID is upstream-oauth's clientId.
	shopClientID = "gateway-edge"
	// orderID is the order every case asks for.
	orderID = "42"
)

// Secret references of the REST example Bundle, as the topology keys them
// (internal/secret.Ref's form).
const (
	refTLSCert      = "file:/etc/ruralz/tls/tls.crt"
	refTLSKey       = "file:/etc/ruralz/tls/tls.key"
	refClientSecret = "file:/etc/ruralz/oauth/client-secret" //nolint:gosec // G101: a secretRef name, not a credential
	refPartnerKey   = "file:/etc/ruralz/keys/partner-acme"
	refStateStore   = "env:RURALZ_STATE_STORE_URL"
)

// Mock Upstream answers of the REST example Bundle's Upstreams.
var (
	// orderDoc is what the orders mock answers for /orders/42: the
	// orders-summary step selects id, status and total.
	orderDoc = []byte(`{"id":"42","status":"shipped","total":99.5,"warehouse":"eu-1"}`)
	// partnerOrderDoc is what the orders mock answers for the partner
	// Route's path.
	partnerOrderDoc = []byte(`{"id":"42","status":"shipped","partner":true}`)
	// stockDoc is what the inventory mock answers for /stock: the
	// orders-summary step unwraps its items, a collection, under the
	// group stock.
	stockDoc = []byte(`{"order":"42","items":[{"sku":"tea","quantity":3},{"sku":"cup","quantity":1}]}`)
)

// shopRest is one run of the REST example Bundle: its mocks, its State
// Store, the rendered Bundle and its Nodes.
type shopRest struct {
	t   *testing.T
	top *topology.Topology
	// work holds the Bundle copy, the environments file and the rendered
	// Bundle.
	work, bundleDir, envFile string
	// rendered is ruralz bundle render --env e2e's output, the Bundle the
	// Nodes serve.
	rendered []byte
	// orders and inventory are the mock Upstreams, idp the mock IdP and
	// sink the OTLP collector.
	orders, inventory *mockup.Server
	idp               *mockidp.Server
	sink              *otlpsink.Sink
	store             *topology.StateStore
	// certPEM and keyPEM are the https listener's certificate chain and
	// key, apiKey the partner's API key and clientSecret upstream-oauth's
	// client secret, all carrying canaries.
	certPEM, keyPEM      []byte
	apiKey, clientSecret string
	nodes                []*topology.Node
	ca                   string // the harness CA's file
}

// shopRestOptions adjust a run.
type shopRestOptions struct {
	// nodes is the number of Nodes; default 2, the process harness's
	// file-mode topology.
	nodes int
	// env adds Node process settings.
	env []string
	// now is the mock IdP's clock, which dates the tokens it signs;
	// default the topology's clock, the Nodes'.
	now func() time.Time
}

// startShopRest starts the REST example Bundle's mocks, State Store and
// Nodes for t and waits until every Node is ready.
func startShopRest(t *testing.T, o shopRestOptions) *shopRest {
	t.Helper()
	if o.nodes == 0 {
		o.nodes = 2
	}
	s := newShopRest(t, o)
	var err error
	s.nodes, err = s.top.AddNodes(t.Context(), o.nodes, topology.NodeOptions{
		Bundle:         s.rendered,
		Secrets:        s.secrets(),
		StateStoreURLs: map[string]string{refStateStore: "main"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// secrets returns the values of the REST example Bundle's file secrets.
func (s *shopRest) secrets() map[string][]byte {
	return map[string][]byte{
		refTLSCert:      s.certPEM,
		refTLSKey:       s.keyPEM,
		refClientSecret: []byte(s.clientSecret),
		refPartnerKey:   []byte(s.apiKey),
	}
}

// newShopRest starts the REST example Bundle's mocks and State Store for
// t and renders the Bundle for the e2e Environment, without a Node.
func newShopRest(t *testing.T, o shopRestOptions) *shopRest {
	t.Helper()
	top := newTopology(t, topology.Config{Env: o.env})
	if o.now == nil {
		o.now = top.Now
	}
	s := &shopRest{t: t, top: top, work: t.TempDir()}
	s.ca = caFile(t, top)
	var err error
	// The logs keep every request a run sends, so the leak scan of the
	// mock Upstreams' request logs is complete.
	if s.orders, err = top.StartMockUpstream(mockup.Config{
		Name: "orders", LogSize: 1 << 15, Behavior: mockup.Behavior{Status: http.StatusNotFound},
	}); err != nil {
		t.Fatal(err)
	}
	s.inventory, err = top.StartMockUpstream(mockup.Config{
		Name: "inventory", LogSize: 1 << 15, Behavior: mockup.Behavior{Status: http.StatusNotFound},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range []struct {
		m    *mockup.Server
		path string
		body []byte
	}{
		{s.orders, "/orders/" + orderID, orderDoc},
		{s.orders, "/partner/v1/orders/" + orderID, partnerOrderDoc},
		{s.orders, "/healthz", nil},
		{s.inventory, "/stock", stockDoc},
	} {
		b := mockup.Behavior{Body: r.body, ContentType: "application/json"}
		if r.body == nil {
			b = mockup.Behavior{Body: []byte("ok"), ContentType: "text/plain"}
		}
		if err := r.m.SetRoute(r.path, b); err != nil {
			t.Fatal(err)
		}
	}
	s.store = top.StartStateStore("main", topology.StateStoreOptions{})
	if s.sink, err = top.StartOTLPSink(otlpsink.Config{}); err != nil {
		t.Fatal(err)
	}
	s.clientSecret = top.Canary()
	if s.idp, err = top.StartIdP(mockidp.Config{
		Issuer:  shopIssuer,
		Clients: map[string]string{shopClientID: s.clientSecret},
		Now:     o.now,
	}); err != nil {
		t.Fatal(err)
	}
	ordersHost, ordersPort, err := net.SplitHostPort(s.orders.Addr())
	if err != nil {
		t.Fatal(err)
	}
	s.bundleDir = copyShopRest(t, s.work)
	s.envFile = writeFile(t, s.work, "environments.yaml", environmentFile(t, map[string]string{
		"OTEL_EXPORTER_OTLP_ENDPOINT": s.sink.URL(),
		"E2E_IDP_URL":                 s.idp.URL(),
		"E2E_ORDERS_HOST":             ordersHost,
		"E2E_ORDERS_PORT":             ordersPort,
		"E2E_INVENTORY_ADDRESS":       s.inventory.Addr(),
	}))
	render := cli(t, top, cliOptions{dir: s.work}, "bundle", "render", "--env", shopRestEnv, "--environments", s.envFile, s.bundleDir)
	if render.code != 0 {
		t.Fatalf("render the REST example Bundle: %s", render)
	}
	s.rendered = []byte(render.stdout)
	writeFile(t, s.work, "rendered.yaml", s.rendered)
	if s.certPEM, s.keyPEM, err = top.ServerCertificate(shopHost, "localhost", "127.0.0.1", "::1"); err != nil {
		t.Fatal(err)
	}
	s.apiKey = top.Canary()
	return s
}

// copyShopRest copies examples/shop-rest-bundle/ under dir and adds the
// e2e overlay of testdata/shop-rest/overlays/e2e/, returning the copy's
// root.
func copyShopRest(t testing.TB, dir string) string {
	t.Helper()
	root, err := binaries.RepoRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, shopRestExample)
	copyTree(t, filepath.Join(root, "examples", shopRestExample), dst)
	copyTree(t, filepath.Join("testdata", "shop-rest", "overlays", shopRestEnv), filepath.Join(dst, "overlays", shopRestEnv))
	return dst
}

// copyTree copies the regular files under src to dst.
func copyTree(t testing.TB, src, dst string) {
	t.Helper()
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: a file of the repository tree under test
		if err != nil {
			return err
		}
		writeFile(t, dst, filepath.ToSlash(rel), data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// environmentFile returns testdata/shop-rest/environments.yaml with every
// variable of its e2e Environment set to vars's value; it fails t unless
// vars sets exactly the file's variables, so the file and the suite stay
// in step.
func environmentFile(t testing.TB, vars map[string]string) []byte {
	t.Helper()
	src := readFile(t, filepath.Join("testdata", "shop-rest", "environments.yaml"))
	docs, diags, err := profile.Parse(context.Background(), src, 1, "environments.yaml", profile.FormatYAML, profile.Options{})
	if err != nil || len(diags) > 0 {
		t.Fatalf("parse testdata/shop-rest/environments.yaml: %v %v", err, diags)
	}
	var found []string
	for _, d := range docs {
		meta, _ := d.Root.Get("metadata")
		if name, ok := meta.Get("name"); !ok || name.Text != shopRestEnv {
			continue
		}
		spec, _ := d.Root.Get("spec")
		v, ok := spec.Get("variables")
		if !ok || v.Kind != tree.KindMap {
			t.Fatal("the e2e Environment has no variables")
		}
		for i := range v.Members {
			name := v.Members[i].Key
			value, ok := vars[name]
			if !ok {
				t.Fatalf("the e2e Environment's variable %s has no value in the suite", name)
			}
			v.Members[i].Value = &tree.Node{Kind: tree.KindString, Style: tree.StyleDoubleQuoted, Text: value}
			found = append(found, name)
		}
	}
	for name := range vars {
		if !slices.Contains(found, name) {
			t.Fatalf("the suite sets %s, which the e2e Environment does not declare", name)
		}
	}
	var out bytes.Buffer
	if err := profile.Encode(&out, rootsOf(docs)); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// rootsOf returns the documents' root nodes.
func rootsOf(docs []profile.Document) []*tree.Node {
	out := make([]*tree.Node, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.Root)
	}
	return out
}

// client returns an HTTP client for the Nodes' listeners (clientFor) with
// the https listener's hostname as SNI.
func (s *shopRest) client(source string) *http.Client {
	return clientFor(s.t, s.top, source, shopHost)
}

// listener returns the base URL of a Node's listener, "http" or "https".
func (s *shopRest) listener(n *topology.Node, name string) string {
	u := n.URL(name)
	if u == "" {
		s.t.Fatalf("%s has no listener %s", n.Name(), name)
	}
	return u
}

// tlsConfig returns the client TLS configuration of the https listener:
// the harness's CA and the listener's hostname as SNI.
func (s *shopRest) tlsConfig() *tls.Config {
	c := s.top.ClientTLS()
	c.ServerName = shopHost
	return c
}

// jwt mints a token the mock IdP signs for jwt-default with the scope,
// claims added or removed (nil) by extra.
func (s *shopRest) jwt(scope string, extra map[string]any) string {
	s.t.Helper()
	claims := map[string]any{"sub": "shopper-7", "aud": shopAudience, "scope": scope}
	for k, v := range extra {
		claims[k] = v
	}
	tok, err := s.idp.Mint(mockidp.RS256, claims)
	if err != nil {
		s.t.Fatal(err)
	}
	return tok
}

// fresh moves the topology's clock past every rate-limit window, so a
// step starts with full buckets.
func (s *shopRest) fresh(ctx context.Context) {
	s.t.Helper()
	if err := s.top.Advance(ctx, 2*time.Second); err != nil {
		s.t.Fatal(err)
	}
}

// String names the run for failure messages.
func (s *shopRest) String() string {
	return fmt.Sprintf("REST example Bundle on %d Nodes", len(s.nodes))
}
