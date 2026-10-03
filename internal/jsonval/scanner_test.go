// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// Tests for the strict scanner: 07 req 58 and 80 (RFC 8259, one value,
// trailing whitespace only, valid UTF-8, depth 64, duplicates), 01 req 15
// (the configuration JSON front end: offsets for RZ-CFG-001, both positions
// for RZ-CFG-002, unpaired surrogate escapes rejected) and 06 req 25 (JWT
// header and payload without duplicate names).

// TestConformance runs the JSON conformance corpus (07 req 58, 80; 01 req
// 15): y_ cases are accepted, n_ cases rejected and i_ cases follow the
// documented Ruralz decision; Decode agrees with Validate on every case.
func TestConformance(t *testing.T) {
	for _, c := range conformanceCases() {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(c.input), ScanOptions{})
			if (err == nil) != c.accept {
				t.Fatalf("Validate accept = %v (err %v), want %v", err == nil, err, c.accept)
			}
			if err != nil {
				var e *Error
				if !errors.As(err, &e) {
					t.Fatalf("error %T is not *Error", err)
				}
				if e.Offset < 0 || e.Offset > len(c.input) {
					t.Fatalf("offset %d outside the input", e.Offset)
				}
			}
			_, _, derr := Decode([]byte(c.input), Options{})
			if (derr == nil) != (err == nil) || (err != nil && derr.Error() != err.Error()) {
				t.Fatalf("Decode error %v, Validate error %v", derr, err)
			}
			switch c.flag {
			case "dup":
				if !errors.Is(err, ErrDuplicateName) {
					t.Fatalf("err = %v, want ErrDuplicateName", err)
				}
				if err := Validate([]byte(c.input), ScanOptions{AllowDuplicateNames: true}); err != nil {
					t.Fatalf("with AllowDuplicateNames: %v", err)
				}
			case "deep":
				if !errors.Is(err, ErrDepth) {
					t.Fatalf("err = %v, want ErrDepth", err)
				}
				if err := Validate([]byte(c.input), ScanOptions{MaxDepth: MaxDepthLimit}); err != nil {
					t.Fatalf("with MaxDepthLimit: %v", err)
				}
			}
		})
	}
}

// tok is a compact token description for table tests.
type tok struct {
	kind Kind
	text string // decoded text
	off  int
	esc  bool
}

func scanAll(t *testing.T, input string, o ScanOptions) []tok {
	t.Helper()
	s := NewScanner([]byte(input), o)
	var out []tok
	for {
		tk, err := s.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		if got := string(s.Bytes(tk)); got != input[tk.Offset:tk.End] {
			t.Fatalf("Bytes = %q", got)
		}
		out = append(out, tok{tk.Kind, s.Text(tk), tk.Offset, tk.Escaped})
	}
}

func TestScannerTokens(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  []tok
	}{
		{"scalars", ` 42 `, []tok{{KindNumber, "42", 1, false}}},
		{"literals", `[null,true,false]`, []tok{
			{KindArrayStart, "[", 0, false},
			{KindNull, "null", 1, false},
			{KindTrue, "true", 6, false},
			{KindFalse, "false", 11, false},
			{KindArrayEnd, "]", 16, false},
		}},
		{"object", "{\"a\" : [1.5e3, -0],\n\t\"b\":{}}", []tok{
			{KindObjectStart, "{", 0, false},
			{KindName, "a", 1, false},
			{KindArrayStart, "[", 7, false},
			{KindNumber, "1.5e3", 8, false},
			{KindNumber, "-0", 15, false},
			{KindArrayEnd, "]", 17, false},
			{KindName, "b", 21, false},
			{KindObjectStart, "{", 25, false},
			{KindObjectEnd, "}", 26, false},
			{KindObjectEnd, "}", 27, false},
		}},
		{"escapes", `["a\"\\\/\b\f\n\r\t\u00e9\ud83d\ude00"]`, []tok{
			{KindArrayStart, "[", 0, false}, {KindString, "a\"\\/\b\f\n\r\té😀", 1, true}, {KindArrayEnd, "]", 38, false},
		}},
		{"unescaped non-ASCII", `"æøå ≠ 😀"`, []tok{{KindString, "æøå ≠ 😀", 0, false}}},
		{"html and separators stay literal", "\"<&>  \x7f\"", []tok{{KindString, "<&>  \x7f", 0, false}}},
		{"empty containers", `[[],{}]`, []tok{
			{KindArrayStart, "[", 0, false},
			{KindArrayStart, "[", 1, false},
			{KindArrayEnd, "]", 2, false},
			{KindObjectStart, "{", 4, false},
			{KindObjectEnd, "}", 5, false},
			{KindArrayEnd, "]", 6, false},
		}},
		{"escaped name", `{"\u0061b":1}`, []tok{
			{KindObjectStart, "{", 0, false}, {KindName, "ab", 1, true}, {KindNumber, "1", 11, false}, {KindObjectEnd, "}", 12, false},
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scanAll(t, c.input, ScanOptions{})
			if fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("tokens\n got %v\nwant %v", got, c.want)
			}
		})
	}
}

func TestScannerErrors(t *testing.T) {
	deep := strings.Repeat("[", DefaultMaxDepth+1) + strings.Repeat("]", DefaultMaxDepth+1)
	cases := []struct {
		name  string
		input string
		o     ScanOptions
		kind  error
		msg   string
		off   int
		other int
	}{
		{"empty", ``, ScanOptions{}, ErrSyntax, msgEOF, 0, -1},
		{"only space", " \n", ScanOptions{}, ErrSyntax, msgEOF, 2, -1},
		{"trailing comma array", `[1,]`, ScanOptions{}, ErrSyntax, msgValueStart, 3, -1},
		{"trailing comma object", `{"a":1,}`, ScanOptions{}, ErrSyntax, msgName, 7, -1},
		{"two values", `{} {}`, ScanOptions{}, ErrSyntax, msgAfterTop, 3, -1},
		{"leading zero", `[01]`, ScanOptions{}, ErrSyntax, msgAfterElement, 2, -1},
		{"bad fraction", `1.x`, ScanOptions{}, ErrSyntax, msgNumber, 2, -1},
		{"minus at end", `-`, ScanOptions{}, ErrSyntax, msgEOF, 1, -1},
		{"bad literal", `nulx`, ScanOptions{}, ErrSyntax, msgLiteral, 3, -1},
		{"short literal", `tru`, ScanOptions{}, ErrSyntax, msgEOF, 3, -1},
		{"comment", `[1/*c*/]`, ScanOptions{}, ErrSyntax, msgAfterElement, 2, -1},
		{"missing colon", `{"a" 1}`, ScanOptions{}, ErrSyntax, msgColon, 5, -1},
		{"member separator", `{"a":1 "b":2}`, ScanOptions{}, ErrSyntax, msgAfterMember, 7, -1},
		{"mismatched close", `[1}`, ScanOptions{}, ErrSyntax, msgAfterElement, 2, -1},
		{"unterminated string", `"abc`, ScanOptions{}, ErrSyntax, msgEOF, 4, -1},
		{"control character", "\"a\tb\"", ScanOptions{}, ErrSyntax, msgControl, 2, -1},
		{"bad escape", `"\x"`, ScanOptions{}, ErrSyntax, msgEscape, 1, -1},
		{"bad hex", `"\u12g4"`, ScanOptions{}, ErrSyntax, msgEscape, 5, -1},
		{"escape at end", `"\`, ScanOptions{}, ErrSyntax, msgEOF, 2, -1},
		{"short unicode escape at end", `"\u12`, ScanOptions{}, ErrSyntax, msgEOF, 5, -1},
		{"name then end", `{"a"`, ScanOptions{}, ErrSyntax, msgEOF, 4, -1},
		{"member then end", `{"a":1`, ScanOptions{}, ErrSyntax, msgEOF, 6, -1},
		{"no name after comma", `{"a":1,2}`, ScanOptions{}, ErrSyntax, msgName, 7, -1},
		{"end after comma", `{"a":1,`, ScanOptions{}, ErrSyntax, msgEOF, 7, -1},
		{"object closed by bracket", `{"a":1]`, ScanOptions{}, ErrSyntax, msgAfterMember, 6, -1},
		{"lone high surrogate", `["\ud800"]`, ScanOptions{}, ErrSurrogate, msgSurrogate, 2, -1},
		{"lone low surrogate", `"x\udc00"`, ScanOptions{}, ErrSurrogate, msgSurrogate, 2, -1},
		{"high then non-low", `"\ud800\u0041"`, ScanOptions{}, ErrSurrogate, msgSurrogate, 1, -1},
		{"high then another high", `"\ud800\ud800\udc00"`, ScanOptions{}, ErrSurrogate, msgSurrogate, 1, -1},
		{"high then short escape", `"\ud800\n"`, ScanOptions{}, ErrSurrogate, msgSurrogate, 1, -1},
		// 01 req 15: after a high surrogate, a malformed or truncated
		// escape is reported at its own offset, not as an unpaired
		// surrogate, so RZ-CFG-001 points at the bad character.
		{"high then bad hex", `"\uD800\uZZZZ"`, ScanOptions{}, ErrSyntax, msgEscape, 9, -1},
		{"high then bad later hex", `"\uD800\uDC0g"`, ScanOptions{}, ErrSyntax, msgEscape, 12, -1},
		{"high then truncated escape", `"\uD800\u00`, ScanOptions{}, ErrSyntax, msgEOF, 11, -1},
		{"high then backslash at end", `"\uD800\`, ScanOptions{}, ErrSyntax, msgEOF, 8, -1},
		{"high at end", `"\uD800`, ScanOptions{}, ErrSyntax, msgEOF, 7, -1},
		{"high then bad escape", `"\uD800\x"`, ScanOptions{}, ErrSyntax, msgEscape, 7, -1},
		{"invalid UTF-8 in string", "[\"a\xffb\"]", ScanOptions{}, ErrInvalidUTF8, msgUTF8, 3, -1},
		{"UTF-8 encoded surrogate", "\"\xed\xa0\x80\"", ScanOptions{}, ErrInvalidUTF8, msgUTF8, 1, -1},
		{"BOM", "\xef\xbb\xbf{}", ScanOptions{}, ErrSyntax, msgValueStart, 0, -1},
		{"depth 65", deep, ScanOptions{}, ErrDepth, msgDepth, DefaultMaxDepth, -1},
		{"depth limit 1", `[[]]`, ScanOptions{MaxDepth: 1}, ErrDepth, msgDepth, 1, -1},
		{"duplicate", `{"a":1,"a":2}`, ScanOptions{}, ErrDuplicateName, msgDuplicate, 7, 1},
		{"escaped duplicate", `{"ab":1,"\u0061\u0062":2}`, ScanOptions{}, ErrDuplicateName, msgDuplicate, 8, 1},
		{"nested duplicate", `{"a":{"b":1,"c":2,"b":3}}`, ScanOptions{}, ErrDuplicateName, msgDuplicate, 18, 6},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(c.input), c.o)
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("err = %v, want *Error", err)
			}
			if !errors.Is(err, c.kind) || !errors.Is(e.Kind, c.kind) || e.Msg != c.msg || e.Offset != c.off || e.Other != c.other {
				t.Fatalf("got {%v %q %d %d}, want {%v %q %d %d}", e.Kind, e.Msg, e.Offset, e.Other, c.kind, c.msg, c.off, c.other)
			}
			wantSyntax := errors.Is(c.kind, ErrSyntax) || errors.Is(c.kind, ErrInvalidUTF8) || errors.Is(c.kind, ErrSurrogate)
			if errors.Is(err, ErrSyntax) != wantSyntax {
				t.Fatalf("errors.Is(err, ErrSyntax) = %v", !wantSyntax)
			}
		})
	}
}

func TestErrorText(t *testing.T) {
	// Messages never quote input (07 req 87).
	secret := `{"password":"hunter2","password":"x"}`
	err := Validate([]byte(secret), ScanOptions{})
	if got, want := err.Error(), "jsonval: duplicate object member name at offset 22 (first at offset 1)"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	err = Validate([]byte(`{"password":hunter2}`), ScanOptions{})
	if got, want := err.Error(), "jsonval: invalid character at start of value at offset 12"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
	if strings.Contains(err.Error(), "hunter") {
		t.Fatal("error text quotes input")
	}
}

func TestScannerDepth(t *testing.T) {
	// 07 req 58: nesting depth at most 64 by default.
	for _, c := range []struct {
		max, depth int
		ok         bool
	}{
		{0, DefaultMaxDepth, true},
		{0, DefaultMaxDepth + 1, false},
		{-5, DefaultMaxDepth + 1, false},
		{3, 3, true},
		{3, 4, false},
		{MaxDepthLimit + 1, MaxDepthLimit, true},
		{MaxDepthLimit + 1, MaxDepthLimit + 1, false},
	} {
		obj := strings.Repeat(`{"k":`, c.depth) + "1" + strings.Repeat("}", c.depth)
		arr := strings.Repeat("[", c.depth) + strings.Repeat("]", c.depth)
		for _, in := range []string{obj, arr} {
			err := Validate([]byte(in), ScanOptions{MaxDepth: c.max})
			if (err == nil) != c.ok {
				t.Fatalf("max %d depth %d: err = %v", c.max, c.depth, err)
			}
			if err != nil && !errors.Is(err, ErrDepth) {
				t.Fatalf("max %d depth %d: err = %v, want ErrDepth", c.max, c.depth, err)
			}
		}
	}
	s := NewScanner([]byte(`[[1]]`), ScanOptions{})
	for _, want := range []int{1, 2, 2, 1, 0} {
		if _, err := s.Next(); err != nil {
			t.Fatal(err)
		}
		if s.Depth() != want {
			t.Fatalf("Depth = %d, want %d", s.Depth(), want)
		}
	}
	if s.Offset() != 5 {
		t.Fatalf("Offset = %d", s.Offset())
	}
}

func TestScannerDuplicates(t *testing.T) {
	// 01 req 15, 06 req 25, 07 req 80: duplicates rejected with both
	// offsets, compared after unescaping, per object, through the linear
	// and the indexed paths.
	names := func(n int, prefix string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf(`"%s%d":%d`, prefix, i, i)
		}
		return out
	}
	big := names(1000, "k")
	cases := []struct {
		name  string
		input string
		dupAt int // offset of the duplicate name, or -1
		first int
	}{
		{"distinct", `{"a":1,"b":2,"A":3}`, -1, 0},
		{"same name in different objects", `{"a":{"a":1},"b":{"a":2,"b":3},"c":[{"a":1},{"a":2}]}`, -1, 0},
		{"sibling objects reuse the level", `[{"a":1,"b":2},{"a":3,"b":4}]`, -1, 0},
		{"thousand distinct", "{" + strings.Join(big, ",") + "}", -1, 0},
		{"thousand plus late duplicate", "{" + strings.Join(big, ",") + `,"k500":1}`, len("{" + strings.Join(big, ",") + ","), len("{" + strings.Join(big[:500], ",") + ",")},
		{"duplicate at the index threshold", "{" + strings.Join(names(linearNames, "n"), ",") + `,"n0":1}`, len("{" + strings.Join(names(linearNames, "n"), ",") + ","), 1},
		{"duplicate just below the threshold", "{" + strings.Join(names(linearNames-1, "n"), ",") + `,"n3":1}`, len("{" + strings.Join(names(linearNames-1, "n"), ",") + ","), len("{" + strings.Join(names(3, "n"), ",") + ",")},
		{"escaped duplicate in a large object", "{" + strings.Join(big, ",") + `,"\u006b7":1}`, len("{" + strings.Join(big, ",") + ","), len("{" + strings.Join(big[:7], ",") + ",")},
		{"large nested objects", "{" + strings.Join(big[:40], ",") + `,"x":{` + strings.Join(big[:40], ",") + `},"y":1}`, -1, 0},
		{"large nested then outer duplicate", "{" + strings.Join(big[:40], ",") + `,"x":{` + strings.Join(big[:40], ",") + `},"k39":1}`, len("{" + strings.Join(big[:40], ",") + `,"x":{` + strings.Join(big[:40], ",") + `},`), len("{" + strings.Join(big[:39], ",") + ",")},
		{"escape-only difference", `{"\u00e9":1,"é":2}`, 12, 1},
		{"surrogate pair names", `{"\ud83d\ude00":1,"😀":2}`, 18, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := Validate([]byte(c.input), ScanOptions{})
			if c.dupAt < 0 {
				if err != nil {
					t.Fatalf("Validate: %v", err)
				}
				return
			}
			var e *Error
			if !errors.As(err, &e) || !errors.Is(e.Kind, ErrDuplicateName) || e.Offset != c.dupAt || e.Other != c.first {
				t.Fatalf("err = %v, want duplicate at %d first %d", err, c.dupAt, c.first)
			}
			if err := Validate([]byte(c.input), ScanOptions{AllowDuplicateNames: true}); err != nil {
				t.Fatalf("with AllowDuplicateNames: %v", err)
			}
		})
	}
}

func TestScannerStickyAndReset(t *testing.T) {
	s := NewScanner([]byte(`[1,,]`), ScanOptions{})
	var first error
	for range 5 {
		if _, err := s.Next(); err != nil {
			first = err
			break
		}
	}
	if first == nil {
		t.Fatal("no error")
	}
	if _, err := s.Next(); err != first { //nolint:errorlint // the same sticky error value is expected
		t.Fatalf("second error %v differs from %v", err, first)
	}
	// io.EOF repeats after the value.
	s.Reset([]byte(` 7 `), ScanOptions{})
	if tk, err := s.Next(); err != nil || tk.Kind != KindNumber {
		t.Fatalf("Next = %v, %v", tk, err)
	}
	for range 2 {
		if _, err := s.Next(); !errors.Is(err, io.EOF) {
			t.Fatalf("err = %v, want io.EOF", err)
		}
	}
	// Reset after an error inside nested objects clears the name state.
	s.Reset([]byte(`{"a":{"b":{"a":1,"a":2}}}`), ScanOptions{})
	for {
		if _, err := s.Next(); err != nil {
			break
		}
	}
	s.Reset([]byte(`{"a":1,"b":{"a":2}}`), ScanOptions{})
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("after reset: %v", err)
		}
	}
}

func TestScannerAppendText(t *testing.T) {
	s := NewScanner([]byte(`{"k\n":[12,"v"]}`), ScanOptions{})
	var got []string
	for {
		tk, err := s.Next()
		if err != nil {
			break
		}
		got = append(got, string(s.AppendText([]byte("|"), tk)))
	}
	want := []string{"|{", "|k\n", "|[", "|12", "|v", "|]", "|}"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("AppendText = %q, want %q", got, want)
	}
}

func TestKindString(t *testing.T) {
	want := map[Kind]string{
		KindInvalid: "invalid", KindNull: "null", KindFalse: "false", KindTrue: "true",
		KindNumber: "number", KindString: "string", KindName: "name",
		KindObjectStart: "object start", KindObjectEnd: "object end",
		KindArrayStart: "array start", KindArrayEnd: "array end", Kind(200): "invalid",
	}
	for k, s := range want {
		if k.String() != s {
			t.Fatalf("%d.String() = %q, want %q", k, k.String(), s)
		}
	}
}

// readOrders returns the 1 KiB orders document used by benchmarks.
func readOrders(tb testing.TB) []byte {
	tb.Helper()
	b, err := readOrdersFile()
	if err != nil {
		tb.Fatal(err)
	}
	return b
}

// scanTokens scans data with a reused scanner and returns the token count.
func scanTokens(s *Scanner, data []byte, o ScanOptions) (int, error) {
	s.Reset(data, o)
	n := 0
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
		n++
	}
}

func TestScannerZeroAllocsPerToken(t *testing.T) {
	// WP-02 "Done when": 0 allocations per scanned token. A reused
	// Scanner allocates nothing once its buffers have grown, with the
	// duplicate check on and through the indexed path.
	big := make([]string, 200)
	for i := range big {
		big[i] = fmt.Sprintf(`"\u006b%d":{"a":%d,"b":"x\n"}`, i, i)
	}
	inputs := map[string][]byte{
		"orders":       readOrders(t),
		"large object": []byte("{" + strings.Join(big, ",") + "}"),
		"deep":         []byte(strings.Repeat(`[{"a":`, 30) + "1" + strings.Repeat("}]", 30)),
	}
	for name, data := range inputs {
		for _, o := range []ScanOptions{{}, {AllowDuplicateNames: true}} {
			s := NewScanner(nil, o)
			if _, err := scanTokens(s, data, o); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			allocs := testing.AllocsPerRun(20, func() {
				if _, err := scanTokens(s, data, o); err != nil {
					t.Fatal(err)
				}
			})
			if allocs != 0 {
				t.Fatalf("%s %+v: %v allocations per scan, want 0", name, o, allocs)
			}
		}
	}
}

func TestScannerDropsLargeScratch(t *testing.T) {
	// Escaped names over 64 KiB are compared through scratch buffers that
	// Reset drops, so a reused Scanner never pins them (07 req 73).
	long := strings.Repeat(`\n`, 70<<10)
	in := []byte(`{"` + long + `a":1,"` + long + `b":2}`)
	s := NewScanner(in, ScanOptions{})
	if _, err := scanTokens(s, in, ScanOptions{}); err != nil {
		t.Fatal(err)
	}
	if cap(s.names.a) <= maxScratch && cap(s.names.b) <= maxScratch {
		t.Fatal("test did not grow the scratch buffers")
	}
	s.Reset(nil, ScanOptions{})
	if s.names.a != nil || s.names.b != nil || s.names.data != nil {
		t.Fatal("Reset kept large scratch or the input")
	}
}

func TestScannerReleasesNameScratch(t *testing.T) {
	// 07 req 73: Reset drops duplicate-name records, nesting-level slices
	// and index maps over the scratch bounds, so a reused Scanner never
	// pins memory sized by a large body; small scratch stays for reuse.
	var deepWide, veryDeep, veryDeepWide strings.Builder
	members := strings.TrimSuffix(strings.TrimPrefix(string(manyMembers(100)), "{"), "}")
	for range DefaultMaxDepth - 1 {
		deepWide.WriteString("{" + members + `,"x":`)
	}
	deepWide.WriteString("{}" + strings.Repeat("}", DefaultMaxDepth-1))
	sixteen := strings.TrimSuffix(strings.TrimPrefix(string(manyMembers(linearNames)), "{"), "}")
	const levels = maxScratchLevels + 100
	for range levels {
		veryDeep.WriteString(`{"a":`)
		veryDeepWide.WriteString("{" + sixteen + `,"x":`)
	}
	veryDeep.WriteString("1" + strings.Repeat("}", levels))
	veryDeepWide.WriteString("1" + strings.Repeat("}", levels))
	// A duplicate in the innermost of 30 open objects of 100 members: the
	// scan stops with every object open.
	openWide := strings.Repeat("{"+members+`,"x":`, 30)
	cases := []struct {
		name    string
		input   []byte
		o       ScanOptions
		wantErr error
	}{
		{"object of 200,000 members", manyMembers(200000), ScanOptions{}, nil},
		{"index room over all levels", []byte(deepWide.String()), ScanOptions{}, nil},
		{"nesting past the level bound", []byte(veryDeep.String()), ScanOptions{MaxDepth: MaxDepthLimit}, nil},
		{"indexes past the level bound", []byte(veryDeepWide.String()), ScanOptions{MaxDepth: MaxDepthLimit}, nil},
		{"duplicate with objects open", []byte(openWide + "{" + members + `,"k7":0}`), ScanOptions{}, ErrDuplicateName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := NewScanner(nil, c.o)
			if _, err := scanTokens(s, readOrders(t), c.o); err != nil {
				t.Fatal(err)
			}
			if _, err := scanTokens(s, c.input, c.o); !errors.Is(err, c.wantErr) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			grown := cap(s.names.recs) > maxScratchNames || cap(s.names.objs) > maxScratchLevels || cap(s.names.index) > maxScratchLevels
			rooms := 0
			for _, li := range s.names.index {
				rooms += li.room
			}
			if !grown && rooms <= maxScratchNames {
				t.Fatal("test did not grow the scratch past its bounds")
			}
			s.Reset(nil, ScanOptions{})
			checkNamesReleased(t, &s.names)
			if cap(s.stack) > MaxDepthLimit/64+1 {
				t.Fatalf("stack %d words", cap(s.stack))
			}
			// The Scanner still works after the release.
			if n, err := scanTokens(s, c.input[:0:0], c.o); err == nil || n != 0 {
				t.Fatalf("empty input: %d tokens, %v", n, err)
			}
			if _, err := scanTokens(s, manyMembers(300), ScanOptions{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
