// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"bufio"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// unitName is the service the live suite installs and drives.
const unitName = "ruralzd.service"

// systemctlBound bounds one systemctl call: above the unit's
// TimeoutStartSec=90, which a start and a reload wait for, and its
// TimeoutStopSec=40.
const systemctlBound = 2 * time.Minute

// Unit properties the live suite reads (org.freedesktop.systemd1.Service).
const (
	propMainPID        = "MainPID"
	propActiveState    = "ActiveState"
	propSubState       = "SubState"
	propResult         = "Result"
	propNRestarts      = "NRestarts"
	propStatusText     = "StatusText"
	propWatchdogUSec   = "WatchdogUSec"
	propWatchdogMono   = "WatchdogTimestampMonotonic"
	propExecMainStatus = "ExecMainStatus"
	propControlGroup   = "ControlGroup"
)

// run runs a command within ctx and systemctlBound and returns its
// combined output, with an error naming the command and its output when
// it fails.
func run(ctx context.Context, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, systemctlBound)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput() //nolint:gosec // G204: system tools with arguments built by the suite
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w\n%s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// systemctl runs systemctl with args.
func systemctl(ctx context.Context, args ...string) (string, error) {
	return run(ctx, "systemctl", args...)
}

// parseShow parses `systemctl show` output, one Key=Value line per
// property; a value keeps any "=" it holds.
func parseShow(out string) map[string]string {
	props := map[string]string{}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		if k, v, ok := strings.Cut(sc.Text(), "="); ok {
			props[k] = v
		}
	}
	return props
}

// show returns the unit's properties names.
func show(ctx context.Context, names ...string) (map[string]string, error) {
	out, err := systemctl(ctx, "show", unitName, "--property="+strings.Join(names, ","))
	if err != nil {
		return nil, err
	}
	props := parseShow(out)
	for _, n := range names {
		if _, ok := props[n]; !ok {
			return nil, fmt.Errorf("systemctl show %s printed no %s:\n%s", unitName, n, out)
		}
	}
	return props, nil
}

// showInt returns the unit's integer property name.
func showInt(ctx context.Context, name string) (int64, error) {
	props, err := show(ctx, name)
	if err != nil {
		return 0, err
	}
	v, err := strconv.ParseInt(props[name], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: %w", name, props[name], err)
	}
	return v, nil
}

// mainProc returns the unit's main process, identified by its start time.
func mainProc(ctx context.Context) (proc, error) {
	props, err := show(ctx, propMainPID)
	if err != nil {
		return proc{}, err
	}
	pid, err := strconv.Atoi(props[propMainPID])
	if err != nil || pid <= 0 {
		return proc{}, fmt.Errorf("%s has no main process (%s=%q)", unitName, propMainPID, props[propMainPID])
	}
	p, ok := lookupProc(pid)
	if !ok {
		return proc{}, fmt.Errorf("the main process %d of %s does not run", pid, unitName)
	}
	return p, nil
}

// Evidence reads at most journalScan lines of the journal, drops the
// access records, which a run under load writes by the thousand, and keeps
// the last journalLines.
const (
	journalScan  = 50000
	journalLines = 400
)

// journal returns the unit's journal since the Unix time since, in the
// short-precise format, without access records: at most the last
// journalLines lines.
func journal(ctx context.Context, since time.Time) string {
	out, err := run(ctx, "journalctl", "--no-pager", "--unit="+unitName, "--output=short-precise",
		"--since=@"+strconv.FormatInt(since.Unix(), 10), "--lines="+strconv.Itoa(journalScan))
	if err != nil {
		return err.Error()
	}
	var kept []string
	for line := range strings.SplitSeq(out, "\n") {
		if !strings.Contains(line, `"msg":"access"`) {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept[max(len(kept)-journalLines, 0):], "\n")
}

// journalText returns the unit's log lines since the Unix time since,
// message text only.
func journalText(ctx context.Context, since time.Time) (string, error) {
	return run(ctx, "journalctl", "--no-pager", "--unit="+unitName, "--output=cat",
		"--since=@"+strconv.FormatInt(since.Unix(), 10))
}
