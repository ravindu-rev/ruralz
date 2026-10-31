// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"encoding/binary"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// MaxEntry is the largest encoded entry ("Response caching", Size row:
// 128 KiB, target), its header blocks and saved request included.
const MaxEntry = 128 << 10

// formatV1 is the entry format this release reads and writes
// (docs/architecture/11-scalability-and-distributed-state.md "Response
// Cache layout", e:<V> row).
const formatV1 = 1

// Entry flags.
const (
	// flagCredentialed: the storing request carried credentials, so the
	// entry is never revalidated in the background and serves stale only
	// under stale-if-error.
	flagCredentialed = 1 << 0
	// flagComposition: the entry holds a composition Route's response and
	// names no Upstream; its replay runs the whole composition.
	flagComposition = 1 << 1
	// flagAuthorized: the storing request carried Authorization
	// (revalidate.Storable).
	flagAuthorized = 1 << 2
)

// Decode errors: either makes the lookup a miss.
var (
	// errFormat is an entry whose format byte this release does not know.
	errFormat = errors.New("cache: unknown entry format")
	// errCorrupt is an entry that ends early or holds an impossible value.
	errCorrupt = errors.New("cache: corrupt entry")
)

// entry is one stored variant: format 1 of "Response Cache layout". A
// decoded entry's body aliases the bytes it was decoded from, which must
// not change while it is in use; its strings and header values are copies.
type entry struct {
	// token is the generation token the entry was written under.
	token statestore.Token
	flags uint64
	// status, initialAge and header are the stored response (initialAge
	// per RFC 9111 section 4.2.3, at millisecond precision).
	status     int
	initialAge time.Duration
	// route and upstream name the Route the entry was stored on and the
	// Upstream whose response it holds; no Upstream for a composition.
	route, upstream string
	// method, target and reqHeader are the saved request: target in
	// absolute form, reqHeader without credentials.
	method, target string
	reqHeader      http.Header
	header         http.Header
	body           []byte
}

// credentialed reports flagCredentialed.
func (e *entry) credentialed() bool { return e.flags&flagCredentialed != 0 }

// authorized reports flagAuthorized.
func (e *entry) authorized() bool { return e.flags&flagAuthorized != 0 }

// appendPrefix appends the encoding of e without its body: a format byte,
// the 16 token bytes, then as unsigned varints or length-prefixed byte
// strings the flags, the status, the initial age in milliseconds, the
// Route, the Upstream, the saved request's method, target and header
// block, and the response header block. The body follows the prefix to the
// end of the entry, so len(prefix) is what the body leaves of MaxEntry.
func appendPrefix(dst []byte, e *entry) []byte {
	dst = append(dst, formatV1)
	dst = append(dst, e.token[:]...)
	dst = binary.AppendUvarint(dst, e.flags)
	dst = binary.AppendUvarint(dst, uint64(max(e.status, 0)))
	dst = binary.AppendUvarint(dst, uint64(max(e.initialAge.Milliseconds(), 0)))
	dst = appendString(dst, e.route)
	dst = appendString(dst, e.upstream)
	dst = appendString(dst, e.method)
	dst = appendString(dst, e.target)
	dst = appendBlock(dst, e.reqHeader)
	return appendBlock(dst, e.header)
}

// encodeEntry returns the whole encoding of e.
func encodeEntry(e *entry) []byte {
	return append(appendPrefix(nil, e), e.body...)
}

// appendString appends a length-prefixed byte string.
func appendString(dst []byte, s string) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(s)))
	return append(dst, s...)
}

// appendBlock appends a header block: the field line count, then each
// line's name and value, names in byte order and each name's lines in the
// order h holds them (net/http keeps no order across names).
func appendBlock(dst []byte, h http.Header) []byte {
	names := make([]string, 0, len(h))
	n := 0
	for k, vs := range h {
		if len(vs) > 0 {
			names = append(names, k)
			n += len(vs)
		}
	}
	slices.Sort(names)
	dst = binary.AppendUvarint(dst, uint64(n))
	for _, k := range names {
		for _, v := range h[k] {
			dst = appendString(dst, k)
			dst = appendString(dst, v)
		}
	}
	return dst
}

// decodeEntry decodes b into e, whose body then aliases b; on failure e is
// left zero.
func decodeEntry(e *entry, b []byte) error {
	*e = entry{}
	if len(b) == 0 {
		return errCorrupt
	}
	if b[0] != formatV1 {
		return errFormat
	}
	d := decoder{b: b[1:]}
	if len(d.b) < len(e.token) {
		return errCorrupt
	}
	var out entry
	copy(out.token[:], d.b)
	d.b = d.b[len(out.token):]
	out.flags = d.uvarint()
	status := d.uvarint()
	age := d.uvarint()
	out.route = d.str()
	out.upstream = d.str()
	out.method = d.str()
	out.target = d.str()
	out.reqHeader = d.block()
	out.header = d.block()
	if d.err || status > 999 || age > uint64(maxAge/time.Millisecond) {
		return errCorrupt
	}
	out.status = int(status)
	out.initialAge = time.Duration(age) * time.Millisecond
	out.body = d.b
	*e = out
	return nil
}

// maxAge bounds a decoded initial age far above any real one (an entry
// lives at most 25 h), so a larger value is corruption and no conversion
// to time.Duration overflows.
const maxAge = (1 << 40) * time.Millisecond

// decoder reads the fields of an entry; err sticks after the first short
// or impossible read.
type decoder struct {
	b   []byte
	err bool
}

// uvarint reads an unsigned varint.
func (d *decoder) uvarint() uint64 {
	if d.err {
		return 0
	}
	v, n := binary.Uvarint(d.b)
	if n <= 0 {
		d.err = true
		return 0
	}
	d.b = d.b[n:]
	return v
}

// str reads a length-prefixed byte string.
func (d *decoder) str() string {
	n := d.uvarint()
	if d.err || n > uint64(len(d.b)) {
		d.err = true
		return ""
	}
	s := string(d.b[:n])
	d.b = d.b[n:]
	return s
}

// block reads a header block.
func (d *decoder) block() http.Header {
	n := d.uvarint()
	// Each line takes at least two length bytes.
	if d.err || n > uint64(len(d.b))/2 {
		d.err = true
		return nil
	}
	h := make(http.Header, min(n, 64))
	for range n {
		k := d.str()
		v := d.str()
		if d.err {
			return nil
		}
		h[k] = append(h[k], v)
	}
	return h
}
