// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package render

import "testing"

// TestParseFormat checks the --output names: yaml, json and text, matched
// exactly, round-tripping through String.
func TestParseFormat(t *testing.T) {
	for _, f := range []Format{FormatYAML, FormatJSON, FormatText} {
		if got, ok := ParseFormat(f.String()); !ok || got != f {
			t.Errorf("ParseFormat(%q) = %v, %v; want %v", f.String(), got, ok, f)
		}
	}
	for _, s := range []string{"", "YAML", "Json", "csv", "unknown", "table"} {
		if f, ok := ParseFormat(s); ok {
			t.Errorf("ParseFormat(%q) = %v, want no format", s, f)
		}
	}
	if Format(0).String() != "unknown" {
		t.Errorf("Format(0) = %q", Format(0).String())
	}
}
