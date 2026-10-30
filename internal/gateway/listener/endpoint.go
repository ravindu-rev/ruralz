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

// endpoint is the listening sockets of a listener identity
// (snapshot.ListenerKey), one per host the Set binds and all on one port,
// and their dispatching listeners: each socket's accept loop hands each
// connection round-robin to the channel-fed listeners of the Shards
// servers the endpoint feeds (docs/architecture/03-data-plane.md "Sources,
// Last-Known-Good and Drain"), so a listener bound to 127.0.0.1 and ::1
// under RURALZ_BIND_LOOPBACK serves both sockets through one group of
// shards. It feeds one group of shards at a time, a new group after a
// change of the client-certificate request mode.
type endpoint struct {
	s     *Set
	key   snapshot.ListenerKey
	https bool

	// socks are the sockets, one per host of the Set in its order; set
	// before the endpoint starts and fixed after.
	socks []*socket
	// bound is the port every socket holds: the listener's, or the one
	// the kernel chose for port 0.
	bound int

	metrics atomic.Pointer[emit.ListenerMetrics] // the latest snapshot's
	tls     atomic.Pointer[tlsState]             // https only
	tlsCfg  *tls.Config                          // https only; reads tls per handshake
	group   atomic.Pointer[shardGroup]
	// next counts the connections dispatched: connection n goes to shard
	// n modulo Shards, or to the next shard still taking connections.
	next atomic.Uint64

	stopOnce sync.Once
	// stopped is closed once the sockets closed.
	stopped chan struct{}
	// unlinger ends a linger early (StopAccepting, Shutdown, Close).
	unlinger     chan struct{}
	unlingerOnce sync.Once
}

// socket is one listening socket of an endpoint, on one host of the Set,
// and its dispatching listener's accept loop.
type socket struct {
	ep   *endpoint
	host string
	// Set before the endpoint starts and fixed after; sock is the bound,
	// not listening, socket of Linux until start.
	sock  *reuseport.Socket
	tcp   *net.TCPListener
	acc   net.Listener // the ceiling and PROXY v2 wrappers over tcp
	local net.Addr
}

// groupKey returns the key of the socket's reuseport group.
func (sk *socket) groupKey() groupKey { return groupKey{host: sk.host, port: sk.ep.bound} }

// port returns the bound port.
func (ep *endpoint) port() int { return ep.bound }

// addr returns the bound address of the endpoint's first socket, the one
// Set.Addr reports.
func (ep *endpoint) addr() net.Addr { return ep.socks[0].local }

// addrs returns the bound addresses of every socket, in host order.
func (ep *endpoint) addrs() []net.Addr {
	out := make([]net.Addr, len(ep.socks))
	for i, sk := range ep.socks {
		out[i] = sk.local
	}
	return out
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
	ep.closeSockets()
	if st := ep.tls.Load(); st != nil {
		st.close()
	}
}

// closeSockets closes the sockets of an endpoint that never started and
// forgets them.
func (ep *endpoint) closeSockets() {
	for _, sk := range ep.socks {
		if sk.sock != nil {
			_ = sk.sock.Close()
		}
		if sk.tcp != nil {
			_ = sk.tcp.Close()
		}
	}
	ep.socks = nil
}

// acceptLoop is the socket's dispatching listener: it accepts connections
// until the socket closes and hands each to a shard of the endpoint's
// current group. Its goroutine is counted in Set.wg.
func (sk *socket) acceptLoop() {
	ep := sk.ep
	s := ep.s
	defer s.wg.Done()
	defer func() {
		if v := recover(); v != nil {
			s.log.Error("listener accept loop panicked", catalog.KeyListener, ep.key.Name, catalog.KeyError, fmt.Sprint(v))
		}
	}()
	var delay time.Duration
	for {
		c, err := sk.acc.Accept()
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

// wait sleeps d on the Set's clock; false means the sockets closed first.
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
// it to a shard. A panic closes the connection only (through its record
// once there is one, so it leaves the open connections).
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

// deliver hands nc to the shard whose turn it is, round-robin over the
// current group. A shard that stopped taking connections, as one the
// Drain shut down, passes its turn to the next shard still taking them; a
// group whose every shard stopped passes nc to its replacement, else nc
// closes.
func (ep *endpoint) deliver(nc net.Conn) {
	turn := ep.next.Add(1) - 1
	for {
		g := ep.group.Load()
		if g.offer(nc, turn) {
			return
		}
		if ep.group.Load() != g {
			continue
		}
		_ = nc.Close()
		return
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
