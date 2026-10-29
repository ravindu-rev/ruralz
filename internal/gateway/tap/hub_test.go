// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package tap

import (
	"bytes"
	"encoding/json"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/adminapi"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// TestActiveTracksSubscribers checks O7's precondition: Active is false
// on a nil Hub and while nobody subscribes, true from the first
// subscriber to the last.
func TestActiveTracksSubscribers(t *testing.T) {
	var none *Hub
	if none.Active() || none.Subscribers() != 0 {
		t.Fatal("a nil Hub reports subscribers")
	}
	none.Publish(0, &Event{}) // a nil Hub ignores events
	h := New(Config{})
	if h.Active() {
		t.Fatal("Active without subscribers")
	}
	a, b := h.subscribe(1), h.subscribe(1)
	if !h.Active() || h.Subscribers() != 2 {
		t.Fatalf("Active %v, Subscribers %d; want true, 2", h.Active(), h.Subscribers())
	}
	h.unsubscribe(a)
	if !h.Active() {
		t.Fatal("Active false with one subscriber left")
	}
	h.unsubscribe(b)
	if h.Active() || h.Subscribers() != 0 {
		t.Fatalf("Active %v, Subscribers %d after the last left", h.Active(), h.Subscribers())
	}
}

// TestDocumentedLimits checks the /tap values of "Admin endpoints": 4
// subscribers with 1 MiB buffers, sampling at 1 by default.
func TestDocumentedLimits(t *testing.T) {
	if MaxSubscribers != 4 || BufferBytes != 1<<20 || DefaultSample != 1 {
		t.Fatalf("MaxSubscribers %d, BufferBytes %d, DefaultSample %v; want 4, 1 MiB, 1",
			MaxSubscribers, BufferBytes, DefaultSample)
	}
	if got := len(newSubscriber(1).buf); got != BufferBytes {
		t.Fatalf("subscriber buffer %d bytes, want %d", got, BufferBytes)
	}
}

// TestZeroHub checks that a Hub built without a Dropped counter, or not
// built by New at all, publishes and drops without failing.
func TestZeroHub(t *testing.T) {
	var h Hub
	s := h.subscribe(1)
	ev, _ := sparseEvent()
	for range BufferBytes/len(appendLine(nil, &ev)) + 2 {
		h.Publish(0, &ev)
	}
	s.mu.Lock()
	dropped := s.dropped
	s.mu.Unlock()
	if dropped != 2 {
		t.Fatalf("subscriber lost %d lines, want 2", dropped)
	}
}

// TestPublishWithoutSubscribersDrawsNothing checks that an exchange
// published while nobody subscribes is neither sampled nor encoded.
func TestPublishWithoutSubscribersDrawsNothing(t *testing.T) {
	src := &seqSource{vals: []uint64{0}}
	h := New(Config{Rand: src})
	ev, _ := fullEvent()
	h.Publish(0, &ev)
	s := h.subscribe(0.5)
	h.unsubscribe(s)
	h.Publish(0, &ev)
	if n := src.count(); n != 0 {
		t.Fatalf("%d draws without subscribers, want 0", n)
	}
}

// TestSamplingPerSubscriber checks ?sample=: each subscriber takes an
// exchange when its own uniform draw falls below rate × 2^64, a
// subscriber at rate 1 takes every exchange without a draw, and the
// event is encoded once for every taker.
func TestSamplingPerSubscriber(t *testing.T) {
	const half = uint64(1) << 63
	// Draws in subscriber order per Publish: quarter, half.
	src := &seqSource{vals: []uint64{
		half / 2, half / 2, // event 1: quarter misses (not below 2^62), half takes
		half/2 - 1, half - 1, // event 2: both take
		half, half, // event 3: both miss
	}}
	h := New(Config{Rand: src})
	all := h.subscribe(1)
	quarter := h.subscribe(0.25)
	halfSub := h.subscribe(0.5)
	for i := range 3 {
		ev, _ := sparseEvent()
		ev.Status = 200 + i
		h.Publish(0, &ev)
	}
	statuses := func(s *subscriber) []int {
		var out []int
		for _, l := range lines(t, drain(s)) {
			var e adminapi.TapEvent
			if err := json.Unmarshal(l, &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e.Status)
		}
		return out
	}
	for _, tc := range []struct {
		name string
		s    *subscriber
		want []int
	}{
		{"rate 1", all, []int{200, 201, 202}},
		{"rate 0.25", quarter, []int{201}},
		{"rate 0.5", halfSub, []int{200, 201}},
	} {
		if got := statuses(tc.s); !equalInts(got, tc.want) {
			t.Errorf("%s got statuses %v, want %v", tc.name, got, tc.want)
		}
	}
	if n := src.count(); n != 6 {
		t.Errorf("%d draws, want 6 (none for the rate 1 subscriber)", n)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSampleRate checks the ?sample= values: (0, 1], default 1; anything
// else is refused.
func TestSampleRate(t *testing.T) {
	for _, tc := range []struct {
		query string
		rate  float64
		ok    bool
	}{
		{"", 1, true},
		{"sample=1", 1, true},
		{"sample=0.25", 0.25, true},
		{"sample=1e-9", 1e-9, true},
		{"other=x&sample=0.5", 0.5, true},
		{"sample=0", 0, false},
		{"sample=-0.5", 0, false},
		{"sample=1.0001", 0, false},
		{"sample=NaN", 0, false},
		{"sample=Inf", 0, false},
		{"sample=abc", 0, false},
		{"sample=", 0, false},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		rate, ok := sampleRate(q)
		if rate != tc.rate || ok != tc.ok {
			t.Errorf("sampleRate(%q) = %v, %v; want %v, %v", tc.query, rate, ok, tc.rate, tc.ok)
		}
	}
}

// TestSubscriberLimit checks that at most MaxSubscribers subscribe at
// once and that a slot frees when one leaves.
func TestSubscriberLimit(t *testing.T) {
	h := New(Config{})
	var subs []*subscriber
	for range MaxSubscribers {
		s := h.subscribe(1)
		if s == nil {
			t.Fatalf("subscriber %d refused", len(subs)+1)
		}
		subs = append(subs, s)
	}
	if h.subscribe(1) != nil {
		t.Fatalf("subscriber %d admitted", MaxSubscribers+1)
	}
	h.unsubscribe(subs[0])
	if h.subscribe(1) == nil {
		t.Fatal("no slot after a subscriber left")
	}
}

// TestSlowSubscriberDrops checks the laggard rule: a line that does not
// fit the subscriber's BufferBytes buffer is dropped for it alone and
// counted on the publisher's stripe, and the subscriber gets one
// {"dropped":N} line right before its next event.
func TestSlowSubscriberDrops(t *testing.T) {
	dropped := &stripeCounter{}
	h := New(Config{Dropped: dropped})
	slow, fast := h.subscribe(1), h.subscribe(1)
	ev, _ := sparseEvent()
	lineLen := len(appendLine(nil, &ev))
	fit := BufferBytes / lineLen
	const lostEvents = 3
	var fastLines int
	for i := range fit + lostEvents {
		h.Publish(emit.Stripe(i%2), &ev)
		fastLines += len(lines(t, drain(fast)))
	}
	if fastLines != fit+lostEvents {
		t.Fatalf("fast subscriber got %d lines, want %d", fastLines, fit+lostEvents)
	}
	if got := dropped.total(); got != lostEvents {
		t.Fatalf("dropped = %d, want %d", got, lostEvents)
	}
	if dropped.stripe(0)+dropped.stripe(1) != lostEvents {
		t.Fatalf("drops not counted on the publishers' stripes: %v", dropped.by)
	}
	held := drain(slow)
	if len(held) != fit*lineLen || len(held) > BufferBytes {
		t.Fatalf("slow subscriber held %d bytes, want %d (at most %d)", len(held), fit*lineLen, BufferBytes)
	}
	ev.Status = 299
	h.Publish(0, &ev)
	got := lines(t, drain(slow))
	if len(got) != 2 {
		t.Fatalf("after draining got %d lines, want the notice and the event", len(got))
	}
	if want := `{"dropped":3}`; string(got[0]) != want {
		t.Fatalf("first line %s, want %s", got[0], want)
	}
	var e adminapi.TapEvent
	if err := json.Unmarshal(got[1], &e); err != nil || e.Status != 299 {
		t.Fatalf("second line %s: %v", got[1], err)
	}
	// The notice is sent once.
	h.Publish(0, &ev)
	if got := lines(t, drain(slow)); len(got) != 1 || bytes.Contains(got[0], []byte("dropped")) {
		t.Fatalf("next lines %q, want one event", got)
	}
}

// TestNoticeNeedsRoomWithItsEvent checks that a pending notice is written
// only together with the event that follows it; while both do not fit,
// that event is dropped too and the count grows.
func TestNoticeNeedsRoomWithItsEvent(t *testing.T) {
	dropped := &stripeCounter{}
	h := New(Config{Dropped: dropped})
	s := h.subscribe(1)
	ev, _ := sparseEvent()
	lineLen := len(appendLine(nil, &ev))
	fit := BufferBytes / lineLen
	for range fit + 1 {
		h.Publish(0, &ev)
	}
	// Free room for exactly one line: the next event fits alone, but not
	// with its notice.
	free := BufferBytes - fit*lineLen
	s.consume(lineLen - free)
	h.Publish(0, &ev)
	if got := dropped.total(); got != 2 {
		t.Fatalf("dropped = %d, want 2", got)
	}
	drain(s)
	h.Publish(0, &ev)
	got := lines(t, drain(s))
	if len(got) != 2 || string(got[0]) != `{"dropped":2}` {
		t.Fatalf("lines %q, want the notice of 2 and the event", got)
	}
}

// TestOversizedEventNeverDelivered checks the buffer bound: an event
// larger than BufferBytes is dropped and counted, never split.
func TestOversizedEventNeverDelivered(t *testing.T) {
	dropped := &stripeCounter{}
	h := New(Config{Dropped: dropped})
	s := h.subscribe(1)
	ev, _ := sparseEvent()
	ev.Host = strings.Repeat("h", BufferBytes)
	h.Publish(0, &ev)
	if got := dropped.total(); got != 1 {
		t.Fatalf("dropped = %d, want 1", got)
	}
	if held := drain(s); len(held) != 0 {
		t.Fatalf("held %d bytes, want 0", len(held))
	}
}

// TestRingWrapsWholeLines checks that lines crossing the end of the ring
// come out whole and in order.
func TestRingWrapsWholeLines(t *testing.T) {
	h := New(Config{})
	s := h.subscribe(1)
	ev, _ := sparseEvent()
	lineLen := len(appendLine(nil, &ev))
	// Move head near the end: fill, then release all but the last keep
	// bytes, the end of the last line.
	for range BufferBytes / lineLen {
		h.Publish(0, &ev)
	}
	keep := lineLen / 2
	s.consume(len(s.pending()) - keep)
	for i := range 5 {
		ev.Status = 500 + i
		h.Publish(0, &ev)
	}
	got := lines(t, drain(s)[keep:])
	if len(got) != 5 {
		t.Fatalf("%d lines after the wrap, want 5", len(got))
	}
	for i, l := range got {
		var e adminapi.TapEvent
		if err := json.Unmarshal(l, &e); err != nil || e.Status != 500+i {
			t.Fatalf("line %d %q: %v", i, l, err)
		}
	}
}

// TestUnsubscribedSubscriberIgnored checks that a Publish holding the
// list from before an unsubscribe neither writes to nor counts drops for
// the departed subscriber.
func TestUnsubscribedSubscriberIgnored(t *testing.T) {
	dropped := &stripeCounter{}
	h := New(Config{Dropped: dropped})
	s := h.subscribe(1)
	h.unsubscribe(s)
	ev, _ := sparseEvent()
	if got := s.offer(appendLine(nil, &ev)); got != gone {
		t.Fatalf("offer to a departed subscriber = %d, want gone", got)
	}
	if dropped.total() != 0 || len(drain(s)) != 0 {
		t.Fatal("a departed subscriber was written or counted")
	}
}

// TestConcurrentPublishAccounting checks, under -race, that concurrent
// publishers and a draining writer never interleave lines and that every
// published event is delivered, announced by a notice or still pending as
// a drop, with the counter equal to the drops.
func TestConcurrentPublishAccounting(t *testing.T) {
	dropped := &stripeCounter{}
	h := New(Config{Dropped: dropped})
	s := h.subscribe(1)
	const publishers, perPublisher = 8, 2000
	var out bytes.Buffer
	done := make(chan struct{})
	var wg sync.WaitGroup
	for p := range emit.Stripe(publishers) {
		wg.Go(func() {
			ev, _ := fullEvent()
			for i := range perPublisher {
				ev.Status = 100 + int(p)
				ev.ResponseBytes = int64(i)
				h.Publish(p, &ev)
			}
		})
	}
	writer := make(chan struct{})
	go func() {
		defer close(writer)
		for {
			select {
			case <-done:
				out.Write(drain(s))
				return
			case <-s.wake:
				out.Write(drain(s))
			}
		}
	}()
	wg.Wait()
	close(done)
	<-writer
	var delivered, announced uint64
	for _, l := range lines(t, out.Bytes()) {
		var d struct {
			Dropped uint64 `json:"dropped"`
		}
		if err := json.Unmarshal(l, &d); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		if d.Dropped > 0 {
			announced += d.Dropped
			continue
		}
		decodeEvent(t, l)
		delivered++
	}
	s.mu.Lock()
	pending := s.dropped
	s.mu.Unlock()
	if total := delivered + announced + pending; total != publishers*perPublisher {
		t.Fatalf("delivered %d + announced %d + pending %d = %d, want %d",
			delivered, announced, pending, total, publishers*perPublisher)
	}
	if got := dropped.total(); got != announced+pending {
		t.Fatalf("counter %d, want %d", got, announced+pending)
	}
}

// decodeEvent decodes one event line.
func decodeEvent(t *testing.T, l []byte) adminapi.TapEvent {
	t.Helper()
	var e adminapi.TapEvent
	if err := json.Unmarshal(l, &e); err != nil || e.TraceID == "" {
		t.Fatalf("line %q is not an event: %v", l, err)
	}
	return e
}
