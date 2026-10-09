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
// schemaidx owns the forbidden-position rules (architecture R-3): place
// and step mirror schemaidx.Info.InSecret and Info.NoSubstitution, and
// TestPositionsFromSchema, which compares both over every string field of
// every kind and dispatch branch of the committed schema, is mandatory: a
// change to those rules in schemaidx fails it until this copy follows.
// The walk cannot use schemaidx.Walk or Lookup instead: Walk's Cursor
// exposes neither the undispatched schema (DispatchValues names the
// members to substitute before an object's dispatch is read) nor a step
// for a member visited ahead of its siblings, and Lookup per scalar would
// re-resolve every path from the root (01 req 54 budget). A contract
// change request asks schemaidx to export one of them.
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

// forbidden reports a position where ${VAR} is RZ-CFG-011 (01 req 28).
func (p place) forbidden() bool { return p.inSecret || p.ref || p.cel || p.envelope != "" }

// what names a forbidden position for messages.
func (p place) what() string {
	switch {
	case p.envelope != "":
		return p.envelope
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

// memberEdge returns the edge of member name below path (01 req 28: the
// envelope fields apiVersion, kind and metadata.name).
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
// current position, pushed and popped as the walk descends and copied
// only into a diagnostic. Its list elements are computed from the
// authored entries before they are substituted (see Substituter.Apply).
type walker struct {
	s     *Substituter
	id    tree.ID
	path  diag.Path
	diags diag.List
	// quiet holds the scalars that take no further RZ-CFG-013 warning
	// from detect or headerValue: one already warned, or one an error
	// left holding its authored ${ expression.
	quiet map[*tree.Node]bool
}

func (w *walker) loc(p tree.Pos) diag.Location {
	if w.s.opts.Files == nil {
		return diag.Location{}
	}
	return w.s.opts.Files.Location(p)
}

// here returns a copy of the current path with extra appended.
func (w *walker) here(extra ...diag.PathElem) diag.Path { return w.path.Append(extra...) }

func (w *walker) report(d diag.Diagnostic) {
	if d.Severity == 0 {
		d.Severity = diag.SeverityError
	}
	if d.Resource == nil && w.id != (tree.ID{}) {
		d.Resource = w.id.ResourceID()
	}
	w.diags = append(w.diags, d)
}

// envelope reports RZ-CFG-011 for ${ in apiVersion or kind of a resource
// whose schema cannot be selected (01 req 16 and 28), and whether it did.
func (w *walker) envelope(root *tree.Node) bool {
	found := false
	for _, name := range [...]string{"apiVersion", "kind"} {
		if v, ok := root.Get(name); ok && v.Kind == tree.KindString && strings.Contains(v.Text, "${") {
			w.report(diag.Diagnostic{
				Code: CodeForbidden, Location: w.loc(v.Pos), Path: diag.Path{diag.Field(name)},
				Message: "substitution is not allowed in " + name,
				Hint:    "write $${ for a literal ${",
			})
			found = true
		}
	}
	return found
}

// child visits a member or element value.
func (w *walker) child(n *tree.Node, static *schemaidx.Node, parent place, e edge) {
	switch n.Kind {
	case tree.KindMap:
		w.visitMap(n, static, parent, e)
	case tree.KindList:
		pl := parent.step(static, e)
		item, _ := static.Items()
		for i, it := range n.Items {
			w.path = append(w.path, schemaidx.ItemElem(static, it, i))
			w.child(it, item, pl, edge{inherit: true})
			w.path = w.path[:len(w.path)-1]
		}
	case tree.KindString:
		w.scalar(n, parent.step(static, e))
	default:
	}
}

// visitMap substitutes an object: keys are checked first, then the string
// members the schema's if/then rules test (a Policy spec.type), so the
// dispatch that selects every other member's schema reads substituted
// values.
func (w *walker) visitMap(n *tree.Node, static *schemaidx.Node, parent place, e edge) {
	for _, m := range n.Members {
		if strings.Contains(m.Key, "${") {
			w.report(diag.Diagnostic{
				Code: CodeForbidden, Location: w.loc(m.KeyPos), Path: w.here(diag.Field(m.Key)),
				Message: "substitution is not allowed in a mapping key",
			})
		}
	}
	var done []bool
	var prePlace place
	pre := static.Select(n)
	for i, m := range n.Members {
		if m.Value.Kind != tree.KindString || len(static.DispatchValues(m.Key)) == 0 {
			continue
		}
		if done == nil {
			done = make([]bool, len(n.Members))
			prePlace = parent.step(pre, e)
		}
		done[i] = true
		child, value := memberSchema(pre, m.Key)
		edge := memberEdge(w.path, m.Key, value)
		w.path = append(w.path, diag.Field(m.Key))
		w.scalar(m.Value, prePlace.step(child, edge))
		w.path = w.path[:len(w.path)-1]
	}
	sel := pre
	if done != nil {
		sel = static.Select(n)
	}
	pl := parent.step(sel, e)
	for i, m := range n.Members {
		if done != nil && done[i] {
			continue
		}
		child, value := memberSchema(sel, m.Key)
		edge := memberEdge(w.path, m.Key, value)
		w.path = append(w.path, diag.Field(m.Key))
		w.child(m.Value, child, pl, edge)
		w.path = w.path[:len(w.path)-1]
	}
	w.headerValue(n, sel, pl)
}

// scalar substitutes one string scalar (01 req 26 to 30).
func (w *walker) scalar(n *tree.Node, pl place) {
	if !strings.Contains(n.Text, "${") {
		w.detect(n, pl)
		return
	}
	segs, bad := scan(n.Text)
	if pl.forbidden() {
		if bad != "" || hasExpression(segs) {
			w.report(diag.Diagnostic{
				Code: CodeForbidden, Location: w.loc(n.Pos), Path: w.here(),
				Message: "substitution is not allowed in " + pl.what(),
				Hint:    "write $${ for a literal ${",
			})
			w.silence(n)
			return
		}
		n.Text = literal(segs)
		w.detect(n, pl)
		return
	}
	if bad != "" {
		w.report(diag.Diagnostic{
			Code: CodeMalformed, Location: w.loc(n.Pos), Path: w.here(),
			Message: "malformed substitution (" + bad + "); write $${ for a literal ${",
		})
		w.silence(n)
		return
	}
	var out strings.Builder
	var vars []string
	exprs, literals, undefined := 0, false, false
	for _, sg := range segs {
		if sg.name == "" {
			out.WriteString(sg.lit)
			literals = true
			continue
		}
		exprs++
		if secretLike(sg.name) {
			w.report(diag.Diagnostic{
				Code: CodeWarning, Severity: diag.SeverityWarning, Location: w.loc(n.Pos), Path: w.here(),
				Message: "variable " + strconv.Quote(sg.name) + " looks like a secret",
				Hint:    "use a secretRef with provider: env instead of substitution",
			})
		}
		v, ok := w.lookup(sg.name)
		switch {
		case sg.hasDef && (!ok || v == ""):
			v = sg.def
		case !ok:
			w.undefined(n, sg.name)
			undefined = true
			continue
		}
		out.WriteString(v)
		if !contains(vars, sg.name) {
			vars = append(vars, sg.name)
		}
	}
	if undefined {
		w.silence(n)
		return
	}
	n.Text, n.Vars = out.String(), vars
	if exprs == 1 && !literals {
		retype(n, pl.node)
	}
	if n.Kind == tree.KindString {
		w.detect(n, pl)
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func (w *walker) lookup(name string) (string, bool) {
	if w.s.opts.Variables == nil {
		return "", false
	}
	return w.s.opts.Variables.Lookup(name)
}

// undefined reports RZ-CFG-010 with the nearest HintNames entry.
func (w *walker) undefined(n *tree.Node, name string) {
	d := diag.Diagnostic{
		Code: CodeUndefined, Location: w.loc(n.Pos), Path: w.here(),
		Message: "variable " + strconv.Quote(name) + " is not defined and has no default",
	}
	if near, ok := diag.Nearest(name, w.s.opts.HintNames); ok {
		d.Hint = "did you mean " + strconv.Quote(near) + "?"
	}
	w.report(d)
}

// detect reports RZ-CFG-013 for a credential-shaped string outside an
// x-ruralz-secret subtree, naming the detector, never the value.
func (w *walker) detect(n *tree.Node, pl place) {
	if pl.inSecret || n.Kind != tree.KindString {
		return
	}
	name := detect(w.s.detectors, n.Text)
	if name == "" {
		return
	}
	w.warn(n, w.here(), "credential-shaped value ("+name+")")
}

// silence keeps n from further RZ-CFG-013 warnings (walker.quiet).
func (w *walker) silence(n *tree.Node) {
	if w.quiet == nil {
		w.quiet = map[*tree.Node]bool{}
	}
	w.quiet[n] = true
}

// warn reports one RZ-CFG-013 warning per scalar.
func (w *walker) warn(n *tree.Node, path diag.Path, msg string) {
	if w.quiet[n] {
		return
	}
	w.silence(n)
	w.report(diag.Diagnostic{
		Code: CodeWarning, Severity: diag.SeverityWarning, Location: w.loc(n.Pos), Path: path,
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

// headerValue reports RZ-CFG-013 for a literal value of a credential
// header in a headers Policy request.set[] or response.set[] entry (01 J
// row 013). A value that came from a variable (Vars set) is not a
// literal: the secret-like name rule and the detectors cover it. A value
// an error left unresolved is not one either.
func (w *walker) headerValue(n *tree.Node, sel *schemaidx.Node, pl place) {
	if pl.inSecret || len(w.path) < 2 || !headerSetEntry(sel) {
		return
	}
	if parent := w.path[len(w.path)-2]; parent.Kind != diag.ElemField || parent.Name != "set" {
		return
	}
	name, ok := n.Get("name")
	if !ok || name.Kind != tree.KindString || !credentialHeader(name.Text) {
		return
	}
	v, ok := n.Get("value")
	if !ok || v.Kind != tree.KindString || v.Text == "" || len(v.Vars) > 0 || w.quiet[v] {
		return
	}
	w.warn(v, w.here(diag.Field("value")),
		"literal value for the credential header "+strconv.Quote(strings.ToLower(name.Text)))
}
