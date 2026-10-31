// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dotpath

import (
	"errors"
	"io"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Reason is why a path cannot be followed through a document.
type Reason uint8

// Path error reasons (docs/architecture/03-data-plane.md "Transform
// Policies": a path through a scalar, * on a value that is not an array, a
// set index past an array's end and a replace path at a value that is not
// a string are path errors; a write cannot create a member of an array, and
// an append needs an array).
const (
	// ThroughScalar is a segment applied to a string, number, boolean or
	// null.
	ThroughScalar Reason = iota + 1
	// StarOnNonArray is a Star segment applied to an object.
	StarOnNonArray
	// IndexPastEnd is a written path whose index is past its array's end.
	IndexPastEnd
	// NameOnArray is a written path whose Name segment meets an array.
	NameOnArray
	// NotString is a replace path at a value that is not a string.
	NotString
	// NotArray is an append path at a value that is not an array.
	NotArray
)

// String describes the reason.
func (r Reason) String() string {
	switch r {
	case ThroughScalar:
		return "path through a scalar"
	case StarOnNonArray:
		return "* on a value that is not an array"
	case IndexPastEnd:
		return "index past the array's end"
	case NameOnArray:
		return "member name on an array"
	case NotString:
		return "value is not a string"
	case NotArray:
		return "value is not an array"
	default:
		return "reason " + strconv.Itoa(int(r))
	}
}

// Sentinel errors of the Editor.
var (
	// ErrPath matches every *PathError.
	ErrPath = errors.New("dotpath: path error")
	// ErrOutputCap reports an operation whose output passed Editor.Limit.
	ErrOutputCap = errors.New("dotpath: output over its cap")
	// ErrNotJSON reports a body that is not JSON for Normalize.
	ErrNotJSON = errors.New("dotpath: body is not JSON")
)

// PathError is a path an operation cannot follow through its document.
// Its text names the reason and the segment, never document data.
type PathError struct {
	// Reason is why the path stops.
	Reason Reason
	// Segment is the zero-based segment the path stops at.
	Segment int
}

// Error returns "dotpath: <reason> at segment <n>".
func (e *PathError) Error() string {
	return "dotpath: " + e.Reason.String() + " at segment " + strconv.Itoa(e.Segment)
}

// Is reports whether target is ErrPath.
func (e *PathError) Is(target error) bool { return target == ErrPath }

// Replacer rewrites the decoded text of one string.
type Replacer interface {
	// Replace appends the rewritten s to dst and reports whether a match
	// rewrote anything.
	Replace(dst, s []byte) ([]byte, bool)
}

// passOptions scan a normalized document: Normalize has checked it, so
// the passes neither repeat the duplicate check nor stop at the depth a
// write may have grown it to.
func passOptions() jsonval.ScanOptions {
	return jsonval.ScanOptions{MaxDepth: jsonval.MaxDepthLimit, AllowDuplicateNames: true}
}

// Editor applies body operations to JSON documents in the transform form
// that Normalize writes. Each operation reads a document and appends the
// edited document to dst, so a caller alternating two buffers edits a
// document without building a tree. An Editor reuses its scanners and
// scratch buffers, so after warm-up its operations allocate nothing beyond
// the growth of the buffers they are given and what a Replacer allocates.
// An Editor is used by one goroutine at a time; the zero Editor is ready.
type Editor struct {
	// Limit, when positive, caps the bytes one operation writes: an
	// output past it fails with ErrOutputCap as soon as a written value
	// passes it.
	Limit int

	c    jsonval.Compactor
	s    jsonval.Scanner
	text []byte
	repl []byte
}

// release drops the document the scanner holds and every unescape buffer
// over maxScratch.
func (e *Editor) release() {
	e.s.Reset(nil, jsonval.ScanOptions{})
	if cap(e.text) > maxScratch {
		e.text = nil
	}
	if cap(e.repl) > maxScratch {
		e.repl = nil
	}
}

// Normalize appends src in the transform form (jsonval.Compactor): no
// insignificant whitespace, members in their order, strings minimally
// escaped and numbers verbatim. src must be strict JSON, as CEL decodes a
// body: one value of valid UTF-8 without duplicate member names, nested at
// most jsonval.DefaultMaxDepth deep; any other src is ErrNotJSON, and dst
// then holds a partial value.
func (e *Editor) Normalize(dst, src []byte) ([]byte, error) {
	out, err := e.c.Append(dst, src)
	if err != nil {
		return out, ErrNotJSON
	}
	if e.Limit > 0 && len(out)-len(dst) > e.Limit {
		return out, ErrOutputCap
	}
	return out, nil
}

// Valid reports whether src is strict JSON as Normalize requires.
func (e *Editor) Valid(src []byte) bool {
	e.s.Reset(src, jsonval.ScanOptions{})
	defer e.s.Reset(nil, jsonval.ScanOptions{})
	for {
		_, err := e.s.Next()
		if errors.Is(err, io.EOF) {
			return true
		}
		if err != nil {
			return false
		}
	}
}

// Remove appends doc without the members and elements p matches: removed
// array elements let later ones move up, and a missing path removes
// nothing. It returns the number of values removed.
func (e *Editor) Remove(dst, doc []byte, p Path) ([]byte, int, error) {
	w := walk{e: e, src: doc, out: dst, base: len(dst), p: p, op: opRemove, emit: true}
	err := w.run()
	return w.out, w.n, err
}

// Extract appends doc without the values p matches, as Remove does, and
// appends each value it removes to capture, comma-separated as the
// elements of an array are written. It returns the number of values.
func (e *Editor) Extract(dst, doc []byte, p Path, capture []byte) (out, captured []byte, n int, err error) {
	w := walk{e: e, src: doc, out: dst, base: len(dst), p: p, op: opExtract, emit: true, capt: capture}
	err = w.run()
	return w.out, w.capt, w.n, err
}

// Collect appends each value p matches in doc to capture, comma-separated
// as the elements of an array are written, and leaves doc as it is. It
// returns the number of values.
func (e *Editor) Collect(doc []byte, p Path, capture []byte) ([]byte, int, error) {
	w := walk{e: e, src: doc, p: p, op: opCollect, capt: capture}
	err := w.run()
	return w.capt, w.n, err
}

// Set appends doc with value, one JSON value in the transform form,
// written at every match of p, replacing what it held. Missing objects
// on the way are created, a new member going to the end of its object;
// nothing is created below a Star segment, which matches nothing in a
// missing array. A Name segment that meets an array and an index past its
// array's end are path errors. It returns the number of values written.
func (e *Editor) Set(dst, doc []byte, p Path, value []byte) ([]byte, int, error) {
	w := walk{e: e, src: doc, out: dst, base: len(dst), p: p, op: opSet, emit: true, payload: value}
	err := w.run()
	return w.out, w.n, err
}

// Append appends doc with elems, JSON values in the transform form
// separated by commas, added to the end of the array at p, which must hold
// no Star segment. A missing array is created as Set creates a value; a
// value that is not an array is a path error.
func (e *Editor) Append(dst, doc []byte, p Path, elems []byte) ([]byte, error) {
	w := walk{e: e, src: doc, out: dst, base: len(dst), p: p, op: opAppend, emit: true, payload: elems}
	err := w.run()
	return w.out, err
}

// Replace appends doc with every string p matches rewritten by r; a match
// that is not a string is a path error. It returns the number of strings
// r rewrote.
func (e *Editor) Replace(dst, doc []byte, p Path, r Replacer) ([]byte, int, error) {
	w := walk{e: e, src: doc, out: dst, base: len(dst), p: p, op: opReplace, emit: true, r: r}
	err := w.run()
	return w.out, w.n, err
}

// opKind is what one walk does at the values its path matches.
type opKind uint8

const (
	opRemove opKind = iota + 1
	opExtract
	opCollect
	opSet
	opAppend
	opReplace
)

// walk is one operation's pass over a document: it writes the document
// again, token by token, and acts where the path matches. The document is
// in the transform form, so every value it copies is already written as
// the output needs it.
type walk struct {
	e       *Editor
	src     []byte
	out     []byte
	base    int
	p       Path
	op      opKind
	emit    bool
	payload []byte
	r       Replacer
	capt    []byte
	n       int
}

// maxScratch is the largest unescape buffer an Editor keeps between
// operations, so an Editor kept in a pool never pins a large string.
const maxScratch = 64 << 10

// run walks the whole document.
func (w *walk) run() error {
	s := &w.e.s
	s.Reset(w.src, passOptions())
	defer w.e.release()
	t, err := s.Next()
	if err != nil {
		return ErrNotJSON
	}
	if err := w.value(t, 0, true); err != nil {
		return err
	}
	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		return ErrNotJSON
	}
	return w.check()
}

// next returns the next token of a document the walk scans.
func (w *walk) next() (jsonval.Token, error) {
	t, err := w.e.s.Next()
	if err != nil {
		return t, ErrNotJSON
	}
	return t, nil
}

// value acts on the value whose first token is t. The value's path has d
// segments, which match the first d of p when ok.
func (w *walk) value(t jsonval.Token, d int, ok bool) error {
	switch {
	case !ok:
		return w.copy(t)
	case d == w.p.Len():
		return w.target(t, d)
	}
	switch t.Kind {
	case jsonval.KindObjectStart:
		return w.object(d)
	case jsonval.KindArrayStart:
		return w.array(d)
	default:
		return &PathError{Reason: ThroughScalar, Segment: d}
	}
}

// object walks the members of an object whose path matches p's first d
// segments, d below its length: segment d selects members by name.
func (w *walk) object(d int) error {
	seg := w.p.Seg(d)
	if seg.Kind == Star {
		return &PathError{Reason: StarOnNonArray, Segment: d}
	}
	last := d+1 == w.p.Len()
	w.byte('{')
	matched := false
	for {
		nt, err := w.next()
		if err != nil {
			return err
		}
		if nt.Kind == jsonval.KindObjectEnd {
			break
		}
		vt, err := w.next()
		if err != nil {
			return err
		}
		hit := w.nameIs(nt, seg.Name)
		matched = matched || hit
		if hit && last && w.drops() {
			if err := w.drop(vt); err != nil {
				return err
			}
			continue
		}
		w.sep()
		w.bytes(w.src[nt.Offset:nt.End])
		w.byte(':')
		if err := w.value(vt, d+1, hit); err != nil {
			return err
		}
	}
	if !matched && w.creates() {
		if err := w.create(d); err != nil {
			return err
		}
	}
	w.byte('}')
	return nil
}

// array walks the elements of an array whose path matches p's first d
// segments, d below its length: segment d selects elements by index, or
// all of them for Star.
func (w *walk) array(d int) error {
	seg := w.p.Seg(d)
	last := d+1 == w.p.Len()
	w.byte('[')
	i := 0
	for {
		vt, err := w.next()
		if err != nil {
			return err
		}
		if vt.Kind == jsonval.KindArrayEnd {
			break
		}
		hit := seg.Kind == Star || (seg.Kind == Index && seg.Index == i)
		i++
		if hit && last && w.drops() {
			if err := w.drop(vt); err != nil {
				return err
			}
			continue
		}
		w.sep()
		if err := w.value(vt, d+1, hit); err != nil {
			return err
		}
	}
	if w.creates() {
		switch {
		case seg.Kind == Name:
			return &PathError{Reason: NameOnArray, Segment: d}
		case seg.Kind == Index && seg.Index >= i:
			return &PathError{Reason: IndexPastEnd, Segment: d}
		}
	}
	w.byte(']')
	return nil
}

// target acts on a value p matches whole: a member or element Remove and
// Extract drop never reaches it, since their container drops it.
func (w *walk) target(t jsonval.Token, d int) error {
	switch w.op {
	case opSet:
		// The container has written the member name or the separator.
		if _, err := w.skip(t); err != nil {
			return err
		}
		w.bytes(w.payload)
		w.n++
		return w.check()
	case opReplace:
		if t.Kind != jsonval.KindString {
			return &PathError{Reason: NotString, Segment: d - 1}
		}
		w.e.text = w.e.s.AppendText(w.e.text[:0], t)
		var changed bool
		w.e.repl, changed = w.r.Replace(w.e.repl[:0], w.e.text)
		if !changed {
			w.bytes(w.src[t.Offset:t.End])
			return nil
		}
		var err error
		if w.out, err = jsonval.AppendStringBytes(w.out, w.e.repl); err != nil {
			return err
		}
		w.n++
		return w.check()
	case opCollect:
		end, err := w.skip(t)
		if err != nil {
			return err
		}
		w.capture(w.src[t.Offset:end])
		return nil
	case opAppend:
		if t.Kind != jsonval.KindArrayStart {
			return &PathError{Reason: NotArray, Segment: d - 1}
		}
		end, err := w.skip(t)
		if err != nil {
			return err
		}
		// The array is in the transform form: "[" elements "]".
		w.bytes(w.src[t.Offset : end-1])
		w.sep()
		w.bytes(w.payload)
		w.byte(']')
		w.n++
		return w.check()
	default:
		return w.copy(t)
	}
}

// create writes the member that segment d of p names, missing from an
// object whose path matches p's first d segments, holding the objects
// down to the written value. Nothing is created below a Star segment.
func (w *walk) create(d int) error {
	k := w.p.Len()
	for j := d + 1; j < k; j++ {
		if w.p.Seg(j).Kind == Star {
			return nil
		}
	}
	w.sep()
	for j := d; j < k; j++ {
		if j > d {
			w.byte('{')
		}
		// Parse admits only UTF-8, so a segment name always encodes.
		w.out, _ = jsonval.AppendString(w.out, w.p.Seg(j).Name)
		w.byte(':')
	}
	if w.op == opAppend {
		w.byte('[')
		w.bytes(w.payload)
		w.byte(']')
	} else {
		w.bytes(w.payload)
	}
	for j := d + 1; j < k; j++ {
		w.byte('}')
	}
	w.n++
	return w.check()
}

// drops reports whether the operation drops the values it matches.
func (w *walk) drops() bool { return w.op == opRemove || w.op == opExtract }

// creates reports whether the operation writes missing values.
func (w *walk) creates() bool { return w.op == opSet || w.op == opAppend }

// drop skips the value starting with t, a member or element the operation
// removes, capturing it for Extract.
func (w *walk) drop(t jsonval.Token) error {
	end, err := w.skip(t)
	if err != nil {
		return err
	}
	if w.op == opExtract {
		w.capture(w.src[t.Offset:end])
	} else {
		w.n++
	}
	return nil
}

// capture appends one matched value to the captured list.
func (w *walk) capture(v []byte) {
	if w.n > 0 {
		w.capt = append(w.capt, ',')
	}
	w.capt = append(w.capt, v...)
	w.n++
}

// copy writes the value starting with t unchanged.
func (w *walk) copy(t jsonval.Token) error {
	end, err := w.skip(t)
	if err != nil {
		return err
	}
	w.bytes(w.src[t.Offset:end])
	return nil
}

// skip consumes the rest of the value starting with t and returns the
// offset just past it.
func (w *walk) skip(t jsonval.Token) (int, error) {
	if t.Kind != jsonval.KindObjectStart && t.Kind != jsonval.KindArrayStart {
		return t.End, nil
	}
	depth := w.e.s.Depth() - 1
	for {
		nt, err := w.next()
		if err != nil {
			return 0, err
		}
		if (nt.Kind == jsonval.KindObjectEnd || nt.Kind == jsonval.KindArrayEnd) && w.e.s.Depth() == depth {
			return nt.End, nil
		}
	}
}

// nameIs reports whether the member name token nt decodes to name.
func (w *walk) nameIs(nt jsonval.Token, name string) bool {
	if !nt.Escaped {
		return string(w.src[nt.Offset+1:nt.End-1]) == name
	}
	w.e.text = w.e.s.AppendText(w.e.text[:0], nt)
	return string(w.e.text) == name
}

// sep writes the comma a member or element needs after another one: the
// output is in the transform form, so the byte written last tells.
func (w *walk) sep() {
	if !w.emit || len(w.out) == w.base {
		return
	}
	switch w.out[len(w.out)-1] {
	case '{', '[', ':':
	default:
		w.out = append(w.out, ',')
	}
}

// byte writes one byte.
func (w *walk) byte(c byte) {
	if w.emit {
		w.out = append(w.out, c)
	}
}

// bytes writes b.
func (w *walk) bytes(b []byte) {
	if w.emit {
		w.out = append(w.out, b...)
	}
}

// check fails once the output passes the Editor's limit.
func (w *walk) check() error {
	if w.e.Limit > 0 && len(w.out)-w.base > w.e.Limit {
		return ErrOutputCap
	}
	return nil
}
