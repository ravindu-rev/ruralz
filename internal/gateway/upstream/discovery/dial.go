// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// DialBudget is the single connect budget of one attempt, shared by every
// cached IP of the Endpoint (05 reqs 5, 11 f and 24; target).
const DialBudget = time.Second

// DialFunc connects to one ip:port, like (*net.Dialer).DialContext. The
// Upstream layer passes the egress guard's dialer, whose Control refuses
// denied socket addresses at connect time (05 req 9).
type DialFunc func(ctx context.Context, network, address string) (net.Conn, error)

// Dial errors. Every error Dial returns is a connect failure for error
// classification (05 req 24: dial expiry is kind connect).
var (
	// ErrNoAddresses reports an Endpoint without any resolved address yet.
	ErrNoAddresses = errors.New("discovery: endpoint has no resolved address")
	// ErrDialBudget reports that the dial budget ran out; it matches
	// context.DeadlineExceeded with errors.Is.
	ErrDialBudget = fmt.Errorf("discovery: dial budget spent: %w", context.DeadlineExceeded)
)

// Dial connects to the first address of addrs that accepts, trying them in
// answer order within one budget on clk (0 means DialBudget; 05 req 5). An
// address the dial function refuses, or that fails, moves on to the next
// while budget remains. The error joins every address's failure, and wraps
// ErrDialBudget when the budget ran out.
func Dial(ctx context.Context, clk clock.Clock, dial DialFunc, network string, addrs []netip.AddrPort, budget time.Duration) (net.Conn, error) {
	if len(addrs) == 0 {
		return nil, ErrNoAddresses
	}
	if budget <= 0 {
		budget = DialBudget
	}
	dctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	t := clk.AfterFunc(budget, func() { cancel(ErrDialBudget) })
	defer t.Stop()
	errs := make([]error, 0, len(addrs)+1)
	for _, ap := range addrs {
		c, err := dial(dctx, network, ap.String())
		if err == nil {
			return c, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", ap, err))
		if dctx.Err() != nil {
			break
		}
	}
	if errors.Is(context.Cause(dctx), ErrDialBudget) {
		errs = append(errs, ErrDialBudget)
	}
	return nil, fmt.Errorf("discovery: dial: %w", errors.Join(errs...))
}
