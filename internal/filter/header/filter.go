// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package header

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/httpfield"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// Type is the Policy type this package implements.
const Type = v1alpha1.PolicyTypeHeaders

// Codes, each registered in internal/errcode.
const (
	// CodeConfig is a config the Policy refuses (package documentation,
	// "Configuration").
	CodeConfig = "RZ-CFG-005"
	// CodeCEL is a valueExpression with a syntax, type or
	// unavailable-variable error.
	CodeCEL = "RZ-CFG-014"
	// CodeCost is a valueExpression whose static cost estimate exceeds the
	// ceiling.
	CodeCost = "RZ-CFG-015"
)

// MaxEntries is the most entries one Policy holds across the set, add and
// remove lists of both sides (target,
// docs/architecture/02-configuration-model.md "Policy"). With
// httpfield.MaxValueBytes it bounds the bytes one Policy adds to a
// message.
const MaxEntries = 32

// AddedRequestBytes is the most bytes cfg's request side adds to a
// request's header block, a headers Policy's share of
// expr.InputCaps.AddedHeaderBytes: each set and add entry counts its name,
// its literal value or httpfield.MaxComputedValueBytes for a computed
// one, and 4 bytes for the separators of its field line. A set entry
// counts whole although it replaces the lines it names, and a remove
// entry counts nothing.
func AddedRequestBytes(cfg *v1alpha1.HeadersConfig) int64 {
	if cfg == nil || cfg.Request == nil {
		return 0
	}
	var n int64
	for _, list := range [][]v1alpha1.HeaderRequestSet{cfg.Request.Set, cfg.Request.Add} {
		for _, e := range list {
			value := int64(httpfield.MaxComputedValueBytes)
			if e.Value != nil {
				value = int64(len(*e.Value))
			}
			n += int64(len(e.Name)) + value + 4
		}
	}
	return n
}

// Error types a Filter names for the span's error.type
// (filter.Result.Err).
const (
	errTypeCEL          = "cel_error"
	errTypeResultType   = "result_type"
	errTypeInvalidValue = "invalid_value"
)

// Sentinel errors.
var (
	// ErrNoCEL reports a Build of a config with a valueExpression without a
	// CEL builder (BuildEnv.CEL).
	ErrNoCEL = errors.New("headers: no CEL builder")
	// errNoMessage is the cause of cannot decide when the Phase has no
	// message to edit.
	errNoMessage = errors.New("headers: no message to edit")
)

// Factory builds headers Filters (filter.Factory).
type Factory struct{}

var _ filter.Factory = Factory{}

// Build compiles a headers config: a config of another type, or one with a
// finding of the package documentation ("Configuration"), is RZ-CFG-005
// naming the first finding, and a valueExpression that does not compile at
// its site RZ-CFG-014 or RZ-CFG-015 as the compiler reports it. The sides
// edit in the client Phases at Gateway and Route scope and in the leg
// Phases at Upstream scope (BuildEnv.Scope). An ended ctx is its error,
// without a code; a valueExpression without BuildEnv.CEL is ErrNoCEL.
// Programs are shared through the snapshot's ProgramSet, so nothing is
// carried over from BuildEnv.Previous.
func (Factory) Build(ctx context.Context, env filter.BuildEnv) (filter.Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg, ok := env.Config.(*v1alpha1.HeadersConfig)
	if !ok || cfg == nil {
		return nil, errcode.Errorf(CodeConfig, "headers: config is %T, want *v1alpha1.HeadersConfig", env.Config)
	}
	if fs := findings(cfg); len(fs) > 0 {
		at := "config"
		if len(fs[0].path) > 0 {
			at += "." + fs[0].path.String()
		}
		return nil, errcode.Errorf(CodeConfig, "headers: %s: %s", at, fs[0].message)
	}
	f := &Filter{reqPhase: phase.OnRequestHeaders, respPhase: phase.OnResponse}
	if env.Scope == phase.ScopeUpstream {
		f.reqPhase, f.respPhase = phase.OnUpstreamRequest, phase.OnUpstreamResponseHeaders
	}
	var err error
	if f.request, err = compileSide(env, expr.PlaceHeadersRequestValue, requestOps(cfg.Request)); err != nil {
		return nil, err
	}
	if f.response, err = compileSide(env, expr.PlaceHeadersResponseValue, responseOps(cfg.Response)); err != nil {
		return nil, err
	}
	return f, nil
}

// compileSide compiles one checked side whose valueExpressions are at
// place.
func compileSide(env filter.BuildEnv, place expr.PlaceID, o ops) (side, error) {
	var s side
	for _, name := range o.remove {
		s.remove = append(s.remove, s.name(name))
	}
	var err error
	if s.set, err = s.compile(env, place, o, fieldSet, o.set); err != nil {
		return side{}, err
	}
	if s.add, err = s.compile(env, place, o, fieldAdd, o.add); err != nil {
		return side{}, err
	}
	return s, nil
}

// compile builds the entries of one set or add list.
func (s *side) compile(env filter.BuildEnv, place expr.PlaceID, o ops, field string, list []op) ([]entry, error) {
	out := make([]entry, 0, len(list))
	for _, e := range list {
		en := entry{key: s.name(e.name)}
		if e.value != nil {
			// A composite literal: length equal to capacity.
			en.lit = []string{*e.value}
			out = append(out, en)
			continue
		}
		if env.CEL == nil {
			return nil, ErrNoCEL
		}
		prg, iss := env.CEL.Compile(site(env, place), e.expression)
		if prg == nil {
			return nil, compileError(o.side, field, e.name, iss)
		}
		en.prog = prg
		s.computed++
		out = append(out, en)
	}
	return out, nil
}

// name returns the canonical key of a checked field name and records it
// among the side's names.
func (s *side) name(n string) string {
	key := http.CanonicalHeaderKey(n)
	if !slices.Contains(s.names, key) {
		s.names = append(s.names, key)
		s.lens |= lenBit(len(key))
	}
	return key
}

// site returns the valueExpression site of place at env's attachment:
// env.Site when set, else the place refined by scope and type alone.
func site(env filter.BuildEnv, place expr.PlaceID) expr.Site {
	if env.Site != nil {
		return env.Site(place)
	}
	return expr.Site{Place: place, Scope: env.Scope, PolicyType: Type}
}

// compileError returns the Build error of a valueExpression the builder
// refused: the first Issue's code, RZ-CFG-015 for the cost ceiling and
// RZ-CFG-014 otherwise, naming the entry.
func compileError(sideField, list, name string, iss []expr.Issue) error {
	code, msg := CodeCEL, "did not compile"
	if len(iss) > 0 {
		msg = iss[0].Message
		if iss[0].Code == CodeCost {
			code = CodeCost
		}
	}
	return errcode.Errorf(code, "headers: config.%s.%s entry %q: valueExpression: %s", sideField, list, name, msg)
}

// Filter is a compiled headers Policy. It is immutable and safe for
// concurrent use; the literal value slices it hands out are shared and
// read-only (package documentation, "Cost").
type Filter struct {
	request, response side
	// reqPhase and respPhase are the Phases the sides edit in: the client
	// Phases, or the leg Phases at Upstream scope.
	reqPhase, respPhase phase.Phase
}

var _ filter.Filter = (*Filter)(nil)

// Handle edits the request in the request side's Phase and the response in
// the response side's, and continues in every other Phase. An attempt that
// got no response is left alone. A CEL runtime error or an invalid
// computed value cannot decide, with the class default code, and leaves
// the message as it was.
func (f *Filter) Handle(ctx context.Context, p phase.Phase, x filter.Exchange) filter.Result {
	switch p {
	case f.reqPhase:
		return f.request.apply(ctx, x)
	case f.respPhase:
		if p == phase.OnUpstreamResponseHeaders {
			if l := x.Leg(); l != nil && l.Response == nil {
				return filter.Next()
			}
		}
		return f.response.apply(ctx, x)
	default:
		return filter.Next()
	}
}

// side is the compiled operations of config.request or config.response.
type side struct {
	// remove holds the canonical keys of remove, in list order.
	remove []string
	// set and add hold the entries of set and add, in list order.
	set, add []entry
	// names holds every canonical key the side names, once each; lens is
	// the lenBit union of their lengths.
	names []string
	lens  uint64
	// computed counts the entries with a valueExpression.
	computed int
}

// entry is one set or add entry.
type entry struct {
	// key is the canonical field name.
	key string
	// lit is a literal value as a one-line field value, shared and
	// read-only; nil for a valueExpression.
	lit []string
	// prog is the compiled valueExpression; nil for a literal value.
	prog expr.Program
}

// apply runs the side on the Phase's message: every valueExpression, then
// remove, set and add.
func (s *side) apply(ctx context.Context, x filter.Exchange) filter.Result {
	if len(s.names) == 0 {
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
	var vals []string
	if s.computed > 0 {
		var err error
		if vals, err = s.evaluate(ctx, x.Vars()); err != nil {
			return filter.Undecided("", err)
		}
	}
	if s.hasVariant(h) {
		s.fold(h)
	}
	s.edit(h, vals)
	return filter.Next()
}

// evaluate runs every valueExpression of the side, set entries first, then
// add entries, and returns the results in one slice. Each result is
// trimmed and checked as a field value.
func (s *side) evaluate(ctx context.Context, v *expr.Vars) ([]string, error) {
	vals := make([]string, s.computed)
	j := 0
	for _, list := range [...][]entry{s.set, s.add} {
		for i := range list {
			e := &list[i]
			if e.prog == nil {
				continue
			}
			raw, err := e.prog.EvalString(ctx, v)
			if err != nil {
				kind := errTypeCEL
				var ee *expr.EvalError
				if errors.As(err, &ee) && ee.Kind == expr.KindResultType {
					kind = errTypeResultType
				}
				return nil, &valueError{key: e.key, kind: kind, err: err}
			}
			val, err := httpfield.NormalizeValue(raw)
			if err != nil {
				return nil, &valueError{key: e.key, kind: errTypeInvalidValue, err: err}
			}
			vals[j] = val
			j++
		}
	}
	return vals, nil
}

// edit applies remove, set and add to h, taking computed values from vals
// in evaluation order. Every slice it stores has no spare capacity, so a
// later append copies it instead of overwriting memory another slice
// shares.
func (s *side) edit(h http.Header, vals []string) {
	for _, k := range s.remove {
		delete(h, k)
	}
	j := 0
	for i := range s.set {
		e := &s.set[i]
		if e.prog == nil {
			h[e.key] = e.lit
			continue
		}
		h[e.key] = vals[j : j+1 : j+1]
		j++
	}
	for i := range s.add {
		e := &s.add[i]
		line := e.lit
		if e.prog != nil {
			line = vals[j : j+1 : j+1]
			j++
		}
		if cur := h[e.key]; len(cur) > 0 {
			h[e.key] = append(slices.Clip(cur), line[0])
		} else {
			h[e.key] = line
		}
	}
}

// hasVariant reports whether h holds one of the side's fields under a
// differently cased key. The length check skips most keys cheaply.
func (s *side) hasVariant(h http.Header) bool {
	for k := range h {
		if s.lens&lenBit(len(k)) == 0 {
			continue
		}
		for _, n := range s.names {
			if len(n) == len(k) && n != k && asciiEqualFold(n, k) {
				return true
			}
		}
	}
	return false
}

// fold merges the differently cased keys of every field the side names
// into the canonical key: its own lines first, then the other keys' lines
// sorted by key. It allocates, so it runs only when hasVariant is true.
func (s *side) fold(h http.Header) {
	var variants []string
	for _, n := range s.names {
		variants = variants[:0]
		for k := range h {
			if len(k) == len(n) && k != n && asciiEqualFold(n, k) {
				variants = append(variants, k)
			}
		}
		if len(variants) == 0 {
			continue
		}
		slices.Sort(variants)
		lines := slices.Clone(h[n])
		for _, k := range variants {
			lines = append(lines, h[k]...)
			delete(h, k)
		}
		if len(lines) == 0 {
			delete(h, n)
			continue
		}
		h[n] = slices.Clip(lines)
	}
}

// valueError is a valueExpression that failed for the field key: a CEL
// runtime error or an invalid computed value. It names the span's
// error.type; its text names the field and the failure, never the value.
type valueError struct {
	key  string
	kind string
	err  error
}

// Error returns the field and the cause's text.
func (e *valueError) Error() string {
	return "headers: valueExpression of " + e.key + ": " + e.err.Error()
}

// Unwrap returns the cause.
func (e *valueError) Unwrap() error { return e.err }

// ErrorType returns the span's error.type: cel_error, result_type or
// invalid_value.
func (e *valueError) ErrorType() string { return e.kind }

// lenBit maps a key length to one bit: bit n for n below 64, bit 0 for 64
// and over (no key or name is empty).
func lenBit(n int) uint64 {
	if n >= 64 {
		return 1
	}
	return 1 << n
}

// asciiEqualFold reports whether a and b are equal ignoring ASCII case;
// unlike strings.EqualFold it never folds a non-ASCII rune, such as the
// Kelvin sign, onto an ASCII letter.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range len(a) {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}
