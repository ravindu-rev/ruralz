// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"encoding/json"
	"errors"
	"io"
)

// Cost accounting. Decode charges every value it builds, before building
// it, at an estimate of the bytes the Go value occupies; Cost computes the
// same figure for an existing tree. A value costs CostValue for its
// interface slot plus:
//
//   - null, true, false: nothing more;
//   - a string or number: CostString plus its decoded bytes;
//   - an array: CostArray plus its elements;
//   - an object: CostObject plus, per member, CostMember, the name bytes and
//     the member value; an *Object of more than 8 members also pays
//     CostIndexEntry per member for its name index (a map[string]any has
//     no separate index).
//
// The cost of a body depends on its density as well as its size. Measured
// ratios of cost to input bytes: about 3 for a typical API document
// (testdata/orders.json), about 10 for an object of short distinct members
// ({"k1":1,"k2":1,...}), 16.5 for an array of one-digit numbers
// ([0,0,...]), 21.3 for an array of empty objects, and never more than
// MaxCostPerByte for any input.
const (
	// CostValue is the interface slot every value takes.
	CostValue = 16
	// CostString is a string or number header.
	CostString = 16
	// CostArray is an array's slice header.
	CostArray = 24
	// CostObject is an object's header.
	CostObject = 48
	// CostMember is a member name's header.
	CostMember = 16
	// CostIndexEntry is one entry of the name index of an *Object with more
	// than 8 members: a map[string]int entry with its share of spare room,
	// 35 to 60 bytes measured on Go 1.27.
	CostIndexEntry = 48
	// MaxCostPerByte bounds the cost of any input relative to its length:
	// Cost(v) <= MaxCostPerByte * len(data) for every text Decode accepts.
	// The bound is reached by a lone one-digit number.
	MaxCostPerByte = CostValue + CostString + 1
)

// Options configures Decode. The zero value is the strict default: depth
// 64, duplicate names rejected, no cost budget, *Object objects.
type Options struct {
	// MaxDepth limits nesting as in ScanOptions.
	MaxDepth int
	// MaxCost is the cost budget; a value that would take the running cost
	// above it fails with ErrTooLarge. Zero or negative means no budget.
	// The specs set the request-path oversize stop at 4 times the raw limit
	// the body arrived under (07 req 58; 03 req 25; 04 req 47, all
	// "target"). Because cost grows with density (see CostValue), a budget
	// of 4 times the raw limit rejects dense bodies well inside that limit:
	// an array of one-digit numbers fails at about a quarter of it. A caller
	// that must accept every body its raw limit admits passes
	// MaxCostPerByte times that limit.
	MaxCost int64
	// AllowDuplicateNames resolves duplicate member names "last wins" (07
	// req 58; 03 req 25): the member keeps its first position and takes the
	// last value. The default rejects them with ErrDuplicateName.
	AllowDuplicateNames bool
	// MapObjects builds objects as map[string]any instead of *Object, for
	// callers that hand trees to code expecting the encoding/json data model
	// (JSON Schema validation, JWT claims).
	MapObjects bool
}

// Decode decodes the single JSON text in data into a tree and returns it
// with its cost. Objects are *Object (or map[string]any with MapObjects),
// arrays []any, numbers json.Number holding the input literal, strings
// string, and true, false and null bool and nil. Failures are *Error values;
// cost is then the charge up to the failure.
func Decode(data []byte, o Options) (v any, cost int64, err error) {
	d := NewDecoder(o)
	return d.Decode(data)
}

// Decoder decodes JSON texts with fixed options, reusing its scanner and
// scratch buffers between calls; the zero Decoder uses the zero Options.
// After each call it drops its reference to the input and every scratch
// structure over about 64 KiB (the unescape buffer and the scanner's
// duplicate-name records and indexes, see Scanner), so a Decoder a caller
// keeps in a sync.Pool never pins a large body (07 req 73).
type Decoder struct {
	opts Options
	s    Scanner
	buf  []byte
	cost int64
}

// NewDecoder returns a Decoder with options o.
func NewDecoder(o Options) *Decoder { return &Decoder{opts: o} }

// Decode decodes data as the package function Decode does.
func (d *Decoder) Decode(data []byte) (v any, cost int64, err error) {
	d.s.Reset(data, ScanOptions{MaxDepth: d.opts.MaxDepth, AllowDuplicateNames: d.opts.AllowDuplicateNames})
	d.cost = 0
	defer d.release()
	t, err := d.s.Next()
	if err != nil {
		return nil, 0, err
	}
	v, err = d.value(t)
	if err != nil {
		return nil, d.cost, err
	}
	// After the top-level value the scanner yields io.EOF or an error.
	if _, err = d.s.Next(); !errors.Is(err, io.EOF) {
		return nil, d.cost, err
	}
	return v, d.cost, nil
}

// release drops references to the input and oversized scratch.
func (d *Decoder) release() {
	d.s.Reset(nil, ScanOptions{})
	if cap(d.buf) > maxScratch {
		d.buf = nil
	}
}

// charge adds n to the running cost, failing at t when over the budget.
func (d *Decoder) charge(n int64, t Token) error {
	d.cost += n
	if d.opts.MaxCost > 0 && d.cost > d.opts.MaxCost {
		return &Error{Kind: ErrTooLarge, Msg: msgTooLarge, Offset: t.Offset, Other: -1}
	}
	return nil
}

// text returns the decoded string of a string or name token after charging
// base plus its length.
func (d *Decoder) text(t Token, base int64) (string, error) {
	raw := d.s.data[t.Offset+1 : t.End-1]
	if t.Escaped {
		d.buf = appendUnescaped(d.buf[:0], raw)
		raw = d.buf
	}
	if err := d.charge(base+int64(len(raw)), t); err != nil {
		return "", err
	}
	return string(raw), nil
}

// value builds the value starting with token t.
func (d *Decoder) value(t Token) (any, error) {
	switch t.Kind {
	case KindNull:
		return nil, d.charge(CostValue, t)
	case KindTrue:
		return true, d.charge(CostValue, t)
	case KindFalse:
		return false, d.charge(CostValue, t)
	case KindNumber:
		lit := d.s.data[t.Offset:t.End]
		if err := d.charge(CostValue+CostString+int64(len(lit)), t); err != nil {
			return nil, err
		}
		return json.Number(lit), nil
	case KindString:
		return d.text(t, CostValue+CostString)
	case KindArrayStart:
		return d.array(t)
	case KindObjectStart:
		if d.opts.MapObjects {
			return d.mapObject(t)
		}
		return d.object(t)
	default:
		// Unreachable: the scanner yields only value starts here.
		return nil, &Error{Kind: ErrSyntax, Msg: msgValueStart, Offset: t.Offset, Other: -1}
	}
}

// array builds an array after its start token.
func (d *Decoder) array(start Token) (any, error) {
	if err := d.charge(CostValue+CostArray, start); err != nil {
		return nil, err
	}
	a := []any{}
	for {
		t, err := d.s.Next()
		if err != nil {
			return nil, err
		}
		if t.Kind == KindArrayEnd {
			return a, nil
		}
		v, err := d.value(t)
		if err != nil {
			return nil, err
		}
		a = append(a, v)
	}
}

// object builds an *Object after its start token. An object of more than
// indexedMembers members leaves with its name index built, so a decoded
// tree is safe for concurrent reads; the index is charged per member before
// it is built.
func (d *Decoder) object(start Token) (any, error) {
	if err := d.charge(CostValue+CostObject, start); err != nil {
		return nil, err
	}
	o := &Object{}
	for {
		nt, name, v, done, err := d.member()
		if err != nil {
			return nil, err
		}
		if done {
			if o.index == nil {
				o.reindex()
			}
			return o, nil
		}
		if d.opts.AllowDuplicateNames {
			// Last wins, in the first position (07 req 58; 03 req 25).
			if i := o.Index(name); i >= 0 {
				o.members[i].Value = v
				continue
			}
		}
		if err := d.charge(indexCost(len(o.members)+1), nt); err != nil {
			return nil, err
		}
		o.members = append(o.members, Member{Name: name, Value: v})
		if d.opts.AllowDuplicateNames {
			// Later names are looked up, so keep the index current. Without
			// duplicates the scanner has checked them, and the index is
			// built once at the end.
			o.indexAppended()
		}
	}
}

// indexCost returns the index charge for growing an *Object to n members:
// all n entries when the index appears, one entry after that.
func indexCost(n int) int64 {
	switch {
	case n <= indexedMembers:
		return 0
	case n == indexedMembers+1:
		return int64(n) * CostIndexEntry
	default:
		return CostIndexEntry
	}
}

// mapObject builds a map[string]any after its start token.
func (d *Decoder) mapObject(start Token) (any, error) {
	if err := d.charge(CostValue+CostObject, start); err != nil {
		return nil, err
	}
	m := map[string]any{}
	for {
		_, name, v, done, err := d.member()
		if err != nil {
			return nil, err
		}
		if done {
			return m, nil
		}
		m[name] = v
	}
}

// member reads one member and returns its name token, or reports the
// object end with done.
func (d *Decoder) member() (nt Token, name string, v any, done bool, err error) {
	nt, err = d.s.Next()
	if err != nil {
		return nt, "", nil, false, err
	}
	if nt.Kind == KindObjectEnd {
		return nt, "", nil, true, nil
	}
	name, err = d.text(nt, CostMember)
	if err != nil {
		return nt, "", nil, false, err
	}
	t, err := d.s.Next()
	if err != nil {
		return nt, "", nil, false, err
	}
	v, err = d.value(t)
	return nt, name, v, false, err
}
