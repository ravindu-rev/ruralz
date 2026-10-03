// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"context"
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// SignalContext returns a context canceled on the first SIGINT or SIGTERM
// with cause Interrupted{Signal: s} (spec 10 req 21). signal.Notify feeds
// a buffered channel (capacity 1) served by one goroutine owned by the
// returned stop function; a second SIGINT restores the default handling,
// so a third Ctrl-C kills the process. stop unregisters, waits for the
// goroutine and cancels the context; it is idempotent.
func SignalContext(parent context.Context) (ctx context.Context, stop context.CancelFunc) {
	return signalContext(parent, signalHooks{notify: signal.Notify, stop: signal.Stop, reset: signal.Reset})
}

// signalHooks are the os/signal functions, injectable for tests.
type signalHooks struct {
	notify func(c chan<- os.Signal, sig ...os.Signal)
	stop   func(c chan<- os.Signal)
	reset  func(sig ...os.Signal)
}

func signalContext(parent context.Context, h signalHooks) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	ch := make(chan os.Signal, 1)
	h.notify(ch, os.Interrupt, syscall.SIGTERM)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		interrupts := 0
		for {
			select {
			case <-quit:
				return
			case s := <-ch:
				// Only the first cancel sets the cause.
				cancel(Interrupted{Signal: s})
				if s == os.Interrupt {
					interrupts++
					if interrupts == 2 {
						h.reset(os.Interrupt)
					}
				}
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			h.stop(ch)
			close(quit)
			<-done
			cancel(nil)
		})
	}
	return ctx, stop
}
