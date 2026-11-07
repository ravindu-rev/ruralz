// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package proc

import (
	"errors"
	"fmt"
)

// SetupNetNS needs Linux network namespaces.
func SetupNetNS() error {
	return fmt.Errorf("proc: network namespaces: %w", errors.ErrUnsupported)
}
