// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package reload

import (
	"context"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/gateway/admission"
)

// fdReserve is the file descriptors beyond the connection ceilings a Node
// keeps for its own files, sockets and pipes (target).
const fdReserve = 1024

// FileDescriptorsNeeded returns the file descriptors a Node needs: the
// client and admin connection ceilings, 20,256 (target), plus the active
// Revision's summed circuitBreaker.maxConnections and a 1,024 reserve
// (docs/architecture/03-data-plane.md "Bounded resources").
func FileDescriptorsNeeded(upstreamConnections int) uint64 {
	return uint64(admission.DefaultConnections+admission.AdminConnections+fdReserve) + uint64(max(upstreamConnections, 0))
}

// checkFileLimit logs a warning when the soft RLIMIT_NOFILE is below
// FileDescriptorsNeeded: at start and at each activation. Go raised the
// soft limit to the hard limit at start, so the limit is the hard one.
func (l *Loader) checkFileLimit(ctx context.Context, upstreamConnections int) {
	limit := l.cfg.FileLimit
	if limit == nil {
		limit = fileLimit
	}
	have, ok := limit()
	need := FileDescriptorsNeeded(upstreamConnections)
	if !ok || have >= need {
		return
	}
	l.log.WarnContext(ctx, "file descriptor limit is below the connection ceilings",
		"limit", strconv.FormatUint(have, 10), "needed", strconv.FormatUint(need, 10))
}
