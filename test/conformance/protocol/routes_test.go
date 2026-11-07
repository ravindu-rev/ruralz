// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"net/http"
	"testing"
)

// routesDoc is the document of the redirect and mirroring rules.
const routesDoc = "docs/architecture/09-traffic-management-and-resilience.md"

// TestRouteRedirects checks Route.spec.redirect on every client protocol
// (docs/architecture/09-traffic-management-and-resilience.md
// "Redirects"): the redirect answers after onRequestHeaders, so
// authentication refuses first; its Location keeps the request's scheme
// and Host unless the redirect sets them, replaces the matched prefix and
// keeps or drops the query; the response Phases run on it (the Gateway's
// security headers), and no request reaches an Upstream.
func TestRouteRedirects(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	t.Logf("rule: %s#redirects", routesDoc)
	eachProto(t, protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		r := g.do(t, p, request{Target: "/moved/orders?x=1", Header: withCase(id)})
		wantProblem(t, r, http.StatusUnauthorized, "RZ-AUTH-001")
		if loc := r.Header.Get("Location"); loc != "" {
			t.Errorf("a refused request got Location %q", loc)
		}
		r = g.do(t, p, request{Target: "/moved/orders?x=1", Header: withCase(id, [2]string{"X-Api-Key", g.creds.apiKey})})
		want := p.scheme() + "://" + g.addr(p) + "/up/h1/orders?x=1"
		if r.Status != http.StatusPermanentRedirect || r.Header.Get("Location") != want || len(r.Body) != 0 {
			t.Errorf("response %v with Location %q, want 308 to %s without content", r, r.Header.Get("Location"), want)
		}
		if r.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Errorf("the redirect lacks the Gateway's security headers: %v", r.Header)
		}
		r = g.do(t, p, request{Target: "/redirect/host?x=1", Header: withCase(id)})
		if want := "https://" + redirectHost + "/landing"; r.Status != http.StatusFound || r.Header.Get("Location") != want {
			t.Errorf("response %v with Location %q, want 302 to %s", r, r.Header.Get("Location"), want)
		}
		if n := len(g.origin.requests(id)); n != 0 {
			t.Errorf("the origin saw %d requests of redirects", n)
		}
	})
}

// TestRouteMirrors checks Route.spec.mirrors on every client protocol
// (docs/architecture/09-traffic-management-and-resilience.md
// "Mirroring"): the mirror Upstream gets what the primary's first attempt
// sent after its onUpstreamRequest Policies (method, target, fields and
// body) with its own Endpoint's Host; the client gets the primary's
// response, which never waits for a mirror that does not answer.
func TestRouteMirrors(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	t.Logf("rule: %s#mirroring", routesDoc)
	t.Run("copy", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			r := g.do(t, p, request{Method: http.MethodPost, Target: "/mirrored/" + bOK + "?x=1", Header: withCase(id), Body: []byte("order=42")})
			wantOK(t, r)
			if got := r.Header.Get(originHeader); got != upH1.String() {
				t.Errorf("the client got the answer of %s, want the primary's (h1)", got)
			}
			rs, err := g.origin.waitRequests(guard(t, exchangeGuard), id, 2)
			if err != nil {
				t.Fatal(err)
			}
			var primary, mirrored *seen
			for i := range rs {
				switch rs[i].Upstream {
				case upH1:
					primary = &rs[i]
				case upH2:
					mirrored = &rs[i]
				default:
				}
			}
			if primary == nil || mirrored == nil || len(rs) != 2 {
				t.Fatalf("the origin saw %+v, want the primary's request on h1 and the mirror's on h2", rs)
			}
			if mirrored.Method != http.MethodPost || mirrored.RequestURI != primary.RequestURI || string(mirrored.Body) != "order=42" {
				t.Errorf("mirror request %s %s body %q, want POST %s order=42", mirrored.Method, mirrored.RequestURI, mirrored.Body, primary.RequestURI)
			}
			if mirrored.Host != g.origin.addr(upH2) {
				t.Errorf("mirror Host %q, want the mirror Endpoint's %s", mirrored.Host, g.origin.addr(upH2))
			}
			for _, k := range []string{"X-Mirror-Signed", "Via", "X-Forwarded-For", caseHeader} {
				if got, want := mirrored.Header.Get(k), primary.Header.Get(k); got != want || want == "" {
					t.Errorf("mirror %s = %q, want the primary attempt's %q", k, got, want)
				}
			}
		})
	})
	t.Run("unanswered mirror", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: "/mirrored-blackhole/" + bOK, Header: withCase(id)}))
			if n := len(g.origin.requests(id)); n != 1 {
				t.Errorf("the origin saw %d requests, want the primary's", n)
			}
		})
	})
}
