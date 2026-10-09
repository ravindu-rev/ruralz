// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package logsink

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/telemetry/catalog"
)

// Framing slots: the catalog keys written after time, level and msg, in
// this order (09 req 63). A top-level attribute with one of these keys
// fills its slot instead of appearing among the other attributes, so the
// member order is fixed and no key repeats.
const (
	slotComponent = iota
	slotNodeID
	slotRevision
	slotTraceID
	slotSpanID
	slotCode
	slotError
	numSlots
)

// slotKey returns the key of a slot.
func slotKey(i int) string {
	switch i {
	case slotComponent:
		return catalog.KeyComponent
	case slotNodeID:
		return catalog.KeyNodeID
	case slotRevision:
		return catalog.KeyRevision
	case slotTraceID:
		return catalog.KeyTraceID
	case slotSpanID:
		return catalog.KeySpanID
	case slotCode:
		return catalog.KeyCode
	default:
		return catalog.KeyError
	}
}

// slotOf returns the slot of key, or -1.
func slotOf(key string) int {
	switch key {
	case catalog.KeyComponent:
		return slotComponent
	case catalog.KeyNodeID:
		return slotNodeID
	case catalog.KeyRevision:
		return slotRevision
	case catalog.KeyTraceID:
		return slotTraceID
	case catalog.KeySpanID:
		return slotSpanID
	case catalog.KeyCode:
		return slotCode
	case catalog.KeyError:
		return slotError
	default:
		return -1
	}
}

// renamePrefix is prepended to a top-level attribute key that would repeat
// a built-in member (time, level, msg); sloglint forbids those keys at
// call sites, and the rename keeps every line free of duplicate names.
const renamePrefix = "attr_"

func reservedKey(key string) bool {
	return key == slog.TimeKey || key == slog.LevelKey || key == slog.MessageKey
}

// emptyAttr reports the zero Attr, which handlers ignore.
func emptyAttr(a slog.Attr) bool {
	return a.Key == "" && a.Value.Kind() == slog.KindAny && a.Value.Any() == nil
}

// level is one nesting level of a derived handler: the top level (name "")
// or a group opened by WithGroup, with the attributes added inside it.
type level struct {
	name  string
	attrs []slog.Attr // frozen and transformed (ReplaceAttr, redaction)
}

// state is the immutable derived state of a Handler, shared by every
// record it queues.
type state struct {
	component string
	slots     [numSlots]slog.Attr // from top-level WithAttrs
	slotSet   uint8
	levels    []level  // levels[0] is the top level
	groups    []string // names of levels[1:], for ReplaceAttr
	pre       []byte   // levels encoded as members, groups with attributes opened
	open      int      // groups opened in pre
	pending   []string // trailing groups without attributes, opened on demand
}

func (st *state) top() bool { return len(st.levels) == 1 }

// clone copies st so a derivation can change its last level.
func (st *state) clone() *state {
	c := *st
	c.levels = slices.Clone(st.levels)
	last := &c.levels[len(c.levels)-1]
	last.attrs = slices.Clip(last.attrs)
	c.groups = slices.Clip(st.groups)
	return &c
}

// encode rebuilds pre, open and pending from the levels.
func (st *state) encode(s *Sink) {
	buf := []byte{'{'}
	st.open = 0
	st.pending = nil
	for i, lv := range st.levels {
		if i > 0 {
			st.pending = append(st.pending, lv.name)
		}
		if len(lv.attrs) == 0 {
			continue
		}
		for _, g := range st.pending {
			buf = appendKey(buf, g)
			buf = append(buf, '{')
			st.open++
		}
		st.pending = nil
		for _, a := range lv.attrs {
			buf = s.appendPrepared(buf, a)
		}
	}
	st.pre = buf[1:]
}

// Handler is the Ruralz slog.Handler (09 req 61). Enabled compares with
// the fixed process level; Handle clones and freezes the record with the
// handler's derived state and the context's trace IDs, and queues it
// without blocking. It never writes: the Sink's worker encodes and writes.
type Handler struct {
	s  *Sink
	st *state
}

var _ slog.Handler = (*Handler)(nil)

// Enabled reports whether l is at or above the process level.
func (h *Handler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.s.level }

// WithAttrs returns a handler whose records carry as. Values are resolved
// and redacted here, once; top-level attributes with a framing key (such
// as code or error) fill their slot.
func (h *Handler) WithAttrs(as []slog.Attr) slog.Handler {
	if len(as) == 0 {
		return h
	}
	st := h.st.clone()
	creds := h.s.creds.Load()
	lv := &st.levels[len(st.levels)-1]
	var add func(a slog.Attr)
	add = func(a slog.Attr) {
		a.Value = freeze(a.Value, creds)
		if st.top() {
			if a.Key == "" && a.Value.Kind() == slog.KindGroup {
				for _, c := range a.Value.Group() {
					add(c)
				}
				return
			}
			if k := slotOf(a.Key); k >= 0 {
				p, ok := h.s.prepareSlot(a)
				st.slots[k] = p
				if ok {
					st.slotSet |= 1 << k
				} else {
					st.slotSet &^= 1 << k
				}
				return
			}
		}
		if p, ok := h.s.prepare(st.groups, a, st.top(), creds); ok {
			lv.attrs = append(lv.attrs, p)
		}
	}
	for _, a := range as {
		add(a)
	}
	st.encode(h.s)
	return &Handler{s: h.s, st: st}
}

// WithGroup returns a handler that nests later attributes under name; an
// empty name returns h.
func (h *Handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	st := h.st.clone()
	st.levels = append(st.levels, level{name: name})
	st.groups = append(st.groups, name)
	st.encode(h.s)
	return &Handler{s: h.s, st: st}
}

// Handle queues r (09 req 61, 65): it counts the record as produced, and
// drops it with reason queue_full when the Sink is closed or its queue is
// out of records or bytes. It never blocks and always returns nil.
//
// The byte reservation counts what the queued record holds: the message,
// string values, and the full size of the values freeze copies (byte
// slices, header maps, request and URL fields). It is checked before the
// copy, so an oversized value is dropped without being cloned, and trued
// up after freezing for values whose frozen size is only known then (an
// error's redacted text, a resolved LogValuer).
func (h *Handler) Handle(ctx context.Context, r slog.Record) error {
	s := h.s
	s.produced.Add(1)
	if s.closed.Load() {
		s.queueFull.Add(1)
		return nil
	}
	size := int64(entryOverhead + len(r.Message))
	needFreeze := false
	r.Attrs(func(a slog.Attr) bool {
		size += attrSize(a)
		if !needFreeze && mutable(a.Value) {
			needFreeze = true
		}
		return true
	})
	if len(s.q) == cap(s.q) || !s.reserve(size) {
		s.queueFull.Add(1)
		return nil
	}
	rec := freezeRecord(r, s.creds.Load(), needFreeze)
	if needFreeze {
		frozen := recordSize(rec)
		if !s.resize(size, frozen) {
			s.queueFull.Add(1)
			return nil
		}
		size = frozen
	}
	e := s.getEntry()
	e.st = h.st
	e.size = size
	e.rec = rec
	if rev := s.revision.Load(); rev != nil {
		e.revision = *rev
	}
	if s.traceContext != nil && ctx != nil {
		e.traceID, e.spanID, e.hasTrace = s.traceContext(ctx)
	}
	select {
	case s.q <- e:
		// A Close that ran between the closed check and the send has
		// already drained the queue; drain again so e is counted.
		if s.closed.Load() {
			s.dropQueued()
		}
	default:
		s.queueFull.Add(1)
		s.release(e)
	}
	return nil
}

// Fixed parts of the byte estimate: an attribute (its slog.Attr, about 40
// bytes, less what entryOverhead covers), a value that is referenced
// rather than copied, and a string or slice header inside a copied value.
const (
	attrOverhead   = 16
	anyOverhead    = 64
	headerOverhead = 24
)

// recordSize estimates the queued bytes of a frozen record.
func recordSize(r slog.Record) int64 {
	n := int64(entryOverhead + len(r.Message))
	r.Attrs(func(a slog.Attr) bool {
		n += attrSize(a)
		return true
	})
	return n
}

// attrSize estimates the queued bytes of a (key, value and framing).
func attrSize(a slog.Attr) int64 {
	n := int64(len(a.Key)) + attrOverhead
	switch a.Value.Kind() {
	case slog.KindString:
		n += int64(len(a.Value.String()))
	case slog.KindGroup:
		for _, c := range a.Value.Group() {
			n += attrSize(c)
		}
	case slog.KindAny:
		n += anySize(a.Value.Any())
	case slog.KindLogValuer:
		n += anyOverhead
	default:
		n += 24
	}
	return n
}

// anySize estimates a KindAny value: the full size of what freeze copies
// or produces (see mutable), and anyOverhead for a value the queue only
// references. It does not allocate.
func anySize(v any) int64 {
	switch x := v.(type) {
	case []byte:
		return headerOverhead + int64(len(x))
	case *[]byte:
		if x != nil {
			return headerOverhead + int64(len(*x))
		}
	case http.Header:
		return headerSize(x)
	case *http.Header:
		if x != nil {
			return headerSize(*x)
		}
	case map[string][]string:
		return headerSize(x)
	case *map[string][]string:
		if x != nil {
			return headerSize(*x)
		}
	case *http.Request:
		if x != nil {
			n := 3*attrOverhead + len(keyMethod) + len(keyHost) + len(catalog.KeyPath) + len(x.Method) + len(x.Host)
			if x.URL != nil {
				n += len(x.URL.Path)
			}
			return int64(n)
		}
	case url.URL:
		return urlSize(&x)
	case *url.URL:
		return urlSize(x)
	case **url.URL:
		if x != nil {
			return urlSize(*x)
		}
	}
	return anyOverhead
}

// headerSize is the size of a copied header map: names, values, and a
// slice and string header each.
func headerSize(h map[string][]string) int64 {
	n := int64(anyOverhead)
	for k, vs := range h {
		n += int64(len(k)) + 2*headerOverhead
		for _, v := range vs {
			n += int64(len(v)) + headerOverhead
		}
	}
	return n
}

// urlSize bounds the redacted string of u: scheme, host and path, the
// path counted three times for percent-encoding.
func urlSize(u *url.URL) int64 {
	if u == nil {
		return headerOverhead
	}
	return int64(headerOverhead + len(u.Scheme) + len(u.Opaque) + len(u.Host) + 3*len(u.Path) + 4)
}

// prepare applies the per-attribute rules to a non-slot attribute: the
// rename of reserved top-level keys, the ReplaceAttr hook (not called for
// groups, whose members get it), credential-name redaction and the empty
// Attr rule. Groups are transformed recursively. It allocates; it runs at
// derivation time and for the bridge only.
func (s *Sink) prepare(groups []string, a slog.Attr, top bool, creds *credSet) (slog.Attr, bool) {
	if emptyAttr(a) {
		return slog.Attr{}, false
	}
	a.Value = a.Value.Resolve()
	if a.Value.Kind() == slog.KindGroup {
		as := a.Value.Group()
		var inner []string
		if a.Key != "" {
			inner = append(slices.Clip(groups), a.Key)
		} else {
			inner = groups
		}
		out := make([]slog.Attr, 0, len(as))
		for _, c := range as {
			if p, ok := s.prepare(inner, c, top && a.Key == "", creds); ok {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return slog.Attr{}, false
		}
		return slog.Attr{Key: a.Key, Value: slog.GroupValue(out...)}, true
	}
	return s.leaf(groups, a, top, creds)
}

// leaf applies the rules of prepare to a resolved non-group attribute.
func (s *Sink) leaf(groups []string, a slog.Attr, top bool, creds *credSet) (slog.Attr, bool) {
	if top && reservedKey(a.Key) {
		a.Key = renamePrefix + a.Key
	}
	if s.replace != nil {
		a = s.replace(groups, a)
		a.Value = a.Value.Resolve()
		if emptyAttr(a) {
			return slog.Attr{}, false
		}
	}
	if creds.name(a.Key) {
		a.Value = slog.StringValue(secret.Redacted)
	}
	return a, true
}

// prepareSlot applies ReplaceAttr to a scalar slot value; the slot keeps
// its key whatever ReplaceAttr returns, and a zero result empties the
// slot. A group value is returned unchanged: its members get ReplaceAttr
// (groups [slot key]) and credential-name redaction where it is encoded
// (appendSlotValue, exportSlot).
func (s *Sink) prepareSlot(a slog.Attr) (slog.Attr, bool) {
	key := a.Key
	a.Value = a.Value.Resolve()
	if s.replace != nil && a.Value.Kind() != slog.KindGroup {
		a = s.replace(nil, a)
		if emptyAttr(a) {
			return slog.Attr{}, false
		}
		a.Value = a.Value.Resolve()
	}
	a.Key = key
	return a, true
}

// appendPrepared encodes an attribute already passed through prepare.
func (s *Sink) appendPrepared(dst []byte, a slog.Attr) []byte {
	if a.Value.Kind() == slog.KindGroup {
		as := a.Value.Group()
		if a.Key == "" {
			for _, c := range as {
				dst = s.appendPrepared(dst, c)
			}
			return dst
		}
		mark := len(dst)
		dst = appendKey(dst, a.Key)
		dst = append(dst, '{')
		inner := len(dst)
		for _, c := range as {
			dst = s.appendPrepared(dst, c)
		}
		if len(dst) == inner {
			return dst[:mark]
		}
		return append(dst, '}')
	}
	dst = appendKey(dst, a.Key)
	return appendScalar(dst, a.Value)
}

// appendRecord encodes one queued record as a JSON line: time, level, msg,
// the framing slots, the handler's attributes, then the record's
// attributes inside the handler's groups (09 req 63).
func (s *Sink) appendRecord(dst []byte, e *entry) []byte {
	st := e.st
	creds := s.creds.Load()
	dst = append(dst, '{')
	if !e.rec.Time.IsZero() {
		dst = appendKey(dst, slog.TimeKey)
		dst = appendTime(dst, e.rec.Time)
	}
	dst = appendKey(dst, slog.LevelKey)
	dst = appendString(dst, e.rec.Level.String())
	dst = appendKey(dst, slog.MessageKey)
	dst = appendString(dst, e.rec.Message)

	var rs [numSlots]slog.Attr
	var rset uint8
	top := st.top()
	if top {
		e.rec.Attrs(func(a slog.Attr) bool {
			scanSlots(a, &rs, &rset)
			return true
		})
	}
	for k := range numSlots {
		switch {
		case rset&(1<<k) != 0:
			if a, ok := s.prepareSlot(rs[k]); ok {
				dst = appendKey(dst, slotKey(k))
				dst = s.appendSlotValue(dst, k, a.Value, creds)
			}
		case st.slotSet&(1<<k) != 0:
			dst = appendKey(dst, slotKey(k))
			dst = s.appendSlotValue(dst, k, st.slots[k].Value, creds)
		default:
			dst = s.appendDefaultSlot(dst, e, k)
		}
	}
	if len(st.pre) > 0 {
		dst = append(dst, ',')
		dst = append(dst, st.pre...)
	}
	mark := len(dst)
	for _, g := range st.pending {
		dst = appendKey(dst, g)
		dst = append(dst, '{')
	}
	before := len(dst)
	e.rec.Attrs(func(a slog.Attr) bool {
		dst = s.appendAttr(dst, st.groups, a, top, creds)
		return true
	})
	if len(dst) == before {
		dst = dst[:mark]
	} else {
		for range st.pending {
			dst = append(dst, '}')
		}
	}
	for range st.open {
		dst = append(dst, '}')
	}
	return append(dst, '}', '\n')
}

// scanSlots records top-level record attributes with a slot key, looking
// into inline groups; the last one of a key wins.
func scanSlots(a slog.Attr, rs *[numSlots]slog.Attr, rset *uint8) {
	if a.Key == "" && a.Value.Kind() == slog.KindGroup {
		for _, c := range a.Value.Group() {
			scanSlots(c, rs, rset)
		}
		return
	}
	if k := slotOf(a.Key); k >= 0 {
		rs[k] = a
		*rset |= 1 << k
	}
}

// appendSlotValue encodes the value of slot k filled by an attribute; a
// group value becomes an object whose members follow the attribute rules,
// with ReplaceAttr seeing the groups [slot key].
func (s *Sink) appendSlotValue(dst []byte, k int, v slog.Value, creds *credSet) []byte {
	if v.Kind() == slog.KindGroup {
		var groups []string
		if s.replace != nil {
			groups = []string{slotKey(k)} // only with ReplaceAttr, so the path stays allocation-free without it
		}
		dst = append(dst, '{')
		for _, c := range v.Group() {
			dst = s.appendAttr(dst, groups, c, false, creds)
		}
		return append(dst, '}')
	}
	return appendScalar(dst, v)
}

// appendDefaultSlot writes a slot's value from the Sink and the entry:
// component from the handler, node_id, the Revision active when the
// record was logged (absent before the first activation), and the trace
// and span IDs of the logging context.
func (s *Sink) appendDefaultSlot(dst []byte, e *entry, k int) []byte {
	switch k {
	case slotComponent:
		if e.st.component != "" {
			dst = appendKey(dst, catalog.KeyComponent)
			dst = appendString(dst, e.st.component)
		}
	case slotNodeID:
		if s.nodeID != "" {
			dst = appendKey(dst, catalog.KeyNodeID)
			dst = appendString(dst, s.nodeID)
		}
	case slotRevision:
		if e.revision != "" {
			dst = appendKey(dst, catalog.KeyRevision)
			dst = appendString(dst, e.revision)
		}
	case slotTraceID:
		if e.hasTrace {
			dst = appendKey(dst, catalog.KeyTraceID)
			dst = appendHex(dst, e.traceID[:])
		}
	case slotSpanID:
		if e.hasTrace {
			dst = appendKey(dst, catalog.KeySpanID)
			dst = appendHex(dst, e.spanID[:])
		}
	}
	return dst
}

// appendAttr encodes a record attribute on the worker without allocating
// (unless ReplaceAttr is set and groups nest): resolution, the empty-Attr
// and empty-group rules, inline groups, reserved-key renames, ReplaceAttr
// and credential-name redaction. Top-level slot keys were written as
// framing and are skipped.
func (s *Sink) appendAttr(dst []byte, groups []string, a slog.Attr, top bool, creds *credSet) []byte {
	if emptyAttr(a) {
		return dst
	}
	if top && slotOf(a.Key) >= 0 {
		return dst // written as framing, even when its value is a group
	}
	v := a.Value.Resolve()
	if v.Kind() == slog.KindGroup {
		as := v.Group()
		if len(as) == 0 {
			return dst
		}
		if a.Key == "" {
			for _, c := range as {
				dst = s.appendAttr(dst, groups, c, top, creds)
			}
			return dst
		}
		var inner []string
		if s.replace != nil {
			inner = append(slices.Clip(groups), a.Key)
		}
		mark := len(dst)
		dst = appendKey(dst, a.Key)
		dst = append(dst, '{')
		start := len(dst)
		for _, c := range as {
			dst = s.appendAttr(dst, inner, c, false, creds)
		}
		if len(dst) == start {
			return dst[:mark]
		}
		return append(dst, '}')
	}
	a.Value = v
	a, ok := s.leaf(groups, a, top, creds)
	if !ok {
		return dst
	}
	if a.Value.Kind() == slog.KindGroup {
		return s.appendPrepared(dst, a)
	}
	dst = appendKey(dst, a.Key)
	return appendScalar(dst, a.Value)
}
