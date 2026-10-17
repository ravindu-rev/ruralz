// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonschema

import (
	"encoding/json"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/filter"
)

// ladderBottom returns {"properties":{...}} with n members whose schemas
// member returns.
func ladderBottom(n int, member func(i int) string) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"p%05d":%s`, i, member(i))
	}
	b.WriteString(`}}`)
	return b.String()
}

// refLadder returns the review finding's ladder: bottom nested depth if
// levels deep under the unknown member x, and rungs references in $defs to
// levels rungs-1 down to 0 of it, the deepest first, so the library meets
// every target before its ancestors.
func refLadder(depth, rungs int, bottom string) string {
	var b strings.Builder
	b.WriteString(`{"$defs":{`)
	for i := range rungs {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"r%04d":{"$ref":"#/x%s"}`, i, strings.Repeat("/if", rungs-1-i))
	}
	b.WriteString(`},"x":` + strings.Repeat(`{"if":`, depth) + bottom + strings.Repeat(`}`, depth) + `}`)
	return b.String()
}

// misalignedLadder returns bottom under levels of properties maps whose
// member is named items, with a reference to every properties map: read
// as a schema, each map's items member is a keyword, so every target's
// keyword tree holds all the trees below it, whatever the order.
func misalignedLadder(levels int, bottom string) string {
	var b strings.Builder
	b.WriteString(`{"$defs":{`)
	for i := range levels {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"r%04d":{"$ref":"#/x%s/properties"}`, i, strings.Repeat("/properties/items", levels-1-i))
	}
	b.WriteString(`},"x":` + strings.Repeat(`{"properties":{"items":`, levels) + bottom + strings.Repeat(`}}`, levels) + `}`)
	return b.String()
}

// outsideTargets returns pad properties and n references to n schemas
// under the unknown member x, each of which the library checks apart.
func outsideTargets(n, pad int) string {
	return outsideTargetsWith(n, pad, `"q%d":{"type":"string","minLength":1}`)
}

// outsideTargetsWith is outsideTargets with the properties prop formats
// with their index.
func outsideTargetsWith(n, pad int, prop string) string {
	var b strings.Builder
	b.WriteString(`{"properties":{`)
	for i := range pad {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, prop, i)
	}
	b.WriteString(`},"$defs":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"r%04d":{"$ref":"#/x/a%04d"}`, i, i)
	}
	b.WriteString(`},"x":{`)
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"a%04d":{"type":"string"}`, i)
	}
	b.WriteString(`}}`)
	return b.String()
}

// compileAlloc returns the bytes CompileSchema allocates for raw, failing
// the test when it does not compile.
func compileAlloc(t *testing.T, raw string) uint64 {
	t.Helper()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	s := mustCompile(t, raw)
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)
	return after.TotalAlloc - before.TotalAlloc
}

// TestReferenceLadder is the regression test of the review finding that
// the library checked a reference target outside the document's keywords
// against the meta-schema again under every ancestor referenced after it:
// a ladder of 59 references from the deepest target up checked a
// 3,900-member tree 59 times (5.3 s, 2.2 GB) and compiled eight of the
// largest patterns 59 times (2.1 s, 2.1 GB), in Check and Factory.Build
// alike (07 F6, a compile returns within 1 s; architecture R-77). The
// compile checks those targets first, ancestors first, so the ladder
// costs what one reference to its top costs; and every distinct pattern
// compiles once, so even a repeated check compiles none again. Allocated
// bytes, not time, so the test is deterministic.
//
// Requirements: 07 req 75 and F6.
func TestReferenceLadder(t *testing.T) {
	members := ladderBottom(1000, func(int) string { return `{"type":"string","minLength":1}` })
	patterns := ladderBottom(2, func(i int) string {
		return fmt.Sprintf(`{"type":"string","pattern":%q}`, "^"+largestPattern(tailRune(i), -2)+"$")
	})
	for _, tc := range []struct{ name, ladder, single string }{
		{"members", refLadder(58, 59, members), refLadder(58, 1, members)},
		{"patterns", refLadder(40, 41, patterns), refLadder(40, 1, patterns)},
		// The checks a misaligned ladder repeats compile no pattern again.
		{"patterns, repeated checks", misalignedLadder(28, patterns), misalignedLadder(1, patterns)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ladder, single := compileAlloc(t, tc.ladder), compileAlloc(t, tc.single)
			if ladder > 2*single {
				t.Errorf("the ladder allocates %d bytes, one reference %d: want at most twice", ladder, single)
			}
		})
	}
	// The walk orders the checks: the top of the aligned ladder is checked
	// apart, and it covers every rung below.
	w := newWalker(decodeInstance(t, refLadder(58, 59, members)))
	w.walk()
	if len(w.issues) > 0 || !slices.Equal(w.checkOrder, []string{"/x"}) {
		t.Errorf("ladder: issues %+v, checked apart %q, want only /x", w.issues, w.checkOrder)
	}
}

// idAnchor is a property, formatted with its index, that is a resource of
// its own with an anchor.
const idAnchor = `"q%[1]d":{"$id":"urn:q%[1]d","$anchor":"a%[1]d","type":"string"}`

// TestMetaChecksBound covers what the ordered checks leave (architecture
// R-77, proposed): a target whose keyword tree holds positions an earlier
// check covered checks them again, and they count again against
// MaxSubschemas and their bytes against MaxSchemaBytes; every target
// checked apart costs a copy of the document's bookkeeping, within
// maxCheckCopies (review finding: 2,040 such targets compiled for 1.1 s).
// Each bound admits the largest document of its shape below it and
// refuses the next, at the reference to the target that crosses it: the
// first reference to it that the walk's last pass, the compile's order,
// resolved. The positions an earlier check covered include the document's
// own keyword tree (audit class GC-02).
//
// Requirements: 07 req 75 and F6.
func TestMetaChecksBound(t *testing.T) {
	members := func(n int) string {
		return ladderBottom(n, func(int) string { return `{"type":"string","minLength":1}` })
	}
	enum := func(n int) string { return `{"enum":[` + strings.Repeat("0,", n) + `0]}` }
	for _, tc := range []struct {
		name, fits, refused, msg string
	}{
		{"repeated subschemas", misalignedLadder(30, members(100)), misalignedLadder(30, members(125)), "more than 4096 subschemas"},
		{"repeated bytes", misalignedLadder(29, enum(4000)), misalignedLadder(29, enum(5000)), "more than 262144 bytes"},
		{"copies", outsideTargets(125, 3800), outsideTargets(130, 3800), "more than 524288 entries"},
		// Minor review finding: a copy holds the resources, each twice, and
		// the anchors besides the positions, here four entries a property.
		{"copies of resources and anchors", outsideTargetsWith(115, 1000, idAnchor), outsideTargetsWith(135, 1000, idAnchor), "more than 524288 entries"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mustCompile(t, tc.fits)
			issues := compileIssues(t, tc.refused)
			if len(issues) != 1 || !strings.HasPrefix(issues[0].Pointer, "/$defs/r") || !strings.HasSuffix(issues[0].Pointer, "/$ref") || !strings.Contains(issues[0].Message, tc.msg) {
				t.Errorf("issues = %+v, want one at a reference containing %q", issues, tc.msg)
			}
		})
	}
	// Targets the keyword tree already holds are never checked apart.
	w := newWalker(decodeInstance(t, `{"$ref":"#/$defs/a","$defs":{"a":{"items":{"$ref":"#/$defs/a/items"}}}}`))
	w.walk()
	if len(w.issues) > 0 || len(w.checkOrder) > 0 {
		t.Errorf("targets in the keyword tree: issues %+v, checked apart %q", w.issues, w.checkOrder)
	}
	// repeatedTree(n) holds n positions under $defs and three targets
	// outside the keywords, /a, /b and /properties, the last a member map
	// whose member items is the keyword tree's /properties/items: n+6
	// positions, and checking /properties apart checks /properties/items
	// again. The root's reference finds /b first and /b's finds
	// /properties, but in the last pass, the compile's order, /a is
	// collected apart before /b and its reference resolves to /properties
	// first.
	repeatedTree := func(n int) string {
		pad := make([]string, n)
		for i := range pad {
			pad[i] = fmt.Sprintf(`"p%d":{}`, i)
		}
		return `{"$ref":"#/b","allOf":[{"$ref":"#/a"}],"a":{"$ref":"#/properties"},"b":{"$ref":"#/properties"},"properties":{"items":{}},"$defs":{` +
			strings.Join(pad, ",") + `}}`
	}
	repeated := []Issue{{"/a/$ref", fmt.Sprintf("schema has more than %d subschemas, counting again those the meta-schema check repeats under a reference such as this one, to a position whose keywords hold subschemas already checked", MaxSubschemas)}}
	// The positions of the document's keyword tree that a target's keyword
	// tree holds count again (walker.keywordTree; check finding: a mutation
	// that counted none of them passed every deterministic test): at
	// MaxSubschemas positions the repeat crosses it.
	t.Run("keyword-tree positions checked again", func(t *testing.T) {
		mustCompile(t, repeatedTree(MaxSubschemas-7))
		w := newWalker(decodeInstance(t, repeatedTree(MaxSubschemas-6)))
		w.walk()
		if len(w.positions) != MaxSubschemas || w.keywordTree != MaxSubschemas-3 || !slices.Equal(w.checkOrder, []string{"/a", "/b", "/properties"}) {
			t.Errorf("%d positions, keyword tree %d, checked apart %q, want %d, %d and /a, /b, /properties", len(w.positions), w.keywordTree, w.checkOrder, MaxSubschemas, MaxSubschemas-3)
		}
		refusedThrough(t, repeatedTree(MaxSubschemas-6), repeated)
	})
	// A finding names the first reference the last pass resolved to the
	// target, /a/$ref, not the one that found it, /b/$ref (walker.targets;
	// check finding: a mutation pairing every target with its finder passed
	// the suite).
	t.Run("the reference a finding names", func(t *testing.T) {
		w := newWalker(decodeInstance(t, repeatedTree(MaxSubschemas-6)))
		w.walk()
		if !slices.Equal(w.issues, repeated) {
			t.Errorf("issues = %+v, want %+v", w.issues, repeated)
		}
		var finder string
		if n := w.pointerTarget(rootNode, w.intern("/properties")); n >= 0 && w.nodes[n].known >= 0 {
			finder = w.siteTab[w.nodes[n].known].ptr
		}
		if want := []refTarget{{"/a", "/allOf/0/$ref"}, {"/b", "/$ref"}, {"/properties", "/a/$ref"}}; !slices.Equal(w.targets, want) || finder != "/b/$ref" {
			t.Errorf("targets %+v, /properties found by %q, want %+v and /b/$ref", w.targets, finder, want)
		}
	})
}

// additionalItemsCases are schemas whose compile compiles a subschema
// below additionalItems, which no meta-schema check of the library
// covers, with the pointer of the finding and the same subschema under
// $defs, which the library checks.
func additionalItemsCases() []struct{ name, schema, ptr, defs string } {
	zero := `{"multipleOf":0}`
	return []struct{ name, schema, ptr, defs string }{
		{"a pointer", `{"$ref":"#/additionalItems","additionalItems":` + zero + `}`, "/additionalItems/multipleOf", zero},
		{"an anchor", `{"$ref":"#a","additionalItems":{"$anchor":"a","multipleOf":0}}`, "/additionalItems/multipleOf", zero},
		{"a dynamic anchor", `{"$dynamicRef":"#d","additionalItems":{"$dynamicAnchor":"d","multipleOf":0}}`, "/additionalItems/multipleOf", zero},
		{"a resource", `{"$ref":"item.json","additionalItems":{"$id":"item.json","multipleOf":0}}`, "/additionalItems/multipleOf", zero},
		{
			"a member below", `{"type":"object","properties":{"price":{"$ref":"#/additionalItems/properties/p"}},"additionalItems":{"properties":{"p":{"type":"number","multipleOf":0}}}}`,
			"/additionalItems/properties/p/multipleOf", `{"type":"number","multipleOf":0}`,
		},
		{"a nested additionalItems", `{"$ref":"#/additionalItems/additionalItems","additionalItems":{"additionalItems":` + zero + `}}`, "/additionalItems/additionalItems/multipleOf", zero},
		// The round-8 fix through $recursiveRef (audit class GC-01, rows
		// CMP-26, CMP-34, REFS-13).
		{"a recursive reference", `{"$recursiveRef":"#/additionalItems","additionalItems":` + zero + `}`, "/additionalItems/multipleOf", zero},
		{"a recursive reference below", `{"items":{"$recursiveRef":"#/additionalItems/properties/p"},"additionalItems":{"properties":{"p":{"multipleOf":0}}}}`, "/additionalItems/properties/p/multipleOf", zero},
		{"an unknown type", `{"$ref":"#/additionalItems","additionalItems":{"type":"strng"}}`, "/additionalItems/type", `{"type":"strng"}`},
		{"a number type", `{"$ref":"#/additionalItems","additionalItems":{"type":5}}`, "/additionalItems/type", `{"type":5}`},
		{"a number enum", `{"$ref":"#/additionalItems","additionalItems":{"enum":5}}`, "/additionalItems/enum", `{"enum":5}`},
		{"a number required", `{"$ref":"#/additionalItems","additionalItems":{"required":5}}`, "/additionalItems/required", `{"required":5}`},
		{"a number property", `{"$ref":"#/additionalItems","additionalItems":{"properties":{"a":5}}}`, "/additionalItems/properties/a", `{"properties":{"a":5}}`},
	}
}

// TestAdditionalItemsChecked is the regression test of the review finding
// that the library never checks against the meta-schema what it compiles
// below additionalItems: the draft 2020-12 meta-schema does not define it,
// and the library marks the subschemas it collects there as processed, so
// a subschema a reference, an anchor or an $id made it compile escaped the
// check (a multipleOf of 0 compiled, and every number reaching it made the
// library panic in Handle and Validate; type "strng" was ignored). Each
// shape is now RZ-CFG-005 with the meta-schema's own message, the one the
// same subschema gets under $defs, through CompileSchema, Check and
// Factory.Build, without a panic; below an additionalItems nothing
// compiles, the rules leave alone (07 req 76).
//
// Requirements: 07 reqs 13, 75 and 76, architecture R-77.
func TestAdditionalItemsChecked(t *testing.T) {
	for _, tc := range additionalItemsCases() {
		t.Run(tc.name, func(t *testing.T) {
			want := compileIssues(t, `{"$defs":{"x":`+tc.defs+`}}`)
			if len(want) != 1 {
				t.Fatalf("under $defs: %+v, want one finding", want)
			}
			noPanic(t, "CompileSchema", func() {
				issues := compileIssues(t, tc.schema)
				if len(issues) != 1 || issues[0].Pointer != tc.ptr || issues[0].Message != want[0].Message {
					t.Errorf("CompileSchema = %+v, want %q at %s", issues, want[0].Message, tc.ptr)
				}
			})
			noPanic(t, "Check", func() {
				if got := runCheck(policyResource("body", tc.schema)); len(got) != 1 || got[0].Code != CodeConfig || got[0].Message != want[0].Message {
					t.Errorf("Check = %+v, want RZ-CFG-005 %q", got, want[0].Message)
				}
			})
			noPanic(t, "Factory.Build", func() {
				_, err := Factory{}.Build(t.Context(), filter.BuildEnv{Config: configOf(tc.schema)})
				if code, _ := errcode.CodeOf(err); code != CodeConfig {
					t.Errorf("Build = %v, want %s", err, CodeConfig)
				}
			})
		})
	}
	// Not compiled, not checked: an additionalItems no reference reaches,
	// and one below a referenced one. A valid subschema there compiles and
	// applies.
	for _, schema := range []string{
		`{"additionalItems":{"multipleOf":0}}`,
		`{"$ref":"#/additionalItems","additionalItems":{"additionalItems":{"multipleOf":0}}}`,
	} {
		s := mustCompile(t, schema)
		noPanic(t, "Validate", func() {
			if _, ok := s.Validate(json.Number("5")); !ok {
				t.Errorf("%s refused 5", schema)
			}
		})
	}
	s := mustCompile(t, `{"$ref":"#/additionalItems","additionalItems":{"type":"string"}}`)
	if _, ok := s.Validate(json.Number("5")); ok {
		t.Error("a referenced additionalItems of type string accepted 5")
	}
	// Only the top-most of the positions no check covers is checked, its
	// keyword tree with it, as far as another additionalItems.
	w := newWalker(decodeInstance(t, `{"$ref":"#/additionalItems/properties/a","allOf":[{"$ref":"#/additionalItems"},{"$ref":"#/additionalItems/additionalItems/not"}],`+
		`"additionalItems":{"properties":{"a":true},"additionalItems":{"not":{"items":true}}}}`))
	w.walk()
	if got, want := w.uncheckedPositions(), []string{"/additionalItems", "/additionalItems/additionalItems/not"}; len(w.issues) > 0 || !slices.Equal(got, want) {
		t.Errorf("uncheckedPositions = %q (issues %+v), want %q", got, w.issues, want)
	}
	// What the library checks with a target apart is covered.
	w = newWalker(decodeInstance(t, `{"$ref":"#/x/a","$defs":{"b":{"$ref":"#/x/a/properties/b"}},"x":{"a":{"properties":{"b":true}}}}`))
	w.walk()
	if got := w.uncheckedPositions(); len(w.issues) > 0 || !slices.Equal(w.checkOrder, []string{"/x/a"}) || len(got) > 0 {
		t.Errorf("a target's keyword tree: checked apart %q, unchecked %q (issues %+v), want /x/a and none", w.checkOrder, got, w.issues)
	}
}
