// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redis

import (
	"bufio"
	"crypto/sha1" //nolint:gosec // G505: the fake server names scripts by SHA-1, as Redis does
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/statestore/keys"
)

// fakeServer is a loopback RESP3 server for the unit tests: the commands
// the State client sends (HELLO, CLIENT, SELECT, PING, ROLE, TIME, GET,
// SET, HMGET, INFO, SCRIPT LOAD and FLUSH, EVALSHA, CLUSTER SLOTS), with
// EVALSHA of the library running fakeLibrary, a line-by-line Go mirror of
// ruralz_v1.lua, on strings and hashes that expire on its clock. Faults
// hold, fail, garble or drop the replies to a command.
type fakeServer struct {
	t    testing.TB
	ln   net.Listener
	addr string
	clk  clock.Clock

	user, password string
	cluster        *fakeCluster
	version        string

	mu         sync.Mutex
	strs       map[string]fstr
	hashes     map[string]*fhash
	scripts    map[string]bool
	counts     map[string]int
	log        []string
	conns      map[net.Conn]struct{}
	accepted   int
	faults     []*fault
	info       string
	scriptsOff string
	reject     bool
	// replica makes ROLE answer slave and writes READONLY, as a primary a
	// failover demoted does.
	replica bool
	closed  bool
	wg      sync.WaitGroup
}

// fstr is a string value with its expiry in Unix ms (0: none).
type fstr struct {
	v   string
	exp int64
}

// fhash is a hash with its expiry in Unix ms (0: none).
type fhash struct {
	f   map[string]string
	exp int64
}

// fault changes the replies to one command name ("SCRIPT LOAD" for a
// subcommand; "" for every command).
type fault struct {
	cmd   string
	times int // 0: until cleared
	err   string
	raw   string
	hold  chan struct{}
	drop  bool
	hits  int
}

// fakeCluster is the slot ownership a cluster of fake servers shares.
type fakeCluster struct {
	mu     sync.Mutex
	ranges []fakeRange
}

type fakeRange struct {
	first, last int
	srv         *fakeServer
}

// owner returns the server owning slot, or nil.
func (c *fakeCluster) owner(slot int) *fakeServer {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.ranges {
		if slot >= r.first && slot <= r.last {
			return r.srv
		}
	}
	return nil
}

// fakeOptions configure a fakeServer.
type fakeOptions struct {
	user, password string
	tls            *tls.Config
	cluster        *fakeCluster
	version        string
	// scriptsOff is the error SCRIPT and EVALSHA answer from the start.
	scriptsOff string
	// info is the INFO memory reply; default 1,000 bytes used of 1 GB,
	// noeviction.
	info string
	// reject closes every connection once accepted.
	reject bool
}

// newFakeServer starts a fake server on a loopback port; it closes when
// t ends.
func newFakeServer(t testing.TB, clk clock.Clock, o fakeOptions) *fakeServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if o.tls != nil {
		ln = tls.NewListener(ln, o.tls)
	}
	s := &fakeServer{
		t: t, ln: ln, addr: ln.Addr().String(), clk: clk, user: o.user, password: o.password,
		cluster: o.cluster, version: o.version, scriptsOff: o.scriptsOff, info: o.info, reject: o.reject,
		strs: map[string]fstr{}, hashes: map[string]*fhash{}, scripts: map[string]bool{},
		counts: map[string]int{}, conns: map[net.Conn]struct{}{},
	}
	if s.version == "" {
		s.version = "7.4.0"
	}
	if s.info == "" {
		s.info = memInfo(1000, 1_000_000_000, noEviction)
	}
	s.wg.Go(s.accept)
	t.Cleanup(s.close)
	return s
}

// url returns a redis:// URL of the server.
func (s *fakeServer) url() string { return "redis://" + s.addr }

func (s *fakeServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			_ = c.Close()
			return
		}
		s.accepted++
		if s.reject {
			s.mu.Unlock()
			_ = c.Close()
			continue
		}
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		s.wg.Go(func() { s.serve(c) })
	}
}

// close stops the server and waits for its goroutines.
func (s *fakeServer) close() {
	s.mu.Lock()
	s.closed = true
	for _, f := range s.faults {
		if f.hold != nil {
			select {
			case <-f.hold:
			default:
				close(f.hold)
			}
		}
	}
	for c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
	_ = s.ln.Close()
	s.wg.Wait()
}

// dropConns closes every client connection.
func (s *fakeServer) dropConns() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.Close()
	}
}

// inject adds f and returns it.
func (s *fakeServer) inject(f *fault) *fault {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults = append(s.faults, f)
	return f
}

// clearFaults removes every fault and releases held replies.
func (s *fakeServer) clearFaults() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.faults {
		if f.hold != nil {
			select {
			case <-f.hold:
			default:
				close(f.hold)
			}
		}
	}
	s.faults = nil
}

// count returns how many commands named name arrived.
func (s *fakeServer) count(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[name]
}

// connections returns the connections accepted so far.
func (s *fakeServer) connections() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.accepted
}

// commands returns the command names received, in order.
func (s *fakeServer) commands() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.log...)
}

// memInfo renders an INFO memory reply; an empty policy omits the field.
func memInfo(used, maxMem int64, policy string) string {
	s := "# Memory\r\nused_memory:" + strconv.FormatInt(used, 10) + "\r\nmaxmemory:" + strconv.FormatInt(maxMem, 10) + "\r\n"
	if policy != "" {
		s += "maxmemory_policy:" + policy + "\r\n"
	}
	return s
}

// setInfo sets the INFO memory reply.
func (s *fakeServer) setInfo(info string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.info = info
}

// flushScripts forgets every loaded script (SCRIPT FLUSH, a failover).
func (s *fakeServer) flushScripts() {
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.scripts)
}

// setReplica makes the server a replica (true) or a primary again.
func (s *fakeServer) setReplica(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.replica = on
}

// open returns the client connections the server holds now.
func (s *fakeServer) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// setReject makes the server close every new connection at once.
func (s *fakeServer) setReject(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reject = on
}

// refuseScripts makes SCRIPT answer err ("" serves it again).
func (s *fakeServer) refuseScripts(err string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.scriptsOff = err
}

// cmdName returns the counted name of a command.
func cmdName(args []string) string {
	name := strings.ToUpper(args[0])
	switch name {
	case "SCRIPT", "CLIENT", "CLUSTER", "CONFIG":
		if len(args) > 1 {
			return name + " " + strings.ToUpper(args[1])
		}
	}
	return name
}

// faultFor returns the fault matching name, counting its hit.
func (s *fakeServer) faultFor(name string) *fault {
	for i, f := range s.faults {
		if f.cmd != "" && f.cmd != name {
			continue
		}
		f.hits++
		if f.times > 0 && f.hits >= f.times {
			s.faults = append(s.faults[:i:i], s.faults[i+1:]...)
		}
		return f
	}
	return nil
}

func (s *fakeServer) serve(c net.Conn) {
	defer func() {
		s.mu.Lock()
		delete(s.conns, c)
		s.mu.Unlock()
		_ = c.Close()
	}()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	authed := s.password == ""
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		name := cmdName(args)
		s.mu.Lock()
		s.counts[name]++
		s.log = append(s.log, name)
		f := s.faultFor(name)
		var reply string
		switch {
		case f != nil && f.err != "":
			reply = "-" + f.err + "\r\n"
		case f != nil && f.raw != "":
			reply = f.raw
		case f != nil && f.drop:
		default:
			reply = s.handle(args, &authed)
		}
		s.mu.Unlock()
		if f != nil && f.drop {
			return
		}
		if f != nil && f.hold != nil {
			<-f.hold
		}
		if _, err := w.WriteString(reply); err != nil {
			return
		}
		if r.Buffered() == 0 {
			if err := w.Flush(); err != nil {
				return
			}
		}
	}
}

// readCommand reads one RESP array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if !strings.HasPrefix(line, "*") {
		return strings.Fields(line), nil
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil || n < 1 {
		return nil, errors.New("bad array")
	}
	args := make([]string, n)
	for i := range n {
		h, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		h = strings.TrimRight(h, "\r\n")
		if !strings.HasPrefix(h, "$") {
			return nil, errors.New("bad bulk")
		}
		size, err := strconv.Atoi(h[1:])
		if err != nil || size < 0 {
			return nil, errors.New("bad bulk size")
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

// RESP3 reply encoders.
func bulk(s string) string       { return "$" + strconv.Itoa(len(s)) + "\r\n" + s + "\r\n" }
func integer(n int64) string     { return ":" + strconv.FormatInt(n, 10) + "\r\n" }
func simple(s string) string     { return "+" + s + "\r\n" }
func errorReply(s string) string { return "-" + s + "\r\n" }

const null = "_\r\n"

func array(items ...string) string {
	return "*" + strconv.Itoa(len(items)) + "\r\n" + strings.Join(items, "")
}

// nowMs returns the server's time in Unix milliseconds.
func (s *fakeServer) nowMs() int64 { return s.clk.Now().UnixMilli() }

// timeNow returns TIME's seconds and microseconds.
func (s *fakeServer) timeNow() (sec, usec int64) {
	t := s.clk.Now()
	return t.Unix(), int64(t.Nanosecond() / 1000)
}

// moved returns a MOVED reply when key belongs to another cluster node.
func (s *fakeServer) moved(key string) string {
	if s.cluster == nil {
		return ""
	}
	slot := int(keys.SlotString(key))
	o := s.cluster.owner(slot)
	if o == nil {
		return errorReply("CLUSTERDOWN Hash slot not served")
	}
	if o != s {
		return errorReply(fmt.Sprintf("MOVED %d %s", slot, o.addr))
	}
	return ""
}

// handle answers one command; the caller holds s.mu.
func (s *fakeServer) handle(args []string, authed *bool) string {
	name := strings.ToUpper(args[0])
	switch name {
	case "HELLO":
		return s.hello(args, authed)
	case "AUTH":
		user, pass := "default", ""
		if len(args) == 2 {
			pass = args[1]
		} else if len(args) >= 3 {
			user, pass = args[1], args[2]
		}
		if !s.credentialsOK(user, pass) {
			return errorReply("WRONGPASS invalid username-password pair or user is disabled.")
		}
		*authed = true
		return simple("OK")
	}
	if !*authed {
		return errorReply("NOAUTH Authentication required.")
	}
	switch name {
	case "PING":
		return simple("PONG")
	case "ROLE":
		if s.replica {
			return array(bulk("slave"), bulk("127.0.0.1"), integer(6379), bulk("connected"), integer(0))
		}
		return array(bulk("master"), integer(0), array())
	case "CLIENT", "SELECT":
		return simple("OK")
	case "TIME":
		sec, usec := s.timeNow()
		return array(bulk(strconv.FormatInt(sec, 10)), bulk(strconv.FormatInt(usec, 10)))
	case "INFO":
		return bulk(s.info)
	case "CLUSTER":
		return s.clusterCmd(args)
	case "SCRIPT":
		return s.scriptCmd(args)
	case "EVAL":
		return errorReply("ERR the fake server refuses EVAL")
	case "EVALSHA":
		if s.replica {
			return errorReply("READONLY You can't write against a read only replica.")
		}
		return s.evalsha(args)
	case "GET", "SET", "HMGET":
		if len(args) < 2 {
			return errorReply("ERR wrong number of arguments")
		}
		if name == "SET" && s.replica {
			return errorReply("READONLY You can't write against a read only replica.")
		}
		if m := s.moved(args[1]); m != "" {
			return m
		}
		switch name {
		case "GET":
			if v, ok := s.get(args[1]); ok {
				return bulk(v)
			}
			return null
		case "HMGET":
			h := s.hash(args[1])
			items := make([]string, 0, len(args)-2)
			for _, f := range args[2:] {
				if v, ok := h.get(f); ok {
					items = append(items, bulk(v))
				} else {
					items = append(items, null)
				}
			}
			return array(items...)
		default:
			return s.setCmd(args)
		}
	default:
		return errorReply(fmt.Sprintf("ERR unknown command '%s'", args[0]))
	}
}

func (s *fakeServer) credentialsOK(user, pass string) bool {
	if s.password == "" {
		return true
	}
	if s.user == "" {
		return pass == s.password
	}
	return user == s.user && pass == s.password
}

func (s *fakeServer) hello(args []string, authed *bool) string {
	for i := 2; i < len(args); i++ {
		if strings.EqualFold(args[i], "AUTH") && i+2 < len(args) {
			if !s.credentialsOK(args[i+1], args[i+2]) {
				return errorReply("WRONGPASS invalid username-password pair or user is disabled.")
			}
			*authed = true
		}
	}
	if !*authed {
		return errorReply("NOAUTH HELLO must be called with the client already authenticated")
	}
	mode := "standalone"
	if s.cluster != nil {
		mode = "cluster"
	}
	kv := []string{
		bulk("server"), bulk("redis"), bulk("version"), bulk(s.version), bulk("proto"), integer(3),
		bulk("id"), integer(1), bulk("mode"), bulk(mode), bulk("role"), bulk("master"), bulk("modules"), array(),
	}
	return "%" + strconv.Itoa(len(kv)/2) + "\r\n" + strings.Join(kv, "")
}

func (s *fakeServer) clusterCmd(args []string) string {
	if len(args) < 2 || !strings.EqualFold(args[1], "SLOTS") {
		return errorReply("ERR unknown subcommand")
	}
	if s.cluster == nil {
		return errorReply("ERR This instance has cluster support disabled")
	}
	s.cluster.mu.Lock()
	defer s.cluster.mu.Unlock()
	items := make([]string, 0, len(s.cluster.ranges))
	for _, r := range s.cluster.ranges {
		host, port, _ := net.SplitHostPort(r.srv.addr)
		p, _ := strconv.ParseInt(port, 10, 64)
		items = append(items, array(integer(int64(r.first)), integer(int64(r.last)),
			array(bulk(host), integer(p), bulk("node-"+port))))
	}
	return array(items...)
}

func (s *fakeServer) scriptCmd(args []string) string {
	if s.scriptsOff != "" {
		return errorReply(s.scriptsOff)
	}
	if len(args) < 2 {
		return errorReply("ERR wrong number of arguments")
	}
	switch strings.ToUpper(args[1]) {
	case "LOAD":
		if len(args) != 3 {
			return errorReply("ERR wrong number of arguments")
		}
		sum := sha1.Sum([]byte(args[2])) //nolint:gosec // G401: the script's name, as Redis computes it
		sha := hex.EncodeToString(sum[:])
		s.scripts[sha] = args[2] == script
		return bulk(sha)
	case "FLUSH":
		clear(s.scripts)
		return simple("OK")
	default:
		return errorReply("ERR unknown subcommand")
	}
}

func (s *fakeServer) evalsha(args []string) string {
	if s.scriptsOff != "" {
		return errorReply(s.scriptsOff)
	}
	if len(args) < 3 {
		return errorReply("ERR wrong number of arguments")
	}
	lib, loaded := s.scripts[strings.ToLower(args[1])]
	if !loaded {
		return errorReply("NOSCRIPT No matching script. Please use EVAL.")
	}
	if !lib {
		return errorReply("ERR the fake server runs only the ruralz_v1 library")
	}
	n, err := strconv.Atoi(args[2])
	if err != nil || n < 0 || 3+n > len(args) {
		return errorReply("ERR Number of keys can't be greater than number of args")
	}
	keyArgs, argv := args[3:3+n], args[3+n:]
	if s.cluster != nil {
		for _, k := range keyArgs {
			if m := s.moved(k); m != "" {
				return m
			}
		}
	}
	return s.library(keyArgs, argv)
}

func (s *fakeServer) setCmd(args []string) string {
	if len(args) < 3 {
		return errorReply("ERR wrong number of arguments")
	}
	key, val := args[1], args[2]
	nx := false
	var px int64
	for i := 3; i < len(args); i++ {
		switch strings.ToUpper(args[i]) {
		case "NX":
			nx = true
		case "PX":
			if i+1 >= len(args) {
				return errorReply("ERR syntax error")
			}
			v, err := strconv.ParseInt(args[i+1], 10, 64)
			if err != nil || v <= 0 {
				return errorReply("ERR invalid expire time in 'set' command")
			}
			px = v
			i++
		default:
			return errorReply("ERR syntax error")
		}
	}
	if _, held := s.get(key); nx && held {
		return null
	}
	s.set(key, val, px)
	return simple("OK")
}

// The keyspace; the caller holds s.mu.

func (s *fakeServer) live(exp int64) bool { return exp == 0 || s.nowMs() <= exp }

func (s *fakeServer) get(k string) (string, bool) {
	v, ok := s.strs[k]
	if !ok || !s.live(v.exp) {
		delete(s.strs, k)
		return "", false
	}
	return v.v, true
}

// set writes a string with a PX expiry (0: none), dropping a hash of the
// same name.
func (s *fakeServer) set(k, v string, px int64) {
	var exp int64
	if px > 0 {
		exp = s.nowMs() + px
	}
	delete(s.hashes, k)
	s.strs[k] = fstr{v: v, exp: exp}
}

// hash returns the live hash k, or an empty one that is not stored.
func (s *fakeServer) hash(k string) *fhash {
	h, ok := s.hashes[k]
	if !ok || !s.live(h.exp) {
		delete(s.hashes, k)
		return &fhash{f: map[string]string{}}
	}
	return h
}

func (h *fhash) get(f string) (string, bool) {
	v, ok := h.f[f]
	return v, ok
}

// hset writes fields of hash k, creating it.
func (s *fakeServer) hset(k string, kv ...string) {
	h, ok := s.hashes[k]
	if !ok || !s.live(h.exp) {
		h = &fhash{f: map[string]string{}}
		s.hashes[k] = h
		delete(s.strs, k)
	}
	for i := 0; i+1 < len(kv); i += 2 {
		h.f[kv[i]] = kv[i+1]
	}
}

// hdel deletes fields of hash k; an emptied hash is deleted.
func (s *fakeServer) hdel(k string, fields ...string) {
	h, ok := s.hashes[k]
	if !ok {
		return
	}
	for _, f := range fields {
		delete(h.f, f)
	}
	if len(h.f) == 0 {
		delete(s.hashes, k)
	}
}

// exists reports whether k holds a live value.
func (s *fakeServer) exists(k string) bool {
	if _, ok := s.get(k); ok {
		return true
	}
	h, ok := s.hashes[k]
	return ok && s.live(h.exp)
}

// pttl returns k's remaining TTL in ms: -2 when missing, -1 without one.
func (s *fakeServer) pttl(k string) int64 {
	if v, ok := s.strs[k]; ok && s.live(v.exp) {
		if v.exp == 0 {
			return -1
		}
		return v.exp - s.nowMs()
	}
	if h, ok := s.hashes[k]; ok && s.live(h.exp) {
		if h.exp == 0 {
			return -1
		}
		return h.exp - s.nowMs()
	}
	return -2
}

// pexpireAt sets k's expiry to the Unix ms at.
func (s *fakeServer) pexpireAt(k string, at int64) {
	if v, ok := s.strs[k]; ok && s.live(v.exp) {
		v.exp = at
		s.strs[k] = v
		return
	}
	if h, ok := s.hashes[k]; ok && s.live(h.exp) {
		h.exp = at
	}
}

// inspect returns a key's value (a hash's names field) and TTL, for the
// conformance suite.
func (s *fakeServer) inspect(k string, hashField string) (string, time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v string
	if hashField != "" {
		h, ok := s.hashes[k]
		if !ok || !s.live(h.exp) {
			return "", 0, false
		}
		v = h.f[hashField]
	} else {
		var ok bool
		if v, ok = s.get(k); !ok {
			return "", 0, false
		}
	}
	return v, time.Duration(s.pttl(k)) * time.Millisecond, true
}

// tamper rewrites one hash field, as a compromised State Store could.
func (s *fakeServer) tamper(k, field string, edit func([]byte) []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hashes[k]
	if !ok || !s.live(h.exp) {
		return false
	}
	v, ok := h.f[field]
	if !ok {
		return false
	}
	h.f[field] = string(edit([]byte(v)))
	return true
}
