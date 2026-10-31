// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package cli

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"syscall"
	"testing"
	"time"
)

// TestRunExitsBySignal sends SIGINT, then SIGTERM, to the test process
// while Run streams ruralz dev tap: the command ends quietly with 130
// after SIGINT and 143 after SIGTERM, 128 plus the signal's number.
func TestRunExitsBySignal(t *testing.T) {
	for _, c := range []struct {
		signal syscall.Signal
		want   int
	}{{syscall.SIGINT, ExitInterrupted}, {syscall.SIGTERM, ExitTerminated}} {
		t.Run(c.signal.String(), func(t *testing.T) {
			subscribed := make(chan struct{}, 1)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/x-ndjson")
				w.WriteHeader(http.StatusOK)
				w.(http.Flusher).Flush()
				subscribed <- struct{}{}
				<-r.Context().Done()
			}))
			defer srv.Close()
			var out, errOut bytes.Buffer
			done := make(chan int, 1)
			go func() {
				done <- Run(context.Background(), []string{"dev", "tap", "--admin", srv.URL, "--admin-token-file="}, &out, &errOut)
			}()
			select {
			case <-subscribed:
			case <-time.After(30 * time.Second):
				t.Fatal("dev tap did not subscribe")
			}
			if err := syscall.Kill(os.Getpid(), c.signal); err != nil {
				t.Fatal(err)
			}
			select {
			case code := <-done:
				if code != c.want || out.Len() != 0 || errOut.Len() != 0 {
					t.Errorf("Run = %d, stdout %q, stderr %q; want %d and nothing", code, out.String(), errOut.String(), c.want)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("dev tap did not end")
			}
		})
	}
}
