// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/reuseport"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Accept retry backoff after an accept error other than a closed socket,
// such as running out of descriptors: 5 ms, doubling up to 1 s, the values
// of net/http's Server.Serve, whose accept loop the endpoint replaces
// (docs/architecture/03-data-plane.md "Listeners and protocols").
const (
	acceptRetryMin = 5 * time.Millisecond
	acceptRetryMax = time.Second
)

// nextAcceptRetry returns the wait after an accept error that follows a
// wait of prev (0 after a successful accept).
func nextAcceptRetry(prev time.Duration) time.Duration {
	return min(max(2*prev, acceptRetryMin), acceptRetryMax)
}

// endpoint is one listening socket of a listener identity
// (snapshot.ListenerKey) and the Servers it feeds: one at a time, a new
// one after a change of the client-certificate request mode.
type endpoint struct {
	s     *Set
	key   snapshot.ListenerKey
	https bool

	// Set before the endpoint starts and fixed after; sock is the bound,
	// not listening, socket of Linux until start.
	sock  *reuseport.Socket
	tcp   *net.TCPListener
	acc   net.Listener // the ceiling and PROXY v2 wrappers over tcp
	local net.Addr

	metrics atomic.Pointer[emit.ListenerMetrics] // the latest snapshot's
	tls     atomic.Pointer[tlsState]             // https only
	tlsCfg  *tls.Config                          // https only; reads tls per handshake
	srv     atomic.Pointer[server]

	stopOnce sync.Once
	// stopped is closed once the socket closed.
	stopped chan struct{}
	// unlinger ends a linger early (StopAccepting, Shutdown, Close).
	unlinger     chan struct{}
	unlingerOnce sync.Once
}

// port returns the bound port.
func (ep *endpoint) port() int {
	if a, ok := ep.local.(*net.TCPAddr); ok {
		return a.Port
	}
	return 0
}

// handshakeState returns the TLS state a handshake reads now
// (tlsconf.ServerOptions.State).
func (ep *endpoint) handshakeState() *tlsconf.ListenerState {
	if st := ep.tls.Load(); st != nil {
		return st.st
	}
	return nil
}

// endLinger ends the endpoint's linger, if any, early.
func (ep *endpoint) endLinger() { ep.unlingerOnce.Do(func() { close(ep.unlinger) }) }

// discard releases an endpoint that never started.
func (ep *endpoint) discard() {
	if ep.sock != nil {
		_ = ep.sock.Close()
	}
	if ep.tcp != nil {
		_ = ep.tcp.Close()
	}
	if st := ep.tls.Load(); st != nil {
		st.close()
	}
}

// acceptLoop accepts connections until the socket closes and hands each
// to the current Server. Its goroutine is counted in Set.wg.
func (ep *endpoint) acceptLoop() {
	s := ep.s
	defer s.wg.Done()
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("listener accept loop panicked", catalog.KeyListener, ep.key.Name, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	var delay time.Duration
	for {
		c, err := ep.acc.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			delay = nextAcceptRetry(delay)
			s.errLog.Warn("listener accept error, retrying", catalog.KeyListener, ep.key.Name, catalog.KeyError, err.Error())
			if !ep.wait(delay) {
				return
			}
			continue
		}
		delay = 0
		ep.admit(c)
	}
}

// wait sleeps d on the Set's clock; false means the socket closed first.
func (ep *endpoint) wait(d time.Duration) bool {
	t := ep.s.clk.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C():
		return true
	case <-ep.stopped:
		return false
	}
}

// admit records an accepted connection, wraps it in TLS on https and hands
// it to the current Server. A panic closes the connection only (through
// its record once there is one, so it leaves the open connections).
func (ep *endpoint) admit(c net.Conn) {
	s := ep.s
	var rec *Conn
	defer func() {
		if v := recover(); v != nil {
			if rec != nil {
				_ = rec.Close()
			} else {
				_ = c.Close()
			}
			s.log.Error("listener connection setup panicked", catalog.KeyListener, ep.key.Name, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	rec = newConn(c, ep.key.Name, ep.metrics.Load(), s.stripe(), s.clk.Now(), ep.https, ep.key.ProxyProtocol)
	var nc net.Conn = rec
	if ep.https {
		nc = tls.Server(rec, ep.tlsCfg)
	}
	ep.deliver(nc)
}

// deliver hands nc to the endpoint's current Server; a Server that stops
// before taking it passes it to its replacement, else nc closes.
func (ep *endpoint) deliver(nc net.Conn) {
	for {
		sv := ep.srv.Load()
		select {
		case sv.ln.conns <- nc:
			return
		case <-sv.ln.done:
			if ep.srv.Load() != sv {
				continue
			}
			_ = nc.Close()
			return
		}
	}
}

// observe starts, for a TLS connection a Server just took, the goroutine
// that waits on its handshake: net/http runs the handshake, and both
// callers of HandshakeContext get its one result.
func (ep *endpoint) observe(c net.Conn) {
	tc, ok := c.(*tls.Conn)
	if !ok {
		return
	}
	rec, ok := tc.NetConn().(*Conn)
	if !ok {
		return
	}
	s := ep.s
	s.wg.Add(1)
	go s.observeHandshake(tc, rec)
}

// observeHandshake measures one handshake. It ends with the handshake,
// which net/http bounds by ReadHeaderTimeout and Server.Close ends.
func (s *Set) observeHandshake(tc *tls.Conn, rec *Conn) {
	defer s.wg.Done()
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("listener handshake observer panicked", catalog.KeyListener, rec.listener, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	// A context that is never done: net/http's own call bounds the
	// handshake, and crypto/tls starts no interrupting goroutine for it.
	err := tc.HandshakeContext(context.Background())
	var alpn string
	if err == nil {
		alpn = tc.ConnectionState().NegotiatedProtocol
	}
	rec.handshakeDone(err, alpn, s.clk.Since(rec.start))
}
