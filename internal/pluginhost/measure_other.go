// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package pluginhost

// measureCode reports no measurement: only Linux exposes the executable
// mappings, so elsewhere the accounted size alone bounds compiled code.
func measureCode() (int64, bool) { return 0, false }
