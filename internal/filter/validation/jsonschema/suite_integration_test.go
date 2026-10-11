// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

//go:build integration

package jsonschema

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/filter"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// suiteRoot is the vendored JSON-Schema-Test-Suite (11 req 18).
const suiteRoot = "../../../../test/fixtures/json-schema-test-suite"

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

// refusedBecause lists the findings a suite schema may legitimately get
// from 07 req 75: references outside the document (the suite's remotes
// and meta-schemas), a $schema other than draft 2020-12 (cross-draft and
// vocabulary cases), and anchors or pointers the walk resolves strictly.
func refusedBecause() []string {
	return []string{
		"resolves outside the schema document",
		"$schema must be",
		"not JSON Schema draft 2020-12",
	}
}

// TestJSONSchemaTestSuite runs the required draft 2020-12 tests of the
// vendored JSON-Schema-Test-Suite through CompileSchema and the Filter's
// strict decoding (07 P7 against the suite's verdicts): every schema
// compiles or is refused for a reason refusedBecause lists, and every case
// of a compiled schema gets the suite's verdict.
func TestJSONSchemaTestSuite(t *testing.T) {
	dir := filepath.Join(suiteRoot, "data", "tests", "draft2020-12")
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || path.Ext(p) != ".json" {
			return err
		}
		if rel, _ := filepath.Rel(dir, p); !strings.HasPrefix(filepath.ToSlash(rel), "optional/") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("suite files under %s: %v, %d", dir, err, len(files))
	}
	slices.Sort(files)
	var groups, refused, cases int
	for _, file := range files {
		data, err := os.ReadFile(file) //nolint:gosec // G304: a file of the vendored suite.
		if err != nil {
			t.Fatal(err)
		}
		var gs []suiteGroup
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.UseNumber()
		if err := dec.Decode(&gs); err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		name := filepath.Base(file)
		for _, g := range gs {
			groups++
			s, err := CompileSchema(g.Schema)
			if err != nil {
				refused++
				if !slices.ContainsFunc(refusedBecause(), func(r string) bool { return strings.Contains(err.Error(), r) }) {
					t.Errorf("%s: %s: refused for another reason: %v", name, g.Description, err)
				}
				continue
			}
			f := newFilter(s, 400, 0, 0)
			for _, tc := range g.Tests {
				cases++
				v, _, err := jsonval.Decode(tc.Data, jsonval.Options{MapObjects: true})
				if err != nil {
					t.Errorf("%s: %s: %s: strict decode: %v", name, g.Description, tc.Description, err)
					continue
				}
				if s.ComparesNumbers() && !numbersInRange(v) {
					continue
				}
				r := handle(t, f, string(tc.Data))
				if got := r.Outcome == filter.Continue; got != tc.Valid {
					t.Errorf("%s: %s: %s: valid = %v, want %v (%+v)", name, g.Description, tc.Description, got, tc.Valid, r.Response)
				}
			}
		}
	}
	t.Logf("%d files, %d groups (%d refused by 07 req 75), %d cases", len(files), groups, refused, cases)
	if refused*5 > groups {
		t.Errorf("%d of %d groups refused, more than a fifth", refused, groups)
	}
}
