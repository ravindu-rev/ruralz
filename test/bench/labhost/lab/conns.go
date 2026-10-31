// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// Connection kinds of the conns service: the harness drivers of C1's idle
// connections, C2's handshake burst, the pre-routing writes run and the
// slow-header flood of the constants runs
// (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Scenarios", "Constants the suite verifies").
const (
	// ConnsIdleHTTP1 sends one HTTP/1.1 request, reads its response and
	// idles on the keep-alive connection: C1's cleartext run.
	ConnsIdleHTTP1 = "idle-http1"
	// ConnsIdleH2 completes the TLS handshake with ALPN h2, exchanges the
	// HTTP/2 preface and SETTINGS and idles: C1's TLS run and C2's burst.
	ConnsIdleH2 = "idle-h2"
	// ConnsStallH2C speaks h2c by prior knowledge with a zero initial
	// stream window, sends one request and never reads its body, so the
	// Node's write blocks: the pre-routing writes run's clients that never
	// read RZ-RT-001.
	ConnsStallH2C = "stall-h2c"
	// ConnsSlowHeader sends a request line and one header field of
	// HeaderBytes without ending the header block: the slow-header flood,
	// which the Node's ReadHeaderTimeout ends.
	ConnsSlowHeader = "slow-header"
)

// ConnsConfig configures the conns service: Count connections to Target
// of one kind, the i-th attempted at start + i/Rate (all at once for a
// Rate of 0), each held open until the service closes.
type ConnsConfig struct {
	Kind   string
	Target string
	// Count is the connections to open.
	Count int
	// Rate is the attempts per second; 0 attempts every connection at
	// once.
	Rate float64
	// CAFile verifies the TLS listener; ServerName is the name checked.
	CAFile, ServerName string
	// Host and Path form the request of the HTTP/1.1, h2c and slow-header
	// kinds; Host defaults to Target.
	Host, Path string
	// HeaderBytes is the slow-header field's value size.
	HeaderBytes int
	// Timeout bounds each connection's setup: connect, handshake and the
	// first exchange; default 30 s.
	Timeout time.Duration
}

// ConnsStats are the conns service's counters.
type ConnsStats struct {
	// Attempted, Opened and Failed count connection setups; Open is the
	// connections held now and ClosedByPeer those the Node closed after
	// their setup.
	Attempted    int64 `json:"attempted"`
	Opened       int64 `json:"opened"`
	Failed       int64 `json:"failed"`
	Open         int64 `json:"open"`
	ClosedByPeer int64 `json:"closedByPeer"`
	// Done reports every connection attempted and set up or failed.
	Done bool `json:"done"`
	// Setup is the per-connection setup time, connect to the end of the
	// first exchange (the TLS handshake for idle-h2), HdrHistogram V2,
	// base64.
	Setup string `json:"setup"`
	// SpreadNs is the time from the first attempt to the last setup.
	Spread time.Duration `json:"spreadNs"`
	// Responded counts stall-h2c streams that got their response HEADERS,
	// Aborted those reset without a response, and Expired those reset
	// after their HEADERS; ExpiredAfter is the time from the request to
	// each such reset, HdrHistogram V2, base64.
	Responded    int64  `json:"responded"`
	Aborted      int64  `json:"aborted"`
	Expired      int64  `json:"expired"`
	ExpiredAfter string `json:"expiredAfter"`
	// ClosedAfter is the time until the Node closed each connection,
	// HdrHistogram V2, base64: from the end of the idle kinds' first
	// exchange, and from the connection's start for the stall-h2c and
	// slow-header kinds, whose deadlines (the pre-routing write deadline,
	// ReadHeaderTimeout) run from the request.
	ClosedAfter string `json:"closedAfter"`
	// Errors are the first setup failures, one line each.
	Errors []string `json:"errors,omitempty"`
}

// maxConnsErrors bounds ConnsStats.Errors.
const maxConnsErrors = 10

// h2Preface is the HTTP/2 client connection preface.
const h2Preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"

// HTTP/2 frame types and flags the conns service reads and writes (RFC
// 9113 section 6).
const (
	h2Data         = 0x0
	h2Headers      = 0x1
	h2RSTStream    = 0x3
	h2Settings     = 0x4
	h2Ping         = 0x6
	h2GoAway       = 0x7
	h2FlagAck      = 0x1
	h2FlagEndStrm  = 0x1
	h2FlagEndHdrs  = 0x4
	h2SettingsPush = 0x2
	h2SettingsWin  = 0x4
	h2MaxFrame     = 1 << 24
)

// Conns is the conns service.
type Conns struct {
	cfg  ConnsConfig
	tls  *tls.Config
	cpu  *cpuMeter
	stop chan struct{}
	wg   sync.WaitGroup

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	setup   *loadgen.Histogram
	expired *loadgen.Histogram
	closed  *loadgen.Histogram
	errs    []string
	first   time.Time
	last    time.Time

	attempted, opened, failed, open, closedByPeer atomic.Int64
	responded, aborted, expiredN                  atomic.Int64
	settled                                       atomic.Int64
}

// StartConns validates c and starts opening its connections.
func StartConns(ctx context.Context, c ConnsConfig) (*Conns, error) {
	switch c.Kind {
	case ConnsIdleHTTP1, ConnsIdleH2, ConnsStallH2C, ConnsSlowHeader:
	default:
		return nil, fmt.Errorf("lab: conns: unknown kind %q", c.Kind)
	}
	if c.Target == "" || c.Count < 1 || c.Rate < 0 {
		return nil, errors.New("lab: conns: a target, a positive count and a non-negative rate are needed")
	}
	if c.Kind == ConnsSlowHeader && c.HeaderBytes < 1 {
		return nil, errors.New("lab: conns: slow-header needs a positive header size")
	}
	if c.Host == "" {
		c.Host = c.Target
	}
	if c.Path == "" {
		c.Path = "/"
	}
	if c.Timeout <= 0 {
		c.Timeout = 30 * time.Second
	}
	s := &Conns{
		cfg: c, cpu: newCPUMeter(), stop: make(chan struct{}), conns: map[net.Conn]struct{}{},
		setup: newHistogram(), expired: newHistogram(), closed: newHistogram(),
	}
	if c.Kind == ConnsIdleH2 {
		pem, err := os.ReadFile(c.CAFile)
		if err != nil {
			return nil, fmt.Errorf("lab: conns: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("lab: conns: the CA file holds no certificate")
		}
		s.tls = &tls.Config{RootCAs: pool, ServerName: c.ServerName, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12}
	}
	s.wg.Go(func() { s.schedule(ctx) })
	return s, nil
}

// schedule attempts connection i at start + i/Rate, each on its own
// goroutine, until every one is attempted or the service stops.
func (s *Conns) schedule(ctx context.Context) {
	start := time.Now()
	for i := range s.cfg.Count {
		if s.cfg.Rate > 0 {
			due := start.Add(time.Duration(float64(i) / s.cfg.Rate * float64(time.Second)))
			if d := time.Until(due); d > 0 {
				t := time.NewTimer(d)
				select {
				case <-t.C:
				case <-s.stop:
					t.Stop()
					return
				case <-ctx.Done():
					t.Stop()
					return
				}
			}
		}
		select {
		case <-s.stop:
			return
		default:
		}
		s.attempted.Add(1)
		s.wg.Go(func() { s.one(ctx) })
	}
}

// one sets up one connection and holds it until the Node closes it or the
// service stops.
func (s *Conns) one(ctx context.Context) {
	t0 := time.Now()
	s.mu.Lock()
	if s.first.IsZero() {
		s.first = t0
	}
	s.mu.Unlock()
	sctx, cancel := context.WithTimeout(ctx, s.cfg.Timeout)
	c, rd, err := s.dial(sctx)
	cancel()
	// A setup's counters, its error and its settling change together under
	// s.mu, so a snapshot that reports every connection settled holds each
	// one's outcome.
	if err != nil {
		s.mu.Lock()
		s.failed.Add(1)
		if len(s.errs) < maxConnsErrors {
			s.errs = append(s.errs, trimErr(err.Error()))
		}
		s.settled.Add(1)
		s.mu.Unlock()
		return
	}
	setupDone := time.Now()
	s.mu.Lock()
	s.opened.Add(1)
	s.open.Add(1)
	s.conns[c] = struct{}{}
	s.setup.Record(setupDone.Sub(t0))
	s.last = setupDone
	s.settled.Add(1)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		s.open.Add(-1)
		_ = c.Close()
	}()
	from := setupDone
	switch s.cfg.Kind {
	case ConnsStallH2C, ConnsSlowHeader:
		from = t0
	}
	peerClosed := s.hold(c, rd, from)
	select {
	case <-s.stop:
		return
	default:
	}
	if peerClosed {
		s.mu.Lock()
		s.closed.Record(time.Since(from))
		s.closedByPeer.Add(1)
		s.mu.Unlock()
	}
}

// dial connects and runs the kind's first exchange.
func (s *Conns) dial(ctx context.Context) (net.Conn, *bufio.Reader, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.cfg.Target)
	if err != nil {
		return nil, nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = c.SetDeadline(dl)
	}
	fail := func(err error) (net.Conn, *bufio.Reader, error) {
		_ = c.Close()
		return nil, nil, err
	}
	switch s.cfg.Kind {
	case ConnsIdleHTTP1:
		rd := bufio.NewReader(c)
		if err := s.http1Exchange(c, rd); err != nil {
			return fail(err)
		}
		_ = c.SetDeadline(time.Time{})
		return c, rd, nil
	case ConnsIdleH2:
		tc := tls.Client(c, s.tls)
		if err := tc.HandshakeContext(ctx); err != nil {
			return fail(fmt.Errorf("handshake: %w", err))
		}
		if p := tc.ConnectionState().NegotiatedProtocol; p != "h2" {
			_ = tc.Close()
			return nil, nil, fmt.Errorf("ALPN %q, want h2", p)
		}
		rd := bufio.NewReader(tc)
		if err := h2Start(tc, rd, false); err != nil {
			_ = tc.Close()
			return nil, nil, err
		}
		_ = tc.SetDeadline(time.Time{})
		return tc, rd, nil
	case ConnsStallH2C:
		rd := bufio.NewReader(c)
		if err := h2Start(c, rd, true); err != nil {
			return fail(err)
		}
		if err := writeFrame(c, h2Headers, h2FlagEndHdrs|h2FlagEndStrm, 1, requestBlock(s.cfg.Host, s.cfg.Path)); err != nil {
			return fail(err)
		}
		_ = c.SetDeadline(time.Time{})
		return c, rd, nil
	default: // ConnsSlowHeader
		head := "GET " + s.cfg.Path + " HTTP/1.1\r\nHost: " + s.cfg.Host + "\r\nX-Flood: "
		if _, err := io.WriteString(c, head); err != nil {
			return fail(err)
		}
		if _, err := c.Write(slices.Repeat([]byte{'a'}, s.cfg.HeaderBytes)); err != nil {
			return fail(err)
		}
		_ = c.SetDeadline(time.Time{})
		return c, bufio.NewReader(c), nil
	}
}

// http1Exchange sends one GET and reads its whole response, which must
// keep the connection open.
func (s *Conns) http1Exchange(c net.Conn, rd *bufio.Reader) error {
	req := "GET " + s.cfg.Path + " HTTP/1.1\r\nHost: " + s.cfg.Host + "\r\nUser-Agent: ruralz-bench-conns\r\n\r\n"
	if _, err := io.WriteString(c, req); err != nil {
		return err
	}
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		return err
	}
	_, err = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return err
	}
	// Any answer that keeps the connection open completes the exchange: a
	// Gateway Policy may refuse the unauthenticated request, which still
	// leaves an idle keep-alive connection.
	if resp.Close {
		return fmt.Errorf("response %d closes the connection", resp.StatusCode)
	}
	return nil
}

// hold reads the connection until the Node closes it or the service stops;
// it answers HTTP/2 SETTINGS and PING frames and, on a stall-h2c
// connection, counts how its stream ended. It reports a close by the Node.
func (s *Conns) hold(c net.Conn, rd *bufio.Reader, setup time.Time) bool {
	switch s.cfg.Kind {
	case ConnsIdleH2, ConnsStallH2C:
		headers := false
		for {
			typ, flags, stream, payload, err := readFrame(rd)
			if err != nil {
				return true
			}
			switch typ {
			case h2Settings:
				if flags&h2FlagAck == 0 {
					if writeFrame(c, h2Settings, h2FlagAck, 0, nil) != nil {
						return true
					}
				}
			case h2Ping:
				if flags&h2FlagAck == 0 && writeFrame(c, h2Ping, h2FlagAck, 0, payload) != nil {
					return true
				}
			case h2Headers:
				if stream == 1 && !headers {
					headers = true
					s.responded.Add(1)
				}
			case h2RSTStream:
				if stream != 1 || s.cfg.Kind != ConnsStallH2C {
					continue
				}
				if !headers {
					s.aborted.Add(1)
				} else {
					s.mu.Lock()
					s.expired.Record(time.Since(setup))
					s.expiredN.Add(1)
					s.mu.Unlock()
				}
			case h2GoAway:
				return true
			}
		}
	default:
		_, err := io.Copy(io.Discard, rd)
		return err == nil || !errors.Is(err, net.ErrClosed)
	}
}

// h2Start writes the client preface and SETTINGS (a zero initial window
// when zeroWindow) and reads frames up to the server's SETTINGS, which it
// acknowledges.
func h2Start(c net.Conn, rd *bufio.Reader, zeroWindow bool) error {
	settings := settingsPayload(map[uint16]uint32{h2SettingsPush: 0})
	if zeroWindow {
		settings = settingsPayload(map[uint16]uint32{h2SettingsPush: 0, h2SettingsWin: 0})
	}
	if _, err := io.WriteString(c, h2Preface); err != nil {
		return err
	}
	if err := writeFrame(c, h2Settings, 0, 0, settings); err != nil {
		return err
	}
	for {
		typ, flags, _, _, err := readFrame(rd)
		if err != nil {
			return fmt.Errorf("server SETTINGS: %w", err)
		}
		switch {
		case typ == h2Settings && flags&h2FlagAck == 0:
			return writeFrame(c, h2Settings, h2FlagAck, 0, nil)
		case typ == h2GoAway:
			return errors.New("server GOAWAY before SETTINGS")
		}
	}
}

// settingsPayload encodes SETTINGS parameters in identifier order.
func settingsPayload(params map[uint16]uint32) []byte {
	ids := make([]uint16, 0, len(params))
	for id := range params {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	b := make([]byte, 0, 6*len(ids))
	for _, id := range ids {
		b = binary.BigEndian.AppendUint16(b, id)
		b = binary.BigEndian.AppendUint32(b, params[id])
	}
	return b
}

// writeFrame writes one HTTP/2 frame.
func writeFrame(w io.Writer, typ, flags byte, stream uint32, payload []byte) error {
	n := len(payload)
	if n >= h2MaxFrame {
		return errors.New("frame payload too long")
	}
	// The 24-bit length, big-endian, one masked byte at a time.
	h := []byte{byte((n >> 16) & 0xff), byte((n >> 8) & 0xff), byte(n & 0xff), typ, flags, 0, 0, 0, 0}
	binary.BigEndian.PutUint32(h[5:], stream&0x7fffffff)
	_, err := w.Write(append(h, payload...))
	return err
}

// readFrame reads one HTTP/2 frame.
func readFrame(rd *bufio.Reader) (typ, flags byte, stream uint32, payload []byte, err error) {
	var h [9]byte
	if _, err = io.ReadFull(rd, h[:]); err != nil {
		return 0, 0, 0, nil, err
	}
	n := int(h[0])<<16 | int(h[1])<<8 | int(h[2])
	payload = make([]byte, n)
	if _, err = io.ReadFull(rd, payload); err != nil {
		return 0, 0, 0, nil, err
	}
	return h[3], h[4], binary.BigEndian.Uint32(h[5:]) & 0x7fffffff, payload, nil
}

// requestBlock is a GET request's field block in HPACK literal fields
// without indexing and without Huffman coding (RFC 7541 section 6.2.2).
func requestBlock(host, path string) []byte {
	var b []byte
	for _, f := range [][2]string{{":method", "GET"}, {":scheme", "http"}, {":path", path}, {":authority", host}, {"user-agent", "ruralz-bench-conns"}} {
		b = append(b, 0x00)
		b = hpackString(b, f[0])
		b = hpackString(b, f[1])
	}
	return b
}

// hpackString appends a string literal with a 7-bit prefix length.
func hpackString(b []byte, s string) []byte {
	n := uint64(len(s))
	if n < 127 {
		b = append(b, byte(n))
	} else {
		b = append(b, 127)
		n -= 127
		for n >= 128 {
			b = append(b, byte(n%128)|0x80)
			n /= 128
		}
		b = append(b, byte(n))
	}
	return append(b, s...)
}

// Ready describes the conns service.
func (s *Conns) Ready() Ready { return ready("conns", s.cfg.Target) }

// Handle answers stats and reset.
func (s *Conns) Handle(_ context.Context, r Request) Response {
	switch r.Op {
	case OpReset:
		s.cpu.reset()
		return Response{OK: true}
	case OpStats:
		return Response{OK: true, Process: s.cpu.stats(), Conns: s.stats()}
	default:
		return unknown(r)
	}
}

// stats snapshots the counters.
func (s *Conns) stats() *ConnsStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := &ConnsStats{
		Attempted: s.attempted.Load(), Opened: s.opened.Load(), Failed: s.failed.Load(), Open: s.open.Load(),
		ClosedByPeer: s.closedByPeer.Load(), Responded: s.responded.Load(), Aborted: s.aborted.Load(), Expired: s.expiredN.Load(),
		Setup: EncodeHistogram(s.setup), ExpiredAfter: EncodeHistogram(s.expired), ClosedAfter: EncodeHistogram(s.closed),
		Errors: slices.Clone(s.errs),
	}
	st.Done = s.settled.Load() >= int64(s.cfg.Count)
	if !s.first.IsZero() && s.last.After(s.first) {
		st.Spread = s.last.Sub(s.first)
	}
	return st
}

// Close stops attempting, closes every held connection and waits.
func (s *Conns) Close() error {
	close(s.stop)
	s.mu.Lock()
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	s.wg.Wait()
	return nil
}

// ConnsArgs returns the labhost command line flags of c, which the conns
// command parses back.
func ConnsArgs(c ConnsConfig) []string {
	args := []string{
		"-kind", c.Kind, "-target", c.Target, "-count", strconv.Itoa(c.Count),
		"-rate", strconv.FormatFloat(c.Rate, 'f', -1, 64),
	}
	for _, f := range []struct{ name, v string }{
		{"-ca", c.CAFile}, {"-server-name", c.ServerName}, {"-host", c.Host}, {"-path", c.Path},
	} {
		if f.v != "" {
			args = append(args, f.name, f.v)
		}
	}
	if c.HeaderBytes > 0 {
		args = append(args, "-header-bytes", strconv.Itoa(c.HeaderBytes))
	}
	if c.Timeout > 0 {
		args = append(args, "-timeout", c.Timeout.String())
	}
	return args
}

// trimErr shortens an error line for ConnsStats.Errors.
func trimErr(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return strings.TrimSpace(s)
}
