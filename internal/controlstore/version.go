// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"cmp"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Version is a Control Store version, a product minor MAJOR.MINOR
// (docs/engineering/04-release-versioning-and-compatibility.md "Ruralz
// Control upgrades"): the replicated version every replica writes formats
// of, which stays N-1 until finalize while N binaries run.
type Version struct {
	Major uint32 `json:"major"`
	Minor uint32 `json:"minor"`
}

// ParseVersion accepts "MAJOR.MINOR" or a product version
// "MAJOR.MINOR.PATCH" with an optional pre-release or build suffix, whose
// patch and suffix it drops: patches never change a format.
func ParseVersion(s string) (Version, error) {
	core, _, _ := strings.Cut(s, "+")
	core, _, _ = strings.Cut(core, "-")
	parts := strings.Split(core, ".")
	if len(parts) != 2 && len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: version %q is not MAJOR.MINOR or MAJOR.MINOR.PATCH", ErrInvalid, s)
	}
	var n [3]uint64
	for i, p := range parts {
		v, err := strconv.ParseUint(p, 10, 32)
		if err != nil || p == "" || (len(p) > 1 && p[0] == '0') {
			return Version{}, fmt.Errorf("%w: version %q has a malformed number %q", ErrInvalid, s, p)
		}
		n[i] = v
	}
	return Version{Major: uint32(n[0]), Minor: uint32(n[1])}, nil //nolint:gosec // G115: parsed with bitSize 32.
}

// String returns "MAJOR.MINOR".
func (v Version) String() string { return fmt.Sprintf("%d.%d", v.Major, v.Minor) }

// IsZero reports whether v is the zero value.
func (v Version) IsZero() bool { return v == Version{} }

// Compare orders v against w: -1, 0 or +1.
func (v Version) Compare(w Version) int {
	if c := cmp.Compare(v.Major, w.Major); c != 0 {
		return c
	}
	return cmp.Compare(v.Minor, w.Minor)
}

// Succeeds reports whether v is the release minor right after w: the next
// minor of the same major, or the first minor of the next major. Ruralz
// Control moves one minor at a time, so only a succeeding version
// finalizes.
func (v Version) Succeeds(w Version) bool {
	return (v.Major == w.Major && v.Minor == w.Minor+1) || (v.Major == w.Major+1 && v.Minor == 0)
}

// FinalizeSoak is how long every replica must have reported N before a
// finalize: 24 hours (target)
// (docs/architecture/04-control-plane-and-gitops.md "Finalizing an
// upgrade").
const FinalizeSoak = 24 * time.Hour

// ReplicaReport is the last committed status report of one replica, voter
// or non-voter, as the leader recorded it. Times come from the leader's
// clock when it committed the change.
type ReplicaReport struct {
	ServerID ServerID `json:"serverId"`
	// BinaryVersion is the replica's product version.
	BinaryVersion string `json:"binaryVersion"`
	// Since is when the replica first reported BinaryVersion, without a
	// break.
	Since time.Time `json:"since"`
	// SnapshotIndex is the index of the replica's newest local snapshot.
	SnapshotIndex uint64 `json:"snapshotIndex"`
	// SnapshotSince is when SnapshotIndex first reached the last
	// version-advance entry; zero while it has not.
	SnapshotSince time.Time `json:"snapshotSince"`
}

// VersionAdvance is the last committed version-advance entry.
type VersionAdvance struct {
	Index uint64    `json:"index"`
	From  Version   `json:"from"`
	To    Version   `json:"to"`
	At    time.Time `json:"at"`
}

// FinalizeStatus is what GET /api/v1/control-store shows of a finalize:
// the Control Store version, each replica's reported binary version and
// snapshot, the soak start and, with a timed soak, when it finalizes.
type FinalizeStatus struct {
	Version Version `json:"version"`
	// Target is N, the version a finalize would advance to; zero when no
	// replica reports a succeeding version.
	Target      Version           `json:"target"`
	LastAdvance *VersionAdvance   `json:"lastAdvance,omitempty"`
	Replicas    []ReplicaFinalize `json:"replicas"`
	// SoakStart is when the last replica began reporting N; zero until
	// every replica does.
	SoakStart time.Time `json:"soakStart"`
	// SoakEnds is SoakStart plus FinalizeSoak.
	SoakEnds time.Time `json:"soakEnds"`
	// HeldSince is when every condition began to hold without a break;
	// zero while one fails.
	HeldSince time.Time `json:"heldSince"`
	// FinalizesAt is HeldSince plus the timed soak's healthy time; zero
	// without a timed soak or while a condition fails.
	FinalizesAt time.Time `json:"finalizesAt"`
	// Ready reports whether a finalize at the evaluation time commits.
	Ready bool `json:"ready"`
	// Blockers say why not, in member order.
	Blockers []string `json:"blockers,omitempty"`
}

// ReplicaFinalize is one member's part of a FinalizeStatus.
type ReplicaFinalize struct {
	ServerID      ServerID `json:"serverId"`
	BinaryVersion string   `json:"binaryVersion"`
	SnapshotIndex uint64   `json:"snapshotIndex"`
	// Reported is false for a member with no committed report.
	Reported bool `json:"reported"`
}

// EvaluateFinalize applies the finalize conditions of "Finalizing an
// upgrade" to committed state: every member (voters and non-voters)
// reports N, the release minor after version; the FinalizeSoak has
// passed since the last began to; and every member reported a snapshot
// at or past the last version-advance entry. healthyFor is the optional
// timed soak's healthy time, zero without one.
func EvaluateFinalize(version Version, last *VersionAdvance, members []ServerID, reports map[ServerID]ReplicaReport,
	now time.Time, healthyFor time.Duration,
) FinalizeStatus {
	st := FinalizeStatus{Version: version, LastAdvance: last}
	var advanceIndex uint64
	if last != nil {
		advanceIndex = last.Index
	}
	sorted := slices.Clone(members)
	slices.Sort(sorted)
	var target Version
	for _, id := range sorted {
		r, ok := reports[id]
		st.Replicas = append(st.Replicas, ReplicaFinalize{ServerID: id, BinaryVersion: r.BinaryVersion, SnapshotIndex: r.SnapshotIndex, Reported: ok})
		if !ok {
			continue
		}
		if v, err := ParseVersion(r.BinaryVersion); err == nil && v.Succeeds(version) && target.IsZero() {
			target = v
		}
	}
	st.Target = target
	if len(sorted) == 0 {
		st.Blockers = append(st.Blockers, "no members")
		return st
	}
	if target.IsZero() {
		st.Blockers = append(st.Blockers, fmt.Sprintf("no replica reports the release after %s", version))
		return st
	}
	held := time.Time{}
	allN := true
	for _, id := range sorted {
		r, ok := reports[id]
		if !ok {
			st.Blockers = append(st.Blockers, fmt.Sprintf("replica %s has not reported", id))
			allN = false
			continue
		}
		v, err := ParseVersion(r.BinaryVersion)
		if err != nil || v != target {
			st.Blockers = append(st.Blockers, fmt.Sprintf("replica %s reports %s, not %s", id, r.BinaryVersion, target))
			allN = false
			continue
		}
		if r.Since.After(st.SoakStart) {
			st.SoakStart = r.Since
		}
		if r.SnapshotIndex < advanceIndex || (advanceIndex > 0 && r.SnapshotSince.IsZero()) {
			st.Blockers = append(st.Blockers, fmt.Sprintf("replica %s has not reported a snapshot past index %d", id, advanceIndex))
			allN = false
			continue
		}
		if r.SnapshotSince.After(held) {
			held = r.SnapshotSince
		}
	}
	if !allN {
		st.SoakStart = time.Time{}
		return st
	}
	st.SoakEnds = st.SoakStart.Add(FinalizeSoak)
	if st.SoakEnds.After(held) {
		held = st.SoakEnds
	}
	if now.Before(st.SoakEnds) {
		st.Blockers = append(st.Blockers, fmt.Sprintf("the soak ends at %s", st.SoakEnds.UTC().Format(time.RFC3339)))
	}
	st.HeldSince = held
	if healthyFor > 0 {
		st.FinalizesAt = held.Add(healthyFor)
	}
	st.Ready = len(st.Blockers) == 0
	return st
}

// ReportRefusal returns why the leader rejects a status report from
// binary (rule 4 of "Ruralz Control upgrades"), or "": a binary older than
// the Control Store version, one that skips a minor, or a binary newer
// than the Control Store version while some member has not reported a
// snapshot at or past the last version-advance entry.
func ReportRefusal(version Version, binary string, last *VersionAdvance, members []ServerID, reports map[ServerID]ReplicaReport) string {
	b, err := ParseVersion(binary)
	if err != nil {
		return fmt.Sprintf("binary version %q is malformed", binary)
	}
	switch c := b.Compare(version); {
	case c < 0:
		return fmt.Sprintf("binary %s is older than the Control Store version %s", b, version)
	case c == 0:
		return ""
	case !b.Succeeds(version):
		return fmt.Sprintf("binary %s skips a minor above the Control Store version %s", b, version)
	}
	if last == nil {
		return ""
	}
	for _, id := range members {
		if r, ok := reports[id]; !ok || r.SnapshotIndex < last.Index {
			return fmt.Sprintf("binary %s is newer than the Control Store version %s and replica %s has not reported a snapshot past index %d",
				b, version, id, last.Index)
		}
	}
	return ""
}
