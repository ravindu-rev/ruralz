// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package control

import (
	"bytes"
	"strings"
	"testing"
)

func TestRunReportsNotImplemented(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := Run(t.Context(), nil, &out, &errOut); code != ExitNotImplemented {
		t.Fatalf("Run = %d, want %d", code, ExitNotImplemented)
	}
	if out.Len() != 0 {
		t.Errorf("Run wrote to stdout: %q", out.String())
	}
	if !strings.Contains(errOut.String(), "Planned (M2)") {
		t.Errorf("stderr = %q, want the milestone", errOut.String())
	}
}
