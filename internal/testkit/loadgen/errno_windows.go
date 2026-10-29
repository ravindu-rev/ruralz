// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package loadgen

import (
	"errors"
	"syscall"
)

// wsaeconnrefused is WSAECONNREFUSED, which package syscall does not name.
const wsaeconnrefused = syscall.Errno(10061)

func isRefused(err error) bool {
	return errors.Is(err, wsaeconnrefused) || errors.Is(err, syscall.ECONNREFUSED)
}

func isReset(err error) bool {
	return errors.Is(err, syscall.WSAECONNRESET) || errors.Is(err, syscall.WSAECONNABORTED) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE)
}
