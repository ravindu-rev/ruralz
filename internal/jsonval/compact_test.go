// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// Tests for the transform form: Compactor (compact, members in input order,
// minimal escaping, numbers verbatim, the Scanner's checks, no allocation
// after warm-up) and AppendStringBytes.

// jsonEsc returns s with every "~u" turned into a JSON \u escape prefix, so
// test tables spell \u escapes without holding them in the source.
func jsonEsc(s string) string { return strings.ReplaceAll(s, "~u", string(rune(0x5c))+"u") }

// TestCompactorTransformForm checks the bytes Compactor writes.
func TestCompactorTransformForm(t *testing.T) {
	cases := []struct {
		name, input, want string
	}{
		{"whitespace dropped", " { \"a\" : [ 1 , 2 ] ,\n\t\"b\" : { } } ", `{"a":[1,2],"b":{}}`},
		{"member order kept", `{"b":1,"a":{"d":[true,null],"c":"x"},"B":2}`, `{"b":1,"a":{"d":[true,null],"c":"x"},"B":2}`},
		{"numbers verbatim", `[1.50E+02, -0, 1e400, 12345678901234567890123, 0.10]`, `[1.50E+02,-0,1e400,12345678901234567890123,0.10]`},
		{"escapes minimized", jsonEsc(`"~u0041\/~u00e9~ud83d~ude00~u2028"`), "\"A/\xc3\xa9\xf0\x9f\x98\x80\xe2\x80\xa8\""},
		{"control characters stay escaped", jsonEsc(`"~u0001~u001f\b\t\n\f\r\"\\"`), jsonEsc(`"~u0001~u001f\b\t\n\f\r\"\\"`)},
		{"no HTML escaping", jsonEsc(`"<a href=~u0022x~u0022>&amp;</a>"`), `"<a href=\"x\">&amp;</a>"`},
		{"escaped names minimized", jsonEsc(`{"~u0061\n":1}`), `{"a\n":1}`},
		{"empty containers", `[ { } , [ ] , "" ]`, `[{},[],""]`},
		{"top-level scalar", ` true `, `true`},
		{"top-level string", ` "x" `, `"x"`},
		{"nested arrays of objects", `[[{"a":[{}]}],[]]`, `[[{"a":[{}]}],[]]`},
	}
	var c Compactor
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := c.Append([]byte("prefix:"), []byte(tc.input))
			if err != nil || string(got) != "prefix:"+tc.want {
				t.Fatalf("Append(%q) = %q, %v; want %q", tc.input, got, err, "prefix:"+tc.want)
			}
			again, err := c.Append(nil, []byte(tc.want))
			if err != nil || string(again) != tc.want {
				t.Fatalf("Append of the transform form %q = %q, %v; want it unchanged", tc.want, again, err)
			}
		})
	}
}

// TestCompactorMatchesInsertionEncoder checks that Compactor writes exactly
// what an Encoder in OrderInsertion writes for the decoded tree, for every
// conformance case the strict decoder accepts, and fails where it fails.
func TestCompactorMatchesInsertionEncoder(t *testing.T) {
	c := Compactor{Options: ScanOptions{MaxDepth: MaxDepthLimit}}
	e := Encoder{Options: EncodeOptions{Order: OrderInsertion}}
	for _, tc := range conformanceCases() {
		v, _, decErr := Decode([]byte(tc.input), Options{MaxDepth: MaxDepthLimit})
		got, err := c.Append(nil, []byte(tc.input))
		if decErr != nil {
			if err == nil {
				t.Errorf("%s: Compactor accepted what Decode refuses (%v)", tc.name, decErr)
			}
			continue
		}
		want, encErr := e.Append(nil, v)
		if encErr != nil {
			t.Fatalf("%s: encoder: %v", tc.name, encErr)
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: Compactor = %q, %v; insertion encoder %q", tc.name, got, err, want)
		}
	}
}

// TestCompactorChecks checks that Compactor refuses what its Scanner
// refuses: invalid JSON, a duplicate member name unless allowed, nesting
// past the depth limit, and trailing data.
func TestCompactorChecks(t *testing.T) {
	deep := strings.Repeat("[", DefaultMaxDepth+1) + strings.Repeat("]", DefaultMaxDepth+1)
	cases := []struct {
		name  string
		input string
		err   error
	}{
		{"syntax", `{"a":}`, ErrSyntax},
		{"invalid UTF-8", "\"\xff\"", ErrInvalidUTF8},
		{"unpaired surrogate", jsonEsc(`"~ud800"`), ErrSurrogate},
		{"duplicate name", `{"a":1,"a":2}`, ErrDuplicateName},
		{"depth", deep, ErrDepth},
		{"trailing data", `{} {}`, ErrSyntax},
		{"empty", ``, ErrSyntax},
	}
	for _, tc := range cases {
		var c Compactor
		if _, err := c.Append(nil, []byte(tc.input)); !errors.Is(err, tc.err) {
			t.Errorf("%s: Append(%q) = %v, want %v", tc.name, tc.input, err, tc.err)
		}
	}
	c := Compactor{Options: ScanOptions{AllowDuplicateNames: true}}
	if got, err := c.Append(nil, []byte(`{"a":1, "a":2}`)); err != nil || string(got) != `{"a":1,"a":2}` {
		t.Errorf("Append with AllowDuplicateNames = %q, %v", got, err)
	}
	c = Compactor{Options: ScanOptions{MaxDepth: DefaultMaxDepth + 1}}
	if _, err := c.Append(nil, []byte(deep)); err != nil {
		t.Errorf("Append within a raised depth limit = %v", err)
	}
}

// TestCompactorZeroAllocs checks that a reused Compactor writing into a
// large enough buffer allocates nothing after warm-up, escaped strings
// included.
func TestCompactorZeroAllocs(t *testing.T) {
	in := []byte(`[` + string(readOrders(t)) + jsonEsc(`, {"esc~u0041ped": "~u00e9\n", "list": [1, 2.50, {"x": null}]} ]`))
	var c Compactor
	buf := make([]byte, 0, 2*len(in))
	if _, err := c.Append(buf, in); err != nil {
		t.Fatal(err)
	}
	if n := testing.AllocsPerRun(50, func() {
		if _, err := c.Append(buf[:0], in); err != nil {
			t.Fatal(err)
		}
	}); n != 0 {
		t.Errorf("Append allocates %v per run, want 0", n)
	}
}

// TestCompactorReleasesScratch checks that a Compactor drops an unescape
// buffer over the scratch bound, and the input, after the call.
func TestCompactorReleasesScratch(t *testing.T) {
	var c Compactor
	big := `"` + strings.Repeat(jsonEsc(`~u0041`), maxScratch) + `"`
	if _, err := c.Append(nil, []byte(big)); err != nil {
		t.Fatal(err)
	}
	if cap(c.buf) > maxScratch || c.s.data != nil {
		t.Errorf("after a large text: buffer capacity %d, input kept %v", cap(c.buf), c.s.data != nil)
	}
}

// TestAppendStringBytes checks that AppendStringBytes writes what
// AppendString writes, and refuses invalid UTF-8 likewise.
func TestAppendStringBytes(t *testing.T) {
	for _, in := range []string{
		"", "plain", "quote\" backslash\\", "\x00\x01\x1f\x7f", "\b\t\n\f\r",
		"\xc3\xa9\xf0\x9f\x98\x80\xe2\x80\xa8\xe2\x80\xa9", "</script>&",
	} {
		want, werr := AppendString([]byte("p"), in)
		got, err := AppendStringBytes([]byte("p"), []byte(in))
		if err != nil || werr != nil || !bytes.Equal(got, want) {
			t.Errorf("AppendStringBytes(%q) = %q, %v; AppendString %q, %v", in, got, err, want, werr)
		}
	}
	if _, err := AppendStringBytes(nil, []byte("a\xffb")); !errors.Is(err, ErrInvalidUTF8) {
		t.Errorf("AppendStringBytes(invalid UTF-8) = %v, want ErrInvalidUTF8", err)
	}
	src := []byte("\xc3\xa9\n\"x")
	buf := make([]byte, 0, 64)
	if n := testing.AllocsPerRun(50, func() {
		_, _ = AppendStringBytes(buf[:0], src)
	}); n != 0 {
		t.Errorf("AppendStringBytes allocates %v per run, want 0", n)
	}
}

// FuzzCompactor checks Compactor against the strict decoder and the
// insertion encoder on any input: both accept or refuse alike, the output
// is the encoder's bytes for the decoded tree, and it is a fixed point.
func FuzzCompactor(f *testing.F) {
	for _, tc := range conformanceCases() {
		f.Add([]byte(tc.input))
	}
	f.Add([]byte(jsonEsc(` {"b" : [1 , "~u00e9"], "a":{}} `)))
	var c Compactor
	e := Encoder{Options: EncodeOptions{Order: OrderInsertion}}
	f.Fuzz(func(t *testing.T, data []byte) {
		v, _, decErr := Decode(data, Options{})
		got, err := c.Append(nil, data)
		if (decErr == nil) != (err == nil) {
			t.Fatalf("Decode error %v, Compactor error %v", decErr, err)
		}
		if err != nil {
			return
		}
		want, encErr := e.Append(nil, v)
		if encErr != nil || !bytes.Equal(got, want) {
			t.Fatalf("Compactor %q, insertion encoder %q (%v)", got, want, encErr)
		}
		again, err := c.Append(nil, got)
		if err != nil || !bytes.Equal(again, got) {
			t.Fatalf("second pass %q, %v; want %q", again, err, got)
		}
	})
}
