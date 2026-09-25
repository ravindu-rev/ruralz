// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code = Run(t.Context(), args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestVersionText(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	for _, key := range []string{"version:", "commit:", "flavor:", "apiVersions: ruralz/v1alpha1"} {
		if !strings.Contains(out, key) {
			t.Errorf("text output lacks %q:\n%s", key, out)
		}
	}
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, "version", "--output", "json")
	if code != ExitOK {
		t.Fatalf("exit code = %d, want %d", code, ExitOK)
	}
	var got struct {
		Version     string   `json:"version"`
		APIVersions []string `json:"apiVersions"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if got.Version == "" || len(got.APIVersions) == 0 {
		t.Fatalf("unexpected JSON: %s", out)
	}
}

func TestUsageErrors(t *testing.T) {
	cases := [][]string{
		nil,
		{"bundle", "validate"},
		{"version", "--output", "yaml"},
		{"version", "extra"},
		{"version", "--no-such-flag"},
	}
	for _, args := range cases {
		code, out, errOut := run(t, args...)
		if code != ExitNoResult {
			t.Errorf("Run(%q) = %d, want %d", args, code, ExitNoResult)
		}
		if out != "" {
			t.Errorf("Run(%q) wrote to stdout: %q", args, out)
		}
		if errOut == "" {
			t.Errorf("Run(%q) wrote nothing to stderr", args)
		}
	}
}

func TestHelp(t *testing.T) {
	for _, arg := range []string{"help", "--help", "-h"} {
		code, out, _ := run(t, arg)
		if code != ExitOK || !strings.Contains(out, "Usage: ruralz") {
			t.Errorf("Run(%q) = %d, %q", arg, code, out)
		}
	}
	if code, _, _ := run(t, "version", "--help"); code != ExitOK {
		t.Errorf("version --help exit code = %d, want %d", code, ExitOK)
	}
}
