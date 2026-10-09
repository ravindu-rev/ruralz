// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package httpfield

import (
	"net/http"
	"strings"
	"testing"
)

// Benchmarks for the per-request helpers: Protect and IsHopByHop run for
// every field the Upstream layer forwards, NormalizeValue for every computed
// header value (07 req 27, 32), StripHopByHop once per leg attempt and
// response (07 req 33), and the query editor per transform query edit (07
// req 48). All but the query Set allocate nothing.

func BenchmarkProtect(b *testing.B) {
	names := []string{"Content-Type", "X-Forwarded-For", "Traceparent", "Proxy-Authorization", "Accept"}
	b.ReportAllocs()
	for b.Loop() {
		for _, n := range names {
			_ = Protect(n)
		}
	}
}

func BenchmarkNormalizeValue(b *testing.B) {
	v := " gold customer 12345 "
	b.ReportAllocs()
	for b.Loop() {
		if _, err := NormalizeValue(v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStripHopByHop(b *testing.B) {
	h := http.Header{
		"Accept": {"*/*"}, "Accept-Encoding": {"gzip"}, "Content-Type": {"application/json"},
		"User-Agent": {"bench"}, "X-Request-Id": {"1"}, "Authorization": {"Bearer x"},
		"X-Forwarded-For": {"1.2.3.4"}, "Traceparent": {"00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"},
	}
	conn, ka := []string{"keep-alive"}, []string{"timeout=5"}
	b.ReportAllocs()
	for b.Loop() {
		h["Connection"], h["Keep-Alive"] = conn, ka
		StripHopByHop(h, TowardUpstream)
	}
}

func BenchmarkQuerySet(b *testing.B) {
	const raw = "page=2&sort=desc&filter=a+b&lang=en&limit=50"
	var q Query
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	for b.Loop() {
		q.Reset(raw)
		q.Set("limit", "100")
		q.Del("sort")
		buf = q.AppendRaw(buf[:0])
	}
}

// BenchmarkStripHopByHopLongConnection is the regression case for a
// client-sized Connection value against 250 fields (07 req 33): the time
// per byte stays flat from 64 KiB to 256 KiB, 64 KiB stays well under 1 ms,
// and options over MaxNameBytes cost no more per byte than short ones.
func BenchmarkStripHopByHopLongConnection(b *testing.B) {
	long := strings.Repeat("x", MaxNameBytes+1)
	for _, bc := range []struct {
		name string
		conn string
		key  string // a field whose length the options share, so they are looked up
	}{
		{"alternating/64KiB", strings.Repeat("a,b,", 64<<10/4), ""},
		{"alternating/256KiB", strings.Repeat("a,b,", 256<<10/4), ""},
		{"alternating-looked-up/64KiB", strings.Repeat("a,b,", 64<<10/4), "C"},
		{"alternating-looked-up/256KiB", strings.Repeat("a,b,", 256<<10/4), "C"},
		// The key must not be one of the options: "Ab" is named by "ab"
		// and deleted in the first iteration, so later iterations would
		// look nothing up.
		{"distinct-looked-up/64KiB", distinctOptions(64 << 10), "A1"},
		{"repeated/64KiB", strings.Repeat("a,", 64<<10/2), ""},
		{"long-options/64KiB", strings.Repeat(long+","+long+"y,", 64<<10/(2*len(long)+3)), strings.Repeat("Z", 300)},
	} {
		b.Run(bc.name, func(b *testing.B) {
			h := adversarialHeader(250)
			if bc.key != "" {
				h[bc.key] = []string{"v"}
			}
			conn := []string{bc.conn}
			b.SetBytes(int64(len(bc.conn)))
			b.ReportAllocs()
			for b.Loop() {
				h["Connection"] = conn
				StripHopByHop(h, TowardUpstream)
			}
		})
	}
}

// distinctOptions returns a Connection value of about size bytes made of
// two-letter options that rarely repeat one another.
func distinctOptions(size int) string {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	var b strings.Builder
	for i := 0; b.Len() < size; i++ {
		b.WriteByte(letters[i%26])
		b.WriteByte(letters[i/26%26])
		b.WriteByte(',')
	}
	return b.String()
}
