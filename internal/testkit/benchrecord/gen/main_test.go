// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/benchrecord"
)

// TestRunWritesTheEmbeddedSchema checks that gen writes exactly the schema
// benchrecord embeds, and exits 1 without -out or with extra arguments.
func TestRunWritesTheEmbeddedSchema(t *testing.T) {
	out := filepath.Join(t.TempDir(), "record.schema.json")
	var stderr bytes.Buffer
	if code := run([]string{"-out", out}, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr %q", code, stderr.String())
	}
	got, err := os.ReadFile(out) //nolint:gosec // a file under t.TempDir
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, benchrecord.Schema()) {
		t.Fatal("written schema differs from the embedded one")
	}
	for name, args := range map[string][]string{
		"no -out":         nil,
		"extra arguments": {"-out", out, "x"},
		"unknown flag":    {"-x"},
		"missing dir":     {"-out", filepath.Join(t.TempDir(), "no", "dir", "s.json")},
	} {
		stderr.Reset()
		if code := run(args, &stderr); code != 1 {
			t.Errorf("%s: run = %d, want 1", name, code)
		}
		if !strings.Contains(stderr.String(), "gen") && name != "unknown flag" {
			t.Errorf("%s: stderr %q does not name the tool", name, stderr.String())
		}
	}
}
