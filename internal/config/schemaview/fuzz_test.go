// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// FuzzValidate feeds arbitrary JSON resources to Validate: it never
// panics, is deterministic, returns the same findings when it yields (01
// req 53) and under a cap of math.MaxInt or math.MaxInt/2 when the default
// cap was not reached (01 req 50), raises only stage F's registered
// codes, and an RZ-CFG-012
// message never repeats a string from its secret subtree (01 req 33).
// A tree of RFC 8259 literals is never "cannot be validated", and its
// findings equal those of the tree with every zero written 0, whatever
// the zero's exponent. When every number literal of that tree is short
// enough for jsonschema/v6 to parse quickly, the findings at the smallest
// grid, where most literals take a surrogate, equal those of the literals
// handed as written (numbers.go). Seeds are the error-mapping golden
// inputs and literals that crashed or stalled the validator before the
// number model, zeros with an exponent beyond int64 among them.
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
	for _, lit := range []string{
		"1e1000001", "1e-1000001", "-1E+9999999999", "1e999999", "1." + strings.Repeat("1", 2000), "0.5",
		"0e99999999999999999999", "-0.0E-9223372036854775809", "0e9223372036854775807",
	} {
		for _, src := range numberDocs(lit) {
			f.Add([]byte(src))
		}
	}
	v := testView(f)
	small := smallGrid(f, v)
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
		yields := 0
		if yielding := v.ValidateWith(res, files, Options{Yield: func() { yields++ }}); texts(yielding) != texts(got) {
			t.Fatalf("ValidateWith() with Yield differs from Validate():\n%s\n%s", texts(got), texts(yielding))
		}
		if nodes := countNodes(res.Root); yields < nodes/yieldEvery {
			t.Fatalf("ValidateWith() yielded %d times for %d nodes", yields, nodes)
		}
		if len(got) <= MaxDiagnostics {
			for _, c := range []int{math.MaxInt, math.MaxInt / 2} {
				if all := v.ValidateWith(res, files, Options{MaxDiagnostics: c}); texts(all) != texts(got) {
					t.Fatalf("ValidateWith(cap %d) differs from Validate():\n%s\n%s", c, texts(got), texts(all))
				}
			}
		}
		if d, ok := noValidatorFailure(got); !ok {
			t.Fatalf("Validate() of RFC 8259 literals = %s", texts(diag.List{d}))
		}
		zr := *res
		zr.Root = zeroed(res.Root)
		if z := v.Validate(&zr, files); texts(z) != texts(got) {
			t.Fatalf("Validate() differs from Validate() with every zero written 0:\n%s\n%s", texts(got), texts(z))
		}
		if quickNumbers(zr.Root) {
			if bounded, raw := small.Validate(res, files), rawValidate(small, &zr, files); texts(bounded) != texts(raw) {
				t.Fatalf("Validate() at the smallest grid differs from the literals as written:\n%s\n%s", texts(bounded), texts(raw))
			}
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

// quickNumbers reports whether every number literal under n has at most
// 64 bytes and an exponent of at most 1000 in magnitude, which
// jsonschema/v6 parses in microseconds.
func quickNumbers(n *tree.Node) bool {
	if n == nil {
		return true
	}
	if n.Kind == tree.KindInt || n.Kind == tree.KindFloat {
		l, ok := parseLiteral(n.Text, nil)
		e, fits := l.exponent()
		return ok && len(n.Text) <= maxPassBytes && fits && e <= 1000 && e >= -1000
	}
	for _, m := range n.Members {
		if !quickNumbers(m.Value) {
			return false
		}
	}
	for _, it := range n.Items {
		if !quickNumbers(it) {
			return false
		}
	}
	return true
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
