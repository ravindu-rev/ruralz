// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
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

// activeRevision returns a Revision with CEL text.
func activeRevision(t *testing.T) revision.Revision {
	t.Helper()
	rev, err := NewEncoder(hubIndex(t)).Encode(context.Background(), bundleOf(t, gatewayDoc, celRoute, routeDoc), Options{})
	if err != nil {
		t.Fatal(err)
	}
	return rev
}

// TestAppendDump is the /config/dump document (02 req 73, test plan 14):
// RFC 8785 member order, content copied byte for byte (CEL '>=' and '&&'
// included), lastKnownGood omitted when there is none.
func TestAppendDump(t *testing.T) {
	active := activeRevision(t)
	lkg := revision.Sum([]byte("previous"))
	got := AppendDump([]byte("prefix:"), active, lkg)
	want := `prefix:{"content":` + string(active.Content) + `,"digest":"` + active.Digest.String() +
		`","lastKnownGood":"` + lkg.String() + `","revision":"` + active.Digest.Short() + `"}`
	sameBytes(t, "dump", got, []byte(want))
	if !bytes.Contains(got, []byte(`"rule":"response.status >= 400 && request.size < 10"`)) {
		t.Errorf("CEL text changed in %s", got)
	}
	none := AppendDump(nil, active, revision.Digest{})
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

// TestReadDump: ReadDump returns the received bytes unchanged, verifies
// the content and decodes it with positions in the dump file (02 reqs 31,
// 73 and 74; 10 reqs 101 and 102).
func TestReadDump(t *testing.T) {
	ctx := context.Background()
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	active := activeRevision(t)
	lkg := revision.Sum([]byte("previous"))
	body := AppendDump(nil, active, lkg)
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
	none := AppendDump(nil, active, revision.Digest{})
	if d, _, err := dec.ReadDump(ctx, bytes.NewReader(none), int64(len(none)), Source{}); err != nil || !d.LastKnownGood.IsZero() {
		t.Errorf("dump without lastKnownGood: %v, %v", d.LastKnownGood, err)
	}
	// Members a newer Node adds are ignored (10 req 69).
	extra := bytes.Replace(body, []byte(`"digest"`), []byte(`"node":{"id":"n1","tags":[1,{"a":null}]},"note":"x","digest"`), 1)
	if _, _, err := dec.ReadDump(ctx, bytes.NewReader(extra), 0, Source{}); err != nil {
		t.Errorf("unknown members: %v", err)
	}
}

// TestReadDumpErrors: tampered content, a wrong revision and newer
// content carry RZ codes; malformed documents are ErrNotDump; the cap is
// ErrDumpTooLarge (02 test plan 14; 10 req 102).
func TestReadDumpErrors(t *testing.T) {
	ctx := context.Background()
	dec := NewDecoder(hubIndex(t), DecoderOptions{})
	active := activeRevision(t)
	body := string(AppendDump(nil, active, revision.Digest{}))
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
	if _, _, err := dec.ReadDump(ctx, strings.NewReader(`{"content":{"a":`+strings.Repeat("[", 300)), 0, Source{}); !errors.Is(err, ErrNotDump) || !errors.Is(err, jsonval.ErrDepth) {
		t.Errorf("deep content: error = %v", err)
	}
}
