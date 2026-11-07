// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package controlstore

import (
	"maps"
	"slices"
	"time"
)

// Record is one generic record: a value under a bucket and key, with the
// content objects it references and the Raft indexes that created and
// last modified it. Values are immutable once returned: callers never
// modify them.
type Record struct {
	Bucket      string   `json:"bucket"`
	Key         string   `json:"key"`
	Value       []byte   `json:"value"`
	Refs        []Digest `json:"refs,omitempty"`
	CreateIndex uint64   `json:"createIndex"`
	ModifyIndex uint64   `json:"modifyIndex"`
}

// AuditEntry is one committed, unsealed audit entry. Data is the audit
// log's encoding of actor, role, action, target, digests, source, result
// and time, opaque to the store.
type AuditEntry struct {
	Seq   uint64    `json:"seq"`
	Index uint64    `json:"index"`
	At    time.Time `json:"at"`
	Data  []byte    `json:"data"`
}

// AuditSegment is one sealed segment: a content object holding entries
// FirstSeq to LastSeq, chained to the previous segment's digest.
type AuditSegment struct {
	Digest    Digest    `json:"digest"`
	Size      int       `json:"size"`
	FirstSeq  uint64    `json:"firstSeq"`
	LastSeq   uint64    `json:"lastSeq"`
	Prev      Digest    `json:"prev"`
	FirstAt   time.Time `json:"firstAt"`
	LastAt    time.Time `json:"lastAt"`
	SealedAt  time.Time `json:"sealedAt"`
	SealIndex uint64    `json:"sealIndex"`
}

// Member is one server of the committed Raft configuration.
type Member struct {
	ServerID ServerID `json:"serverId"`
	Address  string   `json:"address"`
	Voter    bool     `json:"voter"`
}

// cluster is one Cluster's fenced streams in the current epoch: the
// accepted assignments by sequence, the newest promotion, and both
// sequences.
type cluster struct {
	assignSeq   uint64
	assignments map[uint64]*Assignment
	promoSeq    uint64
	promotion   *Promotion
}

// clone copies the cluster; records are immutable and shared.
func (c *cluster) clone() *cluster {
	return &cluster{assignSeq: c.assignSeq, assignments: maps.Clone(c.assignments), promoSeq: c.promoSeq, promotion: c.promotion}
}

// auditState is the unsealed entries, the sealed segments oldest first,
// the chain head and the next entry sequence.
type auditState struct {
	nextSeq  uint64
	entries  []*AuditEntry
	segments []*AuditSegment
	head     Digest
}

// state is everything the state machine holds. Records, Revisions,
// assignments, promotions, entries and segments are immutable once stored:
// an update replaces the pointer, so a snapshot copies containers only.
type state struct {
	index      uint64
	version    Version
	versionSet bool
	epoch      uint64
	anchor     *AnchorSet
	clusters   map[string]*cluster
	envs       map[string]map[Digest]*RevisionRecord
	buckets    map[string]map[string]*Record
	refs       map[Digest]int
	audit      auditState
	backup     *BackupRecord
	members    []Member
	configIdx  uint64
	pending    map[ServerID]PendingMember
	revoked    map[ServerID]uint64
	reports    map[ServerID]ReplicaReport
	advance    *VersionAdvance
}

// newState returns the empty state.
func newState() *state {
	return &state{
		clusters: map[string]*cluster{},
		envs:     map[string]map[Digest]*RevisionRecord{},
		buckets:  map[string]map[string]*Record{},
		refs:     map[Digest]int{},
		audit:    auditState{nextSeq: 1},
		pending:  map[ServerID]PendingMember{},
		revoked:  map[ServerID]uint64{},
		reports:  map[ServerID]ReplicaReport{},
	}
}

// clone copies every container, sharing the immutable values.
func (s *state) clone() *state {
	c := *s
	c.clusters = make(map[string]*cluster, len(s.clusters))
	for k, v := range s.clusters {
		c.clusters[k] = v.clone()
	}
	c.envs = make(map[string]map[Digest]*RevisionRecord, len(s.envs))
	for k, v := range s.envs {
		c.envs[k] = maps.Clone(v)
	}
	c.buckets = make(map[string]map[string]*Record, len(s.buckets))
	for k, v := range s.buckets {
		c.buckets[k] = maps.Clone(v)
	}
	c.refs = maps.Clone(s.refs)
	c.audit.entries = slices.Clone(s.audit.entries)
	c.audit.segments = slices.Clone(s.audit.segments)
	c.members = slices.Clone(s.members)
	c.pending = maps.Clone(s.pending)
	c.revoked = maps.Clone(s.revoked)
	c.reports = maps.Clone(s.reports)
	return &c
}

// isMember reports whether id is in the committed configuration.
func (s *state) isMember(id ServerID) bool {
	return slices.ContainsFunc(s.members, func(m Member) bool { return m.ServerID == id })
}

// memberIDs returns the committed configuration's server IDs.
func (s *state) memberIDs() []ServerID {
	out := make([]ServerID, 0, len(s.members))
	for _, m := range s.members {
		out = append(out, m.ServerID)
	}
	return out
}

// revisionsByAge returns an Environment's Revisions newest first.
func (s *state) revisionsByAge(env string) []*RevisionRecord {
	recs := slices.Collect(maps.Values(s.envs[env]))
	slices.SortFunc(recs, func(a, b *RevisionRecord) int {
		switch {
		case a.Index > b.Index:
			return -1
		case a.Index < b.Index:
			return 1
		default:
			return 0
		}
	})
	return recs
}

// revisionInUse reports whether an accepted assignment or promotion of
// the current epoch names digest d.
func (s *state) revisionInUse(env string, d Digest) bool {
	for _, c := range s.clusters {
		if c.promotion != nil && c.promotion.PromotedDigest == d {
			return true
		}
		for _, a := range c.assignments {
			if a.Digest == d && a.Environment == env {
				return true
			}
		}
	}
	return false
}

// recomputeRefs rebuilds the content reference counts from the records
// that hold references: Revisions, generic records and sealed segments.
func (s *state) recomputeRefs() {
	s.refs = map[Digest]int{}
	for _, env := range s.envs {
		for _, r := range env {
			for _, d := range r.objects() {
				s.refs[d]++
			}
		}
	}
	for _, b := range s.buckets {
		for _, r := range b {
			for _, d := range r.Refs {
				s.refs[d]++
			}
		}
	}
	for _, seg := range s.audit.segments {
		s.refs[seg.Digest]++
	}
}
