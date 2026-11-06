// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// testRecords returns n records in a completion order that reorders
// sequence numbers and times, as concurrent workers finish them: every
// outcome, a request never sent, keyed and unkeyed requests, repeated and
// new codes and client addresses, and end times before the intended one
// never occurring.
func testRecords(n int) []Record {
	base := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	out := make([]Record, n)
	for i := range out {
		seq := uint64(i)
		if i%3 == 1 && i > 0 {
			seq = uint64(i - 1) // a later request finished first
		} else if i%3 == 0 && i > 0 {
			seq = uint64(i + 1)
		}
		r := Record{
			Seq: seq, Intended: base.Add(time.Duration(seq) * time.Millisecond),
			Outcome: loadgen.Outcome(i % int(loadgen.NumOutcomes)), Keyed: i%4 != 0,
		}
		if r.Outcome != loadgen.Missed {
			r.Sent = r.Intended.Add(time.Duration(i%7) * time.Microsecond)
			r.Done = r.Sent.Add(time.Duration(100+i%50) * time.Microsecond)
			r.Status = []int{200, 429, 503}[i%3]
			r.Local = "10.0.0.7:" + strconv.Itoa(40000+i%5)
		} else {
			r.Keyed = false
			r.Done = r.Intended.Add(time.Microsecond)
		}
		if r.Status >= 400 {
			r.Code = []string{"RZ-RL-002", "RZ-STS-001"}[i%2]
		}
		out[i] = r
	}
	return out
}

// TestRecordCodec: records decode as they were added, in order, across
// blocks, the deltas and dictionaries running on from block to block, no
// record straddling two blocks; the counts and sizes add up.
func TestRecordCodec(t *testing.T) {
	want := testRecords(500)
	e := newRecordEncoder(256)
	for _, r := range want {
		e.Add(r)
	}
	if e.Count() != int64(len(want)) || e.Blocks() < 10 {
		t.Fatalf("Count = %d, Blocks = %d; want %d records over many 256-byte blocks", e.Count(), e.Blocks(), len(want))
	}
	var size int64
	var d RecordDecoder
	var got []Record
	for i := range e.Blocks() {
		b, err := e.Block(i)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) > 256 {
			t.Errorf("block %d holds %d bytes, above its 256", i, len(b))
		}
		size += int64(len(b))
		before := len(got)
		if err := d.Decode(b, func(r Record) { got = append(got, r) }); err != nil {
			t.Fatalf("block %d: %v", i, err)
		}
		if len(got) == before {
			t.Errorf("block %d decodes no whole record", i)
		}
	}
	if size != e.Size() {
		t.Errorf("Size = %d, the blocks hold %d", e.Size(), size)
	}
	if len(got) != len(want) {
		t.Fatalf("decoded %d records, want %d", len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		if g.Seq != w.Seq || !g.Intended.Equal(w.Intended) || !g.Sent.Equal(w.Sent) || g.Sent.IsZero() != w.Sent.IsZero() ||
			!g.Done.Equal(w.Done) || g.Outcome != w.Outcome || g.Status != w.Status || g.Code != w.Code || g.Keyed != w.Keyed || g.Local != w.Local {
			t.Fatalf("record %d decoded %+v, want %+v", i, g, w)
		}
	}
	if _, err := e.Block(e.Blocks()); err == nil {
		t.Error("a block past the last answered")
	}
}

// TestRecordOf: a sample's record keeps whether it carried a key, not the
// key, and its connection's client address.
func TestRecordOf(t *testing.T) {
	at := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	s := loadgen.Sample{
		Request: loadgen.Request{Seq: 7, Intended: at}, Sent: at.Add(time.Millisecond), Done: at.Add(2 * time.Millisecond),
		Response: loadgen.Response{Outcome: loadgen.ClientError, Status: 429, Code: "RZ-RL-002", Key: "flood-7", Conn: loadgen.ConnInfo{Local: "10.0.0.7:40001"}},
	}
	r := RecordOf(s)
	if r.Seq != 7 || !r.Keyed || r.Local != "10.0.0.7:40001" || r.Code != "RZ-RL-002" || r.Status != 429 || !r.Done.Equal(s.Done) {
		t.Errorf("RecordOf = %+v", r)
	}
	if RecordOf(loadgen.Sample{Response: loadgen.Response{Outcome: loadgen.Missed}}).Keyed {
		t.Error("a sample without a key is keyed")
	}
}

// TestRecordDecodeRefuses: cut, unknown and inconsistent records fail to
// decode instead of yielding wrong ones.
func TestRecordDecodeRefuses(t *testing.T) {
	e := newRecordEncoder(1 << 20)
	for _, r := range testRecords(3) {
		e.Add(r)
	}
	b, err := e.Block(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
	}{
		{"cut", b[:len(b)-1]},
		{"unknown flag", append([]byte{0x80}, b[1:]...)},
		{"unknown outcome", append([]byte{0x0f}, b[1:]...)},
		{"dictionary index ahead", []byte{0, 0, 0, 0, 0, 9, 0}},
		{"cut dictionary entry", []byte{0, 0, 0, 0, 0, 1, 5, 'a'}},
	} {
		var d RecordDecoder
		if err := d.Decode(tc.data, func(Record) {}); !errors.Is(err, errRecords) {
			t.Errorf("%s: Decode = %v, want errRecords", tc.name, err)
		}
	}
}
