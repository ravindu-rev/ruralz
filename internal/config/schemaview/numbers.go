// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"slices"
	"strconv"
	"strings"
)

// Number literals reach jsonschema/v6 (v6.0.3) bounded. The library reads
// an instance number's value in four functions, each through
// new(big.Rat).SetString on the literal's text: isInteger (the integer
// type), numValidate (minimum, maximum, exclusiveMinimum,
// exclusiveMaximum and multipleOf), equals (const, enum, and uniqueItems
// up to 20 elements) and writeHash (uniqueItems above 20 elements).
// typeOf reads only the Go type, formats are annotations (newCompiler),
// the Ruralz vocabulary's Validate reads nothing, and the numeric error
// kinds (kind.Minimum to kind.MultipleOf) keep the parsed value.
// SetString's time grows with the square of the mantissa's digit count
// and with the size of 10^|exponent|, which it computes, and it returns
// nil for a decimal exponent beyond 10^6 in magnitude, which numValidate
// and writeHash dereference. It also returns nil for a literal whose
// exponent lies outside int64, zero included: it reads the exponent with
// strconv.ParseInt before its special case for a zero mantissa, so
// 0e99999999999999999999 would be no integer, fail every bound with a
// panic and equal no number. The compiler reads the view's own numbers
// the same way (objCompiler.numVal and intVal), so Compile builds the
// number model first and, before compiling, refuses (ErrSchema) a view
// with a number that needs more than maxViewDigits integer or fraction
// digits, multipleOf values whose period needs more than maxPeriodDigits
// digits, or a zero whose exponent lies outside int64, which the compiler
// would read as no value. No stage before F bounds a literal: stage B
// refuses only integers outside int64, stage E re-types variable text
// with any exponent, and stage G's range check runs after F and admits
// underflow and long fractions.
//
// Each of those reads depends on four properties of the value v alone:
// whether v is an integer, its order against and equality with each
// number the view writes, whether v divided by each multipleOf value is
// an integer, and its equality with the instance's other numbers
// (uniqueItems). The number model hands the validator, for every literal,
// a text of bounded size whose value has the same four properties, so
// validation makes every decision it would make on the literal and the
// diagnostics are byte-identical: the messages word bounds from the
// schema text (numberText) and types from the tree, never from the
// instance. No literal is refused.
//
// Every number the view writes is a multiple of 10^-F of magnitude below
// 10^I, F and I at least gridDigits: those values are the grid. A literal
// on the grid reaches the validator as written when it has at most
// maxPassBytes bytes, else in its normalized form (at most I+F digits).
// Zero is on the grid; a zero reaches it as written only when it is short
// and its exponent fits in int64, else as 0. A literal off the grid lies
// strictly between two neighboring grid points, or beyond every grid
// point; it is replaced by a surrogate off the grid on the same side of
// every grid point (numberModel.bound), so its order against the view's
// numbers is kept and it equals none of them, nor any literal on the grid.
// A value strictly between grid points is not an integer and not a
// multiple of any multipleOf value (below 10^I, the integers and those
// multiples are grid points), and neither is its surrogate; a value beyond
// the grid gets a surrogate whose residue modulo the period Q (the least
// common multiple of 10^F and each multipleOf value times 10^F) decides
// both as the value's does. Tags keep equality between instance numbers:
// when the view uses uniqueItems, two off-grid literals get the same
// surrogate exactly when their values are equal.

// gridDigits is the least number of integer and of fraction digits of the
// grid: a literal of at most maxPassBytes bytes whose value is below
// 10^40 in magnitude with at most 40 fraction digits, such as 1e39 or
// 1.5e-30, reaches the validator as written.
const gridDigits = 40

// maxViewDigits bounds the integer and the fraction digits a number of a
// view may need: Compile refuses a view beyond it, so the grid, and every
// text the model hands the validator, stays short.
const maxViewDigits = 400

// maxPeriodDigits bounds the digits of the period Q divided by 10^F, the
// least common multiple of the view's multipleOf values at the grid's
// scale; Compile refuses a view beyond it.
const maxPeriodDigits = 400

// maxPassBytes is the longest literal on the grid that reaches the
// validator as written. A longer literal on the grid is written in its
// normalized form, and a literal longer than maxPassBytes charges its
// bytes as work units as stage F reads them (Options.Yield).
const maxPassBytes = 64

// chunkDigits is the number of mantissa digits read into one uint64 when
// the mantissa is reduced modulo the period.
const chunkDigits = 18

// numberForm says how bound handed a literal to the validator.
type numberForm uint8

const (
	// asWritten is the literal's own text.
	asWritten numberForm = iota
	// rewritten is the normalized form of a long literal on the grid,
	// with the literal's value, or 0 for a zero that is long or whose
	// exponent lies outside int64.
	rewritten
	// surrogate is a bounded value with the literal's properties.
	surrogate
)

// numberModel is the grid and period of one view. It is immutable after
// newNumberModel, and its big.Int values are only read, so it is safe for
// concurrent use.
type numberModel struct {
	// intDigits (I) and fracDigits (F) define the grid: the multiples of
	// 10^-F of magnitude below 10^I.
	intDigits, fracDigits int
	// period (Q) is the least common multiple of 10^F and each multipleOf
	// value times 10^F: v×10^F modulo Q decides whether a value v whose
	// v×10^F is an integer is an integer and a multiple of each
	// multipleOf value.
	period *big.Int
	// twoFive is the factor 2^x × 5^y of the period, coprime the rest,
	// tail max(x, y) (10^tail is 0 modulo twoFive) and inverse twoFive's
	// inverse modulo coprime, nil when coprime is 1.
	twoFive, coprime, inverse *big.Int
	tail                      int
	// shifted is period × 10^I, the step between the surrogates of two
	// tags beyond the grid.
	shifted *big.Int
	// unique records that the view uses uniqueItems: only then does
	// equality between instance numbers matter, and bound tags off-grid
	// values.
	unique bool
}

// newNumberModel reads every number of the decoded schema document doc
// (numbers as json.Number, as jsonschema.UnmarshalJSON decodes them),
// over-approximating where it cannot tell a keyword from data: the grid
// covers every number of the document, every number member named
// multipleOf joins the period, and any uniqueItems member that is true
// sets unique.
// Covering more numbers or multipleOf values than the validator compares
// keeps every property bound preserves. minDigits is the least integer and
// fraction digits of the grid (gridDigits; tests pass less). It returns an
// error, which Compile wraps in ErrSchema, for a number that is not an
// RFC 8259 literal, one that needs more than maxViewDigits integer or
// fraction digits, a zero whose exponent lies outside int64, and
// multipleOf values whose period needs more than maxPeriodDigits digits.
func newNumberModel(doc any, minDigits int) (*numberModel, error) {
	nm := &numberModel{intDigits: minDigits, fracDigits: minDigits}
	var mults []literal
	var walk func(v any, key string) error
	walk = func(v any, key string) error {
		switch t := v.(type) {
		case map[string]any:
			for _, k := range slices.Sorted(maps.Keys(t)) {
				if b, ok := t[k].(bool); ok && b && k == "uniqueItems" {
					nm.unique = true
				}
				if err := walk(t[k], k); err != nil {
					return err
				}
			}
		case []any:
			for _, x := range t {
				if err := walk(x, ""); err != nil {
					return err
				}
			}
		case json.Number:
			l, ok := parseLiteral(string(t), nil)
			if !ok {
				return fmt.Errorf("number %s is not an RFC 8259 number", shorten(string(t)))
			}
			if l.zero() {
				// The compiler parses a zero only when its exponent fits
				// in int64 (objCompiler.numVal returns nil otherwise, which
				// drops a bound and makes a const or enum match nothing).
				if !l.exponentInt64() {
					return fmt.Errorf("number %s has an exponent outside int64", shorten(string(t)))
				}
				return nil
			}
			e, fits := l.exponent()
			whole := int64(l.digits()) + e
			if !fits || whole > maxViewDigits || -e > maxViewDigits {
				return fmt.Errorf("number %s needs more than %d integer or fraction digits", shorten(string(t)), maxViewDigits)
			}
			nm.intDigits = max(nm.intDigits, int(whole))
			nm.fracDigits = max(nm.fracDigits, int(-e))
			if key == "multipleOf" && !l.neg {
				mults = append(mults, l)
			}
		default:
		}
		return nil
	}
	if err := walk(doc, ""); err != nil {
		return nil, err
	}
	scale := pow10(nm.fracDigits)
	q := new(big.Int).Set(scale)
	for i := range mults {
		e, _ := mults[i].exponent()
		m := mults[i].mantissaInt()
		m.Mul(m, pow10(int(e)+nm.fracDigits))
		var g big.Int
		g.GCD(nil, nil, q, m)
		q.Mul(q, m.Quo(m, &g))
		if over := new(big.Int).Quo(q, scale); len(over.String()) > maxPeriodDigits {
			return nil, fmt.Errorf("the multipleOf values need a period of more than %d digits", maxPeriodDigits)
		}
	}
	nm.period = q
	rest := new(big.Int).Set(q)
	two, five := 0, 0
	for rest.Bit(0) == 0 {
		rest.Rsh(rest, 1)
		two++
	}
	var quo, rem big.Int
	for {
		quo.QuoRem(rest, big.NewInt(5), &rem)
		if rem.Sign() != 0 {
			break
		}
		rest.Set(&quo)
		five++
	}
	nm.coprime = rest
	nm.tail = max(two, five)
	nm.twoFive = new(big.Int).Quo(q, rest)
	if rest.Cmp(big.NewInt(1)) > 0 {
		nm.inverse = new(big.Int).ModInverse(new(big.Int).Mod(nm.twoFive, rest), rest)
	}
	nm.shifted = new(big.Int).Mul(q, pow10(nm.intDigits))
	return nm, nil
}

// pow10 returns 10^n for n >= 0.
func pow10(n int) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(n)), nil)
}

// numberTags gives the off-grid values of one instance their tags: equal
// values the same tag, distinct values distinct tags, keyed by the exact
// sign, mantissa digits and exponent. A nil numberTags, for a view without
// uniqueItems, tags every value 0.
type numberTags struct {
	ids map[string]int
}

// newNumberTags returns the tags of one instance of a view with model nm.
func newNumberTags(nm *numberModel) *numberTags {
	if !nm.unique {
		return nil
	}
	return &numberTags{ids: map[string]int{}}
}

// tag returns the tag of l, whose exponent is e (fits as
// literal.exponent returns them). The key copies l's mantissa, charging
// its bytes on w; its map lookup, which hashes the key, is a span that
// cannot yield (Options.Yield).
func (nt *numberTags) tag(l *literal, e int64, fits bool, w *work) int {
	if nt == nil {
		return 0
	}
	key := l.key(e, fits, w)
	id, ok := nt.ids[key]
	if !ok {
		id = len(nt.ids)
		nt.ids[key] = id
	}
	return id
}

// bound returns the text the validator reads for the number literal s,
// how it was formed, and false when s is not an RFC 8259 number literal.
// Its result has at most maxBytes(n) bytes, n the distinct values tags
// holds after the call. A literal longer than maxPassBytes charges the
// bytes stage F reads or copies on w as it goes. A zero is handed as
// written when it is short and its exponent fits in int64, else as 0.
//
// For a literal off the grid, with tag t, sign σ and value v = σ×a:
//   - a below 10^I (so a×10^F is not an integer): σ×(g + 0.1t1)×10^-F,
//     g the integer part of a×10^F: strictly between the two grid points
//     around v, not an integer and not a multiple of any multipleOf value;
//   - a at least 10^I and a×10^F an integer: σ×(Q×10^I×(t+1) + r)×10^-F,
//     r a residue modulo Q that decides integrality and divisibility as
//     a×10^F's does (residue): beyond the grid, an integer and a multiple
//     of each multipleOf value exactly when v is;
//   - a at least 10^I otherwise: σ×((t+1)×10^(I+F+1) + 1)×10^-(F+1):
//     beyond the grid, not an integer and not a multiple of any
//     multipleOf value.
//
// Each surrogate is off the grid; within one form distinct tags give
// distinct values, and the forms do not overlap (the first below 10^I,
// the second with an integral s×10^F, the third without).
func (nm *numberModel) bound(s string, tags *numberTags, w *work) (string, numberForm, bool) {
	long := len(s) > maxPassBytes
	var cw *work
	if long {
		cw = w
	}
	l, ok := parseLiteral(s, cw)
	if !ok {
		return "", asWritten, false
	}
	if l.zero() {
		// jsonschema/v6 cannot parse a zero whose exponent lies outside
		// int64 (numbers.go), whatever its sign, fraction or exponent
		// spelling; 0 has its value.
		if !long && l.exponentInt64() {
			return s, asWritten, true
		}
		return "0", rewritten, true
	}
	e, fits := l.exponent()
	whole := int64(l.digits()) + e
	ip, fp := int64(nm.intDigits), int64(nm.fracDigits)
	if fits && e >= -fp && whole <= ip {
		if !long {
			return s, asWritten, true
		}
		return string(l.appendNormalized(nil, e)), rewritten, true
	}
	t := tags.tag(&l, e, fits, cw)
	var b []byte
	if l.neg {
		b = append(b, '-')
	}
	switch {
	case (fits && whole <= ip) || (!fits && l.expNeg):
		// Strictly between two grid points: e < -F. g has whole+F of the
		// mantissa's digits, at most I+F.
		g := 0
		if fits && whole+fp > 0 {
			g = int(whole + fp)
		}
		b = l.appendMantissa(b, l.first, l.first+g, nil)
		tag := strconv.Itoa(t)
		b = append(b, '1')
		b = append(b, tag...)
		b = append(b, '1')
		b = appendExponent(b, -(nm.fracDigits + 2 + len(tag)))
	case !fits || e >= -fp:
		n := new(big.Int).SetInt64(int64(t) + 1)
		n.Mul(n, nm.shifted)
		n.Add(n, nm.residue(&l, e, fits, cw))
		b = n.Append(b, 10)
		b = appendExponent(b, -nm.fracDigits)
	default:
		b = strconv.AppendInt(b, int64(t)+1, 10)
		b = append(b, strings.Repeat("0", nm.intDigits+nm.fracDigits)...)
		b = append(b, '1')
		b = appendExponent(b, -(nm.fracDigits + 1))
	}
	return string(b), surrogate, true
}

// maxBytes bounds the bytes of a text bound returns when the instance has
// at most tags distinct off-grid values: the longest of a literal as
// written, a normalized form and the three surrogate forms.
func (nm *numberModel) maxBytes(tags int) int {
	tagDigits := len(strconv.Itoa(max(tags, 0) + 2))
	expDigits := len(strconv.Itoa(nm.intDigits+nm.fracDigits+tagDigits+2)) + 2
	widest := max(nm.intDigits+nm.fracDigits+tagDigits+2, len(nm.shifted.String())+tagDigits)
	return max(maxPassBytes, 1+widest+expDigits)
}

// residue returns a residue r of a×10^F modulo the period, a = |v| beyond
// the grid with a×10^F an integer, that decides like the true one whether
// v is an integer and a multiple of each multipleOf value. With the
// mantissa D and shift = e+F, a×10^F = D×10^shift. For shift below tail
// it is the true residue. For a larger shift, D×10^shift is 0 modulo
// twoFive, and modulo coprime, to which 10 is prime, it is 0 exactly when
// D is, so r is 0 modulo twoFive and D modulo coprime: every multipleOf
// value times 10^F is a factor of twoFive times a factor of coprime,
// and v is an integer (shift >= tail >= F). Reducing D modulo coprime
// charges its digits on w.
func (nm *numberModel) residue(l *literal, e int64, fits bool, w *work) *big.Int {
	dr := new(big.Int)
	if nm.inverse != nil {
		dr = l.mod(nm.coprime, w)
	}
	if !fits || e+int64(nm.fracDigits) >= int64(nm.tail) {
		return nm.crt(new(big.Int), dr)
	}
	shift := e + int64(nm.fracDigits)
	// 10^tail is 0 modulo twoFive, so D's last tail digits decide D
	// modulo twoFive.
	from := max(l.first, l.last+1-nm.tail)
	d25, _ := new(big.Int).SetString(string(l.appendMantissa(nil, from, l.last+1, nil)), 10)
	d25.Mod(d25, nm.twoFive)
	r := nm.crt(d25, dr)
	r.Mul(r, new(big.Int).Exp(big.NewInt(10), big.NewInt(shift), nm.period))
	return r.Mod(r, nm.period)
}

// crt returns the residue modulo the period that is a25 modulo twoFive and
// ar modulo coprime.
func (nm *numberModel) crt(a25, ar *big.Int) *big.Int {
	if nm.inverse == nil {
		return a25
	}
	t := new(big.Int).Sub(ar, a25)
	t.Mul(t, nm.inverse)
	t.Mod(t, nm.coprime)
	t.Mul(t, nm.twoFive)
	return t.Add(t, a25)
}

// appendExponent appends the exponent k, nothing for 0.
func appendExponent(b []byte, k int) []byte {
	if k == 0 {
		return b
	}
	b = append(b, 'e')
	return strconv.AppendInt(b, int64(k), 10)
}

// literal is an RFC 8259 number literal split by its grammar. Its mantissa
// is intPart followed by fracPart; with D the mantissa from its first to
// its last nonzero digit (no leading or trailing zeros) the literal's
// value is (-1)^neg × D × 10^E, E = exponent + len(intPart) - 1 - last.
type literal struct {
	neg               bool
	intPart, fracPart string
	expNeg            bool
	// expDigits are the exponent's digits without sign and leading zeros;
	// empty for a zero exponent.
	expDigits string
	// first and last index the mantissa's first and last nonzero digits;
	// first is -1 for zero.
	first, last int
}

// parseLiteral splits s, reporting false when s is not an RFC 8259 number
// literal. It reads s once, charging w one unit per byte yieldEvery at a
// time (nil w charges nothing).
func parseLiteral(s string, w *work) (literal, bool) {
	l := literal{first: -1, last: -1}
	i := 0
	if i < len(s) && s[i] == '-' {
		l.neg = true
		i++
	}
	start := i
	end, first, last := scanDigits(s, i, w)
	if end == start || (s[start] == '0' && end-start > 1) {
		return l, false
	}
	l.intPart = s[start:end]
	if first >= 0 {
		l.first, l.last = first-start, last-start
	}
	i = end
	if i < len(s) && s[i] == '.' {
		i++
		start = i
		end, first, last = scanDigits(s, i, w)
		if end == start {
			return l, false
		}
		l.fracPart = s[start:end]
		if first >= 0 {
			if l.first < 0 {
				l.first = len(l.intPart) + first - start
			}
			l.last = len(l.intPart) + last - start
		}
		i = end
	}
	if i < len(s) && (s[i] == 'e' || s[i] == 'E') {
		i++
		if i < len(s) && (s[i] == '+' || s[i] == '-') {
			l.expNeg = s[i] == '-'
			i++
		}
		start = i
		end, first, _ = scanDigits(s, i, w)
		if end == start {
			return l, false
		}
		if first >= 0 {
			l.expDigits = s[first:end]
		}
		i = end
	}
	return l, i == len(s)
}

// scanDigits returns the end of the run of decimal digits of s starting at
// i and the offsets of its first and last nonzero digits (-1 when none),
// charging w one unit per digit, at most yieldEvery at a time.
func scanDigits(s string, i int, w *work) (end, first, last int) {
	first, last = -1, -1
	for {
		stop := min(len(s), i+yieldEvery)
		j := i
		for ; j < stop && '0' <= s[j] && s[j] <= '9'; j++ {
			if s[j] != '0' {
				if first < 0 {
					first = j
				}
				last = j
			}
		}
		w.charge(j - i)
		if j < stop || j == len(s) {
			return j, first, last
		}
		i = j
	}
}

// zero reports a literal whose value is zero.
func (l *literal) zero() bool { return l.first < 0 }

// digits returns the number of digits of D.
func (l *literal) digits() int { return l.last - l.first + 1 }

// digit returns mantissa digit i.
func (l *literal) digit(i int) byte {
	if i < len(l.intPart) {
		return l.intPart[i]
	}
	return l.fracPart[i-len(l.intPart)]
}

// exponent returns E, the power of ten of D, and true; or false when the
// exponent text has more than 18 significant digits, so that |E| exceeds
// 10^18 minus the mantissa's length, beyond any grid.
func (l *literal) exponent() (int64, bool) {
	if len(l.expDigits) > 18 {
		return 0, false
	}
	var e int64
	for i := range len(l.expDigits) {
		e = e*10 + int64(l.expDigits[i]-'0')
	}
	if l.expNeg {
		e = -e
	}
	return e + int64(len(l.intPart)-1-l.last), true
}

// exponentInt64 reports whether the literal's exponent as written, with
// its sign, fits in int64: what big.Rat.SetString, and so jsonschema/v6,
// needs to parse the literal at all (strconv.ParseInt reads it first).
// expDigits has no leading zeros, so its length and, at 19 digits, its
// text order compare the magnitude with 2^63-1 or, negative, 2^63.
func (l *literal) exponentInt64() bool {
	const maxInt64 = "9223372036854775807"
	switch d := l.expDigits; {
	case len(d) != len(maxInt64):
		return len(d) < len(maxInt64)
	case l.expNeg:
		return d <= "9223372036854775808"
	default:
		return d <= maxInt64
	}
}

// appendMantissa appends mantissa digits from to to, charging w one unit
// per digit, at most yieldEvery at a time.
func (l *literal) appendMantissa(b []byte, from, to int, w *work) []byte {
	n := len(l.intPart)
	for from < to {
		end := min(to, from+yieldEvery)
		b = append(b, l.intPart[min(from, n):min(end, n)]...)
		b = append(b, l.fracPart[max(from, n)-n:max(end, n)-n]...)
		w.charge(end - from)
		from = end
	}
	return b
}

// appendNormalized appends the literal in its normalized form D e E with
// sign, for a literal whose exponent e fits.
func (l *literal) appendNormalized(b []byte, e int64) []byte {
	if l.neg {
		b = append(b, '-')
	}
	b = l.appendMantissa(b, l.first, l.last+1, nil)
	return appendExponent(b, int(e))
}

// mantissaInt returns D for a nonzero literal of a view.
func (l *literal) mantissaInt() *big.Int {
	d, _ := new(big.Int).SetString(string(l.appendMantissa(nil, l.first, l.last+1, nil)), 10)
	return d
}

// mod returns D modulo m, reading chunkDigits digits at a time and
// charging w one unit per digit.
func (l *literal) mod(m *big.Int, w *work) *big.Int {
	acc := new(big.Int)
	var chunk, scale big.Int
	for i := l.first; i <= l.last; {
		end := min(l.last+1, i+chunkDigits)
		var v, p uint64 = 0, 1
		for j := i; j < end; j++ {
			v = v*10 + uint64(l.digit(j)-'0')
			p *= 10
		}
		acc.Mul(acc, scale.SetUint64(p))
		acc.Add(acc, chunk.SetUint64(v))
		acc.Mod(acc, m)
		w.charge(end - i)
		i = end
	}
	return acc
}

// key returns the exact identity of a nonzero value: sign, D and E in
// decimal. A fitting exponent is e; otherwise E is the exponent, more than
// 18 digits long, plus the mantissa shift, added in decimal
// (appendExponentKey). Copying D and the exponent charges their bytes on
// w.
func (l *literal) key(e int64, fits bool, w *work) string {
	b := make([]byte, 0, l.digits()+len(l.expDigits)+24)
	if l.neg {
		b = append(b, '-')
	} else {
		b = append(b, '+')
	}
	b = l.appendMantissa(b, l.first, l.last+1, w)
	b = append(b, 'e')
	if fits {
		return string(strconv.AppendInt(b, e, 10))
	}
	shift := int64(len(l.intPart) - 1 - l.last)
	if l.expNeg {
		b = append(b, '-')
		shift = -shift
	}
	return string(appendExponentKey(b, l.expDigits, shift, w))
}

// decimalDigits maps a digit value to its character.
const decimalDigits = "0123456789"

// appendExponentKey appends the decimal digits of x + delta least
// significant first, x a decimal of more than 18 digits without leading
// zeros and |delta| below 10^18, so the sum is positive. In that order a
// carry past the top appends a digit and a borrow trims the top zeros at
// the end, so no digit moves: it charges w one unit per digit it copies,
// yieldEvery at a time, and one per digit a carry or borrow reaches.
func appendExponentKey(b []byte, x string, delta int64, w *work) []byte {
	start := len(b)
	for i := len(x); i > 0; {
		lo := max(0, i-yieldEvery)
		for j := i - 1; j >= lo; j-- {
			b = append(b, x[j])
		}
		w.charge(i - lo)
		i = lo
	}
	if delta > 0 {
		carry := uint64(delta)
		for i := start; carry > 0; i++ {
			if i == len(b) {
				b = append(b, '0')
			}
			v := uint64(b[i]-'0') + carry
			b[i] = decimalDigits[v%10]
			carry = v / 10
			w.tick()
		}
		return b
	}
	borrow := uint64(-delta)
	for i := start; borrow > 0; i++ {
		v := int64(b[i]-'0') - int64(borrow%10)
		borrow /= 10
		if v < 0 {
			v += 10
			borrow++
		}
		b[i] = decimalDigits[v]
		w.tick()
	}
	for len(b) > start+1 && b[len(b)-1] == '0' {
		b = b[:len(b)-1]
	}
	return b
}
