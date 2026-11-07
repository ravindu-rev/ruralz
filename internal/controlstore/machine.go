// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"fmt"
	"slices"
	"sync"
)

// RejectReason says why the state machine rejected a committed entry.
type RejectReason string

// Rejection reasons.
const (
	// RejectFence: the entry's storeEpoch is not the current one, or its
	// sequence is not the current value plus one, as for a deposed
	// leader's entry at a taken sequence.
	RejectFence RejectReason = "fence"
	// RejectNoEpoch: no anchor set has opened a storeEpoch yet.
	RejectNoEpoch RejectReason = "noEpoch"
	// RejectCondition: a record precondition failed.
	RejectCondition RejectReason = "condition"
	// RejectAnchorSet: the anchor set's version is not higher than the
	// accepted one's, or its storeEpoch is neither the current one nor
	// its own version.
	RejectAnchorSet RejectReason = "anchorSet"
	// RejectRevision: the named Revision is not recorded for that
	// Environment.
	RejectRevision RejectReason = "unknownRevision"
	// RejectRetained: the Revision is among the newest RevisionsKept of
	// its Environment, or an accepted assignment or promotion names it.
	RejectRetained RejectReason = "retained"
	// RejectAudit: an audit seal or prune does not match the audit log.
	RejectAudit RejectReason = "audit"
	// RejectMember: a membership change names a revoked or current
	// member.
	RejectMember RejectReason = "member"
	// RejectVersion: a version entry does not follow the Control Store
	// version.
	RejectVersion RejectReason = "version"
	// RejectInvalid: an op's fields are malformed.
	RejectInvalid RejectReason = "invalid"
)

// Rejection is a committed entry the state machine did not accept: it
// changed nothing, and no replica serves it.
type Rejection struct {
	// Op is the index of the failing op within the command.
	Op     int          `json:"op"`
	Reason RejectReason `json:"reason"`
	Detail string       `json:"detail"`
	// Fence is the stream's current fence for a RejectFence, so the
	// leader re-signs at Fence.Next().
	Fence Fence `json:"fence"`
}

// Error describes the rejection.
func (r *Rejection) Error() string {
	return fmt.Sprintf("controlstore: op %d rejected (%s): %s", r.Op, r.Reason, r.Detail)
}

// Result is the outcome of one applied entry.
type Result struct {
	// Index is the Raft index of the entry.
	Index uint64
	// Rejected is nil when the entry was accepted.
	Rejected *Rejection
	// AuditSeqs are the sequences given to the entries of AppendAudit
	// ops, in order.
	AuditSeqs []uint64
	// Released are the content objects no record references any longer:
	// every replica deletes them locally (ADR-0006 "Snapshots").
	Released []Digest
	// Advanced reports an accepted version-advance entry: every replica
	// then forces a snapshot past it and truncates its log.
	Advanced bool
}

// HaltError reports an entry, snapshot or state this binary must not run:
// a Control Store version above its binary's or an unknown entry type
// (rule 4 of docs/engineering/04-release-versioning-and-compatibility.md
// "Ruralz Control upgrades"). The replica stops before serving and never
// skips the entry.
type HaltError struct {
	Reason string
	Err    error
}

// Error describes the halt.
func (e *HaltError) Error() string {
	if e.Err != nil {
		return "controlstore: halt: " + e.Reason + ": " + e.Err.Error()
	}
	return "controlstore: halt: " + e.Reason
}

// Unwrap returns the cause.
func (e *HaltError) Unwrap() error { return e.Err }

// Halt reasons.
const (
	// HaltVersion: a Control Store version above this binary's.
	HaltVersion = "control_store_version"
	// HaltEntry: an entry this binary cannot decode, such as an unknown
	// op type.
	HaltEntry = "unknown_entry"
	// HaltSkippedMinor: a binary more than one minor above the Control
	// Store version.
	HaltSkippedMinor = "skipped_minor"
	// HaltSnapshot: a binary newer than the Control Store version on a
	// local snapshot older than the last version-advance entry.
	HaltSnapshot = "snapshot_predates_advance"
	// HaltReport: the leader rejected this replica's status report.
	HaltReport = "report_rejected"
)

// Machine is the deterministic Control Store state machine: both drivers
// apply the same rules, so fencing, backup and upgrade semantics stay
// identical (ADR-0006). Apply runs on one goroutine; View may run on any.
type Machine struct {
	binary Version

	mu sync.RWMutex
	st *state
	// changed is closed and replaced after every applied index.
	changed chan struct{}
}

// NewMachine returns an empty machine for a binary of version binary: it
// halts on anything newer.
func NewMachine(binary Version) *Machine {
	return &Machine{binary: binary, st: newState(), changed: make(chan struct{})}
}

// Binary returns the binary version the machine runs.
func (m *Machine) Binary() Version { return m.binary }

// Changed returns a channel closed once the next index applies.
func (m *Machine) Changed() <-chan struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.changed
}

// View runs fn on a consistent view of the committed state; fn must not
// keep the view or block for long.
func (m *Machine) View(fn func(View)) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	fn(View{st: m.st})
}

// Index returns the last applied index.
func (m *Machine) Index() uint64 {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.st.index
}

// notifyLocked wakes Changed waiters; m.mu is held for writing.
func (m *Machine) notifyLocked() {
	close(m.changed)
	m.changed = make(chan struct{})
}

// Apply applies the committed entry data at index. It returns a
// *HaltError, changing nothing, for an entry this binary must not apply.
func (m *Machine) Apply(index uint64, data []byte) (Result, error) {
	cmd, err := DecodeCommand(data)
	if err != nil {
		// An unknown op type, or fields this binary cannot decode: the
		// entry is never skipped.
		return Result{}, &HaltError{Reason: HaltEntry, Err: err}
	}
	if cmd.Version.Compare(m.binary) > 0 {
		return Result{}, &HaltError{Reason: HaltVersion, Err: fmt.Errorf("entry %d is in Control Store version %s, above this binary's %s", index, cmd.Version, m.binary)}
	}
	for _, op := range cmd.Ops {
		if a, ok := op.(AdvanceVersion); ok && a.To.Compare(m.binary) > 0 {
			return Result{}, &HaltError{Reason: HaltVersion, Err: fmt.Errorf("entry %d advances the Control Store version to %s, above this binary's %s", index, a.To, m.binary)}
		}
		if v, ok := op.(InitVersion); ok && v.Version.Compare(m.binary) > 0 {
			return Result{}, &HaltError{Reason: HaltVersion, Err: fmt.Errorf("entry %d sets the Control Store version %s, above this binary's %s", index, v.Version, m.binary)}
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := &txn{st: m.st, index: index, cmd: cmd}
	res := Result{Index: index}
	for i, op := range cmd.Ops {
		if rej := t.apply(op); rej != nil {
			rej.Op = i
			t.rollback()
			res.Rejected = rej
			res.AuditSeqs = nil
			m.st.index = index
			m.notifyLocked()
			return res, nil
		}
	}
	res.AuditSeqs = t.auditSeqs
	res.Released = t.released()
	res.Advanced = slices.ContainsFunc(cmd.Ops, func(op Op) bool { return op.OpType() == OpAdvanceVersion })
	m.st.index = index
	m.notifyLocked()
	return res, nil
}

// SetConfiguration mirrors a committed Raft configuration entry at index.
// A pending member that the configuration now holds stops being pending.
func (m *Machine) SetConfiguration(index uint64, members []Member) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ms := slices.Clone(members)
	slices.SortFunc(ms, func(a, b Member) int {
		switch {
		case a.ServerID < b.ServerID:
			return -1
		case a.ServerID > b.ServerID:
			return 1
		default:
			return 0
		}
	})
	m.st.members = ms
	m.st.configIdx = index
	for _, mem := range ms {
		delete(m.st.pending, mem.ServerID)
	}
	if index > m.st.index {
		m.st.index = index
	}
	m.notifyLocked()
}

// CheckRun returns a *HaltError when this binary must not serve the
// committed state: a Control Store version above the binary's, a binary
// more than one minor above it, or a binary newer than it whose local
// snapshot, at snapshotIndex, predates the last version-advance entry
// (rule 4 of "Ruralz Control upgrades"). The drivers call it at startup,
// after loading the snapshot and log.
func (m *Machine) CheckRun(snapshotIndex uint64) error {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if !m.st.versionSet {
		return nil
	}
	v := m.st.version
	switch c := m.binary.Compare(v); {
	case c < 0:
		return &HaltError{Reason: HaltVersion, Err: fmt.Errorf("the Control Store version %s is above this binary's %s", v, m.binary)}
	case c == 0:
		return nil
	case !m.binary.Succeeds(v):
		return &HaltError{Reason: HaltSkippedMinor, Err: fmt.Errorf("this binary's %s is more than one minor above the Control Store version %s", m.binary, v)}
	}
	if m.st.advance != nil && snapshotIndex < m.st.advance.Index {
		return &HaltError{Reason: HaltSnapshot, Err: fmt.Errorf("the local snapshot at index %d predates the version-advance entry at %d", snapshotIndex, m.st.advance.Index)}
	}
	return nil
}
