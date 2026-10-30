// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package protocol

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

// boundedDoc is the document of the HTTP/2 rows of Bounded resources.
const boundedDoc = "docs/architecture/03-data-plane.md#bounded-resources"

// HTTP/2 settings of the client listeners
// (docs/architecture/03-data-plane.md "Bounded resources").
const (
	h2MaxStreams        = 250
	h2StreamWindow      = 64 << 10
	h2ConnWindow        = 256 << 10
	h2ReadFrameSize     = 16 << 10
	floodPairs          = 20000
	floodRequestsPerRun = 2000
)

// TestHTTP2Frames runs the Ruralz-written HTTP/2 frame cases on the
// golang.org/x/net/http2 Framer, for the HTTP/2 rows of
// docs/architecture/03-data-plane.md "Bounded resources" and the settings
// the Node advertises, on h2c and HTTP/2 over TLS. RST_STREAM on deadline
// expiry is in TestTimeouts, which runs in real time, and one GOAWAY at
// Drain in TestDrain.
func TestHTTP2Frames(t *testing.T) {
	t.Parallel()
	g := startGateway(t, bundleOptions{})
	for _, c := range []struct {
		row string
		run func(t *testing.T, g *gateway, p proto)
	}{
		{"SETTINGS the Node advertises", caseSettings},
		{"the 251st concurrent stream refused", caseStreamLimit},
		{"the receive buffers", caseReceiveBuffers},
		{"16 KiB frames", caseFrameSize},
		{"the header block", caseHeaderBlock},
		{"CONTINUATION flood", caseContinuationFlood},
		{"rapid-reset flood", caseRapidReset},
	} {
		t.Run(c.row, func(t *testing.T) {
			t.Logf("rule: %s, %s", boundedDoc, c.row)
			eachProto(t, h2Protos(), func(t *testing.T, p proto) { c.run(t, g, p) })
		})
	}
}

// caseSettings checks the SETTINGS the Node advertises and its first
// connection WINDOW_UPDATE: 250 concurrent streams, a 64 KiB stream window
// and a 256 KiB connection window of unread request data, 16 KiB frames,
// and a header list size of at least the 256 KiB header block ceiling.
func caseSettings(t *testing.T, g *gateway, p proto) {
	c := g.dialH2(t, p)
	defer c.close()
	for _, tt := range []struct {
		id   http2.SettingID
		want uint32
	}{
		{http2.SettingMaxConcurrentStreams, h2MaxStreams},
		{http2.SettingInitialWindowSize, h2StreamWindow},
		{http2.SettingMaxFrameSize, h2ReadFrameSize},
	} {
		if got, ok := c.settings[tt.id]; !ok || got != tt.want {
			t.Errorf("%v = %d (sent %t), want %d", tt.id, got, ok, tt.want)
		}
	}
	if got := c.settings[http2.SettingMaxHeaderListSize]; got < processHeaderCeiling {
		t.Errorf("SETTINGS_MAX_HEADER_LIST_SIZE = %d, want at least %d", got, processHeaderCeiling)
	}
	// The connection WINDOW_UPDATE follows the SETTINGS; the PING's
	// acknowledgment comes after it.
	if err := c.ping(guard(t, exchangeGuard)); err != nil {
		t.Fatal(err)
	}
	if got := c.connIncrement.Load(); got != h2ConnWindow-h2DefaultWindow {
		t.Errorf("connection WINDOW_UPDATE = %d, want %d, a %d-byte connection window", got, h2ConnWindow-h2DefaultWindow, h2ConnWindow)
	}
}

// collectAll reads the events of every stream in ids until each one's
// response ended, as collect does for one.
func (c *h2Conn) collectAll(ctx context.Context, ids []uint32) map[uint32]*response {
	out := make(map[uint32]*response, len(ids))
	open := map[uint32]bool{}
	for _, id := range ids {
		out[id] = &response{Proto: "HTTP/2.0"}
		open[id] = true
	}
	for len(open) > 0 {
		ev, ok := c.next(ctx)
		if !ok {
			for id := range open {
				out[id].Broken = ev.Err
			}
			return out
		}
		r, mine := out[ev.StreamID]
		if !mine || !open[ev.StreamID] {
			continue
		}
		switch ev.Type {
		case http2.FrameHeaders:
			switch st := ev.status(); {
			case r.Status == 0 && st >= 100 && st < 200:
				r.Interim = append(r.Interim, interim{Status: st, Header: ev.header()})
			case r.Status == 0:
				r.Status, r.Header = st, ev.header()
			default:
				r.Trailer = ev.header()
			}
		case http2.FrameData:
			r.Body = append(r.Body, ev.Data...)
		case http2.FrameRSTStream:
			r.WasReset, r.Reset = true, ev.Code
			delete(open, ev.StreamID)
			continue
		default:
		}
		if ev.EndStream {
			delete(open, ev.StreamID)
		}
	}
	return out
}

// holdWait bounds the wait for the origin to hold every request of a case.
const holdWait = 60 * time.Second

// caseStreamLimit checks HTTP2Config.MaxConcurrentStreams: with 250
// streams open the 251st is reset, refused before any handler (RST_STREAM
// PROTOCOL_ERROR once the client acknowledged the SETTINGS that announced
// the limit, REFUSED_STREAM before), while the 250 complete and the
// connection keeps serving.
func caseStreamLimit(t *testing.T, g *gateway, p proto) {
	c := g.dialH2(t, p)
	defer c.close()
	id := g.origin.newCase()
	req := request{Target: upH1.path(bHold), Header: withCase(id)}
	ids := make([]uint32, h2MaxStreams)
	for i := range ids {
		ids[i] = c.newStream()
		if err := c.writeHeaders(ids[i], req.h2Fields(p, g.addr(p)), true); err != nil {
			t.Fatal(err)
		}
	}
	wait, cancel := context.WithTimeout(t.Context(), holdWait)
	defer cancel()
	if _, err := g.origin.waitRequests(wait, id, h2MaxStreams); err != nil {
		t.Fatal(err)
	}
	extra := c.newStream()
	if err := c.writeHeaders(extra, req.h2Fields(p, g.addr(p)), true); err != nil {
		t.Fatal(err)
	}
	ev, ok := c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool { return e.StreamID == extra || e.Type == http2.FrameGoAway })
	if !ok || ev.Type != http2.FrameRSTStream || (ev.Code != http2.ErrCodeProtocol && ev.Code != http2.ErrCodeRefusedStream) {
		t.Fatalf("stream %d beyond the limit got %+v (ok %t), want RST_STREAM PROTOCOL_ERROR or REFUSED_STREAM", extra, ev, ok)
	}
	if n := len(g.origin.requests(id)); n != h2MaxStreams {
		t.Errorf("the origin saw %d requests, want %d: the refused stream reached it", n, h2MaxStreams)
	}
	g.origin.releaseCase(id)
	for sid, r := range c.collectAll(guard(t, holdWait), ids) {
		if r.Status != http.StatusOK {
			t.Fatalf("stream %d: %v, want 200", sid, r)
		}
	}
	wantOK(t, c.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}))
}

// caseReceiveBuffers checks HTTP2Config.MaxReceiveBufferPerStream and
// MaxReceiveBufferPerConnection: while no handler reads the bodies, a
// stream may send 64 KiB and a connection 256 KiB, and a byte more resets
// the stream that sent it with FLOW_CONTROL_ERROR. The bodies stay unread
// because every attempt waits on the blackhole Upstream's TLS handshake,
// whose timeout runs on the Node clock the test holds still.
func caseReceiveBuffers(t *testing.T, g *gateway, p proto) {
	needsTestClock(t)
	t.Run("stream", func(t *testing.T) {
		c := g.dialH2(t, p)
		defer c.close()
		id := c.newStream()
		fields := request{Method: http.MethodPost, Target: "/blackhole/" + bOK}.h2Fields(p, g.addr(p))
		if err := c.writeHeaders(id, fields, false); err != nil {
			t.Fatal(err)
		}
		writeUnread(t, c, id, h2StreamWindow)
		if err := c.writeData(id, false, []byte{0}); err != nil {
			t.Fatal(err)
		}
		ev, ok := c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool { return e.Type == http2.FrameRSTStream || e.Type == http2.FrameGoAway })
		if !ok || ev.Type != http2.FrameRSTStream || ev.StreamID != id || ev.Code != http2.ErrCodeFlowControl {
			t.Fatalf("one byte past the stream window got %+v (ok %t), want RST_STREAM %d FLOW_CONTROL_ERROR", ev, ok, id)
		}
	})
	t.Run("connection", func(t *testing.T) {
		c := g.dialH2(t, p)
		defer c.close()
		fields := request{Method: http.MethodPost, Target: "/blackhole/" + bOK}.h2Fields(p, g.addr(p))
		var ids []uint32
		for range h2ConnWindow / h2StreamWindow {
			id := c.newStream()
			ids = append(ids, id)
			if err := c.writeHeaders(id, fields, false); err != nil {
				t.Fatal(err)
			}
			writeUnread(t, c, id, h2StreamWindow)
		}
		last := c.newStream()
		if err := c.writeHeaders(last, fields, false); err != nil {
			t.Fatal(err)
		}
		if err := c.writeData(last, false, []byte{0}); err != nil {
			t.Fatal(err)
		}
		ev, ok := c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool { return e.Type == http2.FrameRSTStream || e.Type == http2.FrameGoAway })
		if !ok || ev.Type != http2.FrameRSTStream || ev.StreamID != last || ev.Code != http2.ErrCodeFlowControl {
			t.Fatalf("one byte past the connection window got %+v (ok %t), want RST_STREAM %d FLOW_CONTROL_ERROR; streams %v sent within it", ev, ok, last, ids)
		}
	})
}

// writeUnread writes n bytes of DATA on stream id in maximum-size frames,
// without waiting for the flow-control windows: the case asserts them.
func writeUnread(t testing.TB, c *h2Conn, id uint32, n int) {
	t.Helper()
	chunk := make([]byte, h2MaxFrame)
	for n > 0 {
		k := min(n, len(chunk))
		if err := c.writeData(id, false, chunk[:k]); err != nil {
			t.Fatal(err)
		}
		n -= k
	}
}

// caseFrameSize checks HTTP2Config.MaxReadFrameSize: a 16 KiB frame is
// read, and a frame one byte longer ends the connection with GOAWAY
// FRAME_SIZE_ERROR.
func caseFrameSize(t *testing.T, g *gateway, p proto) {
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	oid := g.origin.newCase()
	fields := request{Method: http.MethodPost, Target: upH1.path(bOK), Header: withCase(oid)}.h2Fields(p, g.addr(p))
	if err := c.writeHeaders(id, fields, false); err != nil {
		t.Fatal(err)
	}
	if err := c.writeData(id, true, make([]byte, h2ReadFrameSize)); err != nil {
		t.Fatal(err)
	}
	wantOK(t, c.collect(guard(t, exchangeGuard), id))
	if s := g.origin.only(t, oid); len(s.Body) != h2ReadFrameSize {
		t.Errorf("the origin got %d bytes, want %d", len(s.Body), h2ReadFrameSize)
	}
	id = c.newStream()
	if err := c.writeHeaders(id, fields, false); err != nil {
		t.Fatal(err)
	}
	if err := c.writeData(id, true, make([]byte, h2ReadFrameSize+1)); err != nil {
		t.Fatal(err)
	}
	ev, ok := c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool { return e.Type == http2.FrameGoAway })
	if !ok || ev.Code != http2.ErrCodeFrameSize {
		t.Fatalf("a %d-byte frame got %+v (ok %t), want GOAWAY FRAME_SIZE_ERROR", h2ReadFrameSize+1, ev, ok)
	}
	wantConnEnd(t, c)
}

// wantConnEnd fails t unless the server ends the connection.
func wantConnEnd(t testing.TB, c *h2Conn) {
	t.Helper()
	if ev, ok := c.waitEvent(guard(t, exchangeGuard), func(h2Event) bool { return false }); ok || ev.Err == nil || ev.Err == context.DeadlineExceeded {
		t.Errorf("the connection did not end: %+v", ev)
	}
}

// caseHeaderBlock checks the header block ceiling on HTTP/2: a header list
// above it gets net/http's plain 431 on its stream, and the connection
// keeps serving.
func caseHeaderBlock(t *testing.T, g *gateway, p proto) {
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	if err := c.writeBlock(id, overCeilingBlock(p, g.addr(p), c.settings[http2.SettingMaxHeaderListSize]), true); err != nil {
		t.Fatal(err)
	}
	wantPlain(t, c.collect(guard(t, exchangeGuard), id), http.StatusRequestHeaderFieldsTooLarge)
	wantOK(t, c.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}))
}

// caseContinuationFlood checks a CONTINUATION flood: a header block that
// never ends, past the header list ceiling, ends with the flooding
// connection closed by GOAWAY, while another connection keeps being
// served.
func caseContinuationFlood(t *testing.T, g *gateway, p proto) {
	other := g.dialH2(t, p)
	defer other.close()
	c := g.dialH2(t, p)
	defer c.close()
	id := c.newStream()
	start := request{Target: upH1.path(bOK)}.h2Fields(p, g.addr(p))
	if err := c.writeFrames(func(fr *http2.Framer) error {
		return fr.WriteHeaders(http2.HeadersFrameParam{StreamID: id, BlockFragment: c.encode(start), EndStream: true})
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		frag := appendLiteral(nil, "x-flood", strings.Repeat("f", 8000))
		limit := int(c.settings[http2.SettingMaxHeaderListSize])
		for sent := 0; sent < 4*limit; sent += len(frag) {
			if err := c.writeFrames(func(fr *http2.Framer) error { return fr.WriteContinuation(id, false, frag) }); err != nil {
				return
			}
		}
	}()
	ev, ok := c.waitEvent(guard(t, exchangeGuard), func(e h2Event) bool { return e.Type == http2.FrameGoAway })
	if !ok {
		t.Fatalf("no GOAWAY for a CONTINUATION flood: %v", ev.Err)
	}
	t.Logf("GOAWAY %v, last stream %d", ev.Code, ev.LastStreamID)
	wantConnEnd(t, c)
	<-done
	wantOK(t, other.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}))
}

// caseRapidReset checks a rapid-reset flood, streams opened and reset at
// once: net/http runs at most 250 of the connection's handlers at a time
// (HTTP2Config.MaxConcurrentStreams), never starts the handler of a stream
// reset while it waits, and cancels a started one at its reset, so every
// held Upstream request ends; it closes the connection with GOAWAY
// ENHANCE_YOUR_CALM only when more than 1,000 handlers wait to start, and
// otherwise the connection keeps serving. Another connection keeps being
// served throughout.
func caseRapidReset(t *testing.T, g *gateway, p proto) {
	other := g.dialH2(t, p)
	defer other.close()
	c := g.dialH2(t, p)
	defer c.close()
	id := g.origin.newCase()
	fields := request{Target: upH1.path(bHold), Header: withCase(id)}.h2Fields(p, g.addr(p))
	started, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		for i := range floodPairs / floodRequestsPerRun {
			if err := c.writeFrames(func(fr *http2.Framer) error {
				for range floodRequestsPerRun {
					sid := c.newStream()
					if err := fr.WriteHeaders(http2.HeadersFrameParam{StreamID: sid, BlockFragment: c.encode(fields), EndStream: true, EndHeaders: true}); err != nil {
						return err
					}
					if err := fr.WriteRSTStream(sid, http2.ErrCodeCancel); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				return
			}
			if i == 0 {
				close(started)
			}
		}
	}()
	select {
	case <-started:
	case <-done:
	}
	wantOK(t, other.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())}))
	<-done
	// The flooding connection either ended with ENHANCE_YOUR_CALM or
	// still serves: the server reads this stream after every flood frame.
	r := c.roundTrip(guard(t, exchangeGuard), p, request{Target: upH1.path(bOK), Header: withCase(g.origin.newCase())})
	switch {
	case r.SawGoAway && r.GoAway != http2.ErrCodeEnhanceYourCalm:
		t.Errorf("the flooding connection got GOAWAY %v, want ENHANCE_YOUR_CALM or none", r.GoAway)
	case !r.SawGoAway && r.Status != http.StatusOK:
		t.Errorf("the flooding connection: %v, want 200 or GOAWAY ENHANCE_YOUR_CALM", r)
	}
	t.Logf("after %d reset streams the flooding connection answered %v", floodPairs, r)
	if n := g.origin.maxHeld(id); n > h2MaxStreams {
		t.Errorf("the origin held %d requests of the flood at once, want at most %d handlers per connection", n, h2MaxStreams)
	}
	wait, cancel := context.WithTimeout(t.Context(), holdWait)
	defer cancel()
	if err := g.origin.waitHeld(wait, id, 0); err != nil {
		t.Errorf("held requests of reset streams did not end: %v", err)
	}
}
