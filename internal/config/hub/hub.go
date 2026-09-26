// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package hub is the validated configuration model: the typed, normalized
// resources of one rendered Bundle, the effective Filter Chain of every
// Route, and the pipeline output a Node compiles and the CLI renders. In
// M1 the hub types are the ruralz/v1alpha1 types; a later apiVersion
// converts into them through internal/config/convert. This package holds
// data only; internal/config/defaults (stage G) and
// internal/config/precedence (stage I) produce it.
package hub

import (
	"cmp"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/expr"
	"github.com/ravindu-rev/ruralz/internal/phase"
	"github.com/ravindu-rev/ruralz/internal/secret"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// ID identifies a resource by kind and metadata.name.
type ID = tree.ID

// KindOrder is the canonical order: Gateway, Upstream, Plugin, Policy,
// Consumer, AIProvider, AIModel, Route; unknown kinds sort last.
func KindOrder(k v1alpha1.Kind) int {
	i := slices.Index([]v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindUpstream, v1alpha1.KindPlugin,
		v1alpha1.KindPolicy, v1alpha1.KindConsumer, v1alpha1.KindAIProvider, v1alpha1.KindAIModel,
		v1alpha1.KindRoute,
	}, k)
	if i < 0 {
		return 99
	}
	return i
}

// CatalogOrder is the display order of the kind catalog: Gateway, Route,
// Upstream, Policy, Plugin, Consumer, AIProvider, AIModel.
func CatalogOrder(k v1alpha1.Kind) int {
	i := slices.Index([]v1alpha1.Kind{
		v1alpha1.KindGateway, v1alpha1.KindRoute, v1alpha1.KindUpstream,
		v1alpha1.KindPolicy, v1alpha1.KindPlugin, v1alpha1.KindConsumer, v1alpha1.KindAIProvider,
		v1alpha1.KindAIModel,
	}, k)
	if i < 0 {
		return 99
	}
	return i
}

// Source records where a resource came from; it is outside the digest.
type Source struct {
	// File is the base (or overlay-added) file, slash-separated; "" for
	// canonical content.
	File string
	// APIVersion is the apiVersion the resource was authored in.
	APIVersion string
	// Start is the document start.
	Start diag.Location
}

// Resource is one validated resource.
type Resource struct {
	ID
	// Labels are metadata.labels.
	Labels map[string]string
	// Annotations are metadata.annotations minus ruralz.io/conversion-data.
	Annotations map[string]string
	// Tree is the normalized envelope after stage G: defaults materialized
	// (StyleDefaulted), scalars and lists normalized, positions kept. The
	// canonical encoder and the diff engine read it.
	Tree *tree.Node
	// Object is the typed hub view: *v1alpha1.Gateway, *v1alpha1.Route,
	// *v1alpha1.Upstream, *v1alpha1.Policy, *v1alpha1.Consumer, and so on.
	Object any
	// Config is the typed Policy config (such as *v1alpha1.RateLimitConfig)
	// for a Policy; nil otherwise.
	Config any
	// Source is where it came from.
	Source Source
}

// Object returns r's typed view when it has type *T.
func Object[T any](r *Resource) (*T, bool) {
	if r == nil {
		return nil, false
	}
	t, ok := r.Object.(*T)
	return t, ok
}

// Bundle is the set of validated resources, sorted in canonical order
// (KindOrder, then name bytewise). It is immutable after NewBundle.
type Bundle struct {
	resources []*Resource
	byID      map[ID]*Resource
}

// NewBundle sorts rs and indexes them; a duplicate identity keeps the first.
func NewBundle(rs []*Resource) *Bundle {
	out := slices.Clone(rs)
	slices.SortStableFunc(out, func(a, b *Resource) int {
		return cmp.Or(cmp.Compare(KindOrder(a.Kind), KindOrder(b.Kind)), cmp.Compare(a.Name, b.Name))
	})
	b := &Bundle{byID: make(map[ID]*Resource, len(out))}
	for _, r := range out {
		if _, dup := b.byID[r.ID]; !dup {
			b.byID[r.ID] = r
			b.resources = append(b.resources, r)
		}
	}
	return b
}

// Resources returns every resource in canonical order.
func (b *Bundle) Resources() []*Resource { return b.resources }

// Get returns the resource with id.
func (b *Bundle) Get(id ID) (*Resource, bool) {
	r, ok := b.byID[id]
	return r, ok
}

// Gateway returns the Gateway, or nil.
func (b *Bundle) Gateway() *v1alpha1.Gateway {
	for _, r := range b.resources {
		if g, ok := Object[v1alpha1.Gateway](r); ok {
			return g
		}
	}
	return nil
}

// Route returns the named Route.
func (b *Bundle) Route(name string) (*v1alpha1.Route, bool) {
	return typed[v1alpha1.Route](b, v1alpha1.KindRoute, name)
}

// Upstream returns the named Upstream.
func (b *Bundle) Upstream(name string) (*v1alpha1.Upstream, bool) {
	return typed[v1alpha1.Upstream](b, v1alpha1.KindUpstream, name)
}

// Policy returns the named Policy.
func (b *Bundle) Policy(name string) (*v1alpha1.Policy, bool) {
	return typed[v1alpha1.Policy](b, v1alpha1.KindPolicy, name)
}

// Consumer returns the named Consumer.
func (b *Bundle) Consumer(name string) (*v1alpha1.Consumer, bool) {
	return typed[v1alpha1.Consumer](b, v1alpha1.KindConsumer, name)
}

// Plugin returns the named Plugin.
func (b *Bundle) Plugin(name string) (*v1alpha1.Plugin, bool) {
	return typed[v1alpha1.Plugin](b, v1alpha1.KindPlugin, name)
}

// Routes returns every Route in name order.
func (b *Bundle) Routes() []*v1alpha1.Route { return all[v1alpha1.Route](b, v1alpha1.KindRoute) }

// Upstreams returns every Upstream in name order.
func (b *Bundle) Upstreams() []*v1alpha1.Upstream {
	return all[v1alpha1.Upstream](b, v1alpha1.KindUpstream)
}

// Policies returns every Policy in name order.
func (b *Bundle) Policies() []*v1alpha1.Policy { return all[v1alpha1.Policy](b, v1alpha1.KindPolicy) }

// Consumers returns every Consumer in name order.
func (b *Bundle) Consumers() []*v1alpha1.Consumer {
	return all[v1alpha1.Consumer](b, v1alpha1.KindConsumer)
}

func typed[T any](b *Bundle, k v1alpha1.Kind, name string) (*T, bool) {
	r, ok := b.byID[ID{Kind: k, Name: name}]
	if !ok {
		return nil, false
	}
	return Object[T](r)
}

func all[T any](b *Bundle, k v1alpha1.Kind) []*T {
	var out []*T
	for _, r := range b.resources {
		if r.Kind == k {
			if t, ok := Object[T](r); ok {
				out = append(out, t)
			}
		}
	}
	return out
}

// Entry is one Policy in an effective chain.
type Entry struct {
	// Policy is the Policy metadata.name.
	Policy string
	// Type is spec.type.
	Type v1alpha1.PolicyType
	// Class is the Filter class (registry, or filterClass for plugin).
	Class phase.Class
	// Slot is the effective slot.
	Slot string
	// Scope is the attachment scope.
	Scope phase.Scope
	// Position is the zero-based index in that scope's spec.policies.
	Position int
	// FailureMode is the materialized failureMode.
	FailureMode v1alpha1.FailureMode
	// Overridable is the materialized spec.overridable.
	Overridable bool
	// Replaces names the Gateway Policy a Route Policy replaced, or "".
	Replaces string
	// Phases are the Phases the Policy runs in at this attachment.
	Phases phase.Set
	// Ref is the position of the attaching PolicyRef.
	Ref diag.Location
}

// FirstPhase returns the earliest Phase the entry runs in.
func (e Entry) FirstPhase() phase.Phase {
	p, _ := e.Phases.First()
	return p
}

// RemovalReason says why a Gateway Policy left a Route's chain.
type RemovalReason uint8

// Removal reasons.
const (
	// Replaced by a Route Policy in the same slot.
	Replaced RemovalReason = iota + 1
	// Excluded by spec.excludePolicies.
	Excluded
)

// Removed is a Gateway Policy absent from a Route's chain.
type Removed struct {
	// Policy is the removed Policy.
	Policy string
	// Slot is its slot.
	Slot string
	// By is the replacing Route Policy, or "" when excluded.
	By string
	// Reason says why.
	Reason RemovalReason
}

// Leg is the upstream-leg chain of one Upstream a Route reaches.
type Leg struct {
	// Upstream is the Upstream name.
	Upstream string
	// Phases holds, per Phase, the Policies in execution order.
	Phases [phase.Count][]Entry
}

// Chain is the effective Filter Chain of one Route. Each Phase holds its
// Policies in execution order: class, scope, position; reversed for
// response Phases; request order for onLog. The data plane builds its
// per-Phase arrays from this value, so render --effective and runtime
// order cannot diverge.
type Chain struct {
	// Route is the Route name.
	Route string
	// Client holds the client-leg Phases.
	Client [phase.Count][]Entry
	// Legs are the upstream legs in first-appearance order.
	Legs []Leg
	// Removed lists Gateway Policies removed, replaced before excluded.
	Removed []Removed
}

// Leg returns the leg of upstream.
func (c *Chain) Leg(upstream string) (*Leg, bool) {
	for i := range c.Legs {
		if c.Legs[i].Upstream == upstream {
			return &c.Legs[i], true
		}
	}
	return nil, false
}

// SecretUse is one secretRef found at an x-ruralz-secret position.
type SecretUse struct {
	// Ref is the reference.
	Ref v1alpha1.SecretRef
	// Kind selects the Node's check.
	Kind secret.Kind
	// Resource and Path locate the field.
	Resource diag.ResourceID
	// Path is the key-aware path.
	Path diag.Path
	// Destination is where the Node transmits the resolved value
	// (secret-to-destination binding, OQ-security-and-identity-22 (a)):
	// DestinationLocal for verification material that never leaves the
	// Node (TLS keys, CA bundles, CRLs, API keys), else the remote origin
	// "https://host:port" (auth.upstream-oauth2 clientSecret: its tokenUrl)
	// or "state-store" (stateStore.url, self-describing). One Ref used with
	// two destinations is RZ-CFG-041 at stage H.
	Destination string
}

// DestinationLocal marks a secret that is never transmitted.
const DestinationLocal = "local"

// PolicyCheck is the offline check of one Policy type's config (R-34). It
// runs at stage H in every binary on a Policy resource whose Config is
// decoded, and reports findings with RZ-CFG codes at key-aware paths under
// spec.config; loc maps such a path to its source location (the resource
// start when unknown). It never resolves secrets, compiles CEL (stage J
// does) or opens a connection.
type PolicyCheck func(r *Resource, loc func(diag.Path) diag.Location, add func(diag.Diagnostic))

// Checks maps a Policy type to its offline check; a type without one is
// absent. Each binary passes internal/filter/builtin/checks.Table() to the
// pipeline, so internal/config never imports internal/filter.
type Checks map[v1alpha1.PolicyType]PolicyCheck

// CELUse is one CEL expression at its site.
type CELUse struct {
	// Site is the place refined by the attachment.
	Site expr.Site
	// Expr is the source text.
	Expr string
	// Resource and Path locate the field.
	Resource diag.ResourceID
	// Path is the key-aware path.
	Path diag.Path
	// Loc is the scalar's source position.
	Loc diag.Location
}

// Validated is the output of the configuration pipeline for one
// Environment: what a Node compiles into a snapshot and what the CLI
// renders, builds and diffs.
type Validated struct {
	// Bundle holds the resources.
	Bundle *Bundle
	// Chains holds the effective chain of every Route, by Route name.
	Chains map[string]*Chain
	// Revision is the canonical content and its digest.
	Revision revision.Revision
	// Secrets lists every secretRef use.
	Secrets []SecretUse
	// CEL lists every expression with its site.
	CEL []CELUse
}
