// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build compose

package compose

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli"
	"github.com/ravindu-rev/ruralz/internal/statestore/statestoretest/redisserver"
	"github.com/ravindu-rev/ruralz/internal/telemetry/accesslog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/test/compose/mock"
)

// What the REST project's mock Upstream answers: the orders and inventory
// services of the example, with an order every Route reads.
const (
	orderID        = "42"
	orderJSON      = `{"id":"42","status":"shipped","total":12.5,"internalNote":"stays upstream"}`
	stockJSON      = `{"items":[{"sku":"A-1","quantity":2}]}`
	partnerJSON    = `{"id":"42","status":"shipped"}`
	idpKeyID       = "compose"
	jwtSubject     = "compose-client"
	ordersReadRole = "orders:read"
)

// restRoutes are the mock Upstream's answers.
func restRoutes() mock.Routes {
	return mock.Routes{
		Default: mock.Answer{Status: http.StatusNotFound, Body: "no such route\n"},
		Paths: map[string]mock.Answer{
			"/healthz":                      {Body: "ok\n"},
			"/orders/" + orderID:            {ContentType: "application/json", Body: orderJSON},
			"/stock":                        {ContentType: "application/json", Body: stockJSON},
			"/partner/v1/orders/" + orderID: {ContentType: "application/json", Body: partnerJSON},
		},
	}
}

// restFixture is the REST project's directory, written before the project
// starts.
type restFixture struct {
	run               string
	bundleSrc, envs   string
	rendered          string
	tokenFile, caFile string
	// secretRoot is the secret root the render's file references name:
	// nodeSecretRoot, where the project mounts secrets/.
	secretRoot string
	key        *mockidp.Key
	captured   []capture
}

// capture is output the suite scans for the canaries at the end.
type capture struct {
	source string
	data   []byte
}

// newRESTFixture writes the project's files and renders the Bundle with
// the CLI under test: the copy of the example with the compose
// Environment's overlay, the secret root, the admin credentials, the CA
// bundle, the mock Upstream's answers, the mock IdP's TLS files, signing
// key and client secret, and the Collector's TLS files and output
// directory.
func newRESTFixture(t *testing.T, s *suite, run string, ca testPKI, sec secrets) *restFixture {
	t.Helper()
	f := &restFixture{run: run, secretRoot: nodeSecretRoot}
	f.bundleSrc, f.envs = restSource(t, s.root, run)
	mkShared(t, filepath.Join(run, "bundle"))
	f.rendered = filepath.Join(run, "bundle", "ruralz.yaml")
	code, stdout, stderr := s.runCLIBinary(t, "bundle", "render", "--env", composeEnv, "--environments", f.envs, "--output-file", f.rendered, f.bundleSrc)
	f.capture("ruralz bundle render", stdout+stderr)
	if code != cli.ExitOK {
		t.Fatalf("ruralz bundle render --env %s: exit %d: %s", composeEnv, code, stderr)
	}
	data, err := os.ReadFile(f.rendered)
	if err != nil {
		t.Fatal(err)
	}
	writeShared(t, f.rendered, data)

	secretsDir := filepath.Join(run, "secrets")
	ca.writeLeaf(t, filepath.Join(secretsDir, "tls"), shopHost)
	writeShared(t, filepath.Join(secretsDir, strings.TrimPrefix(clientSecretRef, nodeSecretRoot+"/")), []byte(sec.get("client secret")))
	writeShared(t, filepath.Join(secretsDir, strings.TrimPrefix(consumerKeyRef, nodeSecretRoot+"/")), []byte(sec.get("API key")))
	nodeFixture(t, run, ca, sec.get("admin token"), "127.0.0.1", "localhost")
	f.tokenFile = filepath.Join(run, "admin", "token")
	f.caFile = filepath.Join(run, "ca", "ca-certificates.crt")
	writeShared(t, f.caFile, ca.ca.CertPEM)
	writeRoutes(t, run, restRoutes())

	idp := filepath.Join(run, "idp")
	ca.writeLeaf(t, idp, idpHost, svcIdP)
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	writeShared(t, filepath.Join(idp, "signing.pem"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	writeShared(t, filepath.Join(idp, "client-secret"), []byte(sec.get("client secret")))
	if f.key, err = mockidp.NewKey(mockidp.ES256, idpKeyID, priv); err != nil {
		t.Fatal(err)
	}

	ca.writeLeaf(t, filepath.Join(run, "collector", "tls"), svcCollector)
	mkShared(t, filepath.Join(run, "collector", "out"))
	return f
}

func (f *restFixture) capture(source, data string) {
	f.captured = append(f.captured, capture{source: source, data: []byte(data)})
}

// token mints a JWT of the example's issuer for audience shop-api with
// scope.
func (f *restFixture) token(t *testing.T, scope string) string {
	t.Helper()
	now := time.Now()
	tok, err := f.key.Sign(nil, map[string]any{
		"iss": issuer, "aud": audience, "sub": jwtSubject, "scope": scope,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// collectorFile reads one of the Collector's files; one not written yet
// reads as empty.
func (f *restFixture) collectorFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.run, "collector", "out", name)) //nolint:gosec // G304: a file the test's Collector writes
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return data
}

// restNode is the addresses the runner reaches the project on.
type restNode struct {
	admin, http, https  string
	upstreamCtl, idpCtl string
}

// traceContext is a sampled W3C trace context the suite sends, so the
// Node exports the request's spans.
type traceContext struct{ traceID, parentID string }

func newTraceContext() traceContext {
	return traceContext{traceID: randomHex(16), parentID: randomHex(8)}
}

func (c traceContext) header() string { return "00-" + c.traceID + "-" + c.parentID + "-01" }

// TestRESTExampleBundle serves the REST example Bundle, which the CLI on
// the runner rendered with the compose Environment, from the image beside
// a Valkey 9 State Store, the mock Upstream and IdP and an OpenTelemetry
// Collector: the effective chains ruralz bundle render --effective
// --route prints are the example's, and the image serves them, Phase by
// Phase in the spans the Collector receives; every Policy's effect shows
// in the answers and at the mock Upstream; spans, metrics and logs reach
// the Collector; ruralz bundle diff reads /config/dump over admin TLS and
// shows exactly a changed field, against the admin URL and a saved dump;
// and no secret leaves its destinations.
func TestRESTExampleBundle(t *testing.T) {
	s := requireSuite(t)
	ctx := t.Context()
	sec := newSecrets(t, "admin token", "client secret", "API key", "State Store password")
	p := s.newProject(t, "rest", restFile, map[string]string{
		varValkeyImage:    redisserver.ImageValkey9,
		varValkeyPassword: sec.get("State Store password"),
		varCollectorImage: s.collectorImage,
		varUID:            strconv.Itoa(os.Getuid()),
		varGID:            strconv.Itoa(os.Getgid()),
	})
	ca := newTestPKI(t)
	f := newRESTFixture(t, s, p.run, ca, sec)

	p.up(ctx, svcValkey, svcUpstream, svcIdP, svcCollector, svcRuralzd)
	n := restNode{
		admin: p.port(ctx, svcRuralzd, portAdmin), http: p.port(ctx, svcRuralzd, portHTTP), https: p.port(ctx, svcRuralzd, portHTTPS),
		upstreamCtl: p.port(ctx, svcUpstream, portControl), idpCtl: p.port(ctx, svcIdP, portControl),
	}
	restChecks(t, s, f, ca, sec, n)

	p.checkHardening(ctx, svcRuralzd)
	p.stop(ctx, svcRuralzd)

	f.capture("ruralzd logs", p.logs(ctx, svcRuralzd))
	f.scanCaptures(t, sec)
}

// restChecks runs the REST project's checks against a Node serving the
// fixture's render at n: the effective chains, the traced requests and
// their spans, what reached the mock Upstream, the refusals, the signals
// at the Collector and ruralz bundle diff against /config/dump. It ends
// with the Collector's files and the mock Upstream's log captured for the
// leak scan.
func restChecks(t *testing.T, s *suite, f *restFixture, ca testPKI, sec secrets, n restNode) {
	t.Helper()
	waitReady(t, ca.client(""), n.admin)

	rows := map[string][]effectiveRow{}
	t.Run("the effective chains are the example's", func(t *testing.T) {
		for _, route := range []string{routeSummary, routePartner} {
			code, got, stderr := s.runCLIBinary(t, "bundle", "render", "--env", composeEnv, "--environments", f.envs, "--effective", "--route", route, "--output", "json", f.bundleSrc)
			f.capture("ruralz bundle render --effective --route "+route, got+stderr)
			if code != cli.ExitOK {
				t.Fatalf("render --effective --route %s: exit %d: %s", route, code, stderr)
			}
			code, want, stderr := s.runCLIBinary(t, "bundle", "render", "--effective", "--route", route, "--output", "json", filepath.Join(s.root, filepath.FromSlash(restBundleDir)))
			if code != cli.ExitOK {
				t.Fatalf("render --effective --route %s of the example: exit %d: %s", route, code, stderr)
			}
			if got != want {
				t.Errorf("route %s: the rendered chain\n%s\nis not the example's\n%s", route, got, want)
			}
			rows[route] = decodeRows(t, got)
		}
	})

	partnerURL := "https://" + n.https + "/partner/v1/orders/" + orderID
	summaryURL := "http://" + n.http + "/v1/orders/" + orderID + "/summary"
	partnerHeader := func() http.Header {
		return http.Header{"Host": {shopHost}, "X-Api-Key": {sec.get("API key")}}
	}
	summaryHeader := func(t *testing.T) http.Header {
		return http.Header{"Host": {shopHost}, "Authorization": {"Bearer " + f.token(t, ordersReadRole)}, "Origin": {partnerOrigin}}
	}
	// Both Routes answer before the traced requests: the Node holds the
	// mock IdP's keys and the upstream token, whichever service started
	// last, and the first metric push proves the export to the Collector.
	t.Run("both Routes answer and the Collector receives the Node's metrics", func(t *testing.T) {
		waitFor(t, readyTimeout, "partner-orders answering 200", func() error {
			return answers(t, ca.client(shopHost), partnerURL, partnerHeader(), http.StatusOK)
		})
		waitFor(t, readyTimeout, "orders-summary answering 200", func() error {
			return answers(t, ca.client(""), summaryURL, summaryHeader(t), http.StatusOK)
		})
		waitFor(t, signalTimeout, "a ruralzd metric push", func() error {
			points, err := decodeMetrics(f.collectorFile(t, "metrics.jsonl"))
			if err != nil {
				return err
			}
			for _, m := range points {
				if m.Name == catalog.HTTPRequestsTotal && m.Service == "ruralzd" {
					return nil
				}
			}
			return fmt.Errorf("no %s from ruralzd among %d metrics", catalog.HTTPRequestsTotal, len(points))
		})
	})

	partner, summary := newTraceContext(), newTraceContext()
	t.Run("the image serves the chains the CLI prints", func(t *testing.T) {
		if len(rows) != 2 {
			t.Skip("the effective chains were not printed")
		}
		h := partnerHeader()
		h.Set("Traceparent", partner.header())
		ex := get(t, ca.client(shopHost), partnerURL, h)
		if ex.status != http.StatusOK || string(ex.body) != partnerJSON || ex.header.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Errorf("GET partner-orders over TLS = %d %q sts %q, want 200 %s with headers-security's header", ex.status, ex.body, ex.header.Get("Strict-Transport-Security"), partnerJSON)
		}
		h = summaryHeader(t)
		h.Set("Traceparent", summary.header())
		checkSummary(t, get(t, ca.client(""), summaryURL, h))
		for _, c := range []struct {
			route string
			tc    traceContext
		}{{routePartner, partner}, {routeSummary, summary}} {
			waitFor(t, signalTimeout, "the spans of "+c.route, func() error {
				spans, err := decodeSpans(f.collectorFile(t, "traces.jsonl"))
				if err != nil {
					return err
				}
				trace := traceSpans(spans, c.tc.traceID)
				for _, s := range trace {
					if s.Service != "ruralzd" {
						return fmt.Errorf("span %s of service %q, want ruralzd", s.Name, s.Service)
					}
				}
				return compareChain(rows[c.route], trace, c.tc.parentID)
			})
		}
		checkUpstreamRequests(t, n, partner, summary)
	})

	t.Run("each Policy refuses what it must", func(t *testing.T) {
		checkProblem(t, get(t, ca.client(""), summaryURL, http.Header{"Host": {shopHost}}), http.StatusUnauthorized, "RZ-AUTH-001")
		checkProblem(t, get(t, ca.client(""), summaryURL, http.Header{"Host": {shopHost}, "Authorization": {"Bearer " + f.token(t, "profile")}}), http.StatusForbidden, "RZ-AUTH-010")
		checkProblem(t, get(t, ca.client(""), "http://"+n.http+"/partner/v1/orders/"+orderID, http.Header{"Host": {shopHost}, "X-Api-Key": {"not-the-partner-key"}}), http.StatusUnauthorized, "RZ-AUTH-002")
		pre := preflight(t, ca.client(""), summaryURL)
		if pre.status != http.StatusNoContent || pre.header.Get("Access-Control-Allow-Origin") != partnerOrigin {
			t.Errorf("CORS preflight of orders-summary = %d allow-origin %q, want 204 %s", pre.status, pre.header.Get("Access-Control-Allow-Origin"), partnerOrigin)
		}
	})

	t.Run("access records reach the Collector", func(t *testing.T) {
		// The refusals above match the Gateway's accessLog.when.
		waitFor(t, signalTimeout, "an access record", func() error {
			records, err := decodeLogs(f.collectorFile(t, "logs.jsonl"))
			if err != nil {
				return err
			}
			for _, r := range records {
				if r.EventName == accesslog.EventName && r.Service == "ruralzd" {
					return nil
				}
			}
			return fmt.Errorf("no %s record from ruralzd among %d log records", accesslog.EventName, len(records))
		})
	})

	t.Run("bundle diff reads the Node's /config/dump", func(t *testing.T) {
		checkDiffs(t, s, f, n)
	})

	for _, file := range signalFiles() {
		f.captured = append(f.captured, capture{source: "Collector " + file, data: f.collectorFile(t, file)})
	}
	var log mock.RequestLog
	raw := controlJSON(t, n.upstreamCtl, "/requests", &log)
	if log.Evicted != 0 {
		t.Errorf("the mock Upstream's log evicted %d requests: the leak scan is incomplete", log.Evicted)
	}
	f.captured = append(f.captured, capture{source: "mock Upstream request log", data: raw})
}

// scanCaptures fails the test for every canary in what the fixture
// captured.
func (f *restFixture) scanCaptures(t *testing.T, sec secrets) {
	t.Helper()
	for _, c := range f.captured {
		sec.scan(t, c.source, c.data)
	}
}

// checkSummary checks orders-summary's aggregate: the order step's
// selected fields under order, the stock step's items under stock, the
// partner origin allowed by cors-partner and headers-security's header.
func checkSummary(t *testing.T, ex exchange) {
	t.Helper()
	var body struct {
		Order map[string]any   `json:"order"`
		Stock []map[string]any `json:"stock"`
	}
	if ex.status != http.StatusOK {
		t.Fatalf("GET orders-summary = %d %s, want 200", ex.status, ex.body)
	}
	if err := json.Unmarshal(ex.body, &body); err != nil {
		t.Fatalf("orders-summary body %s: %v", ex.body, err)
	}
	if body.Order["id"] != orderID || body.Order["status"] != "shipped" || body.Order["internalNote"] != nil || len(body.Stock) != 1 || body.Stock[0]["sku"] != "A-1" {
		t.Errorf("orders-summary body %s, want the order's id, status and total under order and the items under stock", ex.body)
	}
	if ex.header.Get("Ruralz-Partial") != "" {
		t.Errorf("orders-summary answered ruralz-partial %q, want every step", ex.header.Get("Ruralz-Partial"))
	}
	if ex.header.Get("Access-Control-Allow-Origin") != partnerOrigin || ex.header.Get("Strict-Transport-Security") != "max-age=31536000" {
		t.Errorf("orders-summary headers %v, want allow-origin %s and headers-security's header", ex.header, partnerOrigin)
	}
}

// checkUpstreamRequests checks what each Upstream leg received: the
// partner leg the token auth.upstream-oauth2 fetched with the client
// secret, the Consumer headers-internal names and no API key; the summary
// legs the same token on orders only and the anonymous Consumer; every
// leg the request's trace.
func checkUpstreamRequests(t *testing.T, n restNode, partner, summary traceContext) {
	t.Helper()
	var log mock.RequestLog
	controlJSON(t, n.upstreamCtl, "/requests", &log)
	var idp mock.IdPLog
	controlJSON(t, n.idpCtl, "/idp", &idp)
	issued := map[string]bool{}
	for _, r := range idp.TokenRequests {
		if r.Status == http.StatusOK && r.ClientID == oauthClientID && r.Scope == oauthScope && r.AccessToken != "" {
			issued[r.AccessToken] = true
		}
	}
	if len(issued) == 0 {
		t.Errorf("the mock IdP issued no token to %s for %s: %+v", oauthClientID, oauthScope, idp.TokenRequests)
	}
	if idp.JWKSFetches == 0 {
		t.Error("the Node never fetched the mock IdP's JWKS")
	}
	legs := func(tc traceContext) map[string]mock.Request {
		out := map[string]mock.Request{}
		for _, r := range log.Requests {
			if strings.Contains(r.Header.Get("Traceparent"), tc.traceID) {
				out[r.Path] = r
			}
		}
		return out
	}
	bearer := func(r mock.Request) bool {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		return ok && issued[tok]
	}
	p := legs(partner)
	if r, ok := p["/partner/v1/orders/"+orderID]; !ok {
		t.Errorf("no partner-orders request at the mock Upstream with the trace %s", partner.traceID)
	} else if !bearer(r) || r.Header.Get("X-Shop-Consumer") != "partner-acme" || r.Header.Get("X-Api-Key") != "" {
		t.Errorf("partner-orders reached the Upstream with %v, want the issued token, x-shop-consumer partner-acme and no x-api-key", r.Header)
	}
	sm := legs(summary)
	if r, ok := sm["/orders/"+orderID]; !ok {
		t.Errorf("no orders step at the mock Upstream with the trace %s", summary.traceID)
	} else if !bearer(r) || r.Header.Get("X-Shop-Consumer") != "anonymous" {
		t.Errorf("the orders step reached the Upstream with %v, want the issued token and x-shop-consumer anonymous", r.Header)
	}
	if r, ok := sm["/stock"]; !ok {
		t.Errorf("no stock step at the mock Upstream with the trace %s", summary.traceID)
	} else if r.RawQuery != "order="+orderID || r.Header.Get("X-Shop-Consumer") != "anonymous" || bearer(r) {
		t.Errorf("the stock step reached the Upstream as %q with %v, want order=%s, x-shop-consumer anonymous and no upstream token", r.RawQuery, r.Header, orderID)
	}
}

// checkProblem checks a refusal: status, an RFC 9457 problem document and
// its RZ code.
func checkProblem(t *testing.T, ex exchange, status int, code string) {
	t.Helper()
	var doc struct {
		Status int    `json:"status"`
		Code   string `json:"code"`
	}
	if err := json.Unmarshal(ex.body, &doc); err != nil || ex.status != status || doc.Code != code ||
		ex.header.Get("Content-Type") != "application/problem+json" {
		t.Errorf("refusal = %d %q %s, want %d application/problem+json with code %s", ex.status, ex.header.Get("Content-Type"), ex.body, status, code)
	}
}

// preflight sends the CORS preflight of a GET from the partner origin.
func preflight(t *testing.T, client *http.Client, url string) exchange {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = shopHost
	req.Header.Set("Origin", partnerOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodGet)
	ex, err := do(client, req)
	if err != nil {
		t.Fatalf("OPTIONS %s: %v", url, err)
	}
	return ex
}

// checkDiffs runs ruralz bundle diff against the Node's /config/dump over
// admin TLS and against a saved ruralz node dump: the render the Node
// serves exits 0, the render with one changed field exits 1 with exactly
// that operation, its path and impact class, in the human and JSON forms,
// and the dump holds secrets only as references.
func checkDiffs(t *testing.T, s *suite, f *restFixture, n restNode) {
	t.Helper()
	adminURL := "https://" + n.admin
	creds := []string{"--admin-token-file", f.tokenFile, "--ca-file", f.caFile}
	data, err := os.ReadFile(f.rendered)
	if err != nil {
		t.Fatal(err)
	}
	changedData, err := changeTimeout(data)
	if err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(f.run, "changed.yaml")
	if err := os.WriteFile(changed, changedData, 0o600); err != nil {
		t.Fatal(err)
	}
	dump := filepath.Join(f.run, "dump.json")
	code, stdout, stderr := s.runCLIBinary(t, append([]string{"node", "dump", "--admin", adminURL, "--output-file", dump}, creds...)...)
	f.capture("ruralz node dump", stdout+stderr)
	if code != cli.ExitOK {
		t.Fatalf("ruralz node dump: exit %d: %s", code, stderr)
	}
	dumped, err := os.ReadFile(dump) //nolint:gosec // G304: a file the test wrote
	if err != nil {
		t.Fatal(err)
	}
	f.capture("/config/dump", string(dumped))
	for _, ref := range []string{consumerKeyRef, clientSecretRef, tlsKeyRef} {
		ref = f.secretRoot + strings.TrimPrefix(ref, nodeSecretRoot)
		if !strings.Contains(string(dumped), ref) {
			t.Errorf("/config/dump lacks the reference %s: secrets show only as references", ref)
		}
	}
	for _, side := range []struct{ name, to, member string }{{"the admin URL", adminURL, "admin"}, {"a saved dump", dump, "dump"}} {
		t.Run(side.name, func(t *testing.T) {
			args := func(extra ...string) []string {
				out := append([]string{"bundle", "diff"}, extra...)
				if side.member == "admin" {
					out = append(out, creds...)
				}
				return out
			}
			code, out, stderr := s.runCLIBinary(t, append(args("--output", "json"), f.rendered, side.to)...)
			f.capture("ruralz bundle diff (unchanged)", out+stderr)
			if code != cli.ExitOK {
				t.Errorf("diff of the served render against %s: exit %d, want 0: %s%s", side.name, code, out, stderr)
			} else {
				for _, p := range emptyDiffJSON([]byte(out)) {
					t.Error(p)
				}
			}
			code, out, stderr = s.runCLIBinary(t, append(args(), changed, side.to)...)
			f.capture("ruralz bundle diff (changed)", out+stderr)
			if code != cli.ExitNegative {
				t.Errorf("diff of the changed render against %s: exit %d, want %d: %s", side.name, code, cli.ExitNegative, stderr)
			}
			for _, p := range timeoutDiffText(out) {
				t.Error(p)
			}
			code, out, stderr = s.runCLIBinary(t, append(args("--output", "json"), changed, side.to)...)
			f.capture("ruralz bundle diff --output json (changed)", out+stderr)
			if code != cli.ExitNegative {
				t.Errorf("diff --output json of the changed render against %s: exit %d, want %d: %s", side.name, code, cli.ExitNegative, stderr)
			}
			for _, p := range timeoutDiffJSON([]byte(out), side.member) {
				t.Error(p)
			}
		})
	}
}

// answers sends a GET and reports a transport error or an answer other
// than status, for a condition poll.
func answers(t *testing.T, client *http.Client, url string, header http.Header, status int) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), requestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header = header.Clone()
	req.Host = header.Get("Host")
	ex, err := do(client, req)
	if err != nil {
		return err
	}
	if ex.status != status {
		return fmt.Errorf("answered %d %s, want %d", ex.status, ex.body, status)
	}
	return nil
}

// waitFor polls cond until it returns nil, failing the test with its last
// error once within has passed.
func waitFor(t *testing.T, within time.Duration, what string, cond func() error) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		err := cond()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s within %v: %v", what, within, err)
		}
		sleepCtx(t.Context(), pollInterval)
	}
}
