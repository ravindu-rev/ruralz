// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package transform

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/filter/transform/dotpath"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ErrNoCEL reports a Build of a config with a CEL field without a CEL
// builder (BuildEnv.CEL).
var ErrNoCEL = errors.New("transform: no CEL builder")

// opKind is what one operation does.
type opKind uint8

// Operations, in the order a Policy applies them after config.body:
// arrayOps, then set, remove and replace entries, each list in order.
const (
	opMove opKind = iota + 1
	opAppend
	opDelete
	opSetHeader
	opSetQuery
	opSetPath
	opSetBody
	opRemoveHeader
	opRemoveQuery
	opRemoveBody
	opReplace
	opReplaceRaw
)

// op is one compiled entry.
type op struct {
	kind opKind
	// at names the entry in errors, such as "config.set[2]".
	at string
	// key is the canonical header key or the query name.
	key string
	// path is the body dot path the entry acts on: name, from or path; to
	// is where a move or append writes.
	path, to dotpath.Path
	// prog is a set entry's valueExpression; slot indexes its result among
	// the string results (header, query, path) or the value results
	// (body).
	prog expr.Program
	slot int
	// repl rewrites the strings of a replace entry.
	repl dotpath.Replacer
}

// body reports whether the operation reads or writes the body, so it is
// skipped on a response that cannot have content.
func (o *op) body() bool {
	switch o.kind {
	case opSetHeader, opSetQuery, opSetPath, opRemoveHeader, opRemoveQuery:
		return false
	default:
		return true
	}
}

// Build compiles the transform config s at env's attachment. A config with
// a finding of the package documentation ("Configuration") is RZ-CFG-005
// naming the first finding, and a CEL field that does not compile at its
// site RZ-CFG-014 or RZ-CFG-015 as the compiler reports it; a set entry's
// valueExpression is compiled at env.Site's site narrowed to its target's
// result (expr.TransformValueResult). The Filter runs in the Phase of its
// side and scope (BuildEnv.Scope): transform.request in onRequestBody, or
// onUpstreamRequest at Upstream scope; transform.response in onResponse,
// or onUpstreamResponseBody at Upstream scope. An ended ctx is its error,
// without a code; a CEL field without BuildEnv.CEL is ErrNoCEL. Programs
// are shared through the snapshot's ProgramSet, so nothing is carried
// over from BuildEnv.Previous.
func Build(ctx context.Context, env filter.BuildEnv, s Spec) (*Filter, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	typ := s.Side.PolicyType()
	if fs := findings(&s); len(fs) > 0 {
		at := "config"
		if len(fs[0].path) > 0 {
			at += "." + fs[0].path.String()
		}
		return nil, errcode.Errorf(CodeConfig, "%s: %s: %s", typ, at, fs[0].message)
	}
	upstream := env.Scope == phase.ScopeUpstream
	f := &Filter{
		typ:      typ,
		side:     s.Side,
		legCap:   s.Side == Response && upstream,
		limit:    env.Limits.MaxResponseBodyBytes,
		rewrites: s.rewritesBody(),
		emptyDoc: []byte("{}"),
		jsonLine: []string{DefaultContentType},
	}
	switch {
	case s.Side == Request && upstream:
		f.phase = phase.OnUpstreamRequest
	case s.Side == Request:
		f.phase = phase.OnRequestBody
	case upstream:
		f.phase = phase.OnUpstreamResponseBody
	default:
		f.phase = phase.OnResponse
	}
	if s.Side == Request {
		f.limit = env.Limits.MaxRequestBodyBytes
	}
	// A composite literal: length equal to capacity, so a later append to
	// the field line copies it.
	ct := s.contentType()
	f.ctLine = []string{ct}
	f.ctJSON = jsonval.IsJSONMediaType(ct)
	c := compiler{env: env, side: s.Side, typ: typ}
	if s.Body != "" {
		prg, err := c.compile(s.Side.bodyPlace(), 0, s.Body, "config.body")
		if err != nil {
			return nil, err
		}
		f.body = prg
	}
	for i, a := range s.ArrayOps {
		o := op{at: "config.arrayOps[" + strconv.Itoa(i) + "]", path: mustPath(a.From)}
		switch a.Op {
		case v1alpha1.ArrayOpMove:
			o.kind, o.to = opMove, mustPath(a.To)
		case v1alpha1.ArrayOpAppend:
			o.kind, o.to = opAppend, mustPath(a.To)
		default:
			o.kind = opDelete
		}
		f.ops = append(f.ops, o)
	}
	for i, e := range s.Set {
		o, err := c.set(f, i, e)
		if err != nil {
			return nil, err
		}
		f.ops = append(f.ops, o)
	}
	for i, e := range s.Remove {
		o := op{at: "config.remove[" + strconv.Itoa(i) + "]"}
		switch e.Target {
		case TargetHeader:
			o.kind, o.key = opRemoveHeader, http.CanonicalHeaderKey(e.Name)
		case TargetQuery:
			o.kind, o.key = opRemoveQuery, e.Name
		default:
			o.kind, o.path = opRemoveBody, mustPath(e.Name)
		}
		f.ops = append(f.ops, o)
	}
	for i, r := range s.Replace {
		o := op{at: "config.replace[" + strconv.Itoa(i) + "]", kind: opReplaceRaw}
		if r.Path != "" {
			o.kind, o.path = opReplace, mustPath(r.Path)
		}
		if literal(r) {
			o.repl = &literalReplacer{old: []byte(r.Pattern), repl: []byte(r.Replacement)}
		} else {
			re, err := regexp.Compile(r.Pattern)
			if err != nil {
				return nil, errcode.Errorf(CodeConfig, "%s: %s.pattern: %v", typ, o.at, err)
			}
			o.repl = &patternReplacer{re: re, repl: []byte(r.Replacement)}
		}
		f.ops = append(f.ops, o)
	}
	f.finish()
	return f, nil
}

// mustPath parses a dot path findings has checked.
func mustPath(s string) dotpath.Path {
	p, _ := dotpath.Parse(s)
	return p
}

// compiler compiles the CEL fields of one config.
type compiler struct {
	env  filter.BuildEnv
	side Side
	typ  v1alpha1.PolicyType
}

// set compiles config.set entry i of f's config.
func (c *compiler) set(f *Filter, i int, e SetEntry) (op, error) {
	o := op{at: "config.set[" + strconv.Itoa(i) + "]"}
	result := expr.ResultString
	switch e.Target {
	case TargetHeader:
		o.kind, o.key = opSetHeader, http.CanonicalHeaderKey(e.Name)
		f.lines++
	case TargetQuery:
		o.kind, o.key = opSetQuery, e.Name
	case TargetPath:
		o.kind = opSetPath
	default:
		o.kind, o.path, result = opSetBody, mustPath(e.Name), expr.ResultDyn
	}
	prg, err := c.compile(c.side.valuePlace(), result, e.ValueExpression, o.at+".valueExpression")
	if err != nil {
		return op{}, err
	}
	o.prog = prg
	if o.kind == opSetBody {
		o.slot = f.nval
		f.nval++
	} else {
		o.slot = f.nstr
		f.nstr++
		refs := prg.Refs()
		if (c.side == Request && refs.RequestBody) || (c.side == Response && refs.ResponseBody) {
			f.valuesReadBody = true
		}
	}
	return o, nil
}

// compile compiles src at place: env.Site's site when set, else the place
// refined by scope and type alone, with its result narrowed to result
// when not 0.
func (c *compiler) compile(place expr.PlaceID, result expr.Result, src, at string) (expr.Program, error) {
	if c.env.CEL == nil {
		return nil, ErrNoCEL
	}
	site := expr.Site{Place: place, Scope: c.env.Scope, PolicyType: c.typ}
	if c.env.Site != nil {
		site = c.env.Site(place)
	}
	site.Result = result
	prg, iss := c.env.CEL.Compile(site, src)
	if prg == nil {
		code, msg := CodeCEL, "did not compile"
		if len(iss) > 0 {
			msg = iss[0].Message
			if iss[0].Code == CodeCost {
				code = CodeCost
			}
		}
		return nil, errcode.Errorf(code, "%s: %s: %s", c.typ, at, msg)
	}
	return prg, nil
}

// finish derives what Handle needs from the compiled entries: whether the
// Filter reads the body, and which header fields it may write.
func (f *Filter) finish() {
	f.needsBody = f.rewrites || f.valuesReadBody
	add := func(key string) {
		if !slices.Contains(f.fold, key) {
			f.fold = append(f.fold, key)
			f.lens |= lenBit(len(key))
		}
	}
	for i := range f.ops {
		if o := &f.ops[i]; o.kind == opSetHeader || o.kind == opRemoveHeader {
			add(o.key)
		}
	}
	if f.rewrites {
		add(keyContentType)
		if f.side == Response {
			add(keyETag)
			add(keyAcceptRanges)
		}
	}
}
