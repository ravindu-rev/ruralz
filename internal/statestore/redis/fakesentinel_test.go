// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"bufio"
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// fakeSentinel is a loopback RESP3 Redis Sentinel for the sentinel tests:
// HELLO, PING, CLIENT, SENTINEL SENTINELS, GET-MASTER-ADDR-BY-NAME and
// REPLICAS, SUBSCRIBE and UNSUBSCRIBE, and the +switch-master event it
// publishes to its subscribers. With a password, HELLO must authenticate
// (as user, or default without one); without one, the default user takes
// any password and a named user is unknown, as Redis's nopass default
// user does. Every other command, SCRIPT included, is counted and
// refused.
type fakeSentinel struct {
	t    testing.TB
	ln   net.Listener
	addr string

	// user and password are the credentials HELLO needs; "" password
	// needs none.
	user, password string

	mu      sync.Mutex
	masters map[string]string // master set: host:port
	counts  map[string]int
	conns   map[*sentinelConn]struct{}
	closed  bool
	wg      sync.WaitGroup
}

// sentinelConn is one client connection; mu serializes its replies and
// the events published to it.
type sentinelConn struct {
	c          net.Conn
	mu         sync.Mutex
	w          *bufio.Writer
	subscribed bool
}

// newFakeSentinel starts a sentinel naming masters, over TLS when cfg is
// set and requiring user and password when password is set; it closes
// when t ends.
func newFakeSentinel(t testing.TB, cfg *tls.Config, user, password string, masters map[string]string) *fakeSentinel {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	s := &fakeSentinel{
		t: t, ln: ln, addr: ln.Addr().String(), user: user, password: password, masters: map[string]string{},
		counts: map[string]int{}, conns: map[*sentinelConn]struct{}{},
	}
	for k, v := range masters {
		s.masters[k] = v
	}
	s.wg.Go(s.accept)
	t.Cleanup(s.close)
	return s
}

func (s *fakeSentinel) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		sc := &sentinelConn{c: c, w: bufio.NewWriter(c)}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.conns[sc] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(sc) })
	}
}

func (s *fakeSentinel) close() {
	s.mu.Lock()
	s.closed = true
	for c := range s.conns {
		_ = c.c.Close()
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	s.wg.Wait()
}

// count returns how many commands named name arrived ("SENTINEL
// GET-MASTER-ADDR-BY-NAME" for a subcommand, "HELLO AUTH" for a HELLO
// that authenticates).
func (s *fakeSentinel) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[name]
}

// setMaster names addr as set's primary without publishing an event, as
// when a client misses +switch-master.
func (s *fakeSentinel) setMaster(set, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.masters[set] = addr
}

// switchMaster names to as set's primary and publishes +switch-master to
// every subscriber.
func (s *fakeSentinel) switchMaster(set, to string) {
	s.mu.Lock()
	from := s.masters[set]
	s.masters[set] = to
	var subs []*sentinelConn
	for c := range s.conns {
		if c.subscribed {
			subs = append(subs, c)
		}
	}
	s.mu.Unlock()
	fh, fp, _ := net.SplitHostPort(from)
	th, tp, _ := net.SplitHostPort(to)
	msg := push(bulk("message"), bulk("+switch-master"), bulk(strings.Join([]string{set, fh, fp, th, tp}, " ")))
	for _, c := range subs {
		c.mu.Lock()
		_, _ = c.w.WriteString(msg)
		_ = c.w.Flush()
		c.mu.Unlock()
	}
}

// subscribers returns the connections subscribed to the events.
func (s *fakeSentinel) subscribers() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for c := range s.conns {
		if c.subscribed {
			n++
		}
	}
	return n
}

// credentialsOK reports whether user and pass authenticate: without a
// password the default user takes any password and no other user exists;
// with one, the configured user (default without one) and the password.
func (s *fakeSentinel) credentialsOK(user, pass string) bool {
	want := s.user
	if want == "" {
		want = "default"
	}
	if s.password == "" {
		return user == "default"
	}
	return user == want && pass == s.password
}

// push is a RESP3 push frame.
func push(items ...string) string {
	return ">" + strconv.Itoa(len(items)) + "\r\n" + strings.Join(items, "")
}

func (s *fakeSentinel) serve(c *sentinelConn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.c.Close()
	}()
	r := bufio.NewReader(c.c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		reply := s.handle(c, args)
		c.mu.Lock()
		_, err = c.w.WriteString(reply)
		if err == nil && r.Buffered() == 0 {
			err = c.w.Flush()
		}
		c.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// handle answers one command.
func (s *fakeSentinel) handle(c *sentinelConn, args []string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	name := strings.ToUpper(args[0])
	if name == "SENTINEL" && len(args) > 1 {
		name += " " + strings.ToUpper(args[1])
	}
	s.counts[name]++
	switch name {
	case "HELLO":
		authed := s.password == ""
		for i := 2; i+2 < len(args); i++ {
			if strings.EqualFold(args[i], "AUTH") {
				s.counts["HELLO AUTH"]++
				if !s.credentialsOK(args[i+1], args[i+2]) {
					return errorReply("WRONGPASS invalid username-password pair or user is disabled.")
				}
				authed = true
			}
		}
		if !authed {
			return errorReply("NOAUTH HELLO must be called with the client already authenticated")
		}
		kv := []string{
			bulk("server"), bulk("redis"), bulk("version"), bulk("7.4.0"), bulk("proto"), integer(3),
			bulk("id"), integer(1), bulk("mode"), bulk("sentinel"), bulk("role"), bulk("sentinel"), bulk("modules"), array(),
		}
		return "%" + strconv.Itoa(len(kv)/2) + "\r\n" + strings.Join(kv, "")
	case "PING":
		return simple("PONG")
	case "CLIENT":
		return simple("OK")
	case "SENTINEL SENTINELS", "SENTINEL REPLICAS", "SENTINEL SLAVES":
		return array()
	case "SENTINEL GET-MASTER-ADDR-BY-NAME":
		addr, ok := "", false
		if len(args) > 2 {
			addr, ok = s.masters[args[2]]
		}
		if !ok {
			return null
		}
		host, port, _ := net.SplitHostPort(addr)
		return array(bulk(host), bulk(port))
	case "SUBSCRIBE":
		var b strings.Builder
		for i, ch := range args[1:] {
			b.WriteString(push(bulk("subscribe"), bulk(ch), integer(int64(i+1))))
		}
		c.subscribed = true
		return b.String()
	case "UNSUBSCRIBE":
		var b strings.Builder
		for _, ch := range args[1:] {
			b.WriteString(push(bulk("unsubscribe"), bulk(ch), integer(0)))
		}
		c.subscribed = false
		return b.String()
	case "AUTH":
		return errorReply("ERR the fake sentinel authenticates in HELLO only")
	default:
		return errorReply("ERR unknown command '" + args[0] + "'")
	}
}
