// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/testkit/corpus"
)

// diffExampleRender is the render of the configuration model's diff
// example: both sides rendered for prod.
const diffExampleRender = "prod"

// TestDiffExampleMatchesDocument checks the configuration model's diff
// example ("### Diff semantics") against its golden entry, the newest
// shop-bundle-diff entry, whose TO is the example Bundle and whose FROM is
// the earlier state the example describes (checkExampleEntries): at every
// level this release renders, the section's text block equals the entry's
// diff of the prod renders byte for byte, and its json block, compacted,
// equals that diff's JSON form compacted.
func TestDiffExampleMatchesDocument(t *testing.T) {
	s := mustSuite(t)
	e := newest(s.golden(), shopBundle+diffSuffix)
	if e == nil {
		t.Fatalf("the corpus holds no %s%s entry", shopBundle, diffSuffix)
	}
	if !e.IsDiff() {
		t.Fatalf("%s holds no from/ and to/", e.Name)
	}
	const heading = "### Diff semantics"
	text := blocksOf(t, s, heading, "text")
	js := blocksOf(t, s, heading, "json")
	if len(text) != 1 || len(js) != 1 {
		t.Fatalf("%q holds %d text and %d json blocks, want one of each", heading, len(text), len(js))
	}
	for _, n := range s.levels() {
		base := filepath.Join(s.corpusPath(e.Name), corpus.LevelDir(n), outDiff, diffExampleRender+"."+diffExampleRender)
		gotText, err := os.ReadFile(base + ".txt") //nolint:gosec // G304: a corpus file.
		if err != nil {
			t.Fatal(err)
		}
		if string(gotText) != text[0] {
			t.Errorf("the text block differs from %s level %d (%s)", e.Name, n, firstDiff(text[0], string(gotText)))
		}
		gotJSON, err := os.ReadFile(base + ".json") //nolint:gosec // G304: a corpus file.
		if err != nil {
			t.Fatal(err)
		}
		var want, got bytes.Buffer
		if err := json.Compact(&want, []byte(js[0])); err != nil {
			t.Fatalf("the json block: %v", err)
		}
		if err := json.Compact(&got, gotJSON); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want.Bytes(), got.Bytes()) {
			t.Errorf("the json block, compacted, differs from %s level %d:\n%s\nwant\n%s", e.Name, n, want.Bytes(), got.Bytes())
		}
	}
}
