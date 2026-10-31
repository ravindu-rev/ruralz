// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package subst

import (
	"strconv"
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// place is what substitution needs to know about one instance position:
// its schema and the forbidden-position flags accumulated from the
// resource root.
//
// schemaidx owns the forbidden-position rules: place and step mirror
// schemaidx.Info.InSecret and Info.NoSubstitution, and
// TestPositionsFromSchema, which compares both over every string field of
// every kind and dispatch branch of the committed schema, is mandatory: a
// change to those rules in schemaidx fails it until this copy follows.
// The walk cannot use schemaidx.Walk or Lookup instead: Walk's Cursor
// exposes neither the undispatched schema (DispatchValues names the
// members to substitute before an object's dispatch is read) nor a step
// for a member visited ahead of its siblings, and Lookup per scalar would
// re-resolve every path from the root.
type place struct {
	// node is the schema with dispatch applied; nil without schema.
	node *schemaidx.Node
	// inSecret: the node or an ancestor carries x-ruralz-secret.
	inSecret bool
	// ref: an x-ruralz-ref position, or an element or typed-map value of
	// one.
	ref bool
	// cel: an x-ruralz-cel position, or an element or typed-map value of
	// one.
	cel bool
	// envelope names apiVersion, kind or metadata.name; "" elsewhere.
	envelope string
}

// edge says how a position hangs off its parent.
type edge struct {
	// inherit: a list element or typed-map value, which inherits the
	// parent's ref and cel flags as the generator passes them on.
	inherit bool
	// envelope names apiVersion, kind or metadata.name.
	envelope string
}

// step returns the place of a child whose dispatched schema is sel.
func (p place) step(sel *schemaidx.Node, e edge) place {
	out := place{node: sel, inSecret: p.inSecret, envelope: e.envelope}
	if e.inherit {
		out.ref, out.cel = p.ref, p.cel
	}
	if sel != nil {
		kw := sel.Keywords()
		out.inSecret = out.inSecret || kw.Secret
		out.ref = out.ref || kw.Ref != ""
		out.cel = out.cel || kw.CEL != nil
	}
	return out
}

// forbidden reports a position where ${VAR} is RZ-CFG-011.
func (p place) forbidden() bool { return p.inSecret || p.ref || p.cel || p.envelope != "" }

// what names a forbidden position other than an envelope field
// (neverSubstituted names those) for messages.
func (p place) what() string {
	switch {
	case p.inSecret:
		return "a secret field (use secretRef with provider: env)"
	case p.cel:
		return "a CEL expression"
	default:
		return "a reference field (references resolve statically)"
	}
}

// memberSchema returns the schema of member name of an object whose
// dispatched schema is sel, and whether it is a typed-map value.
func memberSchema(sel *schemaidx.Node, name string) (*schemaidx.Node, bool) {
	if c, ok := sel.Property(name); ok {
		return c, false
	}
	if v, ok := sel.Values(); ok {
		return v, true
	}
	return nil, false
}

// memberEdge returns the edge of member name below path (the envelope
// fields apiVersion, kind and metadata.name).
func memberEdge(path diag.Path, name string, value bool) edge {
	e := edge{inherit: value}
	switch {
	case len(path) == 0 && (name == "apiVersion" || name == "kind"):
		e.envelope = name
	case len(path) == 1 && path[0].Kind == diag.ElemField && path[0].Name == "metadata" && name == "name":
		e.envelope = "metadata.name"
	default:
	}
	return e
}

// walker substitutes one resource. path is the key-aware path of the
// current position, pushed and popped as the walk descends (push, pop) and
// copied only into a diagnostic, bounded (here, warnPath). Its list
// elements are computed from the authored entries before they are
// substituted (see Substituter.ApplyWith). levels holds what the walk
// keeps of each element of path for those copies.
type walker struct {
	s      *Substituter
	id     tree.ID
	path   diag.Path
	levels []level
	diags  diag.List
	// resource is the identity every diagnostic of the call names, made
	// for the first one (resourceID).
	resource *diag.ResourceID
	// yield is the call's Yield function; nil yields nothing.
	yield func()
	// quiet holds the scalars that take no further RZ-CFG-013 warning
	// from detect or headerValue: one already warned, or one an error
	// left holding its authored ${ expression.
	quiet map[*tree.Node]bool
	// full is set once the diagnostic beyond Options.MaxDiagnostics was
	// recorded or the output limit was spent; the walk then stops.
	full bool
	// units counts the work units since the last Yield call, and bytes
	// the bytes read or written not yet counted as a unit.
	units, bytes int
	// probe, when set, records how the work is spread (tests).
	probe *probe
}

func (w *walker) loc(p tree.Pos) diag.Location {
	if w.s.opts.Files == nil {
		return diag.Location{}
	}
	return w.s.opts.Files.Location(p)
}

// level is what the walk keeps of one element of walker.path for the
// diagnostics below it.
type level struct {
	// index is the list position of a list element, -1 for an object
	// member.
	index int
	// set is a composite set element the element names by its canonical
	// JSON (itemElem), whose keys and strings credentialAt reads; nil
	// otherwise.
	set *tree.Node
	// shown is the element as a diagnostic shows it (boundElem), made for
	// the first diagnostic below it that needs it, when ok is set.
	shown diag.PathElem
	ok    bool
	// cred says whether the text the element names shows a credential
	// (credentialAt), credUnknown until an RZ-CFG-013 warning below it
	// needs to know.
	cred credState
}

// credState says whether a path element shows a credential.
type credState uint8

// Credential states of a path element.
const (
	credUnknown credState = iota
	credNone
	credShown
)

// push appends e, an element whose level is l, to the current path; pop
// removes the last one.
func (w *walker) push(e diag.PathElem, l level) {
	w.path = append(w.path, e)
	w.levels = append(w.levels, l)
}

func (w *walker) pop() {
	w.path = w.path[:len(w.path)-1]
	w.levels = w.levels[:len(w.levels)-1]
}

// shownAt returns element j of the current path as a diagnostic shows it
// (boundElem), bounding it once for every diagnostic below it.
func (w *walker) shownAt(j int) diag.PathElem {
	l := &w.levels[j]
	if !l.ok {
		l.shown, l.ok = boundElem(w.path[j]), true
	}
	return l.shown
}

// here returns the path of a diagnostic at the current position, extra
// appended, its input text bounded (boundElem): a copy of the current
// path, so the diagnostic keeps it as the walk goes on.
func (w *walker) here(extra ...diag.PathElem) diag.Path {
	out := make(diag.Path, 0, len(w.path)+len(extra))
	for j := range w.path {
		w.tick()
		out = append(out, w.shownAt(j))
	}
	for _, e := range extra {
		w.tick()
		out = append(out, boundElem(e))
	}
	return out
}

// warnPath returns the path of an RZ-CFG-013 warning at the current
// position, extra (schema names) appended, as here returns it, except that
// no element names a credential, since the warning names the detector,
// never the value, and its path must not name the very credential the
// warning is about: a keyed entry or set element whose text shows one
// (credentialAt) is named by its index, and the path stops before an
// object member whose key shows one, at the object holding it. The
// warning's location still names the position. This holds for the
// credential the warning is about, which a set element, a keyed entry's
// key or a mapping key may be, and for one in any enclosing element.
func (w *walker) warnPath(extra ...diag.PathElem) diag.Path {
	out := make(diag.Path, 0, len(w.path)+len(extra))
	for j := range w.path {
		w.tick()
		switch {
		case !w.credentialAt(j):
			out = append(out, w.shownAt(j))
		case w.levels[j].index < 0:
			return out
		default:
			out = append(out, diag.Index(w.levels[j].index))
		}
	}
	for _, e := range extra {
		w.tick()
		out = append(out, boundElem(e))
	}
	return out
}

// credentialAt reports whether element j of the current path names text
// that shows a credential, deciding it once for every warning below it:
// a member's key, a keyed entry's key or a string set element read as
// the credential rules read authored text (shows), or a key or string of
// a composite set element named by its JSON (holds). An index names none.
func (w *walker) credentialAt(j int) bool {
	l := &w.levels[j]
	if l.cred == credUnknown {
		l.cred = credNone
		if w.elementShows(w.path[j], l.set) {
			l.cred = credShown
		}
	}
	return l.cred == credShown
}

// elementShows reports whether path element e names text that shows a
// credential; set is the composite set element e names by its JSON, if
// any.
func (w *walker) elementShows(e diag.PathElem, set *tree.Node) bool {
	switch e.Kind {
	case diag.ElemField:
		return w.shows(e.Name)
	case diag.ElemKeyed:
		return w.shows(e.Key)
	case diag.ElemItem:
		if s, ok := e.Item.(string); ok {
			return w.shows(s)
		}
		return set != nil && w.holds(set)
	default:
		return false
	}
}

// shows reports whether text, read as both credential rules read a
// scalar's authored text (eachRun: escapes resolved, defaults joined to
// their run), holds what a credential detector matches.
func (w *walker) shows(text string) bool {
	found := false
	eachRun(text, w.read, func(r run) bool {
		w.tick()
		found = w.match(r.text) != ""
		return !found
	})
	return found
}

// holds reports whether a key or a string of n, a small composite set
// element, shows a credential.
func (w *walker) holds(n *tree.Node) bool {
	w.tick()
	switch n.Kind {
	case tree.KindString:
		return w.shows(n.Text)
	case tree.KindMap:
		for _, m := range n.Members {
			w.tick()
			if w.shows(m.Key) || w.holds(m.Value) {
				return true
			}
		}
	case tree.KindList:
		for _, it := range n.Items {
			w.tick()
			if w.holds(it) {
				return true
			}
		}
	default:
	}
	return false
}

// report records d unless the budget is spent; the diagnostic beyond
// Options.MaxDiagnostics is the last one recorded and sets full. d names
// the resource as resourceID shows it.
func (w *walker) report(d diag.Diagnostic) {
	if w.full {
		return
	}
	if d.Severity == 0 {
		d.Severity = diag.SeverityError
	}
	if d.Resource == nil && w.id != (tree.ID{}) {
		if w.resource == nil {
			w.resource = resourceID(w.id)
		}
		r := *w.resource
		d.Resource = &r
	}
	w.diags = append(w.diags, d)
	w.full = len(w.diags) > w.s.opts.MaxDiagnostics
}

// envelope reports RZ-CFG-011 for ${ in apiVersion or kind of a resource
// whose schema cannot be selected, and whether it did.
func (w *walker) envelope(root *tree.Node) bool {
	found := false
	for _, name := range [...]string{"apiVersion", "kind"} {
		if v, ok := w.get(root, name); ok && v.Kind == tree.KindString && w.contains(v.Text, "${") {
			w.report(neverSubstituted(name, w.loc(v.Pos), diag.Path{diag.Field(name)}))
			found = true
		}
	}
	return found
}

// neverSubstituted is the RZ-CFG-011 error for ${ in a mapping key or an
// envelope field (apiVersion, kind, metadata.name): substitution never
// reads these, so any ${ there, $${ included, is the error, and the hint
// does not offer $${, which is no escape there.
func neverSubstituted(what string, at diag.Location, path diag.Path) diag.Diagnostic {
	if what == "" {
		return diag.Diagnostic{
			Code: CodeForbidden, Location: at, Path: path,
			Message: "substitution is not allowed in a mapping key",
			Hint:    "a mapping key must not contain ${; keys are never substituted, so $${ is not an escape there",
		}
	}
	return diag.Diagnostic{
		Code: CodeForbidden, Location: at, Path: path,
		Message: "substitution is not allowed in " + what,
		Hint:    what + " must not contain ${; it is never substituted, so $${ is not an escape there",
	}
}

// child visits a member or element value.
func (w *walker) child(n *tree.Node, static *schemaidx.Node, parent place, e edge) {
	w.tick()
	switch n.Kind {
	case tree.KindMap:
		w.visitMap(n, static, parent, e)
	case tree.KindList:
		pl := parent.step(static, e)
		item, _ := static.Items()
		for i, it := range n.Items {
			if w.full {
				return
			}
			if it.Kind != tree.KindString && it.Kind != tree.KindMap && it.Kind != tree.KindList {
				// Nothing below a number, boolean or null is substituted
				// or reported, so its path element is never needed.
				w.tick()
				continue
			}
			e := w.itemElem(static, it, i)
			l := level{index: i}
			if e.Kind == diag.ElemItem && it.Kind != tree.KindString {
				l.set = it
			}
			w.push(e, l)
			w.child(it, item, pl, edge{inherit: true})
			w.pop()
		}
	case tree.KindString:
		w.scalar(n, parent.step(static, e))
	default:
	}
}

// visitMap substitutes an object: keys are checked first, then the string
// members the schema's if/then rules test (a Policy spec.type), so the
// dispatch that selects every other member's schema reads substituted
// values. Outside an x-ruralz-secret subtree, each key that holds no ${
// is read for credentials as any string is (keyWarning).
func (w *walker) visitMap(n *tree.Node, static *schemaidx.Node, parent place, e edge) {
	// keyed holds the members whose key holds ${ (RZ-CFG-011), which take
	// no further diagnostic, and done those visited ahead of the others.
	var keyed, done map[int]bool
	for i, m := range n.Members {
		w.tick()
		if w.full {
			return
		}
		if w.contains(m.Key, "${") {
			w.report(neverSubstituted("", w.loc(m.KeyPos), w.here(diag.Field(m.Key))))
			if keyed == nil {
				keyed = map[int]bool{}
			}
			keyed[i] = true
		}
	}
	var prePlace place
	pre := w.selectOf(static, n)
	slot := w.headerSlot(n, pre)
	for i, m := range n.Members {
		w.tick()
		if m.Value.Kind != tree.KindString || !w.dispatchTested(static, m.Key) {
			continue
		}
		if done == nil {
			done = map[int]bool{}
			prePlace = parent.step(pre, e)
		}
		done[i] = true
		child, value := memberSchema(pre, m.Key)
		edge := memberEdge(w.path, m.Key, value)
		w.push(diag.Field(m.Key), level{index: -1})
		w.scalar(m.Value, prePlace.step(child, edge))
		w.pop()
	}
	sel := pre
	if done != nil {
		sel = w.selectOf(static, n)
	}
	pl := parent.step(sel, e)
	for i, m := range n.Members {
		if w.full {
			return
		}
		cred := credUnknown
		if !pl.inSecret && !keyed[i] {
			cred = credNone
			if w.keyWarning(m) {
				cred = credShown
			}
		}
		if done[i] {
			continue
		}
		child, value := memberSchema(sel, m.Key)
		edge := memberEdge(w.path, m.Key, value)
		w.push(diag.Field(m.Key), level{index: -1, cred: cred})
		w.child(m.Value, child, pl, edge)
		w.pop()
	}
	w.headerValue(n, pl, slot)
}

// keyWarning reports RZ-CFG-013 for a credential-shaped mapping key (a
// credential-shaped literal in any string outside x-ruralz-secret warns,
// and keys are strings) and whether it did. The warning is located at the
// key and names the detector; its path is the object holding the key
// (warnPath), so the warning never repeats the key. visitMap calls it
// outside x-ruralz-secret subtrees for keys without ${, which are text
// written in the Bundle and never substituted, so its answer is also
// whether the key shows a credential (credentialAt) for the warnings below
// it.
func (w *walker) keyWarning(m tree.Member) bool {
	name := w.match(m.Key)
	if name == "" {
		return false
	}
	w.report(diag.Diagnostic{
		Code: CodeWarning, Severity: diag.SeverityWarning, Location: w.loc(m.KeyPos), Path: w.warnPath(),
		Message: "credential-shaped mapping key (" + name + ")",
		Hint:    "keep credentials out of the Bundle: reference a secret with secretRef",
	})
	return true
}

// scalar substitutes one string scalar. A scalar an RZ-CFG-005 or
// RZ-CFG-010 error leaves holding its authored text is still read for
// credentials in its authored runs (as any string outside x-ruralz-secret
// is) and for secret-like names at each well-formed use; a malformed one
// also takes RZ-CFG-010 for each well-formed use of an undefined variable
// (one diagnostic per occurrence), so one run reports every error of the
// scalar. One in a forbidden position takes RZ-CFG-011 alone, and an
// envelope field (metadata.name) takes it for any ${, $${ included.
//
// The text is scanned in passes that allocate nothing (scanEach): one
// checks the grammar, one reports and sizes the result, so nothing is
// built past the output limit, one writes it and one reads the authored
// runs, so a scalar costs time linear in its text and memory linear in
// its result alone.
func (w *walker) scalar(n *tree.Node, pl place) {
	w.tick()
	authored := n.Text
	if !w.contains(authored, "${") {
		w.detect(n, pl)
		return
	}
	if pl.envelope != "" {
		w.report(neverSubstituted(pl.envelope, w.loc(n.Pos), w.here()))
		w.silence(n)
		return
	}
	var lits int64
	exprs, literals := 0, false
	bad := scanEach(authored, false, w.read, func(sg segment) bool {
		w.tick()
		if sg.name == "" {
			lits += int64(len(sg.lit))
			literals = true
		} else {
			exprs++
		}
		return true
	})
	if pl.forbidden() {
		if bad != "" || exprs > 0 {
			w.report(diag.Diagnostic{
				Code: CodeForbidden, Location: w.loc(n.Pos), Path: w.here(),
				Message: "substitution is not allowed in " + pl.what(),
				Hint:    "write $${ for a literal ${",
			})
			w.silence(n)
			return
		}
		if !w.write(lits) {
			return
		}
		n.Text = w.render(authored, lits, nil)
		w.detect(n, pl)
		return
	}
	if bad != "" {
		w.report(diag.Diagnostic{
			Code: CodeMalformed, Location: w.loc(n.Pos), Path: w.here(),
			Message: "malformed substitution (" + bad + "); write $${ for a literal ${",
		})
		w.lenientUses(n, authored)
		w.detectRuns(n, authored)
		w.silence(n)
		return
	}
	size, undefined := w.check(n, authored)
	if w.full {
		return
	}
	if undefined {
		w.detectRuns(n, authored)
		w.silence(n)
		return
	}
	if !w.write(size) {
		return
	}
	var vars []string
	n.Text = w.render(authored, size, &vars)
	n.Vars = vars
	if exprs == 1 && !literals {
		retype(n, pl.node, w.read)
	}
	w.detectRuns(n, authored)
}

// lenientUses reports, for each use in authored, the text of a scalar
// with a malformed ${, what check reports for a use in valid text: the
// RZ-CFG-013 secret-like name warning and, for a use without default
// whose variable is undefined, RZ-CFG-010 with its hint (one diagnostic
// per occurrence; CM "Validation and diff semantics": every error in one
// run). authored is read leniently, as the credential detectors read it
// (eachRun): every well-formed expression is a use, the default of a
// malformed ${A:-${B}} included, so neither the warning nor the error
// depends on which other error the scalar has.
func (w *walker) lenientUses(n *tree.Node, authored string) {
	scanEach(authored, true, w.read, func(sg segment) bool {
		w.tick()
		if w.full {
			return false
		}
		if sg.name == "" {
			return true
		}
		w.secretName(n, sg.name)
		if _, from := w.resolve(sg); from == fromUndefined {
			w.undefined(n, sg.name)
		}
		return true
	})
}

// secretName reports the RZ-CFG-013 warning for a use of name when it
// looks like a secret.
func (w *walker) secretName(n *tree.Node, name string) {
	if secretLike(name, w.read) {
		w.report(diag.Diagnostic{
			Code: CodeWarning, Severity: diag.SeverityWarning, Location: w.loc(n.Pos), Path: w.warnPath(),
			Message: "variable " + strconv.Quote(clip(name)) + " looks like a secret",
			Hint:    "use a secretRef with provider: env instead of substitution",
		})
	}
}

// check reports the secret-like names and undefined variables of
// authored, n's valid text, in order, and returns the size of the
// substituted text and whether a variable is undefined. It stops once the
// diagnostic budget is spent.
func (w *walker) check(n *tree.Node, authored string) (size int64, undefined bool) {
	scanEach(authored, false, w.read, func(sg segment) bool {
		if w.full {
			return false
		}
		if sg.name == "" {
			size += int64(len(sg.lit))
			return true
		}
		w.secretName(n, sg.name)
		v, from := w.resolve(sg)
		if from == fromUndefined {
			w.undefined(n, sg.name)
			undefined = true
			return true
		}
		size += int64(len(v))
		return true
	})
	return size, undefined
}

// render returns the substituted text of authored, valid text whose
// result is size bytes, and records its variables in *vars in first-use
// order when vars is not nil (a text without expressions has none).
func (w *walker) render(authored string, size int64, vars *[]string) string {
	var out strings.Builder
	out.Grow(int(size))
	var seen map[string]struct{}
	scanEach(authored, false, w.read, func(sg segment) bool {
		if sg.name == "" {
			appendText(&out, sg.lit, w.read)
			return true
		}
		v, _ := w.resolve(sg)
		appendText(&out, v, w.read)
		if vars != nil {
			*vars, seen = w.addVar(*vars, seen, sg.name)
		}
		return true
	})
	return out.String()
}

// write counts size bytes of substituted text against the output limit
// of the render (Options.MaxOutputBytes) and reports whether the scalar
// may be written. Once the limit is spent, the call's diagnostics are
// LimitDiagnostic alone and the walk stops.
func (w *walker) write(size int64) bool {
	if w.s.take(size) {
		return true
	}
	w.diags, w.full = diag.List{w.s.LimitDiagnostic()}, true
	return false
}

// addVar appends name to vars unless it is there already, keeping first
// use order: a scan while vars is short, then the set seen, which addVar
// creates, so a scalar with n distinct variables costs O(n).
func (w *walker) addVar(vars []string, seen map[string]struct{}, name string) ([]string, map[string]struct{}) {
	if seen != nil {
		w.tick()
		if _, dup := seen[name]; !dup {
			seen[name] = struct{}{}
			vars = append(vars, name)
		}
		return vars, seen
	}
	for _, v := range vars {
		w.tick()
		if v == name {
			return vars, nil
		}
	}
	vars = append(vars, name)
	if len(vars) > linearScan {
		seen = make(map[string]struct{}, 2*len(vars))
		for _, v := range vars {
			w.tick()
			seen[v] = struct{}{}
		}
	}
	return vars, seen
}

// origin says where the text of one expression comes from.
type origin uint8

// Expression origins.
const (
	// fromSource: the variable is set (and not empty for ${NAME:-default}).
	fromSource origin = iota
	// fromDefault: the authored default of ${NAME:-default}, written in
	// the Bundle.
	fromDefault
	// fromUndefined: no value and no default (RZ-CFG-010).
	fromUndefined
)

// resolve returns the text of expression sg and where it comes from (:-
// treats empty as unset).
func (w *walker) resolve(sg segment) (string, origin) {
	v, ok := w.lookup(sg.name)
	switch {
	case sg.hasDef && (!ok || v == ""):
		return sg.def, fromDefault
	case !ok:
		return "", fromUndefined
	default:
		return v, fromSource
	}
}

func (w *walker) lookup(name string) (string, bool) {
	if w.s.opts.Variables == nil {
		return "", false
	}
	return w.s.opts.Variables.Lookup(name)
}

// undefined reports RZ-CFG-010 with the nearest HintNames entry (hints:
// the answer of diag.Nearest at a cost linear in the name, once per
// distinct name).
func (w *walker) undefined(n *tree.Node, name string) {
	d := diag.Diagnostic{
		Code: CodeUndefined, Location: w.loc(n.Pos), Path: w.here(),
		Message: "variable " + strconv.Quote(clip(name)) + " is not defined and has no default",
	}
	if near, ok := w.s.hints.nearest(name, w.read); ok {
		d.Hint = "did you mean " + strconv.Quote(clip(near)) + "?"
	}
	w.report(d)
}

// detect reports RZ-CFG-013 for a credential-shaped string without
// expressions outside an x-ruralz-secret subtree, naming the detector,
// never the value: all of its text is authored text.
func (w *walker) detect(n *tree.Node, pl place) {
	if pl.inSecret || n.Kind != tree.KindString {
		return
	}
	if name := w.match(n.Text); name != "" {
		w.warn(n, nil, "credential-shaped value ("+name+")")
	}
}

// detectRuns reports RZ-CFG-013 for the first credential-shaped authored
// run of n, whose authored text is authored, naming the detector. The
// runs are the text written in the Bundle (eachRun): the literal text and
// every default, taken or not, so a credential written as a default warns
// whatever the Environment sets, and never a variable's value, so a
// credential from the variable source warns nowhere, after substitution
// as when an error leaves the authored text in place. Each run is read
// alone, so text around an expression without default never forms a match
// across it (the password of "https://user:${PASS}@host" is a variable).
// scalar calls it only outside forbidden positions, so never inside an
// x-ruralz-secret subtree, and whatever type re-typing gave n.
func (w *walker) detectRuns(n *tree.Node, authored string) {
	eachRun(authored, w.read, func(r run) bool {
		if name := w.match(r.text); name != "" {
			w.warn(n, nil, "credential-shaped value ("+name+")")
			return false
		}
		return true
	})
}

// match runs the credential detectors over s, counting the bytes they
// read as they read them.
func (w *walker) match(s string) string { return detect(w.s.detectors, s, w.read) }

// silence keeps n from further RZ-CFG-013 warnings (walker.quiet).
func (w *walker) silence(n *tree.Node) {
	if w.quiet == nil {
		w.quiet = map[*tree.Node]bool{}
	}
	w.quiet[n] = true
}

// warn reports the RZ-CFG-013 warning for a credential in the authored
// text of a scalar, a detector match (detect, detectRuns) or a
// credential header literal (headerValue): at most one per scalar. A
// scalar an error left holding its authored expression takes only a
// detector match on its authored runs, which scalar checks before it
// silences the scalar, never the header literal one. The secret-like
// name warning is separate: scalar reports it at each use of the name,
// so a scalar can carry it once per such expression and this one
// besides. The warning is located at n, its path the current position
// with extra (schema names) appended, without the credential (warnPath):
// a set element or keyed entry that n is, or whose key n is, is named by
// its index.
func (w *walker) warn(n *tree.Node, extra []diag.PathElem, msg string) {
	if w.quiet[n] {
		return
	}
	w.silence(n)
	w.report(diag.Diagnostic{
		Code: CodeWarning, Severity: diag.SeverityWarning, Location: w.loc(n.Pos), Path: w.warnPath(extra...),
		Message: msg,
		Hint:    "keep credentials out of the Bundle: reference a secret with secretRef",
	})
}

// headerSetEntry reports the schema of a headers Policy request or
// response entry.
func headerSetEntry(sel *schemaidx.Node) bool {
	d := sel.Def()
	return d == "HeaderRequestSet" || d == "HeaderResponseSet"
}

// header is a headers Policy request or response set[] entry as
// headerSlot found it before any member was substituted.
type header struct {
	// value is the entry's string value member, nil when there is none.
	value *tree.Node
	// authored is the value's text then, and name the text of the name
	// member then ("" without one).
	authored, name string
}

// headerSlot returns the header entry n when sel, n's schema before any
// member is substituted, is a headers Policy request or response entry
// in a set[] list, with the texts authored there, which substitution then
// rewrites; a header without value elsewhere.
func (w *walker) headerSlot(n *tree.Node, sel *schemaidx.Node) header {
	if len(w.path) < 2 || !headerSetEntry(sel) {
		return header{}
	}
	if parent := w.path[len(w.path)-2]; parent.Kind != diag.ElemField || parent.Name != "set" {
		return header{}
	}
	v, ok := w.get(n, "value")
	if !ok || v.Kind != tree.KindString {
		return header{}
	}
	h := header{value: v, authored: v.Text}
	if name, ok := w.get(n, "name"); ok && name.Kind == tree.KindString {
		h.name = name.Text
	}
	return h
}

// headerValue reports RZ-CFG-013 for a literal value of a credential
// header in a headers Policy request.set[] or response.set[] entry; h is
// the entry as headerSlot found it before substitution (literal says when
// its value is a literal). The header is the one the gateway sends, so its
// name is read after substitution: a name written as ${H} is a credential
// header when H names one. The message then quotes the name as authored,
// never a variable's value. A value an error left unresolved, or one a
// detector already warned about, takes no warning here (warn skips both).
func (w *walker) headerValue(n *tree.Node, pl place, h header) {
	if h.value == nil || pl.inSecret {
		return
	}
	name, ok := w.get(n, "name")
	if !ok || name.Kind != tree.KindString || !credentialHeader(name.Text) {
		return
	}
	if h.value.Text == "" || !w.literal(h.authored) {
		return
	}
	shown := lower(name.Text, w.read)
	if name.Text != h.name {
		shown = clip(h.name)
	}
	w.warn(h.value, []diag.PathElem{diag.Field("value")}, "literal value for the credential header "+strconv.Quote(shown))
}

// literal reports whether authored, a credential header's set[] value as
// written, is text written in the Bundle that may be a credential: an
// authored run, read as detectRuns reads it (literal text and every
// default, taken or not), holds text other than framing, or no expression
// takes its text from the variable source. Framing is a leading "Bearer "
// or "Basic " authentication scheme of the run at the start of the value,
// in any case (RFC 9110 section 11.1), and text holding no letter or digit,
// such as the "; " between two cookies, whether written as literal text or
// as a default: "${A}${S:-; }${B}" and "${P:-Bearer }${T}" are framed
// variables. Any other text, a $${ escape or a cookie name included, may be
// a credential: "hardcoded${EMPTY}" and "${X:-hunter2}" warn whether X is
// set or not. An empty default supplies no text, so "${A}${B:-}" is not a
// literal. A value an error left unresolved is read here too, its undefined
// variables taking their text from no source; warn then drops the warning,
// as it drops one for a value a detector already warned about.
func (w *walker) literal(authored string) bool {
	framed := true
	eachRun(authored, w.read, func(r run) bool {
		text := r.text
		if r.lead {
			text = trimScheme(text)
		}
		framed = !hasAlnum(text, w.read)
		return framed
	})
	if !framed {
		return true
	}
	sourced := false
	scanEach(authored, false, w.read, func(sg segment) bool {
		if sg.name != "" {
			_, from := w.resolve(sg)
			sourced = from == fromSource
		}
		return !sourced
	})
	return !sourced
}
