// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/registry"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
	"github.com/ravindu-rev/ruralz/pkg/config/v1alpha1"
)

// offlineStages returns every offline RZ-CFG code, one whose stage comes
// before the digest, with the letters of the stages of
// docs/architecture/02-configuration-model.md Figure 3 that raise it:
// A. discovery, B. parse, C. base union, identity and environments file,
// D. overlay, E. substitution, F. rendered schema, G. defaults and hub
// conversion, H. references and cross-resource rules, I. effective Filter
// Chains, J. CEL, K. effective-chain rules.
func offlineStages() map[string]string {
	return map[string]string{
		"RZ-CFG-001": "ABE",
		"RZ-CFG-002": "B",
		"RZ-CFG-003": "B",
		"RZ-CFG-004": "B",
		"RZ-CFG-005": "BCDEFGH",
		"RZ-CFG-006": "CDF",
		"RZ-CFG-007": "C",
		"RZ-CFG-008": "CD",
		"RZ-CFG-009": "CDH",
		"RZ-CFG-010": "E",
		"RZ-CFG-011": "E",
		"RZ-CFG-012": "EF",
		"RZ-CFG-013": "E",
		"RZ-CFG-014": "J",
		"RZ-CFG-015": "J",
		"RZ-CFG-016": "ACD",
		"RZ-CFG-017": "C",
		"RZ-CFG-018": "I",
		"RZ-CFG-019": "I",
		"RZ-CFG-020": "I",
		"RZ-CFG-021": "H",
		"RZ-CFG-022": "C",
		"RZ-CFG-023": "H",
		"RZ-CFG-025": "G",
		"RZ-CFG-029": "I",
		"RZ-CFG-030": "D",
		"RZ-CFG-031": "H",
		"RZ-CFG-032": "H",
		"RZ-CFG-034": "K",
		"RZ-CFG-035": "H",
		"RZ-CFG-036": "H",
		"RZ-CFG-037": "H",
		"RZ-CFG-038": "I",
		"RZ-CFG-041": "H",
		"RZ-CFG-043": "J",
	}
}

// postDigestCodes returns the RZ-CFG codes of the checks after the digest
// (Figure 3: the online stage, Ruralz Control and the Node), which may
// carry no source position: the integration and end-to-end cases assert
// them by code and resource (docs/engineering/03-testing-and-quality-
// strategy.md "Conformance suites"), never a negative fixture.
func postDigestCodes() []string {
	return []string{
		"RZ-CFG-024", "RZ-CFG-026", "RZ-CFG-027", "RZ-CFG-028", "RZ-CFG-033",
		"RZ-CFG-039", "RZ-CFG-040", "RZ-CFG-042", "RZ-CFG-044", "RZ-CFG-045",
	}
}

// exemption is one offline rule whose machinery cannot be stubbed yet: no
// fixture can reach it before milestone.
type exemption struct {
	code string
	// kind is the kind of the resource the rule's finding names; a fixture
	// whose diagnostics hold code on a resource of kind reaches the rule.
	kind v1alpha1.Kind
	// rule says what the rule checks.
	rule string
	// milestone makes it reachable.
	milestone string
}

// exemptions returns the suite's exempt list.
func exemptions() []exemption {
	return []exemption{
		{"RZ-CFG-021", v1alpha1.KindPolicy, "the configSchema half: a plugin Policy config that violates its Plugin's configSchema", "M2"},
	}
}

// sources returns the values of a fixture's source: where the position of
// its finding maps.
func sources() []string {
	return []string{
		// sourceBase is a base file of the Bundle.
		sourceBase,
		// sourceOverlay is an overlay file.
		sourceOverlay,
		// sourceSubstituted is a value ${VAR} wrote, at its expression.
		sourceSubstituted,
		// sourceDefault is the nearest authored ancestor of a default or
		// missing field.
		sourceDefault,
		// sourceEnvironments is the environments file.
		sourceEnvironments,
	}
}

// The values of a fixture's source.
const (
	sourceBase         = "base"
	sourceOverlay      = "overlay"
	sourceSubstituted  = "substituted"
	sourceDefault      = "default"
	sourceEnvironments = "environments"
)

// fixture is a negative fixture's fixture.yaml.
type fixture struct {
	// code is the RZ code the fixture reaches.
	code string
	// stage is the letter of the Figure 3 stage that raises it.
	stage string
	// binaries are those of ruralz, ruralz-control and ruralzd that MUST
	// report it, or their in-process paths alone.
	binaries []string
	// source says where its position maps.
	source string
}

// parseFixture reads fixture.yaml: a mapping of exactly code, stage and
// source, strings, and binaries, a list of strings.
func parseFixture(data []byte) (fixture, error) {
	docs, ds, err := profile.Parse(context.Background(), data, 0, corpus.FixtureFile, profile.FormatYAML, profile.Options{})
	switch {
	case err != nil:
		return fixture{}, err
	case len(ds) > 0:
		return fixture{}, errors.New(strings.TrimSpace(texts(ds)))
	case len(docs) != 1:
		return fixture{}, fmt.Errorf("%d documents, want one", len(docs))
	}
	var f fixture
	root := docs[0].Root
	str := func(key string, dst *string) error {
		n, ok := root.Get(key)
		if !ok || n.Kind != tree.KindString || n.Text == "" {
			return fmt.Errorf("%s is not a string", key)
		}
		*dst = n.Text
		return nil
	}
	if err := errors.Join(str("code", &f.code), str("stage", &f.stage), str("source", &f.source)); err != nil {
		return fixture{}, err
	}
	list, ok := root.Get("binaries")
	if !ok || list.Kind != tree.KindList || len(list.Items) == 0 {
		return fixture{}, errors.New("binaries is not a list of binaries")
	}
	for _, it := range list.Items {
		if it.Kind != tree.KindString {
			return fixture{}, errors.New("binaries holds a value that is not a string")
		}
		f.binaries = append(f.binaries, it.Text)
	}
	for _, m := range root.Members {
		if !slices.Contains([]string{"code", "stage", "binaries", "source"}, m.Key) {
			return fixture{}, fmt.Errorf("unknown member %q", m.Key)
		}
	}
	return f, nil
}

// checkFixture checks a fixture's own files against the rules of the
// suite and returns its problems: a directory negative/<code>-<slug>
// naming the code; a registered offline code; a stage that raises it;
// known binaries, the CLI's among them, only in-process paths for a code
// reached through the stand-ins and never for another; ruralzd only
// without an environments file, which a file-mode Node never reads; a
// known source; and bundle/ as the only Bundle, with nothing else.
func checkFixture(e *corpus.Entry, f fixture) []string {
	var out []string
	dir := path.Base(e.Name)
	if !strings.HasPrefix(e.Name, corpus.NegativeDir+"/") || path.Dir(e.Name) != corpus.NegativeDir {
		out = append(out, "a negative fixture is a directory negative/<code>-<slug>")
	}
	if code := corpus.DirCode(dir); code != f.code || !strings.HasPrefix(strings.TrimPrefix(dir, f.code), "-") {
		out = append(out, fmt.Sprintf("the directory names %q, fixture.yaml code %q", code, f.code))
	}
	stages, offline := offlineStages()[f.code]
	if !offline {
		out = append(out, fmt.Sprintf("%s is no offline RZ-CFG code", f.code))
	}
	if len(f.stage) != 1 || !strings.Contains(stages, f.stage) {
		out = append(out, fmt.Sprintf("stage %q does not raise %s (stages %s)", f.stage, f.code, stages))
	}
	cli := false
	for _, b := range f.binaries {
		inProcess := strings.HasSuffix(b, inProcessSuffix)
		switch {
		case !slices.Contains(knownBinaries(), b):
			out = append(out, fmt.Sprintf("unknown binary %q", b))
		case usesStandIns(f.code) && !inProcess:
			out = append(out, fmt.Sprintf("%s is reached through test-only stand-ins: name %s%s, the in-process path", f.code, b, inProcessSuffix))
		case !usesStandIns(f.code) && inProcess:
			out = append(out, fmt.Sprintf("%s needs no stand-in: name the binary %s", f.code, strings.TrimSuffix(b, inProcessSuffix)))
		}
		cli = cli || strings.TrimSuffix(b, inProcessSuffix) == binaryCLI
		if strings.TrimSuffix(b, inProcessSuffix) == binaryNode && e.Environments != nil {
			out = append(out, "ruralzd reads no environments file: a fixture it reports renders without one")
		}
	}
	if !cli {
		out = append(out, "every offline code is ruralz's: binaries names it")
	}
	if !slices.Contains(sources(), f.source) {
		out = append(out, fmt.Sprintf("unknown source %q", f.source))
	}
	if len(e.Bundles) != 1 || e.Bundles[0].Name != corpus.BundleDir {
		out = append(out, "a negative fixture holds one Bundle, bundle/")
	}
	if len(e.Unknown) > 0 {
		out = append(out, fmt.Sprintf("files outside the layout: %q", e.Unknown))
	}
	return out
}

// negatives returns the negative fixtures of the corpus, sorted.
func (s *suite) negatives() []*corpus.Entry {
	var out []*corpus.Entry
	for i := range s.corpus.Entries {
		if e := &s.corpus.Entries[i]; e.Kind == corpus.Negative {
			out = append(out, e)
		}
	}
	return out
}

// fixtureRun returns the run of a negative fixture.
func fixtureRun(s *suite, e *corpus.Entry) run {
	dir := s.corpusPath(e.Name)
	r := run{bundle: filepath.Join(dir, corpus.BundleDir), standIns: usesStandIns(e.Code)}
	if e.Environments != nil {
		r.environments = filepath.Join(dir, corpus.EnvironmentsFile)
	}
	return r
}

// TestGoldenNegative runs every negative fixture: its files follow the
// rules (checkFixture), ruralz reports exactly its expected.txt and
// expected.json, its code among them at the source it names (checkFinding,
// on the facade's diagnostics, which print as ruralz's), and so does every
// other runner of a binary it names (under -update, ruralz's diagnostics
// are written as the expected ones, refusing the files a release holds).
func TestGoldenNegative(t *testing.T) {
	s := mustSuite(t)
	w := &writer{s: s, held: map[string]bool{}}
	if *update {
		var err error
		if w.tag, w.held, err = corpus.Held(t.Context(), s.root); err != nil {
			t.Fatalf("-update must know which files the newest release holds: %v", err)
		}
	}
	fixtures := s.negatives()
	if len(fixtures) == 0 {
		t.Fatal("the corpus holds no negative fixture")
	}
	for _, e := range fixtures {
		t.Run(strings.TrimPrefix(e.Name, corpus.NegativeDir+"/"), func(t *testing.T) {
			f, err := parseFixture(e.Fixture)
			if err != nil {
				t.Fatalf("fixture.yaml: %v", err)
			}
			for _, p := range checkFixture(e, f) {
				t.Error(p)
			}
			r := fixtureRun(s, e)
			ref, err := cliPath{s}.validate(t.Context(), r)
			if err != nil {
				t.Fatalf("ruralz: %v", err)
			}
			if *update {
				for name, data := range map[string][]byte{corpus.ExpectedTextFile: ref.text, corpus.ExpectedFile: ref.json} {
					if have, err := os.ReadFile(s.corpusPath(e.Name + "/" + name)); err != nil || !bytes.Equal(have, data) {
						w.write(t, e.Name+"/"+name, data)
					}
				}
			} else if !bytes.Equal(ref.text, e.ExpectedText) || !bytes.Equal(ref.json, e.Expected) {
				t.Errorf("ruralz reports\n%s\nexpected.txt holds\n%s(run go test ./test/conformance/config -run Golden -update after review)", ref.text, e.ExpectedText)
			}
			pl := s.cli
			if r.standIns {
				pl = s.standIn
			}
			fac, err := facadeOutcome(t.Context(), pl, r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(fac.text, ref.text) || !bytes.Equal(fac.json, ref.json) {
				t.Errorf("the facade reports\n%s\nruralz reports\n%s", fac.text, ref.text)
			}
			if p := checkFinding(t, s, e, f, fac.list); p != "" {
				t.Error(p)
			}
			for _, p := range s.runners(t) {
				if !runs(p, f.binaries) {
					continue
				}
				o, err := p.validate(t.Context(), r)
				if err != nil {
					t.Fatalf("%s: %v", p.binary(), err)
				}
				if !bytes.Equal(o.text, ref.text) || !bytes.Equal(o.json, ref.json) {
					t.Errorf("%s reports\n%s\nruralz reports\n%s", p.binary(), o.text, ref.text)
				}
			}
		})
	}
}

// checkFinding checks that ds, a fixture's diagnostics, hold its code and
// that the first diagnostic of the code sits where source says; it
// returns a problem, or "".
func checkFinding(t testing.TB, s *suite, e *corpus.Entry, f fixture, ds diag.List) string {
	t.Helper()
	i := slices.IndexFunc(ds, func(d diag.Diagnostic) bool { return d.Code == f.code })
	if i < 0 {
		return fmt.Sprintf("the diagnostics hold no %s:\n%s", f.code, texts(ds))
	}
	d := ds[i]
	inOverlay := strings.HasPrefix(d.File, "overlays/")
	// The value at the position holds the expression a substitution read.
	substituted := strings.Contains(scalarAt(sourceText(s, e, d)), "${")
	var ok bool
	switch f.source {
	case sourceOverlay:
		ok = inOverlay
	case sourceEnvironments:
		ok = d.File == corpus.EnvironmentsFile
	case sourceSubstituted:
		ok = !inOverlay && substituted
	case sourceDefault:
		ok = !inOverlay && d.File != corpus.EnvironmentsFile && !authored(t, s, e, d)
	case sourceBase:
		ok = !inOverlay && d.File != corpus.EnvironmentsFile && !substituted
	}
	if !ok {
		return fmt.Sprintf("source %s: %s sits elsewhere: %s", f.source, f.code, d.AppendText(nil))
	}
	return ""
}

// sourceText returns the text of the fixture's Bundle file at the position
// of d, from its column to the end of its line; "" without one.
func sourceText(s *suite, e *corpus.Entry, d diag.Diagnostic) string {
	if d.File == "" || d.Line < 1 || d.Column < 1 {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(s.corpusPath(e.Name), corpus.BundleDir, filepath.FromSlash(d.File))) //nolint:gosec // G304: a corpus file.
	if err != nil {
		return ""
	}
	lines := strings.Split(string(data), "\n")
	if d.Line > len(lines) {
		return ""
	}
	runes := []rune(lines[d.Line-1])
	if d.Column-1 > len(runes) {
		return ""
	}
	return string(runes[d.Column-1:])
}

// scalarAt returns the scalar text starts with: up to its closing quote
// for a quoted scalar, else up to a comment or the end of the line.
func scalarAt(text string) string {
	if text != "" && (text[0] == '"' || text[0] == '\'') {
		if end := strings.IndexByte(text[1:], text[0]); end >= 0 {
			return text[:end+2]
		}
		return text
	}
	if i := strings.Index(text, " #"); i >= 0 {
		return text[:i]
	}
	return text
}

// authored reports whether the field d's path names is written in the
// fixture's source document of d's resource: a default or missing field
// is not.
func authored(t testing.TB, s *suite, e *corpus.Entry, d diag.Diagnostic) bool {
	t.Helper()
	if d.Resource == nil || d.File == "" {
		return false
	}
	file := filepath.Join(s.corpusPath(e.Name), corpus.BundleDir, filepath.FromSlash(d.File))
	data, err := os.ReadFile(file) //nolint:gosec // G304: a corpus file.
	if err != nil {
		t.Fatal(err)
	}
	format, _ := profile.FormatOf(d.File)
	docs, _, err := profile.Parse(context.Background(), data, 0, d.File, format, profile.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range docs {
		res := resourceOf(doc)
		if string(res.ID.Kind) == d.Resource.Kind && res.ID.Name == d.Resource.Name {
			_, ok := doc.Root.At(d.Path)
			return ok
		}
	}
	return false
}

// TestNegativeCoverage checks the negative fixtures against the RZ-CFG
// registry: every registered RZ-CFG code is classified as offline or as a
// check after the digest; every offline code has a fixture whose
// diagnostic carries a file, line and column, or an exempt entry; an
// exempt rule has no fixture that reaches it and is due after the
// milestone this release serves.
func TestNegativeCoverage(t *testing.T) {
	s := mustSuite(t)
	stages := offlineStages()
	registered := map[string]bool{}
	for _, c := range errcode.All() {
		if c.Area != errcode.AreaCFG {
			continue
		}
		registered[c.ID] = true
		_, offline := stages[c.ID]
		if offline == slices.Contains(postDigestCodes(), c.ID) {
			t.Errorf("%s is classified %v as offline and %v as after the digest; it is exactly one", c.ID, offline, !offline)
		}
	}
	for code := range stages {
		if !registered[code] {
			t.Errorf("offline code %s is not registered in internal/errcode", code)
		}
	}
	positioned := map[string]bool{}
	reached := map[string][]string{}
	for _, e := range s.negatives() {
		data := e.Expected
		for _, d := range diagnosticsOf(t, data) {
			code, _ := d["code"].(string)
			if res, ok := d["resource"].(map[string]any); ok {
				kind, _ := res["kind"].(string)
				reached[code+" "+kind] = append(reached[code+" "+kind], e.Name)
			}
			if code == e.Code && d["file"] != nil && d["line"] != nil && d["column"] != nil {
				positioned[code] = true
			}
		}
	}
	exempt := map[string]bool{}
	for _, x := range exemptions() {
		exempt[x.code] = true
		if names := reached[x.code+" "+string(x.kind)]; len(names) > 0 {
			t.Errorf("exempt rule %s (%s) has fixtures %q: drop it from the exempt list", x.code, x.rule, names)
		}
		if x.milestone <= registry.ReleaseMilestone {
			t.Errorf("exempt rule %s is due in %s, which this release (%s) serves: add its fixture", x.code, x.milestone, registry.ReleaseMilestone)
		}
	}
	for _, code := range sortedKeys(stages) {
		if !positioned[code] && !exempt[code] {
			t.Errorf("offline code %s has no negative fixture with a file, line and column, and no exempt entry", code)
		}
	}
}

// TestParseFixture checks the fixture.yaml reader: the four members with
// their types, and nothing else.
func TestParseFixture(t *testing.T) {
	good := "code: RZ-CFG-005\nstage: F\nbinaries: [ruralz, ruralzd]\nsource: base\n"
	f, err := parseFixture([]byte(good))
	if err != nil || f.code != "RZ-CFG-005" || f.stage != "F" || !slices.Equal(f.binaries, []string{binaryCLI, binaryNode}) || f.source != sourceBase {
		t.Fatalf("parseFixture(%q) = %+v, %v", good, f, err)
	}
	for _, bad := range []string{
		"",
		"code: [x]\nstage: F\nbinaries: [ruralz]\nsource: base\n",
		"code: RZ-CFG-005\nstage: F\nbinaries: ruralz\nsource: base\n",
		"code: RZ-CFG-005\nstage: F\nbinaries: []\nsource: base\n",
		"code: RZ-CFG-005\nstage: F\nbinaries: [1]\nsource: base\n",
		"code: RZ-CFG-005\nbinaries: [ruralz]\nsource: base\n",
		"code: RZ-CFG-005\nstage: F\nbinaries: [ruralz]\nsource: base\nnote: x\n",
		"code: RZ-CFG-005\nstage: F\nbinaries: [ruralz]\nsource: base\n---\na: 1\n",
		"code: &a RZ-CFG-005\n",
	} {
		if _, err := parseFixture([]byte(bad)); err == nil {
			t.Errorf("parseFixture(%q) = nil error", bad)
		}
	}
}

// TestCheckFixture checks the rules a fixture's own files follow.
func TestCheckFixture(t *testing.T) {
	entry := func(name string, env bool) *corpus.Entry {
		e := &corpus.Entry{Kind: corpus.Negative, Name: name, Code: corpus.DirCode(path.Base(name)), Bundles: []corpus.Bundle{{Name: corpus.BundleDir}}}
		if env {
			e.Environments = []byte("x")
		}
		return e
	}
	ok := fixture{code: "RZ-CFG-005", stage: "F", binaries: []string{binaryCLI, binaryNode}, source: sourceBase}
	if p := checkFixture(entry("negative/RZ-CFG-005-port", false), ok); len(p) > 0 {
		t.Errorf("checkFixture(valid) = %q", p)
	}
	standIn := fixture{code: "RZ-CFG-030", stage: "D", binaries: []string{binaryCLI + inProcessSuffix}, source: sourceOverlay}
	if p := checkFixture(entry("negative/RZ-CFG-030-overlay", true), standIn); len(p) > 0 {
		t.Errorf("checkFixture(stand-in) = %q", p)
	}
	for _, tc := range []struct {
		name  string
		e     *corpus.Entry
		f     fixture
		wants string
	}{
		{"outside negative/", entry("other/RZ-CFG-005-port", false), ok, "negative/<code>-<slug>"},
		{"directory code", entry("negative/RZ-CFG-006-port", false), ok, "the directory names"},
		{"no slug", entry("negative/RZ-CFG-005", false), ok, "the directory names"},
		{"post-digest code", entry("negative/RZ-CFG-040-x", false), fixture{code: "RZ-CFG-040", stage: "F", binaries: []string{binaryCLI}, source: sourceBase}, "no offline"},
		{"stage", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "J", binaries: []string{binaryCLI}, source: sourceBase}, "does not raise"},
		{"two letters", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "FG", binaries: []string{binaryCLI}, source: sourceBase}, "does not raise"},
		{"unknown binary", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "F", binaries: []string{binaryCLI, "rz"}, source: sourceBase}, "unknown binary"},
		{"stand-in by binary", entry("negative/RZ-CFG-025-x", false), fixture{code: "RZ-CFG-025", stage: "G", binaries: []string{binaryCLI}, source: sourceBase}, "in-process path"},
		{"in-process without stand-in", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "F", binaries: []string{binaryCLI + inProcessSuffix}, source: sourceBase}, "needs no stand-in"},
		{"no CLI", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "F", binaries: []string{binaryNode}, source: sourceBase}, "ruralz's"},
		{"Node with Environments", entry("negative/RZ-CFG-005-port", true), ok, "environments file"},
		{"source", entry("negative/RZ-CFG-005-port", false), fixture{code: "RZ-CFG-005", stage: "F", binaries: []string{binaryCLI}, source: "elsewhere"}, "unknown source"},
		{"bundles", &corpus.Entry{Kind: corpus.Negative, Name: "negative/RZ-CFG-005-port"}, ok, "one Bundle"},
		{"unknown file", &corpus.Entry{Kind: corpus.Negative, Name: "negative/RZ-CFG-005-port", Bundles: []corpus.Bundle{{Name: corpus.BundleDir}}, Unknown: []string{"notes.md"}}, ok, "outside the layout"},
	} {
		got := strings.Join(checkFixture(tc.e, tc.f), "\n")
		if !strings.Contains(got, tc.wants) {
			t.Errorf("%s: checkFixture = %q, want a problem with %q", tc.name, got, tc.wants)
		}
	}
}
