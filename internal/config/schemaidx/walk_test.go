// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaidx

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

const thing = `{
  "apiVersion": "test/v1", "kind": "Thing",
  "metadata": {"name": "t", "labels": {"team": "a"}},
  "spec": {
    "entries": [{"id": "e1", "val": "1s"}, {"val": "2s"}, {"id": 7}],
    "ordered": [{"id": "o1"}],
    "tags": ["b", "a"],
    "nums": [1.50, 2],
    "flags": [true],
    "objs": [{"b": 1, "a": "x"}],
    "raw": [{"k": 1}],
    "free": [{"deep": [1]}],
    "opts": {"anything": {"x": 1}},
    "secret": {"ref": {"name": "n"}},
    "target": "t2",
    "rule": "request.method == 'GET'",
    "variant": {"type": "x", "config": {"x": 1}},
    "unknown": {"y": 1}
  }
}`

// visit records one Walk position.
type visit struct {
	path string
	info Info
	node *tree.Node
}

func walkAll(t *testing.T, x *Index, kind, src string) map[string]visit {
	t.Helper()
	out := map[string]visit{}
	err := x.Walk(kind, parseTree(t, src), func(c *Cursor) error {
		p := c.Path().String()
		if _, dup := out[p]; dup {
			t.Errorf("Walk visited %q twice", p)
		}
		out[p] = visit{path: p, info: c.Info, node: c.Node}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWalkPathsAndInfo(t *testing.T) {
	// 01 req 25 (keyed-list and set element paths, dispatch by type), 01
	// req 28 (forbidden positions), 01 req 34 (key-aware paths), 02 req 17
	// (lists without keyword are atomic; set elements by their canonical
	// value, so the authored 1.50 is [item=1.5]), 02 req 18 (open members
	// are free), 02 req 64 (impact from the root, security under secrets).
	x := mustLoad(t, synthetic)
	v := walkAll(t, x, "Thing", thing)
	type want struct {
		known, free, secret, noSubst bool
		impact                       Impact
		def                          string
	}
	for path, w := range map[string]want{
		"":                                {known: true, def: "Thing"},
		"apiVersion":                      {known: true, noSubst: true},
		"kind":                            {known: true, noSubst: true},
		"metadata.name":                   {known: true, noSubst: true},
		"metadata.labels.team":            {known: true},
		"spec.entries[id=e1]":             {known: true, def: "Entry"},
		"spec.entries[id=e1].val":         {known: true, def: "Duration"},
		"spec.entries[1]":                 {known: true, def: "Entry"},
		"spec.entries[id=7]":              {known: true, def: "Entry"},
		"spec.ordered[id=o1].id":          {known: true},
		"spec.tags[item=b]":               {known: true},
		"spec.nums[item=1.5]":             {known: true},
		"spec.flags[item=true]":           {known: true},
		`spec.objs[item={"a":"x","b":1}]`: {known: true},
		"spec.raw[0].k":                   {free: true},
		"spec.free[0].deep[0]":            {free: true},
		"spec.opts.anything.x":            {free: true},
		"spec.secret":                     {known: true, secret: true, noSubst: true, impact: ImpactSecurity, def: "Secret"},
		"spec.secret.ref.name":            {known: true, secret: true, noSubst: true, impact: ImpactSecurity},
		"spec.target":                     {known: true, noSubst: true},
		"spec.rule":                       {known: true, noSubst: true},
		"spec.variant.config":             {known: true, impact: ImpactTraffic | ImpactAI, def: "XConfig"},
		"spec.variant.config.x":           {known: true, impact: ImpactTraffic | ImpactAI},
		"spec.unknown":                    {},
		"spec.unknown.y":                  {},
	} {
		got, ok := v[path]
		if !ok {
			t.Errorf("Walk did not visit %q", path)
			continue
		}
		i := got.info
		if i.Known() != w.known || i.Free != w.free || i.InSecret != w.secret || i.NoSubstitution != w.noSubst ||
			i.PathImpact != w.impact || i.Node.Def() != w.def {
			t.Errorf("%s: known %v free %v secret %v noSubst %v impact %s def %q; want %+v",
				path, i.Known(), i.Free, i.InSecret, i.NoSubstitution, i.PathImpact, i.Node.Def(), w)
		}
		// Walk and Lookup agree at every position they both resolve.
		li, lok := x.Lookup("Thing", parseTree(t, thing), mustParsePath(t, v, path))
		if w.known || w.free {
			if !lok || li != i {
				t.Errorf("%s: Lookup %+v %v, Walk %+v", path, li, lok, i)
			}
		} else if lok {
			t.Errorf("%s: Lookup resolved an unknown field", path)
		}
	}
	if len(v) != 52 {
		var paths []string
		for p := range v {
			paths = append(paths, p)
		}
		slices.Sort(paths)
		t.Errorf("Walk visited %d positions:\n%s", len(v), strings.Join(paths, "\n"))
	}
}

// mustParsePath finds the recorded diag.Path of a visited position.
func mustParsePath(t *testing.T, v map[string]visit, path string) diag.Path {
	t.Helper()
	var found diag.Path
	x := mustLoad(t, synthetic)
	_ = x.Walk("Thing", parseTree(t, thing), func(c *Cursor) error {
		if c.Path().String() == path {
			found = c.Path()
		}
		return nil
	})
	if _, ok := v[path]; !ok {
		t.Fatalf("no visit %q", path)
	}
	return found
}

func TestWalkControl(t *testing.T) {
	x := mustLoad(t, synthetic)
	res := parseTree(t, thing)
	// ErrSkipChildren skips a subtree; Walk returns nil.
	var paths []string
	err := x.Walk("Thing", res, func(c *Cursor) error {
		paths = append(paths, c.Path().String())
		if last, ok := c.Last(); ok && last.Name == "spec" {
			return ErrSkipChildren
		}
		if c.Depth() == 0 {
			if _, ok := c.Last(); ok {
				t.Error("Last at the root")
			}
		}
		return nil
	})
	if err != nil || slices.ContainsFunc(paths, func(p string) bool { return strings.HasPrefix(p, "spec.") }) {
		t.Errorf("skip: err %v paths %v", err, paths)
	}
	// Any other error stops the walk and is returned.
	stop := errors.New("stop")
	n := 0
	err = x.Walk("Thing", res, func(c *Cursor) error {
		n++
		if c.Path().String() == "spec.tags[item=a]" {
			return fmt.Errorf("wrapped: %w", stop)
		}
		return nil
	})
	if !errors.Is(err, stop) || n == 0 {
		t.Errorf("error: %v after %d visits", err, n)
	}
	// A member without a value (a tree under construction) is skipped.
	holey := parseTree(t, `{"spec":{"tags":["a"]}}`)
	holey.Members = append(holey.Members, tree.Member{Key: "metadata"})
	visits := 0
	if err := x.Walk("Thing", holey, func(*Cursor) error { visits++; return nil }); err != nil || visits != 4 {
		t.Errorf("holey walk: %d visits, %v", visits, err)
	}
	if err := x.Walk("Nothing", res, func(*Cursor) error { return nil }); err == nil {
		t.Error("Walk(unknown kind) succeeded")
	}
	if err := x.Walk("Thing", nil, func(*Cursor) error { return nil }); err == nil {
		t.Error("Walk(nil) succeeded")
	}
	// A retained Cursor.Path is a copy.
	var kept diag.Path
	_ = x.Walk("Thing", res, func(c *Cursor) error {
		if c.Path().String() == "spec.entries[id=e1]" {
			kept = c.Path()
		}
		return nil
	})
	if kept.String() != "spec.entries[id=e1]" {
		t.Errorf("kept path = %s", kept)
	}
}

func TestWalkEditsAreVisited(t *testing.T) {
	// 01 req 36 as the defaults stage uses Walk: a callback that adds the
	// absent defaults of each present object sees them visited, with their
	// own schema; and a callback that sets the dispatch member redirects
	// the children (overlay merges spec.type before descending).
	x := mustLoad(t, synthetic)
	res := parseTree(t, `{"apiVersion":"test/v1","kind":"Thing","metadata":{"name":"t"},"spec":{"variant":{}}}`)
	var defaulted []string
	err := x.Walk("Thing", res, func(c *Cursor) error {
		if c.Node.Kind == tree.KindMap {
			if c.Path().String() == "spec.variant" {
				c.Node.Set("type", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: "x"})
				c.Node.Set("config", tree.Pos{}, &tree.Node{Kind: tree.KindMap})
			}
			for name, p := range c.Schema().Defaults() {
				if _, ok := c.Node.Get(name); !ok {
					c.Node.Set(name, tree.Pos{}, p.DefaultNode())
				}
			}
		}
		if c.Node.Style == tree.StyleDefaulted {
			defaulted = append(defaulted, c.Path().String())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// spec's own defaults are appended after the authored variant member.
	want := []string{"spec.variant.config.x", "spec.added", "spec.count", "spec.obj", "spec.obj.a", "spec.obj.a[0]", "spec.obj.a[1]", "spec.obj.a[2]", "spec.obj.b", "spec.ratio", "spec.size", "spec.wait"}
	if !slices.Equal(defaulted, want) {
		t.Errorf("defaulted = %v\nwant %v", defaulted, want)
	}
	if extra, ok := res.At(pathOf("spec", "variant", "extra")); ok {
		t.Errorf("else default applied after the type was set: %+v", extra)
	}
}

func TestLookupElements(t *testing.T) {
	x := mustLoad(t, synthetic)
	res := parseTree(t, thing)
	for _, tc := range []struct {
		path diag.Path
		text string
	}{
		{pathOf("spec", "entries", diag.Keyed("id", "e1"), "val"), "1s"},
		{pathOf("spec", "entries", diag.Keyed("id", "7")), ""},
		{pathOf("spec", "tags", diag.Item("a")), "a"},
		{pathOf("spec", "nums", diag.Item(json.Number("1.5"))), "1.50"},
		{pathOf("spec", "flags", diag.Item(true)), ""},
		{pathOf("spec", "objs", diag.Item(json.RawMessage(`{"a":"x","b":1}`)), "a"), "x"},
		{pathOf("spec", "entries", 1, "val"), "2s"},
	} {
		info, ok := x.Lookup("Thing", res, tc.path)
		if !ok {
			t.Errorf("Lookup(%s) failed", tc.path)
			continue
		}
		n, found := res.At(tc.path)
		if tc.text != "" && (!found || n.Text != tc.text) {
			t.Errorf("tree.At(%s) = %+v", tc.path, n)
		}
		_ = info
	}
	// Elements that match nothing still resolve the schema (the instance
	// only drives dispatch).
	for _, p := range []diag.Path{
		pathOf("spec", "entries", diag.Keyed("id", "zz")),
		pathOf("spec", "entries", 99),
		pathOf("spec", "tags", diag.Item("zz")),
		pathOf("spec", "tags", diag.Item(1.5)),
		pathOf("spec", "nums", diag.Item(json.Number("9"))),
	} {
		if info, ok := x.Lookup("Thing", res, p); !ok || !info.Known() {
			t.Errorf("Lookup(%s) = %+v %v", p, info, ok)
		}
	}
	// 02 req 76: the schema path, the key of the (apiVersion, schema path)
	// lifecycle table, drops element keys and indexes.
	if sp, ok := x.SchemaPath("Thing", res, pathOf("spec", "entries", diag.Keyed("id", "e1"), "val")); !ok || sp != "spec.entries[].val" {
		t.Errorf("SchemaPath = %q %v", sp, ok)
	}
	if _, ok := x.SchemaPath("Thing", res, pathOf("spec", "nope")); ok {
		t.Error("SchemaPath(unknown) resolved")
	}
	if sp, ok := x.SchemaPath("Thing", res, nil); !ok || sp != "" {
		t.Errorf("SchemaPath(root) = %q %v", sp, ok)
	}
}

func TestItemElem(t *testing.T) {
	// 01 req 34 and 02 req 17: key-aware elements per list type; set
	// elements by their RFC 8785 value, so equal numbers share one element
	// (02 req 69 diff paths carry the same value).
	spec := thingSpec(t, mustLoad(t, synthetic))
	list := func(name string) *Node { n, _ := spec.Property(name); return n }
	for _, tc := range []struct {
		list string
		item string
		want string
	}{
		{"entries", `{"id":"e 1"}`, `[id="e 1"]`},
		{"entries", `{"id":{"a":1}}`, `[3]`},
		{"entries", `{"x":1}`, `[3]`},
		{"ordered", `{"id":"o"}`, `[id=o]`},
		{"tags", `"GET"`, `[item=GET]`},
		{"nums", `2.50`, `[item=2.5]`},
		{"nums", `2.5`, `[item=2.5]`},
		{"nums", `25e-1`, `[item=2.5]`},
		{"nums", `1E2`, `[item=100]`},
		{"nums", `-0`, `[item=0]`},
		{"nums", `-0.0`, `[item=0]`},
		{"nums", `123456789012345`, `[item=123456789012345]`},
		{"nums", `9007199254740993`, `[item=9007199254740992]`},
		{"nums", `1e400`, `[item=1e400]`},
		{"flags", `false`, `[item=false]`},
		{"objs", `{"z":[1,{"b":null,"a":0.10}],"a":"\u0001"}`, `[item={"a":"\u0001","z":[1,{"a":0.1,"b":null}]}]`},
		{"objs", `null`, `[3]`},
		{"raw", `{"k":1}`, `[3]`},
		{"free", `1`, `[3]`},
	} {
		got := diag.Path{ItemElem(list(tc.list), parseTree(t, tc.item), 3)}.String()
		if got != tc.want {
			t.Errorf("ItemElem(%s, %s) = %s, want %s", tc.list, tc.item, got, tc.want)
		}
	}
	if got := (diag.Path{ItemElem(nil, nil, 2)}).String(); got != "[2]" {
		t.Errorf("ItemElem(nil) = %s", got)
	}
}

func TestAppendItemJSON(t *testing.T) {
	// RFC 8785: strings (3.2.2.2), numbers (3.2.2.3, Appendix B) and member
	// order by UTF-16 code units (3.2.3).
	num := func(s string) *tree.Node {
		if isIntegerLiteral(s) {
			return &tree.Node{Kind: tree.KindInt, Text: s}
		}
		return &tree.Node{Kind: tree.KindFloat, Text: s}
	}
	for _, tc := range []struct {
		in   *tree.Node
		want string
	}{
		{num("0.0"), "0"},
		{num("-0.0"), "0"},
		{num("1.0"), "1"},
		{num("1E2"), "100"},
		{num("0.05"), "0.05"},
		{num("0.000001"), "0.000001"},
		{num("1e-7"), "1e-7"},
		{num("1e21"), "1e+21"},
		{num("1e20"), "100000000000000000000"},
		{num("333333333.3333333"), "333333333.3333333"},
		{num("5e-324"), "5e-324"},
		{num("1.7976931348623157e308"), "1.7976931348623157e+308"},
		{num("1.2345678901234568e20"), "123456789012345680000"},
		{num("-1.5e-9"), "-1.5e-9"},
		{num("4.5"), "4.5"},
		{num("1e400"), "1e400"},
		{num("100"), "100"},
		{num("-7"), "-7"},
		{num("-0"), "0"},
		{num("123456789012345"), "123456789012345"},
		{num("-123456789012345"), "-123456789012345"},
		{num("9007199254740993"), "9007199254740992"},
		{num("12345678901234567890"), "12345678901234567000"},
		{num("1000000000000000000000"), "1e+21"},
		{&tree.Node{Kind: tree.KindString, Text: "\"\\/\u007f\u2028\U0001F600\b\t\n\f\r\u001f"}, `"\"\\/` + "\u007f\u2028\U0001F600" + `\b\t\n\f\r\u001f"`},
		{&tree.Node{Kind: tree.KindBool, Bool: true}, "true"},
		{&tree.Node{Kind: tree.KindNull}, "null"},
		{nil, "null"},
		{parseTree(t, `{"\u20ac":1,"\r":2,"\ufb33":3,"1":4,"\ud83d\ude00":5,"\u0080":6,"\u00f6":7}`), "{\"\\r\":2,\"1\":4,\"\u0080\":6,\"\u00f6\":7,\"\u20ac\":1,\"\U0001F600\":5,\"\uFB33\":3}"},
		{parseTree(t, `[[],{}]`), `[[],{}]`},
		// Key order: a prefix first, a surrogate pair before U+FB33.
		{parseTree(t, `{"ab":1,"a":2,"\ufb33":3,"\ud83d\ude00":4}`), "{\"a\":2,\"ab\":1,\"\U0001F600\":4,\"\uFB33\":3}"},
		{num("4.9406564584124654e-324"), "5e-324"}, // math.SmallestNonzeroFloat64
	} {
		if got := string(AppendItemJSON(nil, tc.in)); got != tc.want {
			t.Errorf("AppendItemJSON = %s, want %s", got, tc.want)
		}
	}
}

func TestAppendItemJSONMatchesJSONVal(t *testing.T) {
	// Differential: a set element's item form is the RFC 8785 form that
	// config/canonical writes through jsonval.AppendCanonical, so number
	// form and member order cannot diverge between diagnostics and the
	// canonical form.
	for _, src := range []string{
		`0`, `-0`, `-0.0`, `1.0`, `1E2`, `1e-7`, `0.000001`, `1e20`, `1e21`, `-1.5e-9`, `4.50`,
		`333333333.3333333`, `5e-324`, `4.9406564584124654e-324`, `1e-400`, `1.7976931348623157e308`,
		`9007199254740991`, `-9007199254740991`, `9007199254740992`, `9007199254740993`,
		`12345678901234567890`, `1000000000000000000000`, `11e17`, `123456789012345680000`,
		`"\"\\/\u007f\u2028\ud83d\ude00\b\t\n\f\r\u001f<>&"`,
		`{"\u20ac":1,"\r":2,"\ufb33":3,"1":4,"\ud83d\ude00":5,"\u0080":6,"\u00f6":7,"":8,"ab":9,"a":10}`,
		`[1.50,{"b":[true,false,null],"a":{"y":2.0,"x":1e2}},[],{}]`,
	} {
		v, _, err := jsonval.Decode([]byte(src), jsonval.Options{})
		if err != nil {
			t.Fatalf("jsonval.Decode(%s): %v", src, err)
		}
		want, err := jsonval.AppendCanonical(nil, v)
		if err != nil {
			t.Fatalf("jsonval.AppendCanonical(%s): %v", src, err)
		}
		if got := AppendItemJSON(nil, parseTree(t, src)); string(got) != string(want) {
			t.Errorf("AppendItemJSON(%s) = %s, jsonval.AppendCanonical = %s", src, got, want)
		}
	}
}

// keywordSchema has dispatch keywords on branch objects and reference and
// CEL keywords on arrays, which the committed schema does not use.
const keywordSchema = `{"properties":{"kind":{"enum":["K"]}},
  "allOf":[{"if":{"properties":{"kind":{"const":"K"}},"required":["kind"]},"then":{"$ref":"#/$defs/K"}}],
  "$defs":{
    "K":{"type":"object","properties":{"kind":{"const":"K"},"spec":{"$ref":"#/$defs/S"}}},
    "S":{"type":"object","properties":{
      "names":{"type":"array","items":{"type":"string"},"x-ruralz-ref":"K","x-ruralz-list":{"type":"set"}},
      "rules":{"type":"array","items":{"type":"array","items":{"type":"string"}},
        "x-ruralz-cel":{"variables":["request"],"result":"bool"}},
      "objs":{"type":"array","items":{"type":"object","properties":{"n":{"type":"string"}}},"x-ruralz-ref":"K"},
      "plain":{"type":"array","items":{"type":"string"}},
      "m":{"type":"object","additionalProperties":{"type":"string"},"x-ruralz-ref":"K"},
      "pm":{"type":"object","additionalProperties":{"type":"string"}},
      "v":{"type":"object","properties":{"type":{"type":"string"},"val":{"type":"string"}},"allOf":[
        {"if":{"properties":{"type":{"const":"s"}},"required":["type"]},
         "then":{"x-ruralz-secret":true,"x-ruralz-impact":["security"]},
         "else":{"x-ruralz-impact":["traffic"]}},
        {"if":{"properties":{"type":{"const":"r"}},"required":["type"]},"then":{"x-ruralz-ref":"K"}}]}}}}}`

func TestBranchAndArrayKeywords(t *testing.T) {
	// 01 req 28 and 02 req 64: keywords on a then or else branch object
	// count for the dispatched position and its subtree like keywords on
	// the field; the elements of an x-ruralz-ref or x-ruralz-cel array
	// (nested arrays included) and the values of an x-ruralz-ref typed map
	// are forbidden positions too, while members of an object element are
	// not. Walk and Lookup agree.
	x := mustLoad(t, keywordSchema)
	type want struct {
		secret, noSubst bool
		impact          Impact
	}
	for _, tc := range []struct {
		src   string
		paths map[string]want
	}{
		{
			`{"kind":"K","spec":{"names":["a"],"rules":[["x"]],"objs":[{"n":"y"}],"plain":["p"],"v":{"type":"s","val":"q"},` +
				`"m":{"a":"x"},"pm":{"b":"y"}}}`,
			map[string]want{
				"spec.names":         {noSubst: true},
				"spec.names[item=a]": {noSubst: true},
				"spec.rules[0]":      {noSubst: true},
				"spec.rules[0][0]":   {noSubst: true},
				"spec.objs[0]":       {noSubst: true},
				"spec.objs[0].n":     {},
				"spec.plain[0]":      {},
				"spec.v":             {secret: true, noSubst: true, impact: ImpactSecurity},
				"spec.v.type":        {secret: true, noSubst: true, impact: ImpactSecurity},
				"spec.v.val":         {secret: true, noSubst: true, impact: ImpactSecurity},
				"spec.m":             {noSubst: true},
				"spec.m.a":           {noSubst: true},
				"spec.pm.b":          {},
			},
		},
		{
			`{"kind":"K","spec":{"v":{"type":"t","val":"q"}}}`,
			map[string]want{
				"spec.v":     {impact: ImpactTraffic},
				"spec.v.val": {impact: ImpactTraffic},
			},
		},
		{
			`{"kind":"K","spec":{"v":{"type":"r","val":"q"}}}`,
			map[string]want{
				"spec.v":     {noSubst: true, impact: ImpactTraffic},
				"spec.v.val": {impact: ImpactTraffic},
			},
		},
	} {
		res := parseTree(t, tc.src)
		got := map[string]Info{}
		paths := map[string]diag.Path{}
		if err := x.Walk("K", res, func(c *Cursor) error {
			got[c.Path().String()], paths[c.Path().String()] = c.Info, c.Path()
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for path, w := range tc.paths {
			i, ok := got[path]
			if !ok || i.InSecret != w.secret || i.NoSubstitution != w.noSubst || i.PathImpact != w.impact {
				t.Errorf("%s %s: visited %v secret %v noSubst %v impact %s; want %+v",
					tc.src, path, ok, i.InSecret, i.NoSubstitution, i.PathImpact, w)
			}
			if l, ok := x.Lookup("K", res, paths[path]); !ok || l != i {
				t.Errorf("%s %s: Lookup %+v %v, Walk %+v", tc.src, path, l, ok, i)
			}
		}
	}
	// A callback that changes the dispatch member hands its children the
	// keywords of the new branch.
	res := parseTree(t, `{"kind":"K","spec":{"v":{"type":"t","val":"q"}}}`)
	var val Info
	if err := x.Walk("K", res, func(c *Cursor) error {
		switch c.Path().String() {
		case "spec.v":
			c.Node.Set("type", tree.Pos{}, &tree.Node{Kind: tree.KindString, Text: "s"})
		case "spec.v.val":
			val = c.Info
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !val.InSecret || !val.NoSubstitution || val.PathImpact != ImpactSecurity {
		t.Errorf("spec.v.val after the edit: %+v", val)
	}
	if l, _ := x.Lookup("K", res, pathOf("spec", "v", "val")); l != val {
		t.Errorf("Lookup after the edit %+v, Walk %+v", l, val)
	}
}

func TestFieldsReportBranchKeywords(t *testing.T) {
	// R-62: a then or else branch that annotates the dispatched object
	// itself lists that object again with its Dispatch, so Fields reports
	// the secret, reference and impact markers Lookup and Walk apply.
	x := mustLoad(t, keywordSchema)
	var got []Field
	for _, f := range x.Fields() {
		if f.Path == "spec.v" && len(f.Where) > 0 {
			got = append(got, f)
		}
	}
	type want struct {
		secret bool
		ref    string
		impact Impact
	}
	wants := map[string]want{
		`type="s"`: {secret: true, impact: ImpactSecurity},
		`type="r"`: {ref: "K", impact: ImpactTraffic},
		"none":     {impact: ImpactTraffic},
	}
	if len(got) != len(wants) {
		t.Fatalf("Fields lists spec.v under %d dispatches, want %d: %+v", len(got), len(wants), got)
	}
	for _, f := range got {
		d := f.Where[len(f.Where)-1]
		key := "none"
		if d.Conditions != nil {
			if len(d.Conditions) != 1 {
				t.Errorf("spec.v: conditions %+v", d.Conditions)
				continue
			}
			key = fmt.Sprintf("%s=%q", d.Conditions[0].Member, d.Conditions[0].Value)
		}
		w, ok := wants[key]
		if !ok {
			t.Errorf("spec.v under an unexpected dispatch %s", key)
			continue
		}
		delete(wants, key)
		if kw := f.Node.Keywords(); kw.Secret != w.secret || kw.Ref != w.ref || kw.Impact != w.impact {
			t.Errorf("spec.v under %s: secret %v ref %q impact %s; want %+v", key, kw.Secret, kw.Ref, kw.Impact, w)
		}
		res, path := instanceFor(f)
		if info, ok := x.Lookup(f.Kind, res, path); !ok || info.Node != f.Node {
			t.Errorf("spec.v under %s: Lookup %+v %v resolves another node", key, info, ok)
		}
	}
	if len(wants) > 0 {
		t.Errorf("spec.v not listed under %v", wants)
	}
}
