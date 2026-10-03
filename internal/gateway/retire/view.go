// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package retire

import (
	"time"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
)

// View returns the /debug/snapshots body (spec 04 req 73): every live
// snapshot, the active one first and then newest to oldest, with its state,
// display revision, digest, pin count and times (RFC 3339 UTC; retiredAt
// once retired, graceEndsAt once closing). pending and lastKnownGood are
// the digests ("sha256:…") of the loader's pending Revision and the
// Last-Known-Good pointer; "" encodes null.
func (h *Holder) View(pending, lastKnownGood string) adminapi.Snapshots {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := adminapi.Snapshots{Snapshots: make([]adminapi.SnapshotInfo, 0, len(h.live))}
	for i := len(h.live) - 1; i >= 0; i-- {
		t := h.live[i]
		out.Snapshots = append(out.Snapshots, adminapi.SnapshotInfo{
			State:       t.state.String(),
			Revision:    t.s.Revision.Digest.Short(),
			Digest:      t.s.Revision.Digest.String(),
			Pins:        t.s.Pins.Count(),
			ActivatedAt: stamp(t.activatedAt),
			RetiredAt:   stamp(t.retiredAt),
			GraceEndsAt: stamp(t.graceEndsAt),
		})
	}
	if pending != "" {
		out.Pending = &pending
	}
	if lastKnownGood != "" {
		out.LastKnownGood = &lastKnownGood
	}
	return out
}

// stamp formats t in RFC 3339 UTC, "" for the zero time.
func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}
