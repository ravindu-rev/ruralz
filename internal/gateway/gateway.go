// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package gateway is Ruralz Gateway, the data plane run by ruralzd. The
// listener, router, Filter Chain, Upstream layer, admin server and loader
// arrive in milestone M1 (docs/architecture/03-data-plane.md).
package gateway

import (
	"context"
	"fmt"
	"io"

	"github.com/ravindu-rev/ruralz/internal/buildinfo"
)

// ExitNotImplemented is returned while the data plane is not implemented.
const ExitNotImplemented = 2

// Run starts a Node and returns the process exit code. Until M1 it only
// reports that the data plane is not implemented yet.
func Run(_ context.Context, _ []string, _, stderr io.Writer) int {
	info := buildinfo.Get()
	_, _ = fmt.Fprintf(stderr, "ruralzd %s (commit %s, flavor %s): Ruralz Gateway is not implemented yet; it is Planned (M1)\n",
		info.Version, info.Commit, info.Flavor)
	return ExitNotImplemented
}
