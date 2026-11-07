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

// Kinds of legal texts a module ships.
const (
	// kindLicense is a license text: LICENSE, COPYING, UNLICENSE or the
	// British spelling of LICENSE.
	kindLicense = "license"
	// kindPatents is a patent grant (PATENTS), reproduced with the license
	// but never classified.
	kindPatents = "patents"
	// kindNotice is an attribution notice (NOTICE), reproduced in the
	// artifact's NOTICE as Apache-2.0 section 4(d) requires.
	kindNotice = "notice"
)

// legalText is one license, patent grant or notice of a module.
type legalText struct {
	// Rel is the slash-separated path relative to the module root, such
	// as "LICENSE" or "third_party/x/LICENSE".
	Rel string
	// Kind is kindLicense, kindPatents or kindNotice.
	Kind string
	// Text is the file content with LF line endings and a final newline.
	Text string
}

// legalKind classifies a file name, or returns "" for any other file.
func legalKind(name string) string {
	upper := strings.ToUpper(name)
	for _, prefix := range []string{"LICENSE", "LICENCE", "COPYING", "UNLICENSE"} { //nolint:misspell // LICENCE is a real file name spelling.
		if strings.HasPrefix(upper, prefix) {
			return kindLicense
		}
	}
	switch {
	case strings.HasPrefix(upper, "PATENTS"):
		return kindPatents
	case strings.HasPrefix(upper, "NOTICE"):
		return kindNotice
	}
	return ""
}

// dirReader returns the legal texts directly in dir, sorted by name, with
// Rel set to the file name; tests replace it.
type dirReader func(dir string) ([]legalText, error)

// readLegal reads the license, patent and notice files of one directory.
func readLegal(dir string) ([]legalText, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []legalText
	for _, e := range entries {
		kind := legalKind(e.Name())
		if kind == "" || !e.Type().IsRegular() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) //nolint:gosec // Reads legal texts inside the Go module cache or GOROOT.
		if err != nil {
			return nil, err
		}
		out = append(out, legalText{Rel: e.Name(), Kind: kind, Text: normalizeText(string(data))})
	}
	slices.SortFunc(out, func(a, b legalText) int { return strings.Compare(a.Rel, b.Rel) })
	return out, nil
}

// normalizeText drops a byte order mark, turns CRLF into LF, trims
// trailing blank lines and ends the text with one newline, so a notice is
// byte-identical whichever OS checked the module out.
func normalizeText(s string) string {
	s = strings.TrimPrefix(s, string(rune(0xfeff)))
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimRight(s, " \t\n")
	return s + "\n"
}

// mplSecondaryLicense is MPL-2.0's definition of a Secondary License
// (section 1.12), the one place its text names GNU licenses, lower-cased
// and with its spaces collapsed as classify reads it.
const mplSecondaryLicense = "means either the gnu general public license, version 2.0, the gnu lesser general public license, " +
	"version 2.1, the gnu affero general public license, version 3.0, or any later versions of those licenses"

// classify names the license of a license text by distinctive phrases, or
// returns "unknown". Denied licenses are checked first, so a permissive
// phrase inside a copyleft text never passes it. An MPL-2.0 text names
// the GNU licenses only in its Secondary License definition, so that
// clause is removed before the checks: every MPL-2.0 text would read as
// AGPL otherwise, while a GNU license text beside it still fails.
func classify(text string) string {
	t := strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if strings.Contains(t, "mozilla public license") {
		t = strings.ReplaceAll(t, mplSecondaryLicense, "")
	}
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
