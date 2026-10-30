// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"bytes"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/api/schema"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
)

// views returns both embedded views of ruralz/v1alpha1 by name.
func views() map[string][]byte {
	return map[string][]byte{
		"authoring": schema.AuthoringV1alpha1(),
		"rendered":  schema.RenderedV1alpha1(),
	}
}

// refs collects every $ref value of a decoded schema document.
func refs(v any, out *[]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if s, ok := e.(string); ok && k == "$ref" {
				*out = append(*out, s)
				continue
			}
			if k == "const" || k == "enum" || k == "default" || k == "examples" {
				continue
			}
			refs(e, out)
		}
	case []any:
		for _, e := range t {
			refs(e, out)
		}
	default:
	}
}

// TestMetaViewsCompile compiles both schema files against draft 2020-12
// with the Ruralz vocabulary asserting the x-ruralz-* keyword shapes, each
// with the index loaded from its own bytes as Compile requires (never the
// authoring view with the rendered view's index), resolves every $ref and
// compiles every definition.
func TestMetaViewsCompile(t *testing.T) {
	for name, data := range views() {
		t.Run(name, func(t *testing.T) {
			viewOf(t, data)
			doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if s, _ := doc.(map[string]any)["$schema"].(string); s != "https://json-schema.org/draft/2020-12/schema" {
				t.Errorf("$schema = %q, want draft 2020-12", s)
			}
			c, err := newCompiler()
			if err != nil {
				t.Fatal(err)
			}
			const base = "urn:ruralz:meta"
			if err := c.AddResource(base, doc); err != nil {
				t.Fatal(err)
			}
			var all []string
			refs(doc, &all)
			defs, _ := doc.(map[string]any)["$defs"].(map[string]any)
			for def := range defs {
				all = append(all, "#/$defs/"+def)
			}
			slices.Sort(all)
			all = slices.Compact(all)
			for _, ref := range all {
				if _, err := c.Compile(base + ref); err != nil {
					t.Errorf("Compile(%s) error = %v", ref, err)
				}
			}
			if len(all) < 100 {
				t.Errorf("compiled %d references and definitions, want the whole schema", len(all))
			}
		})
	}
}

// TestMetaDispatch checks that both views dispatch all ten kinds and every
// registered Policy type.
func TestMetaDispatch(t *testing.T) {
	types := registry.New().Types()
	if len(types) != 23 {
		t.Fatalf("registry has %d Policy types, want the 23 of foundation pack section 10", len(types))
	}
	for name, data := range views() {
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		kinds := kindRefs(doc)
		if len(kinds) != 10 {
			t.Errorf("%s: root dispatches %d kinds, want 10", name, len(kinds))
		}
		if got, want := vocabularyKinds(t), slices.Sorted(maps.Keys(kinds)); !slices.Equal(got, want) {
			t.Errorf("%s: the vocabulary's x-ruralz-ref kinds = %v, the root dispatches %v", name, got, want)
		}
		defs, _ := doc["$defs"].(map[string]any)
		policySpec, _ := defs["PolicySpec"].(map[string]any)
		dispatched := map[string]bool{}
		all, _ := policySpec["allOf"].([]any)
		for _, e := range all {
			rule, _ := e.(map[string]any)
			cond, _ := rule["if"].(map[string]any)
			props, _ := cond["properties"].(map[string]any)
			typ, _ := props["type"].(map[string]any)
			if c, ok := typ["const"].(string); ok {
				dispatched[c] = true
			}
		}
		for _, typ := range types {
			if !dispatched[string(typ)] {
				t.Errorf("%s: PolicySpec has no config dispatch for type %s", name, typ)
			}
		}
	}
}

// vocabularyKinds returns the kinds x-ruralz-ref may name in the
// vocabulary meta-schema, sorted.
func vocabularyKinds(t *testing.T) []string {
	t.Helper()
	var meta struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(vocabularyMeta), &meta); err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(slices.Values(meta.Properties["x-ruralz-ref"].Enum))
}

// TestMetaVocabularyAssertsShapes checks that the vocabulary rejects a
// malformed x-ruralz-* keyword anywhere in a schema, and an unknown one
// ("the vocabulary asserting keyword shapes"), with the shapes of
// docs/architecture/02-configuration-model.md
// "Schema keywords that drive tooling": a kind, true, distinct classes, a
// level of at least 1.
func TestMetaVocabularyAssertsShapes(t *testing.T) {
	for _, tc := range []struct {
		kw   string
		ok   bool
		note string
	}{
		{`"x-ruralz-ref": "Upstream"`, true, ""},
		{`"x-ruralz-ref": ""`, false, "empty kind"},
		{`"x-ruralz-ref": 1`, false, "not a string"},
		{`"x-ruralz-ref": "Upstrem"`, false, "not one of the ten kinds"},
		{`"x-ruralz-ref": "Cluster"`, true, ""},
		{`"x-ruralz-secret": true`, true, ""},
		{`"x-ruralz-secret": "yes"`, false, "not a boolean"},
		{`"x-ruralz-secret": false`, false, "the documented value is true"},
		{`"x-ruralz-cel": {"variables": ["request"], "result": "bool"}`, true, ""},
		{`"x-ruralz-cel": {"variables": [], "result": "bool"}`, false, "no variables"},
		{`"x-ruralz-cel": {"variables": ["request"], "result": "int"}`, true, ""},
		{`"x-ruralz-cel": {"variables": ["request"], "result": "float"}`, false, "unknown result"},
		{`"x-ruralz-cel": {"variables": ["request"], "result": "bool", "cost": 1}`, false, "extra member"},
		{`"x-ruralz-list": {"type": "map", "key": "name"}`, true, ""},
		{`"x-ruralz-list": {"type": "set"}`, true, ""},
		{`"x-ruralz-list": {"type": "map"}`, false, "keyed without key"},
		{`"x-ruralz-list": {"type": "set", "key": "name"}`, false, "set with key"},
		{`"x-ruralz-list": {"type": "bag"}`, false, "unknown type"},
		{`"x-ruralz-impact": ["security", "traffic"]`, true, ""},
		{`"x-ruralz-impact": []`, false, "empty"},
		{`"x-ruralz-impact": ["performance"]`, false, "unknown class"},
		{`"x-ruralz-impact": ["security", "security"]`, false, "duplicate class"},
		{`"x-ruralz-since": 2`, true, ""},
		{`"x-ruralz-since": 1`, true, ""},
		{`"x-ruralz-since": 0`, false, "level 0 omits the keyword"},
		{`"x-ruralz-since": -1`, false, "negative"},
		{`"x-ruralz-since": "1"`, false, "not an integer"},
		{`"x-ruralz-validations": [{"rule": "self.a > 0", "message": "m"}]`, true, ""},
		{`"x-ruralz-validations": [{"message": "m"}]`, false, "no rule"},
		{`"x-ruralz-validations": {"rule": "x"}`, false, "not an array"},
		{`"x-ruralz-bogus": true`, false, "unknown keyword"},
	} {
		for _, wrap := range []string{`{%s}`, `{"properties": {"a": {"items": {%s}}}}`, `{"$defs": {"d": {%s}}, "$ref": "#/$defs/d"}`} {
			src := strings.Replace(wrap, "%s", tc.kw, 1)
			c, err := newCompiler()
			if err != nil {
				t.Fatal(err)
			}
			doc, err := jsonschema.UnmarshalJSON(strings.NewReader(src))
			if err != nil {
				t.Fatal(err)
			}
			if err := c.AddResource("urn:test:vocab", doc); err != nil {
				t.Fatal(err)
			}
			_, err = c.Compile("urn:test:vocab")
			if got := err == nil; got != tc.ok {
				t.Errorf("Compile(%s) error = %v, want ok %v (%s)", src, err, tc.ok, tc.note)
			}
		}
	}
}

// rawKeywords collects every schema object of doc that carries x-ruralz-*
// keywords, keyed by its JSON pointer.
func rawKeywords(v any, ptr string, out map[string]map[string]any) {
	switch t := v.(type) {
	case map[string]any:
		for k, e := range t {
			if strings.HasPrefix(k, "x-ruralz-") {
				if out[ptr] == nil {
					out[ptr] = map[string]any{}
				}
				out[ptr][k] = e
				continue
			}
			if k == "const" || k == "enum" || k == "default" || k == "examples" {
				continue
			}
			rawKeywords(e, ptr+"/"+strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"), out)
		}
	case []any:
		for i, e := range t {
			rawKeywords(e, ptr+"/"+strconv.Itoa(i), out)
		}
	default:
	}
}

// TestMetaVocabularyParsesKeywords checks that the vocabulary parses the
// x-ruralz-* keywords of every schema object of the rendered view into
// Schema.Extensions exactly as written, and that internal/config/schemaidx
// reads the same values at every declared property, with or without
// keywords (one keyword model, two parsers).
func TestMetaVocabularyParsesKeywords(t *testing.T) {
	data := schema.RenderedV1alpha1()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	c, err := newCompiler()
	if err != nil {
		t.Fatal(err)
	}
	const base = "urn:ruralz:meta"
	if err := c.AddResource(base, doc); err != nil {
		t.Fatal(err)
	}
	// parsed returns the vocabulary's parse of the schema object at ptr;
	// nil when it carries no keyword.
	parsed := func(ptr string) *keywords {
		sch, err := c.Compile(base + "#" + ptr)
		if err != nil {
			t.Fatalf("Compile(%s) error = %v", ptr, err)
		}
		for _, ext := range sch.Extensions {
			if kw, ok := ext.(*keywords); ok {
				return kw
			}
		}
		return nil
	}
	raw := map[string]map[string]any{}
	rawKeywords(doc, "", raw)
	if len(raw) < 50 {
		t.Fatalf("found %d schema objects with keywords, want the rendered view's", len(raw))
	}
	for _, ptr := range slices.Sorted(maps.Keys(raw)) {
		k := parsed(ptr)
		if k == nil {
			t.Errorf("%s: no parsed keywords in Extensions", ptr)
			continue
		}
		checkParsed(t, ptr, raw[ptr], k)
	}
	if n := checkIndexAgrees(t, doc, testView(t).Index(), parsed); n < 300 {
		t.Errorf("compared %d declared properties, want every property of the rendered view", n)
	}
}

// fromRaw decodes the x-ruralz-* keywords of one schema object directly,
// as the documented shapes read, without the vocabulary's parser.
func fromRaw(raw map[string]any) keywords {
	var k keywords
	k.ref, _ = raw["x-ruralz-ref"].(string)
	k.secret, _ = raw["x-ruralz-secret"].(bool)
	if v, ok := raw["x-ruralz-cel"].(map[string]any); ok {
		vars, _ := v["variables"].([]any)
		for _, e := range vars {
			s, _ := e.(string)
			k.celVariables = append(k.celVariables, s)
		}
		k.celResult, _ = v["result"].(string)
	}
	if v, ok := raw["x-ruralz-list"].(map[string]any); ok {
		k.listType, _ = v["type"].(string)
		k.listKey, _ = v["key"].(string)
	}
	if v, ok := raw["x-ruralz-impact"].([]any); ok {
		for _, e := range v {
			s, _ := e.(string)
			k.impact = append(k.impact, s)
		}
	}
	if v, ok := raw["x-ruralz-since"].(json.Number); ok {
		n, _ := strconv.Atoi(string(v))
		k.since = n
	}
	if v, ok := raw["x-ruralz-validations"].([]any); ok {
		for _, e := range v {
			r, _ := e.(map[string]any)
			rule, _ := r["rule"].(string)
			message, _ := r["message"].(string)
			k.validations = append(k.validations, validation{rule: rule, message: message})
		}
	}
	return k
}

// sameKeywords reports whether two parses agree on all seven keywords.
func sameKeywords(a, b keywords) bool {
	return a.ref == b.ref && a.secret == b.secret &&
		a.celResult == b.celResult && slices.Equal(a.celVariables, b.celVariables) &&
		a.listType == b.listType && a.listKey == b.listKey &&
		slices.Equal(a.impact, b.impact) && a.since == b.since &&
		slices.Equal(a.validations, b.validations)
}

// checkParsed compares the vocabulary's parse of one schema object with
// its raw keywords: every keyword, absent ones included.
func checkParsed(t *testing.T, ptr string, raw map[string]any, k *keywords) {
	t.Helper()
	if want := fromRaw(raw); !sameKeywords(*k, want) {
		t.Errorf("%s: vocabulary parsed %+v, the schema says %+v", ptr, *k, want)
	}
}

// closure returns ptrs with every $ref target and allOf member, nearest
// first, without duplicates: the schema objects schemaidx merges into one
// node (schemaidx expand).
func closure(doc any, ptrs []string) []string {
	var out []string
	seen := map[string]bool{}
	var add func(ptr string)
	add = func(ptr string) {
		if seen[ptr] {
			return
		}
		seen[ptr] = true
		out = append(out, ptr)
		obj := objectAt(doc, ptr)
		if ref, ok := obj["$ref"].(string); ok && strings.HasPrefix(ref, "#") {
			add(ref[1:])
		}
		all, _ := obj["allOf"].([]any)
		for i := range all {
			add(ptr + "/allOf/" + strconv.Itoa(i))
		}
	}
	for _, p := range ptrs {
		add(p)
	}
	return out
}

// objectAt returns the schema object at a JSON pointer of doc.
func objectAt(doc any, ptr string) map[string]any {
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch c := cur.(type) {
		case map[string]any:
			cur = c[tok]
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(c) {
				return nil
			}
			cur = c[i]
		default:
			return nil
		}
	}
	m, _ := cur.(map[string]any)
	return m
}

// escapePointer escapes one JSON pointer token (RFC 6901).
func escapePointer(tok string) string {
	return strings.ReplaceAll(strings.ReplaceAll(tok, "~", "~0"), "/", "~1")
}

// mergeParsed folds k into dst as schemaidx merges the keywords of the
// schema objects of one node: the nearest list, ref, cel and since win,
// secret and impact accumulate, validations concatenate.
func mergeParsed(dst *keywords, k *keywords) {
	if k == nil {
		return
	}
	if dst.listType == "" {
		dst.listType, dst.listKey = k.listType, k.listKey
	}
	if dst.ref == "" {
		dst.ref = k.ref
	}
	dst.secret = dst.secret || k.secret
	if dst.celResult == "" {
		dst.celVariables, dst.celResult = k.celVariables, k.celResult
	}
	for _, c := range k.impact {
		if !slices.Contains(dst.impact, c) {
			dst.impact = append(dst.impact, c)
		}
	}
	if dst.since == 0 {
		dst.since = k.since
	}
	dst.validations = append(dst.validations, k.validations...)
}

// checkIndexAgrees compares, for every declared property
// $defs/<def>/properties/<name> of doc, the keywords schemaidx reports
// with the vocabulary's parses of the schema objects schemaidx merges
// there, in both directions and for all seven keywords. It returns the
// number of properties compared.
func checkIndexAgrees(t *testing.T, doc any, idx *schemaidx.Index, parsed func(string) *keywords) int {
	t.Helper()
	defs, _ := doc.(map[string]any)["$defs"].(map[string]any)
	compared := 0
	for _, defName := range slices.Sorted(maps.Keys(defs)) {
		def, ok := idx.Def(defName)
		if !ok {
			t.Errorf("schemaidx has no definition %s", defName)
			continue
		}
		parts := closure(doc, []string{"/$defs/" + escapePointer(defName)})
		byName := map[string][]string{}
		for _, part := range parts {
			props, _ := objectAt(doc, part)["properties"].(map[string]any)
			for name := range props {
				byName[name] = append(byName[name], part+"/properties/"+escapePointer(name))
			}
		}
		for _, name := range slices.Sorted(maps.Keys(byName)) {
			ptr := "/$defs/" + defName + "/properties/" + name
			var want keywords
			for _, p := range closure(doc, byName[name]) {
				mergeParsed(&want, parsed(p))
			}
			prop, ok := def.Property(name)
			if !ok {
				t.Errorf("%s: schemaidx has no property %s", ptr, name)
				continue
			}
			compared++
			checkKeywords(t, ptr, prop.Keywords(), want)
		}
	}
	return compared
}

// checkKeywords compares schemaidx's keywords with the vocabulary's.
func checkKeywords(t *testing.T, ptr string, got schemaidx.Keywords, want keywords) {
	t.Helper()
	var impact schemaidx.Impact
	for _, class := range want.impact {
		c, ok := schemaidx.ParseImpact(class)
		if !ok {
			t.Errorf("%s: schemaidx does not know impact class %s", ptr, class)
		}
		impact |= c
	}
	vals := make([]validation, 0, len(got.Validations))
	for _, v := range got.Validations {
		vals = append(vals, validation{rule: v.Rule, message: v.Message})
	}
	cel := got.CEL != nil
	switch {
	case got.Ref != want.ref:
		t.Errorf("%s: schemaidx ref = %q, vocabulary %q", ptr, got.Ref, want.ref)
	case got.Secret != want.secret:
		t.Errorf("%s: schemaidx secret = %v, vocabulary %v", ptr, got.Secret, want.secret)
	case got.List.String() != want.listType || got.ListKey != want.listKey:
		t.Errorf("%s: schemaidx list = %q %q, vocabulary %q %q", ptr, got.List, got.ListKey, want.listType, want.listKey)
	case cel != (want.celResult != "") || (cel && (got.CEL.Result != want.celResult || !slices.Equal(got.CEL.Variables, want.celVariables))):
		t.Errorf("%s: schemaidx cel = %+v, vocabulary %v %q", ptr, got.CEL, want.celVariables, want.celResult)
	case got.Impact != impact:
		t.Errorf("%s: schemaidx impact = %q, vocabulary %q", ptr, got.Impact, want.impact)
	case got.Since != want.since:
		t.Errorf("%s: schemaidx since = %d, vocabulary %d", ptr, got.Since, want.since)
	case !slices.Equal(vals, want.validations):
		t.Errorf("%s: schemaidx validations = %+v, vocabulary %+v", ptr, vals, want.validations)
	default:
	}
}

// authoringView compiles the authoring view of ruralz/v1alpha1 with its
// own index (viewOf).
func authoringView(t *testing.T) *View {
	t.Helper()
	return viewOf(t, schema.AuthoringV1alpha1())
}

// viewOf compiles a view of ruralz/v1alpha1 with the index loaded from the
// same bytes, as Compile requires: the mapping reads dispatch, list types,
// secret marks and hint names from the index.
func viewOf(t *testing.T, data []byte) *View {
	t.Helper()
	idx, err := schemaidx.Load("ruralz/v1alpha1", data)
	if err != nil {
		t.Fatalf("schemaidx.Load() error = %v", err)
	}
	v, err := Compile(data, idx)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return v
}
