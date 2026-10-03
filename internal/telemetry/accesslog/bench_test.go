// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package accesslog

import (
	"testing"
)

// BenchmarkAccessLogSubmit: the request-goroutine cost of one logged
// request after the handler decided to log it (Acquire, fill, Submit):
// 09 req 67 and 70 budget 2 µs at p99 and 8 allocations including `when`
// (evaluated by internal/gateway/handler, R-48); this part allocates 0.
func BenchmarkAccessLogSubmit(b *testing.B) {
	w := New(Options{NodeID: testNode})
	var buf []byte
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		r := w.Acquire()
		fullRecord(r)
		w.Submit(r)
		if i++; i%4096 == 0 {
			b.StopTimer()
			for {
				var ok bool
				if buf, ok = w.Next(buf[:0]); !ok {
					break
				}
			}
			w.Flushed(0)
			b.StartTimer()
		}
	}
}

// BenchmarkAccessLogEncode: the worker's encoding of one record.
func BenchmarkAccessLogEncode(b *testing.B) {
	w := New(Options{NodeID: testNode})
	var buf []byte
	b.ReportAllocs()
	for b.Loop() {
		b.StopTimer()
		r := w.Acquire()
		fullRecord(r)
		w.Submit(r)
		b.StartTimer()
		buf, _ = w.Next(buf[:0])
		w.Flushed(0)
	}
}

// BenchmarkAccessLogDrop: the cost of a record dropped by a full queue
// (blocked stdout).
func BenchmarkAccessLogDrop(b *testing.B) {
	w := New(Options{NodeID: testNode, QueueRecords: 1})
	w.Submit(w.Acquire())
	b.ReportAllocs()
	for b.Loop() {
		r := w.Acquire()
		fullRecord(r)
		w.Submit(r)
	}
}
