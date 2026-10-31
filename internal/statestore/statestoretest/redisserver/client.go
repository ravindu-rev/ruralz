// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package redisserver

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Conn is one admin connection to a server: RESP2, authenticated when the
// deployment has users. It is not safe for concurrent use.
type Conn struct {
	nc net.Conn
	r  *bufio.Reader
	w  *bufio.Writer
}

// dialOptions say how to reach and authenticate to a server.
type dialOptions struct {
	tls      *tls.Config // nil for cleartext
	user     string
	password string // "" sends no AUTH
}

// dialConn opens an admin connection to addr.
func dialConn(ctx context.Context, addr string, o dialOptions) (*Conn, error) {
	nc, err := (&net.Dialer{}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("redisserver: dial %s: %w", addr, err)
	}
	if o.tls != nil {
		cfg := o.tls.Clone()
		if cfg.ServerName == "" {
			cfg.ServerName, _, _ = net.SplitHostPort(addr)
		}
		tc := tls.Client(nc, cfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = nc.Close()
			return nil, fmt.Errorf("redisserver: TLS handshake with %s: %w", addr, err)
		}
		nc = tc
	}
	c := &Conn{nc: nc, r: bufio.NewReaderSize(nc, maxLine), w: bufio.NewWriter(nc)}
	if o.password != "" {
		if _, err := c.Do(ctx, "AUTH", o.user, o.password); err != nil {
			_ = c.Close()
			return nil, err
		}
	}
	return c, nil
}

// aLongTimeAgo is a deadline in the past, which interrupts blocked I/O.
func aLongTimeAgo() time.Time { return time.Unix(1, 0) }

// Do sends one command and reads its reply. An error reply is returned
// as a *ReplyError along with the Value; ctx bounds the round trip.
func (c *Conn) Do(ctx context.Context, args ...string) (Value, error) {
	if len(args) == 0 {
		return Value{}, errors.New("redisserver: Do needs a command")
	}
	dl, _ := ctx.Deadline()
	if err := c.nc.SetDeadline(dl); err != nil {
		return Value{}, fmt.Errorf("redisserver: %w", err)
	}
	// A canceled ctx interrupts the I/O; the callback is over before Do
	// returns, so it never cuts a later call short.
	fired := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(fired)
		_ = c.nc.SetDeadline(aLongTimeAgo())
	})
	defer func() {
		if !stop() {
			<-fired
		}
	}()
	v, err := c.roundTrip(args)
	if err != nil {
		if ctx.Err() != nil {
			err = fmt.Errorf("%w (%w)", ctx.Err(), err)
		}
		return Value{}, fmt.Errorf("redisserver: %s: %w", args[0], err)
	}
	return v, v.Err()
}

func (c *Conn) roundTrip(args []string) (Value, error) {
	if err := writeCommand(c.w, args); err != nil {
		return Value{}, err
	}
	return readValue(c.r)
}

// Close closes the connection.
func (c *Conn) Close() error {
	if err := c.nc.Close(); err != nil {
		return fmt.Errorf("redisserver: close: %w", err)
	}
	return nil
}

// parseInfo parses the text of INFO or CLUSTER INFO into its fields.
func parseInfo(s string) map[string]string {
	out := map[string]string{}
	for line := range strings.Lines(s) {
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = v
		}
	}
	return out
}
