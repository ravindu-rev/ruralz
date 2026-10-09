// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// update rewrites the error-mapping goldens: go test ./internal/config/schemaview -run Golden -update.
var update = flag.Bool("update", false, "rewrite the golden files under testdata/mapping")

// sentinel marks secret values in the golden inputs; no output may
// contain it (01 req 33: an RZ-CFG-012 message never contains the value).
const sentinel = "SENTINEL"

// TestGoldenErrorMapping validates each testdata/mapping/<case>.json
// resource and compares the text and JSON diagnostic forms byte for byte
// with <case>.diag.txt and <case>.diag.json (01 reqs 33 to 35, 47 to 50;
// CR 145's JSONSchemaDocument type and OTLP endpoint cases).
func TestGoldenErrorMapping(t *testing.T) {
	inputs, err := filepath.Glob(filepath.Join("testdata", "mapping", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, in := range inputs {
		if strings.HasSuffix(in, ".diag.json") {
			continue
		}
		n++
		name := strings.TrimSuffix(filepath.Base(in), ".json")
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(in) //nolint:gosec // G304: a golden input under testdata.
			if err != nil {
				t.Fatal(err)
			}
			files := &tree.FileTable{}
			res := mustResource(t, files, "routes/"+name+".json", string(data))
			got := testView(t).Validate(res, files)
			var text, js bytes.Buffer
			if err := diag.WriteText(&text, got); err != nil {
				t.Fatal(err)
			}
			if err := diag.WriteJSON(&js, got); err != nil {
				t.Fatal(err)
			}
			for _, out := range []string{text.String(), js.String()} {
				if strings.Contains(out, sentinel) {
					t.Errorf("output contains the secret sentinel:\n%s", out)
				}
			}
			compareGolden(t, strings.TrimSuffix(in, ".json")+".diag.txt", text.Bytes())
			compareGolden(t, strings.TrimSuffix(in, ".json")+".diag.json", js.Bytes())
		})
	}
	if n < 10 {
		t.Errorf("found %d golden inputs, want at least 10", n)
	}
}

func compareGolden(t *testing.T, path string, got []byte) {
	t.Helper()
	if *update {
		if err := os.WriteFile(path, got, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path) //nolint:gosec // G304: a golden file under testdata.
	if err != nil {
		t.Fatalf("read golden (run with -update to create it): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs (run with -update after review)\ngot:\n%s\nwant:\n%s", path, got, want)
	}
}
