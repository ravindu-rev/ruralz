// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"errors"
	"math/big"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"github.com/santhosh-tekuri/jsonschema/v6/kind"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/profile"
	"github.com/ravindu-rev/ruralz/internal/config/schemaidx"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// numbersView compiles testdata/numbers.schema.json: one kind whose spec
// has a field for every keyword through which jsonschema/v6 reads a
// number (the integer type, minimum, maximum, exclusiveMinimum,
// exclusiveMaximum, multipleOf with factors other than 2 and 5, enum,
// const, uniqueItems up to and above 20 elements) and anyOf and oneOf
// integer branches.
func numbersView(t testing.TB) *View {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "numbers.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	idx, err := schemaidx.Load("num/v1", data)
	if err != nil {
		t.Fatalf("schemaidx.Load() error = %v", err)
	}
	v, err := Compile(data, idx)
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
	return v
}

// smallGrid returns a copy of v whose number model has the smallest grid
// the view's numbers allow, so most literals take a surrogate.
func smallGrid(t testing.TB, v *View) *View {
	t.Helper()
	nm, err := newNumberModel(v.doc, 0)
	if err != nil {
		t.Fatalf("newNumberModel() error = %v", err)
	}
	c := *v
	c.nums = nm
	return &c
}

// rawValidate validates res as stage F did before the number model, every
// literal reaching jsonschema/v6 as written: the oracle of the
// differential tests, for literals the library parses quickly. Of two
// members with one key the first wins and the later one is skipped by the
// checks after the validator, and a nil node is a null one, as in
// View.check (built.shadowed, nullFilled).
func rawValidate(v *View, res *tree.Resource, files *tree.FileTable) diag.List {
	if (&work{}).instance(res.Root, v.nums).nils {
		// As runSchema does: a nil node is mapped as a null one.
		filled := *res
		filled.Root = nullFilled(res.Root, nil)
		res = &filled
	}
	m := newMapper(v, res, files, MaxDiagnostics, Options{})
	sch, ok := v.kinds[string(res.ID.Kind)]
	if !ok {
		sch = v.root
	}
	m.shadowed = (&work{}).instance(res.Root, v.nums).shadowed
	m.mapErrors(sch.Validate(rawInstance(res.Root)))
	return m.result()
}

// rawInstance is work.instance with every number literal as written: the
// first of two members with one key wins.
func rawInstance(n *tree.Node) any {
	if n == nil {
		return nil
	}
	switch n.Kind {
	case tree.KindMap:
		m := make(map[string]any, len(n.Members))
		for _, mem := range n.Members {
			if _, dup := m[mem.Key]; !dup {
				m[mem.Key] = rawInstance(mem.Value)
			}
		}
		return m
	case tree.KindList:
		l := make([]any, len(n.Items))
		for i, it := range n.Items {
			l[i] = rawInstance(it)
		}
		return l
	default:
		return n.JSONValue()
	}
}

// ratOf returns the value of a literal.
func ratOf(t testing.TB, s string) *big.Rat {
	t.Helper()
	r, ok := new(big.Rat).SetString(s)
	if !ok {
		t.Fatalf("big.Rat.SetString(%.40q) failed", s)
	}
	return r
}

// docNumbers returns every number of a schema document and the values of
// its multipleOf members, as newNumberModel reads them.
func docNumbers(t testing.TB, doc any) (all, mults []*big.Rat) {
	t.Helper()
	var walk func(v any, key string)
	walk = func(v any, key string) {
		switch x := v.(type) {
		case map[string]any:
			for k, e := range x {
				walk(e, k)
			}
		case []any:
			for _, e := range x {
				walk(e, "")
			}
		case json.Number:
			r := ratOf(t, string(x))
			all = append(all, r)
			if key == "multipleOf" && r.Sign() > 0 {
				mults = append(mults, r)
			}
		default:
		}
	}
	walk(doc, "")
	return all, mults
}

// oracle is the model of one view checked against exact rationals.
type oracle struct {
	nm          *numberModel
	all, mults  []*big.Rat
	gridScale   *big.Rat
	gridCeiling *big.Rat
}

func newOracle(t testing.TB, doc any, minDigits int) *oracle {
	t.Helper()
	nm, err := newNumberModel(doc, minDigits)
	if err != nil {
		t.Fatalf("newNumberModel() error = %v", err)
	}
	all, mults := docNumbers(t, doc)
	return &oracle{
		nm: nm, all: all, mults: mults,
		gridScale:   new(big.Rat).SetInt(pow10(nm.fracDigits)),
		gridCeiling: new(big.Rat).SetInt(pow10(nm.intDigits)),
	}
}

// onGrid reports whether r is a multiple of 10^-F of magnitude below 10^I.
func (o *oracle) onGrid(r *big.Rat) bool {
	scaled := new(big.Rat).Mul(r, o.gridScale)
	return scaled.IsInt() && new(big.Rat).Abs(r).Cmp(o.gridCeiling) < 0
}

// check checks the text bound returned for a literal of value v: as
// written or normalized only on the grid, with v's value; otherwise a
// surrogate off the grid with v's order against every number of the view,
// v's integrality and v's divisibility by every multipleOf value, of at
// most maxBytes bytes.
func (o *oracle) check(t *testing.T, lit string, v *big.Rat, text string, form numberForm, tags int) {
	t.Helper()
	if len(text) > o.nm.maxBytes(tags) {
		t.Errorf("bound(%.40q) = %d bytes, want at most %d", lit, len(text), o.nm.maxBytes(tags))
	}
	if l, _ := parseLiteral(text, nil); !l.zero() {
		if e, fits := l.exponent(); !fits || e > maxExponent(o.nm) || e < -maxExponent(o.nm) {
			t.Errorf("bound(%.40q) = %.40q with exponent %d, want at most %d in magnitude", lit, text, e, maxExponent(o.nm))
		}
	}
	s := ratOf(t, text)
	switch form {
	case asWritten:
		if text != lit || !o.onGrid(v) {
			t.Errorf("bound(%.40q) as written = %.40q, on grid %v", lit, text, o.onGrid(v))
		}
		return
	case rewritten:
		if s.Cmp(v) != 0 || !o.onGrid(v) || len(lit) <= maxPassBytes {
			t.Errorf("bound(%.40q) rewritten = %.40q, want its value on the grid", lit, text)
		}
		return
	case surrogate:
	}
	if o.onGrid(v) || o.onGrid(s) {
		t.Errorf("bound(%.40q) = surrogate %.40q, literal on grid %v, surrogate on grid %v", lit, text, o.onGrid(v), o.onGrid(s))
	}
	for _, c := range o.all {
		if s.Cmp(c) != v.Cmp(c) {
			t.Errorf("bound(%.40q) = %.40q compares %d with %s, the literal %d", lit, text, s.Cmp(c), c.RatString(), v.Cmp(c))
		}
	}
	if s.IsInt() != v.IsInt() {
		t.Errorf("bound(%.40q) = %.40q integer %v, the literal %v", lit, text, s.IsInt(), v.IsInt())
	}
	for _, m := range o.mults {
		sm, vm := new(big.Rat).Quo(s, m).IsInt(), new(big.Rat).Quo(v, m).IsInt()
		if sm != vm {
			t.Errorf("bound(%.40q) = %.40q multiple of %s %v, the literal %v", lit, text, m.RatString(), sm, vm)
		}
	}
}

// specialLiterals reach every case of numberModel.bound for the numbers
// view at a grid of 0 (I 31, F 3, period 15000) and of gridDigits, with
// groups of equal values written differently.
func specialLiterals() []string {
	return []string{
		// Zero and the grid as written.
		"0", "-0", "0.000", "0e5", "-0.0e-7", "1", "-1", "2.5", "-2.5", "7", "0.125", "1e30", "-1e30",
		"0.3", "0.25", "7.5", "15", "-15", "4", "4.0", "40e-1", "1.5", "2", "1.2e6", "12e5", "120e4", "1200000",
		// Between grid points (below 10^I): every bound's neighbors.
		"0.0001", "-0.0001", "1.2345", "-2.5001", "-2.4999", "0.1250001", "0.1249999", "6.9999", "7.0001",
		"1e-50", "1.23e-50", "123e-52", "0.0000000000000000000000000000000000000000000000000123",
		"1e-400", "-1e-400", "2.5e-4", "3.0003", "0.30001", "0.29999", "4.0000001", "3.9999999",
		"999999999999999999999999999999.9999", "-999999999999999999999999999999.9999",
		// Beyond the grid, v×10^F an integer: residues exact and patterned.
		"1e31", "1e32", "-1e32", "3e40", "-3e40", "1.5e35", "7.5e33", "1.2e34", "1e100", "10e99", "0.1e101",
		"1e400", "10e399", "-1e400", "7e40", "1.5e40", "15e39", "-1.5e40", "2.5e45", "1.11e31",
		"300000000000000000000000000000000.3", "300000000000000000000000000000000.25",
		"100000000000000000000000000000000.001", "100000000000000000000000000000000.003",
		"300000000000000000000000000000000.015", "-300000000000000000000000000000000.015",
		"30000000000000000000000000000000000000000000000000.0000",
		// Beyond the grid, v×10^F not an integer.
		"10000000000000000000000000000000000000000.00001", "-10000000000000000000000000000000000000000.00001",
		"10000000000000000000000000000000000000000.00002", "100000000000000000000000000000000.5",
		"1" + strings.Repeat("0", 50) + "." + strings.Repeat("0", 44) + "1",
	}
}

// genLiterals returns n literals from a fixed-seed generator: both signs,
// integer and fraction parts of 0 to 44 digits with zeros at both ends,
// exponents up to 450 in magnitude, and multiples and neighbors of the
// numbers view's bounds and multipleOf values.
func genLiterals(n int, seed uint64) []string {
	r := rand.New(rand.NewPCG(seed, 1)) //nolint:gosec // G404: a fixed seed makes the corpus deterministic; nothing is secret.
	digits := func(k int) string {
		b := make([]byte, k)
		for i := range b {
			switch r.IntN(4) {
			case 0:
				b[i] = '0'
			default:
				b[i] = decimalDigits[r.IntN(10)]
			}
		}
		return string(b)
	}
	bases := []string{"3", "0.3", "0.25", "7.5", "15", "2.5", "0.125", "7", "1", "4", "1.5", "1e30"}
	out := make([]string, 0, n)
	for len(out) < n {
		var b strings.Builder
		if r.IntN(3) == 0 {
			b.WriteByte('-')
		}
		switch r.IntN(3) {
		case 0:
			// A multiple of a view number, scaled by a power of ten.
			k := new(big.Int).SetInt64(r.Int64N(1000))
			base, _ := new(big.Rat).SetString(bases[r.IntN(len(bases))])
			v := new(big.Rat).Mul(base, new(big.Rat).SetInt(k))
			b.WriteString(v.FloatString(r.IntN(6)))
			if r.IntN(2) == 0 {
				b.WriteString("e" + strconv.Itoa(r.IntN(90)-45))
			}
		default:
			intLen := r.IntN(45)
			if intLen == 0 {
				b.WriteByte('0')
			} else {
				b.WriteByte(decimalDigits[1+r.IntN(9)])
				b.WriteString(digits(intLen - 1))
			}
			if r.IntN(2) == 0 {
				b.WriteString("." + digits(1+r.IntN(44)))
			}
			if r.IntN(2) == 0 {
				b.WriteString([]string{"e", "E", "e+", "e-", "E-"}[r.IntN(5)])
				b.WriteString(strconv.Itoa(r.IntN(450)))
			}
		}
		if s := b.String(); jsonval.ValidNumber(s) {
			out = append(out, s)
		}
	}
	return out
}

// TestParseLiteral checks the split of number literals: validity agrees
// with jsonval.ValidNumber (RFC 8259), and D, E, sign and the exponent's
// fit are exact, leading and trailing zeros and an exponent of more than
// 18 digits included.
func TestParseLiteral(t *testing.T) {
	for _, tc := range []struct {
		in        string
		ok        bool
		neg, zero bool
		d         string
		e         int64
		fits      bool
	}{
		{"0", true, false, true, "", 0, true},
		{"-0", true, true, true, "", 0, true},
		{"0.000", true, false, true, "", 0, true},
		{"0e5", true, false, true, "", 0, true},
		{"1", true, false, false, "1", 0, true},
		{"120", true, false, false, "12", 1, true},
		{"1.20", true, false, false, "12", -1, true},
		{"0.0012", true, false, false, "12", -4, true},
		{"12e5", true, false, false, "12", 5, true},
		{"1.2e6", true, false, false, "12", 5, true},
		{"120e4", true, false, false, "12", 5, true},
		{"-1E+2", true, true, false, "1", 2, true},
		{"1e-0002", true, false, false, "1", -2, true},
		{"1e0000000000000000000000001", true, false, false, "1", 1, true},
		{"5e999999999999999999", true, false, false, "5", 999999999999999999, true},
		{"5e-999999999999999999", true, false, false, "5", -999999999999999999, true},
		{"5e1000000000000000000", true, false, false, "5", 0, false},
		{"1.5e-1234567890123456789", true, false, false, "15", 0, false},
		{"", false, false, false, "", 0, false},
		{"-", false, false, false, "", 0, false},
		{"01", false, false, false, "", 0, false},
		{"-01", false, false, false, "", 0, false},
		{"1.", false, false, false, "", 0, false},
		{".5", false, false, false, "", 0, false},
		{"1e", false, false, false, "", 0, false},
		{"1e+", false, false, false, "", 0, false},
		{"+1", false, false, false, "", 0, false},
		{"1.2.3", false, false, false, "", 0, false},
		{"0x10", false, false, false, "", 0, false},
		{"1 ", false, false, false, "", 0, false},
		{"NaN", false, false, false, "", 0, false},
		{"--1", false, false, false, "", 0, false},
		{"1e1.5", false, false, false, "", 0, false},
		{"1" + strings.Repeat("0", 600) + "x", false, false, false, "", 0, false},
	} {
		l, ok := parseLiteral(tc.in, nil)
		if ok != tc.ok || ok != jsonval.ValidNumber(tc.in) {
			t.Errorf("parseLiteral(%.40q) ok = %v, want %v (jsonval.ValidNumber %v)", tc.in, ok, tc.ok, jsonval.ValidNumber(tc.in))
			continue
		}
		if !ok {
			continue
		}
		if l.neg != tc.neg || l.zero() != tc.zero {
			t.Errorf("parseLiteral(%q) neg %v zero %v, want %v %v", tc.in, l.neg, l.zero(), tc.neg, tc.zero)
		}
		if tc.zero {
			continue
		}
		e, fits := l.exponent()
		if d := string(l.appendMantissa(nil, l.first, l.last+1, nil)); d != tc.d || fits != tc.fits || (fits && e != tc.e) {
			t.Errorf("parseLiteral(%q) = D %s E %d fits %v, want %s %d %v", tc.in, d, e, fits, tc.d, tc.e, tc.fits)
		}
	}
}

// TestExponentKey checks the decimal sum of a long exponent and a
// mantissa shift, least significant digit first, against big.Int: carries
// and borrows across every digit, a carry past the top and a borrow that
// removes the top digit included, with one unit per digit copied and per
// digit a carry or borrow reaches.
func TestExponentKey(t *testing.T) {
	for _, tc := range []struct {
		x       string
		delta   int64
		reached int
	}{
		{"1000000000000000000000", 0, 0},
		{"1000000000000000000000", 1, 1},
		{"1000000000000000000000", -1, 22},
		{"9999999999999999999999", 1, 23},
		{"9999999999999999999999", 12345, 23},
		{"1000000000000000000000", -999999999, 22},
		{"1234567890123456789012", 987654321987654321, 19},
		{"1234567890123456789012", -987654321987654321, 19},
		{"1" + strings.Repeat("0", 600), -1, 601},
		{strings.Repeat("9", 600), 1, 601},
	} {
		w, _ := counter()
		key := appendExponentKey([]byte("k"), tc.x, tc.delta, w)
		want, _ := new(big.Int).SetString(tc.x, 10)
		want.Add(want, big.NewInt(tc.delta))
		got := []byte(string(key[1:]))
		slices.Reverse(got)
		if string(key[:1]) != "k" || string(got) != want.String() || w.total != len(tc.x)+tc.reached {
			t.Errorf("appendExponentKey(%.30s, %d) = %.30s charging %d, want %.30s charging %d", tc.x, tc.delta, got, w.total, want.String(), len(tc.x)+tc.reached)
		}
	}
}

// TestNumberModel checks the grid and period read from a view: the
// numbers view (period 15000 at a grid of 0: 3, 0.3, 0.25 and 7.5 at
// 10^3), the rendered view (no multipleOf, no uniqueItems), and the views
// Compile refuses (a number of more than maxViewDigits integer or
// fraction digits, an exponent of more than 18 digits, a period of more
// than maxPeriodDigits digits).
func TestNumberModel(t *testing.T) {
	num := numbersView(t)
	for _, tc := range []struct {
		name                    string
		nm                      *numberModel
		intDigits, fracDigits   int
		period, twoFive, others string
		tail                    int
		unique                  bool
	}{
		{"numbers at grid 0", smallGrid(t, num).nums, 31, 3, "15000", "5000", "3", 4, true},
		{"numbers at gridDigits", num.nums, 40, 40, "15" + strings.Repeat("0", 40), "5" + strings.Repeat("0", 40), "3", 41, true},
		{"rendered at gridDigits", testView(t).nums, 40, 40, "1" + strings.Repeat("0", 40), "1" + strings.Repeat("0", 40), "1", 40, false},
		// traceSampling's default 0.01 has two fraction digits.
		{"rendered at grid 0", smallGrid(t, testView(t)).nums, 10, 2, "100", "100", "1", 2, false},
	} {
		nm := tc.nm
		if nm.intDigits != tc.intDigits || nm.fracDigits != tc.fracDigits || nm.period.String() != tc.period ||
			nm.twoFive.String() != tc.twoFive || nm.coprime.String() != tc.others || nm.tail != tc.tail || nm.unique != tc.unique {
			t.Errorf("%s: I %d F %d period %s = %s × %s tail %d unique %v, want %d %d %s = %s × %s %d %v", tc.name,
				nm.intDigits, nm.fracDigits, nm.period, nm.twoFive, nm.coprime, nm.tail, nm.unique,
				tc.intDigits, tc.fracDigits, tc.period, tc.twoFive, tc.others, tc.tail, tc.unique)
		}
		if want := nm.coprime.Cmp(big.NewInt(1)) > 0; (nm.inverse != nil) != want {
			t.Errorf("%s: inverse %v, want one: %v", tc.name, nm.inverse, want)
		}
	}
	coprime := func(n int) string { return "1" + strings.Repeat("0", n-1) + "1" }
	for _, tc := range []struct {
		name, doc string
		ok        bool
	}{
		{"400 integer digits", `{"maximum": 1e399}`, true},
		{"401 integer digits", `{"maximum": 1e400}`, false},
		{"400 fraction digits", `{"minimum": 1e-400}`, true},
		{"401 fraction digits", `{"minimum": 1e-401}`, false},
		{"exponent of 19 digits", `{"default": 1e1000000000000000000}`, false},
		{"negative zero exponent", `{"default": -0e-1000000000000000000}`, true},
		{"period of 400 digits", `{"a": {"multipleOf": ` + coprime(200) + `}, "b": {"multipleOf": ` + coprime(199) + `}}`, true},
		{"period of 401 digits", `{"a": {"multipleOf": ` + coprime(201) + `}, "b": {"multipleOf": ` + coprime(200) + `}}`, false},
		{"401 integer digits in an array", `{"enum": [0, 1e400]}`, false},
		{"multipleOf as data", `{"enum": [{"multipleOf": 7}]}`, true},
		{"uniqueItems false", `{"uniqueItems": false}`, true},
	} {
		doc, err := jsonschema.UnmarshalJSON(strings.NewReader(tc.doc))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := newNumberModel(doc, gridDigits); (err == nil) != tc.ok {
			t.Errorf("%s: newNumberModel() error = %v, want ok %v", tc.name, err, tc.ok)
		}
	}
	if _, err := newNumberModel(map[string]any{"maximum": json.Number("1.2.3")}, gridDigits); err == nil {
		t.Error("newNumberModel(a number outside RFC 8259) succeeded")
	}
	data, err := os.ReadFile(filepath.Join("testdata", "synthetic.schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(data), `"minimum": 0.5`, `"minimum": 1e-401`, 1)
	idx, err := schemaidx.Load("test/v1", []byte(bad))
	if err != nil {
		t.Fatalf("schemaidx.Load() error = %v", err)
	}
	if _, err := Compile([]byte(bad), idx); !errors.Is(err, ErrSchema) {
		t.Errorf("Compile(view with 1e-401) error = %v, want ErrSchema", err)
	}
	// The number model runs before the compiler, which parses the view's
	// numbers with big.Rat: the refusal names the number although the
	// view also has a keyword the compiler rejects.
	slow := strings.Replace(bad, `"maxItems": 2`, `"maxItems": "two"`, 1)
	if _, err := Compile([]byte(slow), idx); !errors.Is(err, ErrSchema) || !strings.Contains(err.Error(), "integer or fraction digits") {
		t.Errorf("Compile(view with 1e-401 and a bad keyword) error = %v, want the number refusal", err)
	}
}

// TestBoundOracle checks numberModel.bound against exact rationals for
// every case it distinguishes, on the numbers view at a grid of 0 and of
// gridDigits and on the rendered view: on the grid the literal as written
// (or normalized when long) with its value; off it a surrogate off the
// grid with the literal's order against every view number, its
// integrality and its divisibility by every multipleOf value (the
// properties jsonschema/v6 reads: type integer, minimum, maximum, the
// exclusive bounds, multipleOf, enum and const), of bounded size; and,
// with uniqueItems in the view, equal surrogates exactly for equal values.
func TestBoundOracle(t *testing.T) {
	lits := slices.Concat(specialLiterals(), genLiterals(3000, 35))
	for _, tc := range []struct {
		name string
		doc  any
		min  int
	}{
		{"numbers grid 0", numbersView(t).doc, 0},
		{"numbers gridDigits", numbersView(t).doc, gridDigits},
		{"rendered grid 0", testView(t).doc, 0},
		{"rendered gridDigits", testView(t).doc, gridDigits},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOracle(t, tc.doc, tc.min)
			tags := newNumberTags(o.nm)
			// bySurrogate and byValue map each surrogate to the value of
			// its literals and each value to its surrogate: with tags,
			// both must be functions.
			bySurrogate := map[string]string{}
			byValue := map[string]string{}
			forms := map[numberForm]int{}
			for _, lit := range lits {
				text, form, ok := o.nm.bound(lit, tags, nil)
				if !ok {
					t.Fatalf("bound(%q) not ok", lit)
				}
				forms[form]++
				v := ratOf(t, lit)
				n := 0
				if tags != nil {
					n = len(tags.ids)
				}
				o.check(t, lit, v, text, form, n)
				if form != surrogate || tags == nil {
					continue
				}
				sv, value := ratOf(t, text).RatString(), v.RatString()
				if prev, ok := bySurrogate[sv]; ok && prev != value {
					t.Errorf("values %s and %s share the surrogate %.40q", prev, value, text)
				}
				if prev, ok := byValue[value]; ok && prev != sv {
					t.Errorf("value %s of %.40q has the surrogates %s and %s", value, lit, prev, sv)
				}
				bySurrogate[sv], byValue[value] = value, sv
			}
			if forms[surrogate] == 0 || forms[asWritten] == 0 {
				t.Errorf("forms = %v, want literals as written and surrogates", forms)
			}
			if tags != nil && len(byValue) < 100 {
				t.Errorf("%d distinct surrogate values, want at least 100", len(byValue))
			}
		})
	}
}

// hugeEquivalents pairs literals that jsonschema/v6 cannot parse in
// bounded time or at all with a short literal that has the same
// properties at every position of the rendered view the tests use (an
// integer 1 to 65535, a number 0 to 1, ByteSize and IntOrString): the
// diagnostics of the two must be identical. They cover exponents just
// under and over 10^6 in both directions, an exponent of more than 18
// digits, long mantissas and fractions of 1,000,000 digits.
func hugeEquivalents() []struct{ name, lit, short string } {
	return []struct{ name, lit, short string }{
		{"1e999999", "1e999999", "100000"},
		{"1e-999999", "1e-999999", "0.5"},
		{"1e1000001", "1e1000001", "100000"},
		{"1e-1000001", "1e-1000001", "0.5"},
		{"-1e1000001", "-1e1000001", "-100000"},
		{"-1e-1000001", "-1e-1000001", "-0.5"},
		{"-1E+9999999999", "-1E+9999999999", "-100000"},
		{"1e-9999999", "1e-9999999", "0.5"},
		{"exponent of 30 digits", "1e" + strings.Repeat("9", 30), "100000"},
		{"negative exponent of 30 digits", "1e-" + strings.Repeat("9", 30), "0.5"},
		{"mantissa of 1 MiB", "1." + strings.Repeat("1", 1<<20), "1.5"},
		{"fraction of 1,000,000 digits", "0." + strings.Repeat("7", 1_000_000), "0.7"},
		{"tiny fraction of 1,000,000 digits", "0." + strings.Repeat("0", 999_999) + "1", "0.5"},
		{"integer of 1,000,001 digits", "1" + strings.Repeat("0", 1_000_000), "100000"},
		{"integral number of 1,000,000 digits", "1" + strings.Repeat("0", 999_990) + ".000000000", "100000"},
		{"long exact bound", "65535." + strings.Repeat("0", 1_000_000), "65535"},
		{"long just above the bound", "65535." + strings.Repeat("0", 1_000_000) + "1", "65535.5"},
		{"long exact 1", "1." + strings.Repeat("0", 1_000_000), "1"},
		{"long just below 1", "0." + strings.Repeat("9", 1_000_000), "0.9"},
		{"long zero", "-0." + strings.Repeat("0", 1_000_000), "0"},
	}
}

// numberDocs place a literal last in a resource of the rendered view at
// each position whose schema reads numbers: the listener port (integer,
// minimum 1, maximum 65535), traceSampling, failureRatio and
// similarityThreshold (number, minimum 0, maximum 1), ByteSize (anyOf an
// integer of minimum 0 or a quantity string) and IntOrString (anyOf an
// integer or a string). The literal is last, so every diagnostic has the
// same location with a short or a long literal.
func numberDocs(lit string) map[string]string {
	return map[string]string{
		"port":                envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": `+lit+`}]}`),
		"traceSampling":       envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": 80}], "telemetry": {"traceSampling": `+lit+`}}`),
		"failureRatio":        envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "circuitBreaker": {"failureRatio": `+lit+`}}`),
		"similarityThreshold": envelope("AIModel", `{"candidates": [{"name": "a", "provider": "p", "model": "m"}], "cache": {"semantic": {"similarityThreshold": `+lit+`}}}`),
		"ByteSize":            envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": 80}], "limits": {"maxRequestBodyBytes": `+lit+`}}`),
		"IntOrString":         envelope("Upstream", `{"protocol": "http", "discovery": {"type": "dns", "service": "a.example", "port": `+lit+`}}`),
	}
}

// TestHugeNumberLiterals validates the literals that would crash stage F
// (a nil *big.Rat dereferenced in numValidate for an exponent beyond
// 10^6) or make it slow (5^|exponent| and quadratic mantissas in
// big.Rat.SetString) at every number position of the rendered view: no
// panic, the diagnostics of the equivalent short literal byte for byte,
// and every number handed to the validator short with a small exponent
// (checkHanded).
func TestHugeNumberLiterals(t *testing.T) {
	v := testView(t)
	for _, h := range hugeEquivalents() {
		short := numberDocs(h.short)
		for pos, src := range numberDocs(h.lit) {
			t.Run(h.name+"/"+pos, func(t *testing.T) {
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", src)
				got := v.Validate(res, files)
				wantFiles := &tree.FileTable{}
				want := rawValidate(v, mustResource(t, wantFiles, "r.json", short[pos]), wantFiles)
				if texts(got) != texts(want) {
					t.Errorf("Validate() =\n%s\nwant the diagnostics of %s:\n%s", texts(got), h.short, texts(want))
				}
				b := (&work{}).instance(res.Root, v.nums)
				checkHanded(t, v.nums, b.value, 0)
				if b.surrogates+b.rewritten != 1 {
					t.Errorf("instance() replaced %d and rewrote %d literals, want one of them", b.surrogates, b.rewritten)
				}
			})
		}
	}
}

// TestHugeNumbersView validates huge literals at each field of the
// numbers view, one keyword per field (the integer type, minimum and
// maximum, the exclusive bounds, multipleOf 3, 0.3, 0.25 and 7.5, enum,
// const, anyOf and oneOf integer branches), against a short literal with
// the same properties there: 1e1000001 and 1e45 are integers beyond every
// bound, multiples of 0.25 and of no other multipleOf value; 3e1000001
// and 3e45 multiples of 3, 0.3, 0.25 and 7.5. No panic, identical
// diagnostics, and every number handed to the validator short with a
// small exponent (checkHanded), which also bounds the enum and const
// comparisons (equals parsed 1e999999 in about 40 ms per comparison
// before).
func TestHugeNumbersView(t *testing.T) {
	v := numbersView(t)
	fields := []string{"int", "num", "range", "open", "third", "tenth", "quarter", "step", "choice", "exact", "either", "one"}
	for _, h := range []struct{ lit, short string }{
		{"1e1000001", "1e45"},
		{"1e999999", "1e45"},
		{"-1e1000001", "-1e45"},
		{"3e1000001", "3e45"},
		{"3e999999", "3e45"},
		{"1e-1000001", "1e-45"},
		{"-1e-999999", "-1e-45"},
		{"7.5e" + strings.Repeat("9", 25), "7.5e45"},
		{"0." + strings.Repeat("3", 1_000_000), "0.3333"},
		{"2." + strings.Repeat("5", 100_000), "2.5555"},
	} {
		for _, f := range fields {
			t.Run(f+"/"+h.lit[:min(len(h.lit), 16)], func(t *testing.T) {
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", numSpec(`"`+f+`": `+h.lit))
				got := v.Validate(res, files)
				wantFiles := &tree.FileTable{}
				want := rawValidate(v, mustResource(t, wantFiles, "r.json", numSpec(`"`+f+`": `+h.short)), wantFiles)
				if texts(got) != texts(want) {
					t.Errorf("Validate() =\n%s\nwant the diagnostics of %s:\n%s", texts(got), h.short, texts(want))
				}
				b := (&work{}).instance(res.Root, v.nums)
				checkHanded(t, v.nums, b.value, 1)
			})
		}
	}
}

// checkHanded checks every number of an instance value against what keeps
// big.Rat.SetString cheap: at most nm.maxBytes(tags) bytes and an
// exponent of at most maxExponent(nm) in magnitude (SetString computes
// 10^|exponent|; 1e999999 has 9 bytes and cost about 40 ms).
func checkHanded(t *testing.T, nm *numberModel, v any, tags int) {
	t.Helper()
	switch x := v.(type) {
	case json.Number:
		l, ok := parseLiteral(string(x), nil)
		e, fits := l.exponent()
		if !ok || len(x) > nm.maxBytes(tags) || (!l.zero() && (!fits || e > maxExponent(nm) || e < -maxExponent(nm))) {
			t.Errorf("the validator read %.40q (%d bytes), want at most %d bytes and an exponent of at most %d", x, len(x), nm.maxBytes(tags), maxExponent(nm))
		}
	case map[string]any:
		for _, e := range x {
			checkHanded(t, nm, e, tags)
		}
	case []any:
		for _, e := range x {
			checkHanded(t, nm, e, tags)
		}
	default:
	}
}

// maxExponent bounds the power of ten of a number handed to the
// validator: a literal as written and a normalized form lie on the grid,
// and a surrogate's digits are at most those of period × 10^I times the
// tag, plus maxPassBytes for margin.
func maxExponent(nm *numberModel) int64 {
	return int64(len(nm.shifted.String()) + nm.fracDigits + maxPassBytes)
}

// numSpec returns a Num resource of the numbers view with spec members.
func numSpec(members string) string {
	return `{"apiVersion": "num/v1", "kind": "Num", "metadata": {"name": "n"}, "spec": {` + members + `}}`
}

// equalForm writes lit's value with one more mantissa digit and an
// exponent one less, a different text of the same value; ok is false for
// zero and for an exponent of more than 18 digits.
func equalForm(lit string) (string, bool) {
	l, ok := parseLiteral(lit, nil)
	if !ok || l.zero() {
		return "", false
	}
	e, fits := l.exponent()
	if !fits {
		return "", false
	}
	b := []byte{}
	if l.neg {
		b = append(b, '-')
	}
	b = l.appendMantissa(b, l.first, l.last+1, nil)
	b = append(b, '0', 'e')
	return string(strconv.AppendInt(b, e-1, 10)), true
}

// TestValidateNumbersDifferential validates each literal of the oracle
// corpus at every number field of the numbers view, and in uniqueItems
// arrays of up to 20 and of more than 20 elements next to other literals
// and to another text of its own value, and at every number position of
// the rendered view, and checks that the diagnostics equal those of the
// literal handed to jsonschema/v6 as written: identical decisions and
// messages for every keyword that reads numbers, at a grid of 0 (most
// literals take a surrogate) and of gridDigits.
func TestValidateNumbersDifferential(t *testing.T) {
	lits := slices.Concat(specialLiterals(), genLiterals(600, 36))
	fields := []string{"int", "num", "range", "open", "third", "tenth", "quarter", "step", "choice", "exact", "either", "one"}
	for _, tc := range []struct {
		name string
		v    *View
	}{
		{"numbers grid 0", smallGrid(t, numbersView(t))},
		{"numbers gridDigits", numbersView(t)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			surrogates := 0
			for i, lit := range lits {
				var b strings.Builder
				for _, f := range fields {
					b.WriteString(`"` + f + `": ` + lit + `, `)
				}
				twin, ok := equalForm(lit)
				if !ok {
					twin = lit
				}
				few := []string{lit, lits[(i+1)%len(lits)], twin}
				many := slices.Concat(lits[i/2:i/2+22], []string{twin})
				b.WriteString(`"few": [` + strings.Join(few, ", ") + `], "many": [` + strings.Join(many, ", ") + `], "nested": {"a": [` + lit + `, 2]}`)
				src := numSpec(b.String())
				files := &tree.FileTable{}
				res := mustResource(t, files, "r.json", src)
				if got, want := tc.v.Validate(res, files), rawValidate(tc.v, res, files); texts(got) != texts(want) {
					t.Fatalf("literal %.60q: Validate() =\n%s\nwant as written:\n%s", lit, texts(got), texts(want))
				}
				surrogates += (&work{}).instance(res.Root, tc.v.nums).surrogates
			}
			if surrogates == 0 {
				t.Error("no literal took a surrogate")
			}
			t.Logf("%d surrogates", surrogates)
		})
	}
	rendered := smallGrid(t, testView(t))
	for _, lit := range lits {
		for pos, src := range numberDocs(lit) {
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", src)
			if got, want := rendered.Validate(res, files), rawValidate(rendered, res, files); texts(got) != texts(want) {
				t.Fatalf("rendered %s %.60q: Validate() =\n%s\nwant as written:\n%s", pos, lit, texts(got), texts(want))
			}
		}
	}
}

// TestUniqueItemsNumbers checks uniqueItems on arrays of number literals
// that take surrogates, up to 20 elements (pairwise equals) and above 20
// (writeHash, which would dereference the nil *big.Rat of an exponent
// beyond 10^6): equal values written differently are duplicates, distinct
// values sharing the grid point below them are not, and huge exponents
// compare by their exact value, without a panic in the uniqueItems hash.
func TestUniqueItemsNumbers(t *testing.T) {
	v := numbersView(t)
	pad := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = strconv.Itoa(1000 + i)
		}
		return out
	}
	for _, tc := range []struct {
		name  string
		items []string
		want  string
	}{
		{"equal tiny values", []string{"1.23e-50", "123e-52", "0.5"}, "entries 0 and 1 are equal"},
		{"distinct tiny values", []string{"1.23e-50", "1.24e-50", "1.23e-51"}, ""},
		{"equal values beyond the grid", []string{"1e400", "0.1e401", "10e399"}, "entries 0 and 1 are equal"},
		{"signs differ", []string{"1e400", "-1e400"}, ""},
		{"equal huge exponents", []string{"1e1234567890123456789012", "10e1234567890123456789011"}, "entries 0 and 1 are equal"},
		{"distinct huge exponents", []string{"1e1234567890123456789012", "1e1234567890123456789013"}, ""},
		{"equal huge negative exponents", []string{"1e-1000000000000000000000", "10e-1000000000000000000001"}, "entries 0 and 1 are equal"},
		{"carry into the exponent", []string{"100e9999999999999999999", "1e10000000000000000001"}, "entries 0 and 1 are equal"},
		{"borrow from the exponent", []string{"0.1e10000000000000000000", "1e9999999999999999999"}, "entries 0 and 1 are equal"},
		{"exponent beyond 10^6", []string{"1e1000001", "1e-1000001", "1e1000001"}, "entries 0 and 2 are equal"},
		{"above 20: equal huge", append(pad(21), "1e1000001", "10e1000000"), "entries 21 and 22 are equal"},
		{"above 20: distinct huge", append(pad(21), "1e1000001", "1e1000002", "-1e1000001", "1e-1000001"), ""},
		{"above 20: equal long fractions", append(pad(21), "0."+strings.Repeat("7", 100_000), "0."+strings.Repeat("7", 100_000)+"000"), "entries 21 and 22 are equal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			field := "few"
			if len(tc.items) > 20 {
				field = "many"
			}
			got := validateWith(t, v, numSpec(`"`+field+`": [`+strings.Join(tc.items, ", ")+`]`))
			var msgs []string
			for _, d := range got {
				msgs = append(msgs, d.Message)
			}
			if want := []string{tc.want}; (tc.want == "" && len(got) != 0) || (tc.want != "" && !slices.Equal(msgs, want)) {
				t.Errorf("Validate() = %q, want %q", msgs, tc.want)
			}
		})
	}
}

// TestNumberWorkBounded checks with counters, never the clock, that a
// literal's cost to the validator is bounded: 400 listeners with port
// 1e999999 take 400 surrogates of at most numberModel.maxBytes bytes,
// and the validation errors keep at most a few hundred bits per number
// (the parsed 1e999999 was about 415 KB in every kind.Maximum); a 1 MiB
// and a 2 MiB mantissa charge their bytes as work units, so ValidateWith
// yields about once per 256 bytes inside them (it yielded 0 times in 2 s
// before), and stage F reads each byte a bounded number of times.
func TestNumberWorkBounded(t *testing.T) {
	v := testView(t)
	var b strings.Builder
	for i := range 400 {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(`{"name": "l` + strconv.Itoa(i) + `", "protocol": "http", "port": 1e999999}`)
	}
	res := mustResource(t, &tree.FileTable{}, "r.json", envelope("Gateway", `{"listeners": [`+b.String()+`]}`))
	built := (&work{}).instance(res.Root, v.nums)
	if built.surrogates != 400 || built.rewritten != 0 {
		t.Errorf("instance() took %d surrogates and rewrote %d, want 400 and 0", built.surrogates, built.rewritten)
	}
	checkHanded(t, v.nums, built.value, 0)
	if v.nums.maxBytes(0) > 128 {
		t.Errorf("maxBytes(0) = %d for the rendered view, want at most 128", v.nums.maxBytes(0))
	}
	verr := validationError(t, v, res)
	bits, rats := ratBits(verr)
	if rats != 400 || bits > 400*512 {
		t.Errorf("the validation errors hold %d rationals of %d bits, want 400 of at most 512 bits each", rats, bits)
	}
	if got := v.Validate(res, nil); len(got) != 400 || got[0].Message != "must be at most 65535" {
		t.Errorf("Validate() = %d diagnostics starting %s, want 400 maxima", len(got), texts(got[:1]))
	}
	for _, size := range []int{1 << 20, 2 << 20} {
		lit := "1." + strings.Repeat("1", size)
		res := mustResource(t, &tree.FileTable{}, "r.json", numberDocs(lit)["port"])
		w, calls := counter()
		_, _, _ = v.runSchema(res, w)
		nodes := countNodes(res.Root)
		keys, strs := textBytes(res.Root)
		// The grammar pass reads every byte; nothing else reads a literal
		// that takes the first surrogate form. The instance and the
		// validator's span charge the nodes and the member-name bytes,
		// the span the string bytes too.
		if want := 2*nodes + 2*keys + strs + len(lit) - 1; w.total != want {
			t.Errorf("%d-byte mantissa: runSchema() charged %d units, want %d", size, w.total, want)
		}
		if *calls < len(lit)/yieldEvery {
			t.Errorf("%d-byte mantissa: %d Yield calls, want at least %d", size, *calls, len(lit)/yieldEvery)
		}
	}
}

// ratBits returns the bits and number of the rationals the numeric error
// kinds of e's tree hold.
func ratBits(e *jsonschema.ValidationError) (bits, rats int) {
	add := func(r *big.Rat) {
		if r != nil {
			bits += r.Num().BitLen() + r.Denom().BitLen()
			rats++
		}
	}
	switch k := e.ErrorKind.(type) {
	case *kind.Minimum:
		add(k.Got)
	case *kind.Maximum:
		add(k.Got)
	case *kind.ExclusiveMinimum:
		add(k.Got)
	case *kind.ExclusiveMaximum:
		add(k.Got)
	case *kind.MultipleOf:
		add(k.Got)
	default:
	}
	for _, c := range e.Causes {
		b, r := ratBits(c)
		bits += b
		rats += r
	}
	return bits, rats
}

// TestNumberCharges checks the work units of the number model: a literal
// of at most maxPassBytes bytes charges nothing beyond its node; a longer
// one charges every byte its grammar pass reads, and the mantissa digits
// it reduces modulo the period or copies into a tag key.
func TestNumberCharges(t *testing.T) {
	num := numbersView(t)
	long := strings.Repeat("3", 100)
	for _, tc := range []struct {
		name  string
		nm    *numberModel
		lit   string
		units int
	}{
		{"short on the grid", testView(t).nums, "8080", 0},
		{"short surrogate", testView(t).nums, "1e999999", 0},
		{"64 bytes", testView(t).nums, "1." + strings.Repeat("5", maxPassBytes-2), 0},
		// The grammar pass charges the digits it reads.
		{"65 bytes, rewritten", testView(t).nums, "1." + strings.Repeat("0", maxPassBytes-1), maxPassBytes},
		{"long below the grid", testView(t).nums, "0." + long, 101},
		{"long beyond the grid", testView(t).nums, long + "e100", 103},
		// The numbers view has uniqueItems and the period's factor 3: the
		// key copies D, and a surrogate beyond the grid with v×10^F an
		// integer reduces D modulo 3.
		{"long beyond the grid with a residue", num.nums, long + "e100", 103 + 100 + 100},
		{"long below the grid with a key", num.nums, "0." + long, 101 + 100},
		{"long beyond the grid with an exact residue", num.nums, long + ".5", 101 + 101 + 101},
		{"long beyond the grid, fraction", num.nums, "1" + strings.Repeat("0", 50) + "." + strings.Repeat("0", 44) + "1", 96 + 96},
		// An exponent of more than 18 digits: the key copies it, and the
		// mantissa shift (here -1 for 10e-X) borrows through its zeros.
		{"long exponent beyond the grid", num.nums, "1e" + long, 101 + 1 + 100 + 1},
		{"long exponent below the grid", num.nums, "10e-1" + strings.Repeat("0", 99), 102 + 1 + 100 + 100},
	} {
		w, _ := counter()
		text, _, ok := tc.nm.bound(tc.lit, newNumberTags(tc.nm), w)
		if !ok || w.total != tc.units {
			t.Errorf("%s: bound() = %.40q, %v charging %d units, want %d", tc.name, text, ok, w.total, tc.units)
		}
	}
}

// TestValidatorPanicRecovered checks the backstop: a jsonschema/v6 panic
// (here a literal handed as written with an exponent beyond 10^6, at
// minimum and maximum, and in a uniqueItems array of more than 20
// elements) is the RZ-CFG-005 finding "cannot be validated: the schema
// validator failed" at the resource root, without the value, instead of
// a crash.
func TestValidatorPanicRecovered(t *testing.T) {
	for _, tc := range []struct {
		name, kind string
		v          *View
		inst       map[string]any
	}{
		{"maximum", "Gateway", testView(t), map[string]any{"spec": map[string]any{"telemetry": map[string]any{"traceSampling": json.Number("1e1000001")}}}},
		{"minimum", "Upstream", testView(t), map[string]any{"spec": map[string]any{"circuitBreaker": map[string]any{"failureRatio": json.Number("1e-1000001")}}}},
		{"uniqueItems", "Num", numbersView(t), map[string]any{"spec": map[string]any{"many": slices.Repeat([]any{json.Number("1e1000001")}, 21)}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validate(tc.v.kinds[tc.kind], tc.inst)
			if !errors.Is(err, errValidator) {
				t.Fatalf("validate() error = %v, want errValidator", err)
			}
			res := mustResource(t, &tree.FileTable{}, "r.json", `{"kind": "`+tc.kind+`"}`)
			m := newMapper(tc.v, res, nil, MaxDiagnostics, Options{})
			m.mapErrors(err)
			got := m.result()
			if len(got) != 1 || got[0].Code != CodeSchema || got[0].Message != "cannot be validated: the schema validator failed" || len(got[0].Path) != 0 {
				t.Errorf("mapErrors() = %s, want one root RZ-CFG-005 that names no value", texts(got))
			}
		})
	}
}

// TestInvalidNumberText checks a number node whose text is not an RFC
// 8259 literal, which the tree contract excludes: the validator is not
// run (it would parse a long text without bound), one RZ-CFG-005 "cannot
// be validated" finding stands at the node's position, and the checks on
// the tree still run (the duplicate listener).
func TestInvalidNumberText(t *testing.T) {
	v := testView(t)
	for _, text := range []string{"1.2.3", "+5", "0x1F", "", "1e", ".5", strings.Repeat("1", 100_000) + "x"} {
		src := envelope("Gateway", `{"listeners": [{"name": "a", "protocol": "http", "port": 1}, {"name": "a", "protocol": "http", "port": 80}]}`)
		files := &tree.FileTable{}
		res := mustResource(t, files, "r.json", src)
		listeners, _ := res.Root.At(diag.Path{diag.Field("spec"), diag.Field("listeners")})
		port, _ := listeners.Items[0].Get("port")
		port.Kind, port.Text = tree.KindFloat, text
		got := v.Validate(res, files)
		want := []string{"cannot be validated: a number is not an RFC 8259 number literal", "duplicate entry: name \"a\" is already used by an earlier entry"}
		var msgs []string
		for _, d := range got {
			msgs = append(msgs, d.Message)
		}
		if !slices.Equal(msgs, want) {
			t.Errorf("port %.20q: Validate() = %q, want %q", text, msgs, want)
			continue
		}
		if loc := files.Location(port.Pos); got[0].Location != loc || len(got[0].Path) != 0 {
			t.Errorf("port %.20q: finding at %v %s, want %v at the root path", text, got[0].Location, got[0].Path, loc)
		}
	}
}

// TestNumberFrontEnds validates huge literals from both production front
// ends (profile.Parse with YAML and with JSON), which give them no
// diagnostic, and from a variable that stage E re-types into a number
// (subst.Apply): no panic, and the diagnostics of the equivalent short
// literal, for YAML such as `traceSampling: 1e1000001` and
// `failureRatio: 1e-9999999`.
func TestNumberFrontEnds(t *testing.T) {
	v := testView(t)
	yamlDocs := map[string]string{
		"traceSampling": "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\nspec:\n  listeners:\n  - name: http\n    port: 8080\n    protocol: http\n  telemetry:\n    traceSampling: %s\n",
		"failureRatio":  "apiVersion: ruralz/v1alpha1\nkind: Upstream\nmetadata:\n  name: x\nspec:\n  protocol: http\n  endpoints:\n  - address: a:1\n  circuitBreaker:\n    failureRatio: %s\n",
		"port":          "apiVersion: ruralz/v1alpha1\nkind: Gateway\nmetadata:\n  name: x\nspec:\n  listeners:\n  - name: http\n    protocol: http\n    port: %s\n",
	}
	parse := func(t *testing.T, src string, f profile.Format) (diag.List, *tree.FileTable) {
		t.Helper()
		files := &tree.FileTable{}
		name := "r.yaml"
		if f == profile.FormatJSON {
			name = "r.json"
		}
		id := files.Add(tree.File{Path: name, Role: tree.RoleBase})
		docs, ds, err := profile.Parse(t.Context(), []byte(src), id, name, f, profile.Options{})
		if err != nil || len(ds) != 0 || len(docs) != 1 {
			t.Fatalf("profile.Parse() = %d documents, %s, %v", len(docs), texts(ds), err)
		}
		return v.Validate(resourceOf(docs[0].Root), files), files
	}
	for _, h := range []struct{ lit, short string }{
		{"1e1000001", "100000"},
		{"1e-9999999", "0.5"},
		{"-1e1000001", "-100000"},
		{"1e999999", "100000"},
		{"0." + strings.Repeat("7", 200_000), "0.7"},
		{"1." + strings.Repeat("1", 200_000), "1.5"},
	} {
		for pos, format := range yamlDocs {
			name := pos + "/" + h.lit[:min(len(h.lit), 12)]
			t.Run("yaml/"+name, func(t *testing.T) {
				got, _ := parse(t, strings.Replace(format, "%s", h.lit, 1), profile.FormatYAML)
				want, _ := parse(t, strings.Replace(format, "%s", h.short, 1), profile.FormatYAML)
				if texts(got) != texts(want) {
					t.Errorf("Validate() =\n%s\nwant\n%s", texts(got), texts(want))
				}
			})
		}
		for pos, src := range numberDocs(h.lit) {
			t.Run("json/"+pos+"/"+h.lit[:min(len(h.lit), 12)], func(t *testing.T) {
				got, _ := parse(t, src, profile.FormatJSON)
				want, _ := parse(t, numberDocs(h.short)[pos], profile.FormatJSON)
				if texts(got) != texts(want) {
					t.Errorf("Validate() =\n%s\nwant\n%s", texts(got), texts(want))
				}
			})
		}
	}
	src := envelope("Upstream", `{"protocol": "http", "endpoints": [{"address": "a:1"}], "circuitBreaker": {"failureRatio": "${R}"}}`)
	for _, tc := range []struct{ value, want string }{
		{"1e1000001", `must be at most 1; variable "R" was substituted into the value`},
		{"-1e1000001", `must be at least 0; variable "R" was substituted into the value`},
		{"1e-1000001", ""},
		{"0." + strings.Repeat("3", 100_000), ""},
	} {
		got := afterSubstitution(t, v, src, map[string]string{"R": tc.value})
		var msgs []string
		for _, d := range got {
			msgs = append(msgs, d.Message)
		}
		if want := []string{tc.want}; (tc.want == "" && len(got) != 0) || (tc.want != "" && !slices.Equal(msgs, want)) {
			t.Errorf("R=%.20s: Validate() = %q, want %q", tc.value, msgs, tc.want)
		}
	}
}

// FuzzNumberBound checks numberModel.bound on arbitrary literal pairs
// against exact rationals, at the numbers view's grid of 0 (I 31, F 3,
// period 15000, uniqueItems): a literal on the grid passes with its
// value; a surrogate is off the grid with the literal's order against
// every view number, integrality and multipleOf divisibility, of bounded
// size; and two literals get equal surrogates exactly when their values
// are equal. Each literal is checked whatever the other is: one that is
// not a number, a zero or one the oracle cannot parse quickly skips only
// its own value checks and the pair comparison, so the second literal of
// a pair whose first is a zero is still checked.
func FuzzNumberBound(f *testing.F) {
	for _, lit := range specialLiterals() {
		f.Add(lit, "1e400")
	}
	f.Add("1.23e-50", "123e-52")
	f.Add("1e400", "0.1e401")
	f.Add("0e99999999999999999999", "-0.0E-9223372036854775808")
	f.Add("300000000000000000000000000000000.3", "3000000000000000000000000000000003e-1")
	// A first literal that is a zero, not a number, or beyond the oracle,
	// with a second one each check reads.
	f.Add("0", "-0.0E-9223372036854775808")
	f.Add("-0.0e+12", "1e400")
	f.Add("1e1000001", "-0e-9223372036854775808")
	f.Add("1x", "3.5e40")
	f.Add("1e999999999999999999999", "0.25")
	o := newOracle(f, numbersView(f).doc, 0)
	f.Fuzz(func(t *testing.T, a, b string) {
		var vals [2]*big.Rat
		var subs [2]string
		tags := newNumberTags(o.nm)
		for i, lit := range []string{a, b} {
			l, ok := parseLiteral(lit, nil)
			if ok != jsonval.ValidNumber(lit) {
				t.Fatalf("parseLiteral(%q) ok = %v, jsonval.ValidNumber disagrees", lit, ok)
			}
			if !ok {
				continue
			}
			if l.zero() {
				// Any exponent: a zero the library parses as written is
				// handed as written when short, every other as 0.
				text, form, _ := o.nm.bound(lit, tags, nil)
				r, parses := new(big.Rat).SetString(text)
				_, asIs := new(big.Rat).SetString(lit)
				if !parses || r.Sign() != 0 || (form == asWritten) != (asIs && len(lit) <= maxPassBytes) || (form != asWritten && text != "0") {
					t.Fatalf("bound(zero %.40q) = %.40q form %d", lit, text, form)
				}
				continue
			}
			if e, fits := l.exponent(); !fits || e > 2000 || e < -2000 || len(lit) > 400 {
				continue
			}
			text, form, ok := o.nm.bound(lit, tags, nil)
			if !ok {
				t.Fatalf("bound(%q) not ok", lit)
			}
			vals[i] = ratOf(t, lit)
			o.check(t, lit, vals[i], text, form, len(tags.ids))
			if form == surrogate {
				subs[i] = ratOf(t, text).RatString()
			}
		}
		if subs[0] != "" && subs[1] != "" && (subs[0] == subs[1]) != (vals[0].Cmp(vals[1]) == 0) {
			t.Fatalf("surrogates of %q and %q equal %v, the values %v", a, b, subs[0] == subs[1], vals[0].Cmp(vals[1]) == 0)
		}
	})
}

// TestConcurrentNumberModel shares one View's number model across
// goroutines under -race with literals of every surrogate form, an exact
// and a patterned residue modulo the period's factor 3, and tags: the
// model's big.Int values are only read.
func TestConcurrentNumberModel(t *testing.T) {
	v := numbersView(t)
	src := numSpec(`"third": 3e1000001, "tenth": 300000000000000000000000000000000000000000000000.3, "quarter": 1e-1000001, ` +
		`"range": 1` + strings.Repeat("0", 90) + `.5, "many": [` + strings.Repeat("1e400, ", 21) + `2e400]`)
	files := &tree.FileTable{}
	res := mustResource(t, files, "r.json", src)
	want := texts(v.Validate(res, files))
	if built := (&work{}).instance(res.Root, v.nums); built.surrogates != 26 {
		t.Fatalf("instance() took %d surrogates, want 26", built.surrogates)
	}
	done := make(chan string, 8)
	for range 8 {
		go func() { done <- texts(v.Validate(res, files)) }()
	}
	for range 8 {
		if got := <-done; got != want {
			t.Errorf("concurrent Validate() = %q, want %q", got, want)
		}
	}
	if !strings.Contains(want, "entries 0 and 1 are equal") {
		t.Errorf("Validate() = %q, want the duplicate 1e400", want)
	}
}
