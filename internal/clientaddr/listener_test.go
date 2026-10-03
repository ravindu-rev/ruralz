// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package clientaddr

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

// refusal records OnRefused calls.
type refusal struct {
	mu    sync.Mutex
	peers []net.Addr
	errs  []error
}

func (r *refusal) hook(peer net.Addr, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.peers = append(r.peers, peer)
	r.errs = append(r.errs, err)
}

func (r *refusal) snapshot() []error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]error(nil), r.errs...)
}

// listen wraps a loopback TCP listener.
func listen(t *testing.T, o ListenerOptions) *Listener {
	t.Helper()
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l, err := NewListener(inner, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close() })
	return l
}

// pair dials l and returns the client side and the accepted *Conn.
func pair(t *testing.T, l *Listener) (net.Conn, *Conn) {
	t.Helper()
	client, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	c, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	pc, ok := c.(*Conn)
	if !ok {
		t.Fatalf("Accept returned %T", c)
	}
	return client, pc
}

func write(t *testing.T, c net.Conn, b []byte) {
	t.Helper()
	if _, err := c.Write(b); err != nil {
		t.Fatal(err)
	}
}

func tcpPeer(t *testing.T, c net.Conn) netip.AddrPort {
	t.Helper()
	return normalizeAddrPort(c.LocalAddr().(*net.TCPAddr).AddrPort())
}

func TestNewListenerOptions(t *testing.T) {
	inner, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = inner.Close() }()
	for name, tt := range map[string]struct {
		l net.Listener
		o ListenerOptions
	}{
		"nil listener":     {nil, ListenerOptions{Clock: clock.Real()}},
		"nil clock":        {inner, ListenerOptions{}},
		"negative timeout": {inner, ListenerOptions{Clock: clock.Real(), Timeout: -time.Second}},
	} {
		if _, err := NewListener(tt.l, tt.o); !errors.Is(err, ErrOptions) {
			t.Errorf("%s: err = %v, want ErrOptions", name, err)
		}
	}
	l, err := NewListener(inner, ListenerOptions{Clock: clock.Real()})
	if err != nil || l.opts.Timeout != DefaultHeaderTimeout {
		t.Fatalf("NewListener = %+v, %v; want the 10 s default (04 req 10)", l, err)
	}
}

// TestConnProxyHeader: the PROXY source replaces the TCP peer, the
// request bytes after the header are intact, and Accept never reads (04
// req 16, 06 req 61).
func TestConnProxyHeader(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)

	if _, ok := c.Header(); ok {
		t.Fatal("Header ready before any read: Accept must not read")
	}
	if got := c.Peer(); got != tcpPeer(t, client) {
		t.Errorf("Peer before the header = %v, want the TCP peer", got)
	}
	write(t, client, append(proxyTCP4("203.0.113.7:51000", "192.0.2.1:443", tlv(0x04, []byte("pad"))), "hello"...))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("Read = %q, %v", buf, err)
	}
	h, ok := c.Header()
	if !ok || h.Command != CommandProxy {
		t.Fatalf("Header = %+v, %v", h, ok)
	}
	if got, want := c.Peer(), netip.MustParseAddrPort("203.0.113.7:51000"); got != want {
		t.Errorf("Peer = %v, want %v", got, want)
	}
	if got := c.RemoteAddr().String(); got != "203.0.113.7:51000" {
		t.Errorf("RemoteAddr = %s", got)
	}
	if got := PeerOf(c); got != netip.MustParseAddrPort("203.0.113.7:51000") {
		t.Errorf("PeerOf = %v", got)
	}
	if c.NetConn() == nil || c.LocalAddr().String() != c.NetConn().LocalAddr().String() {
		t.Errorf("NetConn/LocalAddr pass-through broken")
	}
}

// TestConnProxyHeaderMappedSource: an IPv4-mapped PROXY source is unmapped
// for Peer and RemoteAddr (04 req 17).
func TestConnProxyHeaderMappedSource(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)
	write(t, client, append(proxyTCP6("[::ffff:198.51.100.9]:4000", "[::1]:80", nil), 'x'))
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if got := c.Peer(); got != netip.MustParseAddrPort("198.51.100.9:4000") {
		t.Errorf("Peer = %v", got)
	}
	if got := c.RemoteAddr().String(); got != "198.51.100.9:4000" {
		t.Errorf("RemoteAddr = %s", got)
	}
}

// TestConnLocal: LOCAL keeps the TCP peer (04 req 16).
func TestConnLocal(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)
	write(t, client, append(local(), 'x'))
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if h, ok := c.Header(); !ok || h.Command != CommandLocal {
		t.Fatalf("Header = %+v, %v", h, ok)
	}
	if got := c.Peer(); got != tcpPeer(t, client) {
		t.Errorf("Peer = %v, want the TCP peer %v", got, tcpPeer(t, client))
	}
	if got := c.RemoteAddr().String(); got != client.LocalAddr().String() {
		t.Errorf("RemoteAddr = %s, want %s", got, client.LocalAddr())
	}
}

// TestConnMalformedCloses is the WP-15 "Done when": malformed PROXY
// headers close the connection, report one refusal, and fail every later
// Read and Write with a net.OpError that net/http answers with nothing.
func TestConnMalformedCloses(t *testing.T) {
	for name, in := range map[string][]byte{
		"v1 text":      []byte("PROXY TCP4 203.0.113.7 192.0.2.1 51000 443\r\n"),
		"plain HTTP":   []byte("GET / HTTP/1.1\r\n\r\n"),
		"UDP family":   v2(0x21, 0x12, make([]byte, 12)),
		"over the cap": v2Len(0x21, 0x11, MaxVariableLen+1, nil),
		"version 1":    v2(0x11, 0x11, make([]byte, 12)),
	} {
		t.Run(name, func(t *testing.T) {
			var ref refusal
			l := listen(t, ListenerOptions{Clock: clock.Real(), OnRefused: ref.hook})
			client, c := pair(t, l)
			write(t, client, in)

			_, err := c.Read(make([]byte, 16))
			var oe *net.OpError
			if !errors.As(err, &oe) || oe.Op != "read" || !errors.Is(err, ErrMalformed) {
				t.Fatalf("Read err = %v, want a read *net.OpError wrapping ErrMalformed", err)
			}
			if _, err := c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n")); !errors.As(err, &oe) || oe.Op != "write" {
				t.Errorf("Write err = %v, want a write *net.OpError", err)
			}
			if got := ref.snapshot(); len(got) != 1 || !errors.Is(got[0], ErrMalformed) {
				t.Errorf("OnRefused calls = %v, want one ErrMalformed", got)
			}
			if ref.peers[0].String() != client.LocalAddr().String() {
				t.Errorf("OnRefused peer = %v, want %v", ref.peers[0], client.LocalAddr())
			}
			// The client sees the close, and no byte was written to it.
			_ = client.SetReadDeadline(clock.Real().Now().Add(5 * time.Second))
			n, err := client.Read(make([]byte, 64))
			if n != 0 || err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("client Read = %d, %v; want the connection closed with nothing written", n, err)
			}
			if err := c.Close(); err != nil {
				t.Errorf("Close after refusal = %v, want the idempotent nil", err)
			}
			if got := ref.snapshot(); len(got) != 1 {
				t.Errorf("OnRefused called %d times", len(got))
			}
		})
	}
}

// TestConnHeaderTimeout: no header within the timeout closes the
// connection (04 req 16 "within ReadHeaderTimeout", 06 req 61).
func TestConnHeaderTimeout(t *testing.T) {
	var ref refusal
	l := listen(t, ListenerOptions{Clock: clock.Real(), Timeout: 50 * time.Millisecond, OnRefused: ref.hook})
	client, c := pair(t, l)
	write(t, client, []byte(Signature[:6])) // a slow start, then silence
	_, err := c.Read(make([]byte, 1))
	if !errors.Is(err, ErrHeaderTimeout) || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Read err = %v, want ErrHeaderTimeout", err)
	}
	if got := ref.snapshot(); len(got) != 1 || !errors.Is(got[0], ErrHeaderTimeout) {
		t.Errorf("OnRefused = %v", got)
	}
}

// TestConnHeaderDeadlineFromClock: the header deadline is the injected
// clock's now plus the timeout; a clock in the past expires at once,
// which is why ListenerOptions.Clock must track wall time.
func TestConnHeaderDeadlineFromClock(t *testing.T) {
	fake := clocktest.New(time.Unix(0, 0))
	l := listen(t, ListenerOptions{Clock: fake, Timeout: time.Second})
	_, c := pair(t, l)
	start := clock.Real().Now()
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrHeaderTimeout) {
		t.Fatalf("Read err = %v, want ErrHeaderTimeout", err)
	}
	if d := clock.Real().Since(start); d > 5*time.Second {
		t.Errorf("expired after %v", d)
	}
}

// TestConnCallerDeadline: an earlier read deadline set by the server
// (net/http's ReadHeaderTimeout) bounds the header read, and the caller's
// deadline is restored once the header is read.
func TestConnCallerDeadline(t *testing.T) {
	t.Run("earlier caller deadline bounds the header", func(t *testing.T) {
		l := listen(t, ListenerOptions{Clock: clock.Real(), Timeout: time.Minute})
		_, c := pair(t, l)
		if err := c.SetDeadline(clock.Real().Now().Add(50 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrHeaderTimeout) {
			t.Fatalf("Read err = %v, want ErrHeaderTimeout", err)
		}
	})
	t.Run("caller deadline restored after the header", func(t *testing.T) {
		l := listen(t, ListenerOptions{Clock: clock.Real(), Timeout: time.Minute})
		client, c := pair(t, l)
		// The header is queued before the caller's deadline starts, so
		// only the read after it can expire.
		write(t, client, local())
		if err := c.SetReadDeadline(clock.Real().Now().Add(250 * time.Millisecond)); err != nil {
			t.Fatal(err)
		}
		_, err := c.Read(make([]byte, 1))
		if !errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, ErrMalformed) {
			t.Fatalf("Read err = %v, want the caller's deadline after a good header", err)
		}
	})
	t.Run("header deadline cleared after the header", func(t *testing.T) {
		l := listen(t, ListenerOptions{Clock: clock.Real(), Timeout: 50 * time.Millisecond})
		client, c := pair(t, l)
		write(t, client, local())
		done := make(chan error, 1)
		go func() {
			b := make([]byte, 1)
			_, err := c.Read(b)
			done <- err
		}()
		_ = clock.Real().Sleep(t.Context(), 200*time.Millisecond)
		write(t, client, []byte("x"))
		if err := <-done; err != nil {
			t.Fatalf("Read after the header timeout elapsed = %v, want data", err)
		}
	})
}

// TestConnWriteFirst: a Write before any Read waits for the header, so
// nothing reaches a client that never sent one.
func TestConnWriteFirst(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)
	done := make(chan error, 2)
	go func() {
		_, err := c.Write([]byte("hi"))
		done <- err
	}()
	go func() {
		b := make([]byte, 1)
		_, err := c.Read(b)
		done <- err
	}()
	write(t, client, append(proxyTCP4("203.0.113.7:1", "192.0.2.1:2", nil), 'x'))
	for range 2 {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	b := make([]byte, 2)
	if _, err := io.ReadFull(client, b); err != nil || string(b) != "hi" {
		t.Fatalf("client got %q, %v", b, err)
	}
}

// TestConnCloseWrite: the half-close reaches the TCP connection after the
// header, so the client reads the response and then EOF.
func TestConnCloseWrite(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)
	write(t, client, append(local(), 'x'))
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := c.CloseWrite(); err != nil {
		t.Fatalf("CloseWrite = %v", err)
	}
	_ = client.SetReadDeadline(clock.Real().Now().Add(5 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil || string(got) != "bye" {
		t.Fatalf("client read %q, %v; want \"bye\" then EOF", got, err)
	}
}

// TestConnClosedWhileWaiting: a local Close during the header wait ends
// the read without reporting a refusal.
func TestConnClosedWhileWaiting(t *testing.T) {
	var ref refusal
	l := listen(t, ListenerOptions{Clock: clock.Real(), OnRefused: ref.hook})
	_, c := pair(t, l)
	done := make(chan error, 1)
	go func() {
		_, err := c.Read(make([]byte, 1))
		done <- err
	}()
	_ = clock.Real().Sleep(t.Context(), 20*time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("Read after Close succeeded")
	}
	if got := ref.snapshot(); len(got) != 0 {
		t.Errorf("OnRefused = %v, want none after a local close", got)
	}
	if err := c.Close(); err != nil {
		t.Errorf("second Close = %v", err)
	}
}

// TestListenerAcceptError passes the inner error through.
func TestListenerAcceptError(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	_ = l.Close()
	if c, err := l.Accept(); err == nil || c != nil {
		t.Fatalf("Accept = %v, %v", c, err)
	}
}

// deadlineConn records deadlines; reads come from r.
type deadlineConn struct {
	net.Conn // nil; only the methods below are used
	r        io.Reader
	mu       sync.Mutex
	reads    []time.Time
	writes   []time.Time
	failSet  error
	closed   atomic.Int32
}

func (d *deadlineConn) Read(b []byte) (int, error) { return d.r.Read(b) }
func (d *deadlineConn) Write(b []byte) (int, error) {
	return len(b), nil
}

func (d *deadlineConn) Close() error {
	d.closed.Add(1)
	return nil
}

func (d *deadlineConn) SetReadDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reads = append(d.reads, t)
	return d.failSet
}

func (d *deadlineConn) SetWriteDeadline(t time.Time) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.writes = append(d.writes, t)
	return nil
}

func (d *deadlineConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.9"), Port: 7}
}

func (d *deadlineConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 8080}
}

// TestConnDeadlineBookkeeping pins the deadline sequence with a fake
// clock: header deadline min(caller, now+timeout) while parsing, then the
// caller's deadline.
func TestConnDeadlineBookkeeping(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	fake := clocktest.New(now)
	o := &ListenerOptions{Clock: fake, Timeout: 10 * time.Second}
	tests := []struct {
		name   string
		caller time.Time
		want   []time.Time
	}{
		{"no caller deadline", time.Time{}, []time.Time{now.Add(10 * time.Second), {}}},
		{"earlier caller deadline", now.Add(3 * time.Second), []time.Time{now.Add(3 * time.Second), now.Add(3 * time.Second), now.Add(3 * time.Second)}},
		{"later caller deadline", now.Add(30 * time.Second), []time.Time{now.Add(30 * time.Second), now.Add(10 * time.Second), now.Add(30 * time.Second)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := &deadlineConn{r: bytes.NewReader(append(local(), 'x'))}
			c := &Conn{Conn: d, opts: o}
			if !tt.caller.IsZero() {
				if err := c.SetDeadline(tt.caller); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := c.Read(make([]byte, 1)); err != nil {
				t.Fatal(err)
			}
			if len(d.reads) != len(tt.want) {
				t.Fatalf("read deadlines %v, want %v", d.reads, tt.want)
			}
			for i := range d.reads {
				if !d.reads[i].Equal(tt.want[i]) {
					t.Errorf("read deadline %d = %v, want %v", i, d.reads[i], tt.want[i])
				}
			}
			if !tt.caller.IsZero() && (len(d.writes) != 1 || !d.writes[0].Equal(tt.caller)) {
				t.Errorf("write deadlines %v", d.writes)
			}
			if got := c.Peer(); got != netip.MustParseAddrPort("192.0.2.9:7") {
				t.Errorf("Peer = %v, want the unmapped TCP peer", got)
			}
			if err := c.CloseWrite(); !errors.Is(err, errors.ErrUnsupported) {
				t.Errorf("CloseWrite = %v, want ErrUnsupported without a half-close", err)
			}
		})
	}
}

// TestConnSetDeadlineFailure: a failing SetReadDeadline before the header
// refuses the connection instead of reading without a bound.
func TestConnSetDeadlineFailure(t *testing.T) {
	var ref refusal
	d := &deadlineConn{r: bytes.NewReader(local()), failSet: net.ErrClosed}
	c := &Conn{Conn: d, opts: &ListenerOptions{Clock: clock.Real(), Timeout: time.Second, OnRefused: ref.hook}}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrMalformed) || !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Read err = %v", err)
	}
	if d.closed.Load() != 1 || len(ref.snapshot()) != 1 {
		t.Errorf("closed %d times, %d refusals", d.closed.Load(), len(ref.snapshot()))
	}
	if err := c.SetDeadline(time.Time{}); err == nil {
		t.Error("SetDeadline error not propagated")
	}
}

// wrapConn is a wrapper exposing NetConn, like a counting listener's.
type wrapConn struct {
	net.Conn
}

func (w wrapConn) NetConn() net.Conn { return w.Conn }

// nilInner is a wrapper whose NetConn is nil.
type nilInner struct {
	addrConn
}

func (nilInner) NetConn() net.Conn { return nil }

// otherAddr is a non-TCP address.
type otherAddr string

func (a otherAddr) Network() string { return string(a) }
func (a otherAddr) String() string  { return "198.51.100.1:9" }

// addrConn reports a fixed remote address.
type addrConn struct {
	net.Conn
	remote net.Addr
}

func (a addrConn) RemoteAddr() net.Addr { return a.remote }

// TestPeerOf: handlers find the PROXY peer through TLS and other
// wrappers; plain TCP connections give their unmapped TCP peer.
func TestPeerOf(t *testing.T) {
	l := listen(t, ListenerOptions{Clock: clock.Real()})
	client, c := pair(t, l)
	write(t, client, append(proxyTCP4("203.0.113.7:51000", "192.0.2.1:443", nil), 'x'))
	if _, err := c.Read(make([]byte, 1)); err != nil {
		t.Fatal(err)
	}
	proxied := netip.MustParseAddrPort("203.0.113.7:51000")
	mapped := &net.TCPAddr{IP: net.ParseIP("::ffff:192.0.2.44"), Port: 99}
	tests := []struct {
		name string
		c    net.Conn
		want netip.AddrPort
	}{
		{"nil", nil, netip.AddrPort{}},
		{"Conn", c, proxied},
		{"TLS over Conn", tls.Server(c, &tls.Config{MinVersion: tls.VersionTLS13}), proxied},
		{"wrapper over TLS over Conn", wrapConn{tls.Server(wrapConn{c}, &tls.Config{MinVersion: tls.VersionTLS13})}, proxied},
		{"plain TCP", client, tcpPeer(t, c.NetConn())},
		{"mapped TCP peer", addrConn{remote: mapped}, netip.MustParseAddrPort("192.0.2.44:99")},
		{"textual tcp address", addrConn{remote: otherAddr("tcp")}, netip.MustParseAddrPort("198.51.100.1:9")},
		{"unix address", addrConn{remote: &net.UnixAddr{Name: "/run/x.sock", Net: "unix"}}, netip.AddrPort{}},
		{"non-TCP network", addrConn{remote: otherAddr("pipe")}, netip.AddrPort{}},
		{"nil remote", addrConn{remote: nil}, netip.AddrPort{}},
		{"wrapper over a non-wrapper", wrapConn{addrConn{remote: mapped}}, netip.MustParseAddrPort("192.0.2.44:99")},
		{"NetConn returns nil", nilInner{addrConn{remote: mapped}}, netip.MustParseAddrPort("192.0.2.44:99")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PeerOf(tt.c); got != tt.want {
				t.Errorf("PeerOf = %v, want %v", got, tt.want)
			}
		})
	}
}

// ctxKey carries the connection into handlers.
type ctxKey struct{}

// proxyTransport dials the server and sends hdr before anything else, so
// on https the header precedes the ClientHello (04 req 16).
func proxyTransport(base *http.Transport, hdr []byte) *http.Transport {
	tr := base.Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		var d net.Dialer
		c, err := d.DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		if _, err := c.Write(hdr); err != nil {
			_ = c.Close()
			return nil, err
		}
		return c, nil
	}
	return tr
}

// TestHTTPServerIntegration serves HTTP/1.1, HTTP/1.1 over TLS and HTTP/2
// over TLS behind the wrapper: handlers see the PROXY source through
// PeerOf, and a connection without a header gets no response at all.
func TestHTTPServerIntegration(t *testing.T) {
	for _, mode := range []string{"http1", "https", "h2"} {
		t.Run(mode, func(t *testing.T) {
			var ref refusal
			srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				c, _ := r.Context().Value(ctxKey{}).(net.Conn)
				_, _ = io.WriteString(w, PeerOf(c).String()+" "+r.Proto) //nolint:gosec // G705: test handler, plain-text echo to the test client
			}))
			l, err := NewListener(srv.Listener, ListenerOptions{Clock: clock.Real(), OnRefused: ref.hook})
			if err != nil {
				t.Fatal(err)
			}
			srv.Listener = l
			srv.Config.ReadHeaderTimeout = 10 * time.Second
			srv.Config.ConnContext = func(ctx context.Context, c net.Conn) context.Context {
				return context.WithValue(ctx, ctxKey{}, c)
			}
			switch mode {
			case "http1":
				srv.Start()
			case "https":
				srv.StartTLS()
			case "h2":
				srv.EnableHTTP2 = true
				srv.StartTLS()
			}
			defer srv.Close()
			base, _ := srv.Client().Transport.(*http.Transport)

			hdr := proxyTCP6("[2001:db8::7]:4711", "[2001:db8::1]:443", tlv(0x01, []byte("h2")))
			client := &http.Client{Transport: proxyTransport(base, hdr)}
			req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, http.NoBody)
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			wantProto := "HTTP/1.1"
			if mode == "h2" {
				wantProto = "HTTP/2.0"
			}
			if got, want := string(body), "[2001:db8::7]:4711 "+wantProto; got != want {
				t.Errorf("handler saw %q, want %q", got, want)
			}

			// Without a header: closed, nothing written, one refusal.
			raw, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = raw.Close() }()
			_, _ = io.WriteString(raw, "GET / HTTP/1.1\r\nHost: x\r\n\r\n")
			_ = raw.SetReadDeadline(clock.Real().Now().Add(5 * time.Second))
			got, err := io.ReadAll(raw)
			if len(got) != 0 || errors.Is(err, os.ErrDeadlineExceeded) {
				t.Errorf("no-header connection got %q, %v; want a close with no bytes", got, err)
			}
			if errs := ref.snapshot(); len(errs) != 1 || !errors.Is(errs[0], ErrSignature) {
				t.Errorf("refusals = %v, want one ErrSignature", errs)
			}
		})
	}
}
