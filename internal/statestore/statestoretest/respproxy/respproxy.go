// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// Config configures a Proxy.
type Config struct {
	// Fault configures the fault proxy: Upstream (required), Listen,
	// MaxConns, DialTimeout, MaxQueuedBytes and the first Mode. ConnTap
	// must be nil: the Counter's takes its place.
	Fault faultproxy.Config
}

// Proxy is a running counting RESP proxy: a faultproxy.Proxy whose
// connections a Counter parses. The fault proxy's methods inject TCP
// faults and count connections (Stats); the Counter's report commands and
// replies (Snapshot, LogSince) and slow replies down (SetReplyDelays).
type Proxy struct {
	*faultproxy.Proxy
	*Counter

	stop      func() bool
	closeOnce sync.Once
	closeErr  error
}

// Start listens and starts proxying. The proxy closes when ctx ends or
// Close is called.
func Start(ctx context.Context, c Config) (*Proxy, error) {
	return start(ctx, c, clock.Real())
}

// start is Start with the clock of the reply delays.
func start(ctx context.Context, c Config, clk clock.Clock) (*Proxy, error) {
	if c.Fault.ConnTap != nil {
		return nil, errors.New("respproxy: Config.Fault.ConnTap is set; the Counter installs its own")
	}
	ctr := newCounter(clk, logSize)
	fc := c.Fault
	fc.ConnTap = ctr.ConnTap
	fp, err := faultproxy.Start(ctx, fc)
	if err != nil {
		ctr.Close()
		return nil, fmt.Errorf("respproxy: %w", err)
	}
	// The fault proxy closes itself when ctx ends; closing the Counter
	// then too ends its holds and their timers.
	return &Proxy{Proxy: fp, Counter: ctr, stop: context.AfterFunc(ctx, ctr.Close)}, nil
}

// Close releases held replies, then stops listening, closes every
// connection and waits for every goroutine of the proxy. It is
// idempotent.
func (p *Proxy) Close() error {
	p.closeOnce.Do(func() {
		p.stop()
		p.Counter.Close()
		p.closeErr = p.Proxy.Close()
	})
	return p.closeErr
}
