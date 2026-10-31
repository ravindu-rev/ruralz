// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/gateway/admin"
)

// Deadlines of docs/architecture/03-data-plane.md "Bounded resources"
// (targets) and the Route timeout of the /deadline/ Routes.
const (
	readHeaderTimeout = 10 * time.Second
	idleTimeout       = 120 * time.Second
	routeDeadline     = 10 * time.Second
	preRoutingWrite   = 5 * time.Second
	// expirySlack is how late an expiry may come (target).
	expirySlack = 2 * time.Second
	// deadlineWait bounds the wait for one expiry, a hang guard only.
	deadlineWait = idleTimeout + 30*time.Second
)

// TestTimeouts checks the deadlines of docs/architecture/03-data-plane.md
// "Bounded resources" in real time and in parallel, each expiry between its
// value and its value plus 2 s: ReadHeaderTimeout 10 s (which net/http also
// applies to the TLS handshake), IdleTimeout 120 s, the body read and
// response write deadlines (10 s from admission, then the Route timeout,
// here 10 s), whose expiry resets the HTTP/2 stream or closes the HTTP/1.1
// connection, and the 5 s pre-routing write deadline. The Go runtime
// enforces socket deadlines on the real clock in every build, so the
// controllable-clock build changes nothing here. Afterwards no request
// holds a pin: every blocked handler was freed. The test is not parallel
// with the suite's other tests: it runs before them, so no flood shares
// the machine with its real-time measurements, while its own cases run in
// parallel.
func TestTimeouts(t *testing.T) {
	g := startGateway(t, bundleOptions{routeTimeout: "10s"})
	t.Run("deadlines", func(t *testing.T) {
		for _, c := range []struct {
			name string
			run  func(t *testing.T, g *gateway)
		}{
			{"ReadHeaderTimeout/h1", func(t *testing.T, g *gateway) { caseHeaderTimeout(t, g, h1) }},
			{"ReadHeaderTimeout/h1tls", func(t *testing.T, g *gateway) { caseHeaderTimeout(t, g, h1TLS) }},
			{"ReadHeaderTimeout/tls-handshake", caseHandshakeTimeout},
			{"IdleTimeout/h1", func(t *testing.T, g *gateway) { caseIdleH1(t, g, h1) }},
			{"IdleTimeout/h1tls", func(t *testing.T, g *gateway) { caseIdleH1(t, g, h1TLS) }},
			{"IdleTimeout/h2c", func(t *testing.T, g *gateway) { caseIdleH2(t, g, h2c) }},
			{"IdleTimeout/h2", func(t *testing.T, g *gateway) { caseIdleH2(t, g, h2TLS) }},
			{"body-read/h1", func(t *testing.T, g *gateway) { caseBodyReadH1(t, g, h1) }},
			{"body-read/h1tls", func(t *testing.T, g *gateway) { caseBodyReadH1(t, g, h1TLS) }},
			{"body-read/h2c", func(t *testing.T, g *gateway) { caseBodyReadH2(t, g, h2c) }},
			{"body-read/h2", func(t *testing.T, g *gateway) { caseBodyReadH2(t, g, h2TLS) }},
			{"response-write/h1", func(t *testing.T, g *gateway) { caseWriteH1(t, g, h1) }},
			{"response-write/h1tls", func(t *testing.T, g *gateway) { caseWriteH1(t, g, h1TLS) }},
			{"response-write/h2c", func(t *testing.T, g *gateway) {
				caseWriteH2(t, g, h2c, "/deadline/"+bBig+"?size=67108864", routeDeadline)
			}},
			{"response-write/h2", func(t *testing.T, g *gateway) {
				caseWriteH2(t, g, h2TLS, "/deadline/"+bBig+"?size=67108864", routeDeadline)
			}},
			{"pre-routing-write/h2c", func(t *testing.T, g *gateway) { caseWriteH2(t, g, h2c, "/no-route", preRoutingWrite) }},
			{"pre-routing-write/h2", func(t *testing.T, g *gateway) { caseWriteH2(t, g, h2TLS, "/no-route", preRoutingWrite) }},
		} {
			t.Run(c.name, func(t *testing.T) {
				t.Parallel()
				c.run(t, g)
			})
		}
	})
	snaps := struct {
		Snapshots []adminapi.SnapshotInfo `json:"snapshots"`
	}{}
	resp, err := g.node.Admin(guard(t, exchangeGuard), admin.PathSnapshots)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(resp.Body, &snaps); err != nil {
		t.Fatalf("/debug/snapshots %q: %v", resp.Body, err)
	}
	for _, s := range snaps.Snapshots {
		if s.Pins != 0 {
			t.Errorf("snapshot %s holds %d pins after every deadline expired: a blocked handler was not freed", s.Revision, s.Pins)
		}
	}
}

// wantExpiry fails t unless took lies within [value, value+expirySlack].
func wantExpiry(t testing.TB, what string, took, value time.Duration) {
	t.Helper()
	if took < value || took > value+expirySlack {
		t.Errorf("%s expired after %v, want between %v and %v", what, took.Round(time.Millisecond), value, value+expirySlack)
	}
}

// caseHeaderTimeout checks ReadHeaderTimeout: a request head that never
// ends closes the connection 10 s after it began.
func caseHeaderTimeout(t *testing.T, g *gateway, p proto) {
	start := time.Now()
	c := g.dialH1(t, p)
	defer c.close()
	c.write([]byte("GET " + upH1.path(bOK) + " HTTP/1.1\r\nHost: x\r\n"))
	closed := c.waitClosed(deadlineWait)
	if !closed {
		t.Fatalf("the connection stayed open %v with an unfinished request head", deadlineWait)
	}
	wantExpiry(t, "ReadHeaderTimeout", time.Since(start), readHeaderTimeout)
}

// caseHandshakeTimeout checks that net/http applies ReadHeaderTimeout to
// the TLS handshake: a client that never sends its ClientHello is closed
// 10 s after it connected.
func caseHandshakeTimeout(t *testing.T, g *gateway) {
	start := time.Now()
	c := dialTCP(t, g.addr(h1TLS))
	defer func() { _ = c.Close() }()
	_ = c.SetReadDeadline(time.Now().Add(deadlineWait))
	_, err := io.Copy(io.Discard, c)
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		t.Fatalf("the connection stayed open %v without a handshake", deadlineWait)
	}
	wantExpiry(t, "the TLS handshake deadline", time.Since(start), readHeaderTimeout)
}

// caseIdleH1 checks IdleTimeout on HTTP/1.1: a keep-alive connection that
// sends no request after its first closes 120 s after it went idle.
func caseIdleH1(t *testing.T, g *gateway, p proto) {
	c := g.dialH1(t, p)
	defer c.close()
	req := request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}
	start := time.Now()
	c.write(req.h1Bytes(g.addr(p)))
	wantOK(t, c.read(guard(t, exchangeGuard), req.method()))
	closed := c.waitClosed(deadlineWait)
	if !closed {
		t.Fatalf("the idle connection stayed open %v", deadlineWait)
	}
	wantExpiry(t, "IdleTimeout", time.Since(start), idleTimeout)
}

// caseIdleH2 checks IdleTimeout on HTTP/2: a connection without streams
// after its first gets GOAWAY NO_ERROR and closes 120 s after it went
// idle.
func caseIdleH2(t *testing.T, g *gateway, p proto) {
	c := g.dialH2(t, p)
	defer c.close()
	start := time.Now()
	wantOK(t, c.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}))
	ev, ok := c.waitEvent(guard(t, deadlineWait), func(e h2Event) bool { return e.Type == http2.FrameGoAway })
	if !ok {
		t.Fatalf("no GOAWAY on an idle connection: %v", ev.Err)
	}
	if ev.Code != http2.ErrCodeNo {
		t.Errorf("idle GOAWAY %v, want NO_ERROR", ev.Code)
	}
	wantExpiry(t, "IdleTimeout", ev.At.Sub(start), idleTimeout)
	wantConnEnd(t, c)
}

// caseBodyReadH1 checks the body read deadline on HTTP/1.1: a body that
// stops arriving is cut at the Route timeout, and the request aborts with
// the connection closed and no response.
func caseBodyReadH1(t *testing.T, g *gateway, p proto) {
	start := time.Now()
	c := g.dialH1(t, p)
	defer c.close()
	req := request{Method: http.MethodPost, Target: "/deadline-gated/" + bOK, Header: withCase(g.origin.newCase()), Body: make([]byte, 100)}
	c.write(append(req.h1Head(g.addr(p)), make([]byte, 10)...))
	r := c.read(guard(t, deadlineWait), req.method())
	took := time.Since(start)
	if r.Status != 0 {
		t.Errorf("response %v to a stalled body, want the connection closed without one", r)
	}
	wantExpiry(t, "the body read deadline", took, routeDeadline)
}

// caseBodyReadH2 checks the body read deadline on HTTP/2: a body that stops
// arriving resets its stream at the Route timeout, RST_STREAM on deadline
// expiry.
func caseBodyReadH2(t *testing.T, g *gateway, p proto) {
	start := time.Now()
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	req := request{Method: http.MethodPost, Target: "/deadline-gated/" + bOK, Header: withCase(g.origin.newCase()), Body: make([]byte, 100)}
	if err := c.writeHeaders(id, req.h2Fields(p, g.addr(p)), false); err != nil {
		t.Fatal(err)
	}
	if err := c.writeData(id, false, make([]byte, 10)); err != nil {
		t.Fatal(err)
	}
	r := c.collect(guard(t, deadlineWait), id)
	if !r.WasReset {
		t.Fatalf("response %v to a stalled body, want RST_STREAM", r)
	}
	wantExpiry(t, "the body read deadline", time.Since(start), routeDeadline)
}

// caseWriteH1 checks the response write deadline on HTTP/1.1: a client
// that stops reading a response larger than the socket buffers is cut at
// the Route timeout, the connection closed. The client must not read
// meanwhile, so the expiry is when the request's access record appears,
// which the Node writes as the request ends; the record's own duration is
// on the Node's clock, which the controllable-clock build holds still. The
// client keeps its default receive buffer, so draining what the kernel
// holds afterwards is quick.
func caseWriteH1(t *testing.T, g *gateway, p proto) {
	trace := newTraceID(t)
	nc := g.dial(t, p, "http/1.1")
	c := &h1Conn{t: t, c: nc, br: bufio.NewReader(nc)}
	defer c.close()
	req := request{Target: "/deadline/" + bBig + "?size=67108864", Header: withCase(g.origin.newCase(),
		[2]string{"Traceparent", "00-" + trace + "-00f067aa0ba902b7-00"})}
	start := time.Now()
	c.write(req.h1Bytes(g.addr(p)))
	re := regexp.MustCompile(`"msg":"access","trace_id":"` + trace + `"`)
	if _, err := g.node.Process().WaitOutput(guard(t, deadlineWait), re); err != nil {
		t.Fatalf("no access record of the stalled response: %v", err)
	}
	wantExpiry(t, "the response write deadline", time.Since(start), routeDeadline)
	if !c.waitClosed(exchangeGuard) {
		t.Error("the connection stayed open after the write deadline")
	}
}

// caseWriteH2 checks a write deadline on HTTP/2: with a zero stream window
// the response's DATA cannot go out, and the stream resets when the
// deadline expires, RST_STREAM on deadline expiry: the Route timeout for a
// matched Route, the 5 s pre-routing deadline for a request that matches
// none.
func caseWriteH2(t *testing.T, g *gateway, p proto, target string, deadline time.Duration) {
	start := time.Now()
	c := g.dialH2(t, p, h2Options{Settings: []http2.Setting{{ID: http2.SettingInitialWindowSize, Val: 0}}, NoWindowUpdate: true})
	defer c.close()
	id := c.newStream()
	if err := c.writeHeaders(id, request{Target: target, Header: withCase(g.origin.newCase())}.h2Fields(p, g.addr(p)), true); err != nil {
		t.Fatal(err)
	}
	r := c.collect(guard(t, deadlineWait), id)
	if !r.WasReset || r.Status == 0 {
		t.Fatalf("response %v, want response headers, then RST_STREAM", r)
	}
	wantExpiry(t, "the write deadline", time.Since(start), deadline)
}

// newTraceID returns a random W3C trace ID.
func newTraceID(t testing.TB) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(b[:])
}
