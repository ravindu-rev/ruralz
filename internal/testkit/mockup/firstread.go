// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mockup

import "net"

// firstReadSize is what the first read of a cleartext h2c-capable
// connection takes from the socket: net/http's read buffer. net/http tells
// h2c from HTTP/1.1 by a first read of the 14 bytes that start the preface
// alone, under the ReadHeaderTimeout deadline it armed at accept; were
// that read to take only those bytes of a request that reached the socket
// whole as the deadline passed, the next read would meet the deadline and
// net/http would take the 14 bytes for the whole request line, answering a
// plain 400 Bad Request and closing. A Node's pooled connection whose first
// request it sends about ReadHeaderTimeout after the dial meets that
// instant, and the Node relays the 400 as the Upstream's answer, as the
// gateway's own http listeners avoid the same way
// (docs/architecture/03-data-plane.md "Listeners and protocols").
const firstReadSize = 4 << 10

// firstReadListener wraps every accepted connection in a firstReadConn.
type firstReadListener struct{ net.Listener }

// Accept accepts the next connection.
func (l firstReadListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &firstReadConn{Conn: c, first: true}, nil
}

// firstReadConn is a connection whose first read, when it asks for fewer
// than firstReadSize bytes, takes up to firstReadSize from the socket;
// later reads return what it took past the bytes asked for, then its
// error, before they read the socket again. net/http never reads a
// connection concurrently.
type firstReadConn struct {
	net.Conn
	first bool
	ahead []byte
	err   error
}

// Read reads the connection.
func (c *firstReadConn) Read(b []byte) (int, error) {
	if c.ahead != nil {
		n := copy(b, c.ahead)
		if c.ahead = c.ahead[n:]; len(c.ahead) > 0 {
			return n, nil
		}
		err := c.err
		c.ahead, c.err = nil, nil
		return n, err
	}
	if !c.first || len(b) == 0 {
		return c.Conn.Read(b)
	}
	c.first = false
	if len(b) >= firstReadSize {
		return c.Conn.Read(b)
	}
	buf := make([]byte, firstReadSize)
	n, err := c.Conn.Read(buf)
	k := copy(b, buf[:n])
	if k < n {
		c.ahead, c.err = buf[k:n], err
		return k, nil
	}
	return k, err
}
