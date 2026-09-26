// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

// Package diag is the diagnostics model shared by ruralz, ruralz-control
// (M2) and ruralzd: one Diagnostic type, key-aware paths, the text and JSON
// forms of docs/architecture/02-configuration-model.md "Diagnostics and
// source map", deterministic sorting and nearest-match hints. It imports
// only the standard library, so every configuration stage and both
// binaries emit byte-identical output for the same input.
package diag

import (
	"cmp"
	"encoding/json"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Severity is error or warning. Warnings (RZ-CFG-013, RZ-CFG-025) never
// change a command's exit code.
type Severity uint8

// Severities.
const (
	// SeverityError blocks a Revision.
	SeverityError Severity = iota + 1
	// SeverityWarning is reported only.
	SeverityWarning
)

// String returns "error" or "warning".
func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// MarshalText encodes the severity name.
func (s Severity) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// ResourceID names the resource a diagnostic is about.
type ResourceID struct {
	// Kind is the resource kind, such as Route.
	Kind string `json:"kind"`
	// Name is metadata.name.
	Name string `json:"name"`
}

// Location is a source position. Line and Column are 1-based; Column
// counts Unicode code points; 0 means unknown.
type Location struct {
	// File is the slash path relative to the Bundle root, or as given.
	File string
	// Line is the 1-based line; 0 when unknown.
	Line int
	// Column is the 1-based column in code points; 0 when unknown.
	Column int
}

// ElemKind selects the form of a path element.
type ElemKind uint8

// Path element kinds.
const (
	// ElemField is an object member.
	ElemField ElemKind = iota
	// ElemIndex is a position in an atomic list.
	ElemIndex
	// ElemKeyed is an entry of a map or orderedMap list.
	ElemKeyed
	// ElemItem is an element of a set list.
	ElemItem
)

// PathElem is one step of a key-aware path.
type PathElem struct {
	// Kind selects which other fields are set.
	Kind ElemKind
	// Name is the member name (ElemField).
	Name string
	// Index is the list position (ElemIndex).
	Index int
	// KeyField is the key member, such as "name" (ElemKeyed).
	KeyField string
	// Key is the key value (ElemKeyed).
	Key string
	// Item is the element value (ElemItem): a string, json.Number, bool or
	// a canonical JSON value (json.RawMessage) for object elements.
	Item any
}

// Field returns an object member element.
func Field(name string) PathElem { return PathElem{Kind: ElemField, Name: name} }

// Index returns an atomic list element.
func Index(i int) PathElem { return PathElem{Kind: ElemIndex, Index: i} }

// Keyed returns a map or orderedMap entry element.
func Keyed(keyField, key string) PathElem {
	return PathElem{Kind: ElemKeyed, KeyField: keyField, Key: key}
}

// Item returns a set element.
func Item(v any) PathElem { return PathElem{Kind: ElemItem, Item: v} }

// Path is a key-aware path from a resource root, such as
// spec.composition.steps[name=stock].upstream.
type Path []PathElem

// Append returns a new path with elems appended; p is not modified.
func (p Path) Append(elems ...PathElem) Path {
	out := make(Path, 0, len(p)+len(elems))
	return append(append(out, p...), elems...)
}

// isFieldName reports whether s matches [A-Za-z_$][A-Za-z0-9_$-]*.
func isFieldName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == '$' || (c|0x20 >= 'a' && c|0x20 <= 'z') || (i > 0 && (c == '-' || (c >= '0' && c <= '9')))
		if !ok {
			return false
		}
	}
	return true
}

// isBare reports whether s matches [A-Za-z0-9_./:@*-]+.
func isBare(s string) bool {
	if s == "" {
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

// String returns the human form: fields joined by ".", a field not
// matching [A-Za-z_$][A-Za-z0-9_$-]* written ["<json>"], keyed entries
// [<keyField>=<key>], set elements [item=<value>], atomic entries [<n>];
// a key or item not matching [A-Za-z0-9_./:@*-]+ is JSON-quoted and object
// items print as canonical JSON.
func (p Path) String() string {
	var b strings.Builder
	for i, e := range p {
		switch e.Kind {
		case ElemField:
			if isFieldName(e.Name) {
				if i > 0 {
					b.WriteByte('.')
				}
				b.WriteString(e.Name)
			} else {
				b.WriteString(`[`)
				b.WriteString(strconv.Quote(e.Name))
				b.WriteString(`]`)
			}
		case ElemIndex:
			b.WriteByte('[')
			b.WriteString(strconv.Itoa(e.Index))
			b.WriteByte(']')
		case ElemKeyed:
			b.WriteByte('[')
			b.WriteString(e.KeyField)
			b.WriteByte('=')
			b.WriteString(quoteValue(e.Key))
			b.WriteByte(']')
		case ElemItem:
			b.WriteString("[item=")
			b.WriteString(itemText(e.Item))
			b.WriteByte(']')
		}
	}
	return b.String()
}

func quoteValue(s string) string {
	if isBare(s) {
		return s
	}
	q, _ := json.Marshal(s)
	return string(q)
}

func itemText(v any) string {
	switch t := v.(type) {
	case string:
		return quoteValue(t)
	case json.RawMessage:
		return string(t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return "?"
		}
		return string(b)
	}
}

// MarshalJSON returns the JSON form: an array of strings (fields), integers
// (atomic indexes) and single-member objects ({"name":"stock"},
// {"item":"GET"}).
func (p Path) MarshalJSON() ([]byte, error) {
	out := make([]any, 0, len(p))
	for _, e := range p {
		switch e.Kind {
		case ElemField:
			out = append(out, e.Name)
		case ElemIndex:
			out = append(out, e.Index)
		case ElemKeyed:
			out = append(out, map[string]string{e.KeyField: e.Key})
		case ElemItem:
			out = append(out, map[string]any{"item": e.Item})
		}
	}
	return json.Marshal(out)
}

// Related is a secondary location, such as the first of two duplicates.
type Related struct {
	Location
	// Message describes the location, such as "declared in".
	Message string
}

// Diagnostic is one configuration finding.
type Diagnostic struct {
	// Code is a registered RZ-CFG-NNN code.
	Code string
	// Severity is error or warning.
	Severity Severity
	Location
	// Resource is the resource the finding is about, when known.
	Resource *ResourceID
	// Path is the key-aware path inside the resource.
	Path Path
	// Message is one line; it never contains a secret value.
	Message string
	// Hint is optional, such as a nearest match.
	Hint string
	// Related lists secondary locations.
	Related []Related
	// Environment names the Environment of a multi-Environment run.
	Environment string
}

// List is an ordered set of diagnostics.
type List []Diagnostic

// HasErrors reports whether any diagnostic has error severity.
func (l List) HasErrors() bool {
	return slices.ContainsFunc(l, func(d Diagnostic) bool { return d.Severity == SeverityError })
}

// Sort orders by (environment, file, line, column, code, path, message).
func (l List) Sort() {
	slices.SortStableFunc(l, func(a, b Diagnostic) int {
		return cmp.Or(
			cmp.Compare(a.Environment, b.Environment),
			cmp.Compare(a.File, b.File),
			cmp.Compare(a.Line, b.Line),
			cmp.Compare(a.Column, b.Column),
			cmp.Compare(a.Code, b.Code),
			cmp.Compare(a.Path.String(), b.Path.String()),
			cmp.Compare(a.Message, b.Message),
		)
	})
}

// FirstErrorCode returns the code of the first error after Sort, or "".
func (l List) FirstErrorCode() string {
	for _, d := range l {
		if d.Severity == SeverityError {
			return d.Code
		}
	}
	return ""
}

// AppendText appends the one-line text form:
// <file>:<line>:<column> <severity> <code> <Kind>/<name> <path>: <message>
// then " (<hint>)", " (<related message> <file>:<line>:<column>)" per
// related location and " [environment=<name>]". "-" stands for no file.
func (d Diagnostic) AppendText(dst []byte) []byte {
	dst = appendLocation(dst, d.Location)
	dst = append(dst, ' ')
	dst = append(dst, d.Severity.String()...)
	dst = append(dst, ' ')
	dst = append(dst, d.Code...)
	if d.Resource != nil {
		dst = append(dst, ' ')
		dst = append(dst, d.Resource.Kind...)
		dst = append(dst, '/')
		dst = append(dst, d.Resource.Name...)
	}
	if len(d.Path) > 0 {
		dst = append(dst, ' ')
		dst = append(dst, d.Path.String()...)
	}
	dst = append(dst, ": "...)
	dst = append(dst, oneLine(d.Message)...)
	if d.Hint != "" {
		dst = append(dst, " ("...)
		dst = append(dst, oneLine(d.Hint)...)
		dst = append(dst, ')')
	}
	for _, r := range d.Related {
		dst = append(dst, " ("...)
		dst = append(dst, oneLine(r.Message)...)
		dst = append(dst, ' ')
		dst = appendLocation(dst, r.Location)
		dst = append(dst, ')')
	}
	if d.Environment != "" {
		dst = append(dst, " [environment="...)
		dst = append(dst, d.Environment...)
		dst = append(dst, ']')
	}
	return dst
}

func appendLocation(dst []byte, l Location) []byte {
	if l.File == "" {
		return append(dst, '-')
	}
	dst = append(dst, l.File...)
	if l.Line > 0 {
		dst = append(dst, ':')
		dst = strconv.AppendInt(dst, int64(l.Line), 10)
		if l.Column > 0 {
			dst = append(dst, ':')
			dst = strconv.AppendInt(dst, int64(l.Column), 10)
		}
	}
	return dst
}

func oneLine(s string) string {
	if !strings.ContainsAny(s, "\r\n") {
		return s
	}
	return strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(s)
}

// WriteText writes one line per diagnostic, in list order.
func WriteText(w io.Writer, l List) error {
	buf := make([]byte, 0, 256)
	for _, d := range l {
		buf = append(d.AppendText(buf[:0]), '\n')
		if _, err := w.Write(buf); err != nil {
			return err
		}
	}
	return nil
}

type jsonRelated struct {
	File    string `json:"file,omitempty"`
	Line    int    `json:"line,omitempty"`
	Column  int    `json:"column,omitempty"`
	Message string `json:"message,omitempty"`
}

type jsonDiagnostic struct {
	Code        string        `json:"code"`
	Severity    Severity      `json:"severity"`
	File        string        `json:"file,omitempty"`
	Line        int           `json:"line,omitempty"`
	Column      int           `json:"column,omitempty"`
	Resource    *ResourceID   `json:"resource,omitempty"`
	Path        Path          `json:"path,omitempty"`
	Message     string        `json:"message"`
	Hint        string        `json:"hint,omitempty"`
	Related     []jsonRelated `json:"related,omitempty"`
	Environment string        `json:"environment,omitempty"`
}

// WriteJSON writes a bare JSON array ("[]" when empty) with members in the
// order code, severity, file, line, column, resource, path, message, hint,
// related, environment; two-space indent, no HTML escaping, final newline.
func WriteJSON(w io.Writer, l List) error {
	out := make([]jsonDiagnostic, 0, len(l))
	for _, d := range l {
		jd := jsonDiagnostic{
			Code: d.Code, Severity: d.Severity, File: d.File, Line: d.Line, Column: d.Column,
			Resource: d.Resource, Path: d.Path, Message: d.Message, Hint: d.Hint, Environment: d.Environment,
		}
		for _, r := range d.Related {
			jd.Related = append(jd.Related, jsonRelated{File: r.File, Line: r.Line, Column: r.Column, Message: r.Message})
		}
		out = append(out, jd)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(out)
}

// Collector gathers diagnostics from concurrent workers and enforces the
// per-run cap (10,000, proposed); reaching it appends one RZ-CFG-001
// "too many diagnostics" error and rejects further adds.
type Collector struct {
	mu   sync.Mutex
	list List
	max  int
	full bool
}

// NewCollector returns a Collector holding at most maxDiags diagnostics.
func NewCollector(maxDiags int) *Collector { return &Collector{max: maxDiags} }

// Add records d; it returns false once the cap is reached.
func (c *Collector) Add(d Diagnostic) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.full {
		return false
	}
	if c.max > 0 && len(c.list) >= c.max {
		c.full = true
		c.list = append(c.list, Diagnostic{Code: "RZ-CFG-001", Severity: SeverityError, Message: "too many diagnostics; stopped after " + strconv.Itoa(c.max)})
		return false
	}
	c.list = append(c.list, d)
	return true
}

// AddAll records every diagnostic of l.
func (c *Collector) AddAll(l List) bool {
	for _, d := range l {
		if !c.Add(d) {
			return false
		}
	}
	return true
}

// Full reports whether the cap was reached.
func (c *Collector) Full() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.full
}

// List returns a sorted copy.
func (c *Collector) List() List {
	c.mu.Lock()
	out := slices.Clone(c.list)
	c.mu.Unlock()
	out.Sort()
	return out
}

// Nearest returns the best candidate by optimal string alignment distance:
// an exact case-insensitive match first, else the smallest distance at most
// max(1, min(3, len(input)/3)), ties broken by byte order. Candidates whose
// length differs by more than 3 are skipped; at most 64 are compared.
func Nearest(input string, candidates []string) (string, bool) {
	for _, c := range candidates {
		if strings.EqualFold(c, input) {
			return c, true
		}
	}
	limit := max(1, min(3, len(input)/3))
	best, bestD, compared := "", limit+1, 0
	for _, c := range candidates {
		if abs(len(c)-len(input)) > 3 {
			continue
		}
		if compared++; compared > 64 {
			break
		}
		d := osa(strings.ToLower(input), strings.ToLower(c))
		if d < bestD || (d == bestD && c < best) {
			best, bestD = c, d
		}
	}
	return best, bestD <= limit
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// osa is the optimal string alignment distance over bytes.
func osa(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, d[i-1][j-1]+cost)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}
