// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/errcode"
)

// FuzzValidate feeds arbitrary JSON resources to Validate: it never
// panics, is deterministic, returns the same findings when it yields and
// under a cap of math.MaxInt or math.MaxInt/2 when the default cap was not
// reached, raises only stage F's registered codes, and an RZ-CFG-012
// message never repeats a string from its secret subtree. A tree of RFC
// 8259 literals satisfies zeroProperties. Seeds are the error-mapping
// golden inputs, literals that crashed or stalled the validator before the
// number model, zeros with an exponent beyond int64 among them, and zeros
// of every spelling as keyed-list keys (keyedZeros), one of them the
// crasher committed under testdata/fuzz/FuzzValidate.
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
	for _, tc := range keyedZeros() {
		f.Add([]byte(tc.src))
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
		if err := zeroProperties(v, small, res, files); err != nil {
			t.Fatal(err)
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

// zeroProperties checks the number properties of a resource res whose
// numbers are RFC 8259 literals, with view v and its copy small at the
// smallest grid (smallGrid): Validate is never "cannot be validated"; at
// either grid, the findings equal those of the tree with every zero
// written 0 (zeroed), whatever the zero's exponent, once each keyed path
// element whose key is a zero is written 0 too (zeroKeyed), because a
// keyed entry's key appears in its path as written; and when every number
// literal of the zeroed tree is short enough for jsonschema/v6 to parse
// quickly, the findings of the zeroed tree at the smallest grid, where
// most literals take a surrogate, equal those of its literals handed as
// written (numbers.go). Each comparison is of one tree with itself or
// with its zeroed twin, never across the two trees, since the keyed paths
// of a key such as 0.0 differ between them.
func zeroProperties(v, small *View, res *tree.Resource, files *tree.FileTable) error {
	if d, ok := noValidatorFailure(v.Validate(res, files)); !ok {
		return fmt.Errorf("Validate() of RFC 8259 literals = %s", texts(diag.List{d}))
	}
	zr := *res
	zr.Root = zeroed(res.Root)
	for _, view := range []*View{v, small} {
		lit, zero := view.Validate(res, files), view.Validate(&zr, files)
		if zeroKeyed(lit) != zeroKeyed(zero) {
			return fmt.Errorf("Validate() differs from Validate() with every zero written 0, keyed zeros aside:\n%s\n%s", texts(lit), texts(zero))
		}
	}
	if quickNumbers(zr.Root) {
		if bounded, raw := small.Validate(&zr, files), rawValidate(small, &zr, files); texts(bounded) != texts(raw) {
			return fmt.Errorf("Validate() at the smallest grid differs from the literals as written:\n%s\n%s", texts(bounded), texts(raw))
		}
	}
	return nil
}

// zeroKeyed renders l with the key of every keyed path element that is
// an RFC 8259 zero literal written 0, one diagnostic per line, the lines
// sorted: the text a tree's findings and those of its zeroed twin share.
func zeroKeyed(l diag.List) string {
	lines := make([]string, len(l))
	for i, d := range l {
		d.Path = slices.Clone(d.Path)
		for j, e := range d.Path {
			if lit, ok := parseLiteral(e.Key, nil); e.Kind == diag.ElemKeyed && ok && lit.zero() {
				d.Path[j].Key = "0"
			}
		}
		lines[i] = texts(diag.List{d})
	}
	slices.Sort(lines)
	return strings.Join(lines, "")
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
