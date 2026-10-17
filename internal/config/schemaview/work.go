// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// yieldEvery is the number of work units between two Yield calls: the
// caller's worker checks its yield timer between resources and every 256
// nodes inside one (01 req 53).
const yieldEvery = 256

// Options configures one validation. The zero value validates with the
// default cap and without yielding.
type Options struct {
	// MaxDiagnostics is the run's diagnostic cap (01 req 50), or what
	// earlier stages left of it: MaxDiagnostics when zero or less, and
	// math.MaxInt-1 when math.MaxInt, so that the cap plus one does not
	// overflow (no resource reaches either). ValidateWith returns at most
	// MaxDiagnostics+1 diagnostics, the first in diag.List.Sort order:
	// MaxDiagnostics+1 results mean the cap was reached. The caller
	// appends the single RZ-CFG-001 "too many diagnostics" error, as the
	// loader's diag.Collector given the same cap does on the extra one.
	//
	// The cap also bounds what the mapping holds: at most 2*(cap+1)
	// diagnostics, pending secret findings at most twice those of cap+1
	// groups, and the memoized positions and path texts on their paths,
	// which grow between two sweeps by as much again or by sweepMin,
	// whichever is more (sweep.go), with the memo of the resource's large
	// objects and costly path elements, which grows with the resource,
	// not with its errors (TestMappingMemoryBounded: the same few
	// thousand positions and texts for 20,000 to 1,000,000 errors at a
	// cap of 10). It does not
	// bound the validator's own error tree: jsonschema/v6 builds the
	// whole tree, about 0.3 KB per error, before stage F maps it, so its
	// peak grows with the errors whatever the cap. The mapping releases
	// each error once mapped, so stage F's peak is about that tree's, not
	// the tree's plus the mapping's (sampled for the 1,000,000 type errors
	// of a 2 MB JSON document: 0.33 GB in the validator, 0.5 GB for all of
	// stage F). A YAML document holds at most MaxDocumentTokens nodes, so
	// at most as many errors; only MaxSourceBytes bounds a JSON document
	// (01 req 6), and no hostile-input fixture measures this peak yet:
	// WP-35 filed both as contract requests (a JSON token cap, a fixture
	// of one error per node, and an open question on the bound beside
	// OQ-configuration-model-18).
	MaxDiagnostics int
	// Yield, when set, is called every 256 units of stage F's own work, so
	// the caller's worker can yield the processor (01 req 53). A unit is
	// one of:
	//   - a node of the instance built for the validator, a byte of a
	//     member name it hashes, a byte of a number literal longer than
	//     maxPassBytes that the number model reads or copies, or a node of
	//     a tree with nil member values or elements copied with null nodes
	//     in their place (nullFilled);
	//   - a node the uniqueness walk reports, two per member of an object
	//     (schemaidx.Walk dispatches each object twice), a byte of a
	//     member name it hashes, a member it scans for a keyed entry's key,
	//     or a keyed entry, label or annotation the uniqueness and
	//     reserved-prefix checks visit;
	//   - a validation error or unknown member mapped, a path step
	//     resolved, a child step compared, a byte of a token compared with
	//     each child of its length or hashed to look it up, a byte of the
	//     token of a new step (looked up in the schema's members or parsed
	//     as an index), a member scanned, indexed or read by a dispatch, a
	//     byte of a member name compared in full, hashed into an index or
	//     looked up, a byte of a member name below a secret field looked
	//     up in its schema, or a node or a byte of a variable name read to
	//     name substituted variables;
	//   - a byte of a path segment rendered and interned, or a byte of the
	//     canonical JSON of a key or set element or of a number literal it
	//     parses;
	//   - a sort comparison, or a byte of the common prefix it reads of two
	//     long sibling segments or two member names;
	//   - a path, step, text or remembered prefix a memo sweep marks,
	//     visits or keeps, or a secret finding a prune compares or drops;
	//   - a diagnostic materialized.
	//
	// Stage F checks for a yield at every unit it charges, but it cannot
	// yield inside a span of work that one library call or one long text
	// does at once. Each such span is charged with its units right after
	// it, so a Yield call follows it, and between two Yield calls stage F
	// charges at most yieldEvery-1 units plus one span (TestLongestSpan).
	// 01 req 53 asks for a check every 256 nodes inside a unit, which the
	// first two spans below do not meet for a large resource; WP-35 filed
	// the exception and the bound as a contract request on 01 req 53:
	//   - the jsonschema/v6 Validate call, the longest: one span over the
	//     whole instance, charged with its nodes and the bytes of its
	//     member names and strings, which it hashes and may read in full
	//     (pattern, length, format, enum and const keywords), so at most
	//     the resource's nodes plus its bytes. It measured 0.1 to 0.9 µs
	//     per node, 0.73 s for a Gateway of 200,000 listeners (800,007
	//     nodes). A YAML document holds at most MaxDocumentTokens nodes;
	//     only MaxSourceBytes bounds a JSON document (01 req 6);
	//   - one schemaidx dispatch (Node.Select, which schemaidx.Walk also
	//     applies to each object) of one object, charged one unit per
	//     member, though each dispatch case scans the members (about
	//     0.1 s for a Policy spec of 1,000,002 members);
	//   - rendering and interning one path segment, such as a 16 MiB keyed
	//     entry's key or field name;
	//   - the canonical JSON of one key or set element
	//     (schemaidx.AppendItemJSON and ItemElem, in the uniqueness checks,
	//     the Walk and the path of a set element);
	//   - one scan of the common prefix of two long sibling segments, at
	//     most lcpMemo bytes unless the pair is then memoized
	//     (memo.commonPrefix);
	//   - hashing or comparing one long member name, token or variable
	//     name;
	//   - in a view with uniqueItems, the map lookup of one off-grid
	//     number's exact value (numberTags), which hashes the bytes charged
	//     while copying it.
	//
	// A message quotes at most maxQuoted code points of an authored text
	// and reads no more of it (quote, shorten). The validator's cost per
	// number is bounded: the number model hands it every number as a text
	// of at most numberModel.maxBytes bytes with an exponent of a few
	// hundred at most (numbers.go), far from the 10^6 at which
	// jsonschema/v6 dereferences nil. The package never reads the clock.
	Yield func()
}

// work counts the work units of one Validate call and calls the yield
// hook every yieldEvery of them. It belongs to one call.
type work struct {
	yield func()
	// units counts the work units since the last Yield call.
	units int
	// total counts the work units charged while a yield hook is set, for
	// tests; without a hook nothing is counted.
	total int
	// observe, set only by tests, sees every charge while a yield hook is
	// set: a charge of k units is a span of k units that could not yield
	// inside, which TestLongestSpan bounds.
	observe func(k int)
}

// tick counts one work unit.
func (w *work) tick() { w.charge(1) }

// charge counts k work units, as k ticks would, calling the yield hook
// once when they reach yieldEvery: k units charged at once were done in
// one span, after which one yield suffices.
func (w *work) charge(k int) {
	if w == nil || w.yield == nil || k <= 0 {
		return
	}
	if w.observe != nil {
		w.observe(k)
	}
	w.total += k
	if w.units += k; w.units >= yieldEvery {
		w.units = 0
		w.yield()
	}
}

// built is the instance value built for the validator with what building
// it counted.
type built struct {
	value any
	// nodes is the number of nodes converted.
	nodes int
	// keyBytes and stringBytes are the bytes of the member names and of
	// the strings converted: the validator hashes every member name and
	// may read every string in full (a pattern, length, format, enum or
	// const keyword).
	keyBytes, stringBytes int
	// rewritten and surrogates count the number literals handed in their
	// normalized form and as surrogates (numberModel.bound).
	rewritten, surrogates int
	// invalid is the first number node whose text is not an RFC 8259
	// number literal, which the tree contract excludes (tree.Node.Text);
	// the validator is not run on such an instance. invalidAt is the
	// nearest known source position at or above it.
	invalid   *tree.Node
	invalidAt tree.Pos
	// shadowed holds the values of the members that an earlier member of
	// the same object and key hides: the validator never sees them, so
	// the checks after it skip them too (mapper.shadowed); nil when no
	// object repeats a key.
	shadowed map[*tree.Node]bool
	// nils reports a member value or list element that is a nil node,
	// which the validator reads as null (nullFilled).
	nils bool
}

// instance converts n to the encoding/json data model the validator
// reads (map[string]any, []any, string, json.Number, bool or nil), as
// tree.Node.JSONValue does except that of two members with one key the
// first wins, as tree.Node.Get, locate (memo.index) and mapper.member
// resolve them, so the validator checks the member every diagnostic
// describes (review finding: the last won, so a finding about the second
// member was reported at the first); a later duplicate is not converted
// and its value is recorded in built.shadowed.
// Each converted node is a work unit, and so is each byte of a member
// name hashed, so that building the instance of a large resource yields
// (01 req 53). Each number is the text nm bounds:
// the literal itself, its normalized form or a surrogate with the
// properties the validator reads (numbers.go). A nil member value or
// list element is null, as tree.Node.JSONValue reads it (built.nils).
func (w *work) instance(n *tree.Node, nm *numberModel) built {
	var b built
	tags := newNumberTags(nm)
	var conv func(n *tree.Node, at tree.Pos) any
	// at is the nearest known source position above n.
	conv = func(n *tree.Node, at tree.Pos) any {
		b.nodes++
		w.tick()
		if n == nil {
			return nil
		}
		if n.Pos.Known() {
			at = n.Pos
		}
		switch n.Kind {
		case tree.KindBool:
			return n.Bool
		case tree.KindInt, tree.KindFloat:
			text, form, ok := nm.bound(n.Text, tags, w)
			if !ok {
				if b.invalid == nil {
					b.invalid, b.invalidAt = n, at
				}
				return nil
			}
			switch form {
			case rewritten:
				b.rewritten++
			case surrogate:
				b.surrogates++
			case asWritten:
			}
			return json.Number(text)
		case tree.KindString:
			b.stringBytes += len(n.Text)
			return n.Text
		case tree.KindMap:
			m := make(map[string]any, len(n.Members))
			for _, mem := range n.Members {
				b.nils = b.nils || mem.Value == nil
				// Hashing the name is a span charged with its bytes.
				w.charge(len(mem.Key))
				if _, dup := m[mem.Key]; dup {
					if mem.Value != nil {
						if b.shadowed == nil {
							b.shadowed = map[*tree.Node]bool{}
						}
						b.shadowed[mem.Value] = true
					}
					continue
				}
				b.keyBytes += len(mem.Key)
				m[mem.Key] = conv(mem.Value, at)
			}
			return m
		case tree.KindList:
			l := make([]any, len(n.Items))
			for i, it := range n.Items {
				b.nils = b.nils || it == nil
				l[i] = conv(it, at)
			}
			return l
		default:
			return nil
		}
	}
	b.value = conv(n, tree.Pos{})
	return b
}

// nullFilled returns n with every member value and list element that is
// a nil node replaced by a null node without a source position, copying
// only the objects and lists on the paths to them; n itself when it has
// none. The validator reads a nil node as null; the mapping and
// schemaidx read member values and elements by their kind, and a nil one
// made them panic (review finding: a nil labels value, keyed-list key or
// dispatch member such as a Policy's spec.type stopped the process). The
// copy is validated and mapped like any tree, so a nil value is reported
// exactly as a null one, at its nearest positioned ancestor (01 req 36).
// Each node visited is a work unit.
func nullFilled(n *tree.Node, w *work) *tree.Node {
	if n == nil {
		return &tree.Node{Kind: tree.KindNull}
	}
	w.tick()
	var members []tree.Member
	for i, m := range n.Members {
		if v := nullFilled(m.Value, w); v != m.Value {
			if members == nil {
				members = slices.Clone(n.Members)
			}
			members[i].Value = v
		}
	}
	var items []*tree.Node
	for i, it := range n.Items {
		if v := nullFilled(it, w); v != it {
			if items == nil {
				items = slices.Clone(n.Items)
			}
			items[i] = v
		}
	}
	if members == nil && items == nil {
		return n
	}
	c := *n
	if members != nil {
		c.Members = members
	}
	if items != nil {
		c.Items = items
	}
	return &c
}
