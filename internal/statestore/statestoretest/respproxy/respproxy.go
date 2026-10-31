// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package respproxy

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/testkit/faultproxy"
)

// Config configures a Proxy.
type Config struct {
	// Fault configures the fault proxy: Upstream (required), Listen,
	// MaxConns, DialTimeout, MaxQueuedBytes, the first Mode, Tap and
	// Seed. ConnTap must be nil: the Counter's takes its place.
	Fault faultproxy.Config
	// Counter configures the counting.
	Counter CounterConfig
}

// Proxy is a running counting RESP proxy: a faultproxy.Proxy whose
// connections a Counter parses. The fault proxy's methods inject TCP
// faults and report connections and bytes (Stats); the Counter's report
// commands and replies (Snapshot, Log) and slow replies down
// (SetReplyDelays).
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
	if c.Fault.ConnTap != nil {
		return nil, errors.New("respproxy: Config.Fault.ConnTap is set; the Counter installs its own")
	}
	ctr := NewCounter(c.Counter)
	fc := c.Fault
	fc.ConnTap = ctr.ConnTap
	fp, err := faultproxy.Start(ctx, fc)
	if err != nil {
		ctr.Close()
		return nil, fmt.Errorf("respproxy: %w", err)
	}
	// The fault proxy closes itself when ctx ends; closing the Counter
	// then too releases held replies, which the fault proxy's Close
	// waits for.
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
