// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proc

import (
	"context"
	"errors"
	"fmt"
)

// RunInNetNS needs Linux network namespaces.
func RunInNetNS(context.Context, NetNSOptions) (int, error) {
	return -1, fmt.Errorf("proc: network namespaces: %w", errors.ErrUnsupported)
}

// SetupNetNS needs Linux.
func SetupNetNS(map[string]string) error {
	return fmt.Errorf("proc: network namespaces: %w", errors.ErrUnsupported)
}

// WriteSysctl needs Linux.
func WriteSysctl(string, string) error {
	return fmt.Errorf("proc: sysctl: %w", errors.ErrUnsupported)
}

// ReadSysctl needs Linux.
func ReadSysctl(string) (string, error) {
	return "", fmt.Errorf("proc: sysctl: %w", errors.ErrUnsupported)
}
