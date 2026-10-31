// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loader

import (
	"cmp"
	"errors"
	"fmt"
	"math"
	"runtime"
	"time"

	"github.com/ravindu-rev/ruralz/internal/config/convert"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/schemaview"
)

// Codes this package raises; each is registered in internal/errcode.
const (
	// CodeLimit is RZ-CFG-001: a size limit, the diagnostics cap, or a
	// symbolic link, Git submodule, Git LFS pointer or other entry that is
	// not a regular file or directory.
	CodeLimit = "RZ-CFG-001"
	// CodeEnvelope is RZ-CFG-005: a document without a string
	// metadata.name, which a resource needs for its identity.
	CodeEnvelope = "RZ-CFG-005"
	// CodeUnserved is RZ-CFG-007: an unknown kind, or a missing, unknown
	// or unserved apiVersion.
	CodeUnserved = "RZ-CFG-007"
	// CodeDuplicate is RZ-CFG-008: one identity in two base documents or
	// two Environments of one file.
	CodeDuplicate = "RZ-CFG-008"
	// CodeMissing is RZ-CFG-009: an Environment name the environments file
	// does not declare.
	CodeMissing = "RZ-CFG-009"
	// CodeGateway is RZ-CFG-016: not exactly one Gateway, or a Gateway
	// outside ruralz.yaml.
	CodeGateway = "RZ-CFG-016"
	// CodeControlKind is RZ-CFG-017: an Environment or Cluster inside a
	// Bundle.
	CodeControlKind = "RZ-CFG-017"
	// CodeCycle is RZ-CFG-022: a promotion cycle.
	CodeCycle = "RZ-CFG-022"
)

// Bundle layout names.
const (
	// RootFile is the file at the Bundle root that declares the Gateway.
	RootFile = "ruralz.yaml"
	// IgnoreFile is the optional gitignore-syntax file at the Bundle root.
	IgnoreFile = ".ruralzignore"
	// OverlayDir is the root directory that holds the overlays.
	OverlayDir = "overlays"
)

// Default limits (docs/architecture/02-configuration-model.md,
// "Restricted YAML profile"). A Node never passes a limit below these.
const (
	// DefaultMaxSourceBytes is the source text of a Bundle: 64 MiB
	// (target).
	DefaultMaxSourceBytes int64 = 64 << 20
	// DefaultMaxResources is the resources of a Bundle: 20,000 (target).
	DefaultMaxResources = 20_000
	// DefaultMaxFiles is the configuration files of a Bundle: 20,000, so
	// every file can hold a resource.
	DefaultMaxFiles = 20_000
	// DefaultMaxDepth is the nesting levels of a document: 64 (target).
	DefaultMaxDepth = profile.DefaultMaxDepth
	// DefaultMaxDocumentNodes is the YAML nodes of a document: 1,000,000
	// (target).
	DefaultMaxDocumentNodes = profile.DefaultMaxDocumentTokens
	// DefaultMaxIgnorePatterns is the patterns of every .ruralzignore of a
	// Bundle together: 1,000. Each path is matched against the patterns
	// of the files above it.
	DefaultMaxIgnorePatterns = 1_000
	// DefaultMaxDiagnostics is the diagnostics of one run: 10,000.
	DefaultMaxDiagnostics = 10_000
	// DefaultYieldAfter is the work a worker does before it yields: 100 µs
	// (target).
	DefaultYieldAfter = 100 * time.Microsecond
)

// NodesPerResource is the YAML and JSON nodes one load may build for each
// resource the resource limit allows: the node budget is 4,000,000 nodes
// at the default of 20,000 resources (Limits.MaxNodes).
const NodesPerResource = 200

// maxIgnorePatternBytes is the longest .ruralzignore pattern: 4,096 bytes,
// the longest path Linux accepts. A longer pattern is RZ-CFG-001, which
// keeps every compiled pattern small.
const maxIgnorePatternBytes = 4096

// Limits bound one load. A zero field takes its default.
type Limits struct {
	// MaxSourceBytes bounds the bytes of every file a Bundle load reads:
	// .ruralzignore, base files and the files of every overlay. An
	// environments file has its own budget of the same size.
	MaxSourceBytes int64
	// MaxResources bounds the documents of the base union, of one
	// overlay, of an environments file and of a rendered Bundle. It also
	// sets the node budget of a load (MaxNodes).
	MaxResources int
	// MaxFiles bounds the configuration files of a Bundle, overlays
	// included.
	MaxFiles int
	// MaxDepth bounds the nesting of a document; its root mapping is
	// depth 1. A value above 10,000 counts as 10,000, as in JSON.
	MaxDepth int
	// MaxDocumentNodes bounds the YAML nodes of one document. The files of
	// one load share the node budget instead of a per-file bound.
	MaxDocumentNodes int
	// MaxIgnorePatterns bounds the patterns of every .ruralzignore of a
	// Bundle together.
	MaxIgnorePatterns int
	// MaxDiagnostics is the cap of one run: past it the load stops with
	// one RZ-CFG-001 "too many diagnostics".
	MaxDiagnostics int
}

// MaxNodes returns the node budget of one load: NodesPerResource YAML and
// JSON nodes for each resource MaxResources allows, counted across every
// file the load parses, the base files and the selected overlay's; an
// environments file has a budget of its own. The node past it is
// RZ-CFG-001 and stops the load. It is math.MaxInt when the product does
// not fit an int. Zero fields count as their defaults.
func (l Limits) MaxNodes() int {
	n := l.withDefaults().MaxResources
	if n > math.MaxInt/NodesPerResource {
		return math.MaxInt
	}
	return n * NodesPerResource
}

// withDefaults returns l with every zero field set to its default.
func (l Limits) withDefaults() Limits {
	l.MaxSourceBytes = cmp.Or(l.MaxSourceBytes, DefaultMaxSourceBytes)
	l.MaxResources = cmp.Or(l.MaxResources, DefaultMaxResources)
	l.MaxFiles = cmp.Or(l.MaxFiles, DefaultMaxFiles)
	l.MaxDepth = cmp.Or(l.MaxDepth, DefaultMaxDepth)
	l.MaxDocumentNodes = cmp.Or(l.MaxDocumentNodes, DefaultMaxDocumentNodes)
	l.MaxIgnorePatterns = cmp.Or(l.MaxIgnorePatterns, DefaultMaxIgnorePatterns)
	l.MaxDiagnostics = cmp.Or(l.MaxDiagnostics, DefaultMaxDiagnostics)
	return l
}

// check reports the first negative limit.
func (l Limits) check() error {
	for _, f := range []struct {
		name string
		v    int64
	}{
		{"MaxSourceBytes", l.MaxSourceBytes},
		{"MaxResources", int64(l.MaxResources)},
		{"MaxFiles", int64(l.MaxFiles)},
		{"MaxDepth", int64(l.MaxDepth)},
		{"MaxDocumentNodes", int64(l.MaxDocumentNodes)},
		{"MaxIgnorePatterns", int64(l.MaxIgnorePatterns)},
		{"MaxDiagnostics", int64(l.MaxDiagnostics)},
	} {
		if f.v < 0 {
			return fmt.Errorf("%w: Limits.%s is %d, want zero for the default or a positive value", ErrOptions, f.name, f.v)
		}
	}
	return nil
}

// Options configure a Loader. They are process-wide, so one Loader
// serves every load of a process.
type Options struct {
	// Schemas indexes the rendered schema of every served apiVersion; nil
	// indexes the schemas embedded in api/schema.
	Schemas *schemaidx.Set
	// Views compiles the same schemas for validating Environment
	// resources; nil compiles the embedded views of Schemas.
	Views *schemaview.Set
	// Converters is the conversion registry whose apiVersions are served;
	// nil means convert.Default().
	Converters *convert.Registry
	// Limits bound each load.
	Limits Limits
	// Workers is the most goroutines a load runs on; zero means
	// max(1, GOMAXPROCS/2), as a Node's activation runs it.
	Workers int
	// Now is the clock of the workers' yield timer: the binaries pass
	// clock.Real().Now, tests a fake. Nil never yields.
	Now func() time.Time
	// YieldAfter is the work a worker does before it yields; zero means
	// DefaultYieldAfter.
	YieldAfter time.Duration
}

// Errors of this package.
var (
	// ErrOptions marks invalid Options or an invalid Request.
	ErrOptions = errors.New("loader: invalid options")
	// ErrSource marks a source that cannot be read: missing, not a file or
	// directory, or failing to open, list or read.
	ErrSource = errors.New("loader: cannot read the source")
	// ErrNoFile marks a ParseFile path that is not a configuration file of
	// the Bundle.
	ErrNoFile = errors.New("loader: not a configuration file of the Bundle")
)

// Loader runs stages A to E. It is immutable and safe for concurrent use.
type Loader struct {
	schemas    *schemaidx.Set
	views      *schemaview.Set
	conv       *convert.Registry
	limits     Limits
	workers    int
	now        func() time.Time
	yieldAfter time.Duration
}

// New returns a Loader. It compiles the embedded schemas when o leaves
// them nil, which takes a while: build one Loader per process. It fails
// with ErrOptions for a negative limit, worker count or yield time, and
// when a served apiVersion has no schema or view.
func New(o Options) (*Loader, error) {
	if err := o.Limits.check(); err != nil {
		return nil, err
	}
	if o.Workers < 0 || o.YieldAfter < 0 {
		return nil, fmt.Errorf("%w: Workers %d and YieldAfter %v must not be negative", ErrOptions, o.Workers, o.YieldAfter)
	}
	l := &Loader{
		schemas: o.Schemas, views: o.Views, conv: o.Converters,
		limits:  o.Limits.withDefaults(),
		workers: o.Workers, now: o.Now, yieldAfter: cmp.Or(o.YieldAfter, DefaultYieldAfter),
	}
	if l.workers == 0 {
		l.workers = max(1, runtime.GOMAXPROCS(0)/2)
	}
	if l.conv == nil {
		l.conv = convert.Default()
	}
	var err error
	if l.schemas == nil {
		if l.schemas, err = schemaidx.Embedded(); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOptions, err)
		}
	}
	if l.views == nil {
		if l.views, err = schemaview.Embedded(l.schemas); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrOptions, err)
		}
	}
	for _, v := range l.conv.Served() {
		if _, ok := l.schemas.Index(v); !ok {
			return nil, fmt.Errorf("%w: apiVersion %s has a converter but no schema", ErrOptions, v)
		}
		if _, ok := l.views.View(v); !ok {
			return nil, fmt.Errorf("%w: apiVersion %s has a converter but no schema view", ErrOptions, v)
		}
	}
	return l, nil
}

// Limits returns the limits the Loader applies, defaults filled in.
func (l *Loader) Limits() Limits { return l.limits }

// served returns the schema index of apiVersion when this release serves
// it: a converter and a schema both exist.
func (l *Loader) served(apiVersion string) (*schemaidx.Index, bool) {
	if !l.conv.Serves(apiVersion) {
		return nil, false
	}
	return l.schemas.Index(apiVersion)
}

// maxInt clamps n to the int range, for the int-typed limits of the
// stages a load calls.
func maxInt(n int64) int {
	return int(min(n, int64(math.MaxInt)))
}
