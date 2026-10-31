// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dotpath

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// The oracle: the operations on a decoded tree (jsonval *Object members in
// their order, numbers verbatim), written by the insertion-order encoder.
// It states the package semantics without the Editor's single-pass byte
// handling, so the differential tests and the fuzz target compare the two.

// oracleResult is one oracle operation's outcome.
type oracleResult struct {
	doc      []byte
	captured []byte
	n        int
	err      error
}

// oracleRun applies op to the normalized document doc, with payload the
// value (set) or elements (append) as transform-form JSON and r the
// replacer.
func oracleRun(tb testing.TB, doc []byte, p Path, op opKind, payload []byte, r Replacer) oracleResult {
	tb.Helper()
	tree, _, err := jsonval.Decode(doc, jsonval.Options{MaxDepth: jsonval.MaxDepthLimit, AllowDuplicateNames: true})
	if err != nil {
		tb.Fatalf("oracle: decode %q: %v", doc, err)
	}
	o := &oracle{p: p, op: op, r: r}
	switch op {
	case opSet:
		o.payload = decodeTree(tb, payload)
	case opAppend:
		o.payload = decodeTree(tb, append(append([]byte("["), payload...), ']'))
	default:
	}
	out, err := o.walk(tree, 0)
	res := oracleResult{n: o.n, err: err}
	if err != nil {
		return res
	}
	enc := jsonval.Encoder{Options: jsonval.EncodeOptions{Order: jsonval.OrderInsertion}}
	if res.doc, err = enc.Append(nil, out); err != nil {
		tb.Fatalf("oracle: encode: %v", err)
	}
	for i, c := range o.capt {
		if i > 0 {
			res.captured = append(res.captured, ',')
		}
		if res.captured, err = enc.Append(res.captured, c); err != nil {
			tb.Fatalf("oracle: encode capture: %v", err)
		}
	}
	return res
}

// decodeTree decodes one transform-form value.
func decodeTree(tb testing.TB, b []byte) any {
	tb.Helper()
	v, _, err := jsonval.Decode(b, jsonval.Options{MaxDepth: jsonval.MaxDepthLimit})
	if err != nil {
		tb.Fatalf("oracle: decode payload %q: %v", b, err)
	}
	return v
}

// oracle walks a tree for one operation.
type oracle struct {
	p       Path
	op      opKind
	r       Replacer
	payload any
	capt    []any
	n       int
}

// walk returns v, whose path matches p's first d segments, with the
// operation applied below it.
func (o *oracle) walk(v any, d int) (any, error) {
	k := o.p.Len()
	seg := o.p.Seg(d)
	last := d+1 == k
	switch x := v.(type) {
	case *jsonval.Object:
		if seg.Kind == Star {
			return nil, &PathError{Reason: StarOnNonArray, Segment: d}
		}
		matched := false
		for i := 0; i < x.Len(); {
			m := x.At(i)
			if m.Name != seg.Name {
				i++
				continue
			}
			matched = true
			if last {
				nv, drop, err := o.target(m.Value, d+1)
				if err != nil {
					return nil, err
				}
				if drop {
					x.DeleteAt(i)
					continue
				}
				x.SetAt(i, nv)
				i++
				continue
			}
			nv, err := o.walk(m.Value, d+1)
			if err != nil {
				return nil, err
			}
			x.SetAt(i, nv)
			i++
		}
		if !matched && (o.op == opSet || o.op == opAppend) {
			for j := d + 1; j < k; j++ {
				if o.p.Seg(j).Kind == Star {
					return x, nil
				}
			}
			val := jsonval.Clone(o.payload)
			for j := k - 1; j > d; j-- {
				obj := jsonval.NewObject(1)
				obj.Set(o.p.Seg(j).Name, val)
				val = obj
			}
			x.Set(seg.Name, val)
			o.n++
		}
		return x, nil
	case []any:
		out := x[:0:0]
		for i, e := range x {
			hit := seg.Kind == Star || (seg.Kind == Index && seg.Index == i)
			switch {
			case !hit:
				out = append(out, e)
			case last:
				nv, drop, err := o.target(e, d+1)
				if err != nil {
					return nil, err
				}
				if !drop {
					out = append(out, nv)
				}
			default:
				nv, err := o.walk(e, d+1)
				if err != nil {
					return nil, err
				}
				out = append(out, nv)
			}
		}
		if o.op == opSet || o.op == opAppend {
			switch {
			case seg.Kind == Name:
				return nil, &PathError{Reason: NameOnArray, Segment: d}
			case seg.Kind == Index && seg.Index >= len(x):
				return nil, &PathError{Reason: IndexPastEnd, Segment: d}
			}
		}
		return out, nil
	default:
		return nil, &PathError{Reason: ThroughScalar, Segment: d}
	}
}

// target acts on a value the whole path matches; drop reports that it is
// removed from its container.
func (o *oracle) target(v any, d int) (nv any, drop bool, err error) {
	switch o.op {
	case opRemove:
		o.n++
		return nil, true, nil
	case opExtract:
		o.capt = append(o.capt, v)
		o.n++
		return nil, true, nil
	case opCollect:
		o.capt = append(o.capt, v)
		o.n++
		return v, false, nil
	case opSet:
		o.n++
		return jsonval.Clone(o.payload), false, nil
	case opReplace:
		s, ok := v.(string)
		if !ok {
			return nil, false, &PathError{Reason: NotString, Segment: d - 1}
		}
		out, changed := o.r.Replace(nil, []byte(s))
		if !changed {
			return v, false, nil
		}
		o.n++
		return string(out), false, nil
	case opAppend:
		a, ok := v.([]any)
		if !ok {
			return nil, false, &PathError{Reason: NotArray, Segment: d - 1}
		}
		o.n++
		return append(a[:len(a):len(a)], jsonval.Clone(o.payload).([]any)...), false, nil
	default:
		return v, false, nil
	}
}
