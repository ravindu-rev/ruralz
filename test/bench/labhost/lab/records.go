// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package lab

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ravindu-rev/ruralz/internal/testkit/loadgen"
)

// RecordBlockBytes bounds one block of a stream's encoded records, the
// unit the load service returns per OpRecords request: 16 MiB, about 22
// MiB as base64 in its Response line, below MaxLine.
const RecordBlockBytes = 16 << 20

// Record is one finished request of a load service stream: what the chaos
// harness keeps of each loadgen.Sample (test/chaos Request), but its key,
// which the harness derives from Seq with the stream's key source, a pure
// function of the sequence number (KeySpec), and the Node, which it
// attributes from the L4 balancer's connection records.
type Record struct {
	Seq      uint64
	Intended time.Time
	// Sent is zero for a request never sent (loadgen.Missed).
	Sent    time.Time
	Done    time.Time
	Outcome loadgen.Outcome
	Status  int
	Code    string
	// Keyed reports a request that carried its key header; a Missed
	// request carried none.
	Keyed bool
	// Local is the client address of the request's connection; "" when
	// it had none.
	Local string
}

// RecordOf returns the record of a sample.
func RecordOf(s loadgen.Sample) Record {
	return Record{
		Seq: s.Seq, Intended: s.Intended, Sent: s.Sent, Done: s.Done,
		Outcome: s.Outcome, Status: s.Status, Code: s.Code, Keyed: s.Key != "", Local: s.Conn.Local,
	}
}

// Flags of a record's first byte, beside the outcome in its low four bits.
const (
	recordSent  = 0x10
	recordKeyed = 0x20
	outcomeMask = 0x0f
)

// RecordEncoder keeps a stream's records in order, compactly: each record
// is its first byte (outcome and flags), then varints: the sequence
// number and the intended send time as zig-zag deltas from the previous
// record's, the send time's offset from the intended one, the end's from
// the send (else from the intended time), the status, and the code and
// the client address as indexes into dictionaries the stream builds as
// they first appear, a new entry written inline. A record never straddles
// two blocks of RecordBlockBytes. It is safe for concurrent use.
type RecordEncoder struct {
	blockBytes int

	mu       sync.Mutex
	blocks   [][]byte
	count    int64
	size     int64
	prevSeq  uint64
	prevTime int64
	codes    map[string]uint64
	locals   map[string]uint64
	scratch  []byte
}

// NewRecordEncoder returns an empty encoder of RecordBlockBytes blocks.
func NewRecordEncoder() *RecordEncoder { return newRecordEncoder(RecordBlockBytes) }

func newRecordEncoder(blockBytes int) *RecordEncoder {
	return &RecordEncoder{blockBytes: blockBytes, codes: map[string]uint64{}, locals: map[string]uint64{}}
}

func zigzag(v int64) uint64 { return uint64(v<<1) ^ uint64(v>>63) } //nolint:gosec // G115: zig-zag encoding maps the sign into bit 0

func unzigzag(u uint64) int64 { return int64(u>>1) ^ -int64(u&1) }

// Add appends r.
func (e *RecordEncoder) Add(r Record) {
	e.mu.Lock()
	defer e.mu.Unlock()
	b := e.scratch[:0]
	head := byte(r.Outcome) & outcomeMask
	if !r.Sent.IsZero() {
		head |= recordSent
	}
	if r.Keyed {
		head |= recordKeyed
	}
	b = append(b, head)
	b = binary.AppendUvarint(b, zigzag(int64(r.Seq-e.prevSeq))) //nolint:gosec // G115: a wrapping delta of sequence numbers
	e.prevSeq = r.Seq
	intended := r.Intended.UnixNano()
	b = binary.AppendUvarint(b, zigzag(intended-e.prevTime))
	e.prevTime = intended
	from := intended
	if !r.Sent.IsZero() {
		sent := r.Sent.UnixNano()
		b = binary.AppendUvarint(b, zigzag(sent-intended))
		from = sent
	}
	b = binary.AppendUvarint(b, zigzag(r.Done.UnixNano()-from))
	b = binary.AppendUvarint(b, uint64(max(r.Status, 0)))
	b = appendIndexed(b, e.codes, r.Code)
	b = appendIndexed(b, e.locals, r.Local)
	e.scratch = b
	if n := len(e.blocks); n == 0 || len(e.blocks[n-1])+len(b) > e.blockBytes {
		e.blocks = append(e.blocks, make([]byte, 0, min(e.blockBytes, max(len(b), 64<<10))))
	}
	last := &e.blocks[len(e.blocks)-1]
	*last = append(*last, b...)
	e.count++
	e.size += int64(len(b))
}

// appendIndexed appends s as an index into dict, 0 for "", writing a new
// entry inline after its index, one more than the entries so far.
func appendIndexed(b []byte, dict map[string]uint64, s string) []byte {
	if s == "" {
		return binary.AppendUvarint(b, 0)
	}
	if i, ok := dict[s]; ok {
		return binary.AppendUvarint(b, i)
	}
	i := uint64(len(dict)) + 1
	dict[s] = i
	b = binary.AppendUvarint(b, i)
	b = binary.AppendUvarint(b, uint64(len(s)))
	return append(b, s...)
}

// Count returns the records added and Size their encoded bytes.
func (e *RecordEncoder) Count() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.count
}

// Size returns the encoded bytes of the records added.
func (e *RecordEncoder) Size() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.size
}

// Blocks returns the number of blocks.
func (e *RecordEncoder) Blocks() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.blocks)
}

// Block returns block i, which a later Add never changes once a newer
// block exists, nor once adding ended.
func (e *RecordEncoder) Block(i int) ([]byte, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if i < 0 || i >= len(e.blocks) {
		return nil, fmt.Errorf("lab: record block %d of %d", i, len(e.blocks))
	}
	return e.blocks[i], nil
}

// errRecords marks encoded records that do not decode.
var errRecords = errors.New("lab: malformed records")

// RecordDecoder decodes a stream's blocks in order: the deltas and the
// dictionaries run on from block to block.
type RecordDecoder struct {
	prevSeq  uint64
	prevTime int64
	codes    []string
	locals   []string
}

// Decode calls fn for every record of block, in the order they were added.
func (d *RecordDecoder) Decode(block []byte, fn func(Record)) error {
	for len(block) > 0 {
		var r Record
		head := block[0]
		block = block[1:]
		r.Outcome = loadgen.Outcome(head & outcomeMask)
		if r.Outcome >= loadgen.NumOutcomes || head&^(outcomeMask|recordSent|recordKeyed) != 0 {
			return fmt.Errorf("%w: record head %#x", errRecords, head)
		}
		r.Keyed = head&recordKeyed != 0
		var err error
		var v uint64
		if v, block, err = uvarint(block); err != nil {
			return err
		}
		d.prevSeq += uint64(unzigzag(v)) //nolint:gosec // G115: a wrapping delta of sequence numbers
		r.Seq = d.prevSeq
		if v, block, err = uvarint(block); err != nil {
			return err
		}
		d.prevTime += unzigzag(v)
		r.Intended = time.Unix(0, d.prevTime)
		from := d.prevTime
		if head&recordSent != 0 {
			if v, block, err = uvarint(block); err != nil {
				return err
			}
			from += unzigzag(v)
			r.Sent = time.Unix(0, from)
		}
		if v, block, err = uvarint(block); err != nil {
			return err
		}
		r.Done = time.Unix(0, from+unzigzag(v))
		if v, block, err = uvarint(block); err != nil {
			return err
		}
		r.Status = int(v) //nolint:gosec // G115: an HTTP status the encoder wrote
		if r.Code, block, err = indexed(block, &d.codes); err != nil {
			return err
		}
		if r.Local, block, err = indexed(block, &d.locals); err != nil {
			return err
		}
		fn(r)
	}
	return nil
}

// uvarint reads one varint off b.
func uvarint(b []byte) (uint64, []byte, error) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, fmt.Errorf("%w: a varint is cut or overflows", errRecords)
	}
	return v, b[n:], nil
}

// indexed reads an index into dict off b, adding the new entry written
// inline.
func indexed(b []byte, dict *[]string) (string, []byte, error) {
	i, b, err := uvarint(b)
	if err != nil {
		return "", nil, err
	}
	switch {
	case i == 0:
		return "", b, nil
	case i <= uint64(len(*dict)):
		return (*dict)[i-1], b, nil
	case i != uint64(len(*dict))+1:
		return "", nil, fmt.Errorf("%w: dictionary index %d of %d", errRecords, i, len(*dict))
	}
	n, b, err := uvarint(b)
	if err != nil {
		return "", nil, err
	}
	if n > uint64(len(b)) {
		return "", nil, fmt.Errorf("%w: a dictionary entry is cut", errRecords)
	}
	s := string(b[:n])
	*dict = append(*dict, s)
	return s, b[n:], nil
}
