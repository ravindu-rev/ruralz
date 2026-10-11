// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package defaults is stage G of the configuration pipeline (architecture
// 3.1 and R-8; spec 02 sections 2.2 to 2.4, spec 01 group I): it turns
// each resource that passed schema validation into a hub.Resource and the
// resources of one rendered Bundle into a hub.Bundle.
//
// For each resource, in order:
//
//  1. Materialize writes the defaults under the schema of the resource's
//     own apiVersion: every +ruralz:default the rendered schema declares,
//     only inside objects that are present (an absent optional object is
//     never created, 02 req 9), inside list entries, typed-map values and
//     a Policy config through its type dispatch (02 req 11), plus the
//     registry defaults slot, failureMode and filterClass of every Policy
//     (02 req 12, R-7). The mechanism is table-free: a new marker needs no
//     code change (02 req 10).
//  2. internal/config/convert converts the tree to the hub representation
//     and strips the conversion-data annotation (02 req 4 to 7).
//  3. Normalize rewrites values to one spelling under the hub schema
//     (02 req 15 to 20, 01 req 37): Duration, ByteSize and Decimal values
//     by their definition, integral numbers at integer positions as
//     integer text, host names in Route match.hosts and Gateway
//     listeners[].hostnames in ASCII lower case, set lists sorted by their
//     elements' RFC 8785 bytes and map lists by key, object members in
//     RFC 8785 order, everything else (orderedMap and atomic lists, free
//     content) as authored. It reports RZ-CFG-005 for a duration that
//     overflows, a byte size that does not parse, an integer outside
//     ±(2^53−1) or a number outside the double range after normalization
//     (02 req 16, R-64; not for canonical content, whose RFC 8785 doubles
//     may print as large integers), and for duplicates that only
//     normalization reveals, such as 90s and 1m30s in one set.
//  4. Decode decodes the tree into its pkg/config/v1alpha1 kind struct and
//     a Policy's config into the registry's config type, reporting a value
//     that does not fit its Go field as RZ-CFG-005 at the field (01 req
//     38). Large values are decoded in pieces of at most 256 nodes, each
//     one encoding/json call, with the result of one call over the whole
//     tree, so the worker can yield between pieces.
//
// Every step is idempotent: running stage G on its own output, or on a
// decoded canonical document, changes nothing (02 req 20, R-47). Presence
// is kept: absent fields stay absent, empty objects and lists and empty
// strings stay (02 req 19).
//
// Free content is data. Undeclared members of +ruralz:open configs, plugin
// configs, Plugin configSchema and the inline JSON Schema document of a
// validation.json-schema Policy (the JSONSchemaDocument definition) get no
// defaults, keep every list in authored order and have their members
// sorted and numbers checked like any other (02 req 17 and 18).
//
// JSON null never reaches the hub (02 req 19): an overlay null removed
// its field before stage F, and any null left, in typed or free content,
// the JSONSchemaDocument value included, is RZ-CFG-005, because the
// canonical form has no null.
//
// Diagnostics are bounded per call, not per resource (01 req 50): Run's
// workers share one budget of MaxDiagnostics, stop taking resources once
// it is spent and report RZ-CFG-001 "too many diagnostics", so a hostile
// Bundle cannot build an unbounded list.
//
// Stage values are immutable and safe for concurrent use; the functions
// rewrite the trees they are given in place. The package never logs,
// reads files or the network, and reads the clock only for the yield
// timer (01 req 55): a worker counts its work units (nodes visited and
// sort comparisons) across every step and every resource, reads the
// timer every 256 units, between resources and around each encoding/json
// call, and yields the processor once 100 µs of work have passed since
// its last yield (01 req 53).
package defaults

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// RZ codes of stage G (registered in internal/errcode).
const (
	// CodeSchema is RZ-CFG-005: a value normalization or the typed decode
	// rejects (02 req 15 to 17, 01 req 38).
	CodeSchema = "RZ-CFG-005"
	// CodeUnserved is RZ-CFG-007: a resource whose apiVersion has no
	// schema or converter, or whose kind has no hub type. Stage C reports
	// these first; stage G repeats the check for resources that re-enter
	// at stage F (02 req 4).
	CodeUnserved = "RZ-CFG-007"
	// CodeLimit is RZ-CFG-001: the run reached MaxDiagnostics (01 req 50).
	CodeLimit = "RZ-CFG-001"
)

// MaxDiagnostics bounds the diagnostics one call records: one Run with
// all its workers, or one Resource or Normalize call (01 req 50, 10,000
// per run, proposed). Run and Resource then append one RZ-CFG-001 "too
// many diagnostics" error, so they return at most MaxDiagnostics+1.
const MaxDiagnostics = 10_000

// defJSONSchemaDocument is the $defs name of the inline JSON Schema
// document (validation.json-schema config.schema): opaque data, below
// which every position is free (contract change request 144).
const defJSONSchemaDocument = "JSONSchemaDocument"

// ErrOptions marks every error New returns.
var ErrOptions = errors.New("defaults: invalid options")

// Options configure a Stage.
type Options struct {
	// Schemas indexes the rendered schema of every served apiVersion; it
	// must serve convert.HubAPIVersion and every apiVersion Converters
	// serves.
	Schemas *schemaidx.Set
	// Registry is the Policy type registry.
	Registry *registry.Registry
	// Converters is the conversion registry; nil means convert.Default().
	Converters *convert.Registry
}

// Stage runs stage G. It is immutable and safe for concurrent use.
type Stage struct {
	schemas *schemaidx.Set
	hub     *schemaidx.Index
	reg     *registry.Registry
	conv    *convert.Registry
}

// New returns a Stage, or an error wrapping ErrOptions when a required
// option is missing or the schemas do not cover the converters.
func New(o Options) (*Stage, error) {
	if o.Schemas == nil || o.Registry == nil {
		return nil, fmt.Errorf("%w: Schemas and Registry are required", ErrOptions)
	}
	conv := o.Converters
	if conv == nil {
		conv = convert.Default()
	}
	hubIdx, ok := o.Schemas.Index(convert.HubAPIVersion)
	if !ok {
		return nil, fmt.Errorf("%w: no schema for the hub apiVersion %s", ErrOptions, convert.HubAPIVersion)
	}
	for _, v := range conv.Served() {
		if _, ok := o.Schemas.Index(v); !ok {
			return nil, fmt.Errorf("%w: no schema for converted apiVersion %s", ErrOptions, v)
		}
	}
	return &Stage{schemas: o.Schemas, hub: hubIdx, reg: o.Registry, conv: conv}, nil
}

// HubIndex returns the schema index of the hub representation.
func (s *Stage) HubIndex() *schemaidx.Index { return s.hub }

// Resource runs stage G on res, a resource that passed stage F: defaults
// under its own apiVersion's schema, conversion to the hub, normalization
// and typed decode. It rewrites res.Root in place and returns the
// hub.Resource holding it, or nil when it reported an error. files gives
// source locations and the file role (nil: line and column only, and the
// resource counts as authored content); diagnostics carry res's identity
// and come in tree order, at most MaxDiagnostics of them and then one
// RZ-CFG-001.
func (s *Stage) Resource(res *tree.Resource, files *tree.FileTable) (*hub.Resource, diag.List) {
	w := newWork(nil)
	h, ds := s.resource(res, files, w)
	return h, limit(ds, w.spent())
}

// resource is Resource on worker w, whose budget may drop diagnostics: it
// returns nil whenever res has an error, recorded or not.
func (s *Stage) resource(res *tree.Resource, files *tree.FileTable, w *work) (*hub.Resource, diag.List) {
	if res == nil {
		return nil, nil
	}
	if res.Root == nil || res.Root.Kind != tree.KindMap {
		return nil, w.admit(diag.List{{
			Code: CodeSchema, Severity: diag.SeverityError, Location: location(files, res.Start),
			Resource: res.ID.ResourceID(), Message: "must be an object",
		}})
	}
	src, ok := s.schemas.Index(res.APIVersion)
	if !ok || !s.conv.Serves(res.APIVersion) {
		d, _ := s.conv.CheckServed(res.APIVersion)
		if d.Code == "" {
			d = diag.Diagnostic{
				Code: CodeUnserved, Severity: diag.SeverityError, Path: diag.Path{diag.Field("apiVersion")},
				Message: fmt.Sprintf("apiVersion %q has no schema in this release", res.APIVersion),
			}
		}
		return nil, w.admit(s.own(res, files, diag.List{d}))
	}
	materialize(src, s.reg, res, w)
	root, ds := s.conv.ToHub(res.APIVersion, string(res.ID.Kind), res.Root, false)
	failed := ds.HasErrors()
	ds = w.admit(s.own(res, files, ds))
	if failed {
		return nil, ds
	}
	h := *res
	h.Root, h.APIVersion = root, convert.HubAPIVersion
	more, failed := normalize(s.hub, &h, files, w)
	ds = append(ds, more...)
	if failed {
		return nil, ds
	}
	obj, cfg, more, failed := decode(s.hub, s.reg, &h, files, w)
	ds = append(ds, more...)
	if failed {
		return nil, ds
	}
	out := &hub.Resource{
		ID: res.ID, Tree: root, Object: obj, Config: cfg,
		Source: hub.Source{File: sourceFile(files, res.Start), APIVersion: res.APIVersion, Start: location(files, res.Start)},
	}
	if m := metaOf(obj); m != nil {
		out.Labels, out.Annotations = m.Labels, m.Annotations
	}
	return out, ds
}

// own fills the resource identity and, when unknown, the location of the
// apiVersion member into diagnostics a converter returned.
func (s *Stage) own(res *tree.Resource, files *tree.FileTable, ds diag.List) diag.List {
	for i := range ds {
		if ds[i].Resource == nil {
			ds[i].Resource = res.ID.ResourceID()
		}
		if ds[i].Location == (diag.Location{}) {
			at := res.Start
			if v, ok := res.Root.Get("apiVersion"); ok && v.Pos.Known() {
				at = v.Pos
			}
			ds[i].Location = location(files, at)
		}
	}
	return ds
}

// Run runs Resource on every resource of rs with at most workers
// goroutines (fewer than 1 means 1), created and joined within the call
// (01 req 53), and returns the hub.Bundle of the results with the sorted
// diagnostics of every resource (02 req 2: every finding of the stage in
// one run). The Bundle is nil when any error was reported.
//
// The workers share one budget of MaxDiagnostics (01 req 50). Once it is
// spent they take no further resource, and Run returns at most
// MaxDiagnostics diagnostics plus one RZ-CFG-001 "too many diagnostics"
// error. Below the cap the result does not depend on the worker count;
// at the cap, which diagnostics are kept may, as with diag.Collector.
// ctx is checked between resources; on cancellation Run returns ctx.Err()
// and no result.
func (s *Stage) Run(ctx context.Context, rs []*tree.Resource, files *tree.FileTable, workers int) (*hub.Bundle, diag.List, error) {
	out := make([]*hub.Resource, len(rs))
	found := make([]diag.List, len(rs))
	budget := newBudget()
	workers = max(1, min(workers, len(rs)))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			w := newWork(budget)
			for {
				// A spent budget ends the run: further resources could
				// only add diagnostics nobody keeps.
				if ctx.Err() != nil || w.spent() {
					return
				}
				i := int(next.Add(1) - 1)
				if i >= len(rs) {
					return
				}
				out[i], found[i] = s.resource(rs[i], files, w)
				w.check()
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	var ds diag.List
	ok := make([]*hub.Resource, 0, len(rs))
	for i := range rs {
		ds = append(ds, found[i]...)
		if out[i] != nil {
			ok = append(ok, out[i])
		}
	}
	ds = limit(ds, budget.Load() < 0)
	ds.Sort()
	if ds.HasErrors() {
		return nil, ds, nil
	}
	return hub.NewBundle(ok), ds, nil
}

// location converts p to a diagnostic location; files nil gives line and
// column only.
func location(files *tree.FileTable, p tree.Pos) diag.Location {
	if !p.Known() {
		return diag.Location{}
	}
	if files == nil {
		return diag.Location{Line: int(p.Line), Column: int(p.Column)}
	}
	return files.Location(p)
}

// canonical reports content that re-enters as ruralz.canonical.v1
// (tree.RoleCanonical): R-64 exempts it from the number range rule.
func canonical(files *tree.FileTable, res *tree.Resource) bool {
	return files != nil && files.File(res.Start.File).Role == tree.RoleCanonical
}

// sourceFile returns the hub.Source file of a resource starting at p: its
// path, or "" for canonical content and unknown files.
func sourceFile(files *tree.FileTable, p tree.Pos) string {
	if files == nil {
		return ""
	}
	f := files.File(p.File)
	if f.Role == tree.RoleCanonical {
		return ""
	}
	return f.Path
}

// metaOf returns the metadata of a typed kind struct.
func metaOf(obj any) *v1alpha1.ObjectMeta {
	switch o := obj.(type) {
	case *v1alpha1.Gateway:
		return &o.Metadata
	case *v1alpha1.Route:
		return &o.Metadata
	case *v1alpha1.Upstream:
		return &o.Metadata
	case *v1alpha1.Policy:
		return &o.Metadata
	case *v1alpha1.Plugin:
		return &o.Metadata
	case *v1alpha1.Consumer:
		return &o.Metadata
	case *v1alpha1.AIProvider:
		return &o.Metadata
	case *v1alpha1.AIModel:
		return &o.Metadata
	case *v1alpha1.Environment:
		return &o.Metadata
	case *v1alpha1.Cluster:
		return &o.Metadata
	default:
		return nil
	}
}

// newObject returns a new typed hub struct for kind.
func newObject(kind v1alpha1.Kind) (any, bool) {
	switch kind {
	case v1alpha1.KindGateway:
		return new(v1alpha1.Gateway), true
	case v1alpha1.KindRoute:
		return new(v1alpha1.Route), true
	case v1alpha1.KindUpstream:
		return new(v1alpha1.Upstream), true
	case v1alpha1.KindPolicy:
		return new(v1alpha1.Policy), true
	case v1alpha1.KindPlugin:
		return new(v1alpha1.Plugin), true
	case v1alpha1.KindConsumer:
		return new(v1alpha1.Consumer), true
	case v1alpha1.KindAIProvider:
		return new(v1alpha1.AIProvider), true
	case v1alpha1.KindAIModel:
		return new(v1alpha1.AIModel), true
	case v1alpha1.KindEnvironment:
		return new(v1alpha1.Environment), true
	case v1alpha1.KindCluster:
		return new(v1alpha1.Cluster), true
	default:
		return nil, false
	}
}
