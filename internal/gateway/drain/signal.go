// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package drain

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// signalHooks are the os/signal functions, injectable for tests.
type signalHooks struct {
	notify func(c chan<- os.Signal, sig ...os.Signal)
	stop   func(c chan<- os.Signal)
	ignore func(sig ...os.Signal)
}

// Notify forwards every SIGTERM and SIGINT the process receives to
// d.Signal, and makes the process ignore SIGPIPE, so a write to a closed
// stdout or stderr fails and drops log lines instead of ending the
// process. SIGHUP is left to the source, which loads it at once and never
// starts a Drain, and SIGQUIT keeps the Go runtime's stack dump and exit.
// stop stops forwarding and waits for the forwarding goroutine; SIGPIPE
// stays ignored. stop is idempotent.
func Notify(d *Drainer) (stop func()) {
	return notify(d, signalHooks{notify: signal.Notify, stop: signal.Stop, ignore: signal.Ignore})
}

func notify(d *Drainer, h signalHooks) func() {
	h.ignore(syscall.SIGPIPE)
	// Two signals matter, the one that starts the Drain and the one that
	// jumps to its deadline; os/signal drops a signal the channel cannot
	// take, as later ones do nothing.
	ch := make(chan os.Signal, 2)
	h.notify(ch, syscall.SIGTERM, os.Interrupt)
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if v := recover(); v != nil {
				d.log.Error("drain signal forwarding panicked", catalog.KeyError, fmt.Sprint(v))
			}
		}()
		for {
			select {
			case <-quit:
				return
			case <-ch:
				d.Signal()
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
