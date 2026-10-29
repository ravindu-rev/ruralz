// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/phase"
)

// Format is the format identifier of the JSON form. A change of meaning
// needs ruralz.diff.v2; additions keep v1.
const Format = "ruralz.diff.v1"

// Codes the engine raises, each registered in internal/errcode.
const (
	// CodeLiteral is RZ-CFG-012: a secret field holds a value that
	// is not a secretRef, which a diff never shows.
	CodeLiteral = "RZ-CFG-012"
	// CodeDigest is RZ-CFG-027: a side's content does not hash to its
	// digest.
	CodeDigest = "RZ-CFG-027"
)

// ErrInput marks a side the engine cannot compare: no rendered state, no
// Revision digest, or a resource without a tree. The pipeline never hands
// one over, so it is a programming error, not a configuration finding.
var ErrInput = errors.New("diff: invalid input")

// Origin says what a side was read from. It names the member of the side's
// JSON object that carries Side.Location.
type Origin uint8

// Origins.
const (
	// OriginNone writes the revision and digest only, as for a Revision
	// fetched by digest.
	OriginNone Origin = iota
	// OriginBundle is a source Bundle directory or a rendered Bundle file:
	// member "bundle".
	OriginBundle
	// OriginDump is a saved /config/dump document, such as the output of
	// ruralz node dump: member "dump".
	OriginDump
	// OriginAdmin is a live Node's /config/dump read from its admin base
	// URL: member "admin".
	OriginAdmin
)

// member returns the JSON member name of o, or "" for OriginNone.
func (o Origin) member() string {
	switch o {
	case OriginBundle:
		return "bundle"
	case OriginDump:
		return "dump"
	case OriginAdmin:
		return "admin"
	default:
		return ""
	}
}

// Side is one of the two rendered states a diff compares.
type Side struct {
	// State is the pipeline output for the side: a Bundle rendered for an
	// Environment, or canonical content (a /config/dump) run again
	// through the pipeline. Its Bundle trees are the normalized stage G
	// trees, its Chains the stage I chains and its Revision the stage L
	// content and digest.
	State *hub.Validated
	// Origin says what the side was read from.
	Origin Origin
	// Location is the directory, file or URL as the user gave it; it is
	// written only with an Origin other than OriginNone.
	Location string
	// Environment names the Environment the side was rendered for; "" when
	// none, as for a /config/dump.
	Environment string
}

// Options configures an Engine.
type Options struct {
	// Workers is the number of goroutines a Compare runs on; less than 1
	// means 1 and more than MaxWorkers means MaxWorkers. The CLI passes
	// GOMAXPROCS. The result never depends on it.
	Workers int
	// Registry is the Policy type registry that gives each attached
	// Policy's Filter class; nil means registry.New(). It is immutable, so
	// one serves every Engine of a process.
	Registry *registry.Registry
}

// MaxWorkers caps Options.Workers, so no worker count overflows the
// chunk arithmetic.
const MaxWorkers = 256

// Engine compares rendered states. It holds only process-wide inputs, is
// immutable and is safe for concurrent use.
type Engine struct {
	idx     *schemaidx.Index
	reg     *registry.Registry
	workers int
}

// New returns an Engine over the hub schema idx, which supplies the list
// types, impact classes, references, secret positions and since levels.
// idx must not be nil.
func New(idx *schemaidx.Index, o Options) (*Engine, error) {
	if idx == nil {
		return nil, fmt.Errorf("%w: no schema index", ErrInput)
	}
	reg := o.Registry
	if reg == nil {
		reg = registry.New()
	}
	return &Engine{idx: idx, reg: reg, workers: min(max(1, o.Workers), MaxWorkers)}, nil
}

// Change says how a resource differs.
type Change uint8

// Changes, in output order.
const (
	// Modified is a resource present on both sides with field operations.
	Modified Change = iota + 1
	// Added is a resource only TO holds.
	Added
	// Removed is a resource only FROM holds.
	Removed
)

// String returns "modified", "added" or "removed".
func (c Change) String() string {
	switch c {
	case Modified:
		return "modified"
	case Added:
		return "added"
	case Removed:
		return "removed"
	default:
		return ""
	}
}

// symbol returns the human form's marker of c.
func (c Change) symbol() byte {
	switch c {
	case Added:
		return '+'
	case Removed:
		return '-'
	default:
		return '~'
	}
}

// Flags are the always-flagged kinds of change.
type Flags uint8

// Flag values.
const (
	// FlagCapabilityGrant marks a change to Plugin spec.capabilities or
	// spec.image, and an added Plugin: a Capability grant widens what
	// sandboxed code may do. JSON "capabilityGrant", human "(Capability
	// grant)".
	FlagCapabilityGrant Flags = 1 << iota
)

// Strings returns the JSON names of f; nil when empty.
func (f Flags) Strings() []string {
	if f&FlagCapabilityGrant != 0 {
		return []string{"capabilityGrant"}
	}
	return nil
}

// OpKind is a field operation or an effective chain operation.
type OpKind uint8

// Operations, in output order within a resource.
const (
	// OpReplace replaces the value at a path (From with To).
	OpReplace OpKind = iota + 1
	// OpAdd adds To at a path.
	OpAdd
	// OpRemove removes From at a path.
	OpRemove
	// OpMove moves an orderedMap entry from position FromIndex to ToIndex
	// (zero-based), or, in a chain, a Policy relative to the others.
	OpMove
)

// String returns "replace", "add", "remove" or "move".
func (k OpKind) String() string {
	switch k {
	case OpReplace:
		return "replace"
	case OpAdd:
		return "add"
	case OpRemove:
		return "remove"
	case OpMove:
		return "move"
	default:
		return ""
	}
}

// symbol returns the human form's marker of k.
func (k OpKind) symbol() byte {
	switch k {
	case OpAdd:
		return '+'
	case OpRemove:
		return '-'
	case OpMove:
		return '>'
	default:
		return '~'
	}
}

// Op is one field operation of a modified resource. Applied in order to
// FROM's canonical form of the resource, the operations of a resource
// yield TO's (see the package documentation for keyed and positional
// entries).
type Op struct {
	// Kind is the operation.
	Kind OpKind
	// Path is the key-aware path from the resource root.
	Path diag.Path
	// From is the removed or replaced value in canonical form, shared
	// with FROM's tree (read-only); nil for add and move. At a secret
	// position it is a secretRef.
	From *tree.Node
	// To is the added or replacing value, shared with TO's tree.
	To *tree.Node
	// FromIndex and ToIndex are the zero-based positions of a moved
	// entry in FROM's and TO's list.
	FromIndex, ToIndex int
	// Impact is the union of the x-ruralz-impact classes on the path
	// and, for add, remove and replace, inside the values.
	Impact schemaidx.Impact
	// Flags marks a Capability grant.
	Flags Flags
}

// Resource is one changed resource.
type Resource struct {
	// ID is the identity (kind, metadata.name).
	ID hub.ID
	// Change says how it differs.
	Change Change
	// Source is the file TO read it from, else the file FROM read it from;
	// "" when neither side has file names (canonical content).
	Source string
	// Impact is the union of the operations' classes; for an added or
	// removed resource, of every field it holds.
	Impact schemaidx.Impact
	// Flags is the union of the operations' flags; FlagCapabilityGrant for
	// an added Plugin.
	Flags Flags
	// Ops are the field operations of a modified resource: replace, add,
	// remove, then move, each in document order. Added and removed
	// resources carry identity only.
	Ops []Op
}

// ChainChange is one effective Filter Chain change of a Route in a Phase.
type ChainChange struct {
	// Route is the Route name.
	Route string
	// Phase is the Phase.
	Phase phase.Phase
	// Leg is the Upstream of an upstream leg; "" for the client leg.
	Leg string
	// Op is OpAdd, OpRemove or OpMove.
	Op OpKind
	// Policy is the Policy name.
	Policy string
	// Class is the Policy's Filter class on the side it is read from.
	Class phase.Class
	// After names the preceding Policy in TO's chain (add, move) or
	// FROM's (remove); "" when it is first.
	After string
}

// Summary counts the changes.
type Summary struct {
	// Changed is Added + Removed + Modified.
	Changed int
	// Added, Removed and Modified count resources.
	Added, Removed, Modified int
	// RouteChainsChanged counts Routes with an effective chain change.
	RouteChainsChanged int
	// Impact is the union of the resources' classes.
	Impact schemaidx.Impact
}

// Endpoint describes one side in the output.
type Endpoint struct {
	// Digest is the side's Revision digest.
	Digest revision.Digest
	// Origin and Location say what the side was read from.
	Origin Origin
	// Location is written under Origin's member.
	Location string
	// Environment is set only when the two sides name different
	// Environments (a promotion review); Diff.Environment is set
	// otherwise.
	Environment string
}

// Diff is the result of a Compare.
type Diff struct {
	// From and To describe the sides.
	From, To Endpoint
	// Environment is the one Environment named by either side, or "".
	Environment string
	// Resources are the changed resources: modified, then added, then
	// removed, each in kind catalog order, then by name.
	Resources []Resource
	// Effective are the effective Filter Chain changes, by Route name,
	// then Phase in execution order, then leg (client, then Upstreams by
	// name): adds, removes, then moves.
	Effective []ChainChange
	// Summary counts them.
	Summary Summary
}

// Changed reports whether the sides differ: the CLI exits 1 when they
// do and 0 when they do not.
func (d *Diff) Changed() bool { return len(d.Resources) > 0 || len(d.Effective) > 0 }

// Compare compares FROM with TO, in that order. It fails with ErrInput
// for a side without a state, Bundle or digest, with an RZ-CFG-027 error
// when a side's content does not hash to its digest, with an RZ-CFG-012
// error when a value it would show at a secret position is not a
// secretRef, and with ctx's error once ctx ends. Equal digests compare
// equal without reading the trees.
func (e *Engine) Compare(ctx context.Context, from, to Side) (*Diff, error) {
	if err := check("FROM", from); err != nil {
		return nil, err
	}
	if err := check("TO", to); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d := &Diff{From: endpoint(from), To: endpoint(to)}
	d.Environment, d.From.Environment, d.To.Environment = environments(from.Environment, to.Environment)
	if from.State.Revision.Digest == to.State.Revision.Digest {
		return d, nil
	}
	pairs := pairUp(from.State.Bundle.Resources(), to.State.Bundle.Resources())
	changes := make([]*Resource, len(pairs))
	comparers := make([]*comparer, e.workers)
	err := run(ctx, len(pairs), e.workers, func(w, i int) error {
		if comparers[w] == nil {
			comparers[w] = newComparer(e.idx, e.reg, from.State.Bundle, to.State.Bundle)
		}
		r, err := comparers[w].pair(pairs[i])
		changes[i] = r
		return err
	})
	if err != nil {
		return nil, err
	}
	for _, r := range changes {
		if r != nil {
			d.Resources = append(d.Resources, *r)
		}
	}
	slices.SortStableFunc(d.Resources, func(a, b Resource) int {
		return cmp.Or(cmp.Compare(a.Change, b.Change),
			cmp.Compare(hub.CatalogOrder(a.ID.Kind), hub.CatalogOrder(b.ID.Kind)),
			cmp.Compare(a.ID.Kind, b.ID.Kind),
			cmp.Compare(a.ID.Name, b.ID.Name))
	})
	if d.Effective, err = effective(ctx, from.State.Chains, to.State.Chains, e.workers); err != nil {
		return nil, err
	}
	d.Summary = summarize(d)
	return d, nil
}

// check refuses a side the engine cannot compare.
func check(name string, s Side) error {
	switch {
	case s.State == nil || s.State.Bundle == nil:
		return fmt.Errorf("%w: %s has no rendered state", ErrInput, name)
	case s.State.Revision.Digest.IsZero():
		return fmt.Errorf("%w: %s has no Revision digest", ErrInput, name)
	}
	if got := revision.Sum(s.State.Revision.Content); got != s.State.Revision.Digest {
		return errcode.Errorf(CodeDigest, "%s content hashes to %s, want %s", name, got, s.State.Revision.Digest)
	}
	for _, r := range s.State.Bundle.Resources() {
		if r == nil || r.Tree == nil || r.Tree.Kind != tree.KindMap {
			return fmt.Errorf("%w: %s has a resource without a tree", ErrInput, name)
		}
	}
	return nil
}

// endpoint returns the output description of s.
func endpoint(s Side) Endpoint {
	return Endpoint{Digest: s.State.Revision.Digest, Origin: s.Origin, Location: s.Location}
}

// environments returns the one Environment the sides name, or, when they
// name two, each side's: a side without one takes no part.
func environments(from, to string) (one, fromEnv, toEnv string) {
	switch {
	case from == to || to == "":
		return from, "", ""
	case from == "":
		return to, "", ""
	default:
		return "", from, to
	}
}

// pair is one identity and its resource on each side (nil when absent).
type pair struct {
	from, to *hub.Resource
}

// pairUp matches resources by identity; renames are never inferred. It
// sorts both lists by kind order, kind and name, then pairs them in one
// merge walk.
func pairUp(from, to []*hub.Resource) []pair {
	order := func(a, b *hub.Resource) int {
		return cmp.Or(cmp.Compare(hub.KindOrder(a.Kind), hub.KindOrder(b.Kind)), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
	}
	from = slices.SortedStableFunc(slices.Values(from), order)
	to = slices.SortedStableFunc(slices.Values(to), order)
	out := make([]pair, 0, max(len(from), len(to)))
	i, j := 0, 0
	for i < len(from) || j < len(to) {
		switch {
		case j == len(to) || (i < len(from) && order(from[i], to[j]) < 0):
			out = append(out, pair{from: from[i]})
			i++
		case i == len(from) || order(from[i], to[j]) > 0:
			out = append(out, pair{to: to[j]})
			j++
		default:
			out = append(out, pair{from: from[i], to: to[j]})
			i++
			j++
		}
	}
	return out
}

// summarize counts d's changes.
func summarize(d *Diff) Summary {
	var s Summary
	for i := range d.Resources {
		r := &d.Resources[i]
		switch r.Change {
		case Added:
			s.Added++
		case Removed:
			s.Removed++
		default:
			s.Modified++
		}
		s.Impact |= r.Impact
	}
	s.Changed = s.Added + s.Removed + s.Modified
	routes := map[string]bool{}
	for _, c := range d.Effective {
		routes[c.Route] = true
	}
	s.RouteChainsChanged = len(routes)
	return s
}
