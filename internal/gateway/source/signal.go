// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package source

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

// Reloader loads the source at once (Watcher.Reload).
type Reloader interface {
	Reload()
}

// signalHooks are the os/signal functions, injectable for tests.
type signalHooks struct {
	notify func(c chan<- os.Signal, sig ...os.Signal)
	stop   func(c chan<- os.Signal)
}

// NotifySIGHUP registers SIGHUP and forwards every one the process
// receives to r.Reload, so a SIGHUP loads the source at once, skipping
// the settle, and never starts a Drain: until it is registered, Go's
// default action ends the process on SIGHUP. The wiring calls it at
// start. stop unregisters and waits for the forwarding goroutine; it is
// idempotent. After stop, SIGHUP is no longer forwarded but stays
// caught only while another registration holds it.
func NotifySIGHUP(r Reloader) (stop func()) {
	return notifySIGHUP(r, signalHooks{notify: signal.Notify, stop: signal.Stop})
}

func notifySIGHUP(r Reloader, h signalHooks) func() {
	// One pending signal is enough: a load already due covers the next.
	ch := make(chan os.Signal, 1)
	h.notify(ch, syscall.SIGHUP)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-quit:
				return
			case <-ch:
				r.Reload()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			h.stop(ch)
			close(quit)
			<-done
		})
	}
}
