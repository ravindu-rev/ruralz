// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"math/big"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/overlay"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// TestSecretKeywordsNeverEchoed checks, on the synthetic view, that no
// form of an RZ-CFG-012 diagnostic contains a value from the secret,
// whatever keyword the value fails inside it: const and maxLength (the
// default reason), not (no cause), pattern and type (01 req 33, test plan
// 012; review finding: no test reached the default reason, so one that
// quoted the instance survived).
func TestSecretKeywordsNeverEchoed(t *testing.T) {
	const secret = "SENTINEL-9c1d"
	v := syntheticView(t)
	for _, ref := range []string{
		`{"id": "a", "c": "` + secret + `"}`,
		`{"id": "a", "m": "` + secret + `"}`,
		`{"id": "a", "nt": "` + secret + `"}`,
		`{"id": "` + secret + `"}`,
		`{"id": "a", "n": "` + secret + `"}`,
		`{"id": "a", "c": "` + secret + `", "m": "` + secret + `", "nt": "` + secret + `", "one": 7, "min": 1}`,
	} {
		got := validateWith(t, v, thing(`{"secret": {"ref": `+ref+`}}`))
		if len(got) != 1 || got[0].Code != CodeLiteral {
			t.Errorf("ref %s: Validate() = %s, want one %s", ref, texts(got), CodeLiteral)
			continue
		}
		var js strings.Builder
		if err := diag.WriteJSON(&js, got); err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{texts(got), js.String()} {
			if strings.Contains(out, "SENTINEL") {
				t.Errorf("ref %s: diagnostic output contains the secret:\n%s", ref, out)
			}
		}
	}
}

// TestSecretReasonsWithoutSchema maps hand-built validation errors at a
// secret position, for the kinds and shapes no view this package compiles
// produces there: a type error whose schema location the view does not
// resolve (the reason names the types the error wants), wrappers with no
// cause, and keywords that take the default reason, among them errors
// that hold a value. Each reason is worded from the schema and the kind
// alone, never the value (01 req 33; review finding: these branches had
// no test).
func TestSecretReasonsWithoutSchema(t *testing.T) {
	v := syntheticView(t)
	at := []string{"spec", "secret", "ref", "n"}
	for _, tc := range []struct {
		name   string
		kind   jsonschema.ErrorKind
		reason string
	}{
		{"type without a type keyword", &kind.Type{Got: "string", Want: []string{"integer", "null"}}, "ref.n must be an integer or null"},
		{"wrapper without a cause", &kind.AllOf{}, "ref.n is not valid"},
		{"not", &kind.Not{}, "ref.n is not valid"},
		{"oneOf matched twice", &kind.OneOf{Subschemas: []int{0, 1}}, "ref.n is not valid"},
		{"anyOf without a cause", &kind.AnyOf{}, "ref.n is not valid"},
		{"const with a value", &kind.Const{Got: "SENTINEL", Want: "x"}, "ref.n is not valid"},
		{"multipleOf", &kind.MultipleOf{Got: big.NewRat(7, 1), Want: big.NewRat(5, 1)}, "ref.n is not valid"},
		{"maxLength", &kind.MaxLength{Got: 9, Want: 3}, "ref.n is not valid"},
		{"minLength", &kind.MinLength{Got: 0, Want: 1}, "ref.n must not be empty"},
		{"enum with a value", &kind.Enum{Got: "SENTINEL", Want: []any{"a", "b"}}, `ref.n must be one of "a", "b"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", thing(`{"secret": {"ref": {"id": "a", "n": 1}}}`))
			m := newMapper(v, res, files, MaxDiagnostics, Options{})
			m.visit(&jsonschema.ValidationError{SchemaURL: "urn:elsewhere#/n", InstanceLocation: at, ErrorKind: tc.kind})
			m.flushSecrets()
			got := m.result()
			want := "invalid secret reference: " + tc.reason + "; write it as {ref: {id}}"
			if len(got) != 1 || got[0].Code != CodeLiteral || got[0].Message != want || got[0].Path.String() != "spec.secret" {
				t.Errorf("visit() = %s, want spec.secret: %s", texts(got), want)
			}
		})
	}
}

// TestPatchMatchesCheckBase checks that stage F reports a $patch member
// of a base document as overlay.CheckBase does, which reports the same
// rule before stage F (01 req 25): for each $patch that stage F reports,
// at the root of a spec, in a keyed entry, in a nested object and in a
// closed Policy config, CheckBase reports a diagnostic with the same code,
// location, path, message and hint (review finding: stage F's message
// added "unknown field"; content that enters at stage F through
// FromResources is reported by stage F alone).
func TestPatchMatchesCheckBase(t *testing.T) {
	v := testView(t)
	set, err := schemaidx.Embedded()
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		envelope("Route", `{"$patch": "replace", "match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}`),
		envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u", "$patch": "delete"}]}`),
		envelope("Route", `{"match": {"path": {"prefix": "/", "$patch": "merge"}}, "upstreams": [{"name": "u"}]}`),
		policy("auth.basic", `{"$patch": "replace", "credentials": []}`),
	} {
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", src)
		base := overlay.CheckBase(res, overlay.Options{Schemas: set, Files: files})
		reported := 0
		for _, d := range v.Validate(res, files) {
			if len(d.Path) == 0 || d.Path[len(d.Path)-1] != diag.Field("$patch") {
				continue
			}
			reported++
			if !slices.ContainsFunc(base, func(b diag.Diagnostic) bool {
				return b.Code == d.Code && b.Location == d.Location && b.Path.String() == d.Path.String() && b.Message == d.Message && b.Hint == d.Hint
			}) {
				t.Errorf("%.60s: stage F reported %s; overlay.CheckBase reported\n%s", src, texts(diag.List{d}), texts(base))
			}
		}
		if reported != 1 {
			t.Errorf("%.60s: stage F reported %d $patch findings, want 1", src, reported)
		}
	}
}

// fallbackListeners is a Gateway with a duplicate listener name and a
// duplicate hostname, one node per line: the spec at line 3, the list at
// line 4, the first entry at line 5, its hostnames at lines 6 and 7, the
// second entry at line 8 and its port at line 9.
const fallbackListeners = `{"apiVersion": "ruralz/v1alpha1", "kind": "Gateway", "metadata": {"name": "g"},
"spec":
{"listeners":
[
{"name": "a", "port": 1, "protocol": "http", "hostnames": [
"h",
"h"]},
{"name": "a", "protocol": "http", "port":
2}]}}`

// TestFallbackPositions checks where findings about nodes without a
// source position point: at the nearest known position at or above the
// node, as locate places every validator finding (01 req 36). A
// duplicate keyed entry and its related location point at each entry's
// key, else at the entry, else at the list or above it; a duplicate set
// element and its related location at the element, else at the list or
// above; a reserved label key at the key, else at the labels object, else
// at metadata or the root; a number outside RFC 8259 at itself or its
// nearest positioned ancestor (review finding: a first key without a
// position related the duplicate to the resource start).
func TestFallbackPositions(t *testing.T) {
	v := testView(t)
	// at returns the node at path p of res.
	at := func(res *tree.Resource, p ...diag.PathElem) *tree.Node {
		n, ok := res.Root.At(p)
		if !ok {
			t.Fatalf("no node at %s", diag.Path(p))
		}
		return n
	}
	unknown := func(n *tree.Node) { n.Pos = tree.Pos{} }
	spec, list := diag.Field("spec"), diag.Field("listeners")
	entry := func(res *tree.Resource, i int) *tree.Node { return at(res, spec, list, diag.Index(i)) }
	key := func(res *tree.Resource, i int) *tree.Node {
		return at(res, spec, list, diag.Index(i), diag.Field("name"))
	}
	hosts := func(res *tree.Resource) *tree.Node {
		return at(res, spec, list, diag.Index(0), diag.Field("hostnames"))
	}
	for _, tc := range []struct {
		name string
		// strip clears the positions of the nodes it names.
		strip func(res *tree.Resource)
		// want is the line of each finding, the duplicate host and then the
		// duplicate entry, and of its related location.
		want [2][2]int
	}{
		{"every position", func(*tree.Resource) {}, [2][2]int{{7, 6}, {8, 5}}},
		{"keys without positions", func(res *tree.Resource) {
			unknown(key(res, 0))
			unknown(key(res, 1))
		}, [2][2]int{{7, 6}, {8, 5}}},
		{"first key without a position", func(res *tree.Resource) { unknown(key(res, 0)) }, [2][2]int{{7, 6}, {8, 5}}},
		{"entries without positions", func(res *tree.Resource) {
			for i := range 2 {
				unknown(key(res, i))
				unknown(entry(res, i))
			}
		}, [2][2]int{{7, 6}, {4, 4}}},
		{"keyed list without a position", func(res *tree.Resource) {
			for i := range 2 {
				unknown(key(res, i))
				unknown(entry(res, i))
			}
			unknown(at(res, spec, list))
		}, [2][2]int{{7, 6}, {3, 3}}},
		{"hosts without positions", func(res *tree.Resource) {
			unknown(hosts(res).Items[0])
			unknown(hosts(res).Items[1])
		}, [2][2]int{{5, 5}, {8, 5}}},
		{"set without a position", func(res *tree.Resource) {
			h := hosts(res)
			unknown(h.Items[0])
			unknown(h.Items[1])
			unknown(h)
			unknown(entry(res, 0))
		}, [2][2]int{{4, 4}, {8, 5}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", fallbackListeners)
			tc.strip(res)
			got := v.Validate(res, files)
			var lines [][2]int
			for _, d := range got {
				related := 0
				if len(d.Related) > 0 {
					related = d.Related[0].Line
				}
				lines = append(lines, [2]int{d.Line, related})
			}
			if len(got) != 2 || !slices.Contains(lines, tc.want[0]) || !slices.Contains(lines, tc.want[1]) {
				t.Errorf("findings at lines %v, want %v:\n%s", lines, tc.want, texts(got))
			}
		})
	}
	// A reserved label key without a position points at the labels
	// object, else at metadata, else at the root.
	const labels = `{"apiVersion": "ruralz/v1alpha1", "kind": "Route",
"metadata":
{"name": "x", "labels":
{"ruralz.io/a": "1"}},
"spec": {"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}]}}`
	labelsAt := func(res *tree.Resource) *tree.Node { return at(res, diag.Field("metadata"), diag.Field("labels")) }
	for _, tc := range []struct {
		name  string
		strip func(res *tree.Resource)
		line  int
	}{
		{"key with a position", func(*tree.Resource) {}, 4},
		{"key without a position", func(res *tree.Resource) {
			labelsAt(res).Members[0].KeyPos = tree.Pos{}
		}, 4},
		{"labels without a position", func(res *tree.Resource) {
			l := labelsAt(res)
			l.Members[0].KeyPos, l.Pos = tree.Pos{}, tree.Pos{}
		}, 3},
		{"metadata without a position", func(res *tree.Resource) {
			l := labelsAt(res)
			l.Members[0].KeyPos, l.Pos = tree.Pos{}, tree.Pos{}
			unknown(at(res, diag.Field("metadata")))
		}, 1},
	} {
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", labels)
		tc.strip(res)
		if got := v.Validate(res, files); len(got) != 1 || got[0].Line != tc.line {
			t.Errorf("%s: Validate() = %s, want the reserved key at line %d", tc.name, texts(got), tc.line)
		}
	}
	// A number outside RFC 8259 (excluded by the tree contract) without a
	// position points at its listener entry.
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", fallbackListeners)
	port := at(res, spec, list, diag.Index(1), diag.Field("port"))
	port.Kind, port.Text, port.Pos = tree.KindFloat, "1.", tree.Pos{}
	got := v.Validate(res, files)
	if i := slices.IndexFunc(got, func(d diag.Diagnostic) bool {
		return strings.Contains(d.Message, "not an RFC 8259 number literal")
	}); i < 0 || got[i].Line != 8 {
		t.Errorf("Validate() = %s, want the invalid number at its entry's line 8", texts(got))
	}
}
