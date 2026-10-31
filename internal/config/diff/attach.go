// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"strings"

	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// side names the state a value belongs to.
type side uint8

// Sides.
const (
	fromSide side = iota
	toSide
)

// policyRef is the x-ruralz-ref target kind of a Policy attachment: the
// name of a PolicyRef in spec.policies or spec.excludePolicies.
const policyRef = string(v1alpha1.KindPolicy)

// classImpact returns the impact class of a Policy in Filter class c, the
// base of its type class (docs/architecture/02-configuration-model.md
// "Diff semantics"): cors, auth, authz, validation and upstream-auth guard who
// may call and with which credentials, so they are security; admission
// and cache shape the traffic that passes, so they are traffic, as
// transform is; custom is a Plugin's own class, so it is plugin.
func classImpact(c phase.Class) schemaidx.Impact {
	switch c {
	case phase.ClassCORS, phase.ClassAuth, phase.ClassAuthz, phase.ClassValidation, phase.ClassUpstreamAuth:
		return schemaidx.ImpactSecurity
	case phase.ClassAdmission, phase.ClassCache, phase.ClassTransform:
		return schemaidx.ImpactTraffic
	case phase.ClassCustom:
		return schemaidx.ImpactPlugin
	default:
		return 0
	}
}

// aiTypePrefix starts the Policy types of the AI/LLM gateway (ai.*).
const aiTypePrefix = "ai."

// typeImpact returns the class of a Policy's type: the class of its Filter
// class, which the registry gives (a plugin Policy's own spec.filterClass),
// plus plugin for a plugin Policy, whose Plugin's code then runs, and ai for
// an ai.* Policy. An attachment of the Policy, a change to its spec, and
// its addition or removal carry it.
func (c *comparer) typeImpact(p *v1alpha1.Policy) schemaidx.Impact {
	var impact schemaidx.Impact
	if e, ok := c.reg.Lookup(p.Spec.Type); ok {
		impact = classImpact(e.EffectiveClass(p))
	}
	switch {
	case p.Spec.Type == v1alpha1.PolicyTypePlugin:
		impact |= schemaidx.ImpactPlugin
	case strings.HasPrefix(string(p.Spec.Type), aiTypePrefix):
		impact |= schemaidx.ImpactAI
	}
	return impact
}

// resourceImpact returns the class every change to r's spec, and r's
// addition or removal, carries: a Policy's type class, plugin for a Plugin
// (its spec is sandboxed code and what it may do), nothing for another
// kind.
func (c *comparer) resourceImpact(r *hub.Resource) schemaidx.Impact {
	if r.Kind == v1alpha1.KindPlugin {
		return schemaidx.ImpactPlugin
	}
	if p, ok := hub.Object[v1alpha1.Policy](r); ok && p != nil {
		return c.typeImpact(p)
	}
	return 0
}

// policyKey caches the class of one Policy on one side.
type policyKey struct {
	side side
	name string
}

// policyImpact returns the impact class of attaching the Policy name as
// side s holds it: its type class. A name s does not hold, which stage H
// refuses, carries none.
func (c *comparer) policyImpact(s side, name string) schemaidx.Impact {
	key := policyKey{side: s, name: name}
	if i, ok := c.classes[key]; ok {
		return i
	}
	var impact schemaidx.Impact
	if b := c.bundles[s]; b != nil {
		if p, ok := b.Policy(name); ok && p != nil {
			impact = c.typeImpact(p)
		}
	}
	c.classes[key] = impact
	return impact
}

// attached returns the impact class of v when it is a Policy reference at
// p, else nothing.
func (c *comparer) attached(v *tree.Node, p pos) schemaidx.Impact {
	if v.Kind != tree.KindString || c.kw(p.s).ref != policyRef {
		return 0
	}
	return c.policyImpact(p.side, v.Text)
}

// refImpact returns the classes of the Policies attached at or below v,
// which a moved entry carries: moving an attachment reorders that
// Policy, whatever else its entry holds.
func (c *comparer) refImpact(v *tree.Node, p pos) schemaidx.Impact {
	impact := c.attached(v, p)
	switch v.Kind {
	case tree.KindMap:
		for _, m := range v.Members {
			if m.Value != nil {
				impact |= c.refImpact(m.Value, c.member(p, m.Key, m.Value))
			}
		}
	case tree.KindList:
		for _, it := range v.Items {
			if it != nil {
				impact |= c.refImpact(it, c.item(p, it))
			}
		}
	default:
	}
	return impact
}
