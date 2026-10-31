// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package dotpath

import (
	"bytes"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// Tests for the Editor (docs/architecture/03-data-plane.md "Transform
// Policies": "Paths", "Operations", "JSON writing"): each operation on
// named documents, checked against the tree oracle too, its path errors,
// the output cap, normalization and the allocation budget.

// literal replaces every occurrence of old, which is not empty, with new,
// allocating nothing beyond the growth of dst.
type literal struct{ old, new []byte }

// lit returns the literal replacer of old with repl.
func lit(old, repl string) literal { return literal{[]byte(old), []byte(repl)} }

func (r literal) Replace(dst, s []byte) ([]byte, bool) {
	i := bytes.Index(s, r.old)
	if i < 0 {
		return append(dst, s...), false
	}
	for i >= 0 {
		dst = append(dst, s[:i]...)
		dst = append(dst, r.new...)
		s = s[i+len(r.old):]
		i = bytes.Index(s, r.old)
	}
	return append(dst, s...), true
}

// pattern replaces the matches of an RE2 pattern.
type pattern struct {
	re   *regexp.Regexp
	repl string
}

func (r pattern) Replace(dst, s []byte) ([]byte, bool) {
	if !r.re.Match(s) {
		return append(dst, s...), false
	}
	return append(dst, r.re.ReplaceAll(s, []byte(r.repl))...), true
}

// mustPath parses a path or stops the test.
func mustPath(tb testing.TB, s string) Path {
	tb.Helper()
	p, err := Parse(s)
	if err != nil {
		tb.Fatalf("Parse(%q) = %v", s, err)
	}
	return p
}

// opCase is one operation on a document.
type opCase struct {
	name    string
	doc     string
	path    string
	op      opKind
	payload string
	r       Replacer
	// want is the edited document; captured the captured values.
	want     string
	captured string
	n        int
	// reason is the path error's reason, 0 for none; segment its segment.
	reason  Reason
	segment int
}

// runEditor runs c on a fresh Editor and returns what it wrote.
func runEditor(tb testing.TB, e *Editor, c opCase) oracleResult {
	tb.Helper()
	p := mustPath(tb, c.path)
	doc := []byte(c.doc)
	var res oracleResult
	switch c.op {
	case opRemove:
		res.doc, res.n, res.err = e.Remove(nil, doc, p)
	case opExtract:
		res.doc, res.captured, res.n, res.err = e.Extract(nil, doc, p, nil)
	case opCollect:
		res.captured, res.n, res.err = e.Collect(doc, p, nil)
		res.doc = doc
	case opSet:
		res.doc, res.n, res.err = e.Set(nil, doc, p, []byte(c.payload))
	case opAppend:
		res.doc, res.err = e.Append(nil, doc, p, []byte(c.payload))
		if res.err == nil && !bytes.Equal(res.doc, doc) {
			res.n = 1
		}
	case opReplace:
		res.doc, res.n, res.err = e.Replace(nil, doc, p, c.r)
	default:
		tb.Fatalf("unknown op %d", c.op)
	}
	return res
}

// checkResult compares an outcome with the case.
func checkResult(t *testing.T, who string, c opCase, got oracleResult) {
	t.Helper()
	if c.reason != 0 {
		var pe *PathError
		if !errors.As(got.err, &pe) || !errors.Is(got.err, ErrPath) || pe.Reason != c.reason || pe.Segment != c.segment {
			t.Errorf("%s: error = %v, want %s at segment %d", who, got.err, c.reason, c.segment)
		}
		return
	}
	if got.err != nil {
		t.Fatalf("%s: error = %v", who, got.err)
	}
	if string(got.doc) != c.want {
		t.Errorf("%s: document = %s, want %s", who, got.doc, c.want)
	}
	if string(got.captured) != c.captured {
		t.Errorf("%s: captured = %s, want %s", who, got.captured, c.captured)
	}
	if c.op != opAppend && got.n != c.n {
		t.Errorf("%s: n = %d, want %d", who, got.n, c.n)
	}
}

// editorCases are the named operations.
func editorCases() []opCase {
	orders := `{"items":[{"sku":"a","q":1},{"sku":"b"}],"total":1.50E+02}`
	return []opCase{
		// Remove.
		{name: "remove a member", doc: `{"a":1,"internal":{"x":1},"b":2}`, path: "internal", op: opRemove, want: `{"a":1,"b":2}`, n: 1},
		{name: "remove the first member", doc: `{"a":1,"b":2}`, path: "a", op: opRemove, want: `{"b":2}`, n: 1},
		{name: "remove the only member", doc: `{"a":1}`, path: "a", op: opRemove, want: `{}`, n: 1},
		{name: "remove a nested member", doc: `{"a":{"b":1,"c":2}}`, path: "a.b", op: opRemove, want: `{"a":{"c":2}}`, n: 1},
		{name: "remove an element moves later ones up", doc: `{"items":[1,2,3]}`, path: "items.1", op: opRemove, want: `{"items":[1,3]}`, n: 1},
		{name: "remove the first element", doc: `{"items":[1,2,3]}`, path: "items.0", op: opRemove, want: `{"items":[2,3]}`, n: 1},
		{name: "remove the last element", doc: `{"items":[1,2,3]}`, path: "items.2", op: opRemove, want: `{"items":[1,2]}`, n: 1},
		{name: "remove through a star", doc: orders, path: "items.*.sku", op: opRemove, want: `{"items":[{"q":1},{}],"total":1.50E+02}`, n: 2},
		{name: "remove every element", doc: `{"items":[1,{"a":[2]},"x"]}`, path: "items.*", op: opRemove, want: `{"items":[]}`, n: 3},
		{name: "remove a missing member", doc: `{"a":1}`, path: "nope", op: opRemove, want: `{"a":1}`},
		{name: "remove below a missing member", doc: `{"a":1}`, path: "x.y", op: opRemove, want: `{"a":1}`},
		{name: "remove an index past the end", doc: `{"items":[1]}`, path: "items.9", op: opRemove, want: `{"items":[1]}`},
		{name: "remove a name on an array", doc: `{"items":[{"sku":1}]}`, path: "items.sku", op: opRemove, want: `{"items":[{"sku":1}]}`},
		{name: "remove through a star over an empty array", doc: `{"items":[]}`, path: "items.*.sku", op: opRemove, want: `{"items":[]}`},
		{name: "remove a numeric member of an object", doc: `{"0":"x","a":1}`, path: "0", op: opRemove, want: `{"a":1}`, n: 1},
		{name: "remove an escaped-dot member", doc: `{"a.b":1,"a":{"b":2}}`, path: `a\.b`, op: opRemove, want: `{"a":{"b":2}}`, n: 1},
		{name: "remove a member whose name is escaped", doc: `{"a\nb":1,"c":2}`, path: "a\nb", op: opRemove, want: `{"c":2}`, n: 1},
		{name: "remove keeps numbers and escapes", doc: `{"x":0.10,"s":"q\"\\","d":1}`, path: "d", op: opRemove, want: `{"x":0.10,"s":"q\"\\"}`, n: 1},
		{name: "remove through a scalar", doc: `{"a":1}`, path: "a.b", op: opRemove, reason: ThroughScalar, segment: 1},
		{name: "remove through null", doc: `{"a":null}`, path: "a.b", op: opRemove, reason: ThroughScalar, segment: 1},
		{name: "remove through a scalar element", doc: `{"items":[{"sku":1},2]}`, path: "items.*.sku", op: opRemove, reason: ThroughScalar, segment: 2},
		{name: "remove star on an object", doc: `{"a":{"x":1}}`, path: "a.*", op: opRemove, reason: StarOnNonArray, segment: 1},
		{name: "remove star on the root object", doc: `{"a":1}`, path: "*", op: opRemove, reason: StarOnNonArray, segment: 0},
		{name: "remove from a scalar document", doc: `"str"`, path: "a", op: opRemove, reason: ThroughScalar, segment: 0},
		// Extract and Collect.
		{name: "extract through a star", doc: orders, path: "items.*.sku", op: opExtract, want: `{"items":[{"q":1},{}],"total":1.50E+02}`, captured: `"a","b"`, n: 2},
		{name: "extract one value", doc: orders, path: "items.0", op: opExtract, want: `{"items":[{"sku":"b"}],"total":1.50E+02}`, captured: `{"sku":"a","q":1}`, n: 1},
		{name: "extract a missing value", doc: orders, path: "nope", op: opExtract, want: orders},
		{name: "collect through a star", doc: orders, path: "items.*.sku", op: opCollect, want: orders, captured: `"a","b"`, n: 2},
		{name: "collect a number verbatim", doc: orders, path: "total", op: opCollect, want: orders, captured: `1.50E+02`, n: 1},
		{name: "collect star on an object", doc: orders, path: "total.*", op: opCollect, reason: ThroughScalar, segment: 1},
		// Set.
		{name: "set an existing member", doc: `{"a":1,"b":2}`, path: "a", op: opSet, payload: `{"x":[1]}`, want: `{"a":{"x":[1]},"b":2}`, n: 1},
		{name: "set a new member at the end", doc: `{"a":1,"b":2}`, path: "c", op: opSet, payload: `3`, want: `{"a":1,"b":2,"c":3}`, n: 1},
		{name: "set creates missing objects", doc: `{"a":1}`, path: "x.y.z", op: opSet, payload: `true`, want: `{"a":1,"x":{"y":{"z":true}}}`, n: 1},
		{name: "set into an existing object", doc: `{"x":{"k":1}}`, path: "x.y", op: opSet, payload: `2`, want: `{"x":{"k":1,"y":2}}`, n: 1},
		{name: "set into an empty object", doc: `{}`, path: "a", op: opSet, payload: `"v"`, want: `{"a":"v"}`, n: 1},
		{name: "set an element", doc: `{"l":[1,2]}`, path: "l.1", op: opSet, payload: `9`, want: `{"l":[1,9]}`, n: 1},
		{name: "set through a star", doc: `{"items":[{"a":1},{}]}`, path: "items.*.cur", op: opSet, payload: `"EUR"`, want: `{"items":[{"a":1,"cur":"EUR"},{"cur":"EUR"}]}`, n: 2},
		{name: "set every element", doc: `{"l":[1,2]}`, path: "l.*", op: opSet, payload: `0`, want: `{"l":[0,0]}`, n: 2},
		{name: "set through a star over an empty array", doc: `{"items":[]}`, path: "items.*.x", op: opSet, payload: `1`, want: `{"items":[]}`},
		{name: "set creates nothing below a star", doc: `{}`, path: "items.*.x", op: opSet, payload: `1`, want: `{}`},
		{name: "set a numeric segment as a member name", doc: `{}`, path: "a.0", op: opSet, payload: `1`, want: `{"a":{"0":1}}`, n: 1},
		{name: "set a member with an escaped name", doc: `{}`, path: `a\.b."q"`, op: opSet, payload: `1`, want: `{"a.b":{"\"q\"":1}}`, n: 1},
		{name: "set an index past the end", doc: `{"l":[1,2]}`, path: "l.2", op: opSet, payload: `9`, reason: IndexPastEnd, segment: 1},
		{name: "set a name on an array", doc: `{"l":[1]}`, path: "l.x", op: opSet, payload: `9`, reason: NameOnArray, segment: 1},
		{name: "set a name on the root array", doc: `[1]`, path: "a", op: opSet, payload: `9`, reason: NameOnArray, segment: 0},
		{name: "set through a scalar", doc: `{"a":"s"}`, path: "a.b", op: opSet, payload: `1`, reason: ThroughScalar, segment: 1},
		{name: "set star on an object", doc: `{"a":{}}`, path: "a.*.b", op: opSet, payload: `1`, reason: StarOnNonArray, segment: 1},
		// Append.
		{name: "append to an array", doc: `{"tags":["x"]}`, path: "tags", op: opAppend, payload: `"y","z"`, want: `{"tags":["x","y","z"]}`},
		{name: "append to an empty array", doc: `{"tags":[]}`, path: "tags", op: opAppend, payload: `"y"`, want: `{"tags":["y"]}`},
		{name: "append creates the array and its objects", doc: `{"k":0}`, path: "a.tags", op: opAppend, payload: `1,{"b":[]}`, want: `{"k":0,"a":{"tags":[1,{"b":[]}]}}`},
		{name: "append to a nested array", doc: `{"m":[[1],[2]]}`, path: "m.1", op: opAppend, payload: `3`, want: `{"m":[[1],[2,3]]}`},
		{name: "append to a value that is not an array", doc: `{"tags":"x"}`, path: "tags", op: opAppend, payload: `1`, reason: NotArray, segment: 0},
		{name: "append at an index past the end", doc: `{"m":[[1]]}`, path: "m.1", op: opAppend, payload: `1`, reason: IndexPastEnd, segment: 1},
		// Replace.
		{name: "replace with a pattern", doc: `{"customer":{"email":"alice@example.com"}}`, path: "customer.email", op: opReplace, r: pattern{regexp.MustCompile(`^[^@]+`), "***"}, want: `{"customer":{"email":"***@example.com"}}`, n: 1},
		{name: "replace groups", doc: `{"d":"2026-10-30"}`, path: "d", op: opReplace, r: pattern{regexp.MustCompile(`(\d+)-(\d+)-(\d+)`), "$3/$2/$1"}, want: `{"d":"30/10/2026"}`, n: 1},
		{name: "replace through a star", doc: `{"l":["ab","cb","x"]}`, path: "l.*", op: opReplace, r: lit("b", "X"), want: `{"l":["aX","cX","x"]}`, n: 2},
		{name: "replace without a match", doc: `{"a":"x"}`, path: "a", op: opReplace, r: lit("q", "z"), want: `{"a":"x"}`},
		{name: "replace escapes its output", doc: `{"a":"x"}`, path: "a", op: opReplace, r: lit("x", "\"q\"\n"), want: `{"a":"\"q\"\n"}`, n: 1},
		{name: "replace inside an escaped string", doc: `{"a":"line\nbreak"}`, path: "a", op: opReplace, r: lit("\n", " "), want: `{"a":"line break"}`, n: 1},
		{name: "replace a missing value", doc: `{"a":"x"}`, path: "b", op: opReplace, r: lit("x", "y"), want: `{"a":"x"}`},
		{name: "replace a value that is not a string", doc: `{"a":1}`, path: "a", op: opReplace, r: lit("1", "2"), reason: NotString, segment: 0},
		{name: "replace an element that is not a string", doc: `{"l":["a",2]}`, path: "l.*", op: opReplace, r: lit("a", "b"), reason: NotString, segment: 1},
	}
}

// TestEditorOperations runs every named operation on the Editor and on the
// oracle.
func TestEditorOperations(t *testing.T) {
	var e Editor
	for _, c := range editorCases() {
		t.Run(c.name, func(t *testing.T) {
			checkResult(t, "editor", c, runEditor(t, &e, c))
			checkResult(t, "oracle", c, oracleRun(t, []byte(c.doc), mustPath(t, c.path), c.op, []byte(c.payload), c.r))
		})
	}
}

// TestEditorAppendsToPrefix checks that an operation appends to dst after
// its existing bytes, which it leaves alone and never reads as part of
// the document.
func TestEditorAppendsToPrefix(t *testing.T) {
	var e Editor
	out, n, err := e.Remove([]byte(`prefix,`), []byte(`[1,2]`), mustPath(t, "0"))
	if err != nil || n != 1 || string(out) != `prefix,[2]` {
		t.Errorf("Remove after a prefix = %s, %d, %v; want prefix,[2]", out, n, err)
	}
	out, _, err = e.Set([]byte(`{`), []byte(`{}`), mustPath(t, "a"), []byte(`1`))
	if err != nil || string(out) != `{{"a":1}` {
		t.Errorf("Set after a prefix = %s, %v", out, err)
	}
}

// TestEditorLimit checks the output cap: a write past Limit stops with
// ErrOutputCap, also in Normalize, and an output at the limit passes.
func TestEditorLimit(t *testing.T) {
	e := Editor{Limit: 16}
	if _, _, err := e.Set(nil, []byte(`{}`), mustPath(t, "a"), []byte(`"012345678"`)); !errors.Is(err, ErrOutputCap) {
		t.Errorf("Set past the limit = %v, want ErrOutputCap", err)
	}
	if out, _, err := e.Set(nil, []byte(`{}`), mustPath(t, "a"), []byte(`"01234567"`)); err != nil || len(out) != 16 {
		t.Errorf("Set at the limit = %s, %v", out, err)
	}
	if _, _, err := e.Set(nil, []byte(`{"l":[{},{}]}`), mustPath(t, "l.*.x"), []byte(`1`)); !errors.Is(err, ErrOutputCap) {
		t.Errorf("Set through a star past the limit = %v, want ErrOutputCap", err)
	}
	if _, err := e.Normalize(nil, []byte(`{"a":"0123456789abcdef"}`)); !errors.Is(err, ErrOutputCap) {
		t.Errorf("Normalize past the limit = %v, want ErrOutputCap", err)
	}
	if _, err := e.Append(nil, []byte(`{"l":[]}`), mustPath(t, "l"), []byte(`"0123456789"`)); !errors.Is(err, ErrOutputCap) {
		t.Errorf("Append past the limit = %v, want ErrOutputCap", err)
	}
	if _, _, err := e.Replace(nil, []byte(`{"a":"x"}`), mustPath(t, "a"), lit("x", "0123456789")); !errors.Is(err, ErrOutputCap) {
		t.Errorf("Replace past the limit = %v, want ErrOutputCap", err)
	}
}

// TestNormalize checks the transform form and the strict JSON check: a
// body that CEL reads as null (invalid JSON, a duplicate member name,
// invalid UTF-8, nesting past 64 levels) is ErrNotJSON.
func TestNormalize(t *testing.T) {
	var e Editor
	got, err := e.Normalize(nil, []byte(" {\"b\" : [ 1 , \"\\u0041\" ] , \"a\" : 1.50E+02 } "))
	if err != nil || string(got) != `{"b":[1,"A"],"a":1.50E+02}` {
		t.Errorf("Normalize = %s, %v", got, err)
	}
	for name, in := range map[string]string{
		"invalid":   `{"a":`,
		"duplicate": `{"a":1,"a":2}`,
		"utf8":      "\"\xff\"",
		"deep":      strings.Repeat("[", 65) + strings.Repeat("]", 65),
		"empty":     ``,
		"two texts": `1 2`,
	} {
		if _, err := e.Normalize(nil, []byte(in)); !errors.Is(err, ErrNotJSON) {
			t.Errorf("Normalize(%s) = %v, want ErrNotJSON", name, err)
		}
		if e.Valid([]byte(in)) {
			t.Errorf("Valid(%s) = true", name)
		}
	}
	if !e.Valid([]byte(` {"a": [1, "x"]} `)) || !e.Valid([]byte(strings.Repeat("[", 64)+strings.Repeat("]", 64))) {
		t.Error("Valid refused strict JSON")
	}
}

// TestPathErrorText checks that a path error names the reason and segment
// and never document data.
func TestPathErrorText(t *testing.T) {
	err := &PathError{Reason: NotString, Segment: 2}
	if got := err.Error(); got != "dotpath: value is not a string at segment 2" {
		t.Errorf("Error() = %q", got)
	}
	for r := ThroughScalar; r <= NotArray; r++ {
		if strings.HasPrefix(r.String(), "reason") {
			t.Errorf("Reason %d has no description", r)
		}
	}
}

// ordersDoc is a 1 KiB order document, the body of the orders-shape
// example (docs/architecture/03-data-plane.md "Transform Policies").
func ordersDoc() []byte {
	var b strings.Builder
	b.WriteString(`{"id":"ord-42","status":"paid","customer":{"name":"Alice Example","email":"alice@example.com","tier":"gold"},`)
	b.WriteString(`"internal":{"warehouse":"w-7","margin":0.31,"notes":"fragile"},"items":[`)
	for i := range 8 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`{"sku":"sku-0000`)
		b.WriteByte(byte('0' + i))
		b.WriteString(`","name":"Item number with a longer description","qty":2,"price":12.50,"tags":["a","b"]}`)
	}
	b.WriteString(`],"total":200.00,"currency":"EUR"}`)
	return []byte(b.String())
}

// TestEditorAllocations checks the allocation budget of the literal
// operations (docs/architecture/12-performance-budgets-and-benchmarking.md
// "Per-stage latency budget": 0 for remove, replace and arrayOps over
// pooled buffers): with buffers grown once, normalizing a 1 KiB order and
// moving, appending, deleting and literally replacing in it allocate
// nothing.
func TestEditorAllocations(t *testing.T) {
	var e Editor
	doc := ordersDoc()
	bufA, bufB := make([]byte, 0, 4096), make([]byte, 0, 4096)
	val, capt := make([]byte, 0, 1024), make([]byte, 0, 1024)
	skus, to, internal := mustPath(t, "items.*.sku"), mustPath(t, "skus"), mustPath(t, "internal")
	email, tags := mustPath(t, "customer.email"), mustPath(t, "items.0.tags")
	var repl Replacer = lit("alice", "*****") // boxed once, as a Filter keeps it
	elems := []byte(`"c"`)
	run := func() {
		cur, err := e.Normalize(bufA[:0], doc)
		if err != nil {
			t.Fatal(err)
		}
		// move from items.*.sku to skus: extract, then set the array.
		next, captured, _, err := e.Extract(bufB[:0], cur, skus, capt[:0])
		if err != nil {
			t.Fatal(err)
		}
		v := append(append(append(val[:0], '['), captured...), ']')
		if cur, _, err = e.Set(bufA[:0], next, to, v); err != nil {
			t.Fatal(err)
		}
		if next, _, err = e.Remove(bufB[:0], cur, internal); err != nil {
			t.Fatal(err)
		}
		if cur, _, err = e.Replace(bufA[:0], next, email, repl); err != nil {
			t.Fatal(err)
		}
		if _, err = e.Append(bufB[:0], cur, tags, elems); err != nil {
			t.Fatal(err)
		}
	}
	run()
	if n := testing.AllocsPerRun(50, run); n != 0 {
		t.Errorf("the literal operations allocate %v per run, want 0", n)
	}
}

// FuzzEditorMatchesOracle checks every operation on any normalizable
// document and parsable path against the tree oracle: the same document,
// captured values, count and path error.
func FuzzEditorMatchesOracle(f *testing.F) {
	for _, c := range editorCases() {
		f.Add([]byte(c.doc), c.path, byte(c.op), byte(0))
	}
	f.Add(ordersDoc(), "items.*.tags.0", byte(opRemove), byte(1))
	f.Add([]byte(`{"a":[{"b":[1,2]},{"b":[]}]}`), "a.*.b", byte(opAppend), byte(2))
	payloads := []string{`1`, `"s"`, `{"k":[true,null]}`, `[]`, `"x","y"`}
	var e Editor
	f.Fuzz(func(t *testing.T, raw []byte, path string, opByte, payloadByte byte) {
		doc, err := e.Normalize(nil, raw)
		if err != nil {
			return
		}
		p, err := Parse(path)
		if err != nil {
			return
		}
		c := opCase{doc: string(doc), path: path, op: opKind(opByte%6) + 1, r: lit("a", "b\"")}
		c.payload = payloads[int(payloadByte)%len(payloads)]
		if c.op == opSet && strings.Contains(c.payload, `","`) {
			c.payload = `"x"`
		}
		if c.op == opAppend && p.HasStar() {
			return // to never holds a star (RZ-CFG-005)
		}
		got := runEditor(t, &e, c)
		want := oracleRun(t, doc, p, c.op, []byte(c.payload), c.r)
		var gpe, wpe *PathError
		switch {
		case errors.As(want.err, &wpe):
			if !errors.As(got.err, &gpe) || *gpe != *wpe {
				t.Fatalf("doc %s path %q op %d: editor error %v, oracle %v", doc, path, c.op, got.err, want.err)
			}
			return
		case got.err != nil:
			t.Fatalf("doc %s path %q op %d: editor error %v, oracle none", doc, path, c.op, got.err)
		}
		if !bytes.Equal(got.doc, want.doc) || !bytes.Equal(got.captured, want.captured) || (c.op != opAppend && got.n != want.n) {
			t.Fatalf("doc %s path %q op %d payload %s:\neditor %s [%s] %d\noracle %s [%s] %d",
				doc, path, c.op, c.payload, got.doc, got.captured, got.n, want.doc, want.captured, want.n)
		}
		// The edited document is in the transform form.
		if again, err := e.Normalize(nil, got.doc); err != nil && !errors.Is(err, ErrNotJSON) || err == nil && !bytes.Equal(again, got.doc) {
			t.Fatalf("edited document %s is not in the transform form: %s, %v", got.doc, again, err)
		}
	})
}
