// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package listener

import (
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// TestAcceptRetryBackoff checks the accept-error backoff: 5 ms after the
// first error, doubling after each further one, at most 1 s.
func TestAcceptRetryBackoff(t *testing.T) {
	ms := time.Millisecond
	tests := []struct{ prev, want time.Duration }{
		{prev: 0, want: 5 * ms},
		{prev: 5 * ms, want: 10 * ms},
		{prev: 10 * ms, want: 20 * ms},
		{prev: 320 * ms, want: 640 * ms},
		{prev: 640 * ms, want: time.Second},
		{prev: time.Second, want: time.Second},
	}
	for _, tt := range tests {
		if got := nextAcceptRetry(tt.prev); got != tt.want {
			t.Errorf("nextAcceptRetry(%v) = %v, want %v", tt.prev, got, tt.want)
		}
	}
}

// errListener is a net.Listener whose Accept returns the errors the test
// sends, then net.ErrClosed once closed; it counts Accept calls.
type errListener struct {
	next   chan error
	calls  atomic.Int64
	closed chan struct{}
	once   sync.Once
}

func (l *errListener) Accept() (net.Conn, error) {
	l.calls.Add(1)
	select {
	case err := <-l.next:
		return nil, err
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *errListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (*errListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)} }

// TestAcceptLoopBacksOff checks that the accept loop waits on the Set's
// clock after an accept error other than a closed socket, 5 ms and then
// 10 ms, before it calls Accept again, and ends when the socket closes.
func TestAcceptLoopBacksOff(t *testing.T) {
	h := newHarness(t)
	fl := &errListener{next: make(chan error), closed: make(chan struct{})}
	ep := &endpoint{s: h.set, key: snapshot.ListenerKey{Name: "web"}, acc: fl, stopped: make(chan struct{}), unlinger: make(chan struct{})}
	ep.metrics.Store(metricsOf(httpListener("web", 0)))
	done := make(chan struct{})
	h.set.wg.Add(1)
	go func() {
		defer close(done)
		ep.acceptLoop()
	}()
	tooMany := errors.New("accept: too many open files")
	calls := int64(1)
	for _, wait := range []time.Duration{5 * time.Millisecond, 10 * time.Millisecond} {
		waitFor(t, "the accept loop to call Accept", func() bool { return fl.calls.Load() == calls })
		fl.next <- tooMany
		waitFor(t, "the retry timer", func() bool { return h.clk.Pending() == 1 })
		h.clk.Advance(wait - time.Millisecond)
		if got := fl.calls.Load(); got != calls {
			t.Fatalf("Accept called %d times before the %v wait ended, want %d", got, wait, calls)
		}
		h.clk.Advance(time.Millisecond)
		calls++
	}
	waitFor(t, "the accept loop to call Accept", func() bool { return fl.calls.Load() == calls })
	_ = fl.Close()
	recv(t, "the accept loop to end", done)
}
