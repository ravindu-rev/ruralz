// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package canary generates secret canaries and scans captured bytes for them
// in every form a leak could take (11 req 39): raw, standard and URL-safe
// base64 at any byte alignment, hexadecimal in any letter case,
// percent-encoding and JSON string escaping (each byte escaped or literal).
//
// A canary is injected where a real secret would be (secretRef files and
// environment variables, API keys, State Store passwords); the end-to-end
// harness then scans logs, admin responses, telemetry and error bodies with
// Scan and fails the run on any Finding outside the designated destinations.
package canary

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"unicode/utf16"
	"unicode/utf8"
)

// Prefix starts every canary value New generates.
const Prefix = "rzcanary-"

// Encoding names reported in Finding.Encoding.
const (
	// EncodingRaw is the canary value byte for byte.
	EncodingRaw = "raw"
	// EncodingBase64 is standard base64 (with or without padding), at any
	// alignment of the canary inside the encoded bytes. For canary values
	// made of letters, digits and '-' the URL-safe alphabet produces the
	// same characters, so this name covers both alphabets.
	EncodingBase64 = "base64"
	// EncodingBase64URL is URL-safe base64, reported only when its
	// characters differ from the standard alphabet's.
	EncodingBase64URL = "base64url"
	// EncodingHex is hexadecimal in upper, lower or mixed case.
	EncodingHex = "hex"
	// EncodingURL is percent-encoding of at least one byte of the value.
	EncodingURL = "url"
	// EncodingJSON is JSON string escaping (\uXXXX or a short escape) of at
	// least one byte of the value.
	EncodingJSON = "json"
)

// Canary is one secret canary value.
type Canary struct {
	// Value is the canary, "rzcanary-" followed by 32 lowercase hex digits
	// when made by New.
	Value string
}

// New returns a canary made of Prefix and 32 hex digits from crypto/rand.
func New() (Canary, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return Canary{}, fmt.Errorf("canary: read random bytes: %w", err)
	}
	return Canary{Value: Prefix + hex.EncodeToString(b[:])}, nil
}

// Finding is one occurrence of the canary.
type Finding struct {
	// Source names where the bytes came from (a log file, an endpoint).
	Source string
	// Encoding is one of the Encoding* names.
	Encoding string
	// Offset is the byte offset in the scanned data where the matched
	// (encoded) form starts. For base64 at a shifted alignment it is the
	// first character determined only by canary bytes.
	Offset int
}

// String formats the finding without the canary value.
func (f Finding) String() string {
	return fmt.Sprintf("%s: canary (%s) at byte %d", f.Source, f.Encoding, f.Offset)
}

// Scan returns every occurrence of the canary in data, ordered by offset
// and then encoding. An empty canary value finds nothing.
func (c Canary) Scan(source string, data []byte) []Finding {
	v := []byte(c.Value)
	if len(v) == 0 {
		return nil
	}
	var out []Finding
	add := func(enc string, off int) {
		out = append(out, Finding{Source: source, Encoding: enc, Offset: off})
	}
	for _, off := range indexAll(data, v) {
		add(EncodingRaw, off)
	}
	std := base64Patterns(v, base64.RawStdEncoding)
	url := base64Patterns(v, base64.RawURLEncoding)
	for i, p := range std {
		for _, off := range indexAll(data, p) {
			add(EncodingBase64, off)
		}
		if bytes.Equal(p, url[i]) {
			continue
		}
		for _, off := range indexAll(data, url[i]) {
			add(EncodingBase64URL, off)
		}
	}
	for _, off := range indexAll(asciiLower(data), []byte(hex.EncodeToString(v))) {
		add(EncodingHex, off)
	}
	for _, m := range scanEscaped(data, v, '%', decodePercent) {
		add(EncodingURL, m)
	}
	for _, m := range scanEscaped(data, v, '\\', decodeJSON) {
		add(EncodingJSON, m)
	}
	slices.SortFunc(out, func(a, b Finding) int {
		if a.Offset != b.Offset {
			return a.Offset - b.Offset
		}
		switch {
		case a.Encoding < b.Encoding:
			return -1
		case a.Encoding > b.Encoding:
			return 1
		}
		return 0
	})
	return out
}

// ScanFile scans one file; the Source of each finding is path.
func (c Canary) ScanFile(path string) ([]Finding, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the harness scans files it wrote itself
	if err != nil {
		return nil, fmt.Errorf("canary: %w", err)
	}
	return c.Scan(path, data), nil
}

// ScanDir scans every regular file under root (symbolic links are not
// followed); the Source of each finding is the slash-separated path
// relative to root.
func (c Canary) ScanDir(root string) ([]Finding, error) {
	var out []Finding
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p) //nolint:gosec // G304: walking a directory the caller names
		if err != nil {
			return err
		}
		out = append(out, c.Scan(filepath.ToSlash(rel), data)...)
		return nil
	})
	if err != nil {
		return out, fmt.Errorf("canary: scan %s: %w", root, err)
	}
	return out, nil
}

// indexAll returns the offsets of every (possibly overlapping) occurrence
// of pat in data.
func indexAll(data, pat []byte) []int {
	var out []int
	for start := 0; start+len(pat) <= len(data); {
		i := bytes.Index(data[start:], pat)
		if i < 0 {
			break
		}
		out = append(out, start+i)
		start += i + 1
	}
	return out
}

// base64Patterns returns, for each of the three byte alignments of v in the
// encoded stream, the characters determined only by v's bytes: the first
// characters mixed with preceding bytes and the last character mixed with
// following bytes are dropped, so a pattern matches whatever surrounds v.
func base64Patterns(v []byte, enc *base64.Encoding) [3][]byte {
	var out [3][]byte
	skip := [3]int{0, 2, 3}
	for k := range 3 {
		buf := make([]byte, k+len(v))
		copy(buf[k:], v)
		s := enc.EncodeToString(buf)
		end := len(s)
		if len(buf)%3 != 0 {
			end--
		}
		out[k] = []byte(s[skip[k]:end])
	}
	return out
}

// asciiLower returns data with ASCII upper-case letters folded, copying
// only when there is one to fold. Lengths and offsets are unchanged.
func asciiLower(data []byte) []byte {
	i := bytes.IndexFunc(data, func(r rune) bool { return 'A' <= r && r <= 'Z' })
	if i < 0 {
		return data
	}
	out := slices.Clone(data)
	for j := i; j < len(out); j++ {
		if b := out[j]; 'A' <= b && b <= 'Z' {
			out[j] = b + 'a' - 'A'
		}
	}
	return out
}

// maxEscape is the longest source form of one decoded byte (\uXXXX) and
// maxUnit the longest escape unit (a \uXXXX\uXXXX surrogate pair).
const (
	maxEscape = 6
	maxUnit   = 12
)

// scanEscaped returns the source offsets of the decoded matches of pat that
// contain at least one escape (a match without one is a raw finding). A
// matched span holds at most maxEscape source bytes per decoded byte, so
// only windows of that radius around each escape byte are decoded; each
// window also starts maxUnit bytes early so decoding is back on unit
// boundaries before any match can start.
func scanEscaped(data, pat []byte, esc byte, dec decoder) []int {
	radius := maxEscape*len(pat) + maxUnit
	var out []int
	for from := 0; ; {
		e := bytes.IndexByte(data[from:], esc)
		if e < 0 {
			break
		}
		lo := max(0, from+e-radius)
		hi := min(len(data), from+e+radius)
		// Merge the windows of the following escape bytes.
		for {
			n := bytes.IndexByte(data[min(hi, from+e+1):hi], esc)
			if n < 0 {
				break
			}
			next := min(hi, from+e+1) + n
			e = next - from
			hi = min(len(data), next+radius)
		}
		for _, m := range scanDecoded(data[lo:hi], pat, esc, dec) {
			if m.escaped && (len(out) == 0 || out[len(out)-1] < lo+m.start) {
				out = append(out, lo+m.start)
			}
		}
		if hi == len(data) {
			break
		}
		from = hi
	}
	return out
}

// decoder decodes one unit at data[i:]: it returns the decoded bytes and
// the number of source bytes consumed (at least 1).
type decoder func(data []byte, i int, out *[utf8.UTFMax]byte) (n, consumed int)

// decodedMatch is one match of a pattern in the decoded stream.
type decodedMatch struct {
	start   int  // source offset of the first matched unit
	escaped bool // the matched source span holds at least one escape
}

// scanDecoded runs a Knuth-Morris-Pratt search for pat over the stream dec
// produces from data, remembering the source offset of the last len(pat)
// decoded bytes in a ring so memory stays O(len(pat)). Bytes other than
// esc are literal and bypass dec; while no prefix of pat is matched, the
// scan skips to the next byte that can start a match or an escape.
func scanDecoded(data, pat []byte, esc byte, dec decoder) []decodedMatch {
	fail := failure(pat)
	ring := make([]int, len(pat))
	var out []decodedMatch
	var buf [utf8.UTFMax]byte
	state, d := 0, 0
	for i := 0; i < len(data); {
		if state == 0 {
			j := indexEither(data[i:], pat[0], esc)
			if j < 0 {
				break
			}
			i += j
		}
		n, consumed := 1, 1
		if c := data[i]; c == esc {
			n, consumed = dec(data, i, &buf)
		} else {
			buf[0] = c
		}
		for _, b := range buf[:n] {
			ring[d%len(pat)] = i
			for state > 0 && pat[state] != b {
				state = fail[state-1]
			}
			if pat[state] == b {
				state++
			}
			if state == len(pat) {
				start := ring[(d-len(pat)+1)%len(pat)]
				out = append(out, decodedMatch{start: start, escaped: i+consumed-start > len(pat)})
				state = fail[state-1]
			}
			d++
		}
		i += consumed
	}
	return out
}

// indexEither returns the index of the first a or b in data, or -1.
func indexEither(data []byte, a, b byte) int {
	i := bytes.IndexByte(data, a)
	if i < 0 {
		return bytes.IndexByte(data, b)
	}
	if j := bytes.IndexByte(data[:i], b); j >= 0 {
		return j
	}
	return i
}

// failure computes the Knuth-Morris-Pratt failure function of pat.
func failure(pat []byte) []int {
	f := make([]int, len(pat))
	k := 0
	for i := 1; i < len(pat); i++ {
		for k > 0 && pat[i] != pat[k] {
			k = f[k-1]
		}
		if pat[i] == pat[k] {
			k++
		}
		f[i] = k
	}
	return f
}

// decodePercent decodes one %XX escape, or passes one byte through.
func decodePercent(data []byte, i int, out *[utf8.UTFMax]byte) (n, consumed int) {
	if data[i] == '%' && i+2 < len(data) {
		hi, ok1 := unhex(data[i+1])
		lo, ok2 := unhex(data[i+2])
		if ok1 && ok2 {
			out[0] = hi<<4 | lo
			return 1, 3
		}
	}
	out[0] = data[i]
	return 1, 1
}

// decodeJSON decodes one JSON string escape (short forms, \uXXXX and
// surrogate pairs), or passes one byte through.
func decodeJSON(data []byte, i int, out *[utf8.UTFMax]byte) (n, consumed int) {
	if data[i] != '\\' || i+1 >= len(data) {
		out[0] = data[i]
		return 1, 1
	}
	switch c := data[i+1]; c {
	case '"', '\\', '/':
		out[0] = c
		return 1, 2
	case 'b':
		out[0] = '\b'
		return 1, 2
	case 'f':
		out[0] = '\f'
		return 1, 2
	case 'n':
		out[0] = '\n'
		return 1, 2
	case 'r':
		out[0] = '\r'
		return 1, 2
	case 't':
		out[0] = '\t'
		return 1, 2
	case 'u':
		r, ok := hex4(data, i+2)
		if !ok {
			break
		}
		consumed = 6
		if utf16.IsSurrogate(r) {
			if r2, ok := hex4(data, i+8); ok && i+7 < len(data) && data[i+6] == '\\' && data[i+7] == 'u' {
				if pair := utf16.DecodeRune(r, r2); pair != utf8.RuneError {
					r, consumed = pair, 12
				}
			}
		}
		return utf8.EncodeRune(out[:], r), consumed
	}
	out[0] = data[i]
	return 1, 1
}

// hex4 parses four hex digits at data[i:].
func hex4(data []byte, i int) (rune, bool) {
	if i+4 > len(data) {
		return 0, false
	}
	var r rune
	for _, b := range data[i : i+4] {
		v, ok := unhex(b)
		if !ok {
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	return r, true
}

func unhex(b byte) (byte, bool) {
	switch {
	case '0' <= b && b <= '9':
		return b - '0', true
	case 'a' <= b && b <= 'f':
		return b - 'a' + 10, true
	case 'A' <= b && b <= 'F':
		return b - 'A' + 10, true
	}
	return 0, false
}
