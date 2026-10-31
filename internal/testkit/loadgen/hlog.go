// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package loadgen

import (
	"bufio"
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
)

// The HdrHistogram V2 encoding: a 40-byte big-endian header (cookie,
// payload length, normalizing index offset, significant digits, lowest
// discernible value, highest trackable value, integer-to-double ratio)
// followed by the counts as ZigZag LEB128 varints, where a negative value
// is a run of empty slots; the compressed form is a cookie, the length and
// the zlib-deflated encoding. The cookies carry 0x10, the word-size bits
// of the V2 format.
const (
	encodingCookie     uint32 = 0x1c849303 | 0x10
	compressedCookie   uint32 = 0x1c849304 | 0x10
	cookieWordSizeMask uint32 = 0xf0
	encodingHeaderSize        = 40
	maxVarintBytes            = 9
)

// LogFormatVersion is the HdrHistogram interval log version LogWriter
// writes.
const LogFormatVersion = "1.3"

// logLegend is the column legend line of an interval log.
const logLegend = `"StartTimestamp","Interval_Length","Interval_Max","Interval_Compressed_Histogram"`

// maxValueUnitRatio scales Interval_Max: nanosecond values are written in
// milliseconds, the interval log's default.
const maxValueUnitRatio = 1e6

// maxLogLine bounds one interval log line a reader accepts (target).
const maxLogLine = 16 << 20

// Encode returns h in the compressed HdrHistogram V2 encoding.
func (h *Histogram) Encode() []byte {
	raw := h.appendV2(nil)
	var buf bytes.Buffer
	var head [8]byte
	binary.BigEndian.PutUint32(head[:4], compressedCookie)
	buf.Write(head[:])
	zw, err := zlib.NewWriterLevel(&buf, zlib.BestCompression)
	if err != nil {
		panic(err) // the level is a valid constant
	}
	_, _ = zw.Write(raw) // writes to a bytes.Buffer never fail
	_ = zw.Close()
	out := buf.Bytes()
	binary.BigEndian.PutUint32(out[4:8], uint32(len(out)-8)) //nolint:gosec // G115: a histogram encoding is far below 4 GiB
	return out
}

// appendV2 appends the uncompressed V2 encoding of h to dst.
func (h *Histogram) appendV2(dst []byte) []byte {
	start := len(dst)
	dst = binary.BigEndian.AppendUint32(dst, encodingCookie)
	dst = binary.BigEndian.AppendUint32(dst, 0)                 // payload length, set below
	dst = binary.BigEndian.AppendUint32(dst, 0)                 // normalizing index offset
	dst = binary.BigEndian.AppendUint32(dst, uint32(h.digits))  //nolint:gosec // G115: 0..5
	dst = binary.BigEndian.AppendUint64(dst, uint64(h.lowest))  //nolint:gosec // G115: positive
	dst = binary.BigEndian.AppendUint64(dst, uint64(h.highest)) //nolint:gosec // G115: positive
	dst = binary.BigEndian.AppendUint64(dst, math.Float64bits(1))
	payload := len(dst)
	limit := h.countsIndex(h.max) + 1
	for i := 0; i < limit; {
		c := h.countAt(i)
		i++
		if c == 0 {
			zeros := int64(1)
			for i < limit && h.countAt(i) == 0 {
				zeros++
				i++
			}
			if zeros > 1 {
				dst = appendZigZag(dst, -zeros)
				continue
			}
		}
		dst = appendZigZag(dst, int64(c)) //nolint:gosec // G115: counts stay far below 2^63
	}
	binary.BigEndian.PutUint32(dst[start+4:], uint32(len(dst)-payload)) //nolint:gosec // G115: bounded by the counts length
	return dst
}

// appendZigZag appends v as a ZigZag LEB128 varint of at most nine bytes:
// eight bytes of seven bits with a continuation bit, then one byte of
// eight bits.
func appendZigZag(dst []byte, v int64) []byte {
	u := uint64(v<<1) ^ uint64(v>>63) //nolint:gosec // G115: ZigZag maps the sign into bit 0
	for range 8 {
		if u>>7 == 0 {
			return append(dst, byte(u))
		}
		dst = append(dst, byte(u&0x7f|0x80)) //nolint:gosec // G115: masked to seven bits and the flag
		u >>= 7
	}
	return append(dst, byte(u))
}

// readZigZag decodes one varint of appendZigZag from b.
func readZigZag(b []byte) (v int64, n int, err error) {
	var u uint64
	for i, c := range b {
		if i == maxVarintBytes-1 {
			u |= uint64(c) << 56
			return unZigZag(u), maxVarintBytes, nil
		}
		u |= uint64(c&0x7f) << (7 * uint(i))
		if c&0x80 == 0 {
			return unZigZag(u), i + 1, nil
		}
	}
	return 0, 0, errors.New("loadgen: truncated histogram varint")
}

func unZigZag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// DecodeHistogram decodes the compressed HdrHistogram V2 encoding, from
// Encode or from any HdrHistogram implementation. Like HdrHistogram, the
// decoded minimum and maximum are the bounds of their equivalence ranges.
func DecodeHistogram(b []byte) (*Histogram, error) {
	if len(b) < 8 {
		return nil, errors.New("loadgen: histogram encoding shorter than its header")
	}
	if c := binary.BigEndian.Uint32(b); c&^cookieWordSizeMask != compressedCookie&^cookieWordSizeMask {
		return nil, fmt.Errorf("loadgen: cookie %#x is not a compressed V2 histogram", c)
	}
	n := binary.BigEndian.Uint32(b[4:])
	if int64(n) > int64(len(b)-8) {
		return nil, fmt.Errorf("loadgen: histogram encoding claims %d compressed bytes, has %d", n, len(b)-8)
	}
	zr, err := zlib.NewReader(bytes.NewReader(b[8 : 8+n]))
	if err != nil {
		return nil, fmt.Errorf("loadgen: histogram encoding: %w", err)
	}
	defer func() { _ = zr.Close() }()
	var head [encodingHeaderSize]byte
	if _, err := io.ReadFull(zr, head[:]); err != nil {
		return nil, fmt.Errorf("loadgen: histogram encoding header: %w", err)
	}
	h, payloadLen, err := decodeHeader(head[:])
	if err != nil {
		return nil, err
	}
	// The payload is bounded by the counts it can describe, so a hostile
	// length cannot make the reader allocate more.
	if payloadLen > int64(h.countsLen)*maxVarintBytes {
		return nil, fmt.Errorf("loadgen: histogram payload of %d bytes exceeds its %d slots", payloadLen, h.countsLen)
	}
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(zr, payload); err != nil {
		return nil, fmt.Errorf("loadgen: histogram payload: %w", err)
	}
	if err := h.fillCounts(payload); err != nil {
		return nil, err
	}
	return h, nil
}

// decodeHeader checks the uncompressed V2 header and returns an empty
// histogram with its settings and the payload length.
func decodeHeader(b []byte) (*Histogram, int64, error) {
	if c := binary.BigEndian.Uint32(b); c&^cookieWordSizeMask != encodingCookie&^cookieWordSizeMask {
		return nil, 0, fmt.Errorf("loadgen: cookie %#x is not a V2 histogram encoding", c)
	}
	payloadLen := int64(int32(binary.BigEndian.Uint32(b[4:])))        //nolint:gosec // G115: a signed 32-bit field
	if offset := int32(binary.BigEndian.Uint32(b[8:])); offset != 0 { //nolint:gosec // G115: a signed 32-bit field
		return nil, 0, fmt.Errorf("loadgen: histogram normalizing index offset %d is not supported", offset)
	}
	digits := int32(binary.BigEndian.Uint32(b[12:]))  //nolint:gosec // G115: a signed 32-bit field
	lowest := int64(binary.BigEndian.Uint64(b[16:]))  //nolint:gosec // G115: a signed 64-bit field
	highest := int64(binary.BigEndian.Uint64(b[24:])) //nolint:gosec // G115: a signed 64-bit field
	if payloadLen < 0 {
		return nil, 0, fmt.Errorf("loadgen: negative histogram payload length %d", payloadLen)
	}
	h, err := NewHistogram(time.Duration(lowest), time.Duration(highest), int(digits))
	if err != nil {
		return nil, 0, err
	}
	return h, payloadLen, nil
}

// fillCounts decodes the V2 counts payload into h.
func (h *Histogram) fillCounts(p []byte) error {
	idx, maxIdx, minIdx := 0, -1, -1
	for len(p) > 0 {
		v, n, err := readZigZag(p)
		if err != nil {
			return err
		}
		p = p[n:]
		if v < 0 {
			if -v > int64(h.countsLen-idx) {
				return fmt.Errorf("loadgen: histogram payload runs past its %d slots", h.countsLen)
			}
			idx += int(-v)
			continue
		}
		if idx >= h.countsLen {
			return fmt.Errorf("loadgen: histogram payload runs past its %d slots", h.countsLen)
		}
		if v > 0 {
			pg := h.pages[idx>>h.halfMagnitude]
			if pg == nil {
				pg = make([]uint64, h.subBucketHalf)
				h.pages[idx>>h.halfMagnitude] = pg
			}
			pg[idx&int(h.subBucketHalf-1)] = uint64(v)
			h.total += uint64(v)
			maxIdx = idx
			if minIdx < 0 && idx != 0 {
				minIdx = idx
			}
		}
		idx++
	}
	if maxIdx >= 0 {
		h.max = h.highestEquivalent(h.valueFromIndex(maxIdx))
	}
	if minIdx >= 0 {
		h.min = h.valueFromIndex(minIdx)
	}
	return nil
}

// LogWriter writes an HdrHistogram interval log (format version 1.3): the
// raw histogram logs a result record publishes (benchrecord
// Latency.HistogramFiles), readable by every HdrHistogram tool.
// Timestamps are seconds relative to the base time, Interval_Max is in
// milliseconds. A LogWriter is not safe for concurrent use.
type LogWriter struct {
	w    io.Writer
	base time.Time
}

// NewLogWriter writes the log header to w: the format version, the start
// time (which is also the base time of every interval), any comments and
// the column legend.
func NewLogWriter(w io.Writer, start time.Time, comments ...string) (*LogWriter, error) {
	var b strings.Builder
	b.WriteString("#[Histogram log format version " + LogFormatVersion + "]\n")
	for _, c := range comments {
		if strings.ContainsAny(c, "\r\n") {
			return nil, fmt.Errorf("loadgen: log comment %q spans lines", c)
		}
		b.WriteString("#" + c + "\n")
	}
	secs := epochSeconds(start)
	b.WriteString("#[StartTime: " + secs + " (seconds since epoch), " + start.UTC().Format("Mon Jan 02 15:04:05 MST 2006") + "]\n")
	b.WriteString("#[BaseTime: " + secs + " (seconds since epoch)]\n")
	b.WriteString(logLegend + "\n")
	if _, err := io.WriteString(w, b.String()); err != nil {
		return nil, fmt.Errorf("loadgen: write log header: %w", err)
	}
	return &LogWriter{w: w, base: start}, nil
}

// Write writes one interval from start to end, with an optional tag (no
// comma, space or line break).
func (lw *LogWriter) Write(tag string, start, end time.Time, h *Histogram) error {
	var b strings.Builder
	if tag != "" {
		if strings.ContainsAny(tag, ", \t\r\n") {
			return fmt.Errorf("loadgen: log tag %q holds a delimiter", tag)
		}
		b.WriteString("Tag=" + tag + ",")
	}
	b.WriteString(seconds(start.Sub(lw.base)) + "," + seconds(end.Sub(start)) + ",")
	b.WriteString(strconv.FormatFloat(float64(h.Max())/maxValueUnitRatio, 'f', 3, 64) + ",")
	b.WriteString(base64.StdEncoding.EncodeToString(h.Encode()) + "\n")
	if _, err := io.WriteString(lw.w, b.String()); err != nil {
		return fmt.Errorf("loadgen: write log interval: %w", err)
	}
	return nil
}

// epochSeconds formats t as seconds since the epoch with milliseconds.
func epochSeconds(t time.Time) string {
	ms := t.UnixMilli()
	sign := ""
	if ms < 0 {
		sign, ms = "-", -ms
	}
	return fmt.Sprintf("%s%d.%03d", sign, ms/1000, ms%1000)
}

// seconds formats d as seconds with milliseconds, rounded to the nearest
// millisecond.
func seconds(d time.Duration) string {
	ms := d.Round(time.Millisecond).Milliseconds()
	sign := ""
	if ms < 0 {
		sign, ms = "-", -ms
	}
	return fmt.Sprintf("%s%d.%03d", sign, ms/1000, ms%1000)
}

// Log is a parsed interval log.
type Log struct {
	// StartTime and BaseTime are the header times; zero when absent.
	StartTime, BaseTime time.Time
	// Intervals are the logged histograms in file order.
	Intervals []Interval
}

// Interval is one logged histogram.
type Interval struct {
	// Tag is the interval's tag, "" when untagged.
	Tag string
	// Start is the interval's absolute start time.
	Start time.Time
	// Length is the interval's length.
	Length time.Duration
	// Max is the logged Interval_Max column, converted back from
	// milliseconds.
	Max time.Duration
	// Histogram is the decoded histogram.
	Histogram *Histogram
}

// ReadLog parses an HdrHistogram interval log. Interval timestamps are
// relative to the BaseTime header; without one they are taken as
// relative to the StartTime header when they lie more than a year before
// it, as HdrHistogram's reader does, and as absolute otherwise.
func ReadLog(r io.Reader) (*Log, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), maxLogLine)
	l := &Log{}
	var base time.Duration // offset of interval timestamps from the epoch
	baseKnown := false
	line := 0
	for sc.Scan() {
		line++
		text := strings.TrimRight(sc.Text(), "\r")
		switch {
		case text == "":
			continue
		case strings.HasPrefix(text, "#[StartTime: "):
			t, err := headerTime(text, "#[StartTime: ")
			if err != nil {
				return nil, fmt.Errorf("loadgen: log line %d: %w", line, err)
			}
			l.StartTime = t
			continue
		case strings.HasPrefix(text, "#[BaseTime: "):
			t, err := headerTime(text, "#[BaseTime: ")
			if err != nil {
				return nil, fmt.Errorf("loadgen: log line %d: %w", line, err)
			}
			l.BaseTime, base, baseKnown = t, time.Duration(t.UnixNano()), true
			continue
		case strings.HasPrefix(text, "#"), strings.HasPrefix(text, `"StartTimestamp"`):
			continue
		}
		iv, err := parseInterval(text)
		if err != nil {
			return nil, fmt.Errorf("loadgen: log line %d: %w", line, err)
		}
		if !baseKnown {
			// The first interval decides: more than a year before the
			// start time means relative timestamps.
			const year = 365 * 24 * time.Hour
			if !l.StartTime.IsZero() && iv.offset < time.Duration(l.StartTime.UnixNano())-year {
				base = time.Duration(l.StartTime.UnixNano())
			}
			baseKnown = true
		}
		iv.Start = time.Unix(0, int64(base+iv.offset)).UTC()
		l.Intervals = append(l.Intervals, iv.Interval)
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("loadgen: read log: %w", err)
	}
	return l, nil
}

// headerTime parses the seconds that follow prefix in a header comment.
func headerTime(text, prefix string) (time.Time, error) {
	rest := strings.TrimPrefix(text, prefix)
	end := strings.IndexAny(rest, " ]")
	if end < 0 {
		end = len(rest)
	}
	d, err := parseSeconds(rest[:end])
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(0, int64(d)).UTC(), nil
}

type parsedInterval struct {
	Interval
	offset time.Duration
}

// parseInterval parses "[Tag=t,]start,length,max,histogram".
func parseInterval(text string) (parsedInterval, error) {
	var p parsedInterval
	if rest, ok := strings.CutPrefix(text, "Tag="); ok {
		tag, after, found := strings.Cut(rest, ",")
		if !found {
			return p, errors.New("tag without fields")
		}
		p.Tag, text = tag, after
	}
	f := strings.Split(text, ",")
	if len(f) != 4 {
		return p, fmt.Errorf("want 4 fields, got %d", len(f))
	}
	var err error
	if p.offset, err = parseSeconds(f[0]); err != nil {
		return p, fmt.Errorf("start timestamp: %w", err)
	}
	if p.Length, err = parseSeconds(f[1]); err != nil {
		return p, fmt.Errorf("interval length: %w", err)
	}
	ms, err := strconv.ParseFloat(f[2], 64)
	if err != nil || math.IsNaN(ms) || math.IsInf(ms, 0) {
		return p, fmt.Errorf("interval max %q is not a number", f[2])
	}
	p.Max = time.Duration(math.Round(ms * maxValueUnitRatio))
	raw, err := base64.StdEncoding.DecodeString(f[3])
	if err != nil {
		return p, fmt.Errorf("histogram: %w", err)
	}
	if p.Histogram, err = DecodeHistogram(raw); err != nil {
		return p, err
	}
	return p, nil
}

// parseSeconds parses decimal seconds such as "12.345" exactly, down to
// nanoseconds.
func parseSeconds(s string) (time.Duration, error) {
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" && frac == "" {
		return 0, errors.New("empty seconds")
	}
	if len(frac) > 9 {
		frac = frac[:9]
	}
	w := uint64(0)
	if whole != "" {
		var err error
		if w, err = strconv.ParseUint(whole, 10, 64); err != nil || w > math.MaxInt64/uint64(time.Second) {
			return 0, fmt.Errorf("seconds %q out of range", s)
		}
	}
	n := uint64(0)
	if frac != "" {
		var err error
		if n, err = strconv.ParseUint(frac+strings.Repeat("0", 9-len(frac)), 10, 64); err != nil {
			return 0, fmt.Errorf("seconds %q: %w", s, err)
		}
	}
	d := time.Duration(w)*time.Second + time.Duration(n) //nolint:gosec // G115: bounded above
	if neg {
		d = -d
	}
	return d, nil
}
