// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// celRoute is a Route whose CEL text holds '>=', '&&' and '<'.
const celRoute = `{"apiVersion": "ruralz/v1alpha1", "kind": "Policy", "metadata": {"name": "authz"}, "spec": {"type": "authz.cel", "config": {"rule": "response.status >= 400 && request.size < 10"}}}`

// dumpOf returns the /config/dump document of active, failing when
// AppendDump refuses it.
func dumpOf(t testing.TB, dst []byte, active revision.Revision, lkg revision.Digest) []byte {
	t.Helper()
	out, ok := AppendDump(dst, active, lkg)
	if !ok {
		t.Fatalf("AppendDump(%s) refused a published Revision", active.Digest)
	}
	return out
}

// activeRevision returns a Revision with CEL text.
func activeRevision(t *testing.T) revision.Revision {
	t.Helper()
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), bundleOf(t, gatewayDoc, celRoute, routeDoc), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

// TestAppendDump is the /config/dump document: RFC 8785 member order,
// content copied byte for byte (CEL '>=' and '&&' included),
// lastKnownGood omitted when there is none.
func TestAppendDump(t *testing.T) {
	active := activeRevision(t)
	lkg := revision.Sum([]byte("previous"))
	got := dumpOf(t, []byte("prefix:"), active, lkg)
	want := `prefix:{"content":` + string(active.Content) + `,"digest":"` + active.Digest.String() +
		`","lastKnownGood":"` + lkg.String() + `","revision":"` + active.Digest.Short() + `"}`
	sameBytes(t, "dump", got, []byte(want))
	if !bytes.Contains(got, []byte(`"rule":"response.status >= 400 && request.size < 10"`)) {
		t.Errorf("CEL text changed in %s", got)
	}
	none := dumpOf(t, nil, active, revision.Digest{})
	if bytes.Contains(none, []byte("lastKnownGood")) {
		t.Errorf("lastKnownGood written without one: %s", none)
	}
	// The document is itself in RFC 8785 form.
	for _, d := range [][]byte{got[len("prefix:"):], none} {
		n, err := jsonNode(d)
		if err != nil {
			t.Fatal(err)
		}
		again, err := AppendNode(nil, n)
		if err != nil {
			t.Fatal(err)
		}
		sameBytes(t, "dump in RFC 8785 form", again, d)
	}
}

// TestAppendDumpNoRevision: a Node without a published Revision has no
// dump: AppendDump returns dst unchanged and false for every Revision
// that names no digest or whose content is not one JSON object between
// the canonical header and trailer (what dumpable checks), and writes a
// document ReadDump accepts for the smallest canonical content. The
// probes give content that starts with the header and ends with the
// trailer yet is not one JSON object: an unterminated string, an injected
// top-level member, garbage, an open object, a missing value.
func TestAppendDumpNoRevision(t *testing.T) {
	empty := []byte(doc())
	other := `{"resources":[],"x":[` + strings.Repeat("1,", 30) + `1]}`
	enclosed := func(mid string) revision.Revision {
		c := []byte(header + mid + trailer)
		return revision.Revision{Content: c, Digest: revision.Sum(c)}
	}
	for _, tc := range []struct {
		name string
		rev  revision.Revision
	}{
		{"zero Revision", revision.Revision{}},
		{"content without a digest", revision.Revision{Content: empty}},
		{"digest without content", revision.Revision{Digest: revision.Sum(nil)}},
		{"empty content", revision.Revision{Content: []byte{}, Digest: revision.Sum(nil)}},
		{"not a canonical document", revision.Revision{Content: []byte(`[]`), Digest: revision.Sum([]byte(`[]`))}},
		{"object of another shape", revision.Revision{Content: []byte(`{"x":1}`), Digest: revision.Sum([]byte(`{"x":1}`))}},
		{"header only", revision.Revision{Content: []byte(header), Digest: revision.Sum([]byte(header))}},
		{"one byte short", revision.Revision{Content: empty[:len(empty)-1], Digest: revision.Sum(empty[:len(empty)-1])}},
		{"no trailer", revision.Revision{Content: []byte(header + "{}"), Digest: revision.Sum([]byte(header + "{}"))}},
		{"another document ending like one", revision.Revision{Content: []byte(other), Digest: revision.Sum([]byte(other))}},
		{"unterminated string", enclosed(`"`)},
		{"injected top-level member", enclosed(`]},"x":{"a":[`)},
		{"garbage between", enclosed(`]}garbage[`)},
		{"open object", enclosed(`{`)},
		{"open string", enclosed(`"x`)},
		{"missing value", enclosed(`1,,2`)},
		{"duplicate member", enclosed(`{"a":1,"a":2}`)},
		{"too deep", enclosed(strings.Repeat("[", 20_000) + strings.Repeat("]", 20_000))},
	} {
		dst := []byte("prefix:")
		got, ok := AppendDump(dst, tc.rev, revision.Sum([]byte("lkg")))
		if ok || string(got) != "prefix:" {
			t.Errorf("%s: AppendDump = %q, %v; want dst unchanged and false", tc.name, got, ok)
		}
	}
	rev := revision.Revision{Content: empty, Digest: revision.Sum(empty)}
	body := dumpOf(t, nil, rev, revision.Digest{})
	if _, _, err := NewDecoder(hubIndex(t), DecoderOptions{}).ReadDump(context.Background(), bytes.NewReader(body), 0, Source{}); err != nil {
		t.Errorf("dump of the empty document: %v", err)
	}
	// Content of the canonical shape that is not canonical is dumped as it
	// is: AppendDump checks the JSON, not the form, which ReadDump judges.
	for _, mid := range []string{`1`, `{"a":1}`, `1],"x":[2`} {
		rev := enclosed(mid)
		body, ok := AppendDump(nil, rev, revision.Digest{})
		if !ok || !json.Valid(body) {
			t.Errorf("content %q: AppendDump = %q, %v; want one JSON object", rev.Content, body, ok)
		}
	}
}

// TestAppendDumpDeep: AppendDump serves content nested as deep as a
// reader of the dump document, one level deeper, accepts:
// jsonval.MaxDepthLimit - 1, not only the decoder's default. ReadDump
// reads each such dump and judges the content's nesting by the decode's
// own limit (RZ-CFG-001 past 128, the content's form below it), never
// ErrNotDump.
func TestAppendDumpDeep(t *testing.T) {
	deep := func(depth int) revision.Revision {
		c := []byte(header + strings.Repeat("[", depth-2) + strings.Repeat("]", depth-2) + trailer)
		return revision.Revision{Content: c, Digest: revision.Sum(c)}
	}
	for _, depth := range []int{65, DefaultMaxDepth, DefaultMaxDepth + 1, 200, jsonval.MaxDepthLimit - 1} {
		body, ok := AppendDump(nil, deep(depth), revision.Digest{})
		if !ok {
			t.Errorf("depth %d: AppendDump refused the content", depth)
			continue
		}
		want := CodeLimit
		if depth <= DefaultMaxDepth {
			want = CodeMismatch
		}
		_, _, err := NewDecoder(hubIndex(t), DecoderOptions{}).ReadDump(context.Background(), bytes.NewReader(body), 0, Source{})
		if codeOf(err) != want {
			t.Errorf("depth %d: ReadDump error = %v, want %s", depth, err, want)
		}
	}
	if _, ok := AppendDump(nil, deep(jsonval.MaxDepthLimit), revision.Digest{}); ok {
		t.Error("AppendDump served content whose dump document no reader accepts")
	}
}

// TestReadDump: ReadDump returns the received bytes unchanged, verifies
// the content and decodes it with positions in the dump file.
func TestReadDump(t *testing.T) {
	ctx := context.Background()
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	active := activeRevision(t)
	lkg := revision.Sum([]byte("previous"))
	body := dumpOf(t, nil, active, lkg)
	d, rs, err := dec.ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "dump.json"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(d.Body, body) || !bytes.Equal(d.Content, active.Content) || d.Digest != active.Digest || d.LastKnownGood != lkg {
		t.Errorf("Dump = digest %s lkg %s, body equal %v", d.Digest, d.LastKnownGood, bytes.Equal(d.Body, body))
	}
	if len(rs) != 3 || rs[0].Start.Line != 1 || int(rs[0].Start.Column) != len(`{"content":`)+len(header)+1 {
		t.Errorf("resources = %d, first at %+v", len(rs), rs[0].Start)
	}
	none := dumpOf(t, nil, active, revision.Digest{})
	if d, _, err := dec.ReadDump(ctx, bytes.NewReader(none), int64(len(none)), Source{}); err != nil || !d.LastKnownGood.IsZero() {
		t.Errorf("dump without lastKnownGood: %v, %v", d.LastKnownGood, err)
	}
	// Members a newer Node adds are ignored.
	extra := bytes.Replace(body, []byte(`"digest"`), []byte(`"node":{"id":"n1","tags":[1,{"a":null}]},"note":"x","digest"`), 1)
	if _, _, err := dec.ReadDump(ctx, bytes.NewReader(extra), 0, Source{}); err != nil {
		t.Errorf("unknown members: %v", err)
	}
}

// TestReadDumpErrors: tampered content, a wrong revision and newer
// content carry RZ codes; malformed documents are ErrNotDump; the cap is
// ErrDumpTooLarge.
func TestReadDumpErrors(t *testing.T) {
	ctx := context.Background()
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	active := activeRevision(t)
	body := string(dumpOf(t, nil, active, revision.Digest{}))
	content := string(active.Content)
	newer := doc(`{"kind":"Route","metadata":{"name":"r","owner":"x"},"spec":{}}`)
	newerDigest := revision.Sum([]byte(newer))
	for _, tc := range []struct {
		name, body string
		code       string
		sentinel   error
	}{
		{"tampered content", strings.Replace(body, `"edge"`, `"edgf"`, 1), CodeMismatch, nil},
		{"reformatted content", strings.Replace(body, `"spec":{`, `"spec": {`, 1), CodeMismatch, nil},
		{"wrong revision", strings.Replace(body, active.Digest.Short(), "rev-000000000000", 1), CodeMismatch, nil},
		{"newer content", `{"content":` + newer + `,"digest":"` + newerDigest.String() + `","revision":"` + newerDigest.Short() + `"}`, CodeNewer, nil},
		{"not JSON", `not json`, "", ErrNotDump},
		{"empty", ``, "", ErrNotDump},
		{"array", `[]`, "", ErrNotDump},
		{"trailing data", body + `{}`, "", ErrNotDump},
		{"truncated", body[:len(body)-2], "", ErrNotDump},
		{"truncated in content", body[:30], "", ErrNotDump},
		{"duplicate member", strings.Replace(body, `"digest"`, `"content":{},"digest"`, 1), "", ErrNotDump},
		{"content not an object", `{"content":"x","digest":"` + active.Digest.String() + `","revision":"x"}`, "", ErrNotDump},
		{"no content", `{"digest":"` + active.Digest.String() + `","revision":"` + active.Digest.Short() + `"}`, "", ErrNotDump},
		{"no digest", `{"content":` + content + `,"revision":"` + active.Digest.Short() + `"}`, "", ErrNotDump},
		{"no revision", `{"content":` + content + `,"digest":"` + active.Digest.String() + `"}`, "", ErrNotDump},
		{"digest not a string", `{"content":` + content + `,"digest":1,"revision":"x"}`, "", ErrNotDump},
		{"uppercase digest", strings.Replace(body, active.Digest.String(), strings.ToUpper(active.Digest.String()), 1), "", ErrNotDump},
		{"short digest", strings.Replace(body, active.Digest.String(), active.Digest.Short(), 1), "", ErrNotDump},
		{"bad lastKnownGood", strings.Replace(body, `"digest"`, `"lastKnownGood":"sha512:00","digest"`, 1), "", ErrNotDump},
		{"unknown member malformed", strings.Replace(body, `"digest"`, `"x":[1,,2],"digest"`, 1), "", ErrNotDump},
	} {
		_, _, err := dec.ReadDump(ctx, strings.NewReader(tc.body), 0, Source{Name: "dump.json"})
		switch {
		case tc.code != "" && codeOf(err) != tc.code:
			t.Errorf("%s: error = %v, want %s", tc.name, err, tc.code)
		case tc.sentinel != nil && !errors.Is(err, tc.sentinel):
			t.Errorf("%s: error = %v, want %v", tc.name, err, tc.sentinel)
		}
	}
	if _, _, err := dec.ReadDump(ctx, strings.NewReader(body), int64(len(body)-1), Source{}); !errors.Is(err, ErrDumpTooLarge) {
		t.Errorf("limit-1: error = %v, want ErrDumpTooLarge", err)
	}
	for _, limit := range []int64{int64(len(body)), math.MaxInt64} {
		if _, _, err := dec.ReadDump(ctx, strings.NewReader(body), limit, Source{}); err != nil {
			t.Errorf("limit %d: error = %v", limit, err)
		}
	}
	boom := errors.New("boom")
	if _, _, err := dec.ReadDump(ctx, iotest.ErrReader(boom), 0, Source{}); !errors.Is(err, boom) {
		t.Errorf("reader error = %v", err)
	}
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err := dec.ReadDump(cctx, strings.NewReader(body), 0, Source{}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: error = %v", err)
	}
	// The content's nesting is the decode's to judge (TestDumpDepth); a
	// body nested beyond jsonval's own limit is not a dump document.
	if _, _, err := dec.ReadDump(ctx, strings.NewReader(`{"content":{"a":`+strings.Repeat("[", jsonval.MaxDepthLimit)), 0, Source{}); !errors.Is(err, ErrNotDump) || !errors.Is(err, jsonval.ErrDepth) {
		t.Errorf("body past jsonval.MaxDepthLimit: error = %v", err)
	}
}
