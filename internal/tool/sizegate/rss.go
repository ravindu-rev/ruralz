// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ravindu-rev/ruralz/internal/clock"
)

// Idle measurement defaults (11 req 68): 120 s settle, sampled every
// second over its last 10 s.
const (
	defaultSettle   = 120 * time.Second
	defaultWindow   = 10 * time.Second
	defaultInterval = time.Second
	stopGrace       = 5 * time.Second
	outputTail      = 64 << 10
)

// errExited reports that the measured process ended during the settle.
var errExited = errors.New("the process exited before the idle settle ended")

// readVMRSS returns the resident set size of pid in bytes, from the VmRSS
// line of <procRoot>/<pid>/status ("VmRSS:    12345 kB").
func readVMRSS(procRoot string, pid int) (int64, error) {
	path := filepath.Join(procRoot, strconv.Itoa(pid), "status")
	data, err := os.ReadFile(path) //nolint:gosec // The /proc root is a command-line argument.
	if err != nil {
		return 0, err
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 || fields[0] != "VmRSS:" {
			continue
		}
		if len(fields) != 3 || fields[2] != "kB" {
			return 0, fmt.Errorf("%s: malformed VmRSS line %q", path, sc.Text())
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || kb < 0 {
			return 0, fmt.Errorf("%s: malformed VmRSS value %q", path, fields[1])
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, err
	}
	return 0, fmt.Errorf("%s: no VmRSS line", path)
}

// sampleIdle waits settle-window, then calls read every interval until the
// settle ends (window/interval samples), and returns the samples. It stops
// early with errExited when done closes, or with ctx's error.
func sampleIdle(ctx context.Context, clk clock.Clock, done <-chan struct{}, read func() (int64, error), settle, window, interval time.Duration) ([]int64, error) {
	if interval <= 0 || window < interval || settle < window {
		return nil, fmt.Errorf("want 0 < interval <= window <= settle, got interval %v, window %v, settle %v", interval, window, settle)
	}
	wait := func(d time.Duration) error {
		t := clk.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-done:
			return errExited
		case <-t.C():
			return nil
		}
	}
	if err := wait(settle - window); err != nil {
		return nil, err
	}
	n := int(window / interval)
	samples := make([]int64, 0, n)
	for range n {
		if err := wait(interval); err != nil {
			return samples, err
		}
		v, err := read()
		if err != nil {
			select {
			case <-done:
				return samples, errExited
			default:
				return samples, err
			}
		}
		samples = append(samples, v)
	}
	return samples, nil
}

// tailBuffer keeps the last max bytes written to it; safe for concurrent use.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

// idleOptions configure one idle RSS measurement.
type idleOptions struct {
	binary   string
	env      []string // extra KEY=VALUE settings
	procRoot string
	settle   time.Duration
	window   time.Duration
	interval time.Duration
	limit    int64
}

// rssResult is the idle RSS check of one binary.
type rssResult struct {
	path    string
	samples []int64
	max     int64
	settle  time.Duration
	limit   int64
	skipped string // reason the measurement was skipped, if it was
}

func (r rssResult) failed() bool { return r.skipped == "" && r.max > r.limit }

// stubMessage is the exact message of the M0 ruralzd stub
// (internal/gateway.Run before WP-76 wires the data plane).
const stubMessage = "Ruralz Gateway is not implemented yet; it is Planned (M1)"

// notImplemented reports the M0 ruralzd stub, which exits 2 at once with
// stubMessage; the gate skips it until the data plane lands instead of
// failing every pull request. Only that exact message and exit status
// match, so an unrelated early exit of the real data plane is a tool error,
// never a silent skip. This branch is removed when WP-76 replaces the stub
// (contract note to the lead).
func notImplemented(exitCode int, output string) bool {
	return exitCode == 2 && strings.Contains(output, stubMessage)
}

// measureIdle starts the binary with telemetry defaults, no Revision
// (RURALZ_CONFIG naming an empty directory) and no connections, samples its
// VmRSS over the end of the settle, and stops it (11 req 68).
func measureIdle(ctx context.Context, clk clock.Clock, o idleOptions) (res rssResult, err error) {
	res = rssResult{path: o.binary, settle: o.settle, limit: o.limit}
	tmp, err := os.MkdirTemp("", "sizegate-")
	if err != nil {
		return res, err
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil && err == nil {
			err = rmErr
		}
	}()
	for _, d := range []string{"config", "data", "home"} {
		if err := os.Mkdir(filepath.Join(tmp, d), 0o700); err != nil {
			return res, err
		}
	}
	cmd := exec.CommandContext(ctx, o.binary) //nolint:gosec // The measured binary is a command-line argument.
	cmd.Dir = tmp
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(tmp, "home"),
		"RURALZ_CONFIG=" + filepath.Join(tmp, "config"),
		"RURALZ_DATA_DIR=" + filepath.Join(tmp, "data"),
	}, o.env...)
	out := &tailBuffer{max: outputTail}
	cmd.Stdout, cmd.Stderr = out, out
	cmd.WaitDelay = stopGrace
	if err := cmd.Start(); err != nil {
		return res, err
	}
	done := make(chan struct{})
	var waitErr error
	go func() { // owned by measureIdle, which always waits for done below
		waitErr = cmd.Wait()
		close(done)
	}()
	defer stop(clk, cmd, done)

	samples, err := sampleIdle(ctx, clk, done, func() (int64, error) {
		return readVMRSS(o.procRoot, cmd.Process.Pid)
	}, o.settle, o.window, o.interval)
	if errors.Is(err, errExited) {
		<-done
		code := cmd.ProcessState.ExitCode()
		if notImplemented(code, out.String()) {
			res.skipped = fmt.Sprintf("%s is the M0 stub (%q, exit status %d)", o.binary, stubMessage, code)
			return res, nil
		}
		status := "exit status 0"
		if waitErr != nil {
			status = waitErr.Error()
		}
		return res, fmt.Errorf("%s: %w (%s):\n%s", o.binary, errExited, status, out.String())
	}
	if err != nil {
		return res, fmt.Errorf("%s: %w", o.binary, err)
	}
	res.samples = samples
	for _, s := range samples {
		res.max = max(res.max, s)
	}
	return res, nil
}

// stop ends the measured process: SIGTERM (a Drain), then SIGKILL after the
// grace period, and waits until its waiter goroutine has returned.
func stop(clk clock.Clock, cmd *exec.Cmd, done <-chan struct{}) {
	select {
	case <-done:
		return
	default:
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	t := clk.NewTimer(stopGrace)
	defer t.Stop()
	select {
	case <-done:
		return
	case <-t.C():
	}
	_ = cmd.Process.Kill()
	<-done
}
