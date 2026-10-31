// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package harness

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// ValkeyInfo is what the State Store breakdown reads of INFO (PBB "Result
// record", G1): the main thread's CPU seconds and EVALSHA's total
// microseconds, and the connected clients the connection counts of G1 and
// O2 read.
type ValkeyInfo struct {
	MainThreadCPU    float64
	EvalshaMicros    float64
	ConnectedClients int64
	At               time.Time
}

// InfoClient reads a State Store's INFO counters over one TLS connection
// as the run's ACL user, for a driver that samples them itself (G1).
type InfoClient struct{ c *infoClient }

// DialInfo connects to addr over TLS verified by caPEM and authenticates
// as user.
func DialInfo(ctx context.Context, addr string, caPEM []byte, user, password string) (*InfoClient, error) {
	c, err := dialInfo(ctx, addr, caPEM, user, password)
	if err != nil {
		return nil, err
	}
	return &InfoClient{c: c}, nil
}

// Info reads INFO clients, cpu and commandstats, stamped with the time.
func (c *InfoClient) Info() (ValkeyInfo, error) {
	v, err := c.c.info()
	v.At = time.Now()
	return v, err
}

// Close closes the connection.
func (c *InfoClient) Close() error { return c.c.Close() }

// infoClient queries INFO over one TLS connection as the run's ACL user.
type infoClient struct {
	conn net.Conn
	r    *bufio.Reader
}

// dialInfo connects to addr over TLS verified by caPEM and authenticates.
func dialInfo(ctx context.Context, addr string, caPEM []byte, user, password string) (*infoClient, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("harness: the run CA holds no certificate")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	d := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: &tls.Config{RootCAs: pool, ServerName: host, MinVersion: tls.VersionTLS12}}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("harness: State Store INFO: %w", err)
	}
	c := &infoClient{conn: conn, r: bufio.NewReader(conn)}
	if _, err := c.do("AUTH", user, password); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

// do sends one command and returns a simple or bulk string reply.
func (c *infoClient) do(args ...string) (string, error) {
	var b strings.Builder
	b.WriteString("*" + strconv.Itoa(len(args)) + "\r\n")
	for _, a := range args {
		b.WriteString("$" + strconv.Itoa(len(a)) + "\r\n" + a + "\r\n")
	}
	_ = c.conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c.conn, b.String()); err != nil {
		return "", err
	}
	return readReply(c.r)
}

// readReply reads one RESP2 simple string, error or bulk string.
func readReply(r *bufio.Reader) (string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return "", err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return "", errors.New("harness: empty RESP reply")
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return "", fmt.Errorf("harness: State Store: %s", line[1:])
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil || n < -1 || n > 64<<20 {
			return "", fmt.Errorf("harness: bad RESP bulk length %q", line)
		}
		if n == -1 {
			return "", nil
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	default:
		return "", fmt.Errorf("harness: unexpected RESP reply %q", line)
	}
}

// info reads INFO clients, cpu and commandstats.
func (c *infoClient) info() (ValkeyInfo, error) {
	clients, err := c.do("INFO", "clients")
	if err != nil {
		return ValkeyInfo{}, err
	}
	cpu, err := c.do("INFO", "cpu")
	if err != nil {
		return ValkeyInfo{}, err
	}
	stats, err := c.do("INFO", "commandstats")
	if err != nil {
		return ValkeyInfo{}, err
	}
	return ParseValkeyInfo(clients + "\n" + cpu + "\n" + stats), nil
}

func (c *infoClient) Close() error { return c.conn.Close() }

// ParseValkeyInfo reads used_cpu_sys_main_thread, used_cpu_user_main_thread,
// cmdstat_evalsha's usec and connected_clients from INFO text.
func ParseValkeyInfo(text string) ValkeyInfo {
	var v ValkeyInfo
	for line := range strings.SplitSeq(text, "\n") {
		k, val, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		switch k {
		case "connected_clients":
			if n, err := strconv.ParseInt(val, 10, 64); err == nil {
				v.ConnectedClients = n
			}
		case "used_cpu_sys_main_thread", "used_cpu_user_main_thread":
			if f, err := strconv.ParseFloat(val, 64); err == nil {
				v.MainThreadCPU += f
			}
		case "cmdstat_evalsha":
			for field := range strings.SplitSeq(val, ",") {
				if u, ok := strings.CutPrefix(field, "usec="); ok {
					if f, err := strconv.ParseFloat(u, 64); err == nil {
						v.EvalshaMicros = f
					}
				}
			}
		}
	}
	return v
}
