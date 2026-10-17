// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// zeroForms are zero literals in every spelling RFC 8259 allows: a sign
// or none, fraction zeros, e or E, an exponent sign or none, leading zeros
// in the exponent and a long mantissa. The first ten have an exponent
// beyond int64, which big.Rat.SetString cannot parse: it reads the
// exponent with strconv.ParseInt before its special case for a zero
// mantissa, so jsonschema/v6 got a nil *big.Rat (review finding: a valid
// resource failed with "cannot be validated" and lost its other findings,
// and an integer zero was "not an integer"). The last four are the
// boundary values the library parses.
func zeroForms() []struct{ name, lit string } {
	return []struct{ name, lit string }{
		{"exponent of 20 digits", "0e99999999999999999999"},
		{"negative zero", "-0e99999999999999999999"},
		{"negative exponent", "0e-99999999999999999999"},
		{"exponent plus sign", "0e+99999999999999999999"},
		{"capital E", "0E99999999999999999999"},
		{"exponent of 40 digits", "0E" + strings.Repeat("9", 40)},
		{"fraction zeros at 2^63", "0.0e9223372036854775808"},
		{"negative fraction below -2^63", "-0.000E-9223372036854775809"},
		{"exponent with leading zeros", "0e" + strings.Repeat("0", 30) + "9223372036854775808"},
		{"long mantissa", "0." + strings.Repeat("0", 100) + "e99999999999999999999"},
		{"exponent 2^63-1", "0e9223372036854775807"},
		{"exponent -2^63", "-0e-9223372036854775808"},
		{"exponent of 18 digits", "0e999999999999999999"},
		{"exponent of 19 digits within int64", "-0.0e+1000000000000000000"},
	}
}

// zeroNumberFields are the fields of the numbers view whose schema reads
// a number (numbersView), each validated alone.
func zeroNumberFields() []string {
	return []string{"int", "num", "range", "open", "third", "tenth", "quarter", "step", "choice", "exact", "either", "one"}
}

// TestZeroBound checks what numberModel.bound hands the validator for
// every zero form: a text that big.Rat.SetString parses, to zero, of at
// most maxPassBytes bytes, as written only when it is short and
// big.Rat.SetString parses it as written, else "0".
func TestZeroBound(t *testing.T) {
	for _, v := range []*View{testView(t), numbersView(t)} {
		for _, z := range zeroForms() {
			text, form, ok := v.nums.bound(z.lit, newNumberTags(v.nums), nil)
			r, parsed := new(big.Rat).SetString(text)
			if !ok || !parsed || r.Sign() != 0 || len(text) > maxPassBytes {
				t.Errorf("%s: bound(%.40q) = %.40q, %v; want a short zero big.Rat parses", z.name, z.lit, text, ok)
			}
			_, libraryParses := new(big.Rat).SetString(z.lit)
			if wantAsWritten := len(z.lit) <= maxPassBytes && libraryParses; (form == asWritten) != wantAsWritten || (form != asWritten && (form != rewritten || text != "0")) {
				t.Errorf("%s: bound(%.40q) = %q form %d, want as written %v, else 0 rewritten", z.name, z.lit, text, form, wantAsWritten)
			}
		}
	}
}

// TestZeroRendered validates every zero form at every number position of
// the rendered view (numberDocs: an integer with minimum 1, numbers in
// [0, 1], ByteSize and IntOrString) and checks that the diagnostics equal
// those of the literal 0 there, with no "cannot be validated", both
// through the test parser and through the production JSON front end.
func TestZeroRendered(t *testing.T) {
	v := testView(t)
	zero := numberDocs("0")
	for _, z := range zeroForms() {
		for pos, src := range numberDocs(z.lit) {
			t.Run(z.name+"/"+pos, func(t *testing.T) {
				got, want := validateWith(t, v, src), validateWith(t, v, zero[pos])
				if texts(got) != texts(want) {
					t.Errorf("Validate(%s) =\n%s\nwant the diagnostics of 0:\n%s", z.lit, texts(got), texts(want))
				}
				if g, w := parsed(t, v, src, profile.FormatJSON), parsed(t, v, zero[pos], profile.FormatJSON); g != w {
					t.Errorf("JSON front end: Validate(%s) =\n%s\nwant the diagnostics of 0:\n%s", z.lit, g, w)
				}
				if strings.Contains(texts(got), "cannot be validated") {
					t.Errorf("Validate(%s) = %s", z.lit, texts(got))
				}
			})
		}
	}
	// A zero must not hide another finding of the resource: the listener
	// without a port is reported whatever the spelling of traceSampling's
	// zero.
	masked := func(lit string) string {
		return envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http"}], "telemetry": {"traceSampling": `+lit+`}}`)
	}
	for _, z := range zeroForms() {
		got, want := validateWith(t, v, masked(z.lit)), validateWith(t, v, masked("0"))
		if texts(got) != texts(want) || !strings.Contains(texts(got), "missing required field port") {
			t.Errorf("%s: Validate() =\n%s\nwant\n%s", z.name, texts(got), texts(want))
		}
	}
	// Stage F's own set uniqueness (01 req 34) compares canonical JSON, in
	// which every zero is 0: each form duplicates an earlier 0 as 0.0 does
	// (the same kind of node, so the type errors read alike).
	hosts := func(lit string) string {
		return envelope("Gateway", `{"listeners": [{"name": "a", "port": 1, "protocol": "http", "hostnames": [0, `+lit+`]}]}`)
	}
	for _, z := range zeroForms() {
		got, want := validateWith(t, v, hosts(z.lit)), validateWith(t, v, hosts("0.0"))
		if texts(got) != texts(want) || !strings.Contains(texts(got), "duplicate element 0 in a set") {
			t.Errorf("%s: Validate() =\n%s\nwant\n%s", z.name, texts(got), texts(want))
		}
	}
}

// parsed parses src with the production front end of format and returns
// the text of its validation with v.
func parsed(t *testing.T, v *View, src string, format profile.Format) string {
	t.Helper()
	files := &tree.FileTable{}
	name := "r.json"
	if format == profile.FormatYAML {
		name = "r.yaml"
	}
	id := files.Add(tree.File{Path: name, Role: tree.RoleBase})
	docs, ds, err := profile.Parse(t.Context(), []byte(src), id, name, format, profile.Options{})
	if err != nil || len(ds) != 0 || len(docs) != 1 {
		t.Fatalf("profile.Parse() = %d documents, %s, %v", len(docs), texts(ds), err)
	}
	return texts(v.Validate(resourceOf(docs[0].Root), files))
}

// TestZeroFrontEnds validates every zero form written in YAML at an
// integer and a number position, and substituted by stage E at two number
// positions (subst.Apply re-types a variable into a number; at an integer
// position a text with a fraction or an exponent stays a string, 01 req
// 29), against the literal 0.
func TestZeroFrontEnds(t *testing.T) {
	v := testView(t)
	yamlDocs := map[string]string{
		"traceSampling": "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\nspec:\n  listeners:\n  - name: http\n    port: 8080\n    protocol: http\n  telemetry:\n    traceSampling: %s\n",
		"port":          "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\nspec:\n  listeners:\n  - name: http\n    protocol: http\n    port: %s\n",
	}
	substDocs := map[string]string{
		"failureRatio":  envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "circuitBreaker": {"failureRatio": "${R}"}}`),
		"traceSampling": envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": 80}], "telemetry": {"traceSampling": "${R}"}}`),
	}
	for _, z := range zeroForms() {
		for pos, format := range yamlDocs {
			t.Run("yaml/"+z.name+"/"+pos, func(t *testing.T) {
				got := parsed(t, v, strings.Replace(format, "%s", z.lit, 1), profile.FormatYAML)
				want := parsed(t, v, strings.Replace(format, "%s", "0", 1), profile.FormatYAML)
				if got != want {
					t.Errorf("Validate() =\n%s\nwant\n%s", got, want)
				}
			})
		}
		for pos, src := range substDocs {
			t.Run("subst/"+z.name+"/"+pos, func(t *testing.T) {
				got := texts(afterSubstitution(t, v, src, map[string]string{"R": z.lit}))
				want := texts(afterSubstitution(t, v, src, map[string]string{"R": "0"}))
				if got != want {
					t.Errorf("R=%.40s: Validate() =\n%s\nwant\n%s", z.lit, got, want)
				}
			})
		}
	}
}

// TestZeroNumbersView validates every zero form at each field of the
// numbers view, one keyword per field (the integer type, minimum and
// maximum, the exclusive bounds, multipleOf 3, 0.3, 0.25 and 7.5, enum,
// const, nested const, anyOf and oneOf integer branches), and in
// uniqueItems arrays up to 20 elements (pairwise equals) and above 20
// (writeHash) next to 0, to itself and to 1, at a grid of 0 and of
// gridDigits: the diagnostics must equal those of the literal 0 (review
// finding: uniqueItems missed [0, 0e99999999999999999999]).
func TestZeroNumbersView(t *testing.T) {
	pad := make([]string, 21)
	for i := range pad {
		pad[i] = strconv.Itoa(1000 + i)
	}
	members := func(lit string) map[string]string {
		out := map[string]string{}
		for _, f := range zeroNumberFields() {
			out[f] = `"` + f + `": ` + lit
		}
		out["nested"] = `"nested": {"a": [` + lit + `, 2]}`
		for name, items := range map[string][]string{
			"few with 0":     {"0", lit},
			"few after 0":    {lit, "0"},
			"few with self":  {lit, lit},
			"few with 1":     {lit, "1"},
			"many with 0":    append(append([]string{}, pad...), "0", lit),
			"many with self": append(append([]string{}, pad...), lit, lit),
			"many with 1":    append(append([]string{}, pad...), lit, "1"),
		} {
			field := "few"
			if len(items) > 20 {
				field = "many"
			}
			out[name] = `"` + field + `": [` + strings.Join(items, ", ") + `]`
		}
		return out
	}
	zero := members("0")
	for _, v := range []*View{numbersView(t), smallGrid(t, numbersView(t))} {
		for _, z := range zeroForms() {
			for field, m := range members(z.lit) {
				t.Run(z.name+"/"+field, func(t *testing.T) {
					got, want := validateWith(t, v, numSpec(m)), validateWith(t, v, numSpec(zero[field]))
					if texts(got) != texts(want) {
						t.Errorf("Validate(%s) =\n%s\nwant the diagnostics of 0:\n%s", m, texts(got), texts(want))
					}
				})
			}
		}
	}
	// The duplicates are found: the comparison above would pass if both
	// missed them.
	if got := texts(validateWith(t, numbersView(t), numSpec(zero["many with 0"]))); !strings.Contains(got, "entries 21 and 22 are equal") {
		t.Errorf("Validate(many [... 0, 0]) = %s, want the duplicate", got)
	}
}

// TestZeroViewNumbers checks the view's own zeros: the compiler reads
// them with big.Rat.SetString too (objCompiler.numVal), which returns nil
// for an exponent beyond int64, so a minimum of such a zero would be
// dropped and a const or enum of it would match nothing. newNumberModel
// refuses a view with one, before compiling it, and admits the boundary
// exponents the library parses.
func TestZeroViewNumbers(t *testing.T) {
	for _, tc := range []struct {
		name, doc string
		ok        bool
	}{
		{"minimum beyond int64", `{"minimum": 0e9223372036854775808}`, false},
		{"const with 20 digits", `{"const": 0e99999999999999999999}`, false},
		{"enum below -2^63", `{"enum": [1, -0.0E-9223372036854775809]}`, false},
		{"leading zeros beyond int64", `{"maximum": 0e` + strings.Repeat("0", 30) + `9223372036854775808}`, false},
		{"exponent 2^63-1", `{"minimum": 0e9223372036854775807}`, true},
		{"exponent -2^63", `{"minimum": -0e-9223372036854775808}`, true},
		{"leading zeros within int64", `{"minimum": 0e` + strings.Repeat("0", 30) + `9223372036854775807}`, true},
		{"plain zero", `{"minimum": 0, "exclusiveMinimum": -0.0}`, true},
	} {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		_, err = newNumberModel(doc, gridDigits)
		if (err == nil) != tc.ok {
			t.Errorf("%s: newNumberModel() error = %v, want ok %v", tc.name, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), "exponent") {
			t.Errorf("%s: newNumberModel() error = %v, want it to name the exponent", tc.name, err)
		}
	}
	if _, err := newNumberModel(map[string]any{"minimum": json.Number("0e1.5")}, gridDigits); err == nil {
		t.Error("newNumberModel(a zero outside RFC 8259) succeeded")
	}
}

// zeroed returns a copy of the tree at n in which every zero number
// literal reads "0", its kind kept: the tree stage F must validate like n
// (FuzzValidate).
func zeroed(n *tree.Node) *tree.Node {
	if n == nil {
		return nil
	}
	c := *n
	if n.Kind == tree.KindInt || n.Kind == tree.KindFloat {
		if l, ok := parseLiteral(n.Text, nil); ok && l.zero() {
			c.Text = "0"
		}
	}
	if n.Members != nil {
		c.Members = make([]tree.Member, len(n.Members))
		for i, m := range n.Members {
			m.Value = zeroed(m.Value)
			c.Members[i] = m
		}
	}
	if n.Items != nil {
		c.Items = make([]*tree.Node, len(n.Items))
		for i, it := range n.Items {
			c.Items[i] = zeroed(it)
		}
	}
	return &c
}

// TestExponentInt64 checks literal.exponentInt64 against
// strconv.ParseInt, which big.Rat.SetString applies to the exponent with
// its sign, at both ends of int64, with leading zeros and without an
// exponent.
func TestExponentInt64(t *testing.T) {
	for _, exp := range []string{
		"", "e0", "e-0", "e+5", "e999999999999999999", "E-999999999999999999",
		"e9223372036854775807", "e+9223372036854775807", "e9223372036854775808", "e+9223372036854775808",
		"e-9223372036854775808", "e-9223372036854775809", "e-9223372036854775807",
		"e10000000000000000000", "e-10000000000000000000", "e" + strings.Repeat("0", 40) + "9223372036854775807",
		"e" + strings.Repeat("0", 40) + "9223372036854775808", "e-" + strings.Repeat("0", 40) + "9223372036854775808",
		"e99999999999999999999", "e-99999999999999999999", "e9300000000000000000",
	} {
		for _, mantissa := range []string{"0", "-0.0", "1", "-25.5"} {
			lit := mantissa + exp
			l, ok := parseLiteral(lit, nil)
			if !ok {
				t.Fatalf("parseLiteral(%q) not ok", lit)
			}
			digits := strings.TrimLeft(lit[len(mantissa):], "eE")
			want := true
			if digits != "" {
				_, err := strconv.ParseInt(digits, 10, 64)
				want = err == nil
			}
			if got := l.exponentInt64(); got != want {
				t.Errorf("exponentInt64(%q) = %v, want %v (strconv.ParseInt)", lit, got, want)
			}
		}
	}
}

// noValidatorFailure reports the first "cannot be validated" finding of
// l, which a tree whose numbers are RFC 8259 literals must never get.
func noValidatorFailure(l diag.List) (diag.Diagnostic, bool) {
	for _, d := range l {
		if strings.Contains(d.Message, "cannot be validated") {
			return d, false
		}
	}
	return diag.Diagnostic{}, true
}

// TestZeroed checks the fuzz helpers: zeroed rewrites every zero, signed,
// with a fraction or an exponent, and nothing else, and copies the tree;
// noValidatorFailure finds the root finding of a failed validator.
func TestZeroed(t *testing.T) {
	root, err := parseJSON([]byte(`{"a": [0e99999999999999999999, -0.0, 1e5, "0"], "b": 0, "c": -0e-5}`), 0)
	if err != nil {
		t.Fatal(err)
	}
	z := zeroed(root)
	got, err := json.Marshal(z.JSONValue())
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"a":[0,0,1e5,"0"],"b":0,"c":0}`; string(got) != want {
		t.Errorf("zeroed() = %s, want %s", got, want)
	}
	if a, _ := root.Get("a"); a.Items[0].Text != "0e99999999999999999999" {
		t.Errorf("zeroed() changed its input: %q", a.Items[0].Text)
	}
	if _, ok := noValidatorFailure(diag.List{{Message: "must be at least 1"}}); !ok {
		t.Error("noValidatorFailure(an ordinary finding) = false")
	}
	if _, ok := noValidatorFailure(diag.List{{Message: "cannot be validated: the schema validator failed"}}); ok {
		t.Error("noValidatorFailure(the root finding) = true")
	}
}
