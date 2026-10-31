// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package canonical

import (
	"bytes"
	"context"
	"math"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ravindu-rev/ruralz/internal/config/revision"
)

// tracking is a context that records the distinct goroutines other than
// the caller's that ask it during a decode: exactly the goroutines the
// decode started that checked a resource, since every check asks the
// context first and goroutine ids are never reused. With hold set it also
// keeps each of them waiting in Err until the parser asks it for element
// release (by default the last element, so no chunk handed over is
// checked before the parse ends), and then keeps the parser waiting until
// every element of the chunks it completed before has begun its check;
// the parser yields
// the processor each time it asks, so a goroutine just started runs and
// takes its chunk before the parser goes on, whatever GOMAXPROCS is.
type tracking struct {
	context.Context
	caller  string
	hold    bool
	release int
	mu      sync.Mutex
	ids     map[string]bool
	asked   int
	checked atomic.Int64
	open    chan struct{}
}

func newTracking(hold bool, elements int) *tracking {
	return &tracking{Context: context.Background(), caller: goroutineID(), hold: hold, release: elements - 1, ids: map[string]bool{}, open: make(chan struct{})}
}

func (c *tracking) Err() error {
	id := goroutineID()
	if id == c.caller {
		pc := make([]uintptr, 1)
		if runtime.Callers(2, pc) == 1 {
			if frame, _ := runtime.CallersFrames(pc).Next(); strings.Contains(frame.Function, "(*Decoder).stream") {
				c.parsed()
				return nil
			}
		}
		c.checked.Add(1)
		return nil
	}
	c.mu.Lock()
	c.ids[id] = true
	c.mu.Unlock()
	c.checked.Add(1)
	if c.hold {
		<-c.open
	}
	return nil
}

// parsed is the parser asking the context for its next element.
func (c *tracking) parsed() {
	if !c.hold {
		return
	}
	c.mu.Lock()
	k := c.asked
	c.asked++
	c.mu.Unlock()
	if k == c.release {
		close(c.open)
		// The chunks the parser completed start at multiples of
		// streamChunk; the elements of the one it is filling wait for it.
		for flushed := k / streamChunk * streamChunk; c.checked.Load() < int64(flushed); {
			runtime.Gosched()
		}
	}
	runtime.Gosched()
}

// workers returns the number of distinct goroutines beside the caller's
// that asked c.
func (c *tracking) workers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

// boundsRun is the stream of one decode of content with ctx, and its run.
func boundsRun(ctx context.Context, t *testing.T, workers int, content []byte) *run {
	t.Helper()
	d := NewDecoder(hubIndex(t), DecoderOptions{Workers: workers})
	r := newRun(d.maxDiags)
	if _, _, _, err := d.stream(ctx, newInput(content, 0, len(content), Source{}), r, nil); err != nil {
		t.Fatal(err)
	}
	return r
}

// checkBounds fails unless the decode whose run is r and whose context is
// c kept the bounds of the package documentation (Concurrency): at most
// Workers-1 goroutines beside the caller's, never more than the chunks it
// handed over, every goroutine that checked a resource among them, and at
// most 2(Workers-1)+1 chunks in flight (Workers-1 waiting in the channel,
// one in each goroutine and the one the parser holds).
func checkBounds(t *testing.T, what string, workers int, r *run, c *tracking) {
	t.Helper()
	w := min(workerCount(workers), maxWorkers)
	if r.started > w-1 || r.started > r.handed {
		t.Errorf("%s: %d goroutines started for %d chunks handed over, want at most min(%d, %d)", what, r.started, r.handed, w-1, r.handed)
	}
	if got := c.workers(); got > r.started {
		t.Errorf("%s: %d goroutines beside the caller's checked resources, %d started", what, got, r.started)
	}
	if peak := r.peak.Load(); peak > int64(2*(w-1)+1) {
		t.Errorf("%s: %d chunks in flight at once, want at most %d", what, peak, 2*(w-1)+1)
	}
	if n := r.inFlight.Load(); n != 0 {
		t.Errorf("%s: %d chunks still in flight after the decode", what, n)
	}
}

// TestDecodeWorkersClamped: a decode with a huge worker count, 5,000 up to
// math.MaxInt, gives the trees of one worker, panics nowhere, and runs at
// most one goroutine per chunk it hands over. The goroutines are counted
// exactly, never sampled with runtime.NumGoroutine, whose count is the
// runtime's and includes goroutines of other calls still exiting: the
// decoder's own count of the goroutines it started, and the distinct
// goroutines that asked the context, which every check does first.
func TestDecodeWorkersClamped(t *testing.T) {
	ctx := context.Background()
	const n = 300 // nine full chunks and a last one the parser checks
	content := routes(n)
	want := mustDecode(t, content)
	rev := revision.Revision{Content: content, Digest: revision.Sum(content)}
	body := dumpOf(t, nil, rev, revision.Digest{})
	_, wantDump, err := NewDecoder(hubIndex(t), DecoderOptions{}).ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "c.json"})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []int{5000, 1 << 40, 1 << 50, math.MaxInt} {
		dec := NewDecoder(hubIndex(t), DecoderOptions{Workers: w})
		if dec.workers != maxWorkers {
			t.Errorf("workers %d: decoder runs %d", w, dec.workers)
		}
		c := newTracking(false, n)
		got, err := dec.Decode(c, content, Source{Name: "c.json"})
		if err != nil {
			t.Fatalf("workers %d: %v", w, err)
		}
		if d := diffResources(want, got); d != "" {
			t.Errorf("workers %d: %s", w, d)
		}
		if extra := c.workers(); extra > n/streamChunk {
			t.Errorf("workers %d: %d goroutines beside the caller's checked resources, want at most %d (one per chunk handed over)", w, extra, n/streamChunk)
		}
		c = newTracking(false, n)
		r := boundsRun(c, t, w, content)
		checkBounds(t, "workers "+strconv.Itoa(w), w, r, c)
		if r.handed > n/streamChunk {
			t.Errorf("workers %d: %d chunks handed over, the content fills %d", w, r.handed, n/streamChunk)
		}
		_, gotDump, err := dec.ReadDump(ctx, bytes.NewReader(body), 0, Source{Name: "c.json"})
		if err != nil {
			t.Fatalf("workers %d, dump: %v", w, err)
		}
		if d := diffResources(wantDump, gotDump); d != "" {
			t.Errorf("workers %d, dump: %s", w, d)
		}
	}
}

// TestDecodeGoroutineAndQueueBounds: a decode runs on at most Workers
// goroutines and keeps at most 2(Workers-1)+1 chunks in flight, through a
// channel of Workers-1 chunks (package documentation, Concurrency), so
// neither a goroutine per chunk handed over nor a channel of 4,096 chunks
// passes. Over 3,000 Routes (93 full chunks), with 2, 3 and 8 workers:
// first while the goroutines the decode starts are held until the parse
// ends, so every chunk handed over is in flight at once and the bounds are
// reached exactly: Workers-1 goroutines, each holding one chunk, and
// Workers-1 chunks in the channel, the parser checking the others itself;
// then free running, where the bounds hold whatever the scheduling.
func TestDecodeGoroutineAndQueueBounds(t *testing.T) {
	const n = 3000
	content := routes(n)
	for _, w := range []int{2, 3, 8} {
		held := newTracking(true, n)
		r := boundsRun(held, t, w, content)
		what := "workers " + strconv.Itoa(w) + ", held"
		checkBounds(t, what, w, r, held)
		if r.started < 1 || r.handed > 2*(w-1) {
			t.Errorf("%s: %d goroutines started, %d chunks handed over; want at least 1 and at most %d chunks", what, r.started, r.handed, 2*(w-1))
		}
		for range 5 {
			free := newTracking(false, n)
			checkBounds(t, "workers "+strconv.Itoa(w)+", free", w, boundsRun(free, t, w, content), free)
		}
	}
	// One worker checks every chunk on the calling goroutine.
	c := newTracking(false, n)
	r := boundsRun(c, t, 1, content)
	if r.started != 0 || r.handed != 0 || r.peak.Load() != 1 || c.workers() != 0 {
		t.Errorf("one worker: %d goroutines, %d chunks handed over, %d in flight, %d goroutines asked", r.started, r.handed, r.peak.Load(), c.workers())
	}
}
