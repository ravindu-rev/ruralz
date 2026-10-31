// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration && !unix

package config

import (
	"errors"
	"fmt"
	"net"
	"os"
)

// clockSockets reports that a controllable-clock ruralzd cannot be started
// here: a process inherits extra descriptors on Unix only.
const clockSockets = false

// commandSocket fails: see clockSockets.
func commandSocket() (net.Conn, *os.File, error) {
	return nil, nil, fmt.Errorf("clock socket: %w", errors.ErrUnsupported)
}
