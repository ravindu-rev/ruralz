// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package aggregate

import (
	"cmp"
	"slices"
	"time"

	"go.opentelemetry.io/otel/attribute"
)

// state is a group's lifecycle state (spec 09 req 56).
type state uint8

const (
	// stPending: created by Admit, not yet bound, not exported.
	stPending state = iota
	// stLive: referenced by at least one live binding.
	stLive
	// stRetiring: referenced only by retired or closing bindings.
	stRetiring
	// stFolded: folded by the retiring ceiling; its series ended and its
	// handles record into _overflow until its bindings are released.
	stFolded
	// stEnded: released or superseded; never exported again.
	stEnded
	// stFixed: Node-wide, _overflow and _unmatched groups, never released.
	stFixed
)

// groupKey identifies a group within its family: the resource (or
// listener) name, and the Phase for per-Phase Policy families (-1 else).
type groupKey struct {
	res   string
	phase int8
}

// group is the unit of admission, folding and retirement: the label sets
// of one family for one resource (and Phase), which share one cell.
// Resource families admit and fold a resource's label sets together
// (spec 09 req 55), so they also end together.
type group struct {
	fam     *family
	key     groupKey
	striped bool
	start   time.Time
	// n is the number of fixed label sets; sets is the static admission
	// fan-out (declared code label sets included) used for the folded
	// label set counts.
	n    int
	sets int
	// ceil is the most series the group can export: its fixed label sets
	// plus every code label set its code table can hold. A retiring group
	// counts ceil against the retiring ceiling, so the exported
	// ruralz_telemetry_series{state="retiring"} never exceeds the count
	// the ceiling holds (spec 09 req 56).
	ceil   int
	attrs  []attribute.Set
	cell   *cell
	slots  []slot
	hslots []histSlot
	ex     []exemplarSlot
	// code is the code dimension of result-code and code layouts.
	code *codeGroup
	// attempts is the target of emit.UpstreamAttempts handles.
	attempts *attemptsTarget
	shared   handleSet

	// Lifecycle, guarded by Registry.mu.
	state   state
	refs    int
	live    int
	entries []*planEntry
	// qpos is the group's index in Registry.queue while it waits there
	// as a retiring foldable group, -1 otherwise.
	qpos int

	// col is the collection state of each fixed label set, owned by the
	// collector.
	col []colState
}

// colState is a label set's collection state: the last sum over stripes
// and the float64 total (counters), or the last integer _sum and its
// float64 total (histograms). Deltas are taken modulo 2^64 (spec 09 req
// 50).
type colState struct {
	last uint64
	tot  float64
}

// add folds the current sum u into the total and returns it.
func (c *colState) add(u uint64) float64 {
	c.tot += float64(u - c.last)
	c.last = u
	return c.tot
}

// codeGroup is the code dimension of a group.
type codeGroup struct {
	table *codeTable
	// base are the attributes every code label set shares.
	base []attribute.KeyValue
	// list are the pre-created codes, kept to rebuild the group.
	list []string
	// auth is the target of emit.AuthDecisions handles.
	auth *authTarget
}

// compareGroups orders a family's groups for export: by resource name,
// then Phase.
func compareGroups(a, b *group) int {
	if c := cmp.Compare(a.key.res, b.key.res); c != 0 {
		return c
	}
	return cmp.Compare(a.key.phase, b.key.phase)
}

// groupSpec is what a new group needs besides its family.
type groupSpec struct {
	key     groupKey
	striped bool
	codes   []string
	start   time.Time
}

// newGroup builds a group of family f with zeroed values.
func (r *Registry) newGroup(f *family, s groupSpec) *group {
	g := &group{
		fam:     f,
		key:     s.key,
		striped: s.striped && r.stripes > 1,
		start:   s.start,
		qpos:    -1,
	}
	g.n = f.n
	if f.layout == layoutAttempts {
		g.n = numClasses + len(f.enums[1].Values) - 1
	}
	base := make([]attribute.KeyValue, 0, 4)
	if f.resLabel != "" {
		base = append(base, attribute.String(f.resLabel, s.key.res))
	}
	if s.key.phase >= 0 {
		base = append(base, attribute.String(labelPhase, f.phase[s.key.phase]))
	}
	g.attrs = make([]attribute.Set, g.n)
	for i := range g.n {
		g.attrs[i] = attribute.NewSet(slices.Concat(base, f.setLabels(i))...)
	}
	if f.hist {
		g.cell = newCell(&r.tab, g.n*histWords, r.stripes, g.striped)
		g.ex = make([]exemplarSlot, g.n)
		g.hslots = make([]histSlot, g.n)
		for i := range g.hslots {
			g.hslots[i] = histSlot{c: g.cell, off: i * histWords, ex: &g.ex[i], b: f.bounds}
		}
	} else {
		hot := g.n
		if f.layout == layoutAttempts {
			// The status classes of error="none" share one striped line;
			// the rarer error series stay unsharded (spec 09 req 49).
			hot = numClasses
		}
		g.cell = newCell(&r.tab, hot, r.stripes, g.striped)
		rare := g.cell
		if hot < g.n {
			rare = newCell(&r.tab, g.n-hot, r.stripes, false)
		}
		g.slots = make([]slot, g.n)
		for i := range g.slots {
			if i < hot {
				g.slots[i] = slot{c: g.cell, off: i}
			} else {
				g.slots[i] = slot{c: rare, off: i - hot}
			}
		}
		if f.layout == layoutAttempts {
			g.attempts = &attemptsTarget{none: g.slots[0], errs: g.slots[hot]}
		}
	}
	g.col = make([]colState, g.n)
	switch f.layout {
	case layoutResultCode:
		codes, capacity := s.codes, len(s.codes)+codeSpare
		if f.role == roleNode {
			codes, capacity = r.codes, r.nodeCodeCapacity()
		}
		t := newCodeTable(f.cat.Name, codes, capacity, r.validCode, r.logger)
		g.code = &codeGroup{
			table: t,
			base:  slices.Concat(base, []attribute.KeyValue{attribute.String(labelResult, f.result[1])}),
			list:  s.codes,
			auth:  &authTarget{allow: g.slots[0], codes: t},
		}
	case layoutCode:
		g.code = &codeGroup{
			table: newCodeTable(f.cat.Name, r.codes, r.nodeCodeCapacity(), r.validCode, r.logger),
			base:  base,
		}
	case layoutValues, layoutStatus, layoutListenerRequests, layoutAttempts, layoutRevisionInfo, layoutComputed:
	}
	g.sets = g.n
	if f.layout == layoutResultCode {
		g.sets += len(s.codes)
	}
	g.ceil = g.n * f.seriesPerSet()
	if g.code != nil {
		g.ceil += int(g.code.table.max)
	}
	return g
}

// codeSpare is the room a code table keeps for codes nobody declared.
const codeSpare = 16

// dynamicCodes is the capacity of a Node-wide code table when the registry
// has no registered code list.
const dynamicCodes = 256

// nodeCodeCapacity is the capacity of a Node-wide code table: every
// registered code, or dynamicCodes without a list.
func (r *Registry) nodeCodeCapacity() int {
	if r.codes == nil {
		return dynamicCodes
	}
	return len(r.codes)
}

// setLabels returns the enumeration attributes of fixed label set i.
func (f *family) setLabels(i int) []attribute.KeyValue {
	switch f.layout {
	case layoutAttempts:
		classes, errs := f.enums[0].Values, f.enums[1].Values
		if i < numClasses {
			return []attribute.KeyValue{
				attribute.String(labelStatusClass, classes[i]),
				attribute.String(labelError, errs[0]),
			}
		}
		return []attribute.KeyValue{
			attribute.String(labelStatusClass, classes[numClasses-1]),
			attribute.String(labelError, errs[i-numClasses+1]),
		}
	case layoutResultCode:
		return []attribute.KeyValue{
			attribute.String(labelResult, f.result[0]),
			attribute.String(labelCode, ""),
		}
	case layoutValues, layoutStatus, layoutListenerRequests, layoutCode, layoutRevisionInfo, layoutComputed:
	}
	vals := f.labelValues(i)
	kv := make([]attribute.KeyValue, len(vals))
	for k, v := range vals {
		kv[k] = attribute.String(f.enums[k].Name, v)
	}
	return kv
}

// codeList returns the pre-created codes of g.
func (g *group) codeList() []string {
	if g.code == nil {
		return nil
	}
	return g.code.list
}

// foldable reports whether the retiring ceiling may fold g.
func (g *group) foldable() bool { return g.fam.foldable() && g.state != stFixed }
