// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package control is Ruralz Control, the control plane run by ruralz-control.
// The REST API, Rollouts, Drift detection, enrollment and the Control Store
// arrive in milestone M2 (docs/architecture/04-control-plane-and-gitops.md).
package control

import (
	"context"
	"fmt"
	"io"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
)

// ExitNotImplemented is returned while the control plane is not implemented.
const ExitNotImplemented = 2

// Run starts a Ruralz Control replica and returns the process exit code.
// Until M2 it only reports that the control plane is not implemented yet.
func Run(_ context.Context, _ []string, _, stderr io.Writer) int {
	info := buildinfo.Get()
	_, _ = fmt.Fprintf(stderr, "ruralz-control %s (commit %s, flavor %s): Ruralz Control is not implemented yet; it is Planned (M2)\n",
		info.Version, info.Commit, info.Flavor)
	return ExitNotImplemented
}
