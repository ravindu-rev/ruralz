// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package precedence

import (
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// TestLocatorWithoutFiles covers Options.Files nil: positions in the
// resource's own file take hub.Resource.Source.File, as in a file-mode
// load; the example diagnostics keep their file, line and column.
func TestLocatorWithoutFiles(t *testing.T) {
	b, _ := loadBundle(t, shopBundle, "testdata/fixtures/019-exclude")
	_, ds, err := New(nil, Options{Body: bodyOracle{}}).Run(t.Context(), b, 1)
	if err != nil {
		t.Fatal(err)
	}
	const want = "routes/cart-grpc.yaml:11:7 error RZ-CFG-019 Route/cart-grpc spec.excludePolicies[name=ratelimit-global]: " +
		"cannot exclude a Policy with overridable: false (declared in policies/common.yaml:46:3)\n"
	if got := diagText(t, ds); got != want {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}

// TestLocatorFallbacks covers the nearest-ancestor and resource-start
// fallbacks: a missing field, a defaulted value (tree.StyleDefaulted
// points at its parent), a node from another file without a FileTable,
// a resource without a tree, canonical content without positions and the
// keyed-path fallback when tree and typed lists disagree.
func TestLocatorFallbacks(t *testing.T) {
	const self, other = tree.FileID(0), tree.FileID(1)
	pos := func(f tree.FileID, line, col int32) tree.Pos { return tree.Pos{File: f, Line: line, Column: col} }
	entry := func(name string, p tree.Pos) *tree.Node {
		return &tree.Node{Kind: tree.KindMap, Pos: p, Members: []tree.Member{
			{Key: "name", KeyPos: p, Value: &tree.Node{Kind: tree.KindString, Text: name, Pos: p}},
		}}
	}
	spec := &tree.Node{Kind: tree.KindMap, Pos: pos(self, 5, 3), Members: []tree.Member{
		{Key: "overridable", KeyPos: pos(self, 6, 3), Value: &tree.Node{Kind: tree.KindBool, Style: tree.StyleDefaulted}},
		{Key: "slot", KeyPos: pos(self, 7, 3), Value: &tree.Node{Kind: tree.KindString, Text: "s", Pos: pos(self, 7, 9)}},
		{Key: "policies", KeyPos: pos(self, 8, 3), Value: &tree.Node{Kind: tree.KindList, Pos: pos(self, 9, 5), Items: []*tree.Node{
			entry("a", pos(self, 9, 7)), entry("b", pos(other, 3, 7)), entry("c", tree.Pos{}),
		}}},
		{Key: "broken", KeyPos: pos(self, 12, 3)},
	}}
	root := &tree.Node{Kind: tree.KindMap, Pos: pos(self, 1, 1), Members: []tree.Member{{Key: "spec", KeyPos: pos(self, 4, 1), Value: spec}}}
	start := diag.Location{File: "a.yaml", Line: 1, Column: 1}
	r := &hub.Resource{ID: hub.ID{Kind: v1alpha1.KindRoute, Name: "r"}, Tree: root, Source: hub.Source{File: "a.yaml", Start: start}}
	at := func(line, col int) diag.Location { return diag.Location{File: "a.yaml", Line: line, Column: col} }
	field := func(names ...string) diag.Path {
		p := make(diag.Path, len(names))
		for i, n := range names {
			p[i] = diag.Field(n)
		}
		return p
	}

	files := &tree.FileTable{}
	files.Add(tree.File{Path: "a.yaml"})
	files.Add(tree.File{Path: "overlays/prod/r.yaml"})
	l, lf := locator{}, locator{files: files}
	tests := []struct {
		name string
		got  diag.Location
		want diag.Location
	}{
		{"value", l.path(r, field("spec", "slot"), false), at(7, 9)},
		{"key", l.path(r, field("spec", "slot"), true), at(7, 3)},
		{"missing field takes the parent", l.path(r, field("spec", "nope"), false), at(5, 3)},
		{"defaulted value takes the parent", l.path(r, field("spec", "overridable"), true), at(5, 3)},
		{"member without value", l.path(r, field("spec", "broken"), false), at(5, 3)},
		{"keyed entry", l.path(r, entryPath(fieldPolicies, "a"), false), at(9, 7)},
		{"missing keyed entry", l.path(r, entryPath(fieldPolicies, "zz"), false), at(9, 5)},
		{"entry by index", l.entry(r, fieldPolicies, 0, "a"), at(9, 7)},
		{"entry from another file without a table", l.entry(r, fieldPolicies, 1, "b"), start},
		{"entry from another file with a table", lf.entry(r, fieldPolicies, 1, "b"), diag.Location{File: "overlays/prod/r.yaml", Line: 3, Column: 7}},
		{"entry without a position", l.entry(r, fieldPolicies, 2, "c"), at(9, 5)},
		{"index and name disagree", l.entry(r, fieldPolicies, 0, "b"), start},
		{"index past the list", l.entry(r, fieldPolicies, 7, "a"), at(9, 7)},
		{"other list", l.entry(r, "excludePolicies", 0, "a"), at(5, 3)},
		{"no tree", l.path(&hub.Resource{Source: hub.Source{Start: start}}, field("spec"), false), start},
		{"no resource", l.path(nil, field("spec"), false), diag.Location{}},
		{"entry without tree", l.entry(&hub.Resource{Source: hub.Source{Start: start}}, fieldPolicies, 0, "a"), start},
	}
	for _, tc := range tests {
		if tc.got != tc.want {
			t.Errorf("%s: got %+v, want %+v", tc.name, tc.got, tc.want)
		}
	}

	// Canonical content: no positions and no file.
	canon := &hub.Resource{Tree: &tree.Node{Kind: tree.KindMap}}
	if got := l.path(canon, field("spec"), false); got != (diag.Location{}) {
		t.Errorf("canonical: %+v", got)
	}
	// A FileTable that does not know the file falls back to the name.
	if got := (locator{files: &tree.FileTable{}}).path(r, field("spec", "slot"), false); got != at(7, 9) {
		t.Errorf("unknown file ID: %+v", got)
	}
}
