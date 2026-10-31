// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package compile

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
	"github.com/ravindu-rev/ruralz/internal/gateway/upstream"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/internal/statestore"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// attachKey identifies one attachment environment of a Policy: a Filter
// is built per Policy, scope and Phase set, never per Route
// (filter.BuildEnv).
type attachKey struct {
	policy string
	scope  phase.Scope
	phases phase.Set
}

// compareKeys orders attachment keys by Policy name, scope and Phase set.
func compareKeys(a, b attachKey) int {
	return cmp.Or(cmp.Compare(a.policy, b.policy), cmp.Compare(a.scope, b.scope), cmp.Compare(a.phases, b.phases))
}

// attachment is one Policy at one attachment environment that some
// Route's effective chain runs, and what the compile built for it.
type attachment struct {
	key attachKey
	res *hub.Resource
	pol *v1alpha1.Policy
	// timeout is the effective stateStoreTimeout.
	timeout time.Duration
	// gatesBody is set when the attachment gates the request body: it
	// runs in onRequestBody, or is a transform.request whose config needs
	// the body (pipeline.Pipeline.PolicySite).
	gatesBody bool
	// readsResponse is set for a client-scope transform.response whose
	// config needs the response body, or whose when reads it.
	readsResponse bool
	when          expr.Program
	filter        filter.Filter
	consumptive   filter.Consumptive
	metrics       *emit.PolicyMetrics
	// failed is the field and error of a refused build; nil when built.
	failed *refusal
}

// refusal is one compile error at a field of a resource: a finding when
// err carries an RZ code, else a fault of the release.
type refusal struct {
	res  *hub.Resource
	path diag.Path
	err  error
}

// build is the state of one compile.
type build struct {
	c      *Compiler
	in     Input
	v      *hub.Validated
	b      *hub.Bundle
	gw     *v1alpha1.Gateway
	gwRes  *hub.Resource
	loc    locator
	opened resources

	// collect fills these.
	atts      []*attachment
	byKey     map[attachKey]*attachment
	upstreams []string
	// limitPolicies and cachePolicies report a reached ratelimit, quota
	// or ai.token-budget Policy and a reached cache Policy.
	limitPolicies, cachePolicies bool

	stateTimeout time.Duration
	limits       snapshot.Limits
	secrets      secret.Store
	consumers    map[string]*expr.Consumer
	views        map[string]*expr.Route
	builder      expr.Builder
	plan         emit.Plan
	shared       *shared
	carry        carryOver

	// compile fills these.
	set           *upstream.Set
	routes        []*snapshot.Route
	routers       map[string]snapshot.Router
	legChains     map[string]*snapshot.LegChain
	listeners     []*snapshot.Listener
	gateway       snapshot.Gateway
	upstreamConns int
	warnings      []Warning
	findings      diag.List
}

// newBuild starts the compile of in, whose Revision must hold its
// Gateway.
func (c *Compiler) newBuild(in Input) (*build, error) {
	var gwRes *hub.Resource
	for _, r := range in.Validated.Bundle.Resources() {
		if r.Kind == v1alpha1.KindGateway {
			gwRes = r
			break
		}
	}
	gw, ok := hub.Object[v1alpha1.Gateway](gwRes)
	if !ok || gw == nil {
		return nil, fmt.Errorf("%w: the Revision holds no Gateway", ErrInput)
	}
	b := &build{
		c: c, in: in, v: in.Validated, b: in.Validated.Bundle, gw: gw, gwRes: gwRes,
		loc: locator{files: in.Files}, opened: resources{log: c.log},
		stateTimeout: statestore.DefaultTimeout,
	}
	if ss := gw.Spec.StateStore; ss != nil && ss.Timeout != nil {
		b.stateTimeout = time.Duration(*ss.Timeout)
	}
	return b, nil
}

// collect finds what some Route reaches: the attachment of each Policy an
// effective chain runs, client and upstream legs alike, and the Upstreams
// the Routes name in upstreams or a composition step.
func (b *build) collect() {
	b.byKey = map[attachKey]*attachment{}
	ups := map[string]bool{}
	for _, rt := range b.b.Routes() {
		for _, u := range rt.Spec.Upstreams {
			ups[u.Name] = true
		}
		if comp := rt.Spec.Composition; comp != nil {
			for _, s := range comp.Steps {
				ups[s.Upstream] = true
			}
		}
		ch := b.v.Chains[rt.Metadata.Name]
		if ch == nil {
			continue
		}
		for ph := range phase.Count {
			for _, e := range ch.Client[ph] {
				b.attach(e)
			}
		}
		for _, leg := range ch.Legs {
			for ph := range phase.Count {
				for _, e := range leg.Phases[ph] {
					b.attach(e)
				}
			}
		}
	}
	for _, name := range slices.Sorted(maps.Keys(ups)) {
		if _, ok := b.b.Upstream(name); ok {
			b.upstreams = append(b.upstreams, name)
		}
	}
	b.atts = slices.SortedFunc(maps.Values(b.byKey), func(x, y *attachment) int { return compareKeys(x.key, y.key) })
}

// attach records the attachment of entry e once.
func (b *build) attach(e hub.Entry) {
	k := attachKey{policy: e.Policy, scope: e.Scope, phases: e.Phases}
	if b.byKey[k] != nil {
		return
	}
	r, _ := b.b.Get(hub.ID{Kind: v1alpha1.KindPolicy, Name: e.Policy})
	pol, ok := hub.Object[v1alpha1.Policy](r)
	if !ok || pol == nil {
		// Stage I builds chains from the Bundle's Policies only.
		return
	}
	a := &attachment{key: k, res: r, pol: pol, timeout: effectiveTimeout(pol, b.stateTimeout)}
	b.byKey[k] = a
	switch pol.Spec.Type {
	case v1alpha1.PolicyTypeRateLimit, v1alpha1.PolicyTypeQuota, v1alpha1.PolicyTypeAITokenBudget:
		b.limitPolicies = true
	case v1alpha1.PolicyTypeCache:
		b.cachePolicies = true
	default:
	}
}

// shape returns what metric admission needs from the Revision: every
// listener and Route, and only the Upstreams and Policies some Route
// reaches, each Policy with the scopes and Phases it runs in; a Route
// whose chain holds a cache Policy gets the Response Cache label sets.
func (b *build) shape() emit.Shape {
	s := emit.Shape{Upstreams: slices.Clone(b.upstreams)}
	for _, l := range b.gw.Spec.Listeners {
		s.Listeners = append(s.Listeners, l.Name)
	}
	for _, rt := range b.b.Routes() {
		s.Routes = append(s.Routes, rt.Metadata.Name)
		if ch := b.v.Chains[rt.Metadata.Name]; ch != nil && holdsCache(ch) {
			s.CachedRoutes = append(s.CachedRoutes, rt.Metadata.Name)
		}
	}
	byName := map[string]*emit.PolicyShape{}
	for _, a := range b.atts {
		ps := byName[a.key.policy]
		if ps == nil {
			ps = &emit.PolicyShape{Name: a.key.policy, Type: string(a.pol.Spec.Type)}
			byName[a.key.policy] = ps
		}
		ps.Scopes |= phase.Scopes(a.key.scope)
		ps.Phases |= a.key.phases
		ps.Gateway = ps.Gateway || a.key.scope == phase.ScopeGateway
	}
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		s.Policies = append(s.Policies, *byName[name])
	}
	return s
}

// holdsCache reports whether chain ch holds a cache Policy.
func holdsCache(ch *hub.Chain) bool {
	for ph := range phase.Count {
		for _, e := range ch.Client[ph] {
			if e.Type == v1alpha1.PolicyTypeCache {
				return true
			}
		}
	}
	return false
}
