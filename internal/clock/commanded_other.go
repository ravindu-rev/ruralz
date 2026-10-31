// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package clock

import (
	"errors"
	"fmt"
	"os"
)

// openCommandFD fails: the harness passes the command socket as an extra
// file descriptor, which only Unix processes inherit.
func openCommandFD(uintptr) (*os.File, error) {
	return nil, fmt.Errorf("%w: %w", ErrNoCommandDescriptor, errors.ErrUnsupported)
}
