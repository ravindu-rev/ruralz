// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"bufio"
	"errors"
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

// cmd encodes args as a RESP array of bulk strings, as clients send.
func cmd(args ...string) string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		_, _ = fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	return b.String()
}

// upstream is a loopback RESP server answering each command with
// reply(args).
type upstream struct {
	ln    net.Listener
	reply func(args []string) string

	mu     sync.Mutex
	conns  map[net.Conn]struct{}
	closed bool
	wg     sync.WaitGroup
}

// defaultReply answers like a State Store whose script cache is empty.
func defaultReply(args []string) string {
	switch strings.ToUpper(args[0]) {
	case "PING":
		return "+PONG\r\n"
	case "EVALSHA":
		return "-NOSCRIPT No matching script. Please use EVAL.\r\n"
	case "GET":
		return "$-1\r\n"
	case "VSIM":
		return "*0\r\n"
	}
	return "+OK\r\n"
}

func startUpstream(t *testing.T, reply func(args []string) string) *upstream {
	t.Helper()
	ln, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{ln: ln, reply: reply, conns: map[net.Conn]struct{}{}}
	u.wg.Add(1)
	go u.serve()
	t.Cleanup(u.close)
	return u
}

func (u *upstream) addr() string { return u.ln.Addr().String() }

func (u *upstream) serve() {
	defer u.wg.Done()
	for {
		c, err := u.ln.Accept()
		if err != nil {
			return
		}
		u.mu.Lock()
		if u.closed {
			u.mu.Unlock()
			_ = c.Close()
			return
		}
		u.conns[c] = struct{}{}
		u.wg.Add(1)
		u.mu.Unlock()
		go u.handle(c)
	}
}

func (u *upstream) handle(c net.Conn) {
	defer u.wg.Done()
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if _, err := io.WriteString(c, u.reply(args)); err != nil {
			return
		}
	}
}

func (u *upstream) close() {
	_ = u.ln.Close()
	u.mu.Lock()
	u.closed = true
	for c := range u.conns {
		_ = c.Close()
	}
	u.mu.Unlock()
	u.wg.Wait()
}

// readCommand reads one array of bulk strings.
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "*"), "\r\n"))
	if err != nil || !strings.HasPrefix(line, "*") || n < 1 {
		return nil, fmt.Errorf("bad command header %q", line)
	}
	args := make([]string, n)
	for i := range args {
		if line, err = r.ReadString('\n'); err != nil {
			return nil, err
		}
		size, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(line, "$"), "\r\n"))
		if err != nil {
			return nil, fmt.Errorf("bad argument header %q", line)
		}
		buf := make([]byte, size+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args[i] = string(buf[:size])
	}
	return args, nil
}

// client is a raw RESP client connection.
type client struct {
	t *testing.T
	c net.Conn
	r *bufio.Reader
}

func dialClient(t *testing.T, addr string) *client {
	t.Helper()
	c, err := (&net.Dialer{Timeout: 10 * time.Second}).DialContext(t.Context(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return &client{t: t, c: c, r: bufio.NewReader(c)}
}

func (cl *client) send(s string) {
	cl.t.Helper()
	if _, err := io.WriteString(cl.c, s); err != nil {
		cl.t.Fatal(err)
	}
}

// reply reads one reply of the kinds upstream sends: "+PONG", "-ERR x",
// ":1", "$-1", "$<payload>" or "*0".
func (cl *client) reply() string {
	cl.t.Helper()
	s, err := cl.read()
	if err != nil {
		cl.t.Fatal(err)
	}
	return s
}

// read reads one reply; the deadline only bounds a broken test.
func (cl *client) read() (string, error) {
	_ = cl.c.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := cl.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimSuffix(line, "\r\n")
	if n, err := strconv.Atoi(strings.TrimPrefix(line, "$")); strings.HasPrefix(line, "$") && err == nil && n >= 0 {
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(cl.r, buf); err != nil {
			return "", err
		}
		return "$" + string(buf[:n]), nil
	}
	return line, nil
}

// isReset reports a read error other than an orderly close or a timeout:
// the RST of a reset connection, whatever the platform calls it.
func isReset(err error) bool {
	return err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, os.ErrDeadlineExceeded)
}

// waitFor polls cond; the deadline only bounds a broken test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}
