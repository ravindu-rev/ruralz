// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"
)

// lateConn is a Node's connection whose bytes all reached the socket just
// as the header deadline net/http armed at accept passed: the first read
// takes what fits of them, and every later read meets that deadline at
// once, whatever is still on the socket. It keeps what the mock writes.
type lateConn struct {
	data   []byte
	closed chan struct{}
	once   sync.Once

	mu    sync.Mutex
	reads int
	out   bytes.Buffer
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

func (*lateConn) LocalAddr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000} }

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

func (*oneConnListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9000} }

// TestLateFirstRequestIsReadWhole checks the first read of the mock's
// cleartext connections: when every byte a Node sent reached the socket as
// the header deadline passed, net/http's 14-byte read of the h2c preface
// takes them all, so an HTTP/1.1 request gets the mock's answer and an h2c
// preface starts HTTP/2. Read 14 bytes at a time, the request line was cut
// there and answered a plain 400 Bad Request, which a Node's pooled
// connection, its first request sent about ReadHeaderTimeout after its
// dial, met and relayed as the Upstream's answer
// (docs/architecture/03-data-plane.md "Listeners and protocols").
func TestLateFirstRequestIsReadWhole(t *testing.T) {
	for _, tt := range []struct {
		name string
		data string
		want func([]byte) bool
		what string
	}{
		{
			name: "HTTP/1.1 request",
			data: "GET /plain HTTP/1.1\r\nHost: upstream.test\r\nUser-Agent: node\r\n\r\n",
			want: func(b []byte) bool { return bytes.HasPrefix(b, []byte("HTTP/1.1 200 OK\r\n")) },
			what: "the mock's 200",
		},
		{
			name: "h2c preface",
			data: "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" + "\x00\x00\x00\x04\x00\x00\x00\x00\x00",
			want: func(b []byte) bool { return len(b) >= 9 && b[3] == 0x4 && !bytes.HasPrefix(b, []byte("HTTP/")) },
			what: "HTTP/2, the server's SETTINGS frame first",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			lc := &lateConn{data: []byte(tt.data), closed: make(chan struct{})}
			ln := &oneConnListener{conns: make(chan net.Conn, 1), done: make(chan struct{})}
			ln.conns <- lc
			s := start(t, Config{listen: func(context.Context, string) (net.Listener, error) { return ln, nil }})
			select {
			case <-lc.closed:
			case <-time.After(10 * time.Second):
				t.Fatal("the mock did not close the connection")
			}
			// The HTTP/2 server flushes its first frames on a goroutine of its
			// own, which may write after the connection closed.
			waitFor(t, func() bool { return len(lc.written()) > 0 })
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			if got := lc.written(); !tt.want(got) {
				t.Errorf("the mock wrote %q, want %s", got, tt.what)
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

// TestFirstReadTakesAhead checks firstReadConn: a first read asking for
// fewer than firstReadSize bytes takes up to firstReadSize from the socket
// and later reads return the rest, then the first read's error, before the
// socket is read again; a first read asking for at least firstReadSize
// bytes goes to the socket as asked.
func TestFirstReadTakesAhead(t *testing.T) {
	type read struct {
		size int
		want string
		err  error
	}
	for _, tt := range []struct {
		name      string
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
			wantSizes: []int{firstReadSize, 100},
		},
		{
			name:   "the first read's error after its bytes",
			chunks: []string{"PRI * HTTP/2.0\r\n"},
			err:    io.EOF,
			reads: []read{
				{size: 14, want: "PRI * HTTP/2.0"},
				{size: 100, want: "\r\n", err: io.EOF},
			},
			wantSizes: []int{firstReadSize},
		},
		{
			name:      "large first read",
			chunks:    []string{"abc", "def"},
			reads:     []read{{size: firstReadSize, want: "abc"}, {size: 1, want: "d"}},
			wantSizes: []int{firstReadSize, 1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cc := &chunkConn{chunks: tt.chunks, err: tt.err}
			c := &firstReadConn{Conn: cc, first: true}
			for i, r := range tt.reads {
				p := make([]byte, r.size)
				n, err := c.Read(p)
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
