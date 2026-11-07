// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"bytes"
	"encoding/asn1"
	"encoding/base64"
	"errors"
	"testing"
)

// pkcs7Overrun is crafted BER whose definite-length elements overrun
// their parents inside an indefinite-length one: the BER decoder of
// github.com/digitorus/pkcs7 on the version floor re-parses the overrun
// bytes at every level and exhausts memory on it
// (docs/architecture/08-security-and-identity.md "Signed Revisions and
// Plugin artifacts").
const pkcs7Overrun = "MIAwAjACMAIwAjACMAIwMDACMDAwAjAwMAIwIDACMCAwAjAgMAIwIDACMCAwAjACMAIwAjACMAIwAjACMAIwMDACMAIwAjACMAIwAjACMDAwAjAwMAIwMDACMCAwAjAgMAIwIDACMCAwAjACMAIwAjACMAIwAjACMAIwAjACMAIwAjACQQIwMDA="

// seqHeader returns the identifier and DER length octets of a SEQUENCE
// of n octets of content.
func seqHeader(n int) []byte {
	if n < 0x80 {
		return []byte{0x30, lowOctet(n)}
	}
	var octets []byte
	for v := n; v > 0; v >>= 8 {
		octets = append([]byte{lowOctet(v)}, octets...)
	}
	return append([]byte{0x30, 0x80 | lowOctet(len(octets))}, octets...)
}

// lowOctet returns the low eight bits of v.
func lowOctet(v int) byte {
	return byte(v & 0xff)
}

// seq encodes a DER SEQUENCE of content.
func seq(content []byte) []byte {
	return append(seqHeader(len(content)), content...)
}

// nested returns depth SEQUENCEs, each holding the next, around an empty
// one.
func nested(depth int) []byte {
	headers := make([][]byte, depth)
	size := 2
	for i := range headers {
		headers[i] = seqHeader(size)
		size += len(headers[i])
	}
	out := make([]byte, 0, size)
	for i := depth - 1; i >= 0; i-- {
		out = append(out, headers[i]...)
	}
	return append(out, 0x30, 0x00)
}

// overrunTimestamp is an RFC 3161 response whose token, of a definite
// length that encoding/asn1 takes, holds pkcs7Overrun.
func overrunTimestamp(t *testing.T) []byte {
	t.Helper()
	ber, err := base64.StdEncoding.DecodeString(pkcs7Overrun)
	if err != nil {
		t.Fatal(err)
	}
	status := seq([]byte{0x02, 0x01, 0x00})
	return seq(append(status, seq(ber)...))
}

func TestCheckDER(t *testing.T) {
	ber, err := base64.StdEncoding.DecodeString(pkcs7Overrun)
	if err != nil {
		t.Fatal(err)
	}
	long := bytes.Repeat([]byte{0x41}, 0x80)
	longer := bytes.Repeat([]byte{0x41}, 0x100)
	tests := []struct {
		name string
		in   []byte
		want string // "" accepts
	}{
		{name: "primitive", in: []byte{0x02, 0x01, 0x05}},
		{name: "empty constructed", in: []byte{0x30, 0x00}},
		{name: "nested", in: []byte{0x30, 0x05, 0x30, 0x03, 0x02, 0x01, 0x05}},
		{name: "siblings", in: []byte{0x31, 0x06, 0x02, 0x01, 0x01, 0x02, 0x01, 0x02}},
		{name: "context-specific constructed", in: []byte{0xa0, 0x03, 0x02, 0x01, 0x01}},
		{name: "empty elements end their parents together", in: []byte{0x30, 0x04, 0x30, 0x02, 0x30, 0x00}},
		{name: "high tag number 31", in: []byte{0x1f, 0x1f, 0x00}},
		{name: "high tag number 128", in: []byte{0x9f, 0x81, 0x00, 0x00}},
		{name: "high tag number of 31 bits", in: []byte{0x1f, 0x87, 0xff, 0xff, 0xff, 0x7f, 0x00}},
		{name: "one length octet", in: append([]byte{0x04, 0x81, 0x80}, long...)},
		{name: "two length octets", in: append([]byte{0x04, 0x82, 0x01, 0x00}, longer...)},
		{name: "deep nesting", in: nested(100000)},
		{name: "a crafted RFC 3161 response", in: overrunTimestamp(t), want: "offset 11: indefinite length"},

		{name: "empty", in: nil, want: "no ASN.1 element"},
		{name: "no length", in: []byte{0x30}, want: "offset 0: truncated length"},
		{name: "indefinite length", in: []byte{0x30, 0x80, 0x00, 0x00}, want: "offset 0: indefinite length"},
		{name: "indefinite length inside", in: []byte{0x30, 0x04, 0x30, 0x80, 0x00, 0x00}, want: "offset 2: indefinite length"},
		{name: "crafted BER overrun", in: ber, want: "offset 0: indefinite length"},
		{name: "long form of a short length", in: []byte{0x04, 0x81, 0x01, 0x41}, want: "offset 0: length not in its short form"},
		{name: "leading zero length octet", in: append([]byte{0x04, 0x82, 0x00, 0x80}, long...), want: "offset 0: length with a leading zero octet"},
		{name: "reserved length octet", in: []byte{0x04, 0xff, 0x01}, want: "offset 0: truncated length"},
		{name: "truncated long length", in: []byte{0x30, 0x82, 0x01}, want: "offset 0: truncated length"},
		{name: "length beyond the data", in: []byte{0x30, 0x84, 0x7f, 0xff, 0xff, 0xff}, want: "offset 0: element overruns its parent"},
		{name: "many length octets", in: append([]byte{0x04, 0xfe}, bytes.Repeat([]byte{0x01}, 126)...), want: "offset 0: element overruns its parent"},
		{name: "content shorter than its length", in: []byte{0x30, 0x03, 0x02, 0x01}, want: "offset 0: element overruns its parent"},
		{name: "child overruns its parent", in: []byte{0x30, 0x05, 0x30, 0x02, 0x02, 0x01, 0x05}, want: "offset 4: element overruns its parent"},
		{name: "child ends inside its parent's sibling", in: []byte{0x30, 0x07, 0x30, 0x01, 0x02, 0x01, 0x05, 0x02, 0x00}, want: "offset 4: truncated length"},
		{name: "data after the element", in: []byte{0x02, 0x01, 0x05, 0x00}, want: "offset 3: data after the element"},
		{name: "second top-level element", in: []byte{0x30, 0x00, 0x30, 0x00}, want: "offset 2: data after the element"},
		{name: "truncated high tag", in: []byte{0x1f, 0x81}, want: "offset 0: truncated tag"},
		{name: "high tag with a leading zero group", in: []byte{0x1f, 0x80, 0x1f, 0x00}, want: "offset 0: tag number not minimally encoded"},
		{name: "high form of a low tag number", in: []byte{0x1f, 0x05, 0x00}, want: "offset 0: tag number not in its short form"},
		{name: "high tag number over 31 bits", in: []byte{0x1f, 0x88, 0x80, 0x80, 0x80, 0x00, 0x00}, want: "offset 0: tag number too large"},
		{name: "high tag number of six octets", in: []byte{0x1f, 0x81, 0x80, 0x80, 0x80, 0x80, 0x00, 0x00}, want: "offset 0: tag number too large"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkDER(tt.in)
			if tt.want == "" {
				if err != nil {
					t.Fatalf("checkDER = %v", err)
				}
				return
			}
			if err == nil || err.Error() != tt.want {
				t.Fatalf("checkDER = %v, want %q", err, tt.want)
			}
		})
	}
}

// asn1Walk checks b with encoding/asn1, element by element: the reference
// checkDER follows.
func asn1Walk(b []byte) error {
	var raw asn1.RawValue
	rest, err := asn1.Unmarshal(b, &raw)
	if err != nil {
		return err
	}
	if len(rest) > 0 {
		return errors.New("data after the element")
	}
	return asn1Children(raw)
}

// asn1Children checks the content of a constructed element.
func asn1Children(raw asn1.RawValue) error {
	if !raw.IsCompound {
		return nil
	}
	for rest := raw.Bytes; len(rest) > 0; {
		var child asn1.RawValue
		var err error
		if rest, err = asn1.Unmarshal(rest, &child); err != nil {
			return err
		}
		if err := asn1Children(child); err != nil {
			return err
		}
	}
	return nil
}

// FuzzCheckDER checks that checkDER never panics and accepts exactly what
// encoding/asn1 parses, element by element.
func FuzzCheckDER(f *testing.F) {
	for _, seed := range [][]byte{
		{0x30, 0x00},
		{0x02, 0x01, 0x05},
		{0x30, 0x05, 0x30, 0x03, 0x02, 0x01, 0x05},
		{0x30, 0x80, 0x00, 0x00},
		{0x1f, 0x1f, 0x00},
		{0x9f, 0x81, 0x00, 0x00},
		{0x04, 0x81, 0x01, 0x41},
		{0x30, 0x05, 0x30, 0x02, 0x02, 0x01, 0x05},
		{0x1f, 0x80, 0x1f, 0x00},
		{0x02, 0x01, 0x05, 0x00},
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		got := checkDER(b)
		want := asn1Walk(b)
		if (got == nil) != (want == nil) {
			t.Fatalf("checkDER(% x) = %v, encoding/asn1 %v", b, got, want)
		}
	})
}
