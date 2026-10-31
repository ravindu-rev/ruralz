// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package pipeline

import (
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/config/validate"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// failures holds the resources that failed stage F or G. Stages H to K run
// on the Bundle of the resources stage G built, without them, and a
// finding that only restates such a failure is left out
// (docs/architecture/02-configuration-model.md "Validation and diff
// semantics").
type failures struct {
	byID map[tree.ID]*tree.Resource
}

// add records r as failed; the first resource of an identity stays.
func (f *failures) add(r *tree.Resource) {
	if f.byID == nil {
		f.byID = map[tree.ID]*tree.Resource{}
	}
	if _, ok := f.byID[r.ID]; !ok {
		f.byID[r.ID] = r
	}
}

// filter returns the stage H findings of ds that do not restate a failure
// of stage F or G, in order. b is the Bundle stage H validated and idx the
// hub schema index.
func (f *failures) filter(ds diag.List, b *hub.Bundle, idx *schemaidx.Index) diag.List {
	if len(f.byID) == 0 {
		return ds
	}
	out := make(diag.List, 0, len(ds))
	for _, d := range ds {
		if !f.restates(d, b, idx) {
			out = append(out, d)
		}
	}
	return out
}

// restates reports whether stage H finding d holds only because a resource
// failed stage F or G:
//
//   - RZ-CFG-009 at an x-ruralz-ref field whose target, of the kind the
//     field implies, failed;
//   - RZ-CFG-009 at a quota or ai.token-budget config.consumerQuota whose
//     quota a failed Consumer declares;
//   - RZ-CFG-031 and RZ-CFG-032, judged against the default of a Gateway
//     limit, when the Gateway failed with that limit authored.
func (f *failures) restates(d diag.Diagnostic, b *hub.Bundle, idx *schemaidx.Index) bool {
	switch d.Code {
	case validate.CodeReference:
		if d.Resource == nil {
			return false
		}
		r, ok := b.Get(hub.ID{Kind: v1alpha1.Kind(d.Resource.Kind), Name: d.Resource.Name})
		if !ok || r.Tree == nil {
			return false
		}
		n, ok := r.Tree.At(d.Path)
		if !ok || n.Kind != tree.KindString {
			return false
		}
		if info, ok := idx.Lookup(string(r.Kind), r.Tree, d.Path); ok && info.Known() {
			if kind := info.Keywords().Ref; kind != "" {
				_, failed := f.byID[tree.ID{Kind: v1alpha1.Kind(kind), Name: n.Text}]
				return failed
			}
		}
		if r.Kind == v1alpha1.KindPolicy && isPath(d.Path, "spec", "config", "consumerQuota") {
			return f.declaresQuota(n.Text)
		}
		return false
	case validate.CodeSteps:
		return f.gatewayAuthored("maxCompositionSteps")
	case validate.CodeStepBodies:
		return f.gatewayAuthored("maxResponseBodyBytes")
	default:
		return false
	}
}

// declaresQuota reports whether a failed Consumer declares a quota named
// name in its spec.quotas.
func (f *failures) declaresQuota(name string) bool {
	for id, r := range f.byID {
		if id.Kind != v1alpha1.KindConsumer {
			continue
		}
		quotas, ok := r.Root.At(diag.Path{diag.Field("spec"), diag.Field("quotas")})
		if !ok || quotas.Kind != tree.KindList {
			continue
		}
		for _, q := range quotas.Items {
			if n, ok := q.Get("name"); ok && n != nil && n.Kind == tree.KindString && n.Text == name {
				return true
			}
		}
	}
	return false
}

// gatewayAuthored reports whether a failed Gateway authors spec.limits
// member name: a value no default wrote.
func (f *failures) gatewayAuthored(name string) bool {
	for id, r := range f.byID {
		if id.Kind != v1alpha1.KindGateway {
			continue
		}
		if n, ok := r.Root.At(diag.Path{diag.Field("spec"), diag.Field("limits"), diag.Field(name)}); ok && n.Style != tree.StyleDefaulted {
			return true
		}
	}
	return false
}

// isPath reports whether p is the path of the object members names.
func isPath(p diag.Path, names ...string) bool {
	if len(p) != len(names) {
		return false
	}
	for i, e := range p {
		if !isField(e, names[i]) {
			return false
		}
	}
	return true
}
