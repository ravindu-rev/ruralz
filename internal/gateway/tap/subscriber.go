// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import "sync"

// offerResult is what offer did with a line.
type offerResult uint8

const (
	// delivered: the line, and a pending drop notice, are in the buffer.
	delivered offerResult = iota
	// lost: the buffer had no room; the line counts as dropped.
	lost
	// gone: the subscriber ended; nothing is counted.
	gone
)

// maxNotice is the longest {"dropped":N} line: N has at most 20 digits.
const maxNotice = len(`{"dropped":}`) + 20 + 1

// subscriber is one /tap subscription: its sampling threshold and a ring
// buffer of BufferBytes that publishers fill and the subscription's own
// request goroutine drains. A line enters the buffer whole or not at all.
type subscriber struct {
	// all is true at rate 1; otherwise a draw below threshold samples.
	all       bool
	threshold uint64
	// wake holds at most one pending wake-up for the writer.
	wake chan struct{}

	mu sync.Mutex
	// buf is the ring; head is its first unwritten byte and size the bytes
	// held, those the writer is writing included, so publishers never
	// touch them.
	buf  []byte
	head int
	size int
	// dropped counts the lines lost since the last delivered one.
	dropped uint64
	closed  bool
}

// newSubscriber returns a subscriber sampling at rate, in (0, 1].
func newSubscriber(rate float64) *subscriber {
	s := &subscriber{wake: make(chan struct{}, 1), buf: make([]byte, BufferBytes)}
	if rate >= 1 {
		s.all = true
	} else {
		// rate × 2^64 is below 2^64 for every rate below 1, so a uniform
		// 64-bit draw falls below it with probability rate.
		s.threshold = uint64(rate * (1 << 64))
	}
	return s
}

// offer appends line, preceded by the {"dropped":N} notice when lines
// were lost since the last delivered one, and wakes the writer.
func (s *subscriber) offer(line []byte) offerResult {
	var notice [maxNotice]byte
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return gone
	}
	var pre []byte
	if s.dropped > 0 {
		pre = appendDropped(notice[:0], s.dropped)
	}
	if len(pre)+len(line) > len(s.buf)-s.size {
		s.dropped++
		s.mu.Unlock()
		return lost
	}
	s.put(pre)
	s.put(line)
	s.dropped = 0
	s.mu.Unlock()
	select {
	case s.wake <- struct{}{}:
	default:
	}
	return delivered
}

// put copies b to the ring's free space, wrapping at its end; the caller
// holds mu and has checked the room.
func (s *subscriber) put(b []byte) {
	tail := s.head + s.size
	if tail >= len(s.buf) {
		tail -= len(s.buf)
	}
	n := copy(s.buf[tail:], b)
	copy(s.buf, b[n:])
	s.size += len(b)
}

// pending returns the held bytes from head up to the ring's end; empty
// when nothing is held. Publishers do not touch them until consume.
func (s *subscriber) pending() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	end := min(s.head+s.size, len(s.buf))
	return s.buf[s.head:end]
}

// consume releases n written bytes.
func (s *subscriber) consume(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.size -= n
	s.head += n
	if s.head == len(s.buf) || s.size == 0 {
		// An empty ring restarts at 0, so the next lines stay contiguous.
		s.head = 0
	}
}

// close ends the subscription for publishers.
func (s *subscriber) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
}
