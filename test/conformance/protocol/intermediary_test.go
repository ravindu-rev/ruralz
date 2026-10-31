// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// intermediaryDoc is the document of the HTTP intermediary behavior rows.
const intermediaryDoc = "docs/architecture/07-multi-protocol.md#http-intermediary-behavior"

// intermediaryCase is the case of one row of the HTTP intermediary
// behavior table; row is the row's Case cell without code spans.
type intermediaryCase struct {
	row string
	run func(t *testing.T, g *gateway)
}

// intermediaryCases returns one case per row of the table, in its order.
// TestIntermediaryRowsCovered holds them to the document.
func intermediaryCases() []intermediaryCase {
	return []intermediaryCase{
		{"Expect: 100-continue", caseExpect},
		{"Upgrade", caseUpgrade},
		{"1xx responses", caseInterim},
		{"Trailer fields", caseTrailers},
		{"TE", caseTE},
		{"Via", caseVia},
		{"Server and Date", caseServerDate},
		{"Absolute-form target", caseAbsoluteForm},
		{"Asterisk-form target", caseAsteriskForm},
		{"Host and :authority", caseHostAuthority},
		{"Content-Length", caseContentLength},
		{"HEAD and 304", caseHeadAnd304},
	}
}

// TestIntermediary runs the cases of the HTTP intermediary behavior table
// (docs/architecture/07-multi-protocol.md "HTTP intermediary behavior")
// against one Node.
func TestIntermediary(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	for _, c := range intermediaryCases() {
		t.Run(c.row, func(t *testing.T) {
			t.Logf("rule: %s, row %q", intermediaryDoc, c.row)
			c.run(t, g)
		})
	}
}

// TestIntermediaryRowsCovered holds intermediaryCases to the HTTP
// intermediary behavior table of docs/architecture/07-multi-protocol.md:
// one case per row, named by its Case cell without code spans, in the
// table's order, so a row added to the document fails the suite until it
// has a case.
func TestIntermediaryRowsCovered(t *testing.T) {
	t.Parallel()
	// Tests run in the package directory, test/conformance/protocol.
	doc, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "architecture", "07-multi-protocol.md"))
	if err != nil {
		t.Fatal(err)
	}
	rows := tableRows(t, string(doc), "### HTTP intermediary behavior")
	var cases []string
	for _, c := range intermediaryCases() {
		cases = append(cases, c.row)
	}
	if !slices.Equal(rows, cases) {
		t.Errorf("intermediaryCases = %q, want the rows of %s: %q", cases, intermediaryDoc, rows)
	}
}

// tableRows returns the first cells, code spans unwrapped, of the first
// table under heading in doc.
func tableRows(t testing.TB, doc, heading string) []string {
	t.Helper()
	_, section, ok := strings.Cut(doc, "\n"+heading+"\n")
	if !ok {
		t.Fatalf("no %q heading", heading)
	}
	var rows []string
	inTable := false
	for line := range strings.Lines(section) {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "#"):
			return rows
		case !strings.HasPrefix(line, "|"):
			if inTable {
				return rows
			}
			continue
		case !inTable:
			// The header row; the separator row follows.
			inTable = true
			continue
		case strings.HasPrefix(line, "|---"):
			continue
		}
		cell, _, _ := strings.Cut(strings.TrimPrefix(line, "|"), "|")
		rows = append(rows, strings.ReplaceAll(strings.TrimSpace(cell), "`", ""))
	}
	return rows
}

// eachProto runs f as a subtest per client protocol of ps.
func eachProto(t *testing.T, ps []proto, f func(t *testing.T, p proto)) {
	t.Helper()
	for _, p := range ps {
		t.Run(p.String(), func(t *testing.T) { f(t, p) })
	}
}

// eachPairing runs f as a subtest per pairing of a client protocol of ps
// with an Upstream transport, for a behavior that crosses the hop.
func eachPairing(t *testing.T, ps []proto, f func(t *testing.T, p proto, u upstream)) {
	t.Helper()
	for _, p := range ps {
		for _, u := range upstreams() {
			t.Run(p.String()+"-to-"+u.String(), func(t *testing.T) { f(t, p, u) })
		}
	}
}

// h1Protos returns the HTTP/1.x client protocols.
func h1Protos() []proto { return []proto{h1, h1TLS} }

// h2Protos returns the HTTP/2 client protocols.
func h2Protos() []proto { return []proto{h2c, h2TLS} }

// withCase returns header with caseHeader set to id.
func withCase(id string, header ...[2]string) [][2]string {
	return append([][2]string{{caseHeader, id}}, header...)
}

// interimStatuses returns the 1xx statuses of r in order.
func interimStatuses(r *response) []int {
	var out []int
	for _, i := range r.Interim {
		out = append(out, i.Status)
	}
	return out
}

// continueTimeout bounds the wait for an interim or final response after
// a request head with Expect: 100-continue.
const continueTimeout = 10 * time.Second

// caseExpect checks the Expect: 100-continue row: a request Phase that
// rejects sends its final status without reading the body; otherwise 100
// Continue goes out when the Node first reads the body; the Upstream
// request carries no Expect; on HTTP/1.1 net/http answers another
// expectation with a plain 417 and ignores the 100-continue of an HTTP/1.0
// request; on HTTP/2 another expectation is dropped.
func caseExpect(t *testing.T, g *gateway) {
	body := []byte("expected-body")
	t.Run("continue-then-forward", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			req := request{Method: http.MethodPost, Target: u.path(bOK), Header: withCase(id, [2]string{"Expect", "100-continue"}), Body: body}
			r := g.expectContinue(t, p, req, true)
			wantOK(t, r)
			if got := interimStatuses(r); !slices.Equal(got, []int{http.StatusContinue}) {
				t.Errorf("interim responses %v, want one 100 Continue", got)
			}
			s := g.origin.only(t, id)
			if !bytes.Equal(s.Body, body) {
				t.Errorf("the origin got body %q, want %q", s.Body, body)
			}
			if v, ok := s.Header["Expect"]; ok {
				t.Errorf("the Upstream request carries Expect %q", v)
			}
		})
	})
	t.Run("rejected-without-reading", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			req := request{Method: http.MethodPost, Target: "/auth/api-key/" + bOK, Header: withCase(id, [2]string{"Expect", "100-continue"}), Body: body}
			r := g.expectContinue(t, p, req, false)
			wantProblem(t, r, http.StatusUnauthorized, "RZ-AUTH-001")
			if len(r.Interim) != 0 {
				t.Errorf("interim responses %v before a rejection, want none", interimStatuses(r))
			}
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests of a rejected one", n)
			}
		})
	})
	t.Run("other-expectation", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			r := g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(id, [2]string{"Expect", "x-conformance"})})
			wantPlain(t, r, http.StatusExpectationFailed)
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests of a 417", n)
			}
		})
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(id, [2]string{"Expect", "x-conformance"})}))
			if v, ok := g.origin.only(t, id).Header["Expect"]; ok {
				t.Errorf("the Upstream request carries Expect %q", v)
			}
		})
	})
	t.Run("http-1.0-ignored", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			r := g.do(t, p, request{
				Method: http.MethodPost, Target: upH1.path(bOK), HTTP10: true, Body: body,
				Header: withCase(id, [2]string{"Expect", "100-continue"}),
			})
			wantOK(t, r)
			if len(r.Interim) != 0 {
				t.Errorf("interim responses %v to HTTP/1.0, want none", interimStatuses(r))
			}
			s := g.origin.only(t, id)
			if _, ok := s.Header["Expect"]; ok || !bytes.Equal(s.Body, body) {
				t.Errorf("the origin got Expect %q and body %q", s.Header["Expect"], s.Body)
			}
		})
	})
}

// expectContinue sends the head of req, which carries Expect:
// 100-continue, waits for the first response, and sends the body only
// after a 100 Continue when sendBody: a client that waits for the 100 as
// RFC 9110 section 10.1.1 describes. It returns every response it saw.
func (g *gateway) expectContinue(t testing.TB, p proto, req request, sendBody bool) *response {
	t.Helper()
	ctx := guard(t, exchangeGuard)
	if !p.isH2() {
		c := g.dialH1(t, p)
		defer c.close()
		c.write(req.h1Head(g.addr(p)))
		wait := guard(t, continueTimeout)
		first, err := c.readOne(wait, req.method())
		if err != nil {
			t.Fatalf("no response to the request head: %v", err)
		}
		// readBody closes a final response's body; a 1xx has none.
		defer func() { _ = first.Body.Close() }()
		if first.StatusCode != http.StatusContinue {
			out := &response{Proto: first.Proto, Status: first.StatusCode, Header: first.Header}
			out.Body, _ = readBody(first)
			return out
		}
		if !sendBody {
			t.Fatalf("100 Continue to a request the Node rejects")
		}
		c.write(req.h1Body())
		r := c.read(ctx, req.method())
		r.Interim = append([]interim{{Status: first.StatusCode, Header: first.Header}}, r.Interim...)
		return r
	}
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	if err := c.writeHeaders(id, req.h2Fields(p, g.addr(p)), false); err != nil {
		t.Fatal(err)
	}
	wait := guard(t, continueTimeout)
	ev, ok := c.waitEvent(wait, func(e h2Event) bool {
		return e.StreamID == id && (e.Type == http2.FrameHeaders || e.Type == http2.FrameRSTStream)
	})
	if !ok {
		t.Fatalf("no response to the request head: %v", ev.Err)
	}
	if ev.Type == http2.FrameHeaders && ev.status() == http.StatusContinue {
		if !sendBody {
			t.Fatalf("100 Continue to a request the Node rejects")
		}
		if err := c.sendBody(ctx, id, req.Body, nil); err != nil {
			t.Fatal(err)
		}
		r := c.collect(ctx, id)
		r.Interim = append([]interim{{Status: ev.status(), Header: ev.header()}}, r.Interim...)
		return r
	}
	c.pending = append([]h2Event{ev}, c.pending...)
	return c.collect(ctx, id)
}

// readBody reads and closes a response body.
func readBody(r *http.Response) ([]byte, error) {
	defer func() { _ = r.Body.Close() }()
	var b bytes.Buffer
	_, err := b.ReadFrom(r.Body)
	return b.Bytes(), err
}

// caseUpgrade checks the Upgrade row: websocket on a Route whose Upstream
// protocol is http is 400 RZ-RT-023 after the match, with the response
// Phases run; any other token, h2c included, is dropped with the other
// hop-by-hop fields and the request is served as HTTP/1.1. On HTTP/2,
// where Upgrade is a connection-specific field (RFC 9113 section 8.2.2),
// net/http answers a plain 400 before any handler.
func caseUpgrade(t *testing.T, g *gateway) {
	t.Run("websocket", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			r := g.do(t, p, request{Target: "/policy/" + bOK, Header: withCase(id,
				[2]string{"Connection", "Upgrade"}, [2]string{"Upgrade", "websocket"},
				[2]string{"Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="}, [2]string{"Sec-WebSocket-Version", "13"})})
			wantProblem(t, r, http.StatusBadRequest, "RZ-RT-023")
			if r.Header.Get("X-Route-Response") != "applied" || r.Header.Get("Strict-Transport-Security") == "" {
				t.Errorf("the response Phases did not run on the 400: %v", r.Header)
			}
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests of a refused upgrade", n)
			}
		})
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			r := g.do(t, p, request{Target: "/policy/" + bOK, Header: withCase(id, [2]string{"upgrade", "websocket"})})
			wantPlain(t, r, http.StatusBadRequest)
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests of a malformed HTTP/2 request", n)
			}
		})
	})
	t.Run("websocket-before-onRequestHeaders", func(t *testing.T) {
		// The Route's auth.api-key Policy would answer 401 RZ-AUTH-001 to
		// a request without a key in onRequestHeaders; the upgrade is
		// refused before it runs.
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			r := g.do(t, p, request{Target: "/auth/api-key/" + bOK, Header: [][2]string{
				{"Connection", "Upgrade"},
				{"Upgrade", "websocket"},
				{"Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ=="},
				{"Sec-WebSocket-Version", "13"},
			}})
			wantProblem(t, r, http.StatusBadRequest, "RZ-RT-023")
		})
	})
	t.Run("other-token-dropped", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			for _, tok := range []string{"h2c", "x-conformance/1"} {
				id := g.origin.newCase()
				r := g.do(t, p, request{Target: u.path(bOK), Header: withCase(id,
					[2]string{"Connection", "Upgrade, HTTP2-Settings"}, [2]string{"Upgrade", tok},
					[2]string{"HTTP2-Settings", "AAMAAABkAAQAAP__"})})
				wantOK(t, r)
				if r.Proto != "HTTP/1.1" {
					t.Errorf("Upgrade %s answered over %s, want HTTP/1.1", tok, r.Proto)
				}
				h := g.origin.only(t, id).Header
				for _, f := range []string{"Upgrade", "Connection", "Http2-Settings"} {
					if v, ok := h[f]; ok {
						t.Errorf("Upgrade %s: the Upstream request carries %s %q", tok, f, v)
					}
				}
			}
		})
	})
}

// caseInterim checks the 1xx responses row: an Upstream's 103 Early Hints
// on a Route with upstreams reaches an HTTP/1.1 or later client before the
// final response, without hop-by-hop fields (docs/architecture/03-data-plane.md
// "Upstream layer"), a composition step's 103 does not, an HTTP/1.0
// client gets no 1xx, and the client sees no Upstream 100 Continue and no
// other 1xx.
func caseInterim(t *testing.T, g *gateway) {
	t.Run("103-relayed", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bHints), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if len(r.Interim) != 1 || r.Interim[0].Status != http.StatusEarlyHints ||
				r.Interim[0].Header.Get("Link") != "</style.css>; rel=preload; as=style" {
				t.Fatalf("interim responses %+v, want the Upstream's 103 with its Link", r.Interim)
			}
			if r.Header.Get("Link") != "" {
				t.Errorf("the final response carries the 103's Link %q", r.Header.Get("Link"))
			}
		})
	})
	t.Run("103-without-hop-by-hop", func(t *testing.T) {
		// The 103's Connection field names X-Hint-Private, a hop-by-hop
		// field of that 103 like Keep-Alive (RFC 9110 section 7.6.1).
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bHopHints), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if len(r.Interim) != 1 || r.Interim[0].Status != http.StatusEarlyHints {
				t.Fatalf("interim responses %+v, want the Upstream's 103", r.Interim)
			}
			h := r.Interim[0].Header
			if h.Get("Link") != "</style.css>; rel=preload; as=style" {
				t.Errorf("the relayed 103 lost its Link: %v", h)
			}
			for _, f := range []string{"Keep-Alive", "Connection", "X-Hint-Private"} {
				if v, ok := h[f]; ok {
					t.Errorf("the relayed 103 carries the hop-by-hop field %s %q", f, v)
				}
			}
		})
	})
	t.Run("composition-step-103-not-relayed", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			r := g.do(t, p, request{Target: "/composed/" + u.String() + "/" + bOK, Header: withCase(id)})
			wantOK(t, r)
			if len(r.Interim) != 0 {
				t.Errorf("interim responses %v from a composition step, want none", interimStatuses(r))
			}
			if s := g.origin.only(t, id); s.RequestURI != composedPath(u) {
				t.Errorf("the step reached the origin as %s, want %s", s.RequestURI, composedPath(u))
			}
		})
	})
	t.Run("none-to-http-1.0", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bHints), HTTP10: true, Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if len(r.Interim) != 0 {
				t.Errorf("interim responses %v to HTTP/1.0, want none", interimStatuses(r))
			}
		})
	})
	t.Run("only-103-crosses", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bInterim), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if got := interimStatuses(r); !slices.Equal(got, []int{http.StatusEarlyHints}) {
				t.Errorf("interim responses %v, want only the 103: no Upstream 100 Continue or 102", got)
			}
		})
	})
}

// caseTrailers checks the Trailer fields row: an Upstream's trailer
// fields reach the client after the body, declared ones announced in
// Trailer and chunked on HTTP/1.1, undeclared ones too, hop-by-hop and
// framing fields never, those the message's Connection field names
// included; an HTTP/1.0 client gets none; a client's declared trailer
// fields reach the Upstream after the body by the same rule; no Phase
// reads or edits them.
func caseTrailers(t *testing.T, g *gateway) {
	t.Run("response", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bTrailers), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			wantTrailers(t, r)
			if !p.isH2() && r.Header.Get("Content-Length") != "" {
				t.Errorf("an HTTP/1.1 response with trailers carries Content-Length %q, want chunked", r.Header.Get("Content-Length"))
			}
			if p.isH2() && !strings.EqualFold(r.Header.Get("Trailer"), "X-Trailer-Declared") {
				t.Errorf("Trailer announces %q, want X-Trailer-Declared", r.Header.Get("Trailer"))
			}
		})
	})
	t.Run("response-hop-by-hop-dropped", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			for _, u := range []upstream{upH1, upH1TLS} {
				r := g.do(t, p, request{Target: u.path(bRawTrailers), Header: withCase(g.origin.newCase())})
				if r.Status != http.StatusOK {
					t.Fatalf("%v: response %v", u, r)
				}
				wantTrailers(t, r)
				for _, f := range []string{"Keep-Alive", "Proxy-Authenticate", "Content-Length", "Trailer"} {
					if v, ok := r.Trailer[f]; ok {
						t.Errorf("%v: the client got trailer field %s %q", u, f, v)
					}
				}
			}
		})
	})
	t.Run("response-connection-named-dropped", func(t *testing.T) {
		// The response's Connection field names a declared and an
		// undeclared trailer field, hop-by-hop fields of the whole
		// response (RFC 9110 section 7.6.1). HTTP/2 has no Connection
		// field (RFC 9113 section 8.2.2): net/http's server drops it from
		// the final response, so over upH2 the fields it named reach the
		// Node as end-to-end fields.
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bConnTrailers), Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			wantTrailers(t, r)
			for _, f := range []string{"X-Trailer-Hop", "X-Trailer-Hop-Late"} {
				_, ok := r.Trailer[f]
				switch {
				case u != upH2 && ok:
					t.Errorf("the client got trailer field %s %q, which the Upstream's Connection names", f, r.Trailer[f])
				case u == upH2 && !ok:
					t.Errorf("the end-to-end trailer field %s did not pass over HTTP/2: %v", f, r.Trailer)
				}
			}
		})
	})
	t.Run("none-to-http-1.0", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bTrailers), HTTP10: true, Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if len(r.Trailer) != 0 || string(r.Body) != "hello" {
				t.Errorf("HTTP/1.0 response body %q trailers %v, want the body without trailers", r.Body, r.Trailer)
			}
		})
	})
	t.Run("not-edited-by-phases", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			r := g.do(t, p, request{Target: "/policy/" + bTrailers, Header: withCase(g.origin.newCase())})
			wantOK(t, r)
			if r.Header.Get("X-Route-Response") != "applied" {
				t.Fatalf("the Route headers Policy did not run: %v", r.Header)
			}
			wantTrailers(t, r)
		})
	})
	t.Run("request", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			r := g.do(t, p, request{
				Method: http.MethodPost, Target: u.path(bOK), Body: []byte("payload"),
				Header:  withCase(id, [2]string{"Trailer", "X-Client-Checksum"}),
				Trailer: [][2]string{{"X-Client-Checksum", "c0ffee"}},
			})
			wantOK(t, r)
			s := g.origin.only(t, id)
			if string(s.Body) != "payload" || s.Trailer.Get("X-Client-Checksum") != "c0ffee" {
				t.Errorf("the origin got body %q trailers %v, want the body then X-Client-Checksum", s.Body, s.Trailer)
			}
		})
	})
	t.Run("request-hop-by-hop-dropped", func(t *testing.T) {
		// Keep-Alive is a fixed hop-by-hop field, and X-Client-Hop one the
		// request's Connection field names (RFC 9110 section 7.6.1).
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			r := g.do(t, p, request{
				Method: http.MethodPost, Target: u.path(bOK), Body: []byte("payload"),
				Header: withCase(id, [2]string{"Connection", "X-Client-Hop"},
					[2]string{"Trailer", "X-Client-Checksum, Keep-Alive, X-Client-Hop"}),
				Trailer: [][2]string{{"X-Client-Checksum", "c0ffee"}, {"Keep-Alive", "timeout=1"}, {"X-Client-Hop", "hop"}},
			})
			wantOK(t, r)
			s := g.origin.only(t, id)
			if s.Trailer.Get("X-Client-Checksum") != "c0ffee" {
				t.Errorf("the origin got trailers %v, want X-Client-Checksum", s.Trailer)
			}
			for _, f := range []string{"Keep-Alive", "X-Client-Hop"} {
				if v, ok := s.Trailer[f]; ok {
					t.Errorf("the origin got the hop-by-hop trailer field %s %q", f, v)
				}
			}
		})
	})
}

// wantTrailers fails t unless r carries the origin's declared and
// undeclared trailer fields after its body.
func wantTrailers(t testing.TB, r *response) {
	t.Helper()
	if string(r.Body) != "hello" || r.Trailer.Get("X-Trailer-Declared") != "declared-value" ||
		r.Trailer.Get("X-Trailer-Undeclared") != "undeclared-value" {
		t.Errorf("body %q trailers %v, want hello then the declared and undeclared fields", r.Body, r.Trailer)
	}
}

// caseTE checks the TE row: TE is hop-by-hop; a trailers member reaches
// the Upstream as exactly TE: trailers and no other member does; on
// HTTP/2 a TE other than trailers makes the request malformed, which
// net/http answers with a plain 400 before any handler.
func caseTE(t *testing.T, g *gateway) {
	t.Run("trailers-member-only", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			for _, tt := range []struct {
				name   string
				header [][2]string
				want   []string
			}{
				{"trailers and a coding", [][2]string{{"TE", "trailers, deflate;q=0.5"}}, []string{"trailers"}},
				{"a coding alone", [][2]string{{"TE", "gzip"}}, nil},
				{"named by Connection", [][2]string{{"Connection", "TE"}, {"TE", "trailers"}}, []string{"trailers"}},
			} {
				id := g.origin.newCase()
				wantOK(t, g.do(t, p, request{Target: u.path(bOK), Header: withCase(id, tt.header...)}))
				if got := g.origin.only(t, id).Header["Te"]; !slices.Equal(got, tt.want) {
					t.Errorf("%s: the Upstream got TE %q, want %q", tt.name, got, tt.want)
				}
			}
		})
	})
	t.Run("http2-trailers", func(t *testing.T) {
		eachPairing(t, h2Protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: u.path(bOK), Header: withCase(id, [2]string{"te", "trailers"})}))
			if got := g.origin.only(t, id).Header["Te"]; !slices.Equal(got, []string{"trailers"}) {
				t.Errorf("the Upstream got TE %q, want trailers", got)
			}
		})
	})
	t.Run("http2-malformed", func(t *testing.T) {
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantPlain(t, g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(id, [2]string{"te", "gzip"})}), http.StatusBadRequest)
			if n := len(g.origin.requests(id)); n != 0 {
				t.Errorf("the origin saw %d requests of a malformed one", n)
			}
		})
	})
}

// caseVia checks the Via row: each Upstream request gains an entry after
// any the client sent, naming the client hop's protocol and the pseudonym
// ruralz; responses get none.
func caseVia(t *testing.T, g *gateway) {
	t.Run("appended", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: u.path(bOK), Header: withCase(id, [2]string{"Via", "1.1 client-proxy"})}))
			got := strings.Join(g.origin.only(t, id).Header["Via"], ", ")
			if want := "1.1 client-proxy, " + p.via(); got != want {
				t.Errorf("the Upstream got Via %q, want %q", got, want)
			}
		})
	})
	t.Run("http-1.0", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: upH1.path(bOK), HTTP10: true, Header: withCase(id)}))
			if got := g.origin.only(t, id).Header["Via"]; !slices.Equal(got, []string{"1.0 ruralz"}) {
				t.Errorf("the Upstream got Via %q, want 1.0 ruralz", got)
			}
		})
	})
	t.Run("responses-get-none", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			if r := g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}); r.Header.Get("Via") != "" {
				t.Errorf("a response carries Via %q", r.Header.Get("Via"))
			}
			r := g.do(t, p, request{Target: upH1.path(bVia), Header: withCase(g.origin.newCase())})
			if got := r.Header["Via"]; !slices.Equal(got, []string{"1.1 origin-proxy"}) {
				t.Errorf("the Upstream's Via reached the client as %q, want it unchanged", got)
			}
			if got := g.do(t, p, request{Target: "/no-route"}).Header.Get("Via"); got != "" {
				t.Errorf("a generated response carries Via %q", got)
			}
		})
	})
}

// caseServerDate checks the Server and Date row: the Node adds no Server,
// an Upstream's passes unless a headers Policy removes it; an Upstream's
// Date passes unchanged, and a forwarded response without one, and each
// generated response, gets the Node's.
func caseServerDate(t *testing.T, g *gateway) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		r := g.do(t, p, request{Target: upH1.path(bDate), Header: withCase(g.origin.newCase())})
		wantOK(t, r)
		if r.Header.Get("Date") != fixedDate || r.Header.Get("Server") != "conformance-origin" {
			t.Errorf("Date %q Server %q, want the Upstream's unchanged", r.Header.Get("Date"), r.Header.Get("Server"))
		}
		r = g.do(t, p, request{Target: "/policy/" + bDate, Header: withCase(g.origin.newCase())})
		if r.Header.Get("Server") != "" {
			t.Errorf("Server %q after a headers Policy removed it", r.Header.Get("Server"))
		}
		r = g.do(t, p, request{Target: upH1.path(bNoDate), Header: withCase(g.origin.newCase())})
		wantOK(t, r)
		wantNodeDate(t, r)
		r = g.do(t, p, request{Target: "/no-route"})
		wantProblem(t, r, http.StatusNotFound, "RZ-RT-001")
		wantNodeDate(t, r)
		if v, ok := r.Header["Server"]; ok {
			t.Errorf("a generated response carries Server %q", v)
		}
	})
}

// wantNodeDate fails t unless r carries a Date in the HTTP-date form that
// is not the origin's fixed one.
func wantNodeDate(t testing.TB, r *response) {
	t.Helper()
	d := r.Header.Get("Date")
	if _, err := http.ParseTime(d); err != nil || d == fixedDate {
		t.Errorf("Date %q, want the Node's HTTP-date: %v", d, err)
	}
}

// caseAbsoluteForm checks the Absolute-form target row: accepted on
// HTTP/1.1, its authority replaces Host for routing and forwarding; a
// scheme other than the listener's is 400 RZ-RT-017.
func caseAbsoluteForm(t *testing.T, g *gateway) {
	eachProto(t, h1Protos(), func(t *testing.T, p proto) {
		id := g.origin.newCase()
		r := g.do(t, p, request{Target: p.scheme() + "://" + routedHost + "/hosted/" + bOK, Host: "elsewhere.example", Header: withCase(id)})
		wantOK(t, r)
		s := g.origin.only(t, id)
		if got := s.Header.Get("X-Forwarded-Host"); got != routedHost {
			t.Errorf("X-Forwarded-Host %q, want the absolute-form authority %s", got, routedHost)
		}
		if s.Host != g.origin.addr(upH1) || s.RequestURI != "/hosted/"+bOK {
			t.Errorf("the Upstream request was Host %q target %q, want the Endpoint authority and origin-form", s.Host, s.RequestURI)
		}
		other := "http"
		if p.scheme() == "http" {
			other = "https"
		}
		wantProblem(t, g.do(t, p, request{Target: other + "://" + routedHost + "/hosted/" + bOK}), http.StatusBadRequest, "RZ-RT-017")
	})
}

// caseAsteriskForm checks the Asterisk-form target row: OPTIONS * matches
// no Route, 404 RZ-RT-001, never net/http's own 200.
func caseAsteriskForm(t *testing.T, g *gateway) {
	eachProto(t, protos(), func(t *testing.T, p proto) {
		wantProblem(t, g.do(t, p, request{Method: http.MethodOptions, Target: "*"}), http.StatusNotFound, "RZ-RT-001")
	})
}

// caseHostAuthority checks the Host and :authority row: an HTTP/2
// request whose Host names another host than :authority, compared
// lowercased and without a default port, is 400 RZ-RT-017; net/http
// answers an HTTP/1.1 request without Host, or with two, with a plain
// 400; toward the Upstream, Host is the Endpoint authority and the
// client's host travels in X-Forwarded-Host, whatever forwarding fields an
// untrusted client sent.
func caseHostAuthority(t *testing.T, g *gateway) {
	t.Run("http2-host-differs", func(t *testing.T) {
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			id := g.origin.newCase()
			wantProblem(t, g.do(t, p, request{Target: upH1.path(bOK), Header: withCase(id, [2]string{"host", "other.example"})}),
				http.StatusBadRequest, "RZ-RT-017")
			port := ":80"
			if p.isTLS() {
				port = ":443"
			}
			wantOK(t, g.do(t, p, request{
				Target: upH1.path(bOK), Host: "Routed.Conformance.TEST",
				Header: withCase(g.origin.newCase(), [2]string{"host", routedHost + port}),
			}))
		})
	})
	t.Run("http1-host-count", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			wantPlain(t, g.do(t, p, request{Target: upH1.path(bOK), NoHost: true}), http.StatusBadRequest)
			wantPlain(t, g.do(t, p, request{Target: upH1.path(bOK), Header: [][2]string{{"Host", "second.example"}}}), http.StatusBadRequest)
		})
	})
	t.Run("toward-upstream", func(t *testing.T) {
		// The client's own forwarding fields come from a peer outside
		// Gateway.spec.trustedProxies, which the Bundle leaves empty, so
		// they are overwritten, never appended
		// (docs/architecture/08-security-and-identity.md "IP filtering and
		// GeoIP", Client address): X-Forwarded-For is the peer, here
		// loopback, X-Forwarded-Proto the listener's scheme and
		// X-Forwarded-Host the client's host, and Forwarded goes.
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			wantOK(t, g.do(t, p, request{Target: u.path(bOK), Host: "client.example", Header: withCase(id,
				[2]string{"X-Forwarded-Host", "spoofed.example"}, [2]string{"X-Forwarded-For", "203.0.113.9"},
				[2]string{"X-Forwarded-Proto", "gopher"}, [2]string{"Forwarded", "for=203.0.113.9;host=spoofed.example"})}))
			s := g.origin.only(t, id)
			if s.Host != g.origin.addr(u) {
				t.Errorf("the Upstream got Host %q, want the Endpoint authority %s", s.Host, g.origin.addr(u))
			}
			for _, f := range []struct{ name, want string }{
				{"X-Forwarded-Host", "client.example"}, {"X-Forwarded-For", "127.0.0.1"}, {"X-Forwarded-Proto", p.scheme()},
			} {
				if got := s.Header[f.name]; !slices.Equal(got, []string{f.want}) {
					t.Errorf("the Upstream got %s %q, want exactly %q", f.name, got, f.want)
				}
			}
			if v, ok := s.Header["Forwarded"]; ok {
				t.Errorf("the Upstream got the client's Forwarded %q", v)
			}
		})
	})
}

// caseContentLength checks the Content-Length row: equal repeated field
// lines count as one; differing values or a list get a plain 400 from
// net/http, which closes the connection; on HTTP/2 a content-length that
// differs from the DATA payload makes the request malformed and never
// reaches the Upstream complete: net/http resets the stream with
// PROTOCOL_ERROR for more DATA than declared and, from Go 1.27 on, for
// differing content-length fields or content declared on a stream that
// ends with its HEADERS, which behind the Go 1.26 floor's net/http get the
// handler's 400 RZ-RT-017; a stream that ends short gets the INTERNAL_ERROR
// reset of the aborted request, since a handler cannot choose the reset
// code.
func caseContentLength(t *testing.T, g *gateway) {
	t.Run("equal-lines", func(t *testing.T) {
		eachPairing(t, h1Protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			c := g.dialH1(t, p)
			defer c.close()
			raw := "POST " + u.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id +
				"\r\nContent-Length: 5\r\nContent-Length: 5\r\n\r\nhello"
			wantOK(t, c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw)))
			if s := g.origin.only(t, id); string(s.Body) != "hello" {
				t.Errorf("the origin got body %q, want hello", s.Body)
			}
		})
	})
	t.Run("conflicting", func(t *testing.T) {
		eachProto(t, h1Protos(), func(t *testing.T, p proto) {
			for _, cl := range []string{"Content-Length: 5\r\nContent-Length: 6", "Content-Length: 5, 5"} {
				id := g.origin.newCase()
				c := g.dialH1(t, p)
				raw := "POST " + upH1.path(bOK) + " HTTP/1.1\r\nHost: x\r\n" + caseHeader + ": " + id + "\r\n" + cl + "\r\n\r\nhello!"
				wantPlain(t, c.exchange(guard(t, exchangeGuard), http.MethodPost, []byte(raw)), http.StatusBadRequest)
				if !c.waitClosed(5 * time.Second) {
					t.Errorf("%q: the connection stayed open after the 400", cl)
				}
				c.close()
				if n := len(g.origin.requests(id)); n != 0 {
					t.Errorf("%q: the origin saw %d requests", cl, n)
				}
			}
		})
	})
	t.Run("http2-payload-differs", func(t *testing.T) {
		eachProto(t, h2Protos(), func(t *testing.T, p proto) {
			for _, tt := range []struct {
				name string
				body string
				code http2.ErrCode
			}{{"more DATA than declared", "hello!", http2.ErrCodeProtocol}, {"less DATA than declared", "hell", http2.ErrCodeInternal}} {
				id := g.origin.newCase()
				r := g.do(t, p, request{
					Method: http.MethodPost, Target: upH1.path(bOK), Body: []byte(tt.body), Chunked: true,
					Header: withCase(id, [2]string{"content-length", "5"}),
				})
				if !r.WasReset || r.Reset != tt.code || r.Status != 0 {
					t.Errorf("%s: response %v, want RST_STREAM %v without a response", tt.name, r, tt.code)
				}
				for _, s := range g.origin.requests(id) {
					if !s.Truncated && string(s.Body) == tt.body {
						t.Errorf("%s: the origin got the malformed body as a complete request", tt.name)
					}
				}
			}
			// Differing content-length field lines, and content declared on
			// a stream that ends with its HEADERS: Go 1.27's net/http resets
			// the stream with PROTOCOL_ERROR; behind the Go 1.26 floor's,
			// which lets both through, the handler's framing check answers
			// 400 RZ-RT-017.
			strict := strictHTTP2ContentLength(t)
			for _, tt := range []struct {
				name string
				req  func(id string) request
			}{
				{"differing content-length fields", func(id string) request {
					return request{
						Method: http.MethodPost, Target: upH1.path(bOK), Body: []byte("hello"), Chunked: true,
						Header: withCase(id, [2]string{"content-length", "5"}, [2]string{"content-length", "6"}),
					}
				}},
				{"content declared without DATA", func(id string) request {
					return request{Method: http.MethodPost, Target: upH1.path(bOK), Header: withCase(id, [2]string{"content-length", "5"})}
				}},
			} {
				id := g.origin.newCase()
				r := g.do(t, p, tt.req(id))
				switch {
				case strict && (!r.WasReset || r.Reset != http2.ErrCodeProtocol || r.Status != 0):
					t.Errorf("%s: response %v, want RST_STREAM PROTOCOL_ERROR from net/http", tt.name, r)
				case !strict && (r.Status != http.StatusBadRequest || !r.isProblem()):
					t.Errorf("%s: response %v, want the handler's 400 RZ-RT-017 behind the Go 1.26 floor's net/http", tt.name, r)
				case !strict:
					wantProblem(t, r, http.StatusBadRequest, "RZ-RT-017")
				}
				if n := len(g.origin.requests(id)); n != 0 {
					t.Errorf("%s: the origin saw %d requests", tt.name, n)
				}
			}
		})
	})
}

// caseHeadAnd304 checks the HEAD and 304 row: methods match literally, so
// a HEAD meets a Route whose methods list HEAD or that sets none; a
// forwarded HEAD stays HEAD and its response keeps the Upstream's framing
// fields without content; a 304 has no content; a generated response to a
// HEAD sends its header section only.
func caseHeadAnd304(t *testing.T, g *gateway) {
	t.Run("methods-match-literally", func(t *testing.T) {
		eachProto(t, protos(), func(t *testing.T, p proto) {
			r := g.do(t, p, request{Method: http.MethodHead, Target: "/methods/get"})
			wantProblem(t, r, http.StatusNotFound, "RZ-RT-001")
			if len(r.Body) != 0 || r.Header.Get("Content-Length") == "" {
				t.Errorf("generated response to HEAD: body %q Content-Length %q, want the header section only", r.Body, r.Header.Get("Content-Length"))
			}
			for _, path := range []string{"/methods/head", "/methods/any"} {
				id := g.origin.newCase()
				wantOK(t, g.do(t, p, request{Method: http.MethodHead, Target: path, Header: withCase(id)}))
				if m := g.origin.only(t, id).Method; m != http.MethodHead {
					t.Errorf("%s: the Upstream got %s, want HEAD", path, m)
				}
			}
			wantProblem(t, g.do(t, p, request{Target: "/methods/head"}), http.StatusNotFound, "RZ-RT-001")
		})
	})
	t.Run("forwarded-head", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			id := g.origin.newCase()
			r := g.headTwice(t, p, request{Method: http.MethodHead, Target: u.path(bSized), Header: withCase(id)})
			wantOK(t, r)
			if r.Header.Get("Content-Length") != "1234" || len(r.Body) != 0 {
				t.Errorf("HEAD response Content-Length %q body %d bytes, want the Upstream's 1234 without content",
					r.Header.Get("Content-Length"), len(r.Body))
			}
			if m := g.origin.only(t, id).Method; m != http.MethodHead {
				t.Errorf("the Upstream got %s, want HEAD", m)
			}
		})
	})
	t.Run("304", func(t *testing.T) {
		eachPairing(t, protos(), func(t *testing.T, p proto, u upstream) {
			r := g.do(t, p, request{Target: u.path(bNotModified), Header: withCase(g.origin.newCase(), [2]string{"If-None-Match", `"v1"`})})
			if r.Status != http.StatusNotModified || len(r.Body) != 0 || r.Header.Get("ETag") != `"v1"` {
				t.Errorf("response %v ETag %q, want 304 without content", r, r.Header.Get("ETag"))
			}
		})
	})
}

// headTwice sends req, a HEAD, then a GET on the same connection on
// HTTP/1.x, so a body wrongly sent after the HEAD response would break
// the second response; it returns the HEAD's response.
func (g *gateway) headTwice(t testing.TB, p proto, req request) *response {
	t.Helper()
	if p.isH2() {
		return g.do(t, p, req)
	}
	c := g.dialH1(t, p)
	defer c.close()
	ctx := guard(t, exchangeGuard)
	c.write(req.h1Bytes(g.addr(p)))
	first := c.read(ctx, http.MethodHead)
	follow := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
	c.write(follow.h1Bytes(g.addr(p)))
	if second := c.read(ctx, http.MethodGet); second.Status != http.StatusOK || string(second.Body) != "ok" {
		t.Errorf("the response after the HEAD's is %v: the HEAD response carried content", second)
	}
	return first
}
