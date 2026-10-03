// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package command

import (
	"context"
	"errors"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestSignalContextRealSIGINTReq21 sends SIGINT to the test process: the
// context is canceled with the typed cause and stop unregisters.
func TestSignalContextRealSIGINTReq21(t *testing.T) {
	ctx, stop := SignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("SIGINT did not cancel the context")
	}
	in, ok := errors.AsType[Interrupted](context.Cause(ctx))
	if !ok || in.Signal != syscall.SIGINT {
		t.Fatalf("cause = %v, want Interrupted{SIGINT}", context.Cause(ctx))
	}
	stop()
}
