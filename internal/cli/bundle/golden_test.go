// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package bundle

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// update rewrites the golden files: go test ./internal/cli/bundle -update.
var update = flag.Bool("update", false, "rewrite testdata golden files")

// golden compares got with testdata/<name>, a slash path, rewriting the
// file with -update. Golden files hold LF line endings on every OS.
func golden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", filepath.FromSlash(name))
	if *update {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", name, err)
	}
	if got != string(want) {
		t.Errorf("%s mismatch (run with -update to accept)\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
	}
}

// scrub returns s with every spelling of the directory dir, as the OS
// writes it and in slash form, replaced by $DIR, and the path separators
// after it in slash form, so an output naming files under a test's
// temporary directory compares equal on every OS.
func scrub(s, dir string) string {
	for _, d := range []string{dir, filepath.ToSlash(dir)} {
		for {
			i := strings.Index(s, d)
			if i < 0 {
				break
			}
			rest := s[i+len(d):]
			end := strings.IndexAny(rest, " \t\n\":)'")
			if end < 0 {
				end = len(rest)
			}
			s = s[:i] + "$DIR" + filepath.ToSlash(rest[:end]) + rest[end:]
		}
	}
	return s
}
