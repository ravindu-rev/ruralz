// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// lateConn is a client connection whose bytes all reached the socket just
// as the header deadline net/http armed at accept passed: the first read
// takes what fits of them, and every later read meets that deadline at
// once, whatever is still on the socket. It keeps what the server writes.
type lateConn struct {
	data   []byte
	closed chan struct{}
	once   sync.Once

	mu    sync.Mutex
	reads int
	out   bytes.Buffer
}

func newLateConn(data string) *lateConn {
	return &lateConn{data: []byte(data), closed: make(chan struct{})}
}

func (c *lateConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.reads++; c.reads > 1 {
		return 0, &net.OpError{Op: "read", Net: "tcp", Err: os.ErrDeadlineExceeded}
	}
	return copy(p, c.data), nil
}

func (c *lateConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.out.Write(p)
}

func (c *lateConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return nil
}

func (c *lateConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.out.Bytes())
}

func (*lateConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }

func (*lateConn) RemoteAddr() net.Addr             { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000} }
func (*lateConn) SetDeadline(time.Time) error      { return nil }
func (*lateConn) SetReadDeadline(time.Time) error  { return nil }
func (*lateConn) SetWriteDeadline(time.Time) error { return nil }

// oneConnListener hands out one connection, then waits until it is closed.
type oneConnListener struct {
	conns chan net.Conn
	done  chan struct{}
	once  sync.Once
}

func newOneConnListener(c net.Conn) *oneConnListener {
	l := &oneConnListener{conns: make(chan net.Conn, 1), done: make(chan struct{})}
	l.conns <- c
	return l
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *oneConnListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return nil
}

func (*oneConnListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8080} }

// settingsFrame is an HTTP/2 SETTINGS frame without settings.
const settingsFrame = "\x00\x00\x00\x04\x00\x00\x00\x00\x00"

// TestLateFirstRequestIsReadWhole checks the first read of an http
// listener's connection (docs/architecture/03-data-plane.md "Listeners and
// protocols"): when every byte a client sent reached the socket as the
// header deadline passed, net/http's 14-byte read of the h2c preface takes
// them all through the record, so an HTTP/1.1 request is served and an h2c
// preface starts HTTP/2. Read 14 bytes at a time, the HTTP/1.1 request
// line was cut there and answered a plain 400 Bad Request, which a pooled
// connection's first request, sent about ReadHeaderTimeout after its dial,
// met, and the h2c preface was dropped.
func TestLateFirstRequestIsReadWhole(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		want func([]byte) bool
		what string
	}{
		{
			name: "HTTP/1.1 request",
			data: "GET /plain HTTP/1.1\r\nHost: node.test\r\nUser-Agent: test\r\n\r\n",
			want: func(b []byte) bool { return bytes.HasPrefix(b, []byte("HTTP/1.1 204 ")) },
			what: "the handler's 204",
		},
		{
			name: "h2c preface",
			data: h2cPreface + settingsFrame,
			want: func(b []byte) bool {
				return len(b) >= len(settingsFrame) && b[3] == 0x4 && !bytes.HasPrefix(b, []byte("HTTP/"))
			},
			what: "HTTP/2, the server's SETTINGS frame first",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lc := newLateConn(tt.data)
			rec := newConn(lc, "web", newFakeMetrics().m, 0, time.Time{}, false, false)
			h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			srv := newHTTPServer(v1alpha1.ListenerProtocolHTTP, h, slog.DiscardHandler)
			ln := newOneConnListener(rec)
			served := make(chan error, 1)
			go func() { served <- srv.Serve(ln) }()
			recv(t, "the server to close the connection", lc.closed)
			// The HTTP/2 server flushes its first frames on a goroutine of its
			// own, which may write after the connection closed.
			waitFor(t, "the server's answer", func() bool { return len(lc.written()) > 0 })
			_ = srv.Close()
			if err := recv(t, "Serve to return", served); !errors.Is(err, http.ErrServerClosed) {
				t.Errorf("Serve = %v, want http.ErrServerClosed", err)
			}
			if got := lc.written(); !tt.want(got) {
				t.Errorf("the server wrote %q, want %s", got, tt.what)
			}
		})
	}
}

// chunkConn returns its chunks one per read, recording the size of each
// read it got.
type chunkConn struct {
	net.Conn
	chunks []string
	err    error // returned with the last chunk
	sizes  []int
}

func (c *chunkConn) Read(p []byte) (int, error) {
	c.sizes = append(c.sizes, len(p))
	if len(c.chunks) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[0])
	c.chunks = c.chunks[1:]
	if len(c.chunks) == 0 {
		return n, c.err
	}
	return n, nil
}

// TestFirstReadTakesAhead checks the record's first read: a cleartext
// connection's first read asking for fewer than firstRead bytes takes up to
// firstRead from the socket and later reads return the rest, then the first
// read's error, before the socket is read again; a first read asking for
// at least firstRead bytes and a TLS connection's reads go to the socket
// as asked.
func TestFirstReadTakesAhead(t *testing.T) {
	type read struct {
		size int
		want string
		err  error
	}
	for _, tt := range []struct {
		name      string
		tls       bool
		chunks    []string
		err       error
		reads     []read
		wantSizes []int
	}{
		{
			name:   "short first read",
			chunks: []string{"GET / HTTP/1.1\r\n\r\n", "next"},
			reads: []read{
				{size: 14, want: "GET / HTTP/1.1"},
				{size: 2, want: "\r\n"},
				{size: 100, want: "\r\n"},
				{size: 100, want: "next"},
			},
			wantSizes: []int{firstRead, 100},
		},
		{
			name:   "the first read's error after its bytes",
			chunks: []string{"PRI * HTTP/2.0\r\n"},
			err:    io.EOF,
			reads: []read{
				{size: 14, want: "PRI * HTTP/2.0"},
				{size: 100, want: "\r\n", err: io.EOF},
			},
			wantSizes: []int{firstRead},
		},
		{
			name:      "large first read",
			chunks:    []string{"abc", "def"},
			reads:     []read{{size: firstRead, want: "abc"}, {size: 1, want: "d"}},
			wantSizes: []int{firstRead, 1},
		},
		{
			name:      "TLS",
			tls:       true,
			chunks:    []string{"abc", "def"},
			reads:     []read{{size: 2, want: "ab"}, {size: 2, want: "de"}},
			wantSizes: []int{2, 2},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cc := &chunkConn{chunks: tt.chunks, err: tt.err}
			rec := newConn(cc, "web", newFakeMetrics().m, 0, time.Time{}, tt.tls, false)
			for i, r := range tt.reads {
				p := make([]byte, r.size)
				n, err := rec.Read(p)
				if string(p[:n]) != r.want || !errors.Is(err, r.err) || (r.err == nil && err != nil) {
					t.Fatalf("read %d of %d bytes = %q, %v; want %q, %v", i, r.size, p[:n], err, r.want, r.err)
				}
			}
			if !slices.Equal(cc.sizes, tt.wantSizes) {
				t.Errorf("socket reads of %v bytes, want %v", cc.sizes, tt.wantSizes)
			}
		})
	}
}
