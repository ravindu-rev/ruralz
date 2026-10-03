// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package sfv

import (
	"errors"
	"strconv"
)

// Errors of this package. Serialization and parsing errors wrap them with
// the failing rule; match them with errors.Is.
var (
	// ErrSerialize reports a value RFC 9651 section 4.1 cannot serialize.
	ErrSerialize = errors.New("sfv: value cannot be serialized")
	// ErrParse reports input that is not a valid structured field (RFC
	// 9651 section 4.2).
	ErrParse = errors.New("sfv: invalid structured field")
)

// Integer bounds (RFC 9651 section 3.3.1): at most 15 decimal digits.
const (
	MaxInteger = 999_999_999_999_999
	MinInteger = -MaxInteger
)

// Kind is the type of a bare item.
type Kind uint8

// Bare item types (RFC 9651 section 3.3). The zero Kind marks the zero
// BareItem, which cannot be serialized.
const (
	KindInteger Kind = iota + 1
	KindDecimal
	KindString
	KindToken
	KindByteSequence
	KindBoolean
	KindDate
	KindDisplayString
)

// String returns the RFC 9651 name of the type.
func (k Kind) String() string {
	switch k {
	case KindInteger:
		return "Integer"
	case KindDecimal:
		return "Decimal"
	case KindString:
		return "String"
	case KindToken:
		return "Token"
	case KindByteSequence:
		return "Byte Sequence"
	case KindBoolean:
		return "Boolean"
	case KindDate:
		return "Date"
	case KindDisplayString:
		return "Display String"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// BareItem is one RFC 9651 bare item. Build it with the constructor of its
// type; the constructors do not validate, the serializer does. BareItem is
// comparable.
type BareItem struct {
	kind Kind
	num  int64   // Integer, Date, Boolean (1 or 0)
	dec  float64 // Decimal
	str  string  // String, Token, Display String, Byte Sequence (raw bytes)
}

// Integer returns an Integer bare item; it serializes only between
// MinInteger and MaxInteger.
func Integer(n int64) BareItem { return BareItem{kind: KindInteger, num: n} }

// Decimal returns a Decimal bare item. It serializes rounded to three
// fractional digits (half to even) and only with at most 12 integer digits
// after rounding; NaN and infinities fail. Rounding uses the exact binary
// value of f, so a decimal tie that float64 cannot represent rounds by
// that value and can differ from rounding the decimal text: 0.0025 is
// stored just above the tie and serializes as 0.003, where RFC 9651
// rounding of the text gives 0.002.
func Decimal(f float64) BareItem { return BareItem{kind: KindDecimal, dec: f} }

// String returns a String bare item; it serializes only when every byte is
// printable ASCII (0x20-0x7E).
func String(s string) BareItem { return BareItem{kind: KindString, str: s} }

// Token returns a Token bare item; it serializes only when s starts with
// ALPHA or "*" and continues with tchar, ":" or "/".
func Token(s string) BareItem { return BareItem{kind: KindToken, str: s} }

// ByteSequence returns a Byte Sequence bare item holding a copy of b.
func ByteSequence(b []byte) BareItem { return BareItem{kind: KindByteSequence, str: string(b)} }

// Boolean returns a Boolean bare item.
func Boolean(b bool) BareItem {
	if b {
		return BareItem{kind: KindBoolean, num: 1}
	}
	return BareItem{kind: KindBoolean}
}

// Date returns a Date bare item for seconds since the Unix epoch; it
// serializes only between MinInteger and MaxInteger.
func Date(unix int64) BareItem { return BareItem{kind: KindDate, num: unix} }

// DisplayString returns a Display String bare item; it serializes only
// when s is valid UTF-8.
func DisplayString(s string) BareItem { return BareItem{kind: KindDisplayString, str: s} }

// Kind returns the item's type; 0 for the zero BareItem.
func (b BareItem) Kind() Kind { return b.kind }

// Int returns the value of an Integer or Date item, else 0.
func (b BareItem) Int() int64 {
	if b.kind == KindInteger || b.kind == KindDate {
		return b.num
	}
	return 0
}

// Float returns the value of a Decimal item, else 0.
func (b BareItem) Float() float64 {
	if b.kind == KindDecimal {
		return b.dec
	}
	return 0
}

// Text returns the value of a String, Token or Display String item, else
// "".
func (b BareItem) Text() string {
	switch b.kind {
	case KindString, KindToken, KindDisplayString:
		return b.str
	default:
		return ""
	}
}

// Bytes returns a copy of the value of a Byte Sequence item, else nil.
func (b BareItem) Bytes() []byte {
	if b.kind == KindByteSequence {
		return []byte(b.str)
	}
	return nil
}

// Bool returns the value of a Boolean item, else false.
func (b BareItem) Bool() bool {
	return b.kind == KindBoolean && b.num == 1
}

// Param is one parameter: a key and a bare item. A Boolean true value
// serializes as the bare key.
type Param struct {
	// Key is the parameter key: lcalpha or "*", then lcalpha, DIGIT, "_",
	// "-", "." or "*".
	Key string
	// Value is the parameter value.
	Value BareItem
}

// Item is a bare item with its parameters. Parameter keys must be unique.
type Item struct {
	// Value is the bare item.
	Value BareItem
	// Params are the item's parameters, in order.
	Params []Param
}

// Member is one List member: an Item, or an Inner List when Inner is true.
type Member struct {
	// Inner marks an Inner List member; its Items are serialized in
	// parentheses and Value is ignored.
	Inner bool
	// Value is the bare item of an Item member.
	Value BareItem
	// Items are the members of an Inner List.
	Items []Item
	// Params are the member's parameters, in order.
	Params []Param
}

// List is an RFC 9651 List. An empty List is not serialized: the field is
// omitted.
type List []Member

// ItemMember returns an Item member.
func ItemMember(v BareItem, params ...Param) Member {
	return Member{Value: v, Params: params}
}

// InnerListMember returns an Inner List member.
func InnerListMember(items []Item, params ...Param) Member {
	return Member{Inner: true, Items: items, Params: params}
}
