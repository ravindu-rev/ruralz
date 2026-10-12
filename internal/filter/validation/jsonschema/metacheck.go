// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"fmt"
	"slices"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// The library's meta-schema checks (architecture R-77, 07 F6).
// Before compiling a schema position, the library checks the position's
// whole keyword tree against the meta-schema (about 15 to 50 µs a
// subschema, compiling every pattern it holds), unless an earlier check
// covered the position: the document's keyword tree once, when the
// document is first used, and then every reference target outside it
// (under a member that is not a keyword, such as "x" in "#/x/if") that no
// earlier check covered, each time copying its bookkeeping, one entry per
// position checked so far (about 0.14 µs an entry). Two review findings
// follow. A target checked before its ancestor is checked again with the
// ancestor's tree: in the order the library meets references, a ladder of
// 59 references from the deepest target up checked a 3,900-member tree
// 59 times (5.3 s and 2.2 GB). And thousands of such targets copied the
// bookkeeping thousands of times (2,040 targets, 1.1 s and 574 MB).
//
// The compile therefore checks the targets apart first, in pointer order,
// an ancestor before its descendants (compileWalked), so a target in the
// keyword tree of an earlier one is never checked again; the walk
// simulates that order and bounds what remains (metaChecks). A target
// whose keyword tree still holds positions an earlier check covered (a
// reference to a member map such as "#/x/properties", whose members are
// read as keywords) checks them again, and they count again against
// MaxSubschemas, their JSON bytes against MaxSchemaBytes; every target
// checked apart counts the copy of its bookkeeping, the document's
// positions, resources and anchors, against maxCheckCopies (proposed).

// maxCheckCopies bounds the bookkeeping entries the library copies for the
// reference targets it checks apart: about 75 ms.
const maxCheckCopies = 1 << 19

// metaChecks simulates the library's meta-schema checks in the order
// compileWalked makes them, lists in checkOrder the targets checked apart,
// and reports a document whose repeated checks or copies exceed their
// bounds, at the reference to the target that crosses one.
func (w *walker) metaChecks() {
	checked := make(map[string]bool, len(w.positions))
	for _, p := range w.positions[:w.keywordTree] {
		checked[p] = true
	}
	targets := slices.Clone(w.targets)
	slices.SortStableFunc(targets, func(a, b refTarget) int { return comparePointers(a.ptr, b.ptr) })
	perCopy := len(w.positions) + 2*len(w.ids) + len(w.anchors)
	mc := metaCount{checks: len(w.positions), checked: checked}
	copies := 0
	for i, t := range targets {
		if checked[t.ptr] || (i > 0 && targets[i-1].ptr == t.ptr) {
			continue
		}
		if copies += perCopy; copies > maxCheckCopies {
			w.add(t.site, fmt.Sprintf("schema refers to too many subschemas outside its keywords for its %d subschemas: the compiler would copy more than %d entries to check them (move them under $defs)", len(w.positions), maxCheckCopies))
			return
		}
		w.checkOrder = append(w.checkOrder, t.ptr)
		v, _ := lookup(w.doc, t.ptr)
		if !mc.check(v, t.ptr, false) {
			if mc.checks > MaxSubschemas {
				w.add(t.site, fmt.Sprintf("schema has more than %d subschemas, counting again those the meta-schema check repeats under a reference such as this one, to a position whose keywords hold subschemas already checked", MaxSubschemas))
			} else {
				w.add(t.site, fmt.Sprintf("the meta-schema check repeats more than %d bytes of the schema under references such as this one, to a position whose keywords hold subschemas already checked", MaxSchemaBytes))
			}
			return
		}
	}
}

// metaCount is the state of the simulated checks: the positions checked,
// counted again for every repeat, the JSON bytes checked again, and the
// positions some check covered.
type metaCount struct {
	checks, again int
	checked       map[string]bool
	buf           []byte
}

// check simulates the check of the keyword tree of v at ptr; within is set
// below a position checked before, whose bytes already count. It reports
// false once a bound is exceeded.
func (mc *metaCount) check(v any, ptr string, within bool) bool {
	if mc.checked[ptr] {
		mc.checks++
		if !within {
			within = true
			e := jsonval.Encoder{Options: jsonval.EncodeOptions{MaxBytes: MaxSchemaBytes - mc.again + 1}}
			var err error
			if mc.buf, err = e.Append(mc.buf[:0], v); err != nil {
				return false
			}
			mc.again += len(mc.buf)
		}
	} else {
		mc.checked[ptr] = true
	}
	if mc.checks > MaxSubschemas || mc.again > MaxSchemaBytes {
		return false
	}
	obj, ok := v.(map[string]any)
	if !ok {
		return true
	}
	fits := true
	eachSubschema(obj, ptr, func(sv any, sp string) {
		fits = fits && mc.check(sv, sp, within)
	})
	return fits
}
