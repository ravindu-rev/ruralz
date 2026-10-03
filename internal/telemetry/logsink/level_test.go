// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// 09 req 64, test plan 6.1 item 4.
func TestParseLevel_Req64(t *testing.T) {
	tests := []struct {
		in      string
		want    slog.Level
		wantErr bool
	}{
		{in: "debug", want: slog.LevelDebug},
		{in: "info", want: slog.LevelInfo},
		{in: "", want: slog.LevelInfo},
		{in: "warn", want: slog.LevelWarn},
		{in: "error", want: slog.LevelError},
		{in: "INFO", wantErr: true},
		{in: "trace", wantErr: true},
		{in: "warning", wantErr: true},
		{in: " info", wantErr: true},
		{in: "Debug", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseLevel(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrLevel) {
					t.Fatalf("ParseLevel(%q) error = %v, want ErrLevel", tt.in, err)
				}
				for _, want := range []string{LevelEnv, "debug", "info", "warn", "error"} {
					if !strings.Contains(err.Error(), want) {
						t.Errorf("error %q does not name %q", err, want)
					}
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseLevel(%q) = %v, %v; want %v", tt.in, got, err, tt.want)
			}
		})
	}
}
