// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package cache

import (
	"bytes"
	"encoding/hex"
	"errors"
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/statestore"
)

// update rewrites the golden files.
var update = flag.Bool("update", false, "rewrite the golden files under testdata")

// goldenEntry is the entry pinned by testdata/entry.golden.
func goldenEntry() *entry {
	return &entry{
		token:      statestore.Token{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15},
		flags:      flagCredentialed | flagAuthorized,
		status:     http.StatusOK,
		initialAge: 1500 * time.Millisecond,
		route:      "catalog", upstream: "catalog-svc",
		method: http.MethodGet, target: "https://shop.example/v1/catalog?page=2",
		reqHeader: http.Header{"Accept": {"application/json"}, "X-B": {"1", "2"}},
		header: http.Header{
			"Cache-Control": {"max-age=60"},
			"Date":          {"Fri, 30 Oct 2026 09:00:00 GMT"},
			"Etag":          {`"v1"`},
		},
		body: []byte("hello"),
	}
}

// TestEntryGolden pins format 1 of "Response Cache layout", e:<V> row:
// format byte, token, flags, status, initial age in milliseconds, Route,
// Upstream, the saved request's method, target and header block, the
// response header block, then the body to the end. Nodes of two releases
// share it, so a change here is a layout change.
func TestEntryGolden(t *testing.T) {
	got := hex.EncodeToString(appendEntry(nil, goldenEntry())) + "\n"
	path := filepath.Join("testdata", "entry.golden")
	if *update {
		if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Errorf("entry encoding changed:\n got %s\nwant %s", got, want)
	}
}

// TestEntryLayout checks the fields of the pinned encoding byte by byte at
// its start: format 1, then the raw token, flags 5, status 200 and the
// 1500 ms age as varints.
func TestEntryLayout(t *testing.T) {
	b := appendEntry(nil, goldenEntry())
	want := append([]byte{formatV1}, goldenEntry().token[:]...)
	want = append(want, 5, 0xc8, 0x01, 0xdc, 0x0b, 7)
	want = append(want, "catalog"...)
	if !bytes.HasPrefix(b, want) {
		t.Errorf("entry starts % x, want % x", b[:len(want)], want)
	}
	if !bytes.HasSuffix(b, []byte("hello")) {
		t.Error("the body is not the end of the entry")
	}
	if n := len(appendPrefix(nil, goldenEntry())); n != len(b)-len("hello") {
		t.Errorf("prefix length %d, want the entry without its body", n)
	}
}

// TestEntryRoundTrip checks that decoding returns every field, header
// lines in order, and that the body aliases the bytes.
func TestEntryRoundTrip(t *testing.T) {
	in := goldenEntry()
	b := appendEntry(nil, in)
	var out entry
	if err := decodeEntry(&out, b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(&out, in) {
		t.Errorf("decoded %+v, want %+v", out, *in)
	}
	if &out.body[0] != &b[len(b)-len(in.body)] {
		t.Error("the decoded body does not alias the entry bytes")
	}
	if !out.credentialed() || !out.authorized() {
		t.Error("flags lost")
	}
}

// TestEntryDecodeErrors checks that an unknown format and every truncation
// fail, so the lookup misses.
func TestEntryDecodeErrors(t *testing.T) {
	b := appendPrefix(nil, goldenEntry())
	var e entry
	if err := decodeEntry(&e, nil); !errors.Is(err, errCorrupt) {
		t.Errorf("empty entry: %v", err)
	}
	other := append([]byte{2}, b[1:]...)
	if err := decodeEntry(&e, other); !errors.Is(err, errFormat) {
		t.Errorf("format 2: %v, want errFormat", err)
	}
	for n := 1; n < len(b); n++ {
		if err := decodeEntry(&e, b[:n]); !errors.Is(err, errCorrupt) {
			t.Fatalf("entry cut at %d of %d bytes decoded: %v", n, len(b), err)
		}
		if e.status != 0 || e.header != nil {
			t.Fatalf("a failed decode left fields set: %+v", e)
		}
	}
}
