// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package defaults is stage G of the configuration pipeline: it turns each
// resource that passed schema validation into a hub.Resource and the
// resources of one rendered Bundle into a hub.Bundle.
//
// For each resource, in order, after the converter registry's
// RZ-CFG-025 warnings for a deprecated apiVersion or field of its
// lifecycle table (CM Figure 3; the table is empty in M1), read from
// the tree as authored (and never for content that re-enters as
// ruralz.canonical.v1, tree.RoleCanonical: it names no apiVersion of
// its own and holds the defaults an earlier run wrote, so the warnings
// belong to the run that read the authored source):
//
//  1. Materialize writes the defaults under the schema of the resource's
//     own apiVersion: every +ruralz:default the rendered schema declares,
//     only inside objects that are present (an absent optional object is
//     never created), inside list entries, typed-map values and
//     a Policy config through its type dispatch, plus the
//     registry defaults slot, failureMode and filterClass of every Policy
//     . The mechanism is table-free: a new marker needs no
//     code change.
//  2. internal/config/convert converts the tree to the hub representation
//     and strips the conversion-data annotation.
//  3. Normalize rewrites values to one spelling under the hub schema
//     : Duration, ByteSize and Decimal values
//     by their definition, integral numbers at integer positions as
//     integer text, host names in Route match.hosts and Gateway
//     listeners[].hostnames in ASCII lower case, set lists sorted by their
//     elements' RFC 8785 bytes and map lists by key, object members in
//     RFC 8785 order, everything else (orderedMap and atomic lists, free
//     content) as authored. It reports RZ-CFG-005 for a duration that
//     overflows, a byte size that does not parse, an integer outside
//     ±(2^53−1) or a number outside the double range after normalization
//     (not for canonical content, whose RFC 8785 doubles
//     may print as large integers), and for duplicates that only
//     normalization reveals, such as 90s and 1m30s in one set. It is
//     also the gate for what the canonical form cannot hold, in every
//     tree, canonical content included: a null, a nil node or a node of
//     no JSON kind, a string or member name that is not valid UTF-8, a
//     member name an object holds twice and a number literal outside the
//     RFC 8259 grammar are each RZ-CFG-005 at their key-aware path and
//     position, so the decode never meets one; and for what a decode
//     would misread: an undeclared member whose name equals a declared
//     one under case folding, such as Key beside key in an open config,
//     which encoding/json would decode as the declared member (a Filter
//     factory decoding the Policy's spec.config included). Each failure
//     is one diagnostic: a list element or keyed entry it reports is not
//     reported again as a duplicate or as an entry without a key.
//  4. Decode decodes the tree into its pkg/config/v1alpha1 kind struct and
//     a Policy's config into the registry's config type, reporting every
//     value that does not fit its Go field as RZ-CFG-005 at the field,
//     the config's too when the kind struct failed; it also runs after a
//     normalization error, then reporting only the values normalization
//     did not report already. Large values are
//     decoded in pieces of at most 256 nodes and about 64 KiB of JSON
//     text, each one encoding/json call, with the result of one call over
//     the whole tree, so the worker can yield between pieces: structs,
//     maps, slices and the any-valued members of plugin configs and
//     Plugin configSchema alike. A value whose Go type has its own
//     UnmarshalJSON gets its JSON text in one call instead: the scalars
//     Duration, ByteSize and IntOrString, a Policy's spec.config
//     (json.RawMessage, a copy) and the JSONSchemaDocument of a
//     validation.json-schema config, which pkg/config/v1alpha1 checks
//     with one json.Valid scan, linear in the document's size, before its
//     copy.
//
// Every step is idempotent: running stage G on its own output, or on a
// decoded canonical document, changes nothing. Presence is kept: absent
// fields stay absent, empty objects and lists and empty strings stay.
//
// Free content is data. Undeclared members of +ruralz:open configs, plugin
// configs, Plugin configSchema and the inline JSON Schema document of a
// validation.json-schema Policy (the JSONSchemaDocument definition) get no
// defaults, keep every list in authored order and have their members
// sorted and numbers checked like any other.
//
// JSON null never reaches the hub: an overlay null removed its field
// before stage F, and any null left, in typed or free content, the
// JSONSchemaDocument value included, is RZ-CFG-005, because the
// canonical form has no null.
//
// A nil node is JSON null wherever stage G meets one (the converters of
// Options.Converters and FromResources trees may hold one): every lookup
// treats it as null, and Normalize reports it as one.
//
// Diagnostics are bounded by Options.MaxDiagnostics, the cap of the run,
// 10,000 by default: each resource records at most the cap plus one in
// tree order, and Run returns the first cap plus one of all of them in
// diag.List.Sort order, the same for every worker count (and for every
// input order, but for diagnostics that tie on the whole sort key). Stage
// G never appends RZ-CFG-001 "too many diagnostics" itself: a result
// longer than the cap tells the pipeline's diag.Collector, which appends
// the run's one RZ-CFG-001, that the cap was reached. A diagnostic costs
// the depth of its key-aware path, whatever the size of the list elements
// on it: Normalize names each element once, by its key member or its RFC
// 8785 text, for all the diagnostics below it, and the paths of a
// converter's diagnostics are resolved by one shared index of the
// containers they step into (TestScaleLinearErrors, TestOwnScaleLinear).
//
// Stage values are immutable and safe for concurrent use; the functions
// rewrite the trees they are given in place. The package never logs,
// reads files, the network or the clock: the yield timer reads the clock
// the caller injects as Options.Now. A worker counts
// its work units (nodes visited; bytes of strings, member names and
// number literals read; sort comparisons and the bytes of the common
// prefix each reads; and the members, nodes or bytes each lookup scans)
// across every step and every resource, reads the timer every 256 units,
// between resources and around each encoding/json call, and yields the
// processor once Options.YieldAfter (100 µs by default) of work have
// passed since its last yield. The time between two reads is bounded by
// the longest single call into another package, linear in the one value
// it reads (see work).
package defaults

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

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
	// rejects.
	CodeSchema = "RZ-CFG-005"
	// CodeUnserved is RZ-CFG-007: a resource whose apiVersion has no
	// schema or converter, or whose kind has no hub type. Stage C reports
	// these first; stage G repeats the check for resources that re-enter
	// at stage F.
	CodeUnserved = "RZ-CFG-007"
)

// DefaultMaxDiagnostics is the cap of one run when Options.MaxDiagnostics
// is zero: the loader's Limits.MaxDiagnostics default (10,000 per run).
const DefaultMaxDiagnostics = 10_000

// defJSONSchemaDocument is the $defs name of the inline JSON Schema
// document (validation.json-schema config.schema): opaque data, below
// which every position is free.
const defJSONSchemaDocument = "JSONSchemaDocument"

// ErrOptions marks every error New returns.
var ErrOptions = errors.New("defaults: invalid options")

// Options configure a Stage.
type Options struct {
	// Schemas indexes the rendered schema of every served apiVersion; it
	// must serve convert.HubAPIVersion and every apiVersion Converters
	// serves.
	Schemas *schemaidx.Set
	// Registry is the Policy type registry (registry.New); it must know
	// every Policy type the hub schema admits.
	Registry *registry.Registry
	// Converters is the conversion registry; nil means convert.Default().
	// It must serve convert.HubAPIVersion, as every registry
	// convert.NewRegistry builds does (the zero Registry does not).
	Converters *convert.Registry
	// MaxDiagnostics is the cap of the run: the pipeline passes its
	// Limits.MaxDiagnostics, or what earlier stages left of it. Each
	// resource records at most MaxDiagnostics+1 diagnostics, and Run and
	// Resource return at most MaxDiagnostics+1, so a diag.Collector of the
	// same cap sees that it was reached and appends its one RZ-CFG-001.
	// Zero means DefaultMaxDiagnostics; a negative value is an error.
	// Every positive value is valid: math.MaxInt, the usual way to say no
	// cap, counts as math.MaxInt-1, so the cap plus one never overflows
	// (no run reaches either).
	MaxDiagnostics int
	// Now is the clock of the yield timer: the pipeline passes
	// clock.Real().Now, tests a fake. Run's workers call it concurrently.
	// Nil never yields, as the zero options of the profile and schemaview
	// stages do.
	Now func() time.Time
	// YieldAfter is the work a worker does before it yields (the loader's
	// Options.Yield); zero means DefaultYieldAfter, and a negative value
	// is an error.
	YieldAfter time.Duration
}

// Stage runs stage G. It is immutable and safe for concurrent use.
type Stage struct {
	schemas        *schemaidx.Set
	hub            *schemaidx.Index
	reg            *registry.Registry
	conv           *convert.Registry
	maxDiagnostics int
	now            func() time.Time
	yieldAfter     time.Duration
	// stopped is a test hook, nil otherwise: Run calls it with the
	// index of the first resource whose panic it recorded, once dispatch
	// is stopped.
	stopped func(index int)
}

// New returns a Stage, or an error wrapping ErrOptions when a required
// option is missing, a limit is negative, or the options do not fit
// together: the converters must serve the hub apiVersion (as
// convert.NewRegistry and convert.Default ensure; the zero
// convert.Registry serves none), the schemas must cover every apiVersion
// the converters serve, and the Policy type registry must know every
// type the hub schema admits in a Policy's spec.type (registry.New does;
// the zero registry.Registry knows none). Each of these would otherwise
// fail or misdecode every resource it touches with a diagnostic that
// blames the configuration for the program's error.
func New(o Options) (*Stage, error) {
	if o.Schemas == nil || o.Registry == nil {
		return nil, fmt.Errorf("%w: Schemas and Registry are required", ErrOptions)
	}
	if o.MaxDiagnostics < 0 || o.YieldAfter < 0 {
		return nil, fmt.Errorf("%w: MaxDiagnostics %d and YieldAfter %v must not be negative", ErrOptions, o.MaxDiagnostics, o.YieldAfter)
	}
	conv := o.Converters
	if conv == nil {
		conv = convert.Default()
	}
	if !conv.Serves(convert.HubAPIVersion) {
		return nil, fmt.Errorf("%w: the converters do not serve the hub apiVersion %s; build them with convert.NewRegistry", ErrOptions, convert.HubAPIVersion)
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
	if t, ok := unregisteredType(hubIdx, o.Registry); ok {
		return nil, fmt.Errorf("%w: the hub schema admits Policy type %q, which the Policy type registry does not know", ErrOptions, t)
	}
	s := &Stage{
		schemas: o.Schemas, hub: hubIdx, reg: o.Registry, conv: conv,
		maxDiagnostics: cmp.Or(o.MaxDiagnostics, DefaultMaxDiagnostics),
		now:            o.Now, yieldAfter: cmp.Or(o.YieldAfter, DefaultYieldAfter),
	}
	return s, nil
}

// unregisteredType returns the first Policy type that the hub schema
// admits in spec.type and reg does not know.
func unregisteredType(hubIdx *schemaidx.Index, reg *registry.Registry) (string, bool) {
	spec, _ := hubIdx.Spec(string(v1alpha1.KindPolicy))
	typ, _ := spec.Property("type")
	for _, t := range typ.Enum() {
		if _, ok := reg.Lookup(v1alpha1.PolicyType(t)); !ok {
			return t, true
		}
	}
	return "", false
}

// HubIndex returns the schema index of the hub representation.
func (s *Stage) HubIndex() *schemaidx.Index { return s.hub }

// Resource runs stage G on res, a resource that passed stage F: defaults
// under its own apiVersion's schema, conversion to the hub, normalization
// and typed decode. It rewrites res.Root in place and returns the
// hub.Resource holding it, or nil when it reported an error. files gives
// source locations and the file role (nil: line and column only, and the
// resource counts as authored content); diagnostics carry res's identity
// and come in step order (the RZ-CFG-025 warnings of the converters'
// lifecycle table, conversion, normalization, decode), each step's in
// tree order (the decode's: the kind struct's, then the config's), at
// most Options.MaxDiagnostics+1 of them (the rest are dropped, and the
// resource still fails when one was an error).
func (s *Stage) Resource(res *tree.Resource, files *tree.FileTable) (*hub.Resource, diag.List) {
	return s.resource(res, files, s.newWork())
}

// resource is Resource on worker w: diagnostics draw on a budget of
// w.keep, and it returns nil whenever res has an error, recorded or not.
func (s *Stage) resource(res *tree.Resource, files *tree.FileTable, w *work) (*hub.Resource, diag.List) {
	w.begin()
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
				Message: fmt.Sprintf("apiVersion %q has no schema in this release", clip(res.APIVersion)),
			}
		}
		return nil, s.own(res, files, w.admit(diag.List{d}), w)
	}
	// The RZ-CFG-025 warnings of the lifecycle table (CM Figure 3 assigns
	// them to stage G) read the tree as authored, before any default is
	// written, so a default never warns. Canonical content gets none: it
	// was authored in an earlier run, which warned, and holds that run's
	// defaults as plain members.
	var ds diag.List
	if !canonical(files, res) {
		ds = w.admit(s.conv.DeprecationsCounted(src, res, files, w.charge))
	}
	materialize(src, s.reg, res, w)
	// Registry.ToHub reports a converter that returns no object as
	// RZ-CFG-007, so root is an object whenever conv has no error.
	root, conv := s.conv.ToHub(res.APIVersion, string(res.ID.Kind), res.Root, false)
	failed := conv.HasErrors()
	ds = append(ds, s.own(res, files, w.admit(conv), w)...)
	if failed {
		return nil, ds
	}
	h := *res
	h.Root, h.APIVersion = root, convert.HubAPIVersion
	more, prior := normalize(s.hub, &h, files, w)
	ds = append(ds, more...)
	// After a normalization error the decode still runs, so every value
	// that does not fit its Go field is reported in this run too, but
	// for the nodes normalization reported already.
	obj, cfg, more, failed := decode(s.hub, s.reg, &h, files, w, prior)
	ds = append(ds, more...)
	if failed || prior != nil {
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

// own fills the resource identity into diagnostics a converter returned,
// and the location of each that has none: the position of the node its
// path names in res.Root, the document the converter was given, or of the
// nearest positioned node on that path (converters report a path, stage
// G adds the file positions); for an empty or unresolvable
// path, the position of the apiVersion member. The paths are resolved
// together by one finder, which indexes each large object, keyed list and
// set they step into once, so the cost stays linear in the tree and the
// paths however many diagnostics name entries of one list; w counts it
// and at most one apiVersion lookup.
func (s *Stage) own(res *tree.Resource, files *tree.FileTable, ds diag.List, w *work) diag.List {
	var fallback *diag.Location
	var f *finder
	for i := range ds {
		if ds[i].Resource == nil {
			ds[i].Resource = res.ID.ResourceID()
		}
		if ds[i].Location != (diag.Location{}) {
			continue
		}
		if f == nil {
			f = w.newFinder()
		}
		if p, ok := f.at(res.Root, ds[i].Path); ok {
			ds[i].Location = location(files, p)
			continue
		}
		if fallback == nil {
			p := res.Start
			if v, ok := w.get(res.Root, "apiVersion"); ok && v != nil && v.Pos.Known() {
				p = v.Pos
			}
			l := location(files, p)
			fallback = &l
		}
		ds[i].Location = *fallback
	}
	return ds
}

// Run runs Resource on every resource of rs with at most workers
// goroutines (fewer than 1 means 1), created and joined within the call,
// and returns the hub.Bundle of the results with the diagnostics of
// every resource (every finding of the stage in one run). The Bundle is
// nil when any resource failed.
//
// Every resource is processed, each recording at most
// Options.MaxDiagnostics+1 diagnostics in tree order, and Run returns the
// first MaxDiagnostics+1 of them all in diag.List.Sort order, ties broken
// by input order. The result is therefore the same for every worker
// count, and for every input order unless two diagnostics of different
// resources tie on the whole sort key; one longer than MaxDiagnostics
// means the cap was reached, which the pipeline's diag.Collector reports
// as RZ-CFG-001. The diagnostics held at any time are at most
// MaxDiagnostics+1 per worker (the resource it processes) and fewer than
// twice that in the keeper; each has a message that quotes authored
// values clipped to 64 code points and a path of one element per level,
// which shares its member names and keys with the tree and its set
// element texts with the other diagnostics below that element. No path is
// rendered to text: the keeper compares paths element by element, only on
// a tie of everything before them in the sort key, so a long authored key
// below thousands of diagnostics is held once. ctx is checked between
// resources; on cancellation Run returns ctx.Err() and no result.
//
// A panic while one resource is processed (in a converter of
// Options.Converters, say) stops the dispatch of further resources; Run
// waits for every resource already started and then raises the first
// panic recovered again in the calling goroutine, as an error value that
// wraps the panic value when it is an error and holds the stack of the
// panicking worker, so a caller's recover sees it and no goroutine
// outlives the call, for every worker count. A call of runtime.Goexit
// while one resource is processed, which ends the worker's goroutine
// with no panic to recover, is handled the same way: it stops the
// dispatch, and Run raises an error value that wraps errGoexit, so no
// resource is ever left out of the result silently.
func (s *Stage) Run(ctx context.Context, rs []*tree.Resource, files *tree.FileTable, workers int) (*hub.Bundle, diag.List, error) {
	out := make([]*hub.Resource, len(rs))
	kept := newKeeper(keepOf(s.maxDiagnostics))
	var failed atomic.Bool
	var st runState
	workers = max(1, min(workers, len(rs)))
	var next atomic.Int64
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			w := s.newWork()
			for {
				if ctx.Err() != nil || st.stop.Load() {
					return
				}
				i := int(next.Add(1) - 1)
				if i >= len(rs) {
					return
				}
				if !s.runOne(rs[i], i, files, w, out, kept, &failed, &st) {
					return
				}
				w.check()
			}
		})
	}
	wg.Wait()
	if st.panicked != nil {
		panic(st.panicked)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	ds := kept.list()
	if failed.Load() {
		return nil, ds, nil
	}
	ok := make([]*hub.Resource, 0, len(rs))
	for _, h := range out {
		if h != nil {
			ok = append(ok, h)
		}
	}
	return hub.NewBundle(ok), ds, nil
}

// runState is what the workers of one Run share about a stop: the stop
// flag, which ends the dispatch, and the first worker failure recorded.
type runState struct {
	stop     atomic.Bool
	mu       sync.Mutex
	panicked *resourcePanic
}

// errGoexit is the value of the resourcePanic of a worker that ended
// while processing a resource without returning and without a panic to
// recover: something it called ran runtime.Goexit (or panicked with nil
// under GODEBUG=panicnil=1, which recover cannot tell apart).
var errGoexit = errors.New("defaults: runtime.Goexit ended the worker while stage G processed the resource")

// runOne runs stage G on resource i of a Run on worker w and records its
// result; it returns false, after recording the stop in st, when the
// resource panicked. A runtime.Goexit never returns here: the deferred
// function records the stop, and the worker's goroutine then ends.
func (s *Stage) runOne(res *tree.Resource, i int, files *tree.FileTable, w *work, out []*hub.Resource, kept *keeper, failed *atomic.Bool, st *runState) (ok bool) {
	done := false
	defer func() {
		if done {
			return
		}
		p := &resourcePanic{value: recover(), index: i, stack: debug.Stack()}
		if p.value == nil {
			p.value, p.goexit = errGoexit, true
		}
		s.halt(st, p)
	}()
	h, ds := s.resource(res, files, w)
	if h == nil && res != nil {
		failed.Store(true)
	}
	out[i] = h
	kept.add(i, ds)
	done = true
	return true
}

// halt stops the dispatch of a Run and records p when it is the first
// failure; the test hook stopped sees the first one's index.
func (s *Stage) halt(st *runState, p *resourcePanic) {
	st.stop.Store(true)
	st.mu.Lock()
	first := st.panicked == nil
	if first {
		st.panicked = p
	}
	st.mu.Unlock()
	if first && s.stopped != nil {
		s.stopped(p.index)
	}
}

// resourcePanic is a panic raised, or a runtime.Goexit called (value
// errGoexit), while Run processed one resource, carried to the goroutine
// that called Run. It is an error that wraps the panic value when that
// value is an error, so errors.Is and errors.As reach it, and its text
// holds the value and the worker's stack.
type resourcePanic struct {
	// value is what was passed to panic, or errGoexit.
	value any
	// index is the resource's index in the Run input.
	index int
	// stack is the worker's stack at the panic or the Goexit.
	stack []byte
	// goexit reports a runtime.Goexit.
	goexit bool
}

// Error returns the panic value, the resource index and the stack.
func (p *resourcePanic) Error() string {
	what := "panicked"
	if p.goexit {
		what = "stopped"
	}
	return fmt.Sprintf("defaults: stage G %s on resource %d: %v\n\n%s", what, p.index, p.value, p.stack)
}

// Unwrap returns the panic value when it is an error, else nil.
func (p *resourcePanic) Unwrap() error {
	err, _ := p.value.(error)
	return err
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
// (tree.RoleCanonical). It is exempt from the number range rule: its RFC
// 8785 doubles may print as integers beyond ±(2^53−1).
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
