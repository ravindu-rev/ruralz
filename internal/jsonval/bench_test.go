// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package jsonval

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

// Benchmarks. WP-02 "Done when" requires 0 allocations per scanned token:
// the Scan benchmarks report allocs/op, which must read 0, and tokens/op.
// The Decode and Append benchmarks feed the 07 req 84 and 89 budgets
// (validation.json-schema and transform.response on 1 KiB).

// readOrdersFile reads the 1 KiB orders document.
func readOrdersFile() ([]byte, error) { return os.ReadFile("testdata/orders.json") }

// largeDocument returns about 64 KiB: the orders document repeated in an
// array.
func largeDocument(b *testing.B) []byte {
	orders := readOrders(b)
	var buf bytes.Buffer
	buf.WriteByte('[')
	for i := 0; buf.Len() < 64<<10; i++ {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(orders)
	}
	buf.WriteByte(']')
	return buf.Bytes()
}

func benchmarkScan(b *testing.B, data []byte, o ScanOptions) {
	s := NewScanner(nil, o)
	tokens, err := scanTokens(s, data, o)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := scanTokens(s, data, o); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(tokens), "tokens/op")
}

func BenchmarkScan1KiB(b *testing.B) { benchmarkScan(b, readOrders(b), ScanOptions{}) }

func BenchmarkScan1KiBAllowDuplicates(b *testing.B) {
	benchmarkScan(b, readOrders(b), ScanOptions{AllowDuplicateNames: true})
}

func BenchmarkScan64KiB(b *testing.B) { benchmarkScan(b, largeDocument(b), ScanOptions{}) }

func BenchmarkValidate1KiB(b *testing.B) {
	data := readOrders(b)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if err := Validate(data, ScanOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDecode(b *testing.B, o Options) {
	data := readOrders(b)
	d := NewDecoder(o)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := d.Decode(data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecode1KiB(b *testing.B) { benchmarkDecode(b, Options{MaxCost: 4 << 10 << 10}) }

func BenchmarkDecode1KiBMap(b *testing.B) { benchmarkDecode(b, Options{MapObjects: true}) }

func BenchmarkDecode1KiBLastWins(b *testing.B) {
	benchmarkDecode(b, Options{AllowDuplicateNames: true})
}

func BenchmarkEncodingJSONDecode1KiB(b *testing.B) {
	// Reference point for the Decode benchmarks.
	data := readOrders(b)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkAppend(b *testing.B, o EncodeOptions) {
	v, _, err := Decode(readOrders(b), Options{})
	if err != nil {
		b.Fatal(err)
	}
	e := Encoder{Options: o}
	buf, err := e.Append(nil, v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buf)))
	b.ReportAllocs()
	for b.Loop() {
		if buf, err = e.Append(buf[:0], v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkAppend1KiB(b *testing.B) { benchmarkAppend(b, EncodeOptions{}) }

func BenchmarkAppendCanonical1KiB(b *testing.B) {
	benchmarkAppend(b, EncodeOptions{Order: OrderUTF16, Numbers: NumbersCanonical})
}

func BenchmarkAppendFloat(b *testing.B) {
	buf := make([]byte, 0, 32)
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = AppendFloat(buf[:0], 333333333.33333325)
	}
}

func BenchmarkAppendCanonicalNumber(b *testing.B) {
	buf := make([]byte, 0, 32)
	b.ReportAllocs()
	for b.Loop() {
		buf, _ = AppendCanonicalNumber(buf[:0], "1.2345678901234568e20")
	}
}

func BenchmarkIsJSONMediaType(b *testing.B) {
	b.ReportAllocs()
	for b.Loop() {
		if !IsJSONMediaType("application/vnd.api+json; charset=utf-8") {
			b.Fatal("not JSON")
		}
	}
}

func BenchmarkLocator64KiB(b *testing.B) {
	data := largeDocument(b)
	var l Locator
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for b.Loop() {
		l.Reset(data)
		for off := 0; off < len(data); off += 97 {
			_ = l.Position(off)
		}
	}
}
