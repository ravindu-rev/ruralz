// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package signing

import (
	"errors"
	"fmt"
	"math"
)

// ErrTimestampEncoding reports a bundle whose RFC 3161 timestamp is not in
// DER's TLV structure (checkDER).
var ErrTimestampEncoding = errors.New("signing: RFC 3161 timestamp not in DER")

// checkDER reports whether b is exactly one ASN.1 element in the TLV
// structure DER requires, under the rules encoding/asn1 applies to every
// element it parses: the tag number in its shortest form, at most 31 bits;
// a definite length in the fewest octets; and the content of each
// constructed element made of whole elements that end where it ends. Values
// stay opaque. The bundle format encodes each RFC 3161 timestamp in DER,
// and a timestamp that is not is refused before sigstore-go parses it: the
// BER decoder of github.com/digitorus/pkcs7, which its timestamp check
// links, re-parses the bytes an element claims past its parent's end, so a
// crafted timestamp of some hundred bytes exhausts memory at every version
// on the Go version floor (docs/architecture/08-security-and-identity.md
// "Signed Revisions and Plugin artifacts"). The walk is iterative, so
// nesting costs no stack.
func checkDER(b []byte) error {
	if len(b) == 0 {
		return errors.New("no ASN.1 element")
	}
	// ends are the end offsets of the open constructed elements, innermost
	// last; each lies within the one before it.
	var ends []int
	off := 0
	for {
		limit := len(b)
		if n := len(ends); n > 0 {
			limit = ends[n-1]
		}
		constructed, content, end, err := readTLV(b, off, limit)
		if err != nil {
			return err
		}
		if constructed {
			ends = append(ends, end)
			off = content
		} else {
			off = end
		}
		for n := len(ends); n > 0 && off == ends[n-1]; n = len(ends) {
			ends = ends[:n-1]
		}
		if len(ends) == 0 {
			if off != len(b) {
				return fmt.Errorf("offset %d: data after the element", off)
			}
			return nil
		}
	}
}

// readTLV reads the identifier and length octets of the element at off,
// which must end by limit, and returns whether it is constructed, where its
// content starts and where it ends.
func readTLV(b []byte, off, limit int) (constructed bool, content, end int, err error) {
	start := off
	if off >= limit {
		return false, 0, 0, fmt.Errorf("offset %d: truncated element", start)
	}
	id := b[off]
	off++
	constructed = id&0x20 != 0
	if id&0x1f == 0x1f {
		// The tag number follows in base 128, at most 31 bits, with no
		// leading zero group, and is at least 31: smaller ones fit in the
		// identifier octet.
		var tag uint64
		for i := 0; ; i++ {
			if off >= limit {
				return false, 0, 0, fmt.Errorf("offset %d: truncated tag", start)
			}
			c := b[off]
			off++
			if i == 0 && c == 0x80 {
				return false, 0, 0, fmt.Errorf("offset %d: tag number not minimally encoded", start)
			}
			if i == 5 {
				return false, 0, 0, fmt.Errorf("offset %d: tag number too large", start)
			}
			tag = tag<<7 | uint64(c&0x7f)
			if c&0x80 == 0 {
				break
			}
		}
		if tag > math.MaxInt32 {
			return false, 0, 0, fmt.Errorf("offset %d: tag number too large", start)
		}
		if tag < 0x1f {
			return false, 0, 0, fmt.Errorf("offset %d: tag number not in its short form", start)
		}
	}
	if off >= limit {
		return false, 0, 0, fmt.Errorf("offset %d: truncated length", start)
	}
	l := b[off]
	off++
	length := int(l)
	if l&0x80 != 0 {
		n := int(l & 0x7f)
		if n == 0 {
			return false, 0, 0, fmt.Errorf("offset %d: indefinite length", start)
		}
		length = 0
		for i := range n {
			if off >= limit {
				return false, 0, 0, fmt.Errorf("offset %d: truncated length", start)
			}
			c := b[off]
			off++
			if i == 0 && c == 0 {
				return false, 0, 0, fmt.Errorf("offset %d: length with a leading zero octet", start)
			}
			// A length past limit after this octet overruns: stopping here
			// keeps the shift from overflowing.
			if length > limit>>8 {
				return false, 0, 0, fmt.Errorf("offset %d: element overruns its parent", start)
			}
			length = length<<8 | int(c)
		}
		if length < 0x80 {
			return false, 0, 0, fmt.Errorf("offset %d: length not in its short form", start)
		}
	}
	if length > limit-off {
		return false, 0, 0, fmt.Errorf("offset %d: element overruns its parent", start)
	}
	return constructed, off, off + length, nil
}
