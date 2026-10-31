// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package schemaview

import (
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/ravindu-rev/ruralz/internal/config/tree"
)

// refQuote and refShorten are the implementations before the bounded
// clip: the reference the bounded ones must equal on every input.
func refQuote(s string) string {
	if utf8.RuneCountInString(s) > maxQuoted {
		r := []rune(s)
		s = string(r[:maxQuoted]) + "..."
	}
	return jsonText(s)
}

func refShorten(s string) string {
	r := []rune(s)
	if len(r) <= maxQuoted {
		return s
	}
	return string(r[:maxQuoted]) + "..."
}

// TestClipQuoteShorten checks that clip keeps the first maxQuoted code
// points, an invalid UTF-8 byte counting as one, and that quote and
// shorten built on it equal the []rune implementations at and around the
// maxQuoted boundary, with multi-byte runes and invalid bytes on either
// side of it, without converting the whole text to []rune.
func TestClipQuoteShorten(t *testing.T) {
	x := func(n int) string { return strings.Repeat("x", n) }
	for _, tc := range []struct {
		name string
		s    string
	}{
		{"empty", ""},
		{"one byte", "a"},
		{"one rune under", x(maxQuoted - 1)},
		{"at the cap", x(maxQuoted)},
		{"one rune over", x(maxQuoted + 1)},
		{"two-byte runes at the cap", strings.Repeat("é", maxQuoted)},
		{"two-byte runes over", strings.Repeat("é", maxQuoted+1)},
		{"four-byte runes over", strings.Repeat("𝄞", maxQuoted+5)},
		{"multi-byte rune at the boundary", x(maxQuoted-1) + "é" + "y"},
		{"multi-byte rune past the boundary", x(maxQuoted) + "é"},
		{"invalid bytes over", strings.Repeat("\xff", maxQuoted+6)},
		{"invalid byte at the boundary", x(maxQuoted-1) + "\xff" + "z"},
		{"invalid byte past the boundary", x(maxQuoted) + "\xff"},
		{"truncated rune at the end", x(maxQuoted-2) + "\xe2\x82"},
		{"truncated rune over the end", x(maxQuoted-1) + "\xe2\x82"},
		{"quotes and controls over", strings.Repeat("\"\n<", maxQuoted)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head, long := clip(tc.s)
			runes := utf8.RuneCountInString(tc.s)
			if got := utf8.RuneCountInString(head); got != min(runes, maxQuoted) || !strings.HasPrefix(tc.s, head) || long != (runes > maxQuoted) {
				t.Errorf("clip(%q) = %q (%d code points), %v; want the first %d code points, %v", tc.s, head, got, long, min(runes, maxQuoted), runes > maxQuoted)
			}
			if got, want := quote(tc.s), refQuote(tc.s); got != want {
				t.Errorf("quote(%q) = %q, want %q", tc.s, got, want)
			}
			if got, want := shorten(tc.s), refShorten(tc.s); got != want {
				t.Errorf("shorten(%q) = %q, want %q", tc.s, got, want)
			}
		})
	}
}

// TestQuoteBoundedAlloc checks that quoting and shortening a 16 MiB text
// allocates a few hundred bytes, not the 64 MiB of a []rune of it, and
// that a short text is returned by shorten without a copy.
func TestQuoteBoundedAlloc(t *testing.T) {
	long := strings.Repeat("a", 16<<20)
	for name, fn := range map[string]func(string) string{"quote": quote, "shorten": shorten} {
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		got := fn(long)
		runtime.ReadMemStats(&after)
		if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 64<<10 {
			t.Errorf("%s(16 MiB) allocated %d bytes, want at most 64 KiB", name, alloc)
		}
		if !strings.Contains(got, "...") {
			t.Errorf("%s(16 MiB) = %.80q, want it shortened", name, got)
		}
	}
	if allocs := testing.AllocsPerRun(10, func() { _ = shorten(`{"a":1}`) }); allocs != 0 {
		t.Errorf("shorten(short) made %v allocations, want none", allocs)
	}
}

// TestLongTextAlloc bounds the bytes the mapping allocates per document
// byte for one long authored text quoted or shortened in a message: a
// 4 MiB unknown member name, bare and JSON-quoted, and a 4 MiB string
// that is a duplicate set element and a duplicate keyed-list key: quote
// and shorten never convert the whole text to []rune (5 to 8.5 bytes per
// document byte), and the canonical JSON of a long element never grows
// its buffer from empty (several times its size). Rendering the text once
// in its path segment and once as the element's canonical JSON remains.
func TestLongTextAlloc(t *testing.T) {
	big := strings.Repeat("a", 4<<20)
	quoted := strings.Repeat("a ", 2<<20)
	// Each budget holds with a margin in the three configurations CI
	// runs: Go 1.27.1 (1.00, 3.01, 2.01 and 2.01 bytes per byte measured),
	// Go 1.27.1 with -race (1.00, 5.01, 2.51 and 2.51) and the Go 1.26.8
	// floor with GOEXPERIMENT=jsonv2 (1.00, 6.01, 2.01 and 2.01); the
	// floor with -race, which CI does not run, measured 1.00, 11.02, 2.51
	// and 2.51. A JSON-quoted segment allocates most: diag.Path.String
	// quotes it with a json.Encoder whose buffer grows from empty, which
	// the Go 1.26 jsonv2 encoder does at twice the cost, so a budget of 6
	// fails on the floor.
	for _, tc := range []struct {
		name, src string
		// budget is the bytes allocated per document byte.
		budget float64
	}{
		{"unknown name", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+big+`": 1}`), 2},
		{"quoted unknown name", envelope("Route", `{"match": {"path": {"prefix": "/"}}, "upstreams": [{"name": "u"}], "`+quoted+`": 1}`), 8},
		{"duplicate set element", envelope("Gateway", `{"listeners": [{"name": "l", "port": 1, "protocol": "http", "hostnames": ["`+big[:2<<20]+`", "`+big[:2<<20]+`"]}]}`), 4},
		{"duplicate keyed-list key", envelope("Gateway", `{"listeners": [{"name": "`+big[:2<<20]+`", "port": 1, "protocol": "http"}, {"name": "`+big[:2<<20]+`", "port": 2, "protocol": "http"}]}`), 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := testView(t)
			files := &tree.FileTable{}
			res := mustResource(t, files, "r.json", tc.src)
			alloc, got := mappingAlloc(v, res, files)
			if len(got) == 0 {
				t.Fatal("no diagnostic")
			}
			if perByte := float64(alloc) / float64(len(tc.src)); perByte > tc.budget {
				t.Errorf("mapping allocated %d bytes for a %d-byte document (%.1f per byte), want at most %.1f per byte", alloc, len(tc.src), perByte, tc.budget)
			}
			t.Logf("%.2f bytes per document byte", float64(alloc)/float64(len(tc.src)))
		})
	}
}
