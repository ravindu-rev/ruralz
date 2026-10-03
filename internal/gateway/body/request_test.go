// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package body

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/ravindu-rev/ruralz/internal/gateway/snapshot"
)

// Tests for architecture R-41 (snapshot.RequestBody over the gate or the
// client stream), spec 05 req 31 (replayable when empty, gated or unread;
// GetBody only for buffered bodies), spec 05 req 54 (a gated body replayed
// to every composition step), spec 04 reqs 34 and 37 (declared length and
// streamed cut at maxRequestBodyBytes) and spec 07 req 71 (a rewritten body
// reserved, the superseded one released).

// clientBody is a client request body that records Close.
type clientBody struct {
	io.Reader
	closed bool
}

func (c *clientBody) Close() error {
	c.closed = true
	return nil
}

// replayState is one row of the replay matrix.
type replayState struct {
	name       string
	setup      func(t *testing.T, q *Request, bud *Budget) *clientBody
	length     int64
	replayable bool
	buffered   bool
	// open is the expected result of Open: "body" (the whole body from
	// its first byte), "nobody" (http.NoBody) or "refused"
	// (snapshot.ErrNotReplayable).
	first, second string
}

const matrixBody = "hello, upstream"

// TestR41ReplayMatrix is the RequestBody replay matrix of the work package
// "Done when": empty, gated, streamed unread and streamed read, each for
// ContentLength, Replayable, Buffered (GetBody) and two Opens (a retry or a
// second step).
func TestR41ReplayMatrix(t *testing.T) {
	gated := func(t *testing.T, q *Request, bud *Budget) *clientBody {
		cb := &clientBody{Reader: strings.NewReader(matrixBody)}
		if err := q.Prepare(context.Background(), cb, int64(len(matrixBody)), true, 1<<20, bud, 0); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		return cb
	}
	streamed := func(declared int64) func(t *testing.T, q *Request, bud *Budget) *clientBody {
		return func(t *testing.T, q *Request, bud *Budget) *clientBody {
			cb := &clientBody{Reader: strings.NewReader(matrixBody)}
			if err := q.Prepare(context.Background(), cb, declared, false, 1<<20, bud, 0); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			return cb
		}
	}
	tests := []replayState{
		{
			name: "empty (declared zero)",
			setup: func(t *testing.T, q *Request, bud *Budget) *clientBody {
				cb := &clientBody{Reader: strings.NewReader("")}
				if err := q.Prepare(context.Background(), cb, 0, false, 1<<20, bud, 0); err != nil {
					t.Fatal(err)
				}
				return cb
			},
			length: 0, replayable: true, first: "nobody", second: "nobody",
		},
		{
			name: "empty (http.NoBody, gate requested)",
			setup: func(t *testing.T, q *Request, bud *Budget) *clientBody {
				if err := q.Prepare(context.Background(), http.NoBody, -1, true, 1<<20, bud, 0); err != nil {
					t.Fatal(err)
				}
				return nil
			},
			length: 0, replayable: true, first: "nobody", second: "nobody",
		},
		{
			name:   "gated",
			setup:  gated,
			length: int64(len(matrixBody)), replayable: true, buffered: true, first: "body", second: "body",
		},
		{
			name:   "streamed unread (declared)",
			setup:  streamed(int64(len(matrixBody))),
			length: int64(len(matrixBody)), replayable: true, first: "body", second: "refused",
		},
		{
			name:   "streamed unread (unknown length)",
			setup:  streamed(-1),
			length: -1, replayable: true, first: "body", second: "refused",
		},
		{
			name: "streamed read",
			setup: func(t *testing.T, q *Request, bud *Budget) *clientBody {
				cb := streamed(-1)(t, q, bud)
				rc, err := q.Open()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := rc.Read(make([]byte, 3)); err != nil {
					t.Fatal(err)
				}
				_ = rc.Close()
				return cb
			},
			length: -1, replayable: false, first: "refused", second: "refused",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bud := NewBudget(1<<20, nil)
			var q Request
			cb := tt.setup(t, &q, bud)
			var _ snapshot.RequestBody = &q
			if q.ContentLength() != tt.length {
				t.Fatalf("ContentLength = %d, want %d", q.ContentLength(), tt.length)
			}
			if q.Replayable() != tt.replayable {
				t.Fatalf("Replayable = %v, want %v", q.Replayable(), tt.replayable)
			}
			if b := q.Buffered(); (b != nil) != tt.buffered || (tt.buffered && string(b) != matrixBody) {
				t.Fatalf("Buffered = %q, want buffered %v", b, tt.buffered)
			}
			for i, want := range []string{tt.first, tt.second} {
				rc, err := q.Open()
				switch want {
				case "refused":
					if !errors.Is(err, snapshot.ErrNotReplayable) || rc != nil {
						t.Fatalf("Open %d = %v, %v; want ErrNotReplayable", i+1, rc, err)
					}
					continue
				case "nobody":
					if err != nil || rc != http.NoBody {
						t.Fatalf("Open %d = %v, %v; want http.NoBody", i+1, rc, err)
					}
					continue
				}
				if err != nil {
					t.Fatalf("Open %d: %v", i+1, err)
				}
				got, err := io.ReadAll(rc)
				if err != nil || string(got) != matrixBody {
					t.Fatalf("Open %d read %q, %v", i+1, got, err)
				}
				if err := rc.Close(); err != nil {
					t.Fatalf("Close: %v", err)
				}
			}
			if cb != nil && cb.closed {
				t.Fatal("closing a reader closed the client stream")
			}
			q.Reset()
			if bud.Used() != 0 {
				t.Fatalf("Used = %d after Reset", bud.Used())
			}
			if q.ContentLength() != 0 || !q.Replayable() || q.Buffered() != nil {
				t.Fatal("Reset did not leave an empty body")
			}
		})
	}
}

// TestReq31StreamedRetryAfterUnreadAttempt: an attempt that opened the
// stream but read nothing (a connect error) leaves it replayable; the new
// reader makes the old one stale, so a lingering transport cannot steal
// bytes from the retry.
func TestReq31StreamedRetryAfterUnreadAttempt(t *testing.T) {
	var q Request
	cb := &clientBody{Reader: strings.NewReader(matrixBody)}
	q.SetStream(cb, -1, 1<<20)
	first, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	_ = first.Close()
	if !q.Replayable() {
		t.Fatal("an unread stream is not replayable")
	}
	second, err := q.Open()
	if err != nil {
		t.Fatalf("Open for the retry: %v", err)
	}
	if n, err := first.Read(make([]byte, 4)); n != 0 || !errors.Is(err, errStale) {
		t.Fatalf("stale reader Read = %d, %v", n, err)
	}
	got, err := io.ReadAll(second)
	if err != nil || string(got) != matrixBody {
		t.Fatalf("retry read %q, %v", got, err)
	}
	if q.BytesRead() != int64(len(matrixBody)) || q.Replayable() {
		t.Fatalf("BytesRead %d, replayable %v", q.BytesRead(), q.Replayable())
	}
	// A closed reader reads nothing more.
	if _, err := second.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("Read at the end = %v", err)
	}
	_ = second.Close()
	if _, err := second.Read(make([]byte, 1)); !errors.Is(err, errStale) {
		t.Fatalf("Read after Close = %v", err)
	}
	q.Reset()
}

// blockingReader blocks its first Read until released.
type blockingReader struct {
	entered chan struct{}
	release chan struct{}
	r       io.Reader
}

func (b *blockingReader) Read(p []byte) (int, error) {
	if b.entered != nil {
		close(b.entered)
		b.entered = nil
		<-b.release
	}
	return b.r.Read(p)
}

// TestReq31StreamedReadInProgress: while a transport goroutine is blocked
// reading the stream, the body is not replayable (the outcome of that read
// is unknown), a second Open is refused, and Reset makes the reader stale
// without waiting for it.
func TestReq31StreamedReadInProgress(t *testing.T) {
	var q Request
	br := &blockingReader{entered: make(chan struct{}), release: make(chan struct{}), r: strings.NewReader(matrixBody)}
	entered := br.entered
	q.SetStream(br, -1, 1<<20)
	rc, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := rc.Read(make([]byte, 64))
		done <- err
	}()
	<-entered
	if q.Replayable() {
		t.Fatal("replayable while a read is in progress")
	}
	if _, err := q.Open(); !errors.Is(err, snapshot.ErrNotReplayable) {
		t.Fatalf("Open during a read = %v", err)
	}
	q.Reset() // must not wait for the blocked read
	close(br.release)
	if err := <-done; err != nil {
		t.Fatalf("the in-progress read failed: %v", err)
	}
	if _, err := rc.Read(make([]byte, 4)); !errors.Is(err, errStale) {
		t.Fatalf("Read after Reset = %v, want stale", err)
	}
}

// TestReq37StreamedOverLimit: a streamed body is cut at the limit with
// ErrTooLarge (413 RZ-RT-003 before commit) and reported by Exceeded.
func TestReq37StreamedOverLimit(t *testing.T) {
	var q Request
	if err := q.Prepare(context.Background(), strings.NewReader(matrixBody), -1, false, 5, nil, 0); err != nil {
		t.Fatal(err)
	}
	rc, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	if !errors.Is(err, ErrTooLarge) || len(got) > 5 {
		t.Fatalf("read %q, %v", got, err)
	}
	if !q.Exceeded() || q.BytesRead() != int64(len(got)) {
		t.Fatalf("Exceeded %v, BytesRead %d", q.Exceeded(), q.BytesRead())
	}
	if Code(err, SideRequest) != "RZ-RT-003" {
		t.Fatalf("code %q", Code(err, SideRequest))
	}
	q.Reset()
}

// TestReq31StreamedEmpty: a stream of unknown length that turns out empty
// stays replayable and later Opens return http.NoBody.
func TestReq31StreamedEmpty(t *testing.T) {
	var q Request
	q.SetStream(strings.NewReader(""), -1, 10)
	rc, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read = %d, %v", n, err)
	}
	if n, err := rc.Read(make([]byte, 4)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("Read at EOF = %d, %v", n, err)
	}
	if !q.Replayable() {
		t.Fatal("an empty stream is not replayable")
	}
	if rc2, err := q.Open(); err != nil || rc2 != http.NoBody {
		t.Fatalf("Open = %v, %v", rc2, err)
	}
	if q.Exceeded() || q.Streamed() != true {
		t.Fatal("state")
	}
	q.Reset()
	if _, err := q.Open(); err != nil {
		t.Fatalf("Open after Reset = %v", err)
	}
}

// TestReq54GatedStepReplay opens a gated body from parallel composition
// steps: each reads the whole body independently (run with -race).
func TestReq54GatedStepReplay(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	data := pattern(100 * kib)
	var q Request
	if err := q.Prepare(context.Background(), bytes.NewReader(data), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 16 {
		rc, err := q.Open()
		if err != nil {
			t.Fatal(err)
		}
		wg.Go(func() {
			defer func() { _ = rc.Close() }()
			got, err := io.ReadAll(iotest.OneByteReader(io.LimitReader(rc, 1<<30)))
			if err != nil || !bytes.Equal(got, data) {
				t.Errorf("step read %d bytes, %v", len(got), err)
			}
		})
	}
	wg.Wait()
	if q.BytesRead() != int64(len(data)) || !q.Replayable() {
		t.Fatalf("BytesRead %d", q.BytesRead())
	}
	q.Reset()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestR41GatedReaderStaleAfterReset: a reader a transport still holds after
// the request ended never reads recycled bytes.
func TestR41GatedReaderStaleAfterReset(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	var q Request
	if err := q.Prepare(context.Background(), strings.NewReader("secret-a"), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	rc, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	q.Reset()
	if err := q.Prepare(context.Background(), strings.NewReader("secret-b"), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	if n, err := rc.Read(make([]byte, 16)); n != 0 || !errors.Is(err, errStale) {
		t.Fatalf("stale reader read %d bytes, %v", n, err)
	}
	_ = rc.Close()
	if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, errStale) {
		t.Fatalf("closed reader = %v", err)
	}
	q.Reset()
}

// TestReq71Replace: a rewritten body is reserved like a decoded value, the
// superseded gate is released, earlier readers turn stale; a refused
// reservation is ErrBudget (503 RZ-RT-004) and leaves the body unchanged.
func TestReq71Replace(t *testing.T) {
	// 256 KiB total, 64 KiB share: 192 KiB of gate budget.
	bud := NewBudget(256*kib, nil)
	var q Request
	if err := q.Prepare(context.Background(), bytes.NewReader(pattern(100*kib)), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	if bud.GateUsed() != 128*kib {
		t.Fatalf("gate reserved %d", bud.GateUsed())
	}
	old, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	// 100 KiB more does not fit next to the 128 KiB gate.
	if err := q.Replace(pattern(100 * kib)); !errors.Is(err, ErrBudget) || Code(err, SideRequest) != "RZ-RT-004" {
		t.Fatalf("Replace over the budget = %v", err)
	}
	if b, ok := q.Gated(); !ok || len(b) != 100*kib {
		t.Fatal("a refused Replace changed the body")
	}
	rewritten := []byte(`{"rewritten":true}`)
	if err := q.Replace(rewritten); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if bud.GateUsed() != 32*kib {
		t.Fatalf("after Replace the budget holds %d, want one increment", bud.GateUsed())
	}
	if _, err := old.Read(make([]byte, 4)); !errors.Is(err, errStale) {
		t.Fatalf("reader of the superseded body = %v", err)
	}
	if q.ContentLength() != int64(len(rewritten)) || !bytes.Equal(q.Buffered(), rewritten) {
		t.Fatal("the rewritten body is not forwarded")
	}
	rc, err := q.Open()
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := io.ReadAll(rc); !bytes.Equal(got, rewritten) {
		t.Fatalf("read %q", got)
	}
	// Replacing with an empty body keeps a gated, empty body.
	if err := q.Replace(nil); err != nil {
		t.Fatal(err)
	}
	if b := q.Buffered(); b == nil || len(b) != 0 || bud.Used() != 0 {
		t.Fatalf("empty rewrite: %v, used %d", b, bud.Used())
	}
	if rc, err := q.Open(); err != nil || rc != http.NoBody {
		t.Fatalf("Open of an empty rewrite = %v, %v", rc, err)
	}
	q.Reset()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
	// Without a budget a non-empty rewrite is refused.
	var bare Request
	if err := bare.Replace([]byte("x")); !errors.Is(err, ErrBudget) {
		t.Fatalf("Replace without a budget = %v", err)
	}
	// A streamed body replaced becomes gated.
	var s Request
	if err := s.Prepare(context.Background(), strings.NewReader("abc"), -1, false, 10, bud, 0); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace([]byte("xyz!")); err != nil {
		t.Fatal(err)
	}
	if b, ok := s.Gated(); !ok || string(b) != "xyz!" || s.Streamed() {
		t.Fatal("a replaced stream is not gated")
	}
	s.Reset()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq71ReplaceAliasedBody: a Filter passing back the slice it got from
// Message.Body (unchanged, truncated, a middle part, emptied, edited in
// place) must not leave the request holding the released gate's array,
// across the slice's whole capacity, which the pool hands to the next gate
// on the Node (cross-request exposure and corruption; spec 07 req 71, spec
// 04 req 48). Replace copies such a body and gives an empty one no
// capacity; a fresh one is kept as is. Gated and Buffered return slices
// whose capacity is their length, so appending to them never writes into
// another request's gate. The rows derive b from the gate's own slice,
// whose capacity is the whole increment, and check the body q keeps as
// well as the accessors, so Replace and the clipping are each checked on
// their own.
func TestReq71ReplaceAliasedBody(t *testing.T) {
	tests := []struct {
		name   string
		derive func(cur []byte) []byte
	}{
		{"unchanged", func(cur []byte) []byte { return cur }},
		{"truncated", func(cur []byte) []byte { return cur[:10] }},
		{"middle", func(cur []byte) []byte { return cur[5:20] }},
		{"full slice expression", func(cur []byte) []byte { return cur[:10:10] }},
		{"emptied", func(cur []byte) []byte { return cur[:0] }},
		{"empty tail", func(cur []byte) []byte { return cur[len(cur):] }},
		{"nil", func([]byte) []byte { return nil }},
		{"edited in place", func(cur []byte) []byte {
			copy(cur, "edited")
			return cur
		}},
		{"appended within capacity", func(cur []byte) []byte { return append(cur[:4], "tail"...) }},
	}
	others := bytes.Repeat([]byte("B"), kib)
	later := bytes.Repeat([]byte("C"), kib)
	gateAll := func(t *testing.T, qs []Request, body []byte, bud *Budget) {
		t.Helper()
		for i := range qs {
			if err := qs[i].Prepare(context.Background(), bytes.NewReader(body), -1, true, 1<<20, bud, 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bud := NewBudget(1<<20, nil)
			var a Request
			if err := a.Prepare(context.Background(), bytes.NewReader(bytes.Repeat([]byte("A"), kib)), -1, true, 1<<20, bud, 0); err != nil {
				t.Fatal(err)
			}
			if g, _ := a.Gated(); cap(g) != len(g) {
				t.Fatalf("Gated: cap %d, len %d; want the capacity cut to the length", cap(g), len(g))
			}
			if g := a.Buffered(); cap(g) != len(g) {
				t.Fatalf("Buffered: cap %d, len %d; want the capacity cut to the length", cap(g), len(g))
			}
			cur := a.own.Bytes() // the gate's slice, with its spare capacity
			gateArray := cur
			if cap(gateArray) <= len(gateArray) {
				t.Fatalf("the gate has no spare capacity (cap %d)", cap(gateArray))
			}
			b := tt.derive(cur)
			want := bytes.Clone(b)
			if err := a.Replace(b); err != nil {
				t.Fatalf("Replace: %v", err)
			}
			// The body q keeps, checked apart from the accessors' clipping.
			a.mu.RLock()
			kept := a.cur
			a.mu.RUnlock()
			if overlaps(kept[:cap(kept)], gateArray) {
				t.Fatalf("Replace kept the released gate's array (len %d, cap %d)", len(kept), cap(kept))
			}
			got, _ := a.Gated()
			if overlaps(got[:cap(got)], gateArray) {
				t.Fatalf("Gated (len %d, cap %d) still uses the released gate's array", len(got), cap(got))
			}
			if buf := a.Buffered(); overlaps(buf[:cap(buf)], gateArray) {
				t.Fatalf("Buffered (len %d, cap %d) still uses the released gate's array", len(buf), cap(buf))
			}
			// Gate other clients' bodies on the same budget; the pool hands
			// them the released array (unless sync.Pool dropped it).
			bs := make([]Request, 4)
			gateAll(t, bs, others, bud)
			// A Filter appending to the body it got must not write into
			// another request's gate.
			appended, _ := a.Gated()
			appended = append(appended, "XYZ"...)
			for i := range bs {
				if body, _ := bs[i].Gated(); !bytes.Equal(body, others) {
					t.Fatalf("appending to a's body changed request %d's body to %.32q", i, body)
				}
			}
			rc, err := a.Open()
			if err != nil {
				t.Fatal(err)
			}
			if sent, _ := io.ReadAll(rc); !bytes.Equal(sent, want) {
				t.Fatalf("the upstream got %.32q, want %.32q", sent, want)
			}
			if b := a.Buffered(); !bytes.Equal(b, want) {
				t.Fatalf("Buffered = %.32q", b)
			}
			// Replacing with the appended body, then recycling the other
			// gates into new ones, leaves a's body intact.
			if err := a.Replace(appended); err != nil {
				t.Fatalf("Replace(appended): %v", err)
			}
			for i := range bs {
				bs[i].Reset()
			}
			gateAll(t, bs, later, bud)
			wantAppended := append(bytes.Clone(want), "XYZ"...)
			if body, _ := a.Gated(); !bytes.Equal(body, wantAppended) {
				t.Fatalf("after other requests gated, a's body = %.32q, want %.32q", body, wantAppended)
			}
			for i := range bs {
				bs[i].Reset()
			}
			a.Reset()
			if bud.Used() != 0 {
				t.Fatalf("Used = %d", bud.Used())
			}
		})
	}
	// A body in fresh memory is taken without a copy.
	bud := NewBudget(1<<20, nil)
	var q Request
	if err := q.Prepare(context.Background(), strings.NewReader(matrixBody), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	fresh := []byte("fresh body")
	if err := q.Replace(fresh); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Gated(); &got[0] != &fresh[0] {
		t.Fatal("a body in fresh memory was copied")
	}
	// Replacing a rewritten body with a part of itself needs no copy: no
	// pool holds its memory.
	if err := q.Replace(fresh[:5]); err != nil {
		t.Fatal(err)
	}
	if got, _ := q.Gated(); &got[0] != &fresh[0] || string(got) != "fresh" {
		t.Fatalf("Gated = %q", got)
	}
	q.Reset()
}

func TestOverlaps(t *testing.T) {
	arr := make([]byte, 100)
	a := arr[10:20:30] // backing range [10, 30)
	tests := []struct {
		name string
		b    []byte
		want bool
	}{
		{"same", a, true},
		{"inside the length", arr[12:15], true},
		{"inside the capacity only", arr[25:28], true},
		{"straddles the start", arr[5:11], true},
		{"straddles the capacity end", arr[29:31], true},
		{"ends at the start", arr[0:10], false},
		{"starts at the capacity end", arr[30:40], false},
		{"empty", arr[15:15], false},
		{"nil", nil, false},
		{"other array", make([]byte, 10), false},
	}
	for _, tt := range tests {
		if got := overlaps(tt.b, a); got != tt.want {
			t.Errorf("overlaps(%s) = %v, want %v", tt.name, got, tt.want)
		}
	}
	if overlaps(arr, nil) || overlaps(arr, arr[:0:0]) {
		t.Error("a slice without capacity overlaps")
	}
}

// TestR41ConcurrentAccess opens and reads a gated body from composition
// step goroutines while the request's owner replaces and resets it (the
// ending protocol ending a request while steps still open readers): run
// with -race; every read sees a whole body or a stale reader.
func TestR41ConcurrentAccess(t *testing.T) {
	bud := NewBudget(4<<20, nil)
	var q Request
	if err := q.Prepare(context.Background(), strings.NewReader(matrixBody), -1, true, 1<<20, bud, 0); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = q.ContentLength()
				_ = q.Replayable()
				_ = q.Buffered()
				_, _ = q.Gated()
				_ = q.Streamed()
				_ = q.BytesRead()
				_ = q.Exceeded()
				rc, err := q.Open()
				if err != nil {
					continue
				}
				got, err := io.ReadAll(rc)
				if err == nil && len(got) > 0 && string(got) != matrixBody && string(got) != "rewritten" {
					t.Errorf("a step read %q", got)
				}
				_ = rc.Close()
			}
		})
	}
	for i := range 200 {
		switch i % 4 {
		case 0:
			_ = q.Replace([]byte("rewritten"))
		case 1:
			q.Reset()
		case 2:
			q.SetStream(strings.NewReader(""), -1, 10)
		default:
			_ = q.Prepare(context.Background(), strings.NewReader(matrixBody), -1, true, 1<<20, bud, 0)
		}
	}
	close(stop)
	wg.Wait()
	q.Reset()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// TestReq34Prepare covers the handler's entry: declared over the limit is
// 413 RZ-RT-003 without reading; a gate over the budget is 503 RZ-RT-004.
func TestReq34Prepare(t *testing.T) {
	bud := NewBudget(128*kib, nil)
	var q Request
	spy := &spyReader{t: t, r: strings.NewReader(matrixBody)}
	err := q.Prepare(context.Background(), spy, 1<<20, true, 1024, bud, 0)
	if !errors.Is(err, ErrTooLarge) || spy.calls != 0 || Code(err, SideRequest) != "RZ-RT-003" {
		t.Fatalf("Prepare = %v after %d reads", err, spy.calls)
	}
	if q.ContentLength() != 0 || q.Streamed() {
		t.Fatal("a refused body left state")
	}
	err = q.Prepare(context.Background(), bytes.NewReader(pattern(100*kib)), -1, true, 1<<20, bud, 0)
	if Code(err, SideRequest) != "RZ-RT-004" {
		t.Fatalf("Prepare over the budget = %v", err)
	}
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
	// Streamed: gate false keeps the stream and its declared length.
	if err := q.Prepare(context.Background(), strings.NewReader(matrixBody), int64(len(matrixBody)), false, 1024, bud, 0); err != nil {
		t.Fatal(err)
	}
	if !q.Streamed() || q.ContentLength() != int64(len(matrixBody)) {
		t.Fatal("not streamed")
	}
	if _, ok := q.Gated(); ok {
		t.Fatal("a stream reports gated bytes")
	}
	if q.BytesRead() != 0 {
		t.Fatalf("BytesRead = %d", q.BytesRead())
	}
	// A new Prepare resets: the old stream's readers turn stale.
	rc, _ := q.Open()
	if err := q.Prepare(context.Background(), nil, -1, false, 1024, bud, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Read(make([]byte, 1)); !errors.Is(err, errStale) {
		t.Fatalf("old reader = %v", err)
	}
	if rc, err := q.Open(); err != nil || rc != http.NoBody {
		t.Fatalf("Open of a nil body = %v, %v", rc, err)
	}
	if _, err := q.Open(); err != nil {
		t.Fatal(err)
	}
	// Opening a stream after the request ended is refused.
	var s Request
	s.SetStream(strings.NewReader("abc"), 3, 10)
	st := s.st
	s.Reset()
	s.mode, s.st = modeStream, st
	if _, err := s.Open(); !errors.Is(err, errStale) || s.Replayable() {
		t.Fatalf("Open of an ended stream = %v", err)
	}
	s.mode, s.st = modeEmpty, nil
	if s.BytesRead() != 0 || s.Exceeded() {
		t.Fatal("empty body state")
	}
}

// TestReq46SetGated hands a gate read elsewhere to a Request.
func TestReq46SetGated(t *testing.T) {
	bud := NewBudget(1<<20, nil)
	g, err := ReadGate(context.Background(), strings.NewReader(""), -1, 10, bud, 0)
	if err != nil {
		t.Fatal(err)
	}
	var q Request
	q.SetGated(g)
	if b := q.Buffered(); b == nil || len(b) != 0 {
		t.Fatalf("Buffered of an empty gate = %v", b)
	}
	if rc, err := q.Open(); err != nil || rc != http.NoBody {
		t.Fatalf("Open = %v, %v", rc, err)
	}
	// A gate of a declared-empty body has no bytes at all; Buffered is
	// still non-nil so GetBody can be set.
	var e Request
	g0, err := ReadGate(context.Background(), strings.NewReader("ignored"), 0, 10, NewBudget(0, nil), 0)
	if err != nil {
		t.Fatal(err)
	}
	e.SetGated(g0)
	if b := e.Buffered(); b == nil || len(b) != 0 || e.ContentLength() != 0 {
		t.Fatalf("Buffered of a declared-empty gate = %v", b)
	}
	e.Reset()
	// The budget comes from the gate, so a rewrite can reserve.
	if err := q.Replace([]byte("x")); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	q.Reset()
	if bud.Used() != 0 {
		t.Fatalf("Used = %d", bud.Used())
	}
}

// BenchmarkRequestGatedOpen opens and reads a 1 KiB gated body, as one
// Upstream attempt does.
func BenchmarkRequestGatedOpen(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, nil)
	data := pattern(kib)
	var q Request
	if err := q.Prepare(context.Background(), bytes.NewReader(data), -1, true, 1<<20, bud, 0); err != nil {
		b.Fatal(err)
	}
	buf := make([]byte, 4*kib)
	b.ReportAllocs()
	for b.Loop() {
		rc, err := q.Open()
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
		_ = rc.Close()
	}
	q.Reset()
}

// BenchmarkRequestGated1KiB gates a 1 KiB request body through Prepare and
// ends the request: the gate reads into the Request's own Buffer and a
// pooled array, so it allocates nothing once warm.
func BenchmarkRequestGated1KiB(b *testing.B) {
	bud := NewBudget(DefaultMaxBufferedBytes, &sumGauge{})
	data := pattern(kib)
	r := bytes.NewReader(data)
	ctx := context.Background()
	var q Request
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		if err := q.Prepare(ctx, r, int64(len(data)), true, 10<<20, bud, 0); err != nil {
			b.Fatal(err)
		}
		q.Reset()
	}
}

// BenchmarkRequestStream prepares and reads a 64 KiB streamed body.
func BenchmarkRequestStream(b *testing.B) {
	data := pattern(64 * kib)
	r := bytes.NewReader(data)
	buf := make([]byte, Increment)
	ctx := context.Background()
	var q Request
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		r.Reset(data)
		if err := q.Prepare(ctx, r, int64(len(data)), false, 10<<20, nil, 0); err != nil {
			b.Fatal(err)
		}
		rc, err := q.Open()
		if err != nil {
			b.Fatal(err)
		}
		for {
			if _, err := rc.Read(buf); err != nil {
				break
			}
		}
		q.Reset()
	}
}
