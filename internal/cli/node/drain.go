// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
	"github.com/ravindu-rev/ruralz/internal/clock"
	"github.com/ravindu-rev/ruralz/internal/nodedir"
)

// ruralz node drain settings (docs/reference/01-cli-and-api-surface.md,
// "Command table" and "Rollout, promotion and Node verbs").
const (
	// EnvDataDir names the data directory when --data-dir is absent
	// (docs/_meta/foundation-pack.md section 2).
	EnvDataDir = "RURALZ_DATA_DIR"
	// DefaultDrainTimeout bounds the wait for the signaled process,
	// counted from the signal, unless --timeout replaces it (target).
	DefaultDrainTimeout = 5 * time.Minute
	// DrainPoll is the interval at which the drain checks whether the
	// signaled process is gone (target).
	DrainPoll = 100 * time.Millisecond
)

// Results of the --output json object.
const (
	resultDrained = "drained"
	resultTimeout = "timeout"
)

// drainHelp is the paragraph of ruralz node drain --help.
const drainHelp = `Sends SIGTERM to the ruralzd that holds the lock on the data directory, as a
process manager does, once holder.json, the PID namespace, the process start
time and /proc/locks confirm that process, then waits until it exits.`

// DrainSpec returns the ruralz node drain command.
func DrainSpec(o Options) command.Spec {
	return command.Spec{
		Path:     []string{"node", "drain"},
		Summary:  "Drain the local ruralzd",
		Help:     drainHelp,
		Platform: drainPlatform,
		New:      func() command.Command { return &drainCommand{opts: o} },
	}
}

// drainPlatform refuses every platform but Linux
// (docs/reference/01-cli-and-api-surface.md, "Platform support"): the
// checks read /proc/locks, which macOS lacks, and Windows has no ruralzd
// build and no SIGTERM.
func drainPlatform(goos string) error {
	switch goos {
	case "linux":
		return nil
	case "windows":
		return errors.New("there is no ruralzd and no SIGTERM")
	default:
		return errors.New("it needs /proc/locks")
	}
}

// drainCommand is one run of ruralz node drain.
type drainCommand struct {
	opts    Options
	flags   *command.Flags
	dataDir string
	timeout time.Duration
	output  string
}

// Flags binds --data-dir, --timeout and --output.
func (c *drainCommand) Flags(f *command.Flags) {
	c.flags = f
	f.String(&c.dataDir, "data-dir", "PATH", "",
		"Data directory of the local ruralzd (default $"+EnvDataDir+", else "+nodedir.DefaultRoot+")", command.CompleteDir)
	f.Duration(&c.timeout, "timeout", DefaultDrainTimeout, "How long to await the exit after the signal")
	f.Output(&c.output, "text", "text", "json")
}

// Run verifies the lock holder, signals it and awaits its exit.
func (c *drainCommand) Run(ctx context.Context, sio command.IO, _ []string) error {
	root, err := dataDir(c.flags.IsSet("data-dir"), c.dataDir, sio.LookupEnv)
	if err != nil {
		return err
	}
	h, err := readHolder(root)
	if err != nil {
		return err
	}
	hs := c.opts.machine()
	// The handle comes first: the checks then vouch for the process it
	// refers to, and only that process can receive the signal.
	p, err := hs.open(h.PID)
	if err != nil {
		return command.NoResult(err)
	}
	defer func() { _ = p.release() }()
	if err := verifyHolder(hs, h, lockPath(root)); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return fmt.Errorf("drain pid %d: %w", h.PID, context.Cause(ctx))
	}
	clk := c.opts.timer()
	if err := p.terminate(); err != nil {
		return signalFailure(h, err)
	}
	signaled := clk.Now()
	elapsed, gone, err := awaitExit(ctx, clk, hs, h, signaled, c.timeout)
	if err != nil {
		return fmt.Errorf("await the exit of pid %d: %w", h.PID, err)
	}
	result := drainResult{NodeID: h.NodeID.String(), PID: h.PID, Seconds: milliseconds(elapsed), Result: resultDrained}
	if !gone {
		result.Result = resultTimeout
	}
	if c.output == "json" {
		if err := command.WriteJSON(sio.Stdout, result); err != nil {
			return command.NoResult(fmt.Errorf("write stdout: %w", err))
		}
	}
	if !gone {
		return command.NoResultf("timed out after %s (--timeout): Node %s (pid %d) still runs; no second signal was sent, so its Drain goes on to its own deadline",
			command.FormatDuration(c.timeout), h.NodeID, h.PID)
	}
	if c.output == "text" {
		_, _ = fmt.Fprintf(sio.Stderr, "drained Node %s (pid %d) in %s s\n", h.NodeID, h.PID, tenths(elapsed))
	}
	return nil
}

// drainResult is the --output json object of ruralz node drain, printed
// once the holder was signaled.
type drainResult struct {
	// NodeID is the holder's node.id.
	NodeID string `json:"nodeId"`
	// PID is the signaled process.
	PID int `json:"pid"`
	// Seconds counts from the signal to the poll that saw the process
	// gone, or to --timeout, with millisecond precision.
	Seconds json.Number `json:"seconds"`
	// Result is drained, or timeout beside exit 2.
	Result string `json:"result"`
}

// dataDir resolves the data directory: --data-dir when given, which must
// not be empty, else RURALZ_DATA_DIR when set and not empty, as ruralzd
// reads it, else /var/lib/ruralz; a relative path is made absolute.
func dataDir(set bool, flagValue string, lookupEnv func(string) (string, bool)) (string, error) {
	dir := nodedir.DefaultRoot
	switch {
	case set && flagValue == "":
		return "", command.Usagef("--data-dir: want the path of a data directory, not an empty one")
	case set:
		dir = flagValue
	default:
		if v, ok := lookupEnv(EnvDataDir); ok && v != "" {
			dir = v
		}
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir, nil
}

// signalFailure is the exit 2 of a SIGTERM that did not reach the holder:
// the process exited after the checks, which leaves a stale record, or the
// signal was denied, as for a process of another user.
func signalFailure(h nodedir.Holder, err error) error {
	if errors.Is(err, os.ErrProcessDone) {
		return stale("pid %d exited before the signal", h.PID)
	}
	return command.NoResultf("cannot send SIGTERM to pid %d: %w", h.PID, err)
}

// awaitExit polls every DrainPoll after the signal until the holder's
// process is gone or timeout has passed since signaled. It returns the
// time from the signal to the poll that saw the process gone, or to the
// deadline, and whether the process is gone; an error is ctx's end, by
// Ctrl-C or the caller, which leaves the Node draining.
func awaitExit(ctx context.Context, clk clock.Clock, hs host, h nodedir.Holder, signaled time.Time,
	timeout time.Duration,
) (time.Duration, bool, error) {
	deadline := signaled.Add(timeout)
	t := clk.NewTimer(min(DrainPoll, timeout))
	defer t.Stop()
	for {
		var at time.Time
		select {
		case <-ctx.Done():
			return clk.Since(signaled), false, context.Cause(ctx)
		case at = <-t.C():
		}
		if processGone(hs, h) {
			return at.Sub(signaled), true, nil
		}
		left := deadline.Sub(at)
		if left <= 0 {
			return at.Sub(signaled), false, nil
		}
		t.Reset(min(DrainPoll, left))
	}
}

// milliseconds formats d in seconds with three decimals, the JSON number
// of the drain's seconds member.
func milliseconds(d time.Duration) json.Number {
	ms := max(d, 0).Round(time.Millisecond).Milliseconds()
	return json.Number(fmt.Sprintf("%d.%03d", ms/1000, ms%1000))
}

// tenths formats d in seconds with one decimal, as the drained line
// prints it.
func tenths(d time.Duration) string {
	n := max(d, 0).Round(100*time.Millisecond) / (100 * time.Millisecond)
	return fmt.Sprintf("%d.%d", n/10, n%10)
}
