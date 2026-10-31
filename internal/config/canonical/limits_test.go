// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"errors"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/diag"
	"github.com/ravindu-rev/ruralz/internal/config/revision"
	"github.com/ravindu-rev/ruralz/internal/config/tree"
	"github.com/ravindu-rev/ruralz/internal/jsonval"
)

// TestDecodeOptionsClamped: every count option means at most 2^30, so
// math.MaxInt and its neighbors, the usual ways to say "no cap", decode
// as the default cap does, and a cap plus one never wraps. Each member of
// the class of findings (a newer field, a newer document member, a form
// problem, an unknown kind, a resource apiVersion member) gives the code
// and findings of the default decoder under each cap, with one worker and
// with three, never the internal errStop.
func TestDecodeOptionsClamped(t *testing.T) {
	contents := []struct{ name, content string }{
		{"newer field", doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"zz":1}}`)},
		{"newer document member", `{"format":"ruralz.canonical.v1","resources":[],"x":1}`},
		{"form problem", doc(`1`)},
		{"unknown kind", doc(`{"kind":"Zebra","metadata":{"name":"z"}}`)},
		{"resource apiVersion member", doc(`{"apiVersion":"x","kind":"Route","metadata":{"name":"r"},"spec":{}}`)},
	}
	caps := []int{0, 1, math.MaxInt - 1, math.MaxInt/2 + 1, math.MaxInt}
	for _, tc := range contents {
		want := decodeErr(t, tc.content)
		if codeOf(want) == "" {
			t.Fatalf("%s: default decode error = %v", tc.name, want)
		}
		for _, c := range caps {
			for _, w := range []int{1, 3} {
				// The resource and value limits take the huge caps only:
				// one resource or value is a limit of its own.
				limit := c
				if c > 0 && c < maxCount {
					limit = 0
				}
				dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: c, MaxResources: limit, MaxValues: limit})
				_, err := dec.Decode(context.Background(), []byte(tc.content), Source{Name: "c.json"})
				if errors.Is(err, errStop) || codeOf(err) != codeOf(want) || describe(DiagnosticsOf(err)) != describe(DiagnosticsOf(want)) {
					t.Errorf("%s, cap %d, workers %d: error = %v, want %v", tc.name, c, w, err, want)
				}
			}
		}
	}
	for _, tc := range []struct{ in, want int }{
		{-1, DefaultMaxDiagnostics},
		{0, DefaultMaxDiagnostics},
		{1, 1},
		{maxCount, maxCount},
		{maxCount + 1, maxCount},
		{math.MaxInt/2 + 1, maxCount},
		{math.MaxInt, maxCount},
	} {
		d := NewDecoder(nil, DecoderOptions{MaxDiagnostics: tc.in, MaxResources: tc.in, MaxValues: tc.in})
		wantRes, wantVal := tc.want, tc.want
		if tc.in <= 0 {
			wantRes, wantVal = DefaultMaxResources, DefaultMaxValues
		}
		if d.maxDiags != tc.want || d.maxResources != wantRes || d.maxValues != wantVal {
			t.Errorf("options %d: caps %d, %d, %d; want %d, %d, %d", tc.in, d.maxDiags, d.maxResources, d.maxValues, tc.want, wantRes, wantVal)
		}
	}
}

// TestChunkCount: the encoder's chunk count never multiplies past n, so
// no worker count, 1<<62 included, overflows it to zero chunks that drop
// every resource.
func TestChunkCount(t *testing.T) {
	for _, tc := range []struct{ n, workers, want int }{
		{0, 5, 1},
		{1, 5, 1},
		{10, 1, 1},
		{10, 0, 1},
		{91, 2, 8},
		{91, 22, 88},
		{91, 23, 91},
		{91, 1 << 62, 91},
		{91, math.MaxInt/4*3 + 1, 91},
		{91, math.MaxInt, 91},
		{math.MaxInt, math.MaxInt, math.MaxInt},
		{math.MaxInt, 2, 8},
	} {
		if got := chunkCount(tc.n, tc.workers); got != tc.want {
			t.Errorf("chunkCount(%d, %d) = %d, want %d", tc.n, tc.workers, got, tc.want)
		}
	}
	for _, tc := range []struct{ in, want int }{{-3, 1}, {0, 1}, {1, 1}, {256, 256}, {257, 256}, {math.MaxInt, 256}} {
		if got := workerCount(tc.in); got != tc.want {
			t.Errorf("workerCount(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestEncodeWorkersClamped: huge worker counts, 1<<62 and math.MaxInt,
// give the bytes of one worker, never an empty document, and no panic.
func TestEncodeWorkersClamped(t *testing.T) {
	b := largeBundle(t, 30)
	enc := NewEncoder(hubIndex(t))
	one, err := enc.Encode(context.Background(), b, Options{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{maxWorkers, maxWorkers + 1, 1 << 62, math.MaxInt/4*3 + 1, math.MaxInt} {
		got, err := enc.Encode(context.Background(), b, Options{Workers: w})
		if err != nil {
			t.Fatalf("workers %d: %v", w, err)
		}
		sameBytes(t, "workers "+strconv.Itoa(w), got.Content, one.Content)
	}
}

// route is the i-th of a run of Routes of five values each.
func route(i int) string { return `{"kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{}}` }

// TestDecodeLimits: the decoder's limits: content at a limit decodes, and
// one resource, value or level past it gives one RZ-CFG-001 finding
// located where it passes, the parse stopping there, for Decode, Verify
// and ReadDump, with one worker and with three. A document of n such
// Routes holds 3 + 5n values (document, format, resources and five per
// Route).
func TestDecodeLimits(t *testing.T) {
	routesDoc := func(n int) string { return resources(n, route) }
	lastSpec := func(c string) int { return strings.LastIndex(c, `{}`) + 1 }
	// nested returns a Plugin whose content nests depth levels.
	nested := func(depth int) string {
		return doc(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":` + strings.Repeat(`{"a":`, depth-4) + `1` + strings.Repeat(`}`, depth-4) + `}}`)
	}
	for _, tc := range []struct {
		name    string
		opts    DecoderOptions
		content string
		// msg is the limit finding's message, "" when the content decodes;
		// path and col locate it in the content.
		msg, path string
		col       int
	}{
		{"resources at the limit", DecoderOptions{MaxResources: 5}, routesDoc(5), "", "", 0},
		{"resources one past", DecoderOptions{MaxResources: 5}, routesDoc(6), "content holds more than 5 resources, the most this decoder reads", "resources[5]", len(header) + 5*(len(route(0))+1) + 1},
		{"resources past across chunks", DecoderOptions{MaxResources: 40}, routesDoc(100), "content holds more than 40 resources", "resources[40]", len(header) + 40*(len(route(0))+1) + 1},
		{"resources past, not objects", DecoderOptions{MaxResources: 2}, header + `1,2,3` + trailer, "more than 2 resources", "resources[2]", len(header) + 5},
		{"values at the limit", DecoderOptions{MaxValues: 3 + 5*7}, routesDoc(7), "", "", 0},
		{"values one past", DecoderOptions{MaxValues: 3 + 5*7 - 1}, routesDoc(7), "content holds more than 37 values, the most this decoder reads", "resources[6]", lastSpec(routesDoc(7))},
		{"values past at the resources array", DecoderOptions{MaxValues: 2}, doc(), "more than 2 values", "resources", len(header)},
		{"values past at a document member", DecoderOptions{MaxValues: 3}, `{"format":"ruralz.canonical.v1","resources":[],"x":[1,2]}`, "more than 3 values", "x", strings.Index(`{"format":"ruralz.canonical.v1","resources":[],"x":[1,2]}`, `[1`) + 1},
		{"values past at the document", DecoderOptions{MaxValues: 1}, doc(), "more than 1 values", "format", 11},
		{"depth at the limit", DecoderOptions{MaxDepth: 10}, nested(10), "", "", 0},
		{"depth one past", DecoderOptions{MaxDepth: 10}, nested(11), "nesting deeper than 10", "", strings.Index(nested(11), `{"a":{"a":{"a":{"a":{"a":{"a":{"a":1`) + 31},
		{"depth one past the default", DecoderOptions{}, nested(DefaultMaxDepth + 1), "nesting deeper than 128", "", 0},
		{"depth at the default", DecoderOptions{}, nested(DefaultMaxDepth), "", "", 0},
	} {
		for _, mode := range []string{"Decode", "Verify", "ReadDump"} {
			for _, w := range []int{1, 3} {
				o := tc.opts
				o.Workers = w
				err := decodeIn(t, NewDecoder(hubIndex(t), o), mode, tc.content)
				if tc.msg == "" {
					if err != nil {
						t.Errorf("%s, %s, workers %d: error = %v", tc.name, mode, w, err)
					}
					continue
				}
				ds := DiagnosticsOf(err)
				if codeOf(err) != CodeLimit || len(ds) != 1 || ds[0].Code != CodeLimit || !strings.Contains(ds[0].Message, tc.msg) || ds[0].Path.String() != tc.path || ds[0].Resource != nil {
					t.Errorf("%s, %s, workers %d: error = %v, diagnostics %v; want one %s %q at %q", tc.name, mode, w, err, ds, CodeLimit, tc.msg, tc.path)
					continue
				}
				if mode != "ReadDump" && tc.col > 0 && (ds[0].Line != 1 || ds[0].Column != tc.col) {
					t.Errorf("%s, %s, workers %d: at %d:%d, want 1:%d", tc.name, mode, w, ds[0].Line, ds[0].Column, tc.col)
				}
			}
		}
	}
}

// TestDecodeLimitPrecedence: a limit ends the judging where it occurs, as
// invalid JSON does: newer findings past the cap before it still give the
// first findings and RZ-CFG-001 "too many diagnostics", fewer give the
// limit alone, and a format other than ruralz.canonical.v1 before it
// gives that finding.
func TestDecodeLimitPrecedence(t *testing.T) {
	status := func(i int) string {
		return `{"kind":"Route","metadata":{"name":"r` + num(i) + `"},"spec":{},"status":{}}`
	}
	for _, tc := range []struct {
		name, content, code, first string
		count                      int
	}{
		{"newer past the cap, then the resource limit", resources(6, status), CodeNewer, "Route/r100000 status", 3},
		{"newer under the cap, then the resource limit", doc(status(0), route(1), route(2), route(3), route(4), route(5)), CodeLimit, "resources[5]", 1},
		{"another format, then the resource limit", `{"format":"x","resources":[` + strings.Join([]string{route(0), route(1), route(2), route(3), route(4), route(5)}, ",") + `]}`, CodeNewer, "format", 1},
		{"the resource limit, then another format", `{"resources":[` + strings.Join([]string{route(0), route(1), route(2), route(3), route(4), route(5)}, ",") + `],"format":"x"}`, CodeLimit, "resources[5]", 1},
	} {
		for _, w := range []int{1, 3} {
			_, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: 2, MaxResources: 5}).Decode(context.Background(), []byte(tc.content), Source{})
			ds := DiagnosticsOf(err)
			if codeOf(err) != tc.code || len(ds) != tc.count || describeOne(ds[0]) != tc.first {
				t.Errorf("%s, workers %d: error = %v, %d diagnostics; want %s with %d, first %q", tc.name, w, err, len(ds), tc.code, tc.count, tc.first)
			}
		}
	}
}

// streamRun runs the parse and checks of a decode and returns the
// document tree and the chunks.
func streamRun(t *testing.T, workers int, content []byte) (*tree.Node, []*chunk) {
	t.Helper()
	d := NewDecoder(hubIndex(t), DecoderOptions{Workers: workers, MaxDiagnostics: 100})
	doc, chunks, _, err := d.stream(context.Background(), newInput(content, 0, len(content), Source{}), newRun(d.maxDiags), nil)
	if err != nil {
		t.Fatal(err)
	}
	return doc, chunks
}

// TestDecodeRetention: a decode keeps the tree of an element only while it
// can still succeed, so a failing decode keeps no element alive until it
// returns, through the parser's items, the resources node or the chunks.
// The document tree holds no resources element, every chunk drops its items
// and spans once checked, and the entries kept are those of a decode that
// can succeed: all of them for valid content, none once a finding is known,
// those checked before it included, and none after the first resource whose
// re-encoding differs. One worker, so the order of the checks is the
// content order.
func TestDecodeRetention(t *testing.T) {
	const n = 3*streamChunk + 5
	valid := routes(n)
	spaced := bytes.Replace(valid, []byte(`"name":"r0050"},"spec":{`), []byte(`"name":"r0050"},"spec": {`), 1)
	newerLate := bytes.Replace(valid, []byte(`"name":"r0050"},"spec":{`), []byte(`"name":"r0050"},"spec":{"aa":1,`), 1)
	for _, tc := range []struct {
		name    string
		content []byte
		entries int
	}{
		{"valid", valid, n},
		{"a form finding first", []byte(strings.Replace(string(valid), header, header+`1,`, 1)), 0},
		{"a newer document member first", bytes.Replace(valid, []byte(`{"format"`), []byte(`{"aa":1,"format"`), 1), 0},
		{"a newer field at resource 50", newerLate, 0},
		{"a form finding at resource 50", bytes.Replace(valid, []byte(`{"kind":"Route","metadata":{"name":"r0050"}`), []byte(`{"apiVersion":"x","kind":"Route","metadata":{"name":"r0050"}`), 1), 0},
		{"a re-encoding difference at resource 50", spaced, 51},
	} {
		doc, chunks := streamRun(t, 1, tc.content)
		got := 0
		for _, c := range chunks {
			got += len(c.entries)
			if c.items != nil || c.spans != nil {
				t.Errorf("%s: chunk at %d keeps %d items", tc.name, c.lo, len(c.items))
			}
		}
		if got != tc.entries {
			t.Errorf("%s: %d entries kept, want %d", tc.name, got, tc.entries)
		}
		if res, ok := doc.Get("resources"); !ok || res.Kind != tree.KindList || len(res.Items) != 0 {
			t.Errorf("%s: the document's resources node holds %v", tc.name, res)
		}
		// The checks drop their chunks' items with several workers too.
		_, chunks = streamRun(t, 3, tc.content)
		for _, c := range chunks {
			if c.items != nil || c.spans != nil {
				t.Errorf("%s, workers 3: chunk at %d keeps its items", tc.name, c.lo)
			}
		}
	}
	// The outcome of a resource rejected by its re-encoding is the
	// sequence's first failure, as before the entries after it were
	// dropped.
	err := decodeErr(t, string(spaced))
	if ds := DiagnosticsOf(err); codeOf(err) != CodeMismatch || len(ds) != 1 || resText(ds[0]) != "Route/r0050" {
		t.Errorf("re-encoding difference: %v", err)
	}
}

// liveHeap returns the bytes of live heap objects.
func liveHeap() uint64 {
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// sampling is a context that measures the live heap. With at 0 it does
// so at every call, from whichever goroutine of the decode makes it, and
// keeps the largest. Otherwise at is a multiple of streamChunk: when the
// parser asks it for element at of the resources array, every element
// before is in a chunk the parser completed, and it waits until each of
// them has begun its check, so every chunk before at is checked (but for
// the last element of each) whatever the worker count and the scheduling;
// it then measures once when the parser asks it for element
// at+streamChunk, after completing the chunk at starts, which drops the
// trees a decode known to fail no longer needs (sweep). What is measured
// is what the decode keeps, not what its goroutines have not reached yet.
//
// With one worker the decode asks it in a fixed order (once, then for
// each element as the parser reads it and, once a chunk is complete, for
// each of its elements as they are checked), so element k's read is call
// 2k+2 and the measure needs no look at the stack.
type sampling struct {
	context.Context
	at              int64
	oneWorker       bool
	calls           atomic.Int64
	parsed, checked atomic.Int64
	mu              sync.Mutex
	live            uint64
}

func (s *sampling) Err() error {
	if s.at == 0 {
		s.sample()
		return nil
	}
	if s.oneWorker {
		if s.calls.Add(1) == 2*(s.at+streamChunk)+2 {
			s.sample()
		}
		return nil
	}
	pc := make([]uintptr, 1)
	if runtime.Callers(2, pc) != 1 {
		return nil
	}
	frame, _ := runtime.CallersFrames(pc).Next()
	switch {
	case strings.Contains(frame.Function, "(*checker).run"):
		s.checked.Add(1)
	case strings.Contains(frame.Function, "(*Decoder).stream"):
		switch s.parsed.Add(1) - 1 {
		case s.at:
			for s.checked.Load() < s.at {
				runtime.Gosched()
			}
		case s.at + streamChunk:
			s.sample()
		}
	}
	return nil
}

func (s *sampling) sample() {
	s.mu.Lock()
	s.live = max(s.live, liveHeap())
	s.mu.Unlock()
}

// scannerKeeps returns the live heap a bare jsonval.Scanner over content
// holds once it has read the start of element k of the resources array:
// the scratch it keeps of what it read before, chiefly the name records
// of the widest object, kept until Reset, which is the scanner's and not
// charged to the decoder.
func scannerKeeps(t *testing.T, content []byte, k int) float64 {
	t.Helper()
	base := liveHeap()
	s := jsonval.NewScanner(content, jsonval.ScanOptions{MaxDepth: DefaultMaxDepth})
	for seen := 0; ; {
		tok, err := s.Next()
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		// An element of resources is a scalar at depth 2 or a container
		// that opens depth 3.
		switch tok.Kind {
		case jsonval.KindObjectStart, jsonval.KindArrayStart:
			if s.Depth() == 3 {
				seen++
			}
		case jsonval.KindName, jsonval.KindObjectEnd, jsonval.KindArrayEnd:
		default:
			if s.Depth() == 2 {
				seen++
			}
		}
		if seen == k+1 {
			break
		}
	}
	kept := float64(liveHeap()) - float64(base)
	runtime.KeepAlive(s)
	return max(0, kept)
}

// zeros returns n zeros joined by commas.
func zeros(n int) string { return strings.TrimSuffix(strings.Repeat("0,", n), ",") }

// wideMembers returns n members "k<i>":0 joined by commas.
func wideMembers(n int) string {
	var b strings.Builder
	for i := range n {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"k` + strconv.Itoa(i) + `":0`)
	}
	return b.String()
}

// TestDecodeFailingLiveHeap: a decode known to fail holds no more than the
// content, the parser's copy of it and the elements in flight. Late in the
// decode of failing content (at the parser's read of an element some 90% of
// the way through, once the elements before it were checked; see sampling),
// the live heap beside the content and the parser's copy of it stays under
// 1 MiB (it measures about 200 KB), with one worker and with three: the
// elements, their chunks and, for Routes, the slabs they share with their
// neighbors are not kept (keeping them took 5 MB for the chunks of empty
// objects and 48 MB for the Routes), and neither is anything of an element
// checked early: the links of the parser's scratch stacks to a list or an
// object of 2^18 values and their width (32 MB beside 0.6 MB of content for
// a list, 41 MB beside 3 MB for an object), their stale links to the
// objects of a list of 4,000 or an object of 2,000 of 100 members each
// (45 MB), and the trees of 10,000 Routes checked before the decode was
// known to fail (8 MB). The scanner's own scratch (the name records of the
// widest object) is measured apart and not charged. What is measured is the
// live heap right after a forced collection, in a build without the race
// detector (raceEnabled).
func TestDecodeFailingLiveHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test")
	}
	if raceEnabled {
		t.Skip("measures memory, which the race detector changes and slows tenfold; the run without it measures it")
	}
	// One worker reads 4 MiB of short elements, three a sixteenth of it
	// (the stack look of the measure is slow), and the wide elements hold
	// 2^18 values.
	const wide, tail = 1 << 18, 2000
	// first, then tail Routes; every first element fails the decode with
	// a newer member.
	routesAfter := func(first ...string) (string, int) {
		rs := append([]string{}, first...)
		for i := range tail {
			rs = append(rs, route(i))
		}
		return doc(rs...), len(rs)
	}
	for _, tc := range []struct {
		name string
		gen  func(size int) (string, int)
	}{
		{"numbers", func(size int) (string, int) {
			n := size / 2
			return header + zeros(n) + trailer, n
		}},
		{"empty objects", func(size int) (string, int) {
			n := size / 3
			return header + strings.TrimSuffix(strings.Repeat("{},", n), ",") + trailer, n
		}},
		{"Routes after a form finding", func(size int) (string, int) {
			n := size / (len(route(0)) + 1)
			return header + "1," + strings.TrimPrefix(resources(n, route), header), n + 1
		}},
		{"a wide list first", func(int) (string, int) {
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":[` + zeros(wide) + `]},"zz":1}`)
		}},
		{"a wide object first", func(int) (string, int) {
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":{` + wideMembers(wide) + `}},"zz":1}`)
		}},
		{"a wide list in an object first", func(int) (string, int) {
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":{"a":[` + zeros(wide) + `]}},"zz":1}`)
		}},
		{"a wide list and a wide object first", func(int) (string, int) {
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":[`+zeros(wide)+`]},"zz":1}`,
				`{"kind":"Plugin","metadata":{"name":"q"},"spec":{"configSchema":{`+wideMembers(wide)+`}},"zz":1}`)
		}},
		// Containers narrow enough that the scratch stacks keep their
		// width (at most 4,096 slots, the inner objects' members on top of
		// the outer object's), whose stale links to thousands of objects of
		// 100 members would keep hundreds of slabs of nodes alive.
		{"a list of 4,000 objects first", func(int) (string, int) {
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":[` + strings.TrimSuffix(strings.Repeat(`{`+wideMembers(100)+`},`, 4000), ",") + `]},"zz":1}`)
		}},
		{"an object of 2,000 objects first", func(int) (string, int) {
			ms := make([]string, 2000)
			for i := range ms {
				ms[i] = `"o` + strconv.Itoa(i) + `":{` + wideMembers(100) + `}`
			}
			return routesAfter(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":{` + strings.Join(ms, ",") + `}},"zz":1}`)
		}},
		// Valid Routes checked and kept before the finding is known, then
		// dropped (sweep).
		{"10,000 Routes, then a newer field", func(int) (string, int) {
			rs := make([]string, 0, 10_000+1+tail)
			for i := range 10_000 {
				rs = append(rs, route(i))
			}
			rs = append(rs, `{"kind":"Route","metadata":{"name":"s"},"spec":{"zz":1}}`)
			for i := range tail {
				rs = append(rs, route(10_000+i))
			}
			return doc(rs...), len(rs)
		}},
		{"a newer field in a deep list item first", func(int) (string, int) {
			return routesAfter(`{"kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"u","zz":[` + zeros(wide) + `]}]}}`)
		}},
	} {
		for _, w := range []int{1, 3} {
			size := 4 << 20
			if w > 1 {
				size /= 16
			}
			c, elems := tc.gen(size)
			content := []byte(c)
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: 100, MaxResources: maxCount, MaxValues: maxCount})
			at := elems * 9 / 10 / streamChunk * streamChunk
			ctx := &sampling{Context: context.Background(), at: int64(at), oneWorker: w == 1}
			scanner := scannerKeeps(t, content, at+streamChunk)
			base := liveHeap()
			_, err := dec.Decode(ctx, content, Source{})
			if code := codeOf(err); (code != CodeMismatch && code != CodeNewer) || ctx.live == 0 {
				t.Fatalf("%s, workers %d: error = %v, sampled %d", tc.name, w, err, ctx.live)
			}
			held := float64(ctx.live) - float64(base) - float64(len(content)) - scanner
			t.Logf("%s, workers %d: %d elements, %.0f bytes held beside %d bytes of content and %.0f of the scanner's", tc.name, w, elems, held, len(content), scanner)
			if held > 1<<20 {
				t.Errorf("%s, workers %d: %.0f bytes held beside the content late in a failing decode of %d bytes, want at most 1 MiB", tc.name, w, held, len(content))
			}
			runtime.KeepAlive(content)
		}
	}
}

// TestDecodeInFlightBounded: the one element a failing decode builds in
// flight is bounded by MaxValues, not by the content: even a dump known to
// fail from its first member builds its one element in full (66 times its
// bytes for 4Mi zeros; at the defaults such an element reaches some 8 GiB,
// see the package documentation). With MaxValues 2^18 and an element of
// almost that many values, as a list and as an object, the peak live heap
// at any context check stays under 250 bytes per value of the limit (it
// measures 120 for the list and 195 for the object: a node and its item or
// member slot per value, and for the object the scanner's record of each
// member name, kept until the parse ends), and one value more is a limit,
// located there.
func TestDecodeInFlightBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test")
	}
	const limit, perValue = 1 << 18, 250
	// The document, its newer member aa (whose value fails the decode),
	// format and the resources array, then the Plugin, its kind,
	// metadata, name, spec and configSchema take 10 values, so the
	// content holds exactly MaxValues.
	const fill = limit - 10
	for _, tc := range []struct{ name, schema string }{
		{"list", `[` + zeros(fill) + `]`},
		{"object", `{` + wideMembers(fill) + `}`},
	} {
		content := []byte(`{"aa":1,"format":"ruralz.canonical.v1","resources":[{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":` + tc.schema + `}}]}`)
		for _, w := range []int{1, 3} {
			dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxValues: limit})
			ctx := &sampling{Context: context.Background()}
			base := liveHeap()
			_, err := dec.Decode(ctx, content, Source{})
			if codeOf(err) != CodeNewer || ctx.live == 0 {
				t.Fatalf("%s, workers %d: error = %v, sampled %d", tc.name, w, err, ctx.live)
			}
			held := float64(ctx.live) - float64(base) - float64(len(content))
			t.Logf("%s, workers %d: %.0f bytes held beside %d bytes of content, %.1f per value of the limit", tc.name, w, held, len(content), held/limit)
			if held > limit*perValue {
				t.Errorf("%s, workers %d: %.0f bytes held in flight, want at most %d per value of MaxValues %d", tc.name, w, held, perValue, limit)
			}
			runtime.KeepAlive(content)
		}
		over := bytes.Replace(content, []byte(`"configSchema":`+tc.schema[:1]), []byte(`"configSchema":`+tc.schema[:1]+`0,`), 1)
		if tc.name == "object" {
			over = bytes.Replace(content, []byte(`"configSchema":{`), []byte(`"configSchema":{"x":0,`), 1)
		}
		_, err := NewDecoder(hubIndex(t), DecoderOptions{MaxValues: limit}).Decode(context.Background(), over, Source{})
		if ds := DiagnosticsOf(err); codeOf(err) != CodeLimit || len(ds) != 1 || ds[0].Path.String() != "resources[0]" {
			t.Errorf("%s, one value more: error = %v", tc.name, err)
		}
	}
}

// TestDecodeAllocationPerByte: a failing decode allocates a bounded number
// of bytes per byte of the body. The slope of the bytes ReadDump allocates
// between 512 KiB and 2 MiB bodies of three hostile shapes stays under 90
// bytes per byte with the limits raised (it measures 50 to 75), and under
// 8 with the default limits, which stop the parse at the 20,001st
// resource, so reading the body and the parser's copy of the content are
// all a hostile dump of any size costs (it measures 3.5). One worker;
// bytes allocated, the least of three runs, not time or a sampled heap, in
// a build without the race detector (raceEnabled).
func TestDecodeAllocationPerByte(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation test")
	}
	if raceEnabled {
		t.Skip("measures allocation, which the race detector changes and slows tenfold; the run without it measures it")
	}
	alloc := func(dec *Decoder, body []byte) float64 {
		least := ^uint64(0)
		for range 3 {
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			_, _, _ = dec.ReadDump(context.Background(), bytes.NewReader(body), 0, Source{})
			runtime.ReadMemStats(&after)
			least = min(least, after.TotalAlloc-before.TotalAlloc)
		}
		return float64(least)
	}
	dump := func(elem string, size int) []byte {
		n := size / (len(elem) + 1)
		content := []byte(header + strings.TrimSuffix(strings.Repeat(elem+",", n), ",") + trailer)
		return dumpOf(t, nil, revision.Revision{Content: content, Digest: revision.Sum(content)}, revision.Digest{})
	}
	raised := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1, MaxResources: maxCount, MaxValues: maxCount})
	def := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1})
	for _, elem := range []string{"1", "{}", "[]"} {
		small, large := dump(elem, 512<<10), dump(elem, 2<<20)
		for _, tc := range []struct {
			name  string
			dec   *Decoder
			bound float64
		}{{"raised limits", raised, 90}, {"default limits", def, 8}} {
			slope := (alloc(tc.dec, large) - alloc(tc.dec, small)) / float64(len(large)-len(small))
			t.Logf("%s, %s: %.1f bytes allocated per body byte", elem, tc.name, slope)
			if slope > tc.bound {
				t.Errorf("%s, %s: %.1f bytes allocated per body byte, want at most %.0f", elem, tc.name, slope, tc.bound)
			}
		}
	}
}

// TestDecodeAllocsPerResource: a clean decode allocates at most about one
// object per resource, whatever their number, since the identity of a
// resource is built only when a finding needs it. The slope between N and
// 2N resources, for small Routes and for the ladder's Routes, Upstreams and
// Policies, is counted with testing.AllocsPerRun on one worker.
func TestDecodeAllocsPerResource(t *testing.T) {
	dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1})
	allocs := func(content []byte) float64 {
		return testing.AllocsPerRun(5, func() {
			if _, err := dec.Decode(context.Background(), content, Source{}); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, tc := range []struct {
		name   string
		a, b   []byte
		na, nb int
	}{
		{"Routes", routes(1000), routes(2000), 1000, 2000},
		{"ladder", mustEncodeBundle(t, largeBundle(t, 150)), mustEncodeBundle(t, largeBundle(t, 300)), 451, 901},
	} {
		slope := (allocs(tc.b) - allocs(tc.a)) / float64(tc.nb-tc.na)
		t.Logf("%s: %.2f allocations per resource", tc.name, slope)
		if slope > 1 {
			t.Errorf("%s: %.2f allocations per resource, want at most 1", tc.name, slope)
		}
	}
}

// TestFindingsKeepNoContent: an error keeps none of the content alive: no
// path element of a finding is a substring of the parser's copy. Each case
// is 16 MiB of content with one kind of content text in its findings: a
// field name, a resource name and kind, an unknown kind, a keyed entry's
// key, a document member name, a resource member name, a re-encoding path
// and a format; only the error is kept, and the live heap grows by less
// than 4 MiB.
func TestFindingsKeepNoContent(t *testing.T) {
	if testing.Short() {
		t.Skip("memory test")
	}
	// Each content is built inside the measured call and dropped with it.
	filler := func() string {
		return `{"kind":"Plugin","metadata":{"name":"zfill"},"spec":{"configSchema":{"a":"` + strings.Repeat("x", 16<<20) + `"}}}`
	}
	for _, tc := range []struct {
		name string
		gen  func() string
	}{
		{"field name", func() string {
			return doc(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":{"a":"` + strings.Repeat("x", 16<<20) + `"},"zz":1}}`)
		}},
		{"resource identity", func() string { return doc(`{"kind":"Route","metadata":{"name":"r"},"spec":{"zz":1}}`, filler()) }},
		{"unknown kind", func() string { return doc(filler(), `{"kind":"Zebra","metadata":{"name":"z"}}`) }},
		{"keyed entry key", func() string {
			return doc(`{"kind":"Gateway","metadata":{"name":"g"},"spec":{"listeners":[{"name":"l","zz":1}]}}`, filler())
		}},
		{"document member", func() string { return `{"aa":1,"format":"ruralz.canonical.v1","resources":[` + filler() + `]}` }},
		{"resource member", func() string { return doc(filler(), `{"kind":"Route","metadata":{"name":"r"},"spec":{},"status":1}`) }},
		{"apiVersion member", func() string {
			return doc(filler(), `{"apiVersion":"x","kind":"Route","metadata":{"name":"r"},"spec":{}}`)
		}},
		{"re-encoding path", func() string {
			return doc(`{"kind":"Plugin","metadata":{"name":"p"},"spec":{"configSchema":{"k":1e400}}}`, filler())
		}},
		{"format", func() string { return `{"resources":[` + filler() + `],"format":"v2"}` }},
	} {
		base := liveHeap()
		err := func() error {
			content := []byte(tc.gen())
			_, err := NewDecoder(hubIndex(t), DecoderOptions{Workers: 1}).Decode(context.Background(), content, Source{})
			return err
		}()
		if err == nil {
			t.Fatalf("%s: no error", tc.name)
		}
		kept := float64(liveHeap()) - float64(base)
		if kept > 4<<20 {
			t.Errorf("%s: the error keeps %.0f bytes alive: %.100v", tc.name, kept, err)
		}
		runtime.KeepAlive(err)
	}
}

// TestDecodeShallowValues: the values of a document member other than
// resources, and of a document that is not an object, are scanned, not
// built (only their kind and position matter), so they count one value
// each against MaxValues and cost no tree: a member holding a list, under
// a limit its elements would pass, is reported as the newer member it is,
// and so is a format holding an object; an array document as not an
// object; the scanner still judges their JSON.
func TestDecodeShallowValues(t *testing.T) {
	for _, tc := range []struct {
		name, content, code, msg string
	}{
		{"newer member", `{"format":"ruralz.canonical.v1","resources":[],"x":[1,2,3,4,5]}`, CodeNewer, "member is not part of"},
		{"format object", `{"format":{"a":[1,2,3,4,5]},"resources":[]}`, CodeNewer, "format {...} is not served"},
		{"resources not an array", `{"format":"ruralz.canonical.v1","resources":{"a":[1,2,3,4]}}`, CodeMismatch, "resources is not an array"},
		{"array document", `[1,2,3,4,5,6,7]`, CodeMismatch, "not a JSON object"},
		{"invalid JSON in a newer member", `{"format":"ruralz.canonical.v1","resources":[],"x":[1,,2]}`, CodeMismatch, "invalid JSON"},
		{"nesting in a newer member", `{"format":"ruralz.canonical.v1","resources":[],"x":[[[[[[[[[[1]]]]]]]]]]}`, CodeLimit, "nesting deeper than 8"},
	} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{MaxValues: 4, MaxDepth: 8})
		_, err := dec.Decode(context.Background(), []byte(tc.content), Source{})
		ds := DiagnosticsOf(err)
		if codeOf(err) != tc.code || len(ds) != 1 || !strings.Contains(ds[0].Message, tc.msg) {
			t.Errorf("%s: error = %v, want one %s %q", tc.name, err, tc.code, tc.msg)
		}
	}
}

// TestDecodeSweepsAfterFinding: the trees a decode checked before a finding
// was known are dropped once it is, while the parse goes on, with several
// workers too: the entries of chunks checked while another goroutine was
// still checking an earlier resource with a finding are not kept until the
// decode returns. The first resource has a newer field and the goroutines
// the decode starts are held until the parser reads element 320, so it
// checks chunk after chunk of valid Routes itself, keeping their entries,
// before the finding is offered; once every element before 320 has begun
// its check, the chunks the parser completes after drop them, so no chunk
// keeps an entry when the parse ends, and every tree the checks kept is
// unlinked. (With one worker the finding is known before any other check:
// TestDecodeRetention covers the sweep there.)
func TestDecodeSweepsAfterFinding(t *testing.T) {
	const n = 3000
	content := bytes.Replace(routes(n), []byte(`"timeout":"4s"}}`), []byte(`"timeout":"4s","zz":1}}`), 1)
	for _, w := range []int{1, 3} {
		d := NewDecoder(hubIndex(t), DecoderOptions{Workers: w, MaxDiagnostics: 100})
		r := newRun(d.maxDiags)
		c := newTracking(w > 1, n)
		c.release = 10 * streamChunk
		_, chunks, _, err := d.stream(c, newInput(content, 0, len(content), Source{}), r, nil)
		if err != nil || !r.newer.any() {
			t.Fatalf("workers %d: error %v, finding %v", w, err, r.newer.any())
		}
		kept := 0
		for _, c := range chunks {
			kept += len(c.entries)
		}
		if kept != 0 {
			t.Errorf("workers %d: %d entries kept after the decode is known to fail", w, kept)
		}
	}
	// sweep unlinks the trees of a done chunk's entries, and keeps a chunk
	// still in check.
	root := &tree.Node{Kind: tree.KindMap, Members: []tree.Member{{Key: "kind", Value: &tree.Node{Kind: tree.KindString}}}}
	done, busy := &chunk{entries: []entry{{node: root}}}, &chunk{lo: streamChunk, entries: []entry{{node: &tree.Node{}}}}
	done.done.Store(true)
	r := newRun(1)
	if got := sweep(r, []*chunk{done, busy}); len(got) != 2 || len(done.entries) != 1 {
		t.Errorf("before a finding: sweep kept %d chunks, %d entries", len(got), len(done.entries))
	}
	r.form.add(order{}, diag.Diagnostic{}, nil)
	got := sweep(r, []*chunk{done, busy})
	if len(got) != 1 || got[0] != busy || done.entries != nil || root.Members != nil || len(busy.entries) != 1 {
		t.Errorf("after a finding: sweep kept %v, done entries %v, root members %v", got, done.entries, root.Members)
	}
}

// TestNewerWalkKeepsNoItem: once a resource's walk is done, its frames'
// scratch holds no list item of it, so a resource the checks dropped is
// not kept alive by the walk while the next one is parsed.
func TestNewerWalkKeepsNoItem(t *testing.T) {
	d := NewDecoder(hubIndex(t), DecoderOptions{})
	c := d.newChecker(newInput(nil, 0, 0, Source{}), newRun(d.maxDiags), nil)
	rn := jsonTree(t, `{"kind":"Route","metadata":{"name":"r"},"spec":{"upstreams":[{"name":"u","weight":1}],"match":{"hosts":["a"]}}}`)
	c.found.reset(true)
	c.resource(rn, 0)
	if cap(c.nw.frames) == 0 {
		t.Fatal("the walk pushed no frame")
	}
	for i, f := range c.nw.frames[:cap(c.nw.frames)] {
		if f.item != nil || f.list != nil || f.name != "" || f.elem != nil {
			t.Errorf("frame %d of the scratch after the walk: %+v", i, f)
		}
	}
}
