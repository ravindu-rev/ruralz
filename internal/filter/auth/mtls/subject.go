// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mtls

import (
	"crypto/x509"
	"encoding/asn1"
	"encoding/hex"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// attributeTypeAndValue is one RDN member, its value kept as encoded so
// the "#" form repeats the certificate's own bytes.
type attributeTypeAndValue struct {
	Type  asn1.ObjectIdentifier
	Value asn1.RawValue
}

// relativeDistinguishedNameSET is one RDN; the SET suffix makes
// encoding/asn1 read it as a SET OF.
type relativeDistinguishedNameSET []attributeTypeAndValue

// rdnSequence is a Name in encoded order.
type rdnSequence []relativeDistinguishedNameSET

// Subject returns the RFC 4514 string of c's subject, the form
// config.subjects[].subject and credentials.certificates[].subject match
// exactly, source.clientCertSubject shows and the principal mtls:<subject>
// carries (RFC 4514 section 2):
//
//   - the RDNs from the last one encoded to the first, joined by ",", and
//     the members of a multi-valued RDN joined by "+" in encoded order;
//   - an attribute type is its RFC 4514 section 3 short name (CN, L, ST,
//     O, OU, C, STREET, DC, UID), else its dotted OID;
//   - a value of a short-named type held as a directory string is its
//     UTF-8 text, a T61String read as Latin-1 and a BMPString as UCS-2 as
//     crypto/x509 reads them, with a leading space or "#", a trailing
//     space and any of `"+,;<>\` escaped by a backslash and NUL as \00;
//   - every other value is "#" and the lower-case hex of its BER
//     encoding as the certificate holds it.
//
// A subject encoding/asn1 cannot read, which crypto/x509 accepted, falls
// back to crypto/x509's own rendering. An empty subject is "".
func Subject(c *x509.Certificate) string {
	var seq rdnSequence
	rest, err := asn1.Unmarshal(c.RawSubject, &seq)
	if err != nil || len(rest) != 0 {
		return c.Subject.String()
	}
	var b strings.Builder
	for i := len(seq) - 1; i >= 0; i-- {
		if len(seq[i]) == 0 {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		for j, atv := range seq[i] {
			if j > 0 {
				b.WriteByte('+')
			}
			writeAttribute(&b, atv)
		}
	}
	return b.String()
}

// writeAttribute appends one type=value pair.
func writeAttribute(b *strings.Builder, atv attributeTypeAndValue) {
	name := shortName(atv.Type)
	if name == "" {
		b.WriteString(atv.Type.String())
		writeHexValue(b, atv.Value.FullBytes)
		return
	}
	b.WriteString(name)
	s, ok := directoryString(atv.Value)
	if !ok {
		writeHexValue(b, atv.Value.FullBytes)
		return
	}
	b.WriteByte('=')
	writeEscaped(b, s)
}

// writeHexValue appends "=#" and the lower-case hex of der.
func writeHexValue(b *strings.Builder, der []byte) {
	b.WriteString("=#")
	b.WriteString(hex.EncodeToString(der))
}

// shortName returns the RFC 4514 section 3 short name of oid, "" for any
// other type.
func shortName(oid asn1.ObjectIdentifier) string {
	switch {
	case len(oid) == 4 && oid[0] == 2 && oid[1] == 5 && oid[2] == 4:
		switch oid[3] {
		case 3:
			return "CN"
		case 6:
			return "C"
		case 7:
			return "L"
		case 8:
			return "ST"
		case 9:
			return "STREET"
		case 10:
			return "O"
		case 11:
			return "OU"
		}
	case len(oid) == 7 && oid[0] == 0 && oid[1] == 9 && oid[2] == 2342 && oid[3] == 19200300 && oid[4] == 100 && oid[5] == 1:
		switch oid[6] {
		case 1:
			return "UID"
		case 25:
			return "DC"
		}
	}
	return ""
}

// directoryString decodes a primitive universal string value to UTF-8 the
// way crypto/x509 does; it reports false for any other value.
func directoryString(v asn1.RawValue) (string, bool) {
	if v.Class != asn1.ClassUniversal || v.IsCompound {
		return "", false
	}
	switch v.Tag {
	case asn1.TagUTF8String:
		if !utf8.Valid(v.Bytes) {
			return "", false
		}
		return string(v.Bytes), true
	case asn1.TagPrintableString, asn1.TagIA5String, asn1.TagNumericString:
		for _, c := range v.Bytes {
			if c >= utf8.RuneSelf {
				return "", false
			}
		}
		return string(v.Bytes), true
	case asn1.TagT61String:
		r := make([]rune, len(v.Bytes))
		for i, c := range v.Bytes {
			r[i] = rune(c)
		}
		return string(r), true
	case asn1.TagBMPString:
		return bmpString(v.Bytes)
	default:
		return "", false
	}
}

// bmpString decodes UCS-2 big-endian text as crypto/x509 does: a
// terminating NUL is dropped, and surrogates and the noncharacters FFFE,
// FFFF and FDD0 to FDEF are refused.
func bmpString(b []byte) (string, bool) {
	if len(b)%2 != 0 {
		return "", false
	}
	if l := len(b); l >= 2 && b[l-1] == 0 && b[l-2] == 0 {
		b = b[:l-2]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i < len(b); i += 2 {
		p := uint16(b[i])<<8 | uint16(b[i+1])
		if p == 0xfffe || p == 0xffff || (p >= 0xfdd0 && p <= 0xfdef) || (p >= 0xd800 && p <= 0xdfff) {
			return "", false
		}
		u = append(u, p)
	}
	return string(utf16.Decode(u)), true
}

// writeEscaped appends s with the escapes RFC 4514 section 2.4 requires.
func writeEscaped(b *strings.Builder, s string) {
	for i := range len(s) {
		c := s[i]
		switch {
		case c == 0:
			b.WriteString(`\00`)
		case c == '"' || c == '+' || c == ',' || c == ';' || c == '<' || c == '>' || c == '\\',
			i == 0 && (c == ' ' || c == '#'),
			i == len(s)-1 && c == ' ':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
}

// uriSANs returns the URI subject alternative names of c exactly as the
// certificate holds them (GeneralName [6] uniformResourceIdentifier), the
// strings config.subjects[].uriSan and credentials.certificates[].uriSan
// match exactly. An extension encoding/asn1 cannot read, which
// crypto/x509 accepted, falls back to crypto/x509's parsed URIs.
func uriSANs(c *x509.Certificate) []string {
	for _, ext := range c.Extensions {
		if !isSubjectAltName(ext.Id) {
			continue
		}
		var names []asn1.RawValue
		if rest, err := asn1.Unmarshal(ext.Value, &names); err != nil || len(rest) != 0 {
			break
		}
		var out []string
		for _, n := range names {
			if n.Class == asn1.ClassContextSpecific && n.Tag == 6 && !n.IsCompound {
				out = append(out, string(n.Bytes))
			}
		}
		return out
	}
	if len(c.URIs) == 0 {
		return nil
	}
	out := make([]string, len(c.URIs))
	for i, u := range c.URIs {
		out[i] = u.String()
	}
	return out
}

// isSubjectAltName reports the subjectAltName extension, 2.5.29.17.
func isSubjectAltName(oid asn1.ObjectIdentifier) bool {
	return len(oid) == 4 && oid[0] == 2 && oid[1] == 5 && oid[2] == 29 && oid[3] == 17
}
