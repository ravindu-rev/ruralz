// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proc

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
)

// PeerSuffix names the other end of the veth pair AddDevice creates on
// Linux.
const PeerSuffix = "-peer"

// AddDevice needs Linux network devices.
func AddDevice(string, netip.Prefix) error {
	return fmt.Errorf("proc: network devices: %w", errors.ErrUnsupported)
}

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
