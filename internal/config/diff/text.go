// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diff

import (
	"cmp"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/canonical"
	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// Column widths of the resource lines of the human form: the identity
// column is at least identityWidth characters wide and the source column
// at least sourceWidth, each widened to its longest cell plus two spaces.
const (
	identityWidth = 48
	sourceWidth   = 29
)

// capabilityNote follows every flagged line of the human form.
const capabilityNote = "  (Capability grant)"

// WriteText writes the human form to w.
func (d *Diff) WriteText(w io.Writer) error {
	_, err := w.Write(d.AppendText(nil))
	return err
}

// AppendText appends the human form: a header naming both Revisions, one
// line per changed resource with its field operations indented below it,
// the effective Filter Chain changes, and a summary line. Every authored
// text it writes is printable: a rune that is not, such as a newline or a
// bidirectional override, is escaped, so no value can forge or hide a
// line.
func (d *Diff) AppendText(dst []byte) []byte {
	dst = append(dst, "ruralz bundle diff: "...)
	dst = append(dst, short(d.From.Digest)...)
	dst = append(dst, " -> "...)
	dst = append(dst, short(d.To.Digest)...)
	switch {
	case d.Environment != "":
		dst = append(dst, " (environment: "...)
		dst = appendPrintable(dst, d.Environment)
		dst = append(dst, ')')
	case d.From.Environment != "" || d.To.Environment != "":
		dst = append(dst, " (environment: "...)
		dst = appendPrintable(dst, cmp.Or(d.From.Environment, "-"))
		dst = append(dst, " -> "...)
		dst = appendPrintable(dst, cmp.Or(d.To.Environment, "-"))
		dst = append(dst, ')')
	}
	dst = append(dst, "\n\n"...)
	if len(d.Resources) > 0 {
		dst = d.appendResources(dst)
		dst = append(dst, '\n')
	}
	if len(d.Effective) > 0 {
		dst = append(dst, "Effective Filter Chain changes:\n"...)
		for i := range d.Effective {
			dst = appendChainChange(dst, &d.Effective[i])
		}
		dst = append(dst, '\n')
	}
	return appendSummary(dst, &d.Summary)
}

// short returns the display form of a digest.
func short(d revision.Digest) string {
	if d.IsZero() {
		return "-"
	}
	return d.Short()
}

// appendResources appends one line per resource, columns aligned, each
// followed by its operations.
func (d *Diff) appendResources(dst []byte) []byte {
	idents := make([]string, len(d.Resources))
	sources := make([]string, len(d.Resources))
	idW, srcW := identityWidth, 0
	for i := range d.Resources {
		r := &d.Resources[i]
		idents[i] = string(appendPrintable([]byte{r.Change.symbol(), ' '}, r.ID.String()))
		sources[i] = string(appendPrintable(nil, r.Source))
		idW = max(idW, utf8.RuneCountInString(idents[i])+2)
		if sources[i] != "" {
			srcW = max(srcW, sourceWidth, utf8.RuneCountInString(sources[i])+2)
		}
	}
	for i := range d.Resources {
		r := &d.Resources[i]
		line := pad(nil, idents[i], idW)
		if srcW > 0 {
			line = pad(line, sources[i], srcW)
		}
		if r.Impact != 0 {
			line = append(line, '[')
			line = append(line, r.Impact.String()...)
			line = append(line, ']')
		}
		line = []byte(strings.TrimRight(string(line), " "))
		if len(r.Ops) == 0 && r.Flags&FlagCapabilityGrant != 0 {
			line = append(line, capabilityNote...)
		}
		dst = append(dst, line...)
		dst = append(dst, '\n')
		for j := range r.Ops {
			dst = appendOp(dst, &r.Ops[j])
		}
	}
	return dst
}

// pad appends s and spaces up to width runes, at least one space.
func pad(dst []byte, s string, width int) []byte {
	dst = append(dst, s...)
	n := max(1, width-utf8.RuneCountInString(s))
	return append(dst, strings.Repeat(" ", n)...)
}

// appendOp appends one indented operation line.
func appendOp(dst []byte, op *Op) []byte {
	dst = append(dst, "    "...)
	dst = append(dst, op.Kind.symbol(), ' ')
	dst = appendPath(dst, op.Path)
	switch op.Kind {
	case OpReplace:
		dst = append(dst, ": "...)
		dst = appendValue(dst, op.From)
		dst = append(dst, " -> "...)
		dst = appendValue(dst, op.To)
	case OpAdd:
		if !implied(op.Path, op.To) {
			dst = append(dst, ": "...)
			dst = appendValue(dst, op.To)
		}
	case OpRemove:
		if !implied(op.Path, op.From) {
			dst = append(dst, ": "...)
			dst = appendValue(dst, op.From)
		}
	case OpMove:
		dst = append(dst, ": position "...)
		dst = strconv.AppendInt(dst, int64(op.FromIndex)+1, 10)
		dst = append(dst, " -> "...)
		dst = strconv.AppendInt(dst, int64(op.ToIndex)+1, 10)
	}
	if op.Flags&FlagCapabilityGrant != 0 {
		dst = append(dst, capabilityNote...)
	}
	return append(dst, '\n')
}

// implied reports a value its path already shows: a set element, or a
// keyed entry that holds nothing but its key.
func implied(p diag.Path, v *tree.Node) bool {
	if len(p) == 0 {
		return false
	}
	last := p[len(p)-1]
	switch last.Kind {
	case diag.ElemItem:
		return true
	case diag.ElemKeyed:
		return v != nil && v.Kind == tree.KindMap && len(v.Members) == 1 && v.Members[0].Key == last.KeyField
	default:
		return false
	}
}

// appendPath appends the key-aware human form of p (diag.Path.String),
// with its non-printable runes escaped.
func appendPath(dst []byte, p diag.Path) []byte {
	return appendPrintable(dst, p.String())
}

// appendValue appends the human form of a value: a string bare when it
// is made of [A-Za-z0-9_./:@*-] and does not read as a number, true,
// false or null, else quoted; numbers in RFC 8785 form; objects and lists
// as RFC 8785 JSON. Non-printable runes are escaped.
func appendValue(dst []byte, n *tree.Node) []byte {
	if n == nil {
		return append(dst, '-')
	}
	switch n.Kind {
	case tree.KindString:
		if bare(n.Text) {
			return append(dst, n.Text...)
		}
		return appendQuoted(dst, n.Text)
	case tree.KindInt, tree.KindFloat:
		return append(dst, canonicalNumber(n.Text)...)
	case tree.KindBool:
		return strconv.AppendBool(dst, n.Bool)
	default:
		b, err := canonical.AppendNode(nil, n)
		if err != nil {
			// Compare checked every value it shows.
			return append(dst, "null"...)
		}
		return appendPrintable(dst, string(b))
	}
}

// bare reports a string the human form writes without quotes.
func bare(s string) bool {
	if s == "" || s == "true" || s == "false" || s == "null" || jsonval.ValidNumber(s) {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		letter := c|0x20 >= 'a' && c|0x20 <= 'z'
		digit := c >= '0' && c <= '9'
		if !letter && !digit && !strings.ContainsRune("_./:@*-", rune(c)) {
			return false
		}
	}
	return true
}

// appendQuoted appends s as a JSON string with non-printable runes
// escaped.
func appendQuoted(dst []byte, s string) []byte {
	q, err := jsonval.AppendString(nil, s)
	if err != nil {
		q, _ = jsonval.AppendString(nil, strings.ToValidUTF8(s, "�"))
	}
	return appendPrintable(dst, string(q))
}

// appendPrintable appends s with every rune strconv.IsPrint rejects (C0
// and C1 controls, format characters such as U+202E, line and paragraph
// separators) written as \uXXXX or \UXXXXXXXX, and invalid UTF-8 bytes as
// \xXX, so one authored text cannot start a new line or reorder the
// terminal's display of a diff under review.
func appendPrintable(dst []byte, s string) []byte {
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			dst = append(dst, `\x`...)
			dst = appendHex(dst, int(s[i]), 2)
		case r == ' ' || strconv.IsPrint(r):
			dst = append(dst, s[i:i+size]...)
		case r <= 0xFFFF:
			dst = append(dst, `\u`...)
			dst = appendHex(dst, int(r), 4)
		default:
			dst = append(dst, `\U`...)
			dst = appendHex(dst, int(r), 8)
		}
		i += size
	}
	return dst
}

// appendHex appends v, a byte or a rune, as n lowercase hex digits.
func appendHex(dst []byte, v, n int) []byte {
	const digits = "0123456789abcdef"
	for k := n - 1; k >= 0; k-- {
		dst = append(dst, digits[(v>>(4*k))&0xf])
	}
	return dst
}

// appendChainChange appends one effective change line.
func appendChainChange(dst []byte, c *ChainChange) []byte {
	dst = append(dst, "  Route/"...)
	dst = appendPrintable(dst, c.Route)
	dst = append(dst, ' ')
	dst = append(dst, c.Phase.String()...)
	if c.Leg != "" {
		dst = append(dst, " leg "...)
		dst = appendPrintable(dst, c.Leg)
	}
	dst = append(dst, ": "...)
	dst = append(dst, c.Op.symbol(), ' ')
	dst = appendPrintable(dst, c.Policy)
	dst = append(dst, " ("...)
	dst = append(dst, c.Class.String()...)
	if c.After != "" {
		dst = append(dst, ", after "...)
		dst = appendPrintable(dst, c.After)
	} else {
		dst = append(dst, ", first"...)
	}
	return append(dst, ")\n"...)
}

// appendSummary appends the summary line.
func appendSummary(dst []byte, s *Summary) []byte {
	dst = strconv.AppendInt(dst, int64(s.Changed), 10)
	dst = append(dst, plural(s.Changed, " resource changed: ", " resources changed: ")...)
	dst = strconv.AppendInt(dst, int64(s.Added), 10)
	dst = append(dst, " added, "...)
	dst = strconv.AppendInt(dst, int64(s.Removed), 10)
	dst = append(dst, " removed, "...)
	dst = strconv.AppendInt(dst, int64(s.Modified), 10)
	dst = append(dst, " modified; "...)
	dst = strconv.AppendInt(dst, int64(s.RouteChainsChanged), 10)
	dst = append(dst, plural(s.RouteChainsChanged, " Route chain changed; impact: ", " Route chains changed; impact: ")...)
	if s.Impact == 0 {
		dst = append(dst, "none"...)
	} else {
		dst = append(dst, s.Impact.String()...)
	}
	return append(dst, '\n')
}

// plural returns one when n is 1, else many.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
