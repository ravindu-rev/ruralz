// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"log/slog"
	"slices"
)

// exported builds the Bridge view of a written record: the same members
// as the stdout line, with groups as slog groups. It allocates; it runs on
// the worker only while a Bridge is set.
func (s *Sink) exported(e *entry, dropped bool) Exported {
	st := e.st
	creds := s.creds.Load()
	r := slog.NewRecord(e.rec.Time, e.rec.Level, e.rec.Message, e.rec.PC)

	var rs [numSlots]slog.Attr
	var rset uint8
	top := st.top()
	if top {
		e.rec.Attrs(func(a slog.Attr) bool {
			scanSlots(a, &rs, &rset)
			return true
		})
	}
	framing := make([]slog.Attr, 0, numSlots)
	for k := range numSlots {
		switch {
		case rset&(1<<k) != 0:
			if a, ok := s.prepareSlot(rs[k]); ok {
				framing = append(framing, slog.Attr{Key: slotKey(k), Value: a.Value})
			}
		case st.slotSet&(1<<k) != 0:
			framing = append(framing, slog.Attr{Key: slotKey(k), Value: st.slots[k].Value})
		case k == slotComponent && st.component != "":
			framing = append(framing, slog.String(slotKey(k), st.component))
		case k == slotRevision && e.revision != "":
			framing = append(framing, slog.String(slotKey(k), e.revision))
		}
	}

	var inner []slog.Attr
	// add mirrors appendAttr: at the top level, inline groups (key "") are
	// flattened at any depth and slot keys inside them were written as
	// framing, so the bridge record carries each member once, as stdout does.
	var add func(a slog.Attr)
	add = func(a slog.Attr) {
		if top && !emptyAttr(a) {
			a.Value = a.Value.Resolve()
			if a.Key == "" && a.Value.Kind() == slog.KindGroup {
				for _, c := range a.Value.Group() {
					add(c)
				}
				return
			}
			if slotOf(a.Key) >= 0 {
				return
			}
		}
		if p, ok := s.prepare(st.groups, a, top, creds); ok {
			inner = append(inner, p)
		}
	}
	e.rec.Attrs(func(a slog.Attr) bool {
		add(a)
		return true
	})
	for i := len(st.levels) - 1; i >= 0; i-- {
		lv := st.levels[i]
		cur := append(slices.Clip(lv.attrs), inner...)
		if i == 0 {
			inner = cur
			break
		}
		inner = nil
		if len(cur) > 0 {
			inner = []slog.Attr{{Key: lv.name, Value: slog.GroupValue(cur...)}}
		}
	}
	r.AddAttrs(framing...)
	r.AddAttrs(inner...)
	return Exported{Record: r, TraceID: e.traceID, SpanID: e.spanID, HasTrace: e.hasTrace, Dropped: dropped}
}
