// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
)

// Snapshot is a point-in-time copy of the state machine, taken on the
// apply goroutine and written on any: the containers are copied, the
// immutable values shared.
type Snapshot struct{ st *state }

// Snapshot copies the current state.
func (m *Machine) Snapshot() *Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return &Snapshot{st: m.st.clone()}
}

// Index returns the last index the snapshot holds.
func (s *Snapshot) Index() uint64 { return s.st.index }

// View runs fn on the snapshot's state.
func (s *Snapshot) View(fn func(View)) { fn(View{st: s.st}) }

// snapItem is one line of the encoded snapshot: a type tag and the fields
// of that type. The header comes first and an end item with the item
// count last, so a truncated snapshot never restores.
type snapItem struct {
	T string `json:"t"`

	// header
	Version     *Version `json:"v,omitempty"`
	VersionSet  bool     `json:"versionSet,omitempty"`
	Index       uint64   `json:"index,omitempty"`
	Epoch       uint64   `json:"epoch,omitempty"`
	ConfigIndex uint64   `json:"configIndex,omitempty"`
	NextSeq     uint64   `json:"nextSeq,omitempty"`
	Head        *Digest  `json:"head,omitempty"`

	Anchor     *AnchorSet      `json:"anchor,omitempty"`
	Advance    *VersionAdvance `json:"advance,omitempty"`
	Backup     *BackupRecord   `json:"backup,omitempty"`
	Member     *Member         `json:"member,omitempty"`
	Pending    *PendingMember  `json:"pending,omitempty"`
	ServerID   ServerID        `json:"serverId,omitempty"`
	Report     *ReplicaReport  `json:"report,omitempty"`
	Cluster    string          `json:"cluster,omitempty"`
	AssignSeq  uint64          `json:"assignSeq,omitempty"`
	PromoSeq   uint64          `json:"promoSeq,omitempty"`
	Promotion  *Promotion      `json:"promotion,omitempty"`
	Assignment *Assignment     `json:"assignment,omitempty"`
	Revision   *RevisionRecord `json:"revision,omitempty"`
	Record     *Record         `json:"record,omitempty"`
	Entry      *AuditEntry     `json:"entry,omitempty"`
	Segment    *AuditSegment   `json:"segment,omitempty"`
	Count      uint64          `json:"count,omitempty"`
}

// Item type tags.
const (
	itemHeader     = "header"
	itemAnchor     = "anchor"
	itemAdvance    = "advance"
	itemBackup     = "backup"
	itemMember     = "member"
	itemPending    = "pending"
	itemRevoked    = "revoked"
	itemReport     = "report"
	itemCluster    = "cluster"
	itemAssignment = "assignment"
	itemRevision   = "revision"
	itemRecord     = "record"
	itemEntry      = "auditEntry"
	itemSegment    = "auditSegment"
	itemEnd        = "end"
)

// WriteTo encodes the snapshot as JSON lines in a deterministic order, in
// the format of its Control Store version.
func (s *Snapshot) WriteTo(w io.Writer) (int64, error) {
	cw := &countWriter{w: w}
	bw := bufio.NewWriterSize(cw, 64<<10)
	enc := json.NewEncoder(bw)
	var n uint64
	put := func(it snapItem) error {
		n++
		return enc.Encode(it)
	}
	st := s.st
	head := st.audit.head
	v := st.version
	items := []snapItem{{
		T: itemHeader, Version: &v, VersionSet: st.versionSet, Index: st.index, Epoch: st.epoch,
		ConfigIndex: st.configIdx, NextSeq: st.audit.nextSeq, Head: &head,
	}}
	if st.anchor != nil {
		items = append(items, snapItem{T: itemAnchor, Anchor: st.anchor})
	}
	if st.advance != nil {
		items = append(items, snapItem{T: itemAdvance, Advance: st.advance})
	}
	if st.backup != nil {
		items = append(items, snapItem{T: itemBackup, Backup: st.backup})
	}
	for i := range st.members {
		items = append(items, snapItem{T: itemMember, Member: &st.members[i]})
	}
	for _, id := range slices.Sorted(maps.Keys(st.pending)) {
		p := st.pending[id]
		items = append(items, snapItem{T: itemPending, Pending: &p})
	}
	for _, id := range slices.Sorted(maps.Keys(st.revoked)) {
		items = append(items, snapItem{T: itemRevoked, ServerID: id, Index: st.revoked[id]})
	}
	for _, id := range slices.Sorted(maps.Keys(st.reports)) {
		r := st.reports[id]
		items = append(items, snapItem{T: itemReport, Report: &r})
	}
	for _, it := range items {
		if err := put(it); err != nil {
			return cw.n, err
		}
	}
	for _, name := range slices.Sorted(maps.Keys(st.clusters)) {
		c := st.clusters[name]
		if err := put(snapItem{T: itemCluster, Cluster: name, AssignSeq: c.assignSeq, PromoSeq: c.promoSeq, Promotion: c.promotion}); err != nil {
			return cw.n, err
		}
		for _, seq := range slices.Sorted(maps.Keys(c.assignments)) {
			if err := put(snapItem{T: itemAssignment, Assignment: c.assignments[seq]}); err != nil {
				return cw.n, err
			}
		}
	}
	for _, env := range slices.Sorted(maps.Keys(st.envs)) {
		e := st.envs[env]
		ds := slices.SortedFunc(maps.Keys(e), func(a, b Digest) int { return slices.Compare(a[:], b[:]) })
		for _, d := range ds {
			if err := put(snapItem{T: itemRevision, Revision: e[d]}); err != nil {
				return cw.n, err
			}
		}
	}
	for _, bucket := range slices.Sorted(maps.Keys(st.buckets)) {
		b := st.buckets[bucket]
		for _, k := range slices.Sorted(maps.Keys(b)) {
			if err := put(snapItem{T: itemRecord, Record: b[k]}); err != nil {
				return cw.n, err
			}
		}
	}
	for _, e := range st.audit.entries {
		if err := put(snapItem{T: itemEntry, Entry: e}); err != nil {
			return cw.n, err
		}
	}
	for _, seg := range st.audit.segments {
		if err := put(snapItem{T: itemSegment, Segment: seg}); err != nil {
			return cw.n, err
		}
	}
	if err := enc.Encode(snapItem{T: itemEnd, Count: n}); err != nil {
		return cw.n, err
	}
	if err := bw.Flush(); err != nil {
		return cw.n, err
	}
	return cw.n, nil
}

// countWriter counts written bytes.
type countWriter struct {
	w io.Writer
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// errSnapshot reports a malformed snapshot.
var errSnapshot = errors.New("controlstore: malformed snapshot")

// ReadSnapshot decodes an encoded snapshot for a binary of version
// binary. A snapshot in a Control Store version above binary's is a
// *HaltError.
func ReadSnapshot(r io.Reader, binary Version) (*Snapshot, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 64<<10))
	dec.DisallowUnknownFields()
	st := newState()
	var n uint64
	var sawHeader, sawEnd bool
	var cur *cluster
	for {
		var it snapItem
		if err := dec.Decode(&it); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("%w: %w", errSnapshot, err)
		}
		if sawEnd {
			return nil, fmt.Errorf("%w: data after the end item", errSnapshot)
		}
		if !sawHeader && it.T != itemHeader {
			return nil, fmt.Errorf("%w: the first item is %q, not the header", errSnapshot, it.T)
		}
		if it.T == itemEnd {
			if it.Count != n {
				return nil, fmt.Errorf("%w: %d items, the end item counts %d", errSnapshot, n, it.Count)
			}
			sawEnd = true
			continue
		}
		n++
		if err := readItem(st, it, &cur, &sawHeader, binary); err != nil {
			return nil, err
		}
	}
	if !sawHeader || !sawEnd {
		return nil, fmt.Errorf("%w: truncated", errSnapshot)
	}
	st.recomputeRefs()
	return &Snapshot{st: st}, nil
}

// readItem adds one decoded item to st.
func readItem(st *state, it snapItem, cur **cluster, sawHeader *bool, binary Version) error {
	switch it.T {
	case itemHeader:
		if *sawHeader || it.Version == nil || it.Head == nil {
			return fmt.Errorf("%w: header", errSnapshot)
		}
		*sawHeader = true
		if it.VersionSet && it.Version.Compare(binary) > 0 {
			return &HaltError{Reason: HaltVersion, Err: fmt.Errorf("the snapshot is in Control Store version %s, above this binary's %s", it.Version, binary)}
		}
		st.version, st.versionSet, st.index, st.epoch = *it.Version, it.VersionSet, it.Index, it.Epoch
		st.configIdx, st.audit.nextSeq, st.audit.head = it.ConfigIndex, it.NextSeq, *it.Head
	case itemAnchor:
		if it.Anchor == nil {
			return fmt.Errorf("%w: anchor", errSnapshot)
		}
		st.anchor = it.Anchor
	case itemAdvance:
		if it.Advance == nil {
			return fmt.Errorf("%w: advance", errSnapshot)
		}
		st.advance = it.Advance
	case itemBackup:
		if it.Backup == nil {
			return fmt.Errorf("%w: backup", errSnapshot)
		}
		st.backup = it.Backup
	case itemMember:
		if it.Member == nil {
			return fmt.Errorf("%w: member", errSnapshot)
		}
		st.members = append(st.members, *it.Member)
	case itemPending:
		if it.Pending == nil {
			return fmt.Errorf("%w: pending member", errSnapshot)
		}
		st.pending[it.Pending.ServerID] = *it.Pending
	case itemRevoked:
		st.revoked[it.ServerID] = it.Index
	case itemReport:
		if it.Report == nil {
			return fmt.Errorf("%w: report", errSnapshot)
		}
		st.reports[it.Report.ServerID] = *it.Report
	case itemCluster:
		c := &cluster{assignSeq: it.AssignSeq, promoSeq: it.PromoSeq, promotion: it.Promotion, assignments: map[uint64]*Assignment{}}
		st.clusters[it.Cluster] = c
		*cur = c
	case itemAssignment:
		if it.Assignment == nil || *cur == nil || st.clusters[it.Assignment.Cluster] != *cur {
			return fmt.Errorf("%w: assignment outside its Cluster", errSnapshot)
		}
		(*cur).assignments[it.Assignment.Fence.Seq] = it.Assignment
	case itemRevision:
		if it.Revision == nil {
			return fmt.Errorf("%w: revision", errSnapshot)
		}
		env := st.envs[it.Revision.Environment]
		if env == nil {
			env = map[Digest]*RevisionRecord{}
			st.envs[it.Revision.Environment] = env
		}
		env[it.Revision.Digest] = it.Revision
	case itemRecord:
		if it.Record == nil {
			return fmt.Errorf("%w: record", errSnapshot)
		}
		b := st.buckets[it.Record.Bucket]
		if b == nil {
			b = map[string]*Record{}
			st.buckets[it.Record.Bucket] = b
		}
		b[it.Record.Key] = it.Record
	case itemEntry:
		if it.Entry == nil {
			return fmt.Errorf("%w: audit entry", errSnapshot)
		}
		st.audit.entries = append(st.audit.entries, it.Entry)
	case itemSegment:
		if it.Segment == nil {
			return fmt.Errorf("%w: audit segment", errSnapshot)
		}
		st.audit.segments = append(st.audit.segments, it.Segment)
	default:
		return &HaltError{Reason: HaltEntry, Err: fmt.Errorf("unknown snapshot item %q", strings.TrimSpace(it.T))}
	}
	return nil
}

// Restore replaces the machine's state with a decoded snapshot.
func (m *Machine) Restore(s *Snapshot) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.st = s.st.clone()
	m.notifyLocked()
}
