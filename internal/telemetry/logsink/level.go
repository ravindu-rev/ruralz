// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"errors"
	"fmt"
	"log/slog"
)

// LevelEnv is the environment variable that fixes the process log level
// (foundation pack section 2 "Env vars"; 09 req 64).
const LevelEnv = "RURALZ_LOG_LEVEL"

// ErrLevel is wrapped by ParseLevel for a value outside debug, info, warn
// and error. ruralzd refuses to start with exit code 2 on it.
var ErrLevel = errors.New("allowed values are debug, info, warn and error")

// ParseLevel maps a RURALZ_LOG_LEVEL value to a level (09 req 64): debug,
// info, warn or error, spelled in lower case; the empty string (unset)
// means info. The level is fixed for the process lifetime and never
// depends on a Revision (OQ-observability-6 (a)).
func ParseLevel(s string) (slog.Level, error) {
	switch s {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("%s=%q: %w", LevelEnv, s, ErrLevel)
	}
}
