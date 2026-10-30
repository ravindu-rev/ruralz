// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build e2e

package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/sfv"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
	"github.com/ravindu-rev/ruralz/internal/testkit/mockup"
	"github.com/ravindu-rev/ruralz/internal/testkit/otlpsink"
)

// Scenario 2 of docs/engineering/03-testing-and-quality-strategy.md
// "End-to-end tests": the REST example Bundle serves its Routes with the
// Filter Chains ruralz bundle render --effective --route prints. Per
// Route, the printed JSON equals the document's effective table; the
// ruralz.test.v1 cases of testdata/shop-rest/cases exercise every row,
// allowed and each denial, against both Nodes on both listeners; the
// ruralz.filter spans of the traces the cases sample through traceparent
// follow the printed order per Phase and leg; the mock Upstreams see the
// upstream token and headers; and the rate limits and the quota answer
// with their 429s and charges.

// Trace IDs the cases sample through traceparent, per Route, and the
// parent span ID they name.
const (
	traceOrdersSummary = "4bf92f3577b34da6a3ce929d0e0e4736"
	tracePartnerOrders = "5bf92f3577b34da6a3ce929d0e0e4736"
	traceParentSpan    = "00f067aa0ba902b7"
)

// effectiveRow is one row of ruralz bundle render --effective --output
// json.
type effectiveRow struct {
	Phase       string `json:"phase"`
	Leg         string `json:"leg"`
	Policy      string `json:"policy"`
	Type        string `json:"type"`
	FilterClass string `json:"filterClass"`
	Slot        string `json:"slot"`
	From        string `json:"from"`
	Reason      string `json:"reason"`
}

// chainRow returns the row's Phase, leg, Policy and From.
func (r effectiveRow) chainRow() chainRow {
	return chainRow{Phase: r.Phase, Leg: r.Leg, Policy: r.Policy, From: r.From}
}

// effective returns the effective chain of route as the built ruralz
// prints it for the e2e Environment, in JSON.
func (s *shopRest) effective(t testing.TB, route string) []effectiveRow {
	t.Helper()
	r := cli(t, s.top, cliOptions{dir: s.work}, "bundle", "render", "--env", shopRestEnv, "--environments", s.envFile,
		"--effective", "--route", route, "--output", "json", s.bundleDir)
	if r.code != 0 {
		t.Fatalf("%s", r)
	}
	var rows []effectiveRow
	if err := json.Unmarshal([]byte(r.stdout), &rows); err != nil {
		t.Fatalf("decode the effective chain of %s: %v\n%s", route, err, r.stdout)
	}
	return rows
}

// TestRESTExampleBundle is scenario 2 on two Nodes.
func TestRESTExampleBundle(t *testing.T) {
	s := startShopRest(t, shopRestOptions{})
	t.Run("EffectiveChains", func(t *testing.T) { checkEffectiveChains(t, s) })
	t.Run("Cases", func(t *testing.T) { runShopRestCases(t, s) })
	t.Run("Spans", func(t *testing.T) { checkFilterSpans(t, s) })
	t.Run("UpstreamLegs", func(t *testing.T) { checkUpstreamLegs(t, s) })
	t.Run("RateLimits", func(t *testing.T) { checkRateLimits(t, s) })
	t.Run("Quota", func(t *testing.T) { checkQuota(t, s) })
}

// checkEffectiveChains holds, per Route, the JSON the built ruralz prints
// equal to the configuration model's table: orders-summary runs the
// Worked example chain without geo-block-default, which the REST example
// Bundle leaves out, and partner-orders the REST example Bundle's table.
func checkEffectiveChains(t *testing.T, s *shopRest) {
	for route, want := range map[string][]chainRow{
		"orders-summary": docChain(t, "### Worked example", "geo-block-default"),
		"partner-orders": docChain(t, "## REST example Bundle"),
	} {
		var got []chainRow
		for _, r := range s.effective(t, route) {
			got = append(got, r.chainRow())
		}
		if !slices.Equal(got, want) {
			t.Errorf("ruralz bundle render --effective --route %s --output json rows =\n%v\nwant the document's\n%v", route, got, want)
		}
	}
}

// caseSecrets writes the credentials the shop-rest cases read through
// {file: PATH} beside a copy of the cases and returns the copy's
// directory: tokens the mock IdP signs (scope orders:read, scope profile,
// expired, for another audience, signed by a key the JWKS does not
// publish), the partner's API key and an API key no Consumer holds.
func caseSecrets(t testing.TB, s *shopRest) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cases")
	copyTree(t, filepath.Join("testdata", "shop-rest", "cases"), dir)
	unpublished, err := mockidp.GenerateKey(mockidp.RS256)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := s.idp.MintWith(unpublished, nil, map[string]any{"sub": "shopper-7", "aud": shopAudience, "scope": "orders:read"})
	if err != nil {
		t.Fatal(err)
	}
	now := s.top.Now()
	for name, value := range map[string]string{
		"orders-reader":  "Bearer " + s.jwt("orders:read", nil),
		"profile-reader": "Bearer " + s.jwt("profile", nil),
		"expired":        "Bearer " + s.jwt("orders:read", map[string]any{"iat": now.Add(-2 * time.Hour).Unix(), "exp": now.Add(-time.Hour).Unix()}),
		"other-audience": "Bearer " + s.jwt("orders:read", map[string]any{"aud": "other-api"}),
		"unknown-key":    "Bearer " + foreign,
		"partner-key":    s.apiKey,
		"wrong-api-key":  s.top.Canary(),
	} {
		writeFile(t, dir, "secrets/"+name, []byte(value+"\n"))
	}
	return dir
}

// shopTargets returns the base URLs of every Node's listeners.
func shopTargets(t testing.TB, s *shopRest) []string {
	t.Helper()
	var out []string
	for _, n := range s.nodes {
		out = append(out, s.listener(n, "http"), s.listener(n, "https"))
	}
	return out
}

// runShopRestCases runs the shop-rest cases against both listeners of
// every Node: HTTP/1.1 on http, HTTP/2 over TLS with the Routes' host as
// SNI on https.
func runShopRestCases(t *testing.T, s *shopRest) {
	s.fresh(t.Context())
	runCases(t, caseSecrets(t, s), s.client(""), shopTargets(t, s)...)
}

// checkFilterSpans checks, for each Route, the ruralz.filter spans of the
// requests its cases sampled through traceparent: under each request's
// server span, per Phase and leg (the client leg's spans are the server
// span's children, an upstream leg's the children of its
// ruralz.upstream.<name> span), the Policies run in the order ruralz
// bundle render --effective --route prints; onLog creates no span.
func checkFilterSpans(t *testing.T, s *shopRest) {
	ctx := t.Context()
	requests := len(shopTargets(t, s))
	for route, trace := range map[string]string{"orders-summary": traceOrdersSummary, "partner-orders": tracePartnerOrders} {
		want := map[[2]string][]string{}
		perRequest := 0
		for _, r := range s.effective(t, route) {
			if r.Phase == "none" || r.Phase == "onLog" {
				continue
			}
			k := [2]string{r.Phase, r.Leg}
			want[k] = append(want[k], r.Policy)
			perRequest++
		}
		filterSpans := func() int {
			n := 0
			for _, sp := range s.sink.Trace(trace) {
				if strings.HasPrefix(sp.Name(), "ruralz.filter.") {
					n++
				}
			}
			return n
		}
		// The span processor's batch interval runs on the Node clock.
		err := advancing(ctx, s.top, 500*time.Millisecond, func() error {
			return s.sink.Wait(ctx, func() bool { return filterSpans() >= requests*perRequest })
		})
		if err != nil {
			t.Fatalf("%s: %d ruralz.filter spans in trace %s, want %d: %v\n%s", route, filterSpans(), trace, requests*perRequest, err, s.sink.FormatTree(trace))
		}
		var servers []otlpsink.Span
		for _, sp := range s.sink.Children(trace, traceParentSpan) {
			if sp.Name() == "GET "+route {
				servers = append(servers, sp)
			}
		}
		if len(servers) != requests {
			t.Fatalf("%s: %d server spans under traceparent %s, want %d\n%s", route, len(servers), traceParentSpan, requests, s.sink.FormatTree(trace))
		}
		for _, srv := range servers {
			if got := executedChain(s.sink, trace, srv); !equalChains(got, want) {
				t.Errorf("%s: the spans of server span %s run\n%v\nwant the printed chain\n%v\n%s", route, srv.SpanID(), got, want, s.sink.FormatTree(trace))
			}
		}
	}
}

// executedChain returns the Policies the ruralz.filter spans under the
// server span srv ran, per Phase and leg, in start order.
func executedChain(sink *otlpsink.Sink, trace string, srv otlpsink.Span) map[[2]string][]string {
	out := map[[2]string][]string{}
	add := func(sp otlpsink.Span, leg string) {
		policy, ok := strings.CutPrefix(sp.Name(), "ruralz.filter.")
		if !ok {
			return
		}
		k := [2]string{sp.AttrString(catalog.AttrPhase), leg}
		out[k] = append(out[k], policy)
	}
	for _, sp := range sink.Children(trace, srv.SpanID()) {
		if leg, ok := strings.CutPrefix(sp.Name(), "ruralz.upstream."); ok {
			for _, c := range sink.Children(trace, sp.SpanID()) {
				add(c, leg)
			}
			continue
		}
		add(sp, "client")
	}
	return out
}

// equalChains compares two chains per Phase and leg.
func equalChains(a, b map[[2]string][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if !slices.Equal(v, b[k]) {
			return false
		}
	}
	return true
}

// checkUpstreamLegs checks each Policy's effect at the mock Upstreams and
// the mock IdP after the cases: on the orders leg, upstream-oauth sends a
// token the mock IdP issued and headers-internal names the Consumer
// (anonymous for a token without one, partner-acme for the partner's
// key); on the inventory leg, headers-internal runs without the OAuth
// token; apikey-partner strips the API key; and the IdP saw only the
// client credentials grant of gateway-edge with its secret.
func checkUpstreamLegs(t *testing.T, s *shopRest) {
	issued := map[string]bool{}
	reqs := s.idp.TokenRequests()
	if len(reqs) == 0 {
		t.Fatal("the mock IdP issued no token: upstream-oauth never ran")
	}
	for _, r := range reqs {
		if r.ClientID != shopClientID || r.ClientSecret != s.clientSecret || r.GrantType != "client_credentials" || r.Scope != "orders.read" || r.Status != http.StatusOK {
			t.Errorf("token request: client %q, secret matches %v, grant %q, scope %q, status %d",
				r.ClientID, r.ClientSecret == s.clientSecret, r.GrantType, r.Scope, r.Status)
		}
		issued[r.AccessToken] = true
	}
	bearer := func(h http.Header) string { tok, _ := strings.CutPrefix(h.Get("Authorization"), "Bearer "); return tok }
	for _, c := range []struct {
		path, consumer string
		oauth          bool
		mock           *mockup.Server
	}{
		{"/orders/" + orderID, "anonymous", true, s.orders},
		{"/partner/v1/orders/" + orderID, "partner-acme", true, s.orders},
		{"/stock", "anonymous", false, s.inventory},
	} {
		var seen int
		mock := c.mock
		for _, r := range mock.Requests() {
			if r.Path != c.path {
				continue
			}
			seen++
			if got := r.Header.Get("X-Shop-Consumer"); got != c.consumer {
				t.Errorf("%s %s: x-shop-consumer %q, want %q", mock.Name(), r.URI, got, c.consumer)
			}
			if got := issued[bearer(r.Header)]; got != c.oauth {
				t.Errorf("%s %s: carries an IdP-issued token %v, want %v", mock.Name(), r.URI, got, c.oauth)
			}
			if r.Header.Get("X-Api-Key") != "" {
				t.Errorf("%s %s: the API key reached the Upstream", mock.Name(), r.URI)
			}
			if c.path == "/stock" && r.RawQuery != "order="+orderID {
				t.Errorf("inventory %s: query %q, want the step's order=%s", r.URI, r.RawQuery, orderID)
			}
		}
		if seen == 0 {
			t.Errorf("%s received no request for %s", mock.Name(), c.path)
		}
	}
}

// rateLimit returns the r parameter of the RateLimit item name, -1 when
// the field lists no such item.
func rateLimit(t testing.TB, h http.Header, name string) int64 {
	t.Helper()
	l, err := sfv.ParseList(strings.Join(h.Values("RateLimit"), ", "))
	if err != nil {
		t.Fatalf("RateLimit %q: %v", h.Values("RateLimit"), err)
	}
	for _, m := range l {
		if m.Value.Text() != name {
			continue
		}
		for _, p := range m.Params {
			if p.Key == "r" {
				return p.Value.Int()
			}
		}
	}
	return -1
}

// shopResponse is one answer of a Node.
type shopResponse struct {
	status int
	header http.Header
	code   string // the problem document's code
}

// shopGet sends GET path to base through client with the Routes' host and
// headers, and returns the answer.
func shopGet(t testing.TB, client *http.Client, base, path string, header map[string]string) shopResponse {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = shopHost
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s%s: %v", base, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("GET %s%s: %v", base, path, err)
	}
	out := shopResponse{status: resp.StatusCode, header: resp.Header}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "application/problem+json") {
		var doc struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(body, &doc)
		out.code = doc.Code
	}
	return out
}

// checkRateLimits exercises both ratelimit Policies, each key from a
// loopback source address of its own:
//   - ratelimit-orders, 50 per second keyed on source.ip for a token
//     without a Consumer: one Node's local bucket admits 50 in a window
//     of the Node clock, then answers 429 RZ-RL-001 with Retry-After and
//     the limit's RateLimit item at r=0, while another source is admitted;
//   - ratelimit-global, 1,000 per second per source.ip, on partner-orders:
//     the 1,001st request of a window is 429 RZ-RL-001;
//   - across both Nodes, one key stays within requests plus burst, 100 in
//     a window: GCRA in the State Store denies with RZ-RL-002 once the
//     two Nodes together pass the global limit, or the local buckets with
//     RZ-RL-001 once each Node spent its own.
func checkRateLimits(t *testing.T, s *shopRest) {
	ctx := t.Context()
	n := s.nodes[0]
	token := map[string]string{"Authorization": "Bearer " + s.jwt("orders:read", nil)}
	summary := "/v1/orders/" + orderID + "/summary"

	s.fresh(ctx)
	limited := s.client("127.0.0.2")
	for i := 1; i <= 50; i++ {
		if r := shopGet(t, limited, s.listener(n, "http"), summary, token); r.status != http.StatusOK {
			t.Fatalf("request %d of 50 within ratelimit-orders = %d %s", i, r.status, r.code)
		}
	}
	r := shopGet(t, limited, s.listener(n, "http"), summary, token)
	if r.status != http.StatusTooManyRequests || r.code != "RZ-RL-001" || r.header.Get("Retry-After") == "" || rateLimit(t, r.header, "ratelimit-orders") != 0 {
		t.Errorf("request 51 = %d %s, Retry-After %q, RateLimit %q; want 429 RZ-RL-001 with ratelimit-orders at r=0",
			r.status, r.code, r.header.Get("Retry-After"), r.header.Values("RateLimit"))
	}
	if r := shopGet(t, s.client("127.0.0.3"), s.listener(n, "http"), summary, token); r.status != http.StatusOK {
		t.Errorf("another source after the limited one = %d %s, want 200: the key is source.ip", r.status, r.code)
	}

	s.fresh(ctx)
	partner := map[string]string{"X-Api-Key": s.apiKey}
	global := s.client("127.0.0.4")
	for i := 1; i <= 1000; i++ {
		if r := shopGet(t, global, s.listener(n, "http"), "/partner/v1/orders/"+orderID, partner); r.status != http.StatusOK {
			t.Fatalf("request %d of 1,000 within ratelimit-global = %d %s", i, r.status, r.code)
		}
	}
	r = shopGet(t, global, s.listener(n, "http"), "/partner/v1/orders/"+orderID, partner)
	if r.status != http.StatusTooManyRequests || r.code != "RZ-RL-001" || rateLimit(t, r.header, "ratelimit-global") != 0 {
		t.Errorf("request 1,001 = %d %s, RateLimit %q; want 429 RZ-RL-001 with ratelimit-global at r=0", r.status, r.code, r.header.Values("RateLimit"))
	}

	if len(s.nodes) < 2 {
		return
	}
	s.fresh(ctx)
	spread := s.client("127.0.0.5")
	admitted := 0
	defer func() { t.Logf("the Nodes admitted %d requests of one key in a window", admitted) }()
	for i := range 120 {
		node := s.nodes[i%len(s.nodes)]
		r := shopGet(t, spread, s.listener(node, "http"), summary, token)
		if r.status == http.StatusOK {
			admitted++
			continue
		}
		want := "RZ-RL-002"
		if admitted == 100 {
			want = "RZ-RL-001"
		}
		if r.status != http.StatusTooManyRequests || r.code != want {
			t.Errorf("after %d admitted across the Nodes: %d %s, want 429 %s", admitted, r.status, r.code, want)
		}
		break
	}
	if admitted <= 50 || admitted > 100 {
		t.Errorf("the two Nodes admitted %d requests of one key in a window, want more than the per-Node 50 and at most requests plus burst, 100", admitted)
	}
}

// quotaCounter returns the State Store counter of partner-acme's
// daily-requests window, the units used, and its key.
func quotaCounter(t testing.TB, s *shopRest) (int64, string) {
	t.Helper()
	srv := s.store.Deployment().Primary()
	v, err := srv.Do(t.Context(), "SCAN", "0", "MATCH", "rz:qt:daily-requests:*", "COUNT", "10000")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Elems) != 2 || len(v.Elems[1].Elems) != 1 {
		t.Fatalf("SCAN for the quota counter answered %+v, want one key", v)
	}
	key := v.Elems[1].Elems[0].Str
	got, err := srv.Do(t.Context(), "GET", key)
	if err != nil {
		t.Fatal(err)
	}
	used, err := strconv.ParseInt(got.Str, 10, 64)
	if err != nil {
		t.Fatalf("quota counter %s = %q: %v", key, got.Str, err)
	}
	return used, key
}

// checkQuota checks quota-partner's charge: each admitted partner request,
// through either Node, takes one unit of the daily-requests quota in the
// State Store both Nodes share, reported as the quota's RateLimit item; a
// request rejected before admission takes none; and once the window is
// spent the partner gets 429 RZ-RL-003 with Retry-After.
func checkQuota(t *testing.T, s *shopRest) {
	const limit = 100000
	s.fresh(t.Context())
	client := s.client("127.0.0.6")
	partner := map[string]string{"X-Api-Key": s.apiKey}
	path := "/partner/v1/orders/" + orderID
	used, key := quotaCounter(t, s)
	for i := range 4 {
		node := s.nodes[i%len(s.nodes)]
		r := shopGet(t, client, s.listener(node, "http"), path, partner)
		if r.status != http.StatusOK {
			t.Fatalf("partner request through %s = %d %s", node.Name(), r.status, r.code)
		}
		used++
		if left := rateLimit(t, r.header, "quota-partner"); left != limit-used {
			t.Errorf("partner request through %s reports %d units left, want %d", node.Name(), left, limit-used)
		}
		waitFor(t, "the quota counter", func() bool { u, _ := quotaCounter(t, s); return u == used })
	}
	if r := shopGet(t, client, s.listener(s.nodes[0], "http"), path, map[string]string{"X-Api-Key": s.top.Canary()}); r.status != http.StatusUnauthorized {
		t.Fatalf("a request with an unknown key = %d %s, want 401", r.status, r.code)
	}
	if u, _ := quotaCounter(t, s); u != used {
		t.Errorf("a request rejected before admission changed the quota counter to %d, want %d", u, used)
	}

	if _, err := s.store.Deployment().Primary().Do(t.Context(), "SET", key, strconv.Itoa(limit), "KEEPTTL"); err != nil {
		t.Fatal(err)
	}
	r := shopGet(t, client, s.listener(s.nodes[len(s.nodes)-1], "http"), path, partner)
	retry, _ := strconv.Atoi(r.header.Get("Retry-After"))
	if r.status != http.StatusTooManyRequests || r.code != "RZ-RL-003" || retry < 1 || rateLimit(t, r.header, "quota-partner") != 0 {
		t.Errorf("a request past the spent quota = %d %s, Retry-After %q, RateLimit %q; want 429 RZ-RL-003, the quota at r=0",
			r.status, r.code, r.header.Get("Retry-After"), r.header.Values("RateLimit"))
	}
}
