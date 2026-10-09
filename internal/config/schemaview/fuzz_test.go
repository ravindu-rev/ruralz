// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// FuzzValidate feeds arbitrary JSON resources to Validate: it never
// panics, is deterministic, raises only stage F's registered codes, and an
// RZ-CFG-012 message never repeats a string from its secret subtree (01
// req 33). Seeds are the error-mapping golden inputs.
func FuzzValidate(f *testing.F) {
	seeds, err := filepath.Glob(filepath.Join("testdata", "mapping", "*.json"))
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range seeds {
		if strings.HasSuffix(s, ".diag.json") {
			continue
		}
		data, err := os.ReadFile(s) //nolint:gosec // G304: a seed under testdata.
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte(`{"kind": "Consumer", "spec": {"credentials": {"apiKeys": [{"name": "a", "secretRef": {"SENTINEL-key": ["SENTINEL-value"]}}]}}}`))
	f.Add([]byte(`[]`))
	f.Add([]byte(`null`))
	v := testView(f)
	allowed := map[string]bool{CodeSchema: true, CodeUnknownField: true, CodeLiteral: true}
	f.Fuzz(func(t *testing.T, data []byte) {
		root, err := parseJSON(data, 0)
		if err != nil {
			return
		}
		files := &tree.FileTable{}
		files.Add(tree.File{Path: "f.json", Role: tree.RoleBase})
		res := resourceOf(root)
		got := v.Validate(res, files)
		if again := v.Validate(res, files); texts(again) != texts(got) {
			t.Fatalf("Validate() is not deterministic:\n%s\n%s", texts(got), texts(again))
		}
		for _, d := range got {
			if !allowed[d.Code] || !errcode.Valid(d.Code) {
				t.Fatalf("Validate() raised %s, want only stage F codes", d.Code)
			}
			if d.Severity != diag.SeverityError {
				t.Fatalf("Validate() raised a %s", d.Severity)
			}
			if d.Code != CodeLiteral {
				continue
			}
			n, ok := res.Root.At(d.Path)
			if !ok {
				continue
			}
			for _, s := range subtreeStrings(n) {
				if len(s) >= 10 && strings.Contains(d.Message, s) {
					t.Fatalf("RZ-CFG-012 message %q repeats %q from the secret field", d.Message, s)
				}
			}
		}
	})
}

// subtreeStrings returns every member name and string value under n.
func subtreeStrings(n *tree.Node) []string {
	var out []string
	var walk func(*tree.Node)
	walk = func(n *tree.Node) {
		switch n.Kind {
		case tree.KindString:
			out = append(out, n.Text)
		case tree.KindMap:
			for _, m := range n.Members {
				out = append(out, m.Key)
				walk(m.Value)
			}
		case tree.KindList:
			for _, it := range n.Items {
				walk(it)
			}
		default:
		}
	}
	walk(n)
	return out
}
