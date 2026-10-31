// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/redis/rueidis"

	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
	"github.com/ravindu-rev/ruralz/internal/tlsconf"
)

// Dial errors.
var (
	// errPaced reports a dial the shard's reconnect pacer refused without
	// touching the network; it is not a connect failure.
	errPaced = errors.New("redis: dial paced")
	// errClosed reports a dial or call after Close.
	errClosed = errors.New("redis: driver closed")
	// errProtocol reports a reply the dialer cannot read.
	errProtocol = errors.New("redis: unreadable reply while connecting")
)

// tlsFor returns the client configuration verifying the server at host:
// the connection's TLS block's, else the system roots'.
func (d *Driver) tlsFor(host string) *tls.Config {
	if d.tlsc != nil {
		return d.tlsc.ForHost(host)
	}
	c := tlsconf.StateStore(host)
	if d.opts.RootCAs != nil {
		c.RootCAs = d.opts.RootCAs
	}
	return c
}

// dial is the rueidis DialCtxFn: every connection of the State client is
// dialed here, to dst only and without proxies: a URL host or a server
// its cluster or sentinels announce. The shard's pacer may refuse the
// dial: its per-second limit, or dst's own backoff after connect
// failures, never another address's, so the primary a sentinel failover
// promotes is dialed at once. The dialer connects, runs the TLS handshake
// for rediss:// and, except on a sentinel, authenticates and loads the
// library on the new connection before rueidis takes it, so SCRIPT LOAD
// runs at every connect, reconnect and failover. A dial, TLS or load I/O
// failure counts toward the shard breaker's consecutive connect failures;
// a sentinel's counts toward the primary's only while no client exists,
// as calls then wait on it.
func (d *Driver) dial(ctx context.Context, dst string, dialer *net.Dialer, tlsCfg *tls.Config) (net.Conn, error) {
	if d.closed.Load() {
		return nil, errClosed
	}
	sentinel := d.sentinelMark != nil && tlsCfg == d.sentinelMark
	sh := d.dialShard(dst, sentinel)
	if !sh.pace.TryDialAddr(dst) {
		return nil, errPaced
	}
	nc, err := d.connect(ctx, dst, sh, dialer, !sentinel)
	sh.pace.ResultAddr(dst, err == nil)
	if err != nil {
		switch {
		case !sentinel:
			sh.brk.ConnectFailed()
			if d.unmapped != nil && d.loadClient() == nil {
				// Before the cluster client exists, its calls wait on the
				// unmapped shard.
				d.unmapped.brk.ConnectFailed()
			}
		case d.loadClient() == nil:
			// Before the sentinel client exists, its calls wait on the
			// primary that no sentinel has named yet.
			d.single.brk.ConnectFailed()
		}
		if sh.connLog.allow(d.clk.Now()) {
			d.log.WarnContext(ctx, "state store dial failed", slog.String(catalog.KeyShard, sh.id), slog.String(catalog.KeyErrorType, errType(err)))
		}
		return nil, err
	}
	if !sentinel {
		sh.brk.ConnectSucceeded()
		if d.unmapped != nil && d.loadClient() == nil {
			d.unmapped.brk.ConnectSucceeded()
		}
		if d.topo == statestore.TopologySentinel {
			// Every other connection of the sentinel topology goes to the
			// primary the sentinels named: its host labels the primary
			// shard's calls from this answer on.
			d.single.recordDial(dst)
		}
	}
	tc := &trackedConn{Conn: nc, d: d, sh: sh, sentinel: sentinel}
	if !d.conns.add(tc) {
		_ = nc.Close()
		return nil, errClosed
	}
	return tc, nil
}

// dialShard returns the shard a dial to dst counts on: the sentinels' for
// a sentinel connection, the primary for any other of the sentinel
// topology, whose address moves with failovers, else dst's own.
func (d *Driver) dialShard(dst string, sentinel bool) *shard {
	switch {
	case sentinel:
		return d.sentinels
	case d.topo == statestore.TopologySentinel:
		return d.single
	default:
		return d.shardAt(dst)
	}
}

// connect dials dst, runs the TLS handshake for rediss:// and, with
// preload, authenticates and loads the library, all within the dialer's
// timeout and cut short when the driver closes.
func (d *Driver) connect(ctx context.Context, dst string, sh *shard, dialer *net.Dialer, preload bool) (net.Conn, error) {
	timeout := dialer.Timeout
	if timeout <= 0 {
		timeout = rueidis.DefaultDialTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if !d.goStart() {
		return nil, errClosed
	}
	go func() {
		defer d.goEnd()
		select {
		case <-d.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	nc, err := dialer.DialContext(ctx, "tcp", dst)
	if err != nil {
		return nil, err
	}
	if d.ep.tls {
		host, _, err := net.SplitHostPort(dst)
		if err != nil {
			_ = nc.Close()
			return nil, err
		}
		tc := tls.Client(nc, d.tlsFor(host))
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, err
		}
		nc = tc
	}
	if !preload {
		return nc, nil
	}
	if err := d.preload(ctx, nc, sh); err != nil {
		_ = nc.Close()
		return nil, err
	}
	return nc, nil
}

// preload authenticates a new connection and loads the library on it,
// RESP2 before the client's HELLO, so the connection never meets NOSCRIPT
// for a server that lost its scripts. An I/O failure fails the dial. An
// error reply does not: an AUTH refusal is left to the client's HELLO, a
// server answering SCRIPT as unknown or not permitted lacks the script
// command set (RZ-STS-005), and any other refusal leaves the load pending
// for the maintenance goroutine.
func (d *Driver) preload(ctx context.Context, nc net.Conn, sh *shard) error {
	if dl, ok := ctx.Deadline(); ok {
		if err := nc.SetDeadline(dl); err != nil {
			return err
		}
	}
	stop := context.AfterFunc(ctx, func() { _ = nc.SetDeadline(time.Unix(1, 0)) })
	r := bufio.NewReaderSize(nc, 128)
	err := d.preloadOn(ctx, nc, r, sh)
	if !stop() {
		// The cancellation already moved the deadline into the past.
		if err == nil {
			err = context.Cause(ctx)
		}
		return err
	}
	if err != nil {
		return err
	}
	if r.Buffered() != 0 {
		return errProtocol
	}
	return nc.SetDeadline(time.Time{})
}

// preloadOn sends AUTH, when the URL has credentials (a user name alone
// selects an ACL user without a password), and SCRIPT LOAD.
func (d *Driver) preloadOn(ctx context.Context, nc net.Conn, r *bufio.Reader, sh *shard) error {
	if u, pw := d.ep.opt.Username, d.ep.opt.Password; u != "" || pw != "" {
		auth := []string{"AUTH", pw}
		if u != "" {
			auth = []string{"AUTH", u, pw}
		}
		reply, err := roundTrip(nc, r, auth)
		if err != nil {
			return err
		}
		if reply.err != "" {
			return nil
		}
	}
	reply, err := roundTrip(nc, r, []string{"SCRIPT", "LOAD", script})
	if err != nil {
		return err
	}
	switch {
	case reply.err == "" && reply.s == d.sha:
		d.scriptsLoaded(ctx, sh)
	case reply.err == "":
		sh.pending.Store(true)
		if sh.scriptLog.allow(d.clk.Now()) {
			d.log.ErrorContext(ctx, "state store script load returned another SHA-1", slog.String(catalog.KeyShard, sh.id))
		}
	case lacksScriptsMsg(reply.err):
		d.scriptsRefused(ctx, sh, reply.err)
	default:
		sh.pending.Store(true)
		if sh.scriptLog.allow(d.clk.Now()) {
			w, _, _ := strings.Cut(reply.err, " ")
			d.log.WarnContext(ctx, "state store script load failed", slog.String(catalog.KeyShard, sh.id), slog.String(catalog.KeyErrorType, w))
		}
	}
	return nil
}

// resp2 is one RESP2 reply: a simple or bulk string, or an error.
type resp2 struct {
	s, err string
}

// roundTrip writes one command and reads its reply.
func roundTrip(w io.Writer, r *bufio.Reader, args []string) (resp2, error) {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	if _, err := io.WriteString(w, b.String()); err != nil {
		return resp2{}, err
	}
	return readReply(r)
}

// maxPreloadReply bounds a reply the dialer reads.
const maxPreloadReply = 1 << 16

// readReply reads one RESP2 reply; an integer or array is a protocol
// error, as AUTH and SCRIPT LOAD return neither.
func readReply(r *bufio.Reader) (resp2, error) {
	line, err := readLine(r)
	if err != nil {
		return resp2{}, err
	}
	if line == "" {
		return resp2{}, errProtocol
	}
	switch line[0] {
	case '+':
		return resp2{s: line[1:]}, nil
	case '-':
		return resp2{err: line[1:]}, nil
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < -1 || n > maxPreloadReply {
			return resp2{}, errProtocol
		}
		if n == -1 {
			return resp2{}, nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return resp2{}, err
		}
		if buf[n] != '\r' || buf[n+1] != '\n' {
			return resp2{}, errProtocol
		}
		return resp2{s: string(buf[:n])}, nil
	default:
		return resp2{}, errProtocol
	}
}

// readLine reads one CRLF-terminated line of at most maxPreloadReply
// bytes, without the CRLF.
func readLine(r *bufio.Reader) (string, error) {
	var b []byte
	for {
		chunk, err := r.ReadSlice('\n')
		b = append(b, chunk...)
		if len(b) > maxPreloadReply {
			return "", errProtocol
		}
		if err == nil {
			break
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return "", err
		}
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return "", errProtocol
	}
	return string(b[:len(b)-2]), nil
}

// trackedConn is a State client connection the driver closes at Close; its
// close in a cluster asks for a slot map refresh.
type trackedConn struct {
	net.Conn
	d  *Driver
	sh *shard
	// sentinel is true for a connection to a sentinel.
	sentinel bool
	once     sync.Once
}

// Close closes the connection once and forgets it.
func (c *trackedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() {
		c.d.conns.remove(c)
		if c.d.unmapped != nil {
			c.d.requestRefresh()
		}
	})
	return err
}

// connSet holds the open connections.
type connSet struct {
	mu     sync.Mutex
	conns  map[*trackedConn]struct{}
	closed bool
}

func (s *connSet) add(c *trackedConn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *connSet) remove(c *trackedConn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, c)
}

// len returns the open connections.
func (s *connSet) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// closeWhere closes the connections match selects and returns how many.
func (s *connSet) closeWhere(match func(*trackedConn) bool) int {
	s.mu.Lock()
	var sel []*trackedConn
	for c := range s.conns {
		if match(c) {
			sel = append(sel, c)
		}
	}
	s.mu.Unlock()
	for _, c := range sel {
		_ = c.Close()
	}
	return len(sel)
}

// reconnect closes sh's connections after a READONLY reply, at most once
// per minRefreshGap: a failover demoted the server they reach. Their
// redials, paced by sh's pacer, reach the new primary: a standalone
// endpoint follows it, a cluster's closed connections request a slot map
// refresh, and under sentinel the sentinel connections close too, so the
// client asks the sentinels for the primary again.
func (d *Driver) reconnect(ctx context.Context, sh *shard) {
	now := d.clk.Now()
	if !sh.reconnects.allowEvery(now, minRefreshGap) {
		return
	}
	d.conns.closeWhere(func(c *trackedConn) bool { return c.sh == sh || c.sentinel })
	if sh.reconnectLog.allow(now) {
		d.log.WarnContext(ctx, "state store server answered READONLY; reconnecting", slog.String(catalog.KeyShard, sh.id))
	}
}

// closeAll closes every connection and refuses later ones.
func (s *connSet) closeAll() {
	s.mu.Lock()
	s.closed = true
	all := make([]*trackedConn, 0, len(s.conns))
	for c := range s.conns {
		all = append(all, c)
	}
	s.mu.Unlock()
	for _, c := range all {
		_ = c.Close()
	}
}

// requestRefresh asks the maintenance goroutine for a slot map refresh.
func (d *Driver) requestRefresh() {
	d.refresh.Store(true)
	select {
	case d.wake <- struct{}{}:
	default:
	}
}

// errType classifies err for logs without its text, which may name an
// address: an error reply's first word, or the failure kind.
func errType(err error) string {
	var re *rueidis.RedisError
	var ne net.Error
	var recErr tls.RecordHeaderError
	var certErr *tls.CertificateVerificationError
	var alert tls.AlertError
	switch {
	case err == nil:
		return ""
	case errors.As(err, &re):
		w, _, _ := strings.Cut(re.Error(), " ")
		return w
	case errors.Is(err, errPaced):
		return "paced"
	case errors.Is(err, errClosed), errors.Is(err, rueidis.ErrClosing):
		return "closed"
	case errors.Is(err, errNotCluster):
		return "not_cluster"
	case errors.Is(err, errProtocol):
		return "protocol"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, os.ErrDeadlineExceeded):
		return "timeout"
	case errors.As(err, &recErr), errors.As(err, &certErr), errors.As(err, &alert):
		return "tls"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, net.ErrClosed):
		return "reset"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "refused"
	case errors.As(err, &ne) && ne.Timeout():
		return "timeout"
	default:
		return "network"
	}
}

// awaitClient waits until a client is published; nil after Close.
func (d *Driver) awaitClient() rueidis.Client {
	select {
	case <-d.ready:
		return d.loadClient()
	case <-d.closing:
		return nil
	}
}

// loadClient returns the published client, or nil.
func (d *Driver) loadClient() rueidis.Client {
	if r := d.client.Load(); r != nil {
		return r.c
	}
	return nil
}

// nodeClient returns the client of the server at addr: the State client
// itself for standalone, else the cluster node's client, nil when the
// cluster client knows no such node.
func (d *Driver) nodeClient(c rueidis.Client, addr string) rueidis.Client {
	if d.topo != statestore.TopologyCluster {
		return c
	}
	return c.Nodes()[addr]
}
