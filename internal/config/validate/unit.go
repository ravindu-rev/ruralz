// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// unit is the validation of one resource on one worker.
type unit struct {
	v  *Validator
	ix *bundleIndex
	l  locator
	w  *work
	r  *hub.Resource
	// idx is r's index in canonical order.
	idx int
	f   *finder
	// out numbers the resource's findings.
	out sink
	// secrets are the resource's secretRef uses in document order.
	secrets []hub.SecretUse
}

// validate runs every per-resource rule of r: the schema walk (references
// and secret uses), then the rules of its kind.
func (u *unit) validate() {
	u.walk()
	switch u.r.Kind {
	case v1alpha1.KindGateway:
		u.gateway()
	case v1alpha1.KindRoute:
		u.route()
	case v1alpha1.KindUpstream:
		u.upstream()
	case v1alpha1.KindPolicy:
		u.policy()
	case v1alpha1.KindPlugin:
		u.plugin()
	case v1alpha1.KindConsumer:
		u.consumer()
	default:
	}
}

// report records an error at p, located by the resource's tree.
func (u *unit) report(code string, p diag.Path, msg, hint string, related []diag.Related) {
	u.reportAt(code, p, u.f.path(p), msg, hint, related)
}

// reportAt records an error at p with an explicit location.
func (u *unit) reportAt(code string, p diag.Path, loc diag.Location, msg, hint string, related []diag.Related) {
	u.out.add(diag.Diagnostic{
		Code: code, Severity: diag.SeverityError, Location: loc,
		Resource: u.r.ResourceID(), Path: p, Message: msg, Hint: hint, Related: related,
	})
}

// specPath returns spec followed by elems.
func specPath(elems ...diag.PathElem) diag.Path {
	return append(diag.Path{diag.Field("spec")}, elems...)
}

// walk visits the resource's tree with the hub schema: every x-ruralz-ref
// field is a reference to check (RZ-CFG-009) and every x-ruralz-secret
// field a secretRef use. Free content (open configs, inline JSON Schema
// documents, Plugin configSchema) holds neither and is skipped.
func (u *unit) walk() {
	if u.r.Tree == nil {
		return
	}
	policyType := ""
	if p, ok := hub.Object[v1alpha1.Policy](u.r); ok && p != nil {
		policyType = string(p.Spec.Type)
	}
	_ = u.v.hub.Walk(string(u.r.Kind), u.r.Tree, func(c *schemaidx.Cursor) error {
		u.w.charge(1)
		if c.Info.Node == nil {
			return schemaidx.ErrSkipChildren
		}
		kw := c.Info.Keywords()
		switch {
		case kw.Secret:
			u.secretUse(c, policyType)
			return schemaidx.ErrSkipChildren
		case kw.Ref != "" && c.Node.Kind == tree.KindString:
			u.reference(c, v1alpha1.Kind(kw.Ref))
		default:
		}
		return nil
	})
}

// reference checks one x-ruralz-ref field: its target must be a resource
// of the kind the field implies. A name held by a resource of another
// kind is reported as a wrong-kind target, with that resource as related
// location; any other gets a nearest-name hint. Targets outside the
// Bundle (Environment) are stage C's.
func (u *unit) reference(c *schemaidx.Cursor, target v1alpha1.Kind) {
	if !inBundle(target) {
		return
	}
	name := c.Node.Text
	if _, ok := u.ix.lookup(target, name); ok {
		return
	}
	msg := string(target) + " " + quote(name) + " not found"
	hint := u.ix.names[target].hint(name)
	var related []diag.Related
	if other, ok := u.ix.otherKind(target, name); ok {
		hint = quote(name) + " is " + article(other.Kind) + ", not " + article(target)
		related = []diag.Related{{Location: other.Source.Start, Message: "declared in"}}
	}
	u.reportAt(CodeReference, c.Path(), u.l.position(u.r, c.Node.Pos), msg, hint, related)
}

// article returns the kind with its indefinite article.
func article(k v1alpha1.Kind) string {
	switch k {
	case v1alpha1.KindUpstream, v1alpha1.KindAIModel, v1alpha1.KindAIProvider, v1alpha1.KindEnvironment:
		return "an " + string(k)
	default:
		return "a " + string(k)
	}
}
