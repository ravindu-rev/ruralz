// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// session is one client connection of a fake server.
type session struct {
	user string // authenticated user; "" before AUTH
}

// handler answers one command with raw RESP.
type handler func(s *session, args []string) string

// fakeServer is a RESP server for tests: in process, or as the fake
// redis-server child process.
type fakeServer struct {
	ln     net.Listener
	handle handler

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	log    [][]string
	wg     sync.WaitGroup
}

func serveFake(ln net.Listener, h handler) *fakeServer {
	f := &fakeServer{ln: ln, handle: h, conns: map[net.Conn]struct{}{}}
	f.wg.Add(1)
	go f.serve()
	return f
}

// startFake serves h on a loopback port, with TLS when cfg is set, until
// the test ends.
func startFake(t *testing.T, cfg *tls.Config, h handler) *fakeServer {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	f := serveFake(ln, h)
	t.Cleanup(f.close)
	return f
}

func (f *fakeServer) addr() string { return f.ln.Addr().String() }

// server returns a Server reaching f.
func (f *fakeServer) server(name string, acc access) *Server {
	return &Server{name: name, addr: f.addr(), client: f.addr(), acc: acc}
}

func (f *fakeServer) commands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]string(nil), f.log...)
}

func (f *fakeServer) serve() {
	defer f.wg.Done()
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		if f.closed {
			f.mu.Unlock()
			_ = c.Close()
			return
		}
		f.conns[c] = struct{}{}
		f.wg.Add(1)
		f.mu.Unlock()
		go f.serveConn(c)
	}
}

func (f *fakeServer) serveConn(c net.Conn) {
	defer f.wg.Done()
	defer func() { _ = c.Close() }()
	r := bufio.NewReaderSize(c, maxLine)
	s := &session{}
	for {
		v, err := readValue(r)
		if err != nil {
			return
		}
		args := make([]string, len(v.Elems))
		for i, e := range v.Elems {
			args[i] = e.Str
		}
		if len(args) == 0 {
			return
		}
		f.mu.Lock()
		f.log = append(f.log, args)
		f.mu.Unlock()
		if _, err := io.WriteString(c, f.handle(s, args)); err != nil {
			return
		}
	}
}

func (f *fakeServer) close() {
	_ = f.ln.Close()
	f.mu.Lock()
	f.closed = true
	for c := range f.conns {
		_ = c.Close()
	}
	f.mu.Unlock()
	f.wg.Wait()
}

// bulk encodes s as a bulk string.
func bulk(s string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s) }

// splitArgs splits a configuration line as Redis's sdssplitargs does: bare
// words, or double-quoted words with \xHH, \n, \r, \t and \<char>
// escapes.
func splitArgs(line string) ([]string, error) {
	var out []string
	for i := 0; ; {
		for i < len(line) && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i == len(line) {
			return out, nil
		}
		var b strings.Builder
		if line[i] != '"' {
			for i < len(line) && line[i] != ' ' && line[i] != '\t' {
				if line[i] == '"' || line[i] == '\'' {
					return nil, fmt.Errorf("quote inside a bare word in %q", line)
				}
				b.WriteByte(line[i])
				i++
			}
			out = append(out, b.String())
			continue
		}
		for i++; ; i++ {
			if i == len(line) {
				return nil, fmt.Errorf("unbalanced quotes in %q", line)
			}
			c := line[i]
			if c == '"' {
				i++
				if i < len(line) && line[i] != ' ' {
					return nil, fmt.Errorf("closing quote not followed by a space in %q", line)
				}
				break
			}
			if c != '\\' || i+1 == len(line) {
				b.WriteByte(c)
				continue
			}
			i++
			switch line[i] {
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'x':
				n, err := strconv.ParseUint(line[i+1:min(i+3, len(line))], 16, 8)
				if err != nil {
					return nil, fmt.Errorf("bad \\x escape in %q", line)
				}
				b.WriteByte(byte(n))
				i += 2
			default:
				b.WriteByte(line[i])
			}
		}
		out = append(out, b.String())
	}
}

// parseConf reads a configuration file into its directives.
func parseConf(text string) ([][]string, error) {
	var out [][]string
	for line := range strings.Lines(text) {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		args, err := splitArgs(line)
		if err != nil {
			return nil, err
		}
		out = append(out, args)
	}
	return out, nil
}

// lookup returns the arguments of the last directive named name.
func lookup(conf [][]string, name string) ([]string, bool) {
	for i := len(conf) - 1; i >= 0; i-- {
		if conf[i][0] == name {
			return conf[i][1:], true
		}
	}
	return nil, false
}

// helperEnv makes the test binary act as the fake redis-server.
const helperEnv = "RURALZ_REDISSERVER_HELPER"

// helperLauncher returns a launcher running this test binary as
// redis-server (TestHelperRedisServer).
func helperLauncher(t *testing.T, ports func(context.Context) (int, error)) *launcher {
	t.Helper()
	if ports == nil {
		ports = freePort
	}
	return &launcher{
		tb:    t,
		path:  os.Args[0],
		args:  []string{"-test.run=^TestHelperRedisServer$", "-test.count=1"},
		env:   append(childEnv(), helperEnv+"=1"),
		ports: ports,
	}
}

// TestHelperRedisServer is the fake redis-server of the process-flavor
// tests: it reads the configuration file the launcher writes, refuses
// what it does not know as Redis does, binds the port or TLS port, logs
// Redis's readiness line and answers the commands the launcher sends.
func TestHelperRedisServer(t *testing.T) {
	if os.Getenv(helperEnv) == "" {
		t.Skip("child process of the process-flavor tests")
	}
	os.Exit(fakeRedisMain(flag.Args()))
}

// knownDirectives are the directives the fake redis-server accepts.
func knownDirectives() map[string]bool {
	return map[string]bool{
		"bind": true, "dir": true, "save": true, "appendonly": true, "port": true, "tls-port": true,
		"tls-cert-file": true, "tls-key-file": true, "tls-ca-cert-file": true, "tls-auth-clients": true,
		"tls-replication": true, "tls-cluster": true, "user": true, "replicaof": true, "masteruser": true,
		"masterauth": true, "cluster-enabled": true, "cluster-config-file": true, "cluster-port": true,
		"cluster-announce-ip": true, "cluster-announce-port": true, "cluster-announce-tls-port": true,
		"maxmemory": true,
	}
}

func fakeRedisMain(args []string) int {
	out := os.Stdout
	if len(args) == 0 {
		_, _ = fmt.Fprintln(out, "no configuration file")
		return 1
	}
	text, err := os.ReadFile(args[len(args)-1])
	if err != nil {
		_, _ = fmt.Fprintln(out, err)
		return 1
	}
	conf, err := parseConf(string(text))
	if err == nil {
		for _, d := range conf {
			if !knownDirectives()[d[0]] {
				err = fmt.Errorf("unknown directive %q", d[0])
				break
			}
		}
	}
	if err != nil {
		_, _ = fmt.Fprintf(out, "*** FATAL CONFIG FILE ERROR ***\n%v\nBad directive or wrong number of arguments\n", err)
		return 1
	}
	var cfg *tls.Config
	port, _ := lookup(conf, "port")
	if tp, ok := lookup(conf, "tls-port"); ok {
		port = tp
		certFile, _ := lookup(conf, "tls-cert-file")
		keyFile, _ := lookup(conf, "tls-key-file")
		cert, err := tls.LoadX509KeyPair(certFile[0], keyFile[0])
		if err != nil {
			_, _ = fmt.Fprintln(out, err)
			return 1
		}
		cfg = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}
	}
	addr := net.JoinHostPort("127.0.0.1", port[0])
	ln, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", addr)
	if err != nil {
		reason := err.Error()
		if isAddrInUse(err) {
			reason = "bind: Address already in use"
		}
		_, _ = fmt.Fprintf(out, "Warning: Could not create server TCP listening socket %s: %s\n", addr, reason)
		return 1
	}
	if cfg != nil {
		ln = tls.NewListener(ln, cfg)
	}
	serveFake(ln, newFakeRedis(conf).handle)
	_, _ = fmt.Fprintln(out, "* Ready to accept connections tcp")
	time.Sleep(time.Hour) // until the parent kills it
	return 0
}

// isAddrInUse reports a bind failure because the port is taken, on any
// platform.
func isAddrInUse(err error) bool {
	var se *os.SyscallError
	return errors.As(err, &se) && (strings.Contains(se.Err.Error(), "in use") || strings.Contains(se.Err.Error(), "normally permitted"))
}

// fakeRedis answers like redis-server for what the launcher and the
// tests send.
type fakeRedis struct {
	conf [][]string

	mu       sync.Mutex
	data     map[string]string
	users    map[string]string // on users and their passwords
	noAuth   bool              // the default user is on without a password
	replica  bool
	clusterd bool
}

func newFakeRedis(conf [][]string) *fakeRedis {
	r := &fakeRedis{conf: conf, data: map[string]string{}, users: map[string]string{}, noAuth: true}
	for _, d := range conf {
		switch d[0] {
		case "user":
			on, pw := false, ""
			for _, a := range d[2:] {
				switch {
				case a == "on":
					on = true
				case a == "off":
					on = false
				case strings.HasPrefix(a, ">"):
					pw = a[1:]
				}
			}
			if d[1] == "default" {
				r.noAuth = on && pw == ""
			}
			if on && pw != "" {
				r.users[d[1]] = pw
			}
		case "replicaof":
			r.replica = true
		case "cluster-enabled":
			r.clusterd = d[1] == "yes"
		}
	}
	return r
}

func (r *fakeRedis) handle(s *session, args []string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	name := strings.ToUpper(args[0])
	if name == "AUTH" {
		if len(args) == 3 && r.users[args[1]] == args[2] && args[2] != "" {
			s.user = args[1]
			return "+OK\r\n"
		}
		return "-WRONGPASS invalid username-password pair or user is disabled.\r\n"
	}
	if !r.noAuth && s.user == "" {
		return "-NOAUTH Authentication required.\r\n"
	}
	switch name {
	case "PING":
		return "+PONG\r\n"
	case "SET":
		r.data[args[1]] = args[2]
		return "+OK\r\n"
	case "GET":
		if v, ok := r.data[args[1]]; ok {
			return bulk(v)
		}
		return "$-1\r\n"
	case "INFO":
		role, link := "master", ""
		if r.replica {
			role, link = "slave", "master_link_status:up\r\n"
		}
		return bulk("# Server\r\nredis_version:7.0.15\r\n\r\n# Replication\r\nrole:" + role + "\r\n" + link)
	case "REPLICAOF":
		r.replica = false
		return "+OK\r\n"
	case "CONFIG":
		if v, ok := lookup(r.conf, args[2]); ok {
			return "*2\r\n" + bulk(args[2]) + bulk(strings.Join(v, " "))
		}
		return "*0\r\n"
	case "CLUSTER":
		if !r.clusterd {
			return "-ERR This instance has cluster support disabled\r\n"
		}
		if strings.EqualFold(args[1], "INFO") {
			return bulk("cluster_state:ok\r\ncluster_slots_ok:16384\r\ncluster_known_nodes:3\r\n")
		}
		return "+OK\r\n"
	}
	return fmt.Sprintf("-ERR unknown command '%s'\r\n", args[0])
}
