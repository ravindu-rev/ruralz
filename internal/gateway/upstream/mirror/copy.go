// Copyright 2026 Revington
// SPDX-License-Identifier: Apache-2.0

package mirror

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"

	"github.com/ravindu-rev/ruralz/internal/gateway/body"
	"github.com/ravindu-rev/ruralz/internal/telemetry/emit"
)

// errRefused stops the copy of a buffered body that the budget refused.
var errRefused = errors.New("mirror: the buffer budget refused the body copy")

// bodyCopy is the copy of an attempt's body that the mirrors of one offer
// share (package documentation, "Offers"). The memory it holds, its
// capacity, is reserved from the Node buffer budget before it is
// allocated, and returned when the copy ends incomplete or the last mirror
// released its share. A streamed body's copy is filled by the reader Offer
// put in place of the attempt's body, on whatever goroutine the transport
// reads it, and ends when that body does: complete at its end, incomplete
// when it failed or was closed before it, or when a reservation was
// refused.
type bodyCopy struct {
	// done is closed once the copy ended, complete or not.
	done chan struct{}

	// mu guards the fields below: the transport's reader and the legs
	// reach them from their own goroutines.
	mu       sync.Mutex
	acct     body.Account
	buf      []byte
	complete bool
	ended    bool
	// refs counts the mirrors that still hold a share.
	refs int
}

// copyBody copies the body of req for refs mirrors and reports whether the
// copy can still complete: no body needs none (nil); a buffered body, one
// with GetBody (the gate, or a body an onUpstreamRequest Policy wrote), is
// read through GetBody at once; a streamed body is copied as the attempt
// reads it, through a reader put in place of req.Body. A buffered body
// whose copy the budget refuses, or that GetBody cannot read, is false. A
// declared length sizes the copy, and reserves it, at once.
func (r *Runner) copyBody(req *http.Request, stripe emit.Stripe, refs int) (*bodyCopy, bool) {
	if req.Body == nil || req.Body == http.NoBody {
		return nil, true
	}
	c := &bodyCopy{done: make(chan struct{}), refs: refs}
	c.acct.Bind(r.budget, body.Gate, stripe)
	if n := req.ContentLength; n > 0 && !c.grow(n) {
		return nil, false
	}
	if req.GetBody == nil {
		req.Body = &teeBody{rc: req.Body, c: c}
		return c, true
	}
	rc, err := req.GetBody()
	if err != nil {
		c.drop()
		return nil, false
	}
	_, err = io.Copy(copyWriter{c}, rc)
	_ = rc.Close()
	c.end(err == nil)
	if !c.isComplete() {
		c.drop()
		return nil, false
	}
	return c, true
}

// grow raises the copy's capacity to hold n bytes, reserving the new
// capacity before allocating it: doubled, at least one increment, else
// exactly n when the budget refuses the doubling. It reports false,
// changing nothing, when even n is refused.
func (c *bodyCopy) grow(n int64) bool {
	have := int64(cap(c.buf))
	if n <= have {
		return true
	}
	want := max(2*have, n, body.Increment)
	if c.acct.Reserve(want-have) != nil {
		if want = n; c.acct.Reserve(want-have) != nil {
			return false
		}
	}
	nb := make([]byte, len(c.buf), want)
	copy(nb, c.buf)
	c.buf = nb
	return true
}

// write appends p within reserved capacity; false, ending the copy
// incomplete, when the budget refuses the capacity or the copy already
// ended.
func (c *bodyCopy) write(p []byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ended {
		return false
	}
	if !c.grow(int64(len(c.buf)) + int64(len(p))) {
		c.endLocked(false)
		return false
	}
	c.buf = append(c.buf, p...)
	return true
}

// copyWriter is the io.Writer of a buffered body's copy.
type copyWriter struct{ c *bodyCopy }

// Write copies p, or fails with errRefused.
func (w copyWriter) Write(p []byte) (int, error) {
	if !w.c.write(p) {
		return 0, errRefused
	}
	return len(p), nil
}

// end ends the copy, complete when ok, once.
func (c *bodyCopy) end(ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endLocked(ok)
}

func (c *bodyCopy) endLocked(ok bool) {
	if c.ended {
		return
	}
	c.ended, c.complete = true, ok
	if !ok {
		// An incomplete copy serves no mirror: its memory goes now.
		c.acct.ReleaseAll()
		c.buf = nil
	}
	close(c.done)
}

// isComplete reports whether the copy ended complete.
func (c *bodyCopy) isComplete() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.complete
}

// wait waits until the copy ended or ctx ends, and reports whether it is
// complete; a copy that ended first counts even when ctx has ended too.
func (c *bodyCopy) wait(ctx context.Context) bool {
	select {
	case <-c.done:
		return c.isComplete()
	default:
	}
	select {
	case <-c.done:
		return c.isComplete()
	case <-ctx.Done():
		return false
	}
}

// bytes returns the complete copy; call it only after wait reported true,
// while the caller holds its share.
func (c *bodyCopy) bytes() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf
}

// reader returns a reader of b, the complete copy, for one request.
func (c *bodyCopy) reader(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }

// release gives back one mirror's share; the last share returns the
// reservation and ends a copy still filling, so what the attempt still
// sends is no longer copied. The bytes are left to the garbage collector,
// so a transport still reading a share's reader never meets other memory.
func (c *bodyCopy) release() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.refs--; c.refs > 0 {
		return
	}
	c.dropLocked()
}

// drop ends the copy incomplete and returns everything it holds.
func (c *bodyCopy) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropLocked()
}

func (c *bodyCopy) dropLocked() {
	if !c.ended {
		c.ended = true
		close(c.done)
	}
	c.acct.ReleaseAll()
	c.buf = nil
}

// teeBody is the body of an attempt whose streamed body the mirrors copy:
// every byte the transport reads goes to the copy, which completes at the
// body's end. It never blocks on a mirror, and the copy's refusal never
// reaches the attempt.
type teeBody struct {
	rc io.ReadCloser
	c  *bodyCopy
}

// Read reads the attempt's body and copies what it read.
func (t *teeBody) Read(p []byte) (int, error) {
	n, err := t.rc.Read(p)
	if n > 0 {
		t.c.write(p[:n])
	}
	switch {
	case errors.Is(err, io.EOF):
		t.c.end(true)
	case err != nil:
		t.c.end(false)
	}
	return n, err
}

// Close closes the attempt's body; a copy that has not reached the body's
// end by then never completes.
func (t *teeBody) Close() error {
	err := t.rc.Close()
	t.c.end(false)
	return err
}
