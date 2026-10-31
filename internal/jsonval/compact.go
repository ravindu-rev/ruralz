// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"io"
)

// Compactor rewrites JSON texts in the transform form without building a
// tree (package documentation, "Encoding"): no insignificant whitespace,
// object members in their input order, strings and member names with the
// minimal escaping of AppendString and numbers and literals verbatim, so
// the output keeps each object's member order and each number's text. It
// checks its input as a Scanner with its Options does. A Compactor reuses
// its scanner and unescape buffer, so after warm-up Append allocates
// nothing beyond the growth of dst; like Decoder, it drops its reference to
// the input and every scratch structure over about 64 KiB after each call.
// The zero Compactor is ready to use with the strict defaults.
type Compactor struct {
	// Options selects the checks: the zero value is the strict default
	// (nesting depth DefaultMaxDepth, duplicate member names rejected).
	Options ScanOptions
	s       Scanner
	buf     []byte
}

// Append appends src, one JSON text, to dst in the transform form. An input
// the Scanner refuses is its *Error, and dst then holds a partial value.
// Appending the output of Append again yields identical bytes.
func (c *Compactor) Append(dst, src []byte) ([]byte, error) {
	c.s.Reset(src, c.Options)
	defer c.release()
	base := len(dst)
	for {
		t, err := c.s.Next()
		switch {
		case errors.Is(err, io.EOF):
			return dst, nil
		case err != nil:
			return dst, err
		}
		if t.Kind == KindObjectEnd || t.Kind == KindArrayEnd {
			dst = append(dst, src[t.Offset])
			continue
		}
		// The Scanner has checked the structure, so the byte written last
		// tells whether a separator is due: none after an opening bracket
		// or a name's colon, a comma after any value.
		if len(dst) > base {
			switch dst[len(dst)-1] {
			case '{', '[', ':':
			default:
				dst = append(dst, ',')
			}
		}
		switch t.Kind {
		case KindString, KindName:
			if t.Escaped {
				c.buf = c.s.AppendText(c.buf[:0], t)
				// The Scanner has checked the UTF-8 and the surrogate
				// pairs, so the decoded text always encodes.
				dst, _ = AppendStringBytes(dst, c.buf)
			} else {
				// Text without an escape holds no quote, backslash or
				// control character: it is its own minimal form.
				dst = append(dst, src[t.Offset:t.End]...)
			}
			if t.Kind == KindName {
				dst = append(dst, ':')
			}
		default:
			dst = append(dst, src[t.Offset:t.End]...)
		}
	}
}

// release drops the input and an oversized unescape buffer.
func (c *Compactor) release() {
	c.s.Reset(nil, ScanOptions{})
	if cap(c.buf) > maxScratch {
		c.buf = nil
	}
}
