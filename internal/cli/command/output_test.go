// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package command

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Tests for spec 10 reqs 14 and 16: timestamps, the JSON document writer
// and the NDJSON line writer.

func TestFormatTimeReq14(t *testing.T) {
	loc := time.FixedZone("CEST", 2*3600)
	cases := map[time.Time]string{
		time.Date(2026, 9, 26, 14, 0, 0, 0, loc):                                      "2026-09-26T12:00:00.000Z",
		time.Date(2026, 1, 2, 3, 4, 5, 678901234, time.UTC):                           "2026-01-02T03:04:05.678Z",
		time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC):                      "2026-12-31T23:59:59.999Z",
		time.Date(2026, 12, 31, 23, 59, 59, 999999999, time.UTC).Add(time.Nanosecond): "2027-01-01T00:00:00.000Z",
	}
	for in, want := range cases {
		if got := FormatTime(in); got != want {
			t.Errorf("FormatTime(%v) = %q, want %q", in, got, want)
		}
	}
}

type jsonDoc struct {
	Digest   string            `json:"digest,omitempty"`
	Revision string            `json:"revision,omitempty"`
	Items    []string          `json:"items"`
	Optional []string          `json:"optional,omitempty"`
	Labels   map[string]string `json:"labels"`
	Nested   *jsonNested       `json:"nested,omitempty"`
	List     []jsonNested      `json:"list"`
	Any      any               `json:"any,omitempty"`
	When     time.Time         `json:"when"`
	Raw      json.RawMessage   `json:"raw,omitempty"`
	Array    [2]jsonNested     `json:"array"`
	Map      map[string][]int  `json:"map"`
	hidden   []string
}

type jsonNested struct {
	Rows []int `json:"rows"`
}

// TestWriteJSONReq16 pins the document form: two-space indentation, field
// order, no HTML escaping, one trailing newline, [] and {} instead of null.
func TestWriteJSONReq16(t *testing.T) {
	doc := jsonDoc{
		Revision: "rev-<&>",
		Nested:   &jsonNested{},
		List:     []jsonNested{{}, {Rows: []int{1}}},
		Any:      []jsonNested{{}},
		When:     time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC),
		Raw:      json.RawMessage(`{"a":1}`),
		Map:      map[string][]int{"k": nil},
		hidden:   []string{"x"},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, doc); err != nil {
		t.Fatal(err)
	}
	want := `{
  "revision": "rev-<&>",
  "items": [],
  "labels": {},
  "nested": {
    "rows": []
  },
  "list": [
    {
      "rows": []
    },
    {
      "rows": [
        1
      ]
    }
  ],
  "any": [
    {
      "rows": []
    }
  ],
  "when": "2026-09-26T00:00:00Z",
  "raw": {
    "a": 1
  },
  "array": [
    {
      "rows": []
    },
    {
      "rows": []
    }
  ],
  "map": {
    "k": []
  }
}
`
	if buf.String() != want {
		t.Errorf("WriteJSON =\n%s\nwant\n%s", buf.String(), want)
	}
	// The caller's value is not modified.
	if doc.Items != nil || doc.Nested.Rows != nil || doc.Map["k"] != nil {
		t.Error("WriteJSON modified its argument")
	}
}

func TestWriteJSONValues(t *testing.T) {
	cases := []struct {
		in   any
		want string
	}{
		{nil, "null\n"},
		{[]string(nil), "[]\n"},
		{map[string]int(nil), "{}\n"},
		{(*jsonNested)(nil), "null\n"},
		{&jsonNested{}, "{\n  \"rows\": []\n}\n"},
		{"a<b>", "\"a<b>\"\n"},
		{[]any{nil, []int(nil)}, "[\n  null,\n  []\n]\n"},
	}
	for _, c := range cases {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, c.in); err != nil {
			t.Fatalf("WriteJSON(%#v) = %v", c.in, err)
		}
		if buf.String() != c.want {
			t.Errorf("WriteJSON(%#v) = %q, want %q", c.in, buf.String(), c.want)
		}
	}
}

// marked is zero through its IsZero method, whatever its fields hold.
type marked struct {
	Set  bool  `json:"-"`
	Rows []int `json:"rows"`
}

func (m marked) IsZero() bool { return !m.Set }

// ptrMarked has IsZero on its pointer receiver.
type ptrMarked struct {
	Set  bool  `json:"-"`
	Rows []int `json:"rows"`
}

func (m *ptrMarked) IsZero() bool { return !m.Set }

type omitZeroDoc struct {
	Absent    []string                   `json:"absent,omitzero"`
	Empty     []string                   `json:"empty,omitzero"`
	NoMap     map[string]int             `json:"noMap,omitzero"`
	Zero      jsonNested                 `json:"zero,omitzero"`
	Window    jsonNested                 `json:"window,omitempty,omitzero"`
	NilPtr    *jsonNested                `json:"nilPtr,omitzero"`
	Ptr       *jsonNested                `json:"ptr,omitzero"`
	NilAny    any                        `json:"nilAny,omitzero"`
	Any       any                        `json:"any,omitzero"`
	Marked    marked                     `json:"marked,omitzero"`
	MarkedSet marked                     `json:"markedSet,omitzero"`
	PtrMarked ptrMarked                  `json:"ptrMarked,omitzero"`
	PtrSet    ptrMarked                  `json:"ptrSet,omitzero"`
	Zeroer    interface{ IsZero() bool } `json:"zeroer,omitzero"`
	NilZeroer interface{ IsZero() bool } `json:"nilZeroer,omitzero"`
	TypedNil  interface{ IsZero() bool } `json:"typedNil,omitzero"`
	MarkedPtr *marked                    `json:"markedPtr,omitzero"`
	Dash      []string                   `json:"-"`
	Plain     []string                   `json:"plain"`
}

// TestWriteJSONOmitZeroReq16 covers req 16's "optional members omitted
// when absent" for omitzero: a zero field stays zero, so the encoder
// omits it, while a non-zero one is still written with [] for nil slices.
func TestWriteJSONOmitZeroReq16(t *testing.T) {
	doc := omitZeroDoc{
		Empty:     []string{},
		Window:    jsonNested{Rows: []int{}},
		Ptr:       &jsonNested{},
		Any:       jsonNested{},
		Marked:    marked{Rows: []int{1}},
		MarkedSet: marked{Set: true},
		PtrMarked: ptrMarked{Rows: []int{1}},
		PtrSet:    ptrMarked{Set: true},
		Zeroer:    marked{},
		TypedNil:  (*marked)(nil),
		MarkedPtr: &marked{Set: true},
	}
	var buf bytes.Buffer
	if err := WriteJSON(&buf, doc); err != nil {
		t.Fatal(err)
	}
	want := `{
  "empty": [],
  "window": {
    "rows": []
  },
  "ptr": {
    "rows": []
  },
  "any": {
    "rows": []
  },
  "markedSet": {
    "rows": []
  },
  "ptrSet": {
    "rows": []
  },
  "markedPtr": {
    "rows": []
  },
  "plain": []
}
`
	if buf.String() != want {
		t.Errorf("WriteJSON =\n%s\nwant\n%s", buf.String(), want)
	}
}

// TestJSONZero pins jsonZero to encoding/json's omitzero rule.
func TestJSONZero(t *testing.T) {
	var nilZeroer interface{ IsZero() bool }
	cases := []struct {
		name string
		v    reflect.Value
		want bool
	}{
		{"nil slice", reflect.ValueOf([]int(nil)), true},
		{"empty slice", reflect.ValueOf([]int{}), false},
		{"zero struct", reflect.ValueOf(jsonNested{}), true},
		{"nil pointer", reflect.ValueOf((*jsonNested)(nil)), true},
		{"pointer to zero", reflect.ValueOf(&jsonNested{}), false},
		{"IsZero true despite fields", reflect.ValueOf(marked{Rows: []int{1}}), true},
		{"IsZero false on a zero-looking value", reflect.ValueOf(marked{Set: true}), false},
		{"pointer-receiver IsZero", reflect.ValueOf(ptrMarked{Rows: []int{1}}), true},
		{"pointer-receiver IsZero false", reflect.ValueOf(ptrMarked{Set: true}), false},
		{"pointer with IsZero", reflect.ValueOf(&marked{}), true},
		{"pointer with IsZero false", reflect.ValueOf(&marked{Set: true}), false},
		{"nil interface", reflect.ValueOf(&nilZeroer).Elem(), true},
	}
	for _, c := range cases {
		if got := jsonZero(c.v); got != c.want {
			t.Errorf("jsonZero(%s) = %v, want %v", c.name, got, c.want)
		}
	}
	var holder struct {
		Z interface{ IsZero() bool }
		A any
	}
	holder.Z = (*marked)(nil)
	if !jsonZero(reflect.ValueOf(holder).Field(0)) {
		t.Error("jsonZero(interface holding a nil pointer) = false, want true")
	}
	holder.Z = marked{Set: true}
	if jsonZero(reflect.ValueOf(holder).Field(0)) {
		t.Error("jsonZero(interface holding a set value) = true, want false")
	}
	holder.A = 0
	if jsonZero(reflect.ValueOf(holder).Field(1)) {
		t.Error("jsonZero(any holding 0) = true, want false")
	}
}

type cyclic struct {
	Next *cyclic `json:"next"`
}

func TestWriteJSONErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := WriteJSON(&buf, map[string]any{"c": make(chan int)}); err == nil || buf.Len() != 0 {
		t.Errorf("unsupported value: err %v, wrote %q", err, buf.String())
	}
	c := &cyclic{}
	c.Next = c
	if err := WriteJSON(&buf, c); err == nil || buf.Len() != 0 {
		t.Errorf("cycle: err %v, wrote %d bytes", err, buf.Len())
	}
	if err := WriteJSON(failWriter{}, 1); err == nil {
		t.Error("writer error not returned")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

type flushWriter struct {
	bytes.Buffer
	flushes int
}

func (f *flushWriter) Flush() error {
	f.flushes++
	return nil
}

// TestLineWriterReq16 pins NDJSON: one compact object per line, flushed
// per line.
func TestLineWriterReq16(t *testing.T) {
	var w flushWriter
	l := NewLineWriter(&w)
	if err := l.Object(struct {
		Time  string   `json:"time"`
		Event string   `json:"event"`
		List  []string `json:"list"`
	}{"2026-09-26T12:00:00.000Z", "ready<>", nil}); err != nil {
		t.Fatal(err)
	}
	if err := l.Line("listener web: 8080 -> 41234"); err != nil {
		t.Fatal(err)
	}
	if err := l.Raw([]byte(`{"route":"a","x":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := l.Raw([]byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if err := l.Raw(nil); err != nil {
		t.Fatal(err)
	}
	want := `{"time":"2026-09-26T12:00:00.000Z","event":"ready<>","list":[]}
listener web: 8080 -> 41234
{"route":"a","x":1}
{}

`
	if w.String() != want {
		t.Errorf("lines =\n%q\nwant\n%q", w.String(), want)
	}
	if w.flushes != 5 {
		t.Errorf("flushes = %d, want 5", w.flushes)
	}
	if err := l.Object(make(chan int)); err == nil {
		t.Error("encode error not returned")
	}
	bad := NewLineWriter(failWriter{})
	if bad.Line("x") == nil || bad.Object(1) == nil || bad.Raw([]byte("x")) == nil {
		t.Error("write error not returned")
	}
}

func TestLineWriterConcurrent(t *testing.T) {
	var buf bytes.Buffer
	l := NewLineWriter(&buf)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for range 50 {
				_ = l.Object(map[string]int{"writer": i})
			}
		})
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("%d lines, want 400", len(lines))
	}
	for _, line := range lines {
		var v map[string]int
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			t.Fatalf("interleaved line %q: %v", line, err)
		}
	}
}
