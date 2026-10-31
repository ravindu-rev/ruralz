// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/ravindu-rev/ruralz/internal/testkit/mockidp"
)

// hardeningDoc is the document of the request hardening controls.
const hardeningDoc = "docs/architecture/08-security-and-identity.md#request-hardening"

// TestRequestHardening checks the controls of
// docs/architecture/08-security-and-identity.md "Request hardening" on
// every client protocol. Header floods share the 256-line case of
// TestListeners, slow clients the cases of TestTimeouts, and request
// smuggling the framing and hop-by-hop cases of TestListeners and the
// Content-Length row of TestIntermediary.
func TestRequestHardening(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	for _, c := range []struct {
		control string
		run     func(t *testing.T, g *gateway)
	}{
		{"oversized requests", caseOversizedBody},
		{"request smuggling: a body whose framing breaks", caseBrokenBody},
		{"path confusion", casePathConfusion},
		{"credential forwarding", caseCredentialForwarding},
		{"browser protections", caseBrowserProtections},
	} {
		t.Run(c.control, func(t *testing.T) {
			t.Logf("rule: %s, %s", hardeningDoc, c.control)
			c.run(t, g)
		})
	}
}

// caseOversizedBody checks RZ-RT-003 over maxRequestBodyBytes: a declared
// Content-Length over the limit is 413 before the handler reads any of the
// body; a streamed body that passes it while an attempt sends it is 413
// before commit; on HTTP/1.1 net/http drains up to 256 KiB of the rest of
// the body, keeping the connection when the body ends within it and
// closing it otherwise; a gated body read whole is cut at the limit; and a
// body decoded for CEL counts decoded, over four times the raw limit.
func caseOversizedBody(t *testing.T, g *gateway) {
	t.Run("declared", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			// An HTTP/1.1 client waits for the body's go-ahead with
			// Expect: 100-continue; an HTTP/2 client simply stops after
			// HEADERS.
			id := g.origin.newCase()
			req := request{Method: http.MethodPost, Target: upH1.path(bOK), Header: withCase(id), Body: make([]byte, bodyLimit+1)}
			if !p.isH2() {
				req.Header = withCase(id, [2]string{"Expect", "100-continue"})
			}
			r := g.headOnly(t, p, req)
			wantProblem(t, r, http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if len(r.Interim) != 0 {
				t.Errorf("interim responses %v before the 413, want none: the body is never read", interimStatuses(r))
			}
			if !p.isH2() && !r.Close {
				t.Errorf("413 %v to a body never read without Connection: close", r)
			}
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests", n)
			}
		})
	})
	t.Run("declared-beyond-drain", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			// Without Expect, net/http drains up to 256 KiB of an unread
			// body before it answers; beyond that it answers at once and
			// closes the connection.
			c := g.dialH1(t, p)
			defer c.close()
			req := request{Method: http.MethodPost, Target: upH1.path(bOK), Header: withCase(g.origin.newCase()), Body: make([]byte, 1<<20)}
			c.write(req.h1Head(g.addr(p)))
			r := c.read(guard(t, exchangeGuard), req.method())
			wantProblem(t, r, http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if !r.Close {
				t.Errorf("413 %v to a body beyond the drain without Connection: close", r)
			}
		})
	})
	t.Run("declared-within-drain", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			// A declared body within net/http's 256 KiB drain is read and
			// discarded before the 413 goes out, and the connection is
			// kept for the next request.
			c := g.dialH1(t, p)
			defer c.close()
			id := g.origin.newCase()
			req := request{Method: http.MethodPost, Target: upH1.path(bOK), Header: withCase(id), Body: make([]byte, bodyLimit+1)}
			r := c.exchange(guard(t, exchangeGuard), req.method(), req.h1Bytes(g.addr(p)))
			wantProblem(t, r, http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if r.Close {
				t.Errorf("413 %v closed the connection after a body within the drain", r)
			}
			next := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
			wantOK(t, c.exchange(guard(t, exchangeGuard), next.method(), next.h1Bytes(g.addr(p))))
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests", n)
			}
		})
	})
	t.Run("streamed-while-sent", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			// More than net/http's 256 KiB drain of an unread body stays
			// unread past the limit, so the HTTP/1.1 connection closes.
			req := request{
				Method: http.MethodPost, Target: u.path(bOK), Chunked: true,
				Header: withCase(g.origin.newCase()), Body: bytes.Repeat([]byte("x"), 1<<20),
			}
			if p.isH2() {
				wantProblem(t, g.do(t, p, req), http.StatusRequestEntityTooLarge, "RZ-RT-003")
				return
			}
			c := g.dialH1(t, p)
			defer c.close()
			wantProblem(t, c.exchange(guard(t, exchangeGuard), req.method(), req.h1Bytes(g.addr(p))),
				http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if !c.waitClosed(settleGuard) {
				t.Error("the HTTP/1.1 connection stayed open after a body read in part")
			}
		})
	})
	t.Run("streamed-within-drain", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			// The rest of the body past the limit ends within net/http's
			// 256 KiB drain, so the connection is kept.
			c := g.dialH1(t, p)
			defer c.close()
			req := request{
				Method: http.MethodPost, Target: upH1.path(bOK), Chunked: true,
				Header: withCase(g.origin.newCase()), Body: bytes.Repeat([]byte("x"), bodyLimit+(100<<10)),
			}
			r := c.exchange(guard(t, exchangeGuard), req.method(), req.h1Bytes(g.addr(p)))
			wantProblem(t, r, http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if r.Close {
				t.Errorf("413 %v closed the connection after a body that ended within the drain", r)
			}
			next := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
			wantOK(t, c.exchange(guard(t, exchangeGuard), next.method(), next.h1Bytes(g.addr(p))))
		})
	})
	t.Run("gated", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			req := request{
				Method: http.MethodPost, Target: "/gated/" + bOK, Chunked: true,
				Header: withCase(id), Body: bytes.Repeat([]byte("x"), bodyLimit+1),
			}
			wantProblem(t, g.do(t, p, req), http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests", n)
			}
			id = g.origin.newCase()
			req.Header, req.Body = withCase(id), bytes.Repeat([]byte("x"), bodyLimit)
			wantOK(t, g.do(t, p, req))
			if s := g.origin.only(t, id); len(s.Body) != bodyLimit {
				t.Errorf("the origin got %d bytes, want the whole %d-byte body", len(s.Body), bodyLimit)
			}
		})
	})
	t.Run("decoded", func(t *testing.T) {
		// 20,000 empty arrays: under 64 KiB raw, decoded well over four
		// times that.
		dense := []byte("[" + strings.Repeat("[],", 19999) + "[]]")
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			req := request{
				Method: http.MethodPost, Target: "/decoded/" + bOK, Body: dense,
				Header: withCase(id, [2]string{"Content-Type", "application/json"}),
			}
			wantProblem(t, g.do(t, p, req), http.StatusRequestEntityTooLarge, "RZ-RT-003")
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests", n)
			}
		})
	})
}

// settleGuard bounds the wait for a connection the Node closes at once.
const settleGuard = 5 * time.Second

// headOnly sends the head of req, which declares its body, and reads the
// response without sending the body.
func (g *gateway) headOnly(t testing.TB, p proto, req request) *response {
	t.Helper()
	ctx := guard(t, exchangeGuard)
	if !p.isH2() {
		c := g.dialH1(t, p)
		defer c.close()
		c.write(req.h1Head(g.addr(p)))
		return c.read(ctx, req.method())
	}
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	if err := c.writeHeaders(id, req.h2Fields(p, g.addr(p)), false); err != nil {
		t.Fatal(err)
	}
	return c.collect(ctx, id)
}

// caseBrokenBody checks that a client body whose framing breaks while it
// streams never reaches the Upstream whole: an HTTP/1.1 chunk size that is
// not hexadecimal ends the request with the connection closed and no
// response.
func caseBrokenBody(t *testing.T, g *gateway) {
	eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
		id := g.origin.newCase()
		c := g.dialH1(t, p)
		defer c.close()
		raw := "POST " + u.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id +
			"\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\nzz\r\nworld\r\n0\r\n\r\n"
		r := c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw))
		if r.Status != 0 {
			t.Errorf("response %v to a request whose body broke, want the connection closed without one", r)
		}
		for _, s := range g.origin.requests(id) {
			if strings.Contains(string(s.Body), "world") {
				t.Errorf("the origin got the broken body as %q", s.Body)
			}
		}
	})
}

// casePathConfusion checks the one normalized path: the Router and the
// Upstream see it; %00, raw or encoded backslashes, other control bytes
// and invalid escapes are 400 RZ-RT-017 (raw control bytes, which no
// request line or HTTP/2 field value may carry, are refused by net/http
// itself); a transform.request path rewrite runs under the same hardening,
// a result it refuses being the Policy's cannot decide.
func casePathConfusion(t *testing.T, g *gateway) {
	t.Run("normalized", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			for _, tt := range []struct{ target, want string }{
				{"/up/h1/./x/../" + bOK, "/up/h1/" + bOK},
				{"/up/%68%31/" + bOK, "/up/h1/" + bOK},
				{"/up/h1/a%2fb/../" + bOK, "/up/h1/" + bOK},
				{"/up/h1/a%2fb", "/up/h1/a%2Fb"},
				{"/up/h1/" + bOK + "?q=%2F", "/up/h1/" + bOK + "?q=%2F"},
			} {
				id := g.origin.newCase()
				r := g.do(t, p, request{Target: tt.target, Header: withCase(id)})
				if r.Status != http.StatusOK {
					t.Errorf("%s: response %v, want 200", tt.target, r)
					continue
				}
				if got := g.origin.only(t, id).RequestURI; got != tt.want {
					t.Errorf("%s reached the Upstream as %s, want %s", tt.target, got, tt.want)
				}
			}
		})
	})
	t.Run("refused", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			for _, target := range []string{
				"/up/h1/a%00b", "/up/h1/a%5Cb", "/up/h1/a%5cb", "/up/h1/a\\b",
				"/up/h1/a%01b", "/up/h1/a%7Fb",
			} {
				id := g.origin.newCase()
				r := g.do(t, p, request{Target: target, Header: withCase(id)})
				if r.Status != http.StatusBadRequest || !r.isProblem() {
					t.Errorf("%q: response %v, want 400 RZ-RT-017", target, r)
				} else {
					wantProblem(t, r, http.StatusBadRequest, "RZ-RT-017")
				}
				if n := len(g.origin.requests(id)); n != 0 {
					t.Errorf("%q: the origin saw %d requests", target, n)
				}
			}
		})
		// net/http refuses a raw control byte and an invalid escape in the
		// request target before any handler.
		for _, target := range []string{"/up/h1/a\x01b", "/up/h1/a%zzb", "/up/h1/a%4"} {
			eachProto(t, h1Protos(), func(t *testing.T, p proto) {
				wantPlain(t, g.do(t, p, request{Target: target}), http.StatusBadRequest)
			})
			eachProto(t, h2Protos(), func(t *testing.T, p proto) {
				if r := g.do(t, p, request{Target: target}); !r.WasReset || r.Reset != http2.ErrCodeProtocol {
					t.Errorf("%q: response %v, want RST_STREAM PROTOCOL_ERROR", target, r)
				}
			})
		}
	})
	t.Run("rewrite-under-hardening", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: "/rewrite/" + bOK, Header: withCase(id, [2]string{"X-New-Path", "/rewritten/%6Fk"})}))
			if got := g.origin.only(t, id).RequestURI; got != "/rewritten/"+bOK {
				t.Errorf("the rewritten path reached the Upstream as %s, want its normalized form /rewritten/%s", got, bOK)
			}
			for _, bad := range []string{"/rewritten/./" + bOK, "/a%00b", "/a%5Cb"} {
				id = g.origin.newCase()
				wantProblem(t, g.do(t, p, request{Target: "/rewrite/" + bOK, Header: withCase(id, [2]string{"X-New-Path", bad})}),
					http.StatusServiceUnavailable, "RZ-RT-011")
				if n := len(g.origin.requests(id)); n != 0 {
					t.Errorf("%s: the origin saw %d requests of a refused rewrite", bad, n)
				}
			}
		})
	})
}

// caseCredentialForwarding checks that auth.api-key and auth.basic strip
// their credential before the request reaches the Upstream, and auth.jwt
// forwards it.
func caseCredentialForwarding(t *testing.T, g *gateway) {
	c := g.creds
	token, err := c.idp.Mint(mockidp.RS256, map[string]any{"sub": "alice", "aud": "conformance"})
	if err != nil {
		t.Fatal(err)
	}
	basic := "Basic " + base64.StdEncoding.EncodeToString([]byte(c.basicUser+":"+c.basicPassword))
	eachProto(t, protos(), func(t *testing.T, p proto) {
		for _, tt := range []struct {
			path, field, value string
			forwarded          bool
		}{
			{"/auth/api-key/" + bOK, "X-Api-Key", c.apiKey, false},
			{"/auth/basic/" + bOK, "Authorization", basic, false},
			{"/auth/jwt/" + bOK, "Authorization", "Bearer " + token, true},
		} {
			id := g.origin.newCase()
			r := g.do(t, p, request{Target: tt.path, Header: withCase(id, [2]string{tt.field, tt.value})})
			wantOK(t, r)
			got, ok := g.origin.only(t, id).Header[tt.field]
			switch {
			case tt.forwarded && (!ok || got[0] != tt.value):
				t.Errorf("%s: the Upstream got %s %q, want the credential forwarded", tt.path, tt.field, got)
			case !tt.forwarded && ok:
				t.Errorf("%s: the Upstream got the credential in %s", tt.path, tt.field)
			}
		}
	})
}

// caseBrowserProtections checks the browser protections: a cors Policy
// answers a preflight in onRequestHeaders, before authentication, and a
// non-overridable Gateway headers Policy sets the security headers on
// every routed response, generated ones included.
func caseBrowserProtections(t *testing.T, g *gateway) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		r := g.do(t, p, request{Method: http.MethodOptions, Target: "/cors/" + bOK, Header: withCase(id,
			[2]string{"Origin", corsOrigin}, [2]string{"Access-Control-Request-Method", http.MethodPost},
			[2]string{"Access-Control-Request-Headers", "x-api-key"})})
		if r.Status != http.StatusNoContent || r.Header.Get("Access-Control-Allow-Origin") != corsOrigin {
			t.Errorf("preflight: response %v Access-Control-Allow-Origin %q, want 204 for %s",
				r, r.Header.Get("Access-Control-Allow-Origin"), corsOrigin)
		}
		if n := len(g.origin.requests(id)); n != 0 {
			t.Errorf("the origin saw %d requests of a preflight", n)
		}
		r = g.do(t, p, request{Method: http.MethodOptions, Target: "/cors/" + bOK, Header: [][2]string{
			{"Origin", "https://evil.example"}, {"Access-Control-Request-Method", http.MethodPost},
		}})
		wantProblem(t, r, http.StatusForbidden, "RZ-RT-008")
		r = g.do(t, p, request{Target: "/auth/api-key/" + bOK})
		wantProblem(t, r, http.StatusUnauthorized, "RZ-AUTH-001")
		if r.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Errorf("the generated 401 lacks the Gateway's security headers: %v", r.Header)
		}
		r = g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())})
		if r.Header.Get("Strict-Transport-Security") != "max-age=31536000" {
			t.Errorf("a proxied response lacks the Gateway's security headers: %v", r.Header)
		}
	})
}
