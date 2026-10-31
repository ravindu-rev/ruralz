// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package drain

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
)

// TestNotifyRealSIGTERM sends SIGTERM to the test process, as ruralz node
// drain does to ruralzd: the Drain starts, and SIGPIPE is ignored.
func TestNotifyRealSIGTERM(t *testing.T) {
	r := newRig(t)
	r.run(t.Context())
	stop := Notify(r.d)
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	recv(t, "the Drain to start", r.d.Started())
	if !signal.Ignored(syscall.SIGPIPE) {
		t.Error("SIGPIPE is not ignored")
	}
	r.waitArmed(t)
	r.d.Signal()
	r.finished(t)
}
