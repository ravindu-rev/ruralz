// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"
	"testing"
	"time"
)

func TestZigZagVarints(t *testing.T) {
	for _, c := range []struct {
		v    int64
		want []byte
	}{
		{0, []byte{0x00}},
		{-1, []byte{0x01}},
		{1, []byte{0x02}},
		{-3, []byte{0x05}},
		{63, []byte{0x7e}},
		{64, []byte{0x80, 0x01}},
		{-65, []byte{0x81, 0x01}},
		{math.MaxInt64, []byte{0xfe, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
		{math.MinInt64, []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}},
	} {
		got := appendZigZag(nil, c.v)
		if !bytes.Equal(got, c.want) {
			t.Errorf("appendZigZag(%d) = % x, want % x", c.v, got, c.want)
		}
		back, n, err := readZigZag(got)
		if err != nil || back != c.v || n != len(got) {
			t.Errorf("readZigZag(% x) = %d, %d, %v; want %d, %d", got, back, n, err, c.v, len(got))
		}
	}
	if _, _, err := readZigZag([]byte{0x80}); err == nil {
		t.Error("readZigZag of a truncated varint succeeded")
	}
}

// The encoding is HdrHistogram's compressed V2 format: a 0x1c849314
// cookie (log lines start "HISTF"), then zlib over a 0x1c849313 header
// and ZigZag LEB128 counts with runs of zeros.
func TestEncodeIsHdrHistogramV2(t *testing.T) {
	h := newHist(t, time.Nanosecond, 1<<20, 3)
	h.Record(3) // slot 3: counts 0, 0, 0, 1
	enc := h.Encode()
	if got := binary.BigEndian.Uint32(enc); got != 0x1c849314 {
		t.Fatalf("compressed cookie %#x, want 0x1c849314", got)
	}
	if got := int(binary.BigEndian.Uint32(enc[4:])); got != len(enc)-8 {
		t.Fatalf("compressed length %d, want %d", got, len(enc)-8)
	}
	if s := base64.StdEncoding.EncodeToString(enc); !strings.HasPrefix(s, "HISTF") {
		t.Fatalf("base64 %q does not start with HISTF", s[:8])
	}
	zr, err := zlib.NewReader(bytes.NewReader(enc[8:]))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{
		0x1c, 0x84, 0x93, 0x13, // cookie
		0, 0, 0, 2, // payload length
		0, 0, 0, 0, // normalizing index offset
		0, 0, 0, 3, // significant digits
		0, 0, 0, 0, 0, 0, 0, 1, // lowest discernible value
		0, 0, 0, 0, 0, 0x10, 0, 0, // highest trackable value 2^20
		0x3f, 0xf0, 0, 0, 0, 0, 0, 0, // integer-to-double ratio 1.0
		0x05, // -3: three empty slots
		0x02, // 1
	}
	if !bytes.Equal(raw, want) {
		t.Fatalf("encoding\n% x\nwant\n% x", raw, want)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	for _, digits := range []int{0, 2, 3, 5} {
		h := newHist(t, DefaultLowest, DefaultHighest, digits)
		for i := range 5000 {
			h.Record(time.Duration(i*i) * time.Microsecond)
		}
		h.RecordN(42*time.Second, 7)
		got, err := DecodeHistogram(h.Encode())
		if err != nil {
			t.Fatalf("digits %d: DecodeHistogram = %v", digits, err)
		}
		if got.Count() != h.Count() || got.Percentiles() != h.Percentiles() || got.Min() != h.Min() || got.Digits() != digits {
			t.Errorf("digits %d: decoded count %d %+v min %v, want %d %+v min %v",
				digits, got.Count(), got.Percentiles(), got.Min(), h.Count(), h.Percentiles(), h.Min())
		}
		gb, hb := got.Buckets(), h.Buckets()
		if len(gb) != len(hb) {
			t.Fatalf("digits %d: %d buckets, want %d", digits, len(gb), len(hb))
		}
		for i := range gb {
			if gb[i] != hb[i] {
				t.Fatalf("digits %d: bucket %d = %+v, want %+v", digits, i, gb[i], hb[i])
			}
		}
	}
	empty := newHist(t, DefaultLowest, DefaultHighest, 3)
	got, err := DecodeHistogram(empty.Encode())
	if err != nil || got.Count() != 0 {
		t.Fatalf("empty round trip: count %v, err %v", got, err)
	}
}

func TestDecodeHistogramRejects(t *testing.T) {
	valid := newHist(t, DefaultLowest, DefaultHighest, 3)
	valid.Record(time.Millisecond)
	enc := valid.Encode()
	compress := func(raw []byte) []byte {
		var b bytes.Buffer
		head := make([]byte, 8)
		binary.BigEndian.PutUint32(head, 0x1c849314)
		b.Write(head)
		zw := zlib.NewWriter(&b)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		out := b.Bytes()
		binary.BigEndian.PutUint32(out[4:], uint32(len(out)-8)) //nolint:gosec // G115: a test encoding of a few bytes
		return out
	}
	header := func(cookie uint32, payload uint32, digits uint32, lowest, highest uint64) []byte {
		b := binary.BigEndian.AppendUint32(nil, cookie)
		b = binary.BigEndian.AppendUint32(b, payload)
		b = binary.BigEndian.AppendUint32(b, 0)
		b = binary.BigEndian.AppendUint32(b, digits)
		b = binary.BigEndian.AppendUint64(b, lowest)
		b = binary.BigEndian.AppendUint64(b, highest)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(1))
	}
	for _, c := range []struct {
		name string
		b    []byte
	}{
		{"short", enc[:5]},
		{"wrong outer cookie", append([]byte{0, 0, 0, 0}, enc[4:]...)},
		{"length past end", append(append([]byte{}, enc[:4]...), 0xff, 0xff, 0xff, 0xff)},
		{"truncated zlib", enc[:len(enc)-4]},
		{"wrong inner cookie", compress(header(0x12345678, 0, 3, 1000, 1<<40))},
		{"bad settings", compress(header(0x1c849313, 0, 9, 1000, 1<<40))},
		// 23,552 empty slots fill a 1 µs to 2^40 ns histogram; a count after
		// them has no slot.
		{"count past slots", compress(append(header(0x1c849313, 4, 3, 1000, 1<<40), 0xff, 0xef, 0x02, 0x02))},
		{"payload length above slots", compress(header(0x1c849313, 1<<30, 3, 1000, 1<<40))},
		{"zero run past slots", compress(append(header(0x1c849313, 4, 3, 1000, 1<<40), 0xff, 0xff, 0xff, 0x0f))},
	} {
		if _, err := DecodeHistogram(c.b); err == nil {
			t.Errorf("%s: DecodeHistogram succeeded, want an error", c.name)
		}
	}
}

// LogWriter writes the HdrHistogram interval log; ReadLog reads it back.
func TestIntervalLogRoundTrip(t *testing.T) {
	startTime := time.Date(2026, 10, 29, 12, 0, 0, 250_000_000, time.UTC)
	var buf bytes.Buffer
	lw, err := NewLogWriter(&buf, startTime, "scenario S1")
	if err != nil {
		t.Fatal(err)
	}
	h1 := newHist(t, DefaultLowest, DefaultHighest, 3)
	h1.Record(1500 * time.Microsecond)
	h2 := newHist(t, DefaultLowest, DefaultHighest, 3)
	h2.Record(3 * time.Millisecond)
	h2.Record(4 * time.Millisecond)
	if err := lw.Write("", startTime, startTime.Add(10*time.Second), h1); err != nil {
		t.Fatal(err)
	}
	if err := lw.Write("node-1", startTime.Add(10*time.Second), startTime.Add(20*time.Second), h2); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(buf.String(), "\n")
	wantHead := []string{
		"#[Histogram log format version 1.3]",
		"#scenario S1",
		"#[StartTime: 1793275200.250 (seconds since epoch), Thu Oct 29 12:00:00 UTC 2026]",
		"#[BaseTime: 1793275200.250 (seconds since epoch)]",
		`"StartTimestamp","Interval_Length","Interval_Max","Interval_Compressed_Histogram"`,
	}
	for i, w := range wantHead {
		if lines[i] != w {
			t.Errorf("line %d = %q, want %q", i+1, lines[i], w)
		}
	}
	// Interval_Max is the top of the largest value's range, in ms.
	if want := fmt.Sprintf("0.000,10.000,%.3f,HISTF", float64(h1.Max())/1e6); !strings.HasPrefix(lines[5], want) {
		t.Errorf("first interval %q, want prefix %q", lines[5], want)
	}
	if want := fmt.Sprintf("Tag=node-1,10.000,10.000,%.3f,HISTF", float64(h2.Max())/1e6); !strings.HasPrefix(lines[6], want) {
		t.Errorf("second interval %q, want prefix %q", lines[6], want)
	}

	l, err := ReadLog(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if !l.StartTime.Equal(startTime) || !l.BaseTime.Equal(startTime) || len(l.Intervals) != 2 {
		t.Fatalf("log start %v base %v intervals %d", l.StartTime, l.BaseTime, len(l.Intervals))
	}
	iv := l.Intervals[1]
	if iv.Tag != "node-1" || !iv.Start.Equal(startTime.Add(10*time.Second)) || iv.Length != 10*time.Second ||
		iv.Histogram.Count() != 2 || iv.Histogram.Max() != h2.Max() || iv.Max != h2.Max().Round(time.Microsecond) {
		t.Errorf("interval 2 = %+v (count %d, max %v)", iv, iv.Histogram.Count(), iv.Histogram.Max())
	}

	if _, err := NewLogWriter(&buf, startTime, "two\nlines"); err == nil {
		t.Error("a comment with a line break was accepted")
	}
	if err := lw.Write("a,b", startTime, startTime, h1); err == nil {
		t.Error("a tag with a comma was accepted")
	}
}

// Without a BaseTime header, timestamps more than a year before the start
// time are relative to it, as HdrHistogram's reader decides.
func TestReadLogDeducesBaseTime(t *testing.T) {
	h := newHist(t, DefaultLowest, DefaultHighest, 3)
	h.Record(time.Millisecond)
	line := base64.StdEncoding.EncodeToString(h.Encode())
	rel := "#[StartTime: 1000000000.000 (seconds since epoch), x]\n5.500,1.000,1.000," + line + "\n"
	l, err := ReadLog(strings.NewReader(rel))
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(1000000005, 500_000_000).UTC(); !l.Intervals[0].Start.Equal(want) {
		t.Errorf("relative start %v, want %v", l.Intervals[0].Start, want)
	}
	abs := "#[StartTime: 1000000000.000 (seconds since epoch), x]\n1000000002.000,1.000,1.000," + line + "\n"
	if l, err = ReadLog(strings.NewReader(abs)); err != nil {
		t.Fatal(err)
	}
	if want := time.Unix(1000000002, 0).UTC(); !l.Intervals[0].Start.Equal(want) {
		t.Errorf("absolute start %v, want %v", l.Intervals[0].Start, want)
	}
	for _, bad := range []string{"1.0,2.0\n", "x,1.000,1.000," + line + "\n", "1.000,1.000,nan," + line + "\n", "1.000,1.000,1.000,!!!\n", "Tag=x\n"} {
		if _, err := ReadLog(strings.NewReader(bad)); err == nil {
			t.Errorf("ReadLog(%q) succeeded, want an error", bad)
		}
	}
}
