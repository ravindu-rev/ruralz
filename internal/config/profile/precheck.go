// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"fmt"
	"strconv"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// checkSize reports a file over the size limit at the first byte past it.
func (p *fileParser) checkSize(src []byte) bool {
	if len(src) <= p.opts.MaxBytes {
		return true
	}
	at := jsonval.PositionOf(src, p.opts.MaxBytes)
	p.add(p.errorAt(codeParse, p.pos(at.Line, at.Column),
		"file is "+strconv.Itoa(len(src))+" bytes, over the "+strconv.Itoa(p.opts.MaxBytes)+"-byte limit"))
	return false
}

// yamlPrintable reports whether r is a YAML printable character (YAML 1.2.2
// production c-printable): TAB, LF, CR, U+0020 to U+007E, U+0085, U+00A0
// to U+D7FF, U+E000 to U+FFFD and U+10000 to U+10FFFF.
func yamlPrintable(r rune) bool {
	switch {
	case r == '\t', r == '\n', r == '\r':
		return true
	case r >= 0x20 && r <= 0x7E:
		return true
	case r == 0x85:
		return true
	case r >= 0xA0 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	default:
		return r >= 0x10000 && r <= 0x10FFFF
	}
}

// precheckYAML applies the byte pre-checks of 01 req 7 before tokenizing:
// valid UTF-8, no U+FEFF anywhere, only YAML printable characters. Each
// failure is RZ-CFG-001 at the offending character and stops the file. On
// success it returns the text with every CR LF and lone CR replaced by LF:
// YAML normalizes line breaks inside scalars to LF and ignores their form
// elsewhere, so CRLF and CR files parse exactly like LF files, and every
// position (lines, code-point columns) is unchanged.
func (p *fileParser) precheckYAML(src []byte) (string, bool) {
	line, col := 1, 1
	crs := 0
	for i := 0; i < len(src); {
		c := src[i]
		if c < utf8.RuneSelf {
			switch {
			case c == '\n':
				line, col = line+1, 1
			case c == '\r':
				crs++
				if i+1 < len(src) && src[i+1] == '\n' {
					i++
				}
				line, col = line+1, 1
			case !yamlPrintable(rune(c)):
				p.add(p.errorAt(codeParse, p.pos(line, col), fmt.Sprintf("character %U is not allowed in YAML", c)))
				return "", false
			default:
				col++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(src[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			p.add(p.errorAt(codeParse, p.pos(line, col), "file is not valid UTF-8"))
			return "", false
		case r == 0xFEFF:
			p.add(p.errorAt(codeParse, p.pos(line, col), "byte order mark (U+FEFF) is not allowed"))
			return "", false
		case !yamlPrintable(r):
			p.add(p.errorAt(codeParse, p.pos(line, col), fmt.Sprintf("character %U is not allowed in YAML", r)))
			return "", false
		}
		col++
		i += size
	}
	if crs == 0 {
		return string(src), true
	}
	out := make([]byte, 0, len(src))
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\r' {
			if i+1 < len(src) && src[i+1] == '\n' {
				continue
			}
			c = '\n'
		}
		out = append(out, c)
	}
	return string(out), true
}
