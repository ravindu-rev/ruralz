// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build t5 && linux

package t5

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The helper checks run ruralzd-handover with this test binary as BINARY:
// re-executed with envStandIn set, it is a stand-in for the new ruralzd
// process, configured by the environment the helper passes on.
const (
	// envStandIn selects the stand-in's mode.
	envStandIn = "RURALZ_T5_STANDIN"
	// envStandInLog names a file the stand-in appends its records to.
	envStandInLog = "RURALZ_T5_STANDIN_LOG"
	// envStandInReady names a file the stand-in creates once its signal
	// disposition is set.
	envStandInReady = "RURALZ_T5_STANDIN_READY"
	// envStandInHolder is the holder.json content modeHolder writes,
	// with %d replaced by its process ID.
	envStandInHolder = "RURALZ_T5_STANDIN_HOLDER"
	// envStandInExit is the exit status of modeExit.
	envStandInExit = "RURALZ_T5_STANDIN_EXIT"
	// envDataDir is the data directory holder.json lives in, as the
	// helper reads it.
	envDataDir = "RURALZ_DATA_DIR"
	// envConfig is a setting of the unit's environment the stand-in
	// records, so a check sees the helper pass that environment on.
	envConfig = "RURALZ_CONFIG"
)

// Stand-in modes.
const (
	// modeHolder writes holder.json naming itself, as a successor that
	// took the lock, then runs until SIGTERM, which it records.
	modeHolder = "holder"
	// modeExit exits at once with the status envStandInExit names, as a
	// refused or failed successor.
	modeExit = "exit"
	// modeIgnoreTerm never takes the lock and ignores SIGTERM, so only
	// SIGKILL ends it.
	modeIgnoreTerm = "ignore-term"
)

// Stand-in records, one line each in the envStandInLog file.
const (
	recordStart = "start"
	recordTerm  = "sigterm"
)

// TestMain runs the stand-in when the helper re-executes this binary,
// else the tests, then removes what they built.
func TestMain(m *testing.M) {
	if mode := os.Getenv(envStandIn); mode != "" {
		os.Exit(standIn(mode))
	}
	code := m.Run()
	builds.cleanup()
	os.Exit(code)
}

// standIn runs the stand-in in mode and returns its exit status.
func standIn(mode string) int {
	record(fmt.Sprintf("%s pid=%d stdin-null=%t config=%s", recordStart, os.Getpid(), stdinIsNull(), os.Getenv(envConfig)))
	switch mode {
	case modeExit:
		code, err := strconv.Atoi(os.Getenv(envStandInExit))
		if err != nil {
			return 125
		}
		return code
	case modeHolder:
		if err := writeHolder(); err != nil {
			record("error " + err.Error())
			return 125
		}
		return untilTerm()
	case modeIgnoreTerm:
		signal.Ignore(syscall.SIGTERM)
		ready()
		for {
			time.Sleep(time.Hour)
		}
	default:
		record("error unknown mode " + mode)
		return 125
	}
}

// untilTerm waits for SIGTERM, records it and returns 0.
func untilTerm() int {
	c := make(chan os.Signal, 1)
	signal.Notify(c, syscall.SIGTERM)
	ready()
	<-c
	record(recordTerm)
	return 0
}

// ready creates the envStandInReady file, when one is named.
func ready() {
	if p := os.Getenv(envStandInReady); p != "" {
		_ = os.WriteFile(p, nil, 0o600) //nolint:gosec // G703: the ready file the test names
	}
}

// record appends line to the envStandInLog file, when one is named.
func record(line string) {
	p := os.Getenv(envStandInLog)
	if p == "" {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // G304: a file the test names
	if err != nil {
		return
	}
	_, _ = fmt.Fprintln(f, line)
	_ = f.Close()
}

// stdinIsNull reports whether standard input is /dev/null, as the helper
// starts BINARY with it.
func stdinIsNull() bool {
	in, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	null, err := os.Stat(os.DevNull)
	return err == nil && os.SameFile(in, null)
}

// writeHolder replaces holder.json in the data directory, atomically, with
// the envStandInHolder content naming this process.
func writeHolder() error {
	dir := os.Getenv(envDataDir)
	if dir == "" {
		return fmt.Errorf("%s is not set", envDataDir)
	}
	content := strings.ReplaceAll(os.Getenv(envStandInHolder), "%d", strconv.Itoa(os.Getpid()))
	tmp := filepath.Join(dir, ".holder.json.standin")
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil { //nolint:gosec // G703: a fixed name under the data directory the test names
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, "holder.json")) //nolint:gosec // G703: fixed names under the data directory the test names
}
