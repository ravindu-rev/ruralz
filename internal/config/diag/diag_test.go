// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package diag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// Tests for architecture section 2.4 (WP-01) and spec 01 group M:
// requirement 47 (model), 48 (paths), 49 (text form), 50 (JSON form, sort,
// cap). FuzzHumanPath is spec 02 section 6 item 35 (R-4: diag.Path is the
// only path type).

func golden(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name)) //nolint:gosec // G304: the test reads its own golden files.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// cmExample is the pair of diagnostics of docs/architecture/02-configuration-model.md
// "Diagnostics and source map".
func cmExample() List {
	return List{
		{
			Code: "RZ-CFG-009", Severity: SeverityError,
			Location: Location{File: "routes/orders-summary.yaml", Line: 23, Column: 19},
			Resource: &ResourceID{Kind: "Route", Name: "orders-summary"},
			Path:     Path{Field("spec"), Field("composition"), Field("steps"), Keyed("name", "stock"), Field("upstream")},
			Message:  `Upstream "inventroy" not found`,
			Hint:     `did you mean "inventory"?`,
		},
		{
			Code: "RZ-CFG-019", Severity: SeverityError,
			Location: Location{File: "routes/cart-grpc.yaml", Line: 11, Column: 7},
			Resource: &ResourceID{Kind: "Route", Name: "cart-grpc"},
			Path:     Path{Field("spec"), Field("excludePolicies"), Keyed("name", "ratelimit-global")},
			Message:  "cannot exclude a Policy with overridable: false",
			Related:  []Related{{Location: Location{File: "policies/common.yaml", Line: 46, Column: 3}, Message: "declared in"}},
		},
	}
}

func TestGoldenText(t *testing.T) {
	// 01 req 49: the CM example lines, byte for byte.
	var buf bytes.Buffer
	if err := WriteText(&buf, cmExample()); err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "cm-example.txt"); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("WriteText =\n%s\nwant\n%s", buf.Bytes(), want)
	}
}

func TestGoldenJSON(t *testing.T) {
	// 01 req 50: bare array, member order, two-space indent, final newline.
	var buf bytes.Buffer
	if err := WriteJSON(&buf, cmExample()); err != nil {
		t.Fatal(err)
	}
	if want := golden(t, "cm-example.json"); !bytes.Equal(buf.Bytes(), want) {
		t.Fatalf("WriteJSON =\n%s\nwant\n%s", buf.Bytes(), want)
	}
}

func TestWriteJSONEmptyAndEscaping(t *testing.T) {
	for _, l := range []List{nil, {}} {
		var buf bytes.Buffer
		if err := WriteJSON(&buf, l); err != nil {
			t.Fatal(err)
		}
		if buf.String() != "[]\n" {
			t.Fatalf("WriteJSON(empty) = %q, want \"[]\\n\"", buf.String())
		}
	}
	var buf bytes.Buffer
	err := WriteJSON(&buf, List{{Code: "RZ-CFG-013", Severity: SeverityWarning, Message: "<a> & <b>", Environment: "prod"}})
	if err != nil {
		t.Fatal(err)
	}
	want := "[\n  {\n    \"code\": \"RZ-CFG-013\",\n    \"severity\": \"warning\",\n    \"message\": \"<a> & <b>\",\n    \"environment\": \"prod\"\n  }\n]\n"
	if buf.String() != want {
		t.Fatalf("WriteJSON = %q, want %q (no HTML escaping, empty members omitted)", buf.String(), want)
	}

	// Paths follow the same rule as every other member: no HTML escaping.
	buf.Reset()
	err = WriteJSON(&buf, List{{Code: "RZ-CFG-005", Severity: SeverityError, Path: Path{Field("a<b"), Keyed("name", "x&y"), Item(">")}, Message: "m"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, esc := range []string{`\u003c`, `\u003e`, `\u0026`} {
		if strings.Contains(buf.String(), esc) {
			t.Fatalf("WriteJSON HTML-escaped a path: %s", buf.String())
		}
	}
}

func TestSeverity(t *testing.T) {
	tests := []struct {
		s    Severity
		want string
	}{{SeverityError, "error"}, {SeverityWarning, "warning"}, {0, "error"}}
	for _, tt := range tests {
		if got := tt.s.String(); got != tt.want {
			t.Errorf("Severity(%d).String() = %q, want %q", tt.s, got, tt.want)
		}
		b, err := tt.s.MarshalText()
		if err != nil || string(b) != tt.want {
			t.Errorf("Severity(%d).MarshalText() = %q, %v", tt.s, b, err)
		}
	}
}

func TestPathString(t *testing.T) {
	// 01 req 48: fields joined by ".", non-identifier fields ["<json>"],
	// keyed [k=v], set items [item=v], atomic [n]; values that are not
	// bare are JSON-quoted.
	tests := []struct {
		name string
		p    Path
		want string
	}{
		{"empty", nil, ""},
		{"fields", Path{Field("spec"), Field("routes")}, "spec.routes"},
		{"identifier characters", Path{Field("_a$b-c9"), Field("$ref")}, "_a$b-c9.$ref"},
		{"field starting with a digit", Path{Field("spec"), Field("9lives")}, `spec["9lives"]`},
		{"field with a dot", Path{Field("metadata"), Field("labels"), Field("ruralz.io/tier")}, `metadata.labels["ruralz.io/tier"]`},
		{"leading non-identifier field", Path{Field("a b"), Field("c")}, `["a b"].c`},
		{"empty field", Path{Field("")}, `[""]`},
		{"field with a quote", Path{Field(`a"b`)}, `["a\"b"]`},
		{"index", Path{Field("spec"), Field("hosts"), Index(0), Field("x")}, "spec.hosts[0].x"},
		{"keyed bare", Path{Field("routes"), Keyed("name", "checkout")}, "routes[name=checkout]"},
		{"keyed bare punctuation", Path{Keyed("name", "a_b./:@*-9")}, "[name=a_b./:@*-9]"},
		{"keyed quoted", Path{Keyed("name", "a b")}, `[name="a b"]`},
		{"keyed empty", Path{Keyed("name", "")}, `[name=""]`},
		{"keyed html", Path{Keyed("name", "<x>")}, `[name="<x>"]`},
		{"item string", Path{Field("capabilities"), Item("request.body.read")}, "capabilities[item=request.body.read]"},
		{"item quoted", Path{Item("GET /x")}, `[item="GET /x"]`},
		{"item number", Path{Item(json.Number("42"))}, "[item=42]"},
		{"item bool", Path{Item(true)}, "[item=true]"},
		{"item object", Path{Item(json.RawMessage(`{"a":1}`))}, `[item={"a":1}]`},
		{"item unencodable", Path{Item(func() {})}, "[item=?]"},
		{"item nil", Path{Item(nil)}, "[item=null]"},
		{"example", Path{Field("spec"), Field("composition"), Field("steps"), Keyed("name", "stock"), Field("upstream")}, "spec.composition.steps[name=stock].upstream"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.String(); got != tt.want {
				t.Fatalf("String = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestPathStringFieldIsJSON covers 01 req 48: a field that is not an
// identifier is written ["<json string>"], so the bracket contents are valid
// JSON decoding to the field name, quoted like keyed values, for control
// characters, DEL, invalid UTF-8 (U+FFFD, as encoding/json writes it),
// astral non-printables and line separators too. Such names reach paths
// from JSON files or YAML double-quoted escapes (unknown fields, labels).
func TestPathStringFieldIsJSON(t *testing.T) {
	tests := []struct {
		name  string
		field string
		// want is the exact form; "" leaves the bytes to the encoder (an
		// invalid byte is written as U+FFFD or as its \ufffd escape,
		// depending on the encoding/json implementation) and checks only
		// validity and the decoded string.
		want string
		// decoded is the string the bracket contents decode to.
		decoded string
	}{
		{"control character", "a\x01", `["a\u0001"]`, "a\x01"},
		{"bell", "a\a", `["a\u0007"]`, "a\a"},
		{"DEL", "a\x7f", "[\"a\x7f\"]", "a\x7f"},
		{"invalid UTF-8", "a\xff", "", "a\ufffd"},
		{"astral non-printable", "a\U000e0001", "[\"a\U000e0001\"]", "a\U000e0001"},
		{"line separator", "a\u2028", `["a\u2028"]`, "a\u2028"},
		{"newline", "a\nb", `["a\nb"]`, "a\nb"},
		{"html", "<&>", `["<&>"]`, "<&>"},
		{"quote and backslash", `a"\b`, `["a\"\\b"]`, `a"\b`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Path{Field(tt.field)}.String()
			if tt.want != "" && got != tt.want {
				t.Fatalf("String = %s, want %s", got, tt.want)
			}
			inner, ok := strings.CutPrefix(got, "[")
			inner, ok2 := strings.CutSuffix(inner, "]")
			if !ok || !ok2 || !json.Valid([]byte(inner)) {
				t.Fatalf("bracket contents %q are not valid JSON", inner)
			}
			var dec string
			if err := json.Unmarshal([]byte(inner), &dec); err != nil || dec != tt.decoded {
				t.Fatalf("bracket contents decode to %q (%v), want %q", dec, err, tt.decoded)
			}
			// The same string quotes alike as a field and as a keyed value.
			keyed := Path{Keyed("name", tt.field)}.String()
			if want := "[name=" + inner + "]"; keyed != want {
				t.Fatalf("keyed form %s, want %s", keyed, want)
			}
		})
	}
}

func TestPathMarshalJSON(t *testing.T) {
	p := Path{
		Field("spec"), Field("steps"), Keyed("name", "stock"), Index(2),
		Item("request.body.read"), Item(json.Number("1.5")), Item(false), Item(json.RawMessage(`{"b":[1,2]}`)),
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `["spec","steps",{"name":"stock"},2,{"item":"request.body.read"},{"item":1.5},{"item":false},{"item":{"b":[1,2]}}]`
	if string(b) != want {
		t.Fatalf("MarshalJSON = %s, want %s", b, want)
	}
	if b, _ := json.Marshal(Path{}); string(b) != "[]" {
		t.Fatalf("empty path = %s, want []", b)
	}
}

func TestPathAppend(t *testing.T) {
	base := make(Path, 1, 8)
	base[0] = Field("spec")
	a := base.Append(Field("a"))
	b := base.Append(Field("b"), Index(1))
	if len(base) != 1 {
		t.Fatalf("Append modified its receiver: %v", base)
	}
	if a.String() != "spec.a" || b.String() != "spec.b[1]" {
		t.Fatalf("Append aliased: a=%s b=%s", a, b)
	}
}

func TestAppendText(t *testing.T) {
	// 01 req 49: absent parts are omitted; "-" for no file; <file> alone
	// when the line is unknown; messages are single-line.
	res := &ResourceID{Kind: "Gateway", Name: "main"}
	tests := []struct {
		name string
		d    Diagnostic
		want string
	}{
		{"no file", Diagnostic{Code: "RZ-CFG-016", Severity: SeverityError, Message: "no Gateway"}, "- error RZ-CFG-016: no Gateway"},
		{"file only", Diagnostic{Code: "RZ-CFG-001", Severity: SeverityError, Location: Location{File: "a.yaml", Column: 3}, Message: "m"}, "a.yaml error RZ-CFG-001: m"},
		{"file and line", Diagnostic{Code: "RZ-CFG-001", Severity: SeverityError, Location: Location{File: "a.yaml", Line: 4}, Message: "m"}, "a.yaml:4 error RZ-CFG-001: m"},
		{"resource without path", Diagnostic{Code: "RZ-CFG-016", Severity: SeverityError, Location: Location{File: "ruralz.yaml", Line: 1, Column: 1}, Resource: res, Message: "m"}, "ruralz.yaml:1:1 error RZ-CFG-016 Gateway/main: m"},
		{"path without resource", Diagnostic{Code: "RZ-CFG-005", Severity: SeverityError, Path: Path{Field("spec")}, Message: "m"}, "- error RZ-CFG-005 spec: m"},
		{"warning with environment", Diagnostic{Code: "RZ-CFG-025", Severity: SeverityWarning, Message: "deprecated", Environment: "prod"}, "- warning RZ-CFG-025: deprecated [environment=prod]"},
		{"newlines escaped", Diagnostic{Code: "RZ-CFG-014", Severity: SeverityError, Message: "a\nb\r\nc", Hint: "h\nh", Related: []Related{{Message: "r\nr"}}}, `- error RZ-CFG-014: a\nb\r\nc (h\nh) (r\nr -)`},
		{"two related", Diagnostic{Code: "RZ-CFG-008", Severity: SeverityError, Message: "duplicate", Related: []Related{
			{Location: Location{File: "a.yaml", Line: 1, Column: 1}, Message: "first in"},
			{Location: Location{File: "b.yaml", Line: 2}, Message: "also in"},
		}}, "- error RZ-CFG-008: duplicate (first in a.yaml:1:1) (also in b.yaml:2)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := string(tt.d.AppendText(nil))
			if got != tt.want {
				t.Fatalf("AppendText =\n%s\nwant\n%s", got, tt.want)
			}
			if strings.ContainsAny(got, "\r\n") {
				t.Fatalf("text form spans lines: %q", got)
			}
		})
	}
}

type failWriter struct{ n int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.n == 0 {
		return 0, errors.New("disk full")
	}
	w.n--
	return len(p), nil
}

func TestWriteTextError(t *testing.T) {
	w := &failWriter{n: 1}
	if err := WriteText(w, cmExample()); err == nil || err.Error() != "disk full" {
		t.Fatalf("WriteText = %v, want the writer's error", err)
	}
	if err := WriteJSON(&failWriter{}, cmExample()); err == nil {
		t.Fatal("WriteJSON ignored the writer's error")
	}
}

func TestSortOrder(t *testing.T) {
	// 01 req 50: (environment, file, line, column, code, path, message).
	d := func(env, file string, line, col int, code string, path Path, msg string) Diagnostic {
		return Diagnostic{Environment: env, Location: Location{File: file, Line: line, Column: col}, Code: code, Path: path, Message: msg, Severity: SeverityError}
	}
	want := List{
		d("", "", 0, 0, "RZ-CFG-016", nil, "m"),
		d("", "a.yaml", 1, 1, "RZ-CFG-005", nil, "m"),
		d("", "a.yaml", 1, 2, "RZ-CFG-005", nil, "m"),
		d("", "a.yaml", 2, 1, "RZ-CFG-002", nil, "m"),
		d("", "a.yaml", 2, 1, "RZ-CFG-005", Path{Field("a")}, "m"),
		d("", "a.yaml", 2, 1, "RZ-CFG-005", Path{Field("b")}, "a"),
		d("", "a.yaml", 2, 1, "RZ-CFG-005", Path{Field("b")}, "b"),
		d("", "b.yaml", 1, 1, "RZ-CFG-001", nil, "m"),
		d("dev", "a.yaml", 1, 1, "RZ-CFG-001", nil, "m"),
		d("prod", "", 0, 0, "RZ-CFG-001", nil, "m"),
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	got[0], got[3] = got[3], got[0]
	got.Sort()
	for i := range want {
		if string(got[i].AppendText(nil)) != string(want[i].AppendText(nil)) {
			t.Fatalf("position %d: %s, want %s", i, got[i].AppendText(nil), want[i].AppendText(nil))
		}
	}
}

func TestHasErrorsAndFirstErrorCode(t *testing.T) {
	// 01 req 47 and 51: only warnings (RZ-CFG-013, -025) let a Revision
	// through; every other severity, the zero value included, is an error,
	// exactly as Severity.String prints it.
	warn := Diagnostic{Code: "RZ-CFG-013", Severity: SeverityWarning}
	tests := []struct {
		name      string
		l         List
		hasErrors bool
		first     string
	}{
		{"empty", List{}, false, ""},
		{"nil", nil, false, ""},
		{"warnings only", List{warn, {Code: "RZ-CFG-025", Severity: SeverityWarning}}, false, ""},
		{"errors after a warning", List{warn, {Code: "RZ-CFG-009", Severity: SeverityError}, {Code: "RZ-CFG-005", Severity: SeverityError}}, true, "RZ-CFG-009"},
		{"zero severity is an error", List{{Code: "RZ-CFG-005"}}, true, "RZ-CFG-005"},
		{"zero severity after a warning", List{warn, {Code: "RZ-CFG-006"}}, true, "RZ-CFG-006"},
		{"unknown severity is an error", List{{Code: "RZ-CFG-007", Severity: Severity(9)}}, true, "RZ-CFG-007"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.l.HasErrors(); got != tt.hasErrors {
				t.Errorf("HasErrors = %v, want %v", got, tt.hasErrors)
			}
			if got := tt.l.FirstErrorCode(); got != tt.first {
				t.Errorf("FirstErrorCode = %q, want %q", got, tt.first)
			}
			// What blocks must be what prints as "error" (and the reverse).
			for _, d := range tt.l {
				if printed := d.Severity.String() == "error"; printed != (List{d}).HasErrors() {
					t.Errorf("severity %d prints %q but HasErrors = %v", d.Severity, d.Severity, (List{d}).HasErrors())
				}
			}
		})
	}
}

func TestCollectorCap(t *testing.T) {
	// 01 req 50: on reaching the cap, one RZ-CFG-001 "too many diagnostics".
	c := NewCollector(3)
	for i := range 3 {
		if !c.Add(Diagnostic{Code: "RZ-CFG-005", Severity: SeverityError, Location: Location{File: "f.yaml", Line: 3 - i}}) {
			t.Fatalf("Add %d refused below the cap", i)
		}
	}
	if c.Full() {
		t.Fatal("Full before the cap was exceeded")
	}
	if c.Add(Diagnostic{Code: "RZ-CFG-005"}) {
		t.Fatal("Add past the cap succeeded")
	}
	if c.Add(Diagnostic{Code: "RZ-CFG-006"}) || c.AddAll(List{{Code: "RZ-CFG-006"}}) {
		t.Fatal("Add after Full succeeded")
	}
	if !c.Full() {
		t.Fatal("Full = false after the cap")
	}
	l := c.List()
	if len(l) != 4 {
		t.Fatalf("List has %d diagnostics, want 3 plus the cap error", len(l))
	}
	var caps int
	for _, d := range l {
		if d.Code == "RZ-CFG-001" {
			caps++
			if d.Severity != SeverityError || !strings.HasPrefix(d.Message, "too many diagnostics") {
				t.Fatalf("cap diagnostic = %+v", d)
			}
		}
	}
	if caps != 1 {
		t.Fatalf("%d cap diagnostics, want exactly 1", caps)
	}
	// List is sorted and a copy.
	if l[1].Line != 1 || l[3].Line != 3 {
		t.Fatalf("List is not sorted: %v", l)
	}
	l[0].Code = "changed"
	if c.List()[0].Code == "changed" {
		t.Fatal("List returned the Collector's own slice")
	}
}

func TestCollectorUnlimitedAndConcurrent(t *testing.T) {
	c := NewCollector(0)
	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				c.Add(Diagnostic{Code: "RZ-CFG-005", Severity: SeverityError, Location: Location{File: fmt.Sprintf("w%d.yaml", w), Line: i + 1}})
			}
		}()
	}
	wg.Wait()
	if c.Full() || len(c.List()) != 1600 {
		t.Fatalf("Full %v, %d diagnostics; want unlimited", c.Full(), len(c.List()))
	}
	if !c.AddAll(List{{Code: "RZ-CFG-005"}, {Code: "RZ-CFG-006"}}) || len(c.List()) != 1602 {
		t.Fatal("AddAll did not add every diagnostic")
	}
	// Deterministic whatever the worker interleaving.
	a, b := c.List(), c.List()
	var ta, tb bytes.Buffer
	_ = WriteText(&ta, a)
	_ = WriteText(&tb, b)
	if !bytes.Equal(ta.Bytes(), tb.Bytes()) {
		t.Fatal("List is not deterministic")
	}
}

func TestNearest(t *testing.T) {
	many := func(n int, s string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = s
		}
		return out
	}
	tests := []struct {
		name       string
		input      string
		candidates []string
		want       string
		ok         bool
	}{
		{"CM example transposition", "inventroy", []string{"orders", "inventory"}, "inventory", true},
		{"case-insensitive exact first", "GateWay", []string{"gateways", "gateway"}, "gateway", true},
		{"exact beyond the compare cap", "target", append(many(100, "xxxxxx"), "TARGET"), "TARGET", true},
		{"no candidates", "abc", nil, "", false},
		{"short input limit 1", "ab", []string{"ax"}, "ax", true},
		{"short input over limit", "ab", []string{"xy"}, "", false},
		{"length 6 limit 2", "abcdef", []string{"abcxyf"}, "abcxyf", true},
		{"length 6 over limit", "abcdef", []string{"abxyzf"}, "", false},
		{"limit capped at 3", "abcdefghijkl", []string{"abcdefghixyz"}, "abcdefghixyz", true},
		{"limit capped at 3, over", "abcdefghijkl", []string{"abcdefghwxyz"}, "", false},
		{"ties broken by byte order", "cat", []string{"cot", "bat", "cut"}, "bat", true},
		{"smaller distance wins", "config", []string{"confog", "config2x"}, "confog", true},
		{"length difference over 3 skipped", "ab", []string{"abcdef"}, "", false},
		{"at most 64 compared", "zzzz", append(many(64, "aaaa"), "zzza"), "", false},
		{"skipped candidates are not compared", "zzzz", append(many(100, "aaaaaaaaaa"), "zzza"), "zzza", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := Nearest(tt.input, tt.candidates)
			if ok != tt.ok || (ok && got != tt.want) {
				t.Fatalf("Nearest(%q) = %q, %v; want %q, %v", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestOSA(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"", "", 0},
		{"a", "", 1},
		{"", "abc", 3},
		{"abc", "abc", 0},
		{"ab", "ba", 1},
		{"ca", "abc", 3},
		{"kitten", "sitting", 3},
	}
	for _, tt := range tests {
		if got := osa(tt.a, tt.b); got != tt.want {
			t.Errorf("osa(%q, %q) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

// decodePath is the reverse of Path.MarshalJSON for round-trip checks:
// strings are fields, integers indexes, {"item":v} set items and any
// other single-member object a keyed entry.
func decodePath(b []byte) (Path, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var raw []any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	var p Path
	for _, e := range raw {
		switch v := e.(type) {
		case string:
			p = append(p, Field(v))
		case json.Number:
			i, err := strconv.Atoi(string(v))
			if err != nil {
				return nil, err
			}
			p = append(p, Index(i))
		case map[string]any:
			if len(v) != 1 {
				return nil, fmt.Errorf("object element with %d members", len(v))
			}
			for k, x := range v {
				if k == "item" {
					if m, ok := x.(map[string]any); ok {
						raw, _ := json.Marshal(m)
						p = append(p, Item(json.RawMessage(raw)))
					} else {
						p = append(p, Item(x))
					}
					continue
				}
				s, ok := x.(string)
				if !ok {
					return nil, fmt.Errorf("keyed value %T", x)
				}
				p = append(p, Keyed(k, s))
			}
		default:
			return nil, fmt.Errorf("element %T", e)
		}
	}
	return p, nil
}

// validUTF8 replaces each invalid byte with U+FFFD, as encoding/json does.
func validUTF8(s string) string { return string([]rune(s)) }

// checkQuoted asserts that a one-element human path is prefix, then raw
// verbatim when bare, or else a JSON string decoding to validUTF8(raw),
// then suffix.
func checkQuoted(t *testing.T, got, prefix, suffix, raw string, bare bool) {
	t.Helper()
	inner, ok := strings.CutPrefix(got, prefix)
	inner, ok2 := strings.CutSuffix(inner, suffix)
	if !ok || !ok2 {
		t.Fatalf("path %q is not %s...%s", got, prefix, suffix)
	}
	if bare {
		if inner != raw {
			t.Fatalf("bare %q printed as %q", raw, inner)
		}
		return
	}
	var dec string
	if !json.Valid([]byte(inner)) || json.Unmarshal([]byte(inner), &dec) != nil {
		t.Fatalf("quoted %q is not a JSON string: %q", raw, inner)
	}
	if dec != validUTF8(raw) {
		t.Fatalf("quoted %q decodes to %q, want %q", raw, dec, validUTF8(raw))
	}
}

// FuzzHumanPath (spec 02 item 35): path printing never panics, the human
// form is one line and the JSON form round-trips.
func FuzzHumanPath(f *testing.F) {
	f.Add([]byte{0, 0, 2, 0}, "spec", "stock", 3)
	f.Add([]byte{0, 3, 4, 5, 6}, "a b", "request.body.read", -1)
	f.Add([]byte{1, 2, 1}, "\n\"\\\xff", "<&>", 1<<40)
	f.Add([]byte{0, 2}, "a\x01\x7f\xff\U000e0001", "b\a\xfe\u2028", 0)
	f.Fuzz(func(t *testing.T, kinds []byte, s1, s2 string, n int) {
		if len(kinds) > 32 {
			kinds = kinds[:32]
		}
		// Key fields are schema member names, never "item".
		keyFields := []string{"name", "port", "key", "id"}
		keyField := keyFields[uint(n)%uint(len(keyFields))]
		var p Path
		for i, k := range kinds {
			switch k % 7 {
			case 0:
				p = append(p, Field(validUTF8(s1)))
			case 1:
				p = append(p, Index(n+i))
			case 2:
				p = append(p, Keyed(keyField, validUTF8(s1)))
			case 3:
				p = append(p, Item(validUTF8(s2)))
			case 4:
				p = append(p, Item(k&8 != 0))
			case 5:
				p = append(p, Item(json.Number(strconv.Itoa(n))))
			case 6:
				raw, err := json.Marshal(map[string]string{validUTF8(s1): validUTF8(s2)})
				if err != nil {
					t.Fatal(err)
				}
				p = append(p, Item(json.RawMessage(raw)))
			}
		}
		// 01 req 48 on raw input: a non-identifier field and a non-bare
		// keyed value are valid JSON strings, whatever bytes they hold.
		if field := (Path{Field(s1)}).String(); isFieldName(s1) {
			if field != s1 {
				t.Fatalf("identifier field %q printed as %q", s1, field)
			}
		} else {
			checkQuoted(t, field, "[", "]", s1, false)
		}
		checkQuoted(t, Path{Keyed(keyField, s2)}.String(), "["+keyField+"=", "]", s2, isBare(s2))

		human := p.String()
		if strings.ContainsAny(human, "\r\n") {
			t.Fatalf("human path spans lines: %q", human)
		}
		if human != p.String() {
			t.Fatal("human path is not deterministic")
		}
		b, err := p.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		back, err := decodePath(b)
		if err != nil {
			t.Fatalf("decode %s: %v", b, err)
		}
		if back.String() != human || len(back) != len(p) {
			t.Fatalf("round trip %s -> %s, want %s", b, back, human)
		}
		b2, err := back.MarshalJSON()
		if err != nil || !bytes.Equal(b, b2) {
			t.Fatalf("JSON round trip %s -> %s (%v)", b, b2, err)
		}
	})
}
