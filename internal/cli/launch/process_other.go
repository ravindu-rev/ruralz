// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package launch

import "syscall"

// processGroup returns no attributes: no ruralzd is built for this
// platform, so ruralz dev run refuses it before any launch.
func processGroup() *syscall.SysProcAttr { return nil }
