// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package nodedir

// processStartTime returns 0: there is no /proc/<pid>/stat here, and
// ruralz node drain is Linux-only (spec 10 requirement 97).
func processStartTime() (uint64, error) { return 0, nil }

// processPIDNamespace returns "": PID namespaces are Linux-only.
func processPIDNamespace() (string, error) { return "", nil }
