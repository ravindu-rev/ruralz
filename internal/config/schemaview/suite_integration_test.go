// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package schemaview

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// suiteRoot is the vendored JSON-Schema-Test-Suite (11 req 18), refreshed
// by scripts/update-test-suites.sh json-schema.
const suiteRoot = "../../../test/fixtures/json-schema-test-suite"

// remoteBase is the origin the suite's remote references use; its files
// are data/remotes/.
const remoteBase = "http://localhost:1234/"

// suiteGroup is one schema of a suite file with its test cases.
type suiteGroup struct {
	Description string          `json:"description"`
	Schema      json.RawMessage `json:"schema"`
	Tests       []struct {
		Description string          `json:"description"`
		Data        json.RawMessage `json:"data"`
		Valid       bool            `json:"valid"`
	} `json:"tests"`
}

// usedFormats lists the optional/format files the suite runner includes
// because Ruralz relies on that format (11 req 18: optional/ is skipped
// except the formats Ruralz uses). Ruralz asserts no format: the rendered
// view uses none and is compiled with formats as annotations (01 req 32),
// and validation.json-schema keeps format an annotation too (07 req 75),
// so the list is empty. Asserting a format later adds its file here.
func usedFormats() []string { return nil }

// suiteFiles returns the draft 2020-12 files the runner covers, sorted.
func suiteFiles(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(suiteRoot, "data", "tests", "draft2020-12")
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() || path.Ext(rel) != ".json" {
			return nil
		}
		if strings.HasPrefix(rel, "optional/") {
			name, ok := strings.CutPrefix(rel, "optional/format/")
			if !ok || !slices.Contains(usedFormats(), strings.TrimSuffix(name, ".json")) {
				return nil
			}
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}
	slices.Sort(out)
	return out
}

// suiteRemotes reads data/remotes/ as in-memory resources keyed by their
// http://localhost:1234/ URL. The production compiler keeps its refusing
// loader; the runner adds these documents with AddResource, so no test
// reads the network (CR 134).
func suiteRemotes(t *testing.T) map[string]any {
	t.Helper()
	dir := filepath.Join(suiteRoot, "data", "remotes")
	out := map[string]any{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: a file of the vendored suite.
		if err != nil {
			return err
		}
		doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		out[remoteBase+filepath.ToSlash(rel)] = doc
		return nil
	})
	if err != nil {
		t.Fatalf("read remotes: %v", err)
	}
	return out
}

// TestJSONSchemaTestSuite runs tests/draft2020-12 of the vendored
// JSON-Schema-Test-Suite, without optional/ except the formats Ruralz
// uses, through a compiler configured exactly as the production one
// (newCompiler: draft 2020-12, refusing loader, the asserted Ruralz
// vocabulary, formats and content as annotations): zero failures
// (11 req 18; architecture R-31).
func TestJSONSchemaTestSuite(t *testing.T) {
	files := suiteFiles(t)
	if len(files) == 0 {
		t.Fatalf("no suite files under %s; run scripts/update-test-suites.sh json-schema", suiteRoot)
	}
	remotes := suiteRemotes(t)
	cases := 0
	for _, file := range files {
		data, err := os.ReadFile(filepath.Join(suiteRoot, "data", "tests", "draft2020-12", filepath.FromSlash(file)))
		if err != nil {
			t.Fatal(err)
		}
		var groups []suiteGroup
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&groups); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, g := range groups {
			sch := compileSuiteSchema(t, file, g, remotes)
			for _, tc := range g.Tests {
				cases++
				inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(tc.Data))
				if err != nil {
					t.Fatalf("%s: %s: %s: %v", file, g.Description, tc.Description, err)
				}
				err = sch.Validate(inst)
				var verr *jsonschema.ValidationError
				if err != nil && !errors.As(err, &verr) {
					t.Errorf("%s: %s: %s: Validate error = %v, want a ValidationError", file, g.Description, tc.Description, err)
					continue
				}
				if got := err == nil; got != tc.Valid {
					t.Errorf("%s: %s: %s: valid = %v, want %v", file, g.Description, tc.Description, got, tc.Valid)
				}
			}
		}
	}
	t.Logf("%d files, %d cases", len(files), cases)
}

// compileSuiteSchema compiles one group's schema with the production
// compiler configuration plus the suite's remotes.
func compileSuiteSchema(t *testing.T, file string, g suiteGroup, remotes map[string]any) *jsonschema.Schema {
	t.Helper()
	c, err := newCompiler()
	if err != nil {
		t.Fatal(err)
	}
	for url, doc := range remotes {
		if err := c.AddResource(url, doc); err != nil {
			t.Fatalf("add remote %s: %v", url, err)
		}
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(g.Schema))
	if err != nil {
		t.Fatalf("%s: %s: %v", file, g.Description, err)
	}
	const url = "http://localhost:1234/__suite__/schema.json"
	if err := c.AddResource(url, doc); err != nil {
		t.Fatalf("%s: %s: %v", file, g.Description, err)
	}
	sch, err := c.Compile(url)
	if err != nil {
		t.Fatalf("%s: %s: compile: %v", file, g.Description, err)
	}
	return sch
}
