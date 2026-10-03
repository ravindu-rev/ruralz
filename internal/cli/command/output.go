// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"encoding"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"time"
)

// TimeFormat is the timestamp layout of CLI output: RFC 3339 UTC with
// millisecond precision (spec 10 req 14).
const TimeFormat = "2006-01-02T15:04:05.000Z"

// FormatTime formats t in UTC with TimeFormat.
func FormatTime(t time.Time) string { return t.UTC().Format(TimeFormat) }

// WriteJSON writes v as one JSON document (spec 10 req 16): two-space
// indentation, no HTML escaping, members in struct field order, one
// trailing newline, and empty arrays and objects written [] and {}, never
// null (nil slices and maps are written empty). Optional members are
// omitted when absent: a field tagged omitempty is omitted when empty, and
// a field tagged omitzero is left as it is while zero, so a nil slice or
// map is omitted while an empty one is written [] or {}. The document is
// written with one Write call, so nothing reaches w when encoding fails.
func WriteJSON(w io.Writer, v any) error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(normalizeJSON(v)); err != nil {
		return fmt.Errorf("encode JSON: %w", err)
	}
	_, err := w.Write(buf.Bytes())
	return err
}

// LineWriter writes NDJSON streams and text lines (spec 10 req 16): one
// compact object or line per Write call, flushed per line when the
// writer has a Flush method. It is safe for concurrent use.
type LineWriter struct {
	mu  sync.Mutex
	w   io.Writer
	buf bytes.Buffer
}

// NewLineWriter returns a LineWriter over w.
func NewLineWriter(w io.Writer) *LineWriter { return &LineWriter{w: w} }

// Object writes v as one compact JSON line without HTML escaping, with
// the empty-array rule of WriteJSON.
func (l *LineWriter) Object(v any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
	enc := json.NewEncoder(&l.buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(normalizeJSON(v)); err != nil {
		return fmt.Errorf("encode JSON line: %w", err)
	}
	return l.flush()
}

// Line writes s followed by a newline.
func (l *LineWriter) Line(s string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
	l.buf.WriteString(s)
	l.buf.WriteByte('\n')
	return l.flush()
}

// Raw writes b verbatim, adding a newline unless b ends with one; dev tap
// re-emits accepted stream lines this way.
func (l *LineWriter) Raw(b []byte) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
	l.buf.Write(b)
	if len(b) == 0 || b[len(b)-1] != '\n' {
		l.buf.WriteByte('\n')
	}
	return l.flush()
}

func (l *LineWriter) flush() error {
	if _, err := l.w.Write(l.buf.Bytes()); err != nil {
		return err
	}
	if f, ok := l.w.(interface{ Flush() error }); ok {
		return f.Flush()
	}
	return nil
}

// maxNormalizeDepth bounds the walk over cyclic or very deep values; the
// encoder rejects cycles itself.
const maxNormalizeDepth = 64

// normalizeJSON returns a copy of v in which nil slices and maps are
// empty, so encoding/json writes [] and {} instead of null. Values whose
// type implements json.Marshaler or encoding.TextMarshaler are kept as
// they are, and so are zero struct fields tagged omitzero, which the
// encoder then omits.
func normalizeJSON(v any) any {
	if v == nil {
		return nil
	}
	return normalizeValue(reflect.ValueOf(v), 0).Interface()
}

func normalizeValue(v reflect.Value, depth int) reflect.Value {
	t := v.Type()
	if depth > maxNormalizeDepth || marshals(t) {
		return v
	}
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		p := reflect.New(t.Elem())
		p.Elem().Set(normalizeValue(v.Elem(), depth+1))
		return p
	case reflect.Interface:
		if v.IsNil() {
			return v
		}
		out := reflect.New(t).Elem()
		out.Set(normalizeValue(v.Elem(), depth+1))
		return out
	case reflect.Struct:
		out := reflect.New(t).Elem()
		out.Set(v)
		for i := range t.NumField() {
			sf := t.Field(i)
			if !sf.IsExported() || (omitZero(sf) && jsonZero(v.Field(i))) {
				continue
			}
			out.Field(i).Set(normalizeValue(v.Field(i), depth+1))
		}
		return out
	case reflect.Slice:
		if v.IsNil() {
			return reflect.MakeSlice(t, 0, 0)
		}
		out := reflect.MakeSlice(t, v.Len(), v.Len())
		for i := range v.Len() {
			out.Index(i).Set(normalizeValue(v.Index(i), depth+1))
		}
		return out
	case reflect.Array:
		out := reflect.New(t).Elem()
		for i := range v.Len() {
			out.Index(i).Set(normalizeValue(v.Index(i), depth+1))
		}
		return out
	case reflect.Map:
		if v.IsNil() {
			return reflect.MakeMap(t)
		}
		out := reflect.MakeMapWithSize(t, v.Len())
		for it := v.MapRange(); it.Next(); {
			out.SetMapIndex(it.Key(), normalizeValue(it.Value(), depth+1))
		}
		return out
	default:
		return v
	}
}

// marshals reports whether t or *t encodes itself.
func marshals(t reflect.Type) bool {
	if t.Kind() == reflect.Interface {
		return false
	}
	jm := reflect.TypeFor[json.Marshaler]()
	tm := reflect.TypeFor[encoding.TextMarshaler]()
	pt := reflect.PointerTo(t)
	return t.Implements(jm) || pt.Implements(jm) || t.Implements(tm) || pt.Implements(tm)
}

// omitZero reports whether the json tag of sf has the omitzero option.
func omitZero(sf reflect.StructField) bool {
	tag := sf.Tag.Get("json")
	if tag == "-" {
		return false
	}
	_, opts, _ := strings.Cut(tag, ",")
	return slices.Contains(strings.Split(opts, ","), "omitzero")
}

// isZeroer is the method encoding/json consults for omitzero.
type isZeroer interface{ IsZero() bool }

// jsonZero reports whether encoding/json treats v as zero for omitzero:
// through an IsZero method of the type or its pointer, which a nil
// pointer or interface never reaches, or else the zero value.
func jsonZero(v reflect.Value) bool {
	t := v.Type()
	zeroer := reflect.TypeFor[isZeroer]()
	switch {
	case t.Kind() == reflect.Interface || t.Kind() == reflect.Pointer:
		if v.IsNil() {
			return true
		}
		if !t.Implements(zeroer) {
			return false
		}
		if e := v.Elem(); t.Kind() == reflect.Interface && e.Kind() == reflect.Pointer && e.IsNil() {
			return true
		}
		return callIsZero(v)
	case t.Implements(zeroer):
		return callIsZero(v)
	case reflect.PointerTo(t).Implements(zeroer):
		p := reflect.New(t)
		p.Elem().Set(v)
		return callIsZero(p)
	default:
		return v.IsZero()
	}
}

// callIsZero calls the IsZero method of v, whose type implements isZeroer.
func callIsZero(v reflect.Value) bool {
	z, ok := v.Interface().(isZeroer)
	return ok && z.IsZero()
}
