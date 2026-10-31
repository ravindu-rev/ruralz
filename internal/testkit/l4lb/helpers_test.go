// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package l4lb

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/clock/clocktest"
)

func epoch() time.Time { return time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC) }

// waitFor polls a state condition; the 10 s guard only catches hangs, so
// no outcome depends on wall-clock time.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for !cond() {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(time.Millisecond):
		}
	}
}

// node stands in for a Node: a listener that greets each connection with
// the node's name and then echoes, resetting the connection when it reads
// "rst", and a /readyz server whose answer the test sets.
type node struct {
	name       string
	ln         net.Listener
	ready      *httptest.Server
	status     atomic.Int64
	reason     atomic.Pointer[string]
	hang       atomic.Bool
	probeConns atomic.Int64
	accepted   atomic.Int64

	mu    sync.Mutex
	conns []net.Conn
	wg    sync.WaitGroup
}

func newNode(t *testing.T, name string, status int) *node {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &node{name: name, ln: ln}
	n.status.Store(int64(status))
	n.setReason(adminapi.ReasonNoRevision)
	n.ready = httptest.NewUnstartedServer(http.HandlerFunc(n.readyz))
	n.ready.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			n.probeConns.Add(1)
		}
	}
	n.ready.Start()
	n.wg.Add(1)
	go n.serve()
	t.Cleanup(n.close)
	return n
}

func (n *node) setReason(r string) { n.reason.Store(&r) }

func (n *node) readyz(w http.ResponseWriter, r *http.Request) {
	if n.hang.Load() {
		<-r.Context().Done()
		return
	}
	st := int(n.status.Load())
	doc := adminapi.Readyz{Status: "ready", Revision: "rev-000000000000"}
	if st != http.StatusOK {
		doc = adminapi.Readyz{Status: "not_ready", Reasons: []adminapi.ReadyReason{{Reason: *n.reason.Load()}}}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(st)
	_ = json.NewEncoder(w).Encode(doc)
}

func (n *node) serve() {
	defer n.wg.Done()
	for {
		c, err := n.ln.Accept()
		if err != nil {
			return
		}
		n.accepted.Add(1)
		n.mu.Lock()
		n.conns = append(n.conns, c)
		n.mu.Unlock()
		n.wg.Add(1)
		go func() {
			defer n.wg.Done()
			defer func() { _ = c.Close() }()
			if _, err := io.WriteString(c, n.name+"\n"); err != nil {
				return
			}
			buf := make([]byte, 4096)
			for {
				k, err := c.Read(buf)
				if k > 0 {
					if strings.Contains(string(buf[:k]), "rst") {
						reset(c)
						return
					}
					if _, werr := c.Write(buf[:k]); werr != nil {
						return
					}
				}
				if err != nil {
					if errors.Is(err, io.EOF) {
						_ = c.(*net.TCPConn).CloseWrite()
						_, _ = io.Copy(io.Discard, c)
					}
					return
				}
			}
		}()
	}
}

func (n *node) close() {
	_ = n.ln.Close()
	n.mu.Lock()
	for _, c := range n.conns {
		_ = c.Close()
	}
	n.mu.Unlock()
	n.wg.Wait()
	n.ready.Close()
}

func (n *node) backend() Backend {
	return Backend{Name: n.name, Addr: n.ln.Addr().String(), ReadyURL: n.ready.URL + "/readyz"}
}

func startLB(t *testing.T, c Config) *Balancer {
	t.Helper()
	b, err := Start(t.Context(), c)
	if err != nil {
		t.Fatalf("Start = %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

// probed waits until every named backend has completed n probes.
func probed(t *testing.T, b *Balancer, n int64, names ...string) {
	t.Helper()
	for _, name := range names {
		if err := b.WaitProbes(t.Context(), name, n); err != nil {
			t.Fatalf("WaitProbes(%s, %d) = %v", name, n, err)
		}
	}
}

// tick advances the clock one probe interval and waits for the probes it
// starts.
func tick(t *testing.T, b *Balancer, fk *clocktest.Fake, names ...string) {
	t.Helper()
	want := map[string]int64{}
	for _, name := range names {
		s, _ := b.Backend(name)
		want[name] = s.Probes + 1
	}
	fk.Advance(b.cfg.ProbeInterval)
	for _, name := range names {
		probed(t, b, want[name], name)
	}
}

// client is a connection through the balancer.
type client struct {
	c  *net.TCPConn // nil when the reset ended the dial
	r  *bufio.Reader
	be string // the greeting: the backend's name
}

// dial connects through b and reads the backend's greeting; err is the
// error when the balancer reset the connection. The balancer accepts a
// connection before resetting it, so the reset ends the read, or the dial
// itself when it arrives before the dial sees its handshake complete:
// then the client has no connection.
func dial(t *testing.T, b *Balancer) (*client, error) {
	t.Helper()
	nc, err := (&net.Dialer{}).DialContext(t.Context(), "tcp", b.Addr())
	if errors.Is(err, syscall.ECONNRESET) {
		return &client{}, err
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nc.Close() })
	_ = nc.SetReadDeadline(time.Now().Add(10 * time.Second)) // a hang guard only
	c := &client{c: nc.(*net.TCPConn), r: bufio.NewReader(nc)}
	line, err := c.r.ReadString('\n')
	c.be = strings.TrimSuffix(line, "\n")
	return c, err
}

func mustDial(t *testing.T, b *Balancer) *client {
	t.Helper()
	c, err := dial(t, b)
	if err != nil {
		t.Fatalf("dial through the balancer: %v", err)
	}
	return c
}

// roundTrip writes msg and reads it back.
func (c *client) roundTrip(t *testing.T, msg string) {
	t.Helper()
	if _, err := io.WriteString(c.c, msg); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(c.r, buf); err != nil || string(buf) != msg {
		t.Fatalf("echo of %q = %q, %v", msg, buf, err)
	}
}

// backends returns the backend names of n dials in order.
func backends(t *testing.T, b *Balancer, n int) []string {
	t.Helper()
	out := make([]string, n)
	for i := range out {
		out[i] = mustDial(t, b).be
	}
	return out
}

func count(names []string) map[string]int {
	m := map[string]int{}
	for _, n := range names {
		m[n]++
	}
	return m
}

func fake() *clocktest.Fake { return clocktest.New(epoch()) }

func ctxDone() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
