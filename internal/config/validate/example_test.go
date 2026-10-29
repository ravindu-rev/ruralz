// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package validate

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
)

// TestDiagnosticsExample reproduces the RZ-CFG-009 example of the
// Configuration model "Diagnostics and source map": the example Bundle
// with Upstream "inventory" misspelled in routes/orders-summary.yaml gives
// that exact text line, and the same code, location, resource, key-aware
// path, message and hint in the JSON form.
func TestDiagnosticsExample(t *testing.T) {
	files := readDirs(t, "testdata/shop-bundle")
	src := files["routes-orders-summary.yaml"]
	delete(files, "routes-orders-summary.yaml")
	typo := strings.Replace(src, "upstream: inventory", "upstream: inventroy", 1)
	if typo == src {
		t.Fatal("the example route lost its inventory step")
	}
	files["routes/orders-summary.yaml"] = typo
	b, table := loadFiles(t, files)
	_, ds := stageH(t, newValidator(t, nil), b, table, 2)
	const want = `routes/orders-summary.yaml:23:19 error RZ-CFG-009 Route/orders-summary spec.composition.steps[name=stock].upstream: Upstream "inventroy" not found (did you mean "inventory"?)` + "\n"
	if got := diagText(t, ds); got != want {
		t.Fatalf("text form:\n%s want:\n%s", got, want)
	}
	var buf bytes.Buffer
	if err := diag.WriteJSON(&buf, ds); err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(`{"code": "RZ-CFG-009", "severity": "error", "file": "routes/orders-summary.yaml", "line": 23, "column": 19,
 "resource": {"kind": "Route", "name": "orders-summary"},
 "path": ["spec", "composition", "steps", {"name": "stock"}, "upstream"],
 "message": "Upstream \"inventroy\" not found", "hint": "did you mean \"inventory\"?"}`), &doc); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("JSON form holds %d findings", len(got))
	}
	g, _ := json.Marshal(got[0])
	w, _ := json.Marshal(doc)
	if !bytes.Equal(g, w) {
		t.Errorf("JSON form %s, want the documented %s", g, w)
	}
}
