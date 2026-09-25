// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// licenseFileName reports whether a file name carries a module's license.
func licenseFileName(name string) bool {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "UNLICENSE"} { //nolint:misspell // LICENCE is a real file name spelling.
		if strings.HasPrefix(upper, prefix) {
			return true
		}
	}
	return false
}

// moduleLicenses classifies every license file at the root of a module.
func moduleLicenses(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !licenseFileName(e.Name()) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // Reads license files inside the Go module cache.
		if err != nil {
			return nil, err
		}
		id := classify(string(data))
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// classify names the license of a license text by distinctive phrases, or
// returns "unknown". Denied licenses are checked first, so a permissive
// phrase inside a copyleft text never passes it.
func classify(text string) string {
	t := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	has := func(phrases ...string) bool {
		for _, p := range phrases {
			if !strings.Contains(t, p) {
				return false
			}
		}
		return true
	}
	switch {
	case has("gnu affero general public license"):
		return "AGPL"
	case has("gnu lesser general public license"), has("gnu library general public license"):
		return "LGPL"
	case has("gnu general public license"):
		return "GPL"
	case has("server side public license"):
		return "SSPL"
	case has("business source license"):
		return "BUSL"
	case has("redis source available license"):
		return "RSAL"
	case has("eclipse public license"):
		return "EPL"
	case has("eclipse distribution license"):
		return "EDL-1.0"
	case has("mozilla public license", "2.0"):
		return "MPL-2.0"
	case has("apache license", "version 2.0"):
		return "Apache-2.0"
	case has("permission is hereby granted, free of charge"):
		return "MIT"
	case has("permission to use, copy, modify, and/or distribute this software for any purpose"),
		has("permission to use, copy, modify, and distribute this software for any purpose"):
		return "ISC"
	case has("redistribution and use in source and binary forms"):
		if regexp.MustCompile(`neither the name|names of (its|the) contributors|name of the copyright holder`).MatchString(t) {
			return "BSD-3-Clause"
		}
		return "BSD-2-Clause"
	default:
		return "unknown"
	}
}
