// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"runtime"
	"strconv"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// ladder is the PBB Bundle ladder of the 02 benchmarks: about 116, 1,151,
// 5,751 and 11,501 resources (one Gateway plus Routes, Upstreams and
// Policies in equal numbers).
var ladder = []int{38, 383, 1916, 3833}

// BenchmarkEncode canonicalizes and hashes the ladder Bundles, part of the
// validation budget (target) and of the compile stage of a watched
// directory.
func BenchmarkEncode(b *testing.B) {
	for _, n := range ladder {
		bundle := largeBundle(b, n)
		for _, w := range []int{1, max(1, runtime.GOMAXPROCS(0)/2)} {
			b.Run(strconv.Itoa(len(bundle.Resources()))+"/workers-"+strconv.Itoa(w), func(b *testing.B) {
				enc := NewEncoder(hubIndex(b))
				b.ReportAllocs()
				var size int
				for b.Loop() {
					rev, err := enc.Encode(context.Background(), bundle, Options{Workers: w})
					if err != nil {
						b.Fatal(err)
					}
					size = len(rev.Content)
				}
				b.SetBytes(int64(size))
			})
		}
	}
}

// BenchmarkVerify is Decoder.Verify on the ladder Revisions; the
// 5,751-resource one must stay within the 25 ms verify stage (PBB "Hot
// Reload stages", target).
func BenchmarkVerify(b *testing.B) {
	for _, n := range ladder {
		content := mustEncodeBundle(b, largeBundle(b, n))
		want := revision.Sum(content)
		resources := strconv.Itoa(3*n + 1)
		for _, w := range []int{1, max(1, runtime.GOMAXPROCS(0)/2)} {
			b.Run(resources+"/workers-"+strconv.Itoa(w), func(b *testing.B) {
				dec := NewDecoder(hubIndex(b), DecoderOptions{Workers: w})
				b.ReportAllocs()
				b.SetBytes(int64(len(content)))
				for b.Loop() {
					if _, err := dec.Verify(context.Background(), content, want, Source{Name: "lkg.json"}); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}

// BenchmarkReadDump reads the /config/dump of the 5,751-resource Revision.
func BenchmarkReadDump(b *testing.B) {
	content := mustEncodeBundle(b, largeBundle(b, 1916))
	rev := revision.Revision{Digest: revision.Sum(content), Content: content}
	body := AppendDump(nil, rev, rev.Digest)
	dec := NewDecoder(hubIndex(b), DecoderOptions{Workers: max(1, runtime.GOMAXPROCS(0)/2)})
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for b.Loop() {
		if _, _, err := dec.ReadDump(context.Background(), bytes.NewReader(body), 0, Source{}); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendDump writes the /config/dump document of the
// 5,751-resource Revision, the admin handler's work per request.
func BenchmarkAppendDump(b *testing.B) {
	content := mustEncodeBundle(b, largeBundle(b, 1916))
	rev := revision.Revision{Digest: revision.Sum(content), Content: content}
	var buf []byte
	b.ReportAllocs()
	b.SetBytes(int64(len(content)))
	for b.Loop() {
		buf = AppendDump(buf[:0], rev, rev.Digest)
	}
}
