// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package proc

import (
	"os"
	"regexp"
	"runtime"
	"syscall"
	"testing"
	"time"
)

// TestStartSurvivesLockedThreadExit starts a child from a goroutine that
// exits while locked to its OS thread, so the runtime ends that thread.
// Pdeathsig follows the forking thread, so a child forked on it would get
// SIGKILL; Start forks from the Process's own goroutine instead.
func TestStartSurvivesLockedThreadExit(t *testing.T) { // review finding: Pdeathsig follows the forking thread
	type started struct {
		p   *Process
		err error
	}
	var attempt func(ch chan<- started)
	attempt = func(ch chan<- started) {
		runtime.LockOSThread()
		if syscall.Gettid() == os.Getpid() {
			// The main thread never ends. Hold it, so the next attempt
			// must run on another thread, then give it back.
			inner := make(chan started, 1)
			go attempt(inner)
			ch <- <-inner
			runtime.UnlockOSThread()
			return
		}
		p, err := Start(ctx(t), helper("sleep"))
		ch <- started{p, err}
		// Returns locked: the runtime ends this thread.
	}
	ch := make(chan started, 1)
	go attempt(ch)
	st := <-ch
	if st.err != nil {
		t.Fatal(st.err)
	}
	p := st.p
	if _, err := p.WaitOutput(ctx(t), regexp.MustCompile("ready")); err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-p.Done():
		sig, _ := p.Signaled()
		t.Fatalf("child died (%v) when the starting goroutine's thread ended", sig)
	case <-timer.C:
	}
	if err := p.Kill(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Wait(ctx(t)); err != nil {
		t.Fatal(err)
	}
}
