// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package composition

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Merge rule failures (docs/architecture/03-data-plane.md "Merge rules");
// each fails its step with RZ-RT-015.
var (
	errTargetMissing = errors.New("composition: target is missing")
	errTargetPath    = errors.New("composition: target goes through a value that is not an object or array")
	errNotArray      = errors.New("composition: value is not an array, as collection needs")
	errNotObject     = errors.New("composition: value is not an object")
	errElement       = errors.New("composition: collection element is not an object")
)

// shape returns the value step st merges from a decoded body: the value
// at target (the whole body without one), which must be an array of
// objects with collection and an object without, with select and rename
// applied to the object or to each element.
func (st *step) shape(tree any) (any, error) {
	v := tree
	if st.target != nil {
		var err error
		if v, err = resolve(tree, st.target); err != nil {
			return nil, err
		}
	}
	if st.collection {
		arr, ok := v.([]any)
		if !ok {
			return nil, errNotArray
		}
		out := arr
		if st.selects != nil || st.rename != nil {
			out = make([]any, len(arr))
		}
		for k, e := range arr {
			obj, ok := e.(*jsonval.Object)
			if !ok {
				return nil, errElement
			}
			if st.selects != nil || st.rename != nil {
				out[k] = st.members(obj)
			}
		}
		return out, nil
	}
	obj, ok := v.(*jsonval.Object)
	if !ok {
		return nil, errNotObject
	}
	return st.members(obj), nil
}

// members returns o with select applied (only the named members kept) and
// then rename (members renamed in place); o itself when the step sets
// neither. A rename onto a name another kept member has keeps the later
// value in body order at the first one's position.
func (st *step) members(o *jsonval.Object) *jsonval.Object {
	if st.selects == nil && st.rename == nil {
		return o
	}
	out := jsonval.NewObject(o.Len())
	for k := range o.Len() {
		m := o.At(k)
		if st.selects != nil {
			if _, keep := st.selects[m.Name]; !keep {
				continue
			}
		}
		name := m.Name
		if to, ok := st.rename[name]; ok {
			name = to
		}
		out.Set(name, m.Value)
	}
	return out
}

// resolve returns the value at p in v under the reads of a transform path
// (docs/architecture/03-data-plane.md "Transform Policies", Paths): a
// segment names a member of an object or, when numeric, an index of an
// array. Without "*", a missing member or index is errTargetMissing; with
// one, the value is the array of the matches, in order, a missing value
// matching nothing. A segment on a scalar, null included, a name on an
// array, or "*" on a value that is not an array is errTargetPath.
func resolve(v any, p jsonval.DotPath) (any, error) {
	if p.HasStar() {
		out := []any{}
		if err := collect(v, p, &out); err != nil {
			return nil, err
		}
		return out, nil
	}
	for _, seg := range p {
		next, found, err := child(v, seg)
		switch {
		case err != nil:
			return nil, err
		case !found:
			return nil, errTargetMissing
		}
		v = next
	}
	return v, nil
}

// collect appends the matches of p in v to out.
func collect(v any, p jsonval.DotPath, out *[]any) error {
	if len(p) == 0 {
		*out = append(*out, v)
		return nil
	}
	seg := p[0]
	if seg.Star {
		arr, ok := v.([]any)
		if !ok {
			return errTargetPath
		}
		for _, e := range arr {
			if err := collect(e, p[1:], out); err != nil {
				return err
			}
		}
		return nil
	}
	next, found, err := child(v, seg)
	if err != nil || !found {
		return err
	}
	return collect(next, p[1:], out)
}

// child returns the value seg selects in v.
func child(v any, seg jsonval.DotSegment) (any, bool, error) {
	switch t := v.(type) {
	case *jsonval.Object:
		got, ok := t.Get(seg.Name)
		return got, ok, nil
	case []any:
		i, ok := seg.Index()
		if !ok {
			return nil, false, errTargetPath
		}
		if i >= len(t) {
			return nil, false, nil
		}
		return t[i], true, nil
	default:
		return nil, false, errTargetPath
	}
}

// merged returns the merged response of the executed steps that merge, in
// list order: each step's value under its group, or member by member at
// the top level, a later step winning a key at its first position. Failed
// optional steps are left out; a merge of none is {}. The body is capped
// by limits.maxResponseBodyBytes (RZ-RT-015 above it) and reserved from
// the buffer budget (RZ-RT-004) until the caller closes it.
func (r *run) merged() (*snapshot.UpstreamResponse, error) {
	out := jsonval.NewObject(0)
	merged := make([]int, 0, len(r.res))
	for i := range r.res {
		st, res := &r.c.steps[i], &r.res[i]
		if !res.executed || res.failed || !st.merges {
			continue
		}
		merged = append(merged, i)
		if st.group != "" {
			out.Set(st.group, res.contrib)
			continue
		}
		obj, _ := res.contrib.(*jsonval.Object)
		for k := range obj.Len() {
			m := obj.At(k)
			out.Set(m.Name, m.Value)
		}
	}
	enc := jsonval.Encoder{Options: jsonval.EncodeOptions{Order: jsonval.OrderInsertion, Numbers: jsonval.NumbersVerbatim}}
	if limit := r.c.maxResponse; limit > 0 {
		enc.Options.MaxBytes = int(min(limit, math.MaxInt))
	}
	data, err := enc.Append(nil, out)
	if err != nil {
		if errors.Is(err, jsonval.ErrOutputTooLarge) {
			return nil, r.composite(errcode.Errorf(CodeStepFailed, "merged body over limits.maxResponseBodyBytes (%d)", r.c.maxResponse))
		}
		return nil, r.composite(errcode.Errorf(CodeStepFailed, "write the merged body: %w", err))
	}
	b := &heldBody{data: data}
	b.acct.Bind(r.c.e.budget, body.Gate, r.o.Stripe)
	if err := b.acct.Reserve(int64(len(data))); err != nil {
		return nil, r.composite(errcode.Wrap(CodeBudget, err))
	}
	cacheControl, vary := r.cacheFields(merged, r.c.e.clk.Now())
	h := http.Header{
		"Content-Type":   {"application/json"},
		"Content-Length": {strconv.Itoa(len(data))},
		"Cache-Control":  {cacheControl},
	}
	if vary != "" {
		h["Vary"] = []string{vary}
	}
	if r.partial {
		h[headerPartial] = []string{"true"}
	}
	upstreamName, endpoint, attempts := r.last()
	return &snapshot.UpstreamResponse{
		Status: http.StatusOK, Header: h, Body: b, Partial: r.partial,
		Upstream: upstreamName, Endpoint: endpoint, Attempts: attempts,
	}, nil
}
