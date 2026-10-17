// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package subst is stage E of the configuration pipeline: ${VAR} and
// ${VAR:-default} substitution in string scalars, run exactly once after
// overlay merge and before schema validation
// (docs/architecture/02-configuration-model.md "Environment
// substitution"; spec 01 requirements 26 to 31).
//
// Substitution rewrites the text of string scalars only, so it can never
// inject structure: a value holding a newline, ": ", "- " or "{" stays one
// scalar, and substituted text is never scanned again. A scalar that is
// exactly one expression is re-typed by the rendered schema at its
// position (an integer port, a boolean flag). $${ is a literal ${, and
// Escape writes a literal ${ back as $${ for rendering.
//
// Positions are read from the schema through internal/config/schemaidx
// (architecture R-3) with if/then dispatch applied to each object after
// its dispatch members (a Policy spec.type) were substituted. Mapping
// keys and the envelope fields apiVersion, kind and metadata.name are
// never substituted or unescaped: any ${ there, $${ included, is
// RZ-CFG-011 (01 req 16 and 27; a contract change request adds this
// exception to 01 req 28). Every x-ruralz-ref, x-ruralz-secret (with its
// subtree) and x-ruralz-cel position is forbidden too: an unescaped ${
// there is RZ-CFG-011 and nothing else is reported for that scalar, while
// $${ is read as a literal ${ (01 req 28), so a renderer escapes every
// literal ${ (01 req 31). 02 req 46 and the schemaidx doc of
// Info.NoSubstitution say instead that render writes no $${ in these
// fields, and 02 test plan item 19 expects a verbatim ${ in a CEL field;
// rendered that way, a $${ authored there renders as ${, which a second
// render reads as RZ-CFG-011, so the render is not idempotent. This
// package follows 01 req 28 and 31; a contract change request names the
// conflict for the owner.
//
// The package also owns both RZ-CFG-013 warnings (architecture 3.1): a
// variable whose name looks secret, at each use (01 req 30), and a
// credential written in the Bundle in any string outside an
// x-ruralz-secret subtree, mapping keys included (01 J row 013; keys are
// strings, 01 req 11). A use is a well-formed expression, in a scalar
// that substitutes and in one an error leaves as authored, which is then
// read leniently as the detectors read it, so the warning is the same
// whichever error a scalar has; a malformed scalar likewise reports
// RZ-CFG-010 at each such use of an undefined variable without default
// (01 req 27), so one run reports every error of the scalar. No
// RZ-CFG-013 warning names a credential, in its message or its path (01
// J row 013): a mapping key warning's path is the object holding the key,
// a set element or keyed entry whose text shows a credential is named by
// its index, and a path ends before any other key that shows one
// (walker.warnPath); a contract change request asks 01 req 48 and the J
// row to extend this to the diagnostics of every stage. The credential
// rules read the authored
// text of a scalar, never a variable's value: its literal text and every
// ${NAME:-default}'s default, taken or not, cut into runs at each ${NAME}
// without default (eachRun), whether substitution succeeds or an error
// leaves the authored text in place. The detectors read nothing else, so
// their result never depends on the Environment. The credential header
// literal rule also reads where each expression takes its text from, the
// variable source or the default an unset or empty variable takes, and
// the header name after substitution, the header the gateway sends, so
// its result depends on which variables the Environment sets: a value
// whose authored runs are only framing is a literal unless some
// expression takes its text from the source (walker.literal), and a name
// written as ${H} is a credential header when H names one; the warning
// then quotes the name as authored. A contract change request records
// this reading against the 01 J row.
//
// Substitution costs time linear in its input and its output (01 req 54):
// every byte of a scalar is read a bounded number of times and the scan
// allocates nothing per expression (scanEach), the RZ-CFG-010 hint is
// computed once per distinct name in time linear in the name (hints),
// the credential detectors read the authored text only, and the text
// substitution writes is bounded by Options.MaxOutputBytes for the whole
// render, because one variable used many times multiplies its value (01
// J row 003 asks for bounded heap against the same amplification in YAML
// aliases; the limit is a contract change request against 01 req 6).
// Each call records at most Options.MaxDiagnostics+1 diagnostics, so the
// caller's cap appends its RZ-CFG-001, and calls its Yield function every
// 256 work units (01 req 50 and 53). Each diagnostic carries at most
// maxShown bytes of any input text in each place, as config/canonical
// bounds them: a name its message quotes, the kind and name of its
// resource, and each member name, list key and set element of its path,
// so the text of all of them is linear in their count whatever the size
// of a key or name they repeat. The units are counted as the work is
// done, at most textStep bytes at a time, so the calls are spread over
// the work, never in a burst before or after a pass over a long text
// (meter.go): the scanner, the detectors, the copies into the result and
// every search read their text in windows, and schemaidx Select reads a
// proxy of at most the members its rules may test. What a step cannot
// interrupt is a single runtime operation on one value at memory speed
// (a map hash such as the Source's Lookup of a name, a string comparison,
// one allocation).
package subst

import (
	"strconv"
	"sync/atomic"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// Configuration codes this package raises (all registered in
// internal/errcode).
const (
	// CodeLimit: the text substitution writes in one render exceeds
	// Options.MaxOutputBytes (01 req 6 limits).
	CodeLimit = "RZ-CFG-001"
	// CodeMalformed: a ${ that is not ${NAME} or ${NAME:-default}, or a
	// default holding ${.
	CodeMalformed = "RZ-CFG-005"
	// CodeAPIVersion: a resource whose apiVersion or kind has no schema.
	// Stage C reports and drops such documents first; the check keeps
	// substitution from running without forbidden positions.
	CodeAPIVersion = "RZ-CFG-007"
	// CodeUndefined: an undefined variable without a default, one per
	// occurrence.
	CodeUndefined = "RZ-CFG-010"
	// CodeForbidden: ${ in a mapping key, an envelope field or another
	// forbidden position.
	CodeForbidden = "RZ-CFG-011"
	// CodeWarning: the RZ-CFG-013 warning for a secret-like variable name
	// or a credential-shaped literal; it never blocks a Revision.
	CodeWarning = "RZ-CFG-013"
)

// DefaultMaxDiagnostics is the diagnostics one Apply call records by
// default: the per-run cap of 01 req 50 (10,000, proposed).
const DefaultMaxDiagnostics = 10_000

// DefaultMaxOutputBytes is the text substitution writes in one render by
// default: MaxSourceBytes of 01 req 6 (64 MiB, target), so the rendered
// text is at most twice the source limit. The value is proposed; a
// contract change request asks the owner to record it in 01 req 6.
const DefaultMaxOutputBytes = 64 << 20

// yieldEvery is the number of work units between Yield calls (01 req 53:
// "every 256 nodes inside a unit"). A work unit is a node visited, a
// scanned segment, a variable name compared or byteUnit bytes read,
// written or compared, so the units also measure the walk's cost
// independently of the machine (TestScaleLinear).
const yieldEvery = 256

// byteUnit is the number of bytes one work unit stands for: a scalar's
// text read by the scan, the detectors or the hint search, and the text
// substitution writes.
const byteUnit = 64

// linearScan is the most variable names the Vars dedupe of one scalar
// scans one by one; past it a set takes over (01 req 54).
const linearScan = 8

// Source supplies variable values: an Environment's spec.variables in
// the CLI, the process environment in file-mode ruralzd. It must be safe
// for concurrent reads.
type Source interface {
	// Lookup returns the value of name and whether it is set.
	Lookup(name string) (string, bool)
}

// Map is a Source over a fixed set of variables, such as an Environment's
// spec.variables.
type Map map[string]string

// Lookup returns the value of name.
func (m Map) Lookup(name string) (string, bool) {
	v, ok := m[name]
	return v, ok
}

// Options configure a Substituter.
type Options struct {
	// Schemas holds the schema index of every served apiVersion.
	Schemas *schemaidx.Set
	// Files converts tree positions to diagnostic locations; nil leaves
	// locations empty.
	Files *tree.FileTable
	// Variables supplies the values; nil defines no variable.
	Variables Source
	// HintNames are the names an RZ-CFG-010 hint may suggest: the
	// Environment's spec.variables keys, in any order and with repeats (a
	// Go map's keys will do: the first undefined variable's walk sorts a
	// copy, counting that work, so hints never depend on the order). The
	// Substituter reads the slice then, so the caller must not change it
	// while the Substituter is in use. Leave it nil for the process
	// environment, whose names are never suggested (01 req 27).
	HintNames []string
	// MaxDiagnostics is the run's diagnostic cap, which counts warnings
	// too (01 req 50; DefaultMaxDiagnostics when zero or less), so one
	// resource cannot allocate more than the run may report (01 risk 1).
	// Each call records the first MaxDiagnostics+1 diagnostics in walk
	// order and stops once it has, leaving the rest of the resource
	// unsubstituted: MaxDiagnostics+1 results mean the call stopped
	// early. The caller appends the single RZ-CFG-001 "too many
	// diagnostics" error, as the loader's diag.Collector given the same
	// cap does on the extra one; that error, which a run of warnings
	// alone reaches too, keeps the incomplete resource from use.
	MaxDiagnostics int
	// MaxOutputBytes bounds the text substitution writes, summed over
	// every call on the Substituter (DefaultMaxOutputBytes when zero or
	// less): the new text of each scalar holding ${ that it rewrites,
	// literal parts, unescaped $${, defaults and values alike (a scalar
	// without ${, or one an error leaves as authored, writes nothing). It
	// is checked before a scalar's text is built, so no call allocates
	// past it. The call whose scalar would pass it stops there, and it
	// and every later call return LimitDiagnostic alone; see Substituter.
	MaxOutputBytes int64
	// Yield, when set, is what Apply calls every 256 work units inside a
	// resource, so the caller's worker can yield the processor (01 req
	// 53); the caller yields between resources itself. The units are
	// counted as the work is done, so the calls are spread over it and a
	// time-based Yield sees no burst (see the package doc). Apply on several
	// goroutines shares it, so it must then be safe for concurrent use;
	// a worker that tracks the time since its own last yield calls
	// ApplyWith with its own function instead. The package never reads
	// the clock.
	Yield func()
}

// Substituter applies substitution to the resources of one render. It is
// safe for concurrent use, so one Substituter serves every loader worker
// of the render: each worker calls ApplyWith with its own Yield. Its only
// mutable state is shared by those calls and safe for them: the memo of
// RZ-CFG-010 hints and the count of output bytes against
// Options.MaxOutputBytes. Build one Substituter per render (New costs one
// compilation of the credential detectors), so the output limit counts
// that render alone.
//
// Once the output limit is spent, the render has failed: every call
// returns LimitDiagnostic alone, whichever resource it walks. Whether the
// limit is spent never depends on the order or number of workers (it is
// spent exactly when the text every resource would write passes it), but
// which calls ran before it was spent does, so the caller keeps a single
// LimitDiagnostic as stage E's result and drops stage E's other
// diagnostics and resources (Spent reports the state).
type Substituter struct {
	opts      Options
	detectors []detector
	hints     *hints
	// written counts the output bytes of every call, never more than
	// opts.MaxOutputBytes; spent is set once a scalar would pass it.
	written atomic.Int64
	spent   atomic.Bool
}

// New returns a Substituter; it builds the credential detectors once and
// reads opts.HintNames only when a variable is undefined (Options).
func New(opts Options) *Substituter {
	if opts.MaxDiagnostics <= 0 {
		opts.MaxDiagnostics = DefaultMaxDiagnostics
	}
	if opts.MaxOutputBytes <= 0 {
		opts.MaxOutputBytes = DefaultMaxOutputBytes
	}
	return &Substituter{opts: opts, detectors: newDetectors(), hints: newHints(opts.HintNames, opts.MaxDiagnostics)}
}

// Apply is ApplyWith(res, Options.Yield).
func (s *Substituter) Apply(res *tree.Resource) diag.List { return s.ApplyWith(res, s.opts.Yield) }

// ApplyWith substitutes every string scalar of res in place and returns
// the stage E diagnostics (01 req 26 to 30): RZ-CFG-005 for a malformed
// expression, RZ-CFG-010 per undefined variable (in a malformed scalar
// too, read leniently), RZ-CFG-011 in forbidden positions and RZ-CFG-013
// warnings, at most Options.MaxDiagnostics+1, or LimitDiagnostic alone
// once the output limit is spent; RZ-CFG-007 instead when res has no
// schema. A scalar with an error keeps its authored text. Each
// substituted scalar records its variables in tree.Node.Vars, so schema
// validation names them when a re-typed value does not fit. res is
// rewritten in place ($${ unescaped, values written, scalars re-typed),
// so it is substituted once: a render for another Environment needs its
// own copy of the resources, re-parsed or cloned.
//
// yield, when not nil, is called every 256 work units inside res (01 req
// 53); it belongs to this call, so a worker's function may keep the time
// of its own last yield without a lock.
//
// Diagnostic paths name list entries by their authored key or element
// values (spec.listeners[name="${NAME}"].port), as the overlay stage
// does, never by substituted text: a stage E diagnostic never reveals a
// variable's value, a secret-like one included. The location (file,
// line, column) identifies the node in both the authored and the
// substituted tree; later stages name entries by their substituted keys.
// A path shows at most maxShown bytes of each key or element, and an
// RZ-CFG-013 warning's path shows no credential (see the package doc).
func (s *Substituter) ApplyWith(res *tree.Resource, yield func()) diag.List {
	return s.apply(res, yield, nil)
}

// apply is ApplyWith, recording how its work is spread in p when p is not
// nil (tests).
func (s *Substituter) apply(res *tree.Resource, yield func(), p *probe) diag.List {
	if res == nil || res.Root == nil || res.Root.Kind != tree.KindMap {
		return nil
	}
	if s.Spent() {
		return diag.List{s.LimitDiagnostic()}
	}
	if p != nil {
		// A call starts where its caller yields (the loader yields between
		// resources).
		p.gap = max(p.gap, p.since)
		p.since = 0
	}
	w := &walker{s: s, id: res.ID, yield: yield, probe: p}
	var root *schemaidx.Node
	served := false
	if s.opts.Schemas != nil {
		var x *schemaidx.Index
		if x, served = s.opts.Schemas.Index(res.APIVersion); served {
			root, _ = x.Resource(string(res.ID.Kind))
		}
	}
	if root == nil {
		if !w.envelope(res.Root) {
			w.unserved(res, served)
		}
		return w.diags
	}
	w.visitMap(res.Root, root, place{}, edge{})
	return w.diags
}

// unserved reports RZ-CFG-007 for res, whose apiVersion has no schema
// index, or whose kind has no schema in a served apiVersion, as overlay
// Apply does (stage C reports and drops such documents first): at the
// apiVersion value with path apiVersion, or at the kind value with path
// kind, each value quoted and clipped, or at the document start when the
// value has no position.
func (w *walker) unserved(res *tree.Resource, served bool) {
	member := "apiVersion"
	msg := "apiVersion " + strconv.Quote(clip(res.APIVersion)) + " is not served"
	if served {
		member = "kind"
		msg = "kind " + strconv.Quote(clip(string(res.ID.Kind))) + " is not served by apiVersion " + strconv.Quote(clip(res.APIVersion))
	}
	at := res.Start
	if v, found := w.get(res.Root, member); found && v.Pos.Known() {
		at = v.Pos
	}
	w.report(diag.Diagnostic{Code: CodeAPIVersion, Location: w.loc(at), Path: diag.Path{diag.Field(member)}, Message: msg})
}

// Spent reports whether the output limit is spent: the render has failed
// with LimitDiagnostic.
func (s *Substituter) Spent() bool { return s.spent.Load() }

// LimitDiagnostic is the RZ-CFG-001 error of a spent output limit. It
// names no file, resource or path, so every call that returns it returns
// the same text.
func (s *Substituter) LimitDiagnostic() diag.Diagnostic {
	return diag.Diagnostic{
		Code: CodeLimit, Severity: diag.SeverityError,
		Message: "substitution writes more than " + strconv.FormatInt(s.opts.MaxOutputBytes, 10) +
			" bytes of text in one render",
		Hint: "each use of a variable repeats its value: keep large values out of variables used often",
	}
}

// take counts n output bytes, n >= 0, and reports whether they fit the
// limit; once a count does not, the limit is spent and no later count
// fits. The total never passes the limit, so it cannot overflow whatever
// the workers add concurrently, and the limit is spent exactly when the
// counts of every call add up to more than it, in any order.
func (s *Substituter) take(n int64) bool {
	// bounded: a compare-and-swap retry, once per concurrent update.
	for {
		if s.spent.Load() {
			return false
		}
		w := s.written.Load()
		if n > s.opts.MaxOutputBytes-w {
			s.spent.Store(true)
			return false
		}
		if s.written.CompareAndSwap(w, w+n) {
			return true
		}
	}
}
