// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Error types a Filter names for the span's error.type
// (filter.Result.Err).
const (
	errTypeCEL          = "cel_error"
	errTypeResultType   = "result_type"
	errTypeInvalidValue = "invalid_value"
	errTypeNullBody     = "null_body"
	errTypePath         = "path_error"
	errTypeOutputCap    = "output_cap"
)

// Causes of cannot decide. Their text never holds request data.
var (
	// errNoMessage is the cause when the Phase has no message to edit.
	errNoMessage = errors.New("transform: no message to edit")
	// errNullBody is a JSON operation on a body that is not JSON.
	errNullBody = errors.New("JSON operation on a body that is not JSON")
	// errNotJSONResult is a config.body string or bytes result under a
	// JSON contentType that is not one JSON value.
	errNotJSONResult = errors.New("result under a JSON contentType is not one JSON value")
	// errNoNestedJSON is a value without a nested JSON form.
	errNoNestedJSON = errors.New("value has no JSON form")
	// errOutputCap is a body over the output cap.
	errOutputCap = errors.New("body over its output cap")
	// errQueryValue is a computed query value over the cap.
	errQueryValue = errors.New("query value longer than " + strconv.Itoa(httpfield.MaxQueryValueBytes) + " bytes")
)

// maxScratch is the largest scratch buffer a pooled scratch keeps between
// calls, so a Filter never pins a large body.
const maxScratch = 64 << 10

// Filter is a compiled transform Policy at one attachment environment. It
// is immutable and safe for concurrent use; the field lines it hands out
// are shared and read-only (package documentation, "Cost").
type Filter struct {
	typ  v1alpha1.PolicyType
	side Side
	// phase is the one Phase the Filter acts in.
	phase phase.Phase
	// limit is the output cap; legCap takes the leg's MaxBodyBytes first
	// (transform.response at Upstream scope).
	limit  int64
	legCap bool
	// body is config.body, nil when absent; ctLine is the contentType as a
	// one-line field value and ctJSON whether it names JSON.
	body   expr.Program
	ctLine []string
	ctJSON bool
	// ops are the entries in the order the Policy applies them.
	ops []op
	// nstr and nval count the string and value results; lines the computed
	// header values.
	nstr, nval, lines int
	// rewrites is set when the config may rewrite the body; needsBody when
	// the Filter reads the body whole; valuesReadBody when a header, query
	// or path valueExpression reads it.
	rewrites, needsBody, valuesReadBody bool
	// fold holds the canonical keys of the fields the Filter may write;
	// lens is the lenBit union of their lengths.
	fold []string
	lens uint64
	// emptyDoc is the empty object set and append start from on an empty
	// body, and jsonLine the Content-Type line of a body they create.
	emptyDoc []byte
	jsonLine []string
	pool     sync.Pool
}

var _ filter.Filter = (*Filter)(nil)

// Handle runs the Policy in its Phase and continues in every other one. A
// failure of the package documentation ("Failures") cannot decide with the
// class default code and leaves the message as it was; a spent buffer
// budget or an oversized body returns the SPI sentinel.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	if p != f.phase {
		return filter.Next()
	}
	m := x.Message()
	if m == nil {
		return filter.Undecided("", errNoMessage)
	}
	h := m.Header()
	if h == nil {
		return filter.Undecided("", errNoMessage)
	}
	s := f.get()
	defer f.put(s)
	r := run{f: f, ctx: ctx, x: x, m: m, h: h, s: s, curBuf: -1, docBuf: -1}
	if err := r.do(); err != nil {
		return filter.Undecided("", err)
	}
	return filter.Next()
}

// scratch is the pooled working state of one call.
type scratch struct {
	ed dotpath.Editor
	// buf holds the documents of a call: at most two are in use at once,
	// the current body and its normalized form, so one is always spare.
	buf [3][]byte
	// capt holds the values a move or append takes, val one JSON value.
	capt, val []byte
	q         httpfield.Query
	qbuf      []byte
	strs      []string
	vals      []expr.Value
	edits     []headerEdit
}

// get takes a scratch from the pool, sized for f's results.
func (f *Filter) get() *scratch {
	s, _ := f.pool.Get().(*scratch)
	if s == nil {
		s = &scratch{}
	}
	if cap(s.strs) < f.nstr {
		s.strs = make([]string, f.nstr)
	}
	s.strs = s.strs[:f.nstr]
	if cap(s.vals) < f.nval {
		s.vals = make([]expr.Value, f.nval)
	}
	s.vals = s.vals[:f.nval]
	return s
}

// put drops every reference s holds to request data, and every buffer
// over maxScratch, and returns it to the pool.
func (f *Filter) put(s *scratch) {
	clear(s.strs)
	clear(s.vals)
	clear(s.edits)
	s.edits = s.edits[:0]
	s.q.Reset("")
	for i := range s.buf {
		s.buf[i] = trim(s.buf[i])
	}
	s.capt, s.val, s.qbuf = trim(s.capt), trim(s.val), trim(s.qbuf)
	f.pool.Put(s)
}

// trim returns b emptied, or nil when its capacity is over maxScratch.
func trim(b []byte) []byte {
	if cap(b) > maxScratch {
		return nil
	}
	return b[:0]
}

// run is one call's state: the current body and the edits to commit.
type run struct {
	f   *Filter
	ctx context.Context
	x   filter.Exchange
	m   filter.Message
	h   http.Header
	s   *scratch
	// limit is the output cap of the call, 0 for none.
	limit int64
	// cur is the current body: the message's own bytes until the Policy
	// rewrites it, then a scratch buffer, curBuf (-1 for the message's
	// bytes). docBuf is the scratch buffer holding cur's normalized form,
	// -1 when there is none; when both are set they are equal.
	cur            []byte
	curBuf, docBuf int
	// ctJSON is set when the body's media type names JSON; changed once
	// the Policy rewrote the body.
	ctJSON, changed bool
	// bodyVal is config.body's result.
	bodyVal expr.Value
	// lines backs the computed header values of the call, nl of them
	// used.
	lines []string
	nl    int
	// query is set once s.q holds the query to commit; path is the
	// forwarded path to commit when pathSet.
	query, pathSet bool
	path           string
}

// do runs the Policy: it reads the body when needed, evaluates every CEL
// field, applies config.body and the entries in order, then commits.
func (r *run) do() error {
	f := r.f
	r.limit = f.limit
	if f.legCap {
		if l := r.x.Leg(); l != nil && l.MaxBodyBytes > 0 {
			r.limit = l.MaxBodyBytes
		}
	}
	r.s.ed.Limit = 0
	if r.limit > 0 {
		r.s.ed.Limit = int(min(r.limit, math.MaxInt))
	}
	bodyOK := f.side == Request || bodyAllowed(r.x, r.m, r.h)
	if (bodyOK && f.needsBody) || f.valuesReadBody {
		b, err := r.m.Body(r.ctx)
		if err != nil {
			if errors.Is(err, filter.ErrBudget) || errors.Is(err, filter.ErrTooLarge) {
				return err
			}
			return fmt.Errorf("%s: body: %w", f.typ, err)
		}
		r.cur = b
		r.ctJSON = jsonContent(r.h)
	}
	if f.nstr > 0 || (bodyOK && (f.body != nil || f.nval > 0)) {
		if err := r.evaluate(bodyOK); err != nil {
			return err
		}
	}
	if bodyOK && f.body != nil {
		if err := r.buildBody(); err != nil {
			return err
		}
	}
	for i := range f.ops {
		o := &f.ops[i]
		if !bodyOK && o.body() {
			continue
		}
		if err := r.apply(o); err != nil {
			return err
		}
	}
	return r.commit()
}

// bodyAllowed reports whether a response's body may be rewritten: not on
// a response that cannot have content, to a HEAD request or with status
// 1xx, 204 or 304, nor on a 206 or a response with Content-Range, whose
// body is never rewritten (package documentation, "Absent and empty
// bodies").
func bodyAllowed(x filter.Exchange, m filter.Message, h http.Header) bool {
	switch st := m.Status(); {
	case st >= 100 && st < 200, st == http.StatusNoContent, st == http.StatusNotModified, st == http.StatusPartialContent:
		return false
	}
	if hasField(h, keyContentRange) {
		return false
	}
	method := x.Method()
	if l := x.Leg(); l != nil && l.Request != nil {
		method = l.Request.Method
	}
	return method != http.MethodHead
}

// jsonContent reports whether h has exactly one Content-Type field line
// and it names JSON (jsonval.IsJSONMediaType), the rule CEL request.body
// and response.body follow.
func jsonContent(h http.Header) bool {
	lines := h[keyContentType]
	return len(lines) == 1 && jsonval.IsJSONMediaType(lines[0])
}

// evaluate runs every CEL field of the call before any edit, over the
// message as it reached the Policy, so no entry reads another's output:
// config.body and the set entries with target body only when the body may
// be rewritten. A header value is trimmed and checked as a field value, a
// query value against its cap and a path by checkPath.
func (r *run) evaluate(bodyOK bool) error {
	f := r.f
	v := r.x.Vars()
	if bodyOK && f.body != nil {
		val, err := f.body.EvalValue(r.ctx, v)
		if err != nil {
			return r.evalError("config.body", err)
		}
		r.bodyVal = val
	}
	for i := range f.ops {
		o := &f.ops[i]
		if o.prog == nil {
			continue
		}
		if o.kind == opSetBody {
			if !bodyOK {
				continue
			}
			val, err := o.prog.EvalValue(r.ctx, v)
			if err != nil {
				return r.evalError(o.at, err)
			}
			r.s.vals[o.slot] = val
			continue
		}
		raw, err := o.prog.EvalString(r.ctx, v)
		if err != nil {
			return r.evalError(o.at, err)
		}
		switch o.kind {
		case opSetHeader:
			raw, err = httpfield.NormalizeValue(raw)
		case opSetQuery:
			if len(raw) > httpfield.MaxQueryValueBytes {
				err = errQueryValue
			}
		default:
			raw, err = checkPath(raw)
		}
		if err != nil {
			return r.fail(o.at, errTypeInvalidValue, err)
		}
		r.s.strs[o.slot] = raw
	}
	return nil
}

// evalError classifies a CEL runtime error of the field at: result_type
// for a wrong result type, the SPI sentinel when the body the expression
// read could not be decoded for lack of buffer budget or past its decoded
// limit, else cel_error.
func (r *run) evalError(at string, err error) error {
	kind := errTypeCEL
	var ee *expr.EvalError
	if errors.As(err, &ee) {
		switch ee.Kind {
		case expr.KindResultType:
			kind = errTypeResultType
		case expr.KindBody:
			if _, derr := r.m.Decoded(r.ctx); errors.Is(derr, filter.ErrBudget) || errors.Is(derr, filter.ErrTooLarge) {
				return derr
			}
		default:
		}
	}
	return r.fail(at, kind, err)
}

// fail returns the cannot-decide cause of the field at.
func (r *run) fail(at, kind string, err error) error {
	return &opError{typ: r.f.typ, at: at, kind: kind, err: err}
}

// editFail classifies an Editor error of the entry at: path_error,
// output_cap, invalid_value for a rewritten string that is not UTF-8, and
// null_body for a body that is not JSON.
func (r *run) editFail(at string, err error) error {
	switch {
	case errors.Is(err, dotpath.ErrPath):
		return r.fail(at, errTypePath, err)
	case errors.Is(err, dotpath.ErrOutputCap):
		return r.fail(at, errTypeOutputCap, err)
	case errors.Is(err, jsonval.ErrInvalidUTF8):
		return r.fail(at, errTypeInvalidValue, err)
	default:
		return r.fail(at, errTypeNullBody, errNullBody)
	}
}

// buildBody writes config.body's result as the body: a top-level string
// as its UTF-8 bytes, bytes as they are, any other value as JSON. Under a
// JSON contentType a string or bytes result must be one JSON value. The
// body takes Content-Type contentType.
func (r *run) buildBody() error {
	i := r.spare(-1)
	out, err := r.bodyVal.AppendBody(r.s.buf[i][:0])
	r.s.buf[i] = out
	if err != nil {
		return r.fail("config.body", errTypeResultType, err)
	}
	if r.limit > 0 && int64(len(out)) > r.limit {
		return r.fail("config.body", errTypeOutputCap, errOutputCap)
	}
	if r.f.ctJSON && !r.s.ed.Valid(out) && r.textResult(out) {
		return r.fail("config.body", errTypeInvalidValue, errNotJSONResult)
	}
	r.cur, r.curBuf, r.docBuf = out, i, -1
	r.changed, r.ctJSON = true, r.f.ctJSON
	r.edit(keyContentType, r.f.ctLine)
	return nil
}

// textResult reports whether config.body's result, written as out, is a
// top-level string or bytes, which AppendBody writes as its contents: its
// nested JSON form differs from out. Any other value is JSON by
// construction, however deep it nests.
func (r *run) textResult(out []byte) bool {
	nj, ok := r.bodyVal.(expr.NestedJSON)
	if !ok {
		return true
	}
	j, err := nj.AppendJSON(r.s.val[:0])
	r.s.val = j
	return err != nil || !bytes.Equal(j, out)
}

// apply applies one entry.
func (r *run) apply(o *op) error {
	switch o.kind {
	case opSetHeader:
		if r.lines == nil {
			r.lines = make([]string, r.f.lines)
		}
		r.lines[r.nl] = r.s.strs[o.slot]
		r.edit(o.key, r.lines[r.nl:r.nl+1:r.nl+1])
		r.nl++
	case opRemoveHeader:
		r.edit(o.key, nil)
	case opSetQuery:
		r.queryEditor().Set(o.key, r.s.strs[o.slot])
	case opRemoveQuery:
		r.queryEditor().Del(o.key)
	case opSetPath:
		r.path, r.pathSet = r.s.strs[o.slot], true
	case opSetBody:
		return r.setBody(o)
	case opRemoveBody, opDelete:
		return r.remove(o)
	case opMove:
		return r.move(o)
	case opAppend:
		return r.appendTo(o)
	case opReplace:
		return r.replace(o)
	case opReplaceRaw:
		return r.replaceRaw(o)
	default:
	}
	return nil
}

// edit records a header edit.
func (r *run) edit(key string, line []string) {
	r.s.edits = append(r.s.edits, headerEdit{key: key, line: line})
}

// queryEditor returns the query to edit, parsed from the message's raw
// query at the first query entry.
func (r *run) queryEditor() *httpfield.Query {
	if !r.query {
		r.s.q.Reset(r.m.RawQuery())
		r.query = true
	}
	return &r.s.q
}

// spare returns a scratch buffer that holds neither the current body, its
// normalized form nor buffer not.
func (r *run) spare(not int) int {
	for i := range len(r.s.buf) {
		if i != r.curBuf && i != r.docBuf && i != not {
			return i
		}
	}
	// Unreachable: at most two buffers are in use when a third is asked
	// for, and not is one of them.
	return 0
}

// doc returns the current body as a normalized document for a JSON
// operation; empty reports an empty body, from which set and append start
// as an empty object while every other operation does nothing. A body that
// is not JSON, by its media type or its bytes, is null_body.
func (r *run) doc(at string) (doc []byte, empty bool, err error) {
	if len(r.cur) == 0 {
		return r.f.emptyDoc, true, nil
	}
	if r.docBuf >= 0 {
		return r.s.buf[r.docBuf], false, nil
	}
	if !r.ctJSON {
		return nil, false, r.fail(at, errTypeNullBody, errNullBody)
	}
	i := r.spare(-1)
	out, err := r.s.ed.Normalize(r.s.buf[i][:0], r.cur)
	r.s.buf[i] = out
	if err != nil {
		return nil, false, r.editFail(at, err)
	}
	r.docBuf = i
	if r.changed {
		// The normalized form replaces a rewritten body, so at most one
		// buffer stays in use before the operation.
		r.cur, r.curBuf = out, i
	}
	return out, false, nil
}

// took makes out, in scratch buffer i, the current body and its
// normalized form.
func (r *run) took(out []byte, i int) {
	r.s.buf[i] = out
	r.cur, r.curBuf, r.docBuf = out, i, i
	r.changed = true
}

// setBody writes a set entry's value at its dot path, creating missing
// objects; on an empty body it starts from an empty object, and the body
// it writes takes Content-Type application/json.
func (r *run) setBody(o *op) error {
	doc, empty, err := r.doc(o.at)
	if err != nil {
		return err
	}
	nj, ok := r.s.vals[o.slot].(expr.NestedJSON)
	if !ok {
		return r.fail(o.at, errTypeResultType, errNoNestedJSON)
	}
	val, err := nj.AppendJSON(r.s.val[:0])
	r.s.val = val
	if err != nil {
		return r.fail(o.at, errTypeResultType, err)
	}
	i := r.spare(-1)
	out, n, err := r.s.ed.Set(r.s.buf[i][:0], doc, o.path, val)
	r.s.buf[i] = out
	if err != nil {
		return r.editFail(o.at, err)
	}
	if n > 0 {
		r.took(out, i)
		if empty {
			r.created()
		}
	}
	return nil
}

// created records that the body operations wrote a body where there was
// none: it takes Content-Type application/json.
func (r *run) created() {
	r.ctJSON = true
	r.edit(keyContentType, r.f.jsonLine)
}

// remove drops what a remove entry's or a delete's path matches.
func (r *run) remove(o *op) error {
	doc, empty, err := r.doc(o.at)
	if err != nil || empty {
		return err
	}
	i := r.spare(-1)
	out, n, err := r.s.ed.Remove(r.s.buf[i][:0], doc, o.path)
	r.s.buf[i] = out
	if err != nil {
		return r.editFail(o.at, err)
	}
	if n > 0 {
		r.took(out, i)
	}
	return nil
}

// move takes the value at from, or the array of a * path's matches, out of
// the body and writes it at to; a from that matches nothing moves nothing.
func (r *run) move(o *op) error {
	doc, empty, err := r.doc(o.at)
	if err != nil || empty {
		return err
	}
	i := r.spare(-1)
	out, capt, n, err := r.s.ed.Extract(r.s.buf[i][:0], doc, o.path, r.s.capt[:0])
	r.s.buf[i], r.s.capt = out, capt
	if err != nil {
		return r.editFail(o.at, err)
	}
	if n == 0 {
		return nil
	}
	val := capt
	if o.path.HasStar() {
		r.s.val = append(append(append(r.s.val[:0], '['), capt...), ']')
		val = r.s.val
	}
	j := r.spare(i)
	out2, _, err := r.s.ed.Set(r.s.buf[j][:0], out, o.to, val)
	r.s.buf[j] = out2
	if err != nil {
		return r.editFail(o.at, err)
	}
	r.took(out2, j)
	return nil
}

// appendTo adds the value at from, or each of a * path's matches, to the
// end of the array at to, creating it when missing; a from that matches
// nothing appends nothing.
func (r *run) appendTo(o *op) error {
	doc, empty, err := r.doc(o.at)
	if err != nil || empty {
		// On an empty body from matches nothing, so nothing is appended.
		return err
	}
	capt, n, err := r.s.ed.Collect(doc, o.path, r.s.capt[:0])
	r.s.capt = capt
	if err != nil {
		return r.editFail(o.at, err)
	}
	if n == 0 {
		return nil
	}
	i := r.spare(-1)
	out, err := r.s.ed.Append(r.s.buf[i][:0], doc, o.to, capt)
	r.s.buf[i] = out
	if err != nil {
		return r.editFail(o.at, err)
	}
	r.took(out, i)
	return nil
}

// replace rewrites the strings a replace entry's path matches.
func (r *run) replace(o *op) error {
	doc, empty, err := r.doc(o.at)
	if err != nil || empty {
		return err
	}
	i := r.spare(-1)
	out, n, err := r.s.ed.Replace(r.s.buf[i][:0], doc, o.path, o.repl)
	r.s.buf[i] = out
	if err != nil {
		return r.editFail(o.at, err)
	}
	if n > 0 {
		r.took(out, i)
	}
	return nil
}

// replaceRaw rewrites the raw body, JSON or not.
func (r *run) replaceRaw(o *op) error {
	i := r.spare(-1)
	out, changed := o.repl.Replace(r.s.buf[i][:0], r.cur)
	r.s.buf[i] = out
	if !changed {
		return nil
	}
	if r.limit > 0 && int64(len(out)) > r.limit {
		return r.fail(o.at, errTypeOutputCap, errOutputCap)
	}
	r.cur, r.curBuf, r.docBuf = out, i, -1
	r.changed = true
	return nil
}

// commit applies the call's edits once nothing can fail but the body's
// reservation, which goes first: the body, which the message keeps, so it
// is the call's own copy; the header edits in order over the folded
// fields; on a rewritten response the validators; the query; the path.
func (r *run) commit() error {
	f := r.f
	if r.changed {
		body := make([]byte, len(r.cur))
		copy(body, r.cur)
		if err := r.m.SetBody(r.ctx, body); err != nil {
			return err
		}
	}
	validators := r.changed && f.side == Response
	if (len(r.s.edits) > 0 || validators) && hasVariant(r.h, f.fold, f.lens) {
		foldVariants(r.h, f.fold)
	}
	for _, e := range r.s.edits {
		if e.line == nil {
			delete(r.h, e.key)
			continue
		}
		r.h[e.key] = e.line
	}
	if validators {
		weakenETag(r.h)
		delete(r.h, keyAcceptRanges)
	}
	if r.query {
		raw := r.m.RawQuery()
		r.s.qbuf = r.s.q.AppendRaw(r.s.qbuf[:0])
		if string(r.s.qbuf) != raw {
			r.m.SetRawQuery(string(r.s.qbuf))
		}
	}
	if r.pathSet {
		r.m.SetPath(r.path)
	}
	return nil
}

// opError is a failure of one field or entry: a CEL runtime error, an
// invalid result, a path error, a JSON operation on a body that is not
// JSON or an output over its cap. It names the span's error.type; its text
// names the entry and the failure, never request data.
type opError struct {
	typ  v1alpha1.PolicyType
	at   string
	kind string
	err  error
}

// Error returns the Policy type, the entry and the cause's text.
func (e *opError) Error() string { return string(e.typ) + ": " + e.at + ": " + e.err.Error() }

// Unwrap returns the cause.
func (e *opError) Unwrap() error { return e.err }

// ErrorType returns the span's error.type: cel_error, result_type,
// invalid_value, null_body, path_error or output_cap.
func (e *opError) ErrorType() string { return e.kind }
