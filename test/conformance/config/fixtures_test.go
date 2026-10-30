// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/hub"
	"github.com/ravindu-rev/ruralz/internal/config/pipeline"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// The fixture sets of docs/engineering/03-testing-and-quality-strategy.md
// "Conformance suites" beside the golden entries and the negative
// fixtures: the JSON subset pairs, loader conformance with CR LF input,
// the CEL cost cases and the overlay rows. Each set's inputs are corpus
// entries, so TestGolden and TestGoldenNegative pin their outputs; the
// tests here check what each set states on top.

// entryNamed returns the corpus entry named name, nil when there is none.
func entryNamed(s *suite, name string) *corpus.Entry {
	i := slices.IndexFunc(s.corpus.Entries, func(e corpus.Entry) bool { return e.Name == name })
	if i < 0 {
		return nil
	}
	return &s.corpus.Entries[i]
}

// subsetCases returns the JSON subset pairs, by case directory: tab
// indentation, the \/ escape and surrogate \u escapes (ADR-0003
// "Confirmation").
func subsetCases() []string { return []string{"solidus-escape", "surrogate-escapes", "tabs"} }

// TestJSONSubset checks every JSON subset pair: the corpus holds exactly
// the cases of subsetCases, each a yaml/ Bundle and its json/ twin, whose
// JSON text the YAML parser reads in ruralz.yaml and the JSON parser in
// every .json file; on every runner both render without --env, with no
// diagnostic, to one digest.
func TestJSONSubset(t *testing.T) {
	s := mustSuite(t)
	var got []string
	for i := range s.corpus.Entries {
		e := &s.corpus.Entries[i]
		if e.Kind != corpus.JSONSubset {
			continue
		}
		name := strings.TrimPrefix(e.Name, corpus.JSONSubsetDir+"/")
		got = append(got, name)
		t.Run(name, func(t *testing.T) {
			if len(e.Unknown) > 0 {
				t.Errorf("files outside the layout: %q", e.Unknown)
			}
			yaml, okYAML := e.Bundle(corpus.YAMLDir)
			js, okJSON := e.Bundle(corpus.JSONDir)
			if !okYAML || !okJSON || len(e.Bundles) != 2 {
				t.Fatalf("a pair holds yaml/ and json/, not %+v", e.Bundles)
			}
			if !slices.ContainsFunc(js.Files, func(f corpus.File) bool { return strings.HasSuffix(f.Name, ".json") }) {
				t.Error("json/ holds no .json file for the JSON parser")
			}
			if i := slices.IndexFunc(js.Files, func(f corpus.File) bool { return f.Name == bundleRoot }); i < 0 || !bytes.HasPrefix(bytes.TrimSpace(js.Files[i].Data), []byte("{")) {
				t.Error("json/ruralz.yaml does not hold JSON text for the YAML parser")
			}
			dir := s.corpusPath(e.Name)
			for _, p := range s.runners(t) {
				digests := map[string]string{}
				for _, b := range []string{yaml.Name, js.Name} {
					o, err := p.validate(t.Context(), run{bundle: filepath.Join(dir, b)})
					if err != nil {
						t.Fatal(err)
					}
					if len(o.text) > 0 || o.digest == "" {
						t.Fatalf("%s: %s/ renders\n%s", p.binary(), b, o.text)
					}
					digests[b] = o.digest
				}
				if digests[yaml.Name] != digests[js.Name] {
					t.Errorf("%s: yaml/ renders to %s, json/ to %s", p.binary(), digests[yaml.Name], digests[js.Name])
				}
			}
		})
	}
	if !slices.Equal(got, subsetCases()) {
		t.Errorf("the corpus holds the JSON subset cases %q, want %q", got, subsetCases())
	}
}

// scalarReading is the value one field takes in the plain-scalars entry.
type scalarReading struct {
	kind  v1alpha1.Kind
	name  string
	path  diag.Path
	node  tree.Kind
	value string
}

// TestLoaderConformance checks the loader conformance set: a fixture per
// rejection of the restricted YAML profile, RZ-CFG-001 to RZ-CFG-004, and
// the alias-expansion fixture, whose aliases the profile refuses before
// expanding any; and the readings of ADR-0003 "Confirmation" in every
// render of the plain-scalars entry: 0777 decodes as 777 raw in an integer
// field and substituted into one, and stays "0777" substituted into a
// string field; yes, on, 0b1, 1_000 and 2026-09-23 are strings.
func TestLoaderConformance(t *testing.T) {
	s := mustSuite(t)
	for _, code := range []string{"RZ-CFG-001", "RZ-CFG-002", "RZ-CFG-003", "RZ-CFG-004"} {
		if !slices.ContainsFunc(s.negatives(), func(e *corpus.Entry) bool { return e.Code == code }) {
			t.Errorf("the profile rejection %s has no fixture", code)
		}
	}
	alias := entryNamed(s, corpus.NegativeDir+"/RZ-CFG-003-alias-expansion")
	if alias == nil || !bytes.Contains(alias.ExpectedText, []byte("aliases are not allowed")) {
		t.Error("the alias-expansion fixture is missing, or its aliases are not refused")
	}

	e := newest(s.golden(), "plain-scalars")
	if e == nil {
		t.Fatal("the corpus holds no plain-scalars entry")
	}
	dir, envFile := entryDir(s, e)
	f, k := diag.Field, diag.Keyed
	label := func(key string) diag.Path { return diag.Path{f("metadata"), f("labels"), f(key)} }
	want := []scalarReading{
		{v1alpha1.KindGateway, "edge", label("yes-word"), tree.KindString, "yes"},
		{v1alpha1.KindGateway, "edge", label("on-word"), tree.KindString, "on"},
		{v1alpha1.KindGateway, "edge", label("binary"), tree.KindString, "0b1"},
		{v1alpha1.KindGateway, "edge", label("underscore"), tree.KindString, "1_000"},
		{v1alpha1.KindGateway, "edge", label("date"), tree.KindString, "2026-09-23"},
		{v1alpha1.KindGateway, "edge", label("build"), tree.KindString, "0777"},
		{v1alpha1.KindGateway, "edge", diag.Path{f("spec"), f("listeners"), k("name", "http"), f("port")}, tree.KindInt, "777"},
		{v1alpha1.KindPolicy, "limit", diag.Path{f("spec"), f("config"), f("limits"), diag.Index(0), f("requests")}, tree.KindInt, "777"},
	}
	renders := s.renders(t, envFile)
	if len(renders) < 2 {
		t.Fatalf("plain-scalars renders %q: its Environment substitutes the values the render without --env takes from defaults", renders)
	}
	for _, r := range renders {
		res := result(t, s.cli, renderRun(filepath.Join(dir, corpus.BundleDir), envFile, r))
		if res.Validated == nil {
			t.Fatalf("render %s:\n%s", r, texts(res.Diagnostics))
		}
		for _, w := range want {
			got := nodeOf(t, res, w.kind, w.name, w.path)
			if got == nil || got.Kind != w.node || got.Text != w.value {
				t.Errorf("render %s: %s/%s %s = %+v, want %v %q", r, w.kind, w.name, w.path, got, w.node, w.value)
			}
		}
	}
}

// crlfCases returns the corpus entries whose Bundles hold CR LF files
// under raw/: a clean entry, whose digest shows that a CR LF file reads as
// its LF copy, and a fixture, whose finding shows that no position moves.
func crlfCases() []string {
	return []string{corpus.GoldenDir + "/crlf", corpus.NegativeDir + "/RZ-CFG-006-crlf"}
}

// TestCRLF checks each CR LF case against the same Bundle with LF line
// endings: on every runner both report the same diagnostics and digest.
func TestCRLF(t *testing.T) {
	s := mustSuite(t)
	for _, name := range crlfCases() {
		t.Run(name, func(t *testing.T) {
			e := entryNamed(s, name)
			if e == nil {
				t.Fatalf("the corpus holds no %s", name)
			}
			src := filepath.Join(s.corpusPath(e.Name), corpus.BundleDir)
			crlf := false
			lf := t.TempDir()
			for rel, data := range readTree(t, src) {
				crlf = crlf || (strings.HasPrefix(rel, "raw/") && bytes.Contains(data, []byte("\r\n")))
				p := filepath.Join(lf, filepath.FromSlash(rel))
				if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(p, bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n")), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if !crlf {
				t.Fatal("the Bundle holds no CR LF file under raw/")
			}
			for _, p := range s.runners(t) {
				a, err := p.validate(t.Context(), run{bundle: src})
				if err != nil {
					t.Fatal(err)
				}
				b, err := p.validate(t.Context(), run{bundle: lf})
				if err != nil {
					t.Fatal(err)
				}
				if (e.Kind == corpus.Golden) == (a.digest == "") || a.digest != b.digest || !bytes.Equal(a.text, b.text) || !bytes.Equal(a.json, b.json) {
					t.Errorf("%s: CR LF renders %q\n%s\nLF renders %q\n%s", p.binary(), a.digest, a.text, b.digest, b.text)
				}
			}
		})
	}
}

// overlayRow is one row of the Overlays merge table of the configuration
// model and the golden entry that pins it: its base Bundle, its overlay
// and, in its level outputs, the expected rendered Bundle.
type overlayRow struct {
	construct, entry string
}

// overlayRows returns every row of the Overlays merge table
// (docs/architecture/02-configuration-model.md "Overlays").
func overlayRows() []overlayRow {
	return []overlayRow{
		{"New identity", "overlay-new-identity"},
		{"Existing identity", "overlay-existing-identity"},
		{"map list", "overlay-map-list"},
		{"orderedMap list", "overlay-ordered-map-list"},
		{"set or atomic list, scalar", "overlay-replaced-values"},
		{"Value of the schema's JSONSchemaDocument definition (validation.json-schema config.schema)", "overlay-json-schema-document"},
		{"null", "overlay-null"},
		{"List entry $patch: delete", "overlay-patch-delete"},
		{"Annotation ruralz.io/patch: replace or delete", "overlay-patch-annotation"},
	}
}

// TestOverlayRows checks the overlay set: overlayRows holds every row of
// the Overlays merge table, in order; each row has its golden entry, each
// of whose Environments selects an overlay that changes the render without
// --env (the entry pins the rendered Bundle); and every code stage D
// raises has a fixture of stage D.
func TestOverlayRows(t *testing.T) {
	s := mustSuite(t)
	var table, want []string
	for _, l := range strings.Split(docSection(t, s, configModelDoc, "### Overlays"), "\n") {
		if c := tableCells(l); len(c) == 2 && c[0] != "Construct" && strings.Trim(c[0], "-") != "" {
			table = append(table, c[0])
		}
	}
	for _, row := range overlayRows() {
		want = append(want, row.construct)
	}
	if !slices.Equal(table, want) {
		t.Errorf("the Overlays table has the rows %q, the suite %q", table, want)
	}
	for _, row := range overlayRows() {
		t.Run(row.entry, func(t *testing.T) {
			e := newest(s.golden(), row.entry)
			if e == nil {
				t.Fatalf("row %q has no entry %s", row.construct, row.entry)
			}
			dir, envFile := entryDir(s, e)
			if envFile == "" {
				t.Fatalf("%s holds no environments.yaml selecting its overlay", e.Name)
			}
			bundle := filepath.Join(dir, corpus.BundleDir)
			base := result(t, s.cli, run{bundle: bundle})
			if base.Validated == nil {
				t.Fatalf("the base Bundle:\n%s", texts(base.Diagnostics))
			}
			for _, r := range s.readEnvironments(t, s.cli, envFile) {
				if _, err := os.Stat(filepath.Join(bundle, "overlays", r)); err != nil {
					t.Errorf("Environment %s selects no overlay: %v", r, err)
				}
				res := result(t, s.cli, renderRun(bundle, envFile, r))
				if res.Validated == nil {
					t.Fatalf("render %s:\n%s", r, texts(res.Diagnostics))
				}
				if res.Validated.Revision.Digest == base.Validated.Revision.Digest {
					t.Errorf("render %s holds the Revision of the render without --env: its overlay changed nothing", r)
				}
			}
		})
	}
	stageD := map[string]bool{}
	for _, e := range s.negatives() {
		if f, err := parseFixture(e.Fixture); err == nil && f.stage == "D" {
			stageD[f.code] = true
		}
	}
	for _, code := range sortedKeys(offlineStages()) {
		if strings.Contains(offlineStages()[code], "D") && !stageD[code] {
			t.Errorf("stage D raises %s, which has no fixture of stage D", code)
		}
	}
}

// celVariableRules returns the variable rules of the configuration model's
// "Variables" with the negative fixture of RZ-CFG-014 that breaks each.
func celVariableRules() map[string]string {
	return map[string]string{
		"a place's variables":          "RZ-CFG-014-place-variables",
		"response by first Phase":      "RZ-CFG-014-response-first-phase",
		"upstream by scope":            "RZ-CFG-014-upstream-scope",
		"request.body where readable":  "RZ-CFG-014-request-body",
		"response.body where readable": "RZ-CFG-014-response-body",
		"steps where readable":         "RZ-CFG-014-steps",
		"the sites of an unrun Policy": "RZ-CFG-014-unattached-policy",
	}
}

// ceilingTerm ends the expression of the cel-cost-ceiling entry, and
// overCeiling extends it by one comparison, which costs three units.
const (
	ceilingTerm = `request.method == "M8"`
	overCeiling = ceilingTerm + " ||\n      request.method == \"M9\""
)

// TestCELCost checks the CEL cost set: the authz-orders rule of the
// example Bundle passes RZ-CFG-015 (the shop-bundle entry renders without
// a diagnostic); an expression at exactly the 10,000-unit ceiling passes
// (cel-cost-ceiling: three units more report an estimate of 10,003) and
// one a unit above fails (RZ-CFG-015-cost-ceiling); a Policy no Route runs
// is accepted where one of its sites accepts it (cel-policy-sites); and
// every variable rule has its RZ-CFG-014 fixture.
func TestCELCost(t *testing.T) {
	s := mustSuite(t)
	shop := newest(s.golden(), shopBundle)
	if shop == nil {
		t.Fatal("no shop-bundle entry")
	}
	data, err := os.ReadFile(filepath.Join(s.corpusPath(shop.Name), corpus.BundleDir, "policies", "common.yaml"))
	if err != nil || !bytes.Contains(data, []byte("name: authz-orders")) {
		t.Fatalf("the shop-bundle entry holds no authz-orders rule: %v", err)
	}
	for _, name := range []string{shopBundle, "cel-cost-ceiling", "cel-policy-sites"} {
		e := newest(s.golden(), name)
		if e == nil {
			t.Fatalf("no %s entry", name)
		}
		for _, p := range s.runners(t) {
			o, err := p.validate(t.Context(), run{bundle: filepath.Join(s.corpusPath(e.Name), corpus.BundleDir)})
			if err != nil {
				t.Fatal(err)
			}
			if len(o.text) > 0 || o.digest == "" {
				t.Errorf("%s: %s reports\n%s", p.binary(), e.Name, o.text)
			}
		}
	}

	ceiling := newest(s.golden(), "cel-cost-ceiling")
	files := readTree(t, filepath.Join(s.corpusPath(ceiling.Name), corpus.BundleDir))
	root := string(files[bundleRoot])
	if strings.Count(root, ceilingTerm) != 1 || !strings.HasSuffix(strings.TrimSpace(root), ceilingTerm) {
		t.Fatalf("cel-cost-ceiling does not end its expression with %s", ceilingTerm)
	}
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, bundleRoot), []byte(strings.Replace(root, ceilingTerm, overCeiling, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	o, err := cliPath{s}.validate(t.Context(), run{bundle: tmp})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(o.text, []byte("RZ-CFG-015")) || !bytes.Contains(o.text, []byte("estimated cost 10003 exceeds 10000")) {
		t.Errorf("cel-cost-ceiling and one comparison more report\n%s\nwant an estimate of 10003, three units above the ceiling", o.text)
	}
	over := entryNamed(s, corpus.NegativeDir+"/RZ-CFG-015-cost-ceiling")
	if over == nil || !bytes.Contains(over.ExpectedText, []byte("estimated cost 10001 exceeds 10000")) {
		t.Error("RZ-CFG-015-cost-ceiling does not exceed the ceiling by one unit")
	}

	for rule, name := range celVariableRules() {
		if entryNamed(s, corpus.NegativeDir+"/"+name) == nil {
			t.Errorf("variable rule %q has no fixture %s", rule, name)
		}
	}
}

// nodeOf returns the node at p in the resource kind/name of res, a run that
// reached its digest, nil when the resource holds none there.
func nodeOf(t testing.TB, res *pipeline.Result, kind v1alpha1.Kind, name string, p diag.Path) *tree.Node {
	t.Helper()
	r, ok := res.Validated.Bundle.Get(hub.ID{Kind: kind, Name: name})
	if !ok {
		t.Fatalf("no %s/%s", kind, name)
	}
	n, ok := r.Tree.At(p)
	if !ok {
		return nil
	}
	return n
}
