// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package launch

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ravindu-rev/ruralz/internal/cli/command"
)

// binaryName is the file name of ruralzd on the running platform.
func binaryName() string {
	if runtime.GOOS == "windows" {
		return "ruralzd.exe"
	}
	return "ruralzd"
}

// FindRuralzd returns the absolute path of the ruralzd to launch
// (docs/reference/01-cli-and-api-surface.md "Local ruralzd launched by the
// CLI"): flagPath, the value of --ruralzd, when it is not empty, which skips
// the search; otherwise the ruralzd beside the running ruralz, its symbolic
// links resolved, else the first ruralzd on the PATH of the invocation's
// environment, relative PATH entries skipped. A --ruralzd that is no
// executable regular file, or no ruralzd in either place, exits 2, the
// latter naming both places.
func FindRuralzd(sio command.IO, flagPath string, o Options) (string, error) {
	if flagPath != "" {
		abs, err := filepath.Abs(flagPath)
		if err != nil {
			return "", command.NoResultf("--ruralzd %s: %w", flagPath, err)
		}
		if err := checkExecutable(abs); err != nil {
			return "", command.NoResultf("--ruralzd %s: %w", flagPath, err)
		}
		return abs, nil
	}
	beside := ""
	if exe, err := o.executable(); err == nil {
		if resolved, err := filepath.EvalSymlinks(exe); err == nil {
			exe = resolved
		}
		beside = filepath.Dir(exe)
		if p := filepath.Join(beside, binaryName()); checkExecutable(p) == nil {
			return p, nil
		}
	}
	path, _ := lookupEnv(sio, "PATH")
	for _, dir := range filepath.SplitList(path) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		if p := filepath.Join(dir, binaryName()); checkExecutable(p) == nil {
			return p, nil
		}
	}
	where := "beside ruralz"
	if beside != "" {
		where += " in " + beside
	}
	return "", command.NoResultf("no %s %s or on PATH; name it with --ruralzd PATH", binaryName(), where)
}

// errNotExecutable reports a file without an execute permission bit.
var errNotExecutable = errors.New("not executable")

// checkExecutable reports why path is no executable regular file, or nil.
func checkExecutable(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return fs.ErrNotExist
		}
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("not a regular file")
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o111 == 0 {
		return errNotExecutable
	}
	return nil
}

// lookupEnv reads the invocation's environment snapshot.
func lookupEnv(sio command.IO, key string) (string, bool) {
	if sio.LookupEnv == nil {
		return "", false
	}
	return sio.LookupEnv(key)
}
