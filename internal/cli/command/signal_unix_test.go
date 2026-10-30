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

// TestSignalContextRealSIGINT sends SIGINT to the test process: the
// context is canceled with the typed cause and stop unregisters.
func TestSignalContextRealSIGINT(t *testing.T) {
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
	if code := Code(ctx, ctx.Err()); code != ExitInterrupted {
		t.Errorf("Code = %d, want 130", code)
	}
	stop()
}

// TestSignalContextRealSIGTERM sends SIGTERM to the test process, as a
// process manager or kill does: the context is canceled with the typed
// cause and the invocation exits 143, 128 plus the signal's number.
func TestSignalContextRealSIGTERM(t *testing.T) {
	ctx, stop := SignalContext(context.Background())
	defer stop()
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("SIGTERM did not cancel the context")
	}
	in, ok := errors.AsType[Interrupted](context.Cause(ctx))
	if !ok || in.Signal != syscall.SIGTERM {
		t.Fatalf("cause = %v, want Interrupted{SIGTERM}", context.Cause(ctx))
	}
	if code := Code(ctx, ctx.Err()); code != ExitTerminated {
		t.Errorf("Code = %d, want 143", code)
	}
	stop()
}
