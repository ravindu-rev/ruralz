// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// builder assembles a hub.Bundle from typed objects, without trees, the
// way unit, property and scale tests need it.
type builder struct {
	reg *registry.Registry
	rs  []*hub.Resource
}

func newBuilder() *builder { return &builder{reg: registry.New()} }

func refs(names []string) []v1alpha1.PolicyRef {
	if len(names) == 0 {
		return nil
	}
	out := make([]v1alpha1.PolicyRef, len(names))
	for i, n := range names {
		out[i] = v1alpha1.PolicyRef{Name: n}
	}
	return out
}

func (b *builder) add(k v1alpha1.Kind, name string, obj any) *hub.Resource {
	r := &hub.Resource{ID: hub.ID{Kind: k, Name: name}, Object: obj}
	b.rs = append(b.rs, r)
	return r
}

// gateway adds Gateway edge attaching policies.
func (b *builder) gateway(policies ...string) {
	g := &v1alpha1.Gateway{Metadata: v1alpha1.ObjectMeta{Name: "edge"}}
	g.Spec.Policies = refs(policies)
	b.add(v1alpha1.KindGateway, "edge", g)
}

// route adds a Route with policies, excludes and upstreams.
func (b *builder) route(name string, policies, excludes, upstreams []string) *v1alpha1.Route {
	r := &v1alpha1.Route{Metadata: v1alpha1.ObjectMeta{Name: name}}
	r.Spec.Policies, r.Spec.ExcludePolicies = refs(policies), refs(excludes)
	for _, u := range upstreams {
		r.Spec.Upstreams = append(r.Spec.Upstreams, v1alpha1.RouteUpstream{Name: u})
	}
	b.add(v1alpha1.KindRoute, name, r)
	return r
}

// upstream adds an Upstream attaching policies.
func (b *builder) upstream(name string, policies ...string) {
	u := &v1alpha1.Upstream{Metadata: v1alpha1.ObjectMeta{Name: name}}
	u.Spec.Policies = refs(policies)
	b.add(v1alpha1.KindUpstream, name, u)
}

// policy adds a Policy of typ with a typed config (nil for the type's
// zero config); mods edit the spec before the registry defaults are
// materialized.
func (b *builder) policy(name string, typ v1alpha1.PolicyType, cfg any, mods ...func(*v1alpha1.PolicySpec)) *v1alpha1.Policy {
	p := &v1alpha1.Policy{Metadata: v1alpha1.ObjectMeta{Name: name}, Spec: v1alpha1.PolicySpec{Type: typ}}
	for _, m := range mods {
		m(&p.Spec)
	}
	b.reg.Materialize(p)
	if cfg == nil {
		cfg, _ = b.reg.NewConfig(typ)
	}
	r := b.add(v1alpha1.KindPolicy, name, p)
	r.Config = cfg
	return p
}

// plugin adds a Plugin with phases.
func (b *builder) plugin(name string, phases ...v1alpha1.Phase) {
	pl := &v1alpha1.Plugin{Metadata: v1alpha1.ObjectMeta{Name: name}}
	pl.Spec.Phases = phases
	b.add(v1alpha1.KindPlugin, name, pl)
}

func (b *builder) bundle() *hub.Bundle { return hub.NewBundle(b.rs) }

// Spec modifiers.

func slot(s string) func(*v1alpha1.PolicySpec) { return func(p *v1alpha1.PolicySpec) { p.Slot = s } }

func locked(p *v1alpha1.PolicySpec) {
	f := false
	p.Overridable = &f
}

func pluginOf(name string, class v1alpha1.FilterClass) func(*v1alpha1.PolicySpec) {
	return func(p *v1alpha1.PolicySpec) {
		p.Plugin = name
		p.FilterClass = &class
	}
}

func failOpen(p *v1alpha1.PolicySpec) {
	m := v1alpha1.FailureModeOpen
	p.FailureMode = &m
}

// Typed configs.

func headersCfg(request, response bool) *v1alpha1.HeadersConfig {
	c := &v1alpha1.HeadersConfig{}
	if request {
		c.Request = &v1alpha1.HeaderRequestOps{Set: []v1alpha1.HeaderRequestSet{{Name: "x-req"}}}
	}
	if response {
		c.Response = &v1alpha1.HeaderResponseOps{Set: []v1alpha1.HeaderResponseSet{{Name: "x-resp"}}}
	}
	return c
}

func authzCfg(rule string) *v1alpha1.AuthzCELConfig { return &v1alpha1.AuthzCELConfig{Rule: rule} }

func transformReqCfg(body string) *v1alpha1.TransformRequestConfig {
	return &v1alpha1.TransformRequestConfig{Body: body}
}
