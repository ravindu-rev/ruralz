// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
)

// Limits of the gate (docs/architecture/12-performance-budgets-and-benchmarking.md,
// "Memory budget"; 11 req 68).
const (
	defaultMaxSize = 160 << 20 // 167,772,160 bytes: stripped ruralzd
	defaultMaxRSS  = 89 << 20  // 93,323,264 bytes: idle VmRSS
)

// sizeResult is the stripped size check of one binary.
type sizeResult struct {
	path     string
	platform string // "linux/amd64", or "" when unknown
	size     int64
	limit    int64
}

func (r sizeResult) failed() bool { return r.size > r.limit }

// platformOf returns the GOOS/GOARCH recorded in a Go binary's build
// information, which stripping (-s -w) keeps.
func platformOf(path string) (string, error) {
	bi, err := buildinfo.ReadFile(path)
	if err != nil {
		return "", err
	}
	var goos, goarch string
	for _, s := range bi.Settings {
		switch s.Key {
		case "GOOS":
			goos = s.Value
		case "GOARCH":
			goarch = s.Value
		}
	}
	if goos == "" || goarch == "" {
		return "", errors.New("build information records no GOOS or GOARCH")
	}
	return goos + "/" + goarch, nil
}

// checkSize measures one binary against limit: equal passes, one byte more
// fails (11 test plan item 2).
func checkSize(path string, limit int64) (sizeResult, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return sizeResult{}, err
	}
	if !fi.Mode().IsRegular() {
		return sizeResult{}, fmt.Errorf("%s is not a regular file", path)
	}
	platform, _ := platformOf(path)
	return sizeResult{path: path, platform: platform, size: fi.Size(), limit: limit}, nil
}

// mib formats bytes as MiB with one decimal.
func mib(n int64) string {
	return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
}
